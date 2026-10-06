# Media ingestion, supply grouping and listing curation (Phase 4A.1 – 4A Step 4)

How vendor WhatsApp media becomes durable `MediaAsset`s, and how vendor photos, captions and text become one `SupplySignal`. Nothing here is buyer-visible: stored is not published, and a signal is evidence, not a listing.

## How it runs

There is one Go process. The in-process worker polls the Postgres `jobs` table and handles two job types:

| Job type | Created by | Does |
|---|---|---|
| `MEDIA_DOWNLOAD` | the webhook, for vendor image/video; also the startup backfill | Graph download → validate → SHA-256 → `storage.Provider` → `media_assets` + `MEDIA_ASSET_STORED` |
| `SUPPLY_GROUPING` | supply intake (window timer, closing text, media finished) | evaluates the submission a message belongs to → `supply_signals` + `supply_signal_sources` + `SUPPLY_SIGNAL_CREATED` / `SUPPLY_SIGNAL_UPDATED` |

Execution is **at-least-once with idempotent effects**. A terminal failure and its failure event (`MEDIA_INGESTION_FAILED`) are stored in one transaction:
- Claims use `FOR UPDATE SKIP LOCKED`.
- A `RUNNING` job older than `JOBS_LEASE` is reclaimed.
- Every state change is fenced on `locked_by`.

At startup, vendor image/video messages from the last `MEDIA_BACKFILL_WINDOW` that have no asset and no job are enqueued.

## Settings

All settings are optional; defaults are shown.

| Variable | Default | Meaning |
|---|---|---|
| `MEDIA_STORAGE_DIR` | `data/media` (compose: `/app/data/media` on the `media_data` volume) | Root of the local storage provider. **Must be persistent.** |
| `MEDIA_MAX_IMAGE_BYTES` / `MEDIA_MAX_VIDEO_BYTES` | 5 MB / 16 MB | Size limits |
| `MEDIA_BACKFILL_WINDOW` | `168h` | Startup backfill look-back |
| `JOBS_POLL_INTERVAL` | `2s` | Worker poll interval |
| `JOBS_CONCURRENCY` | `2` | Worker loops |
| `JOBS_MAX_ATTEMPTS` | `8` | Attempts before a job is `FAILED` |
| `JOBS_BACKOFF_BASE` / `JOBS_BACKOFF_MAX` | `30s` / `30m` | Exponential backoff with jitter |
| `JOBS_LEASE` | `5m` | When a `RUNNING` job counts as abandoned |
| `SUPPLY_GROUPING_WINDOW` | `120s` | Grouping window (an experiment parameter) |

The local provider never generates URLs (`GenerateURL` returns "not supported").

## Grouping rules (4A.2)

Grouping only looks at messages from the same vendor in the same conversation. Messages are ordered by the vendor's send time (Meta's timestamp, saved as `messages.metadata.provider_timestamp`).

A new submission starts when any of the following is true:
1. the message already belongs to a signal;
2. it was sent more than the window after the submission's first message;
3. a non-supply message (a reply to a request, an ambiguous photo, a voice note) sits in between;
4. the submission already ended with a standalone text (captions come last);
5. its text names a product **and** a price, and the submission already has both.

A submission with no text from a vendor with no request in context (proactive) becomes an **incomplete** signal: it references its media, and its product, price and the rest are empty, with confidence 0 and `extracted_data.incomplete = true`. Nothing is inferred from the media, and matching ignores signals that have no product. The exception: photos sent within the window right after a text signal are added to that signal, as `SUPPLY_SIGNAL_UPDATED`.

A signal is created only when the submission is final and every one of its photos/videos has finished downloading (stored or failed). A failed photo is kept as a source with no asset.

**Reply context comes first.** A vendor's message is judged against a request in context when that request is either:
- still active (SENT); or
- one the vendor answered within the grouping window before the message (a VendorResponse exists).

In that case the message is supply only if its text or caption:
- names a product and a price;
- does not open like a reply ("yes", "available", "I get" …);
- has no `MM-` reference;
- does not mention the requested product.

Anything else is **not** supply: the media is still stored, and `VENDOR_MESSAGE_UNRESOLVED` (no state change) records it for operator review. For example, "Yes boss" followed by a photo, or followed by "₦620k", stays with the reply.

## Inspecting

```sql
-- Jobs that failed, and why
SELECT job_type, idempotency_key, attempts, max_attempts, last_error, updated_at
FROM jobs WHERE status = 'FAILED' ORDER BY updated_at DESC;

-- Jobs waiting to retry
SELECT job_type, attempts, run_at, last_error FROM jobs WHERE status = 'PENDING' AND attempts > 0;

-- A message's stored asset (the file is at MEDIA_STORAGE_DIR/<storage_key>)
SELECT a.* FROM messages m JOIN media_assets a ON a.id = m.media_asset_id WHERE m.external_message_id = '<wamid>';
-- Verify a stored file:  sha256sum "$MEDIA_STORAGE_DIR/<storage_key>"  must equal media_assets.sha256

-- A signal's evidence, in order
SELECT ss.position, m.external_message_id, m.message_type, ss.media_asset_id
FROM supply_signal_sources ss JOIN messages m ON m.id = ss.message_id
WHERE ss.supply_signal_id = '<signal id>' ORDER BY ss.position;

-- Why a vendor message was (or was not) treated as supply
SELECT * FROM supply_candidate_messages WHERE message_id = '<message id>';  -- no row = not supply
SELECT payload FROM events WHERE event_type = 'VENDOR_MESSAGE_UNRESOLVED' AND entity_id = '<message id>';
```

Logs carry `job_id`, `message_id`, `vendor_id`, `provider_media_id`, `media_asset_id`, `supply_signal_id`, the attempt number, and the grouping or classification reason. Tokens and signed media URLs are never logged.

## Tests

```sh
cd backend
go test ./...                                     # unit tests; the fake Graph endpoint is in-process
MAJO_TEST_DATABASE_URL="postgres://USER:PASS@localhost:5432/postgres?sslmode=disable" \
  go test ./infrastructure/postgres/ -run Integration -v   # real Postgres, one throwaway DB per test
cd ../ai && python3 -m pytest -q
```

**Process-level runs (test-equivalent, not live WhatsApp).** The tooling is in `scripts/e2e`:
- `fake_graph_api.py` with `MEDIA_DIR` set fakes the Graph API.
- `media_ingestion.py send|verify` (4A.1) lets you `kill -9` the backend between the two phases.
- `supply_grouping.py` (4A.2) needs the real AI service running.

Each script's docstring lists the environment variables it needs.

## Listings from signals (4A.3)

Every new signal, and every signal that gains late photos, gets a durable `SUPPLY_LISTING_DRAFT` job. `ListingService.CreateFromSignal` is the only path from a SupplySignal to a SupplyListing; the operator endpoint `POST /api/supply-listings` uses it too.

**Eligibility** (otherwise no listing; the job completes, the API returns 422):
- the vendor is ACTIVE;
- the signal has a product;
- it is AVAILABLE or CAN_SOURCE;
- it is not expired;
- it is not an incomplete media-only signal.

**What the listing gets:**
- **Status:** always **DRAFT**. Only an operator can publish it (see *Curation* below).
- **Fields:** copied from the signal; nothing is invented.
- **Location:** the signal's own location, else the vendor's `primary_location`, else NULL. Service areas are not used.
- **Media:** `media_references` hold the signal's AVAILABLE MediaAssets in evidence order (`asset_id`, kind, `storage_key`); there is no copy and no re-download.
- **Reference:** `public_reference` is `L-` plus 6 Crockford base32 characters, random, unique in the database, retried on collision.
- **Freshness:** `last_verified_at` is when the supply evidence arrived (`supply_signals.created_at`), and `expires_at` is 7 days after that. Nothing expires listings automatically.

**Repeat signals:**
- **Same signal again:** returns the same listing. A DRAFT also gains any of that signal's media it lacks.
- **Phase 3 duplicate** (same vendor + normalized product + variant + price, not expired or archived): only `last_verified_at` and `expires_at` move forward. Fields, media and operator corrections are left alone.

**Concurrency:** creation, duplicate refresh and the event run in one transaction with a per-vendor advisory lock. A partial unique index allows one live listing per signal.

**Search** (`GET /api/supply-listings`):
- product and location queries are compared in compact form: lowercase, letters and digits only, so "iPhone 13", "iphone13" and "IPHONE-13" match, as do "BENIN CITY" and "Benin City";
- category and condition are case-insensitive;
- by default only ACTIVE listings are returned; pass `status=DRAFT` for operator review.

```sql
-- Why does listing L-XXXXXX exist?
SELECT l.public_reference, l.status, l.location, l.last_verified_at, s.id AS signal, m.external_message_id, ss.media_asset_id
FROM supply_listings l JOIN supply_signals s ON s.id = l.supply_signal_id
JOIN supply_signal_sources ss ON ss.supply_signal_id = s.id JOIN messages m ON m.id = ss.message_id
WHERE l.public_reference = 'L-XXXXXX' ORDER BY ss.position;
```

## Curation (Phase 4A Step 4)

The operator is the publication gate. A listing becomes ACTIVE only through `POST /api/supply-listings/{id}/publish`; nothing else sets ACTIVE (not creation, edits, media, refresh or confidence).

**Dashboard:** *Supply Listings* (`/offers`) opens on the DRAFT queue (newest first; expired drafts are not shown). A listing page (`/offers/{id}`) shows the editable marketplace fields next to the source signal's values, the media previews, the source messages, and why the listing cannot be published yet.

**Operator token:** set `OPERATOR_API_TOKEN` (16+ characters). The backend refuses to start without it unless `APP_ENV=development` is set *explicitly* (the APP_ENV default does not count). Every listing endpoint needs `Authorization: Bearer <token>`: search, the per-vendor list, the detail (it shows the vendor's raw messages), editing, publishing, rejecting and media previews. In the dashboard, the operator types it once per browser tab. There are no per-operator identities, so audit rows say OPERATOR without a name. Other operator pages (requests, vendors, responses) are still unprotected, as before.

| Operation | Allowed from | Result | Event / audit |
|---|---|---|---|
| `PATCH /api/supply-listings/{id}` | DRAFT, ACTIVE | changes only: product, category, variant, condition, price, currency (NGN), quantity, location, description, `media_order`, `primary_media_asset_id`. Any other field is a 400. | `SUPPLY_LISTING_UPDATED` + `SUPPLY_LISTING_CORRECTED` audit (old/new) |
| `POST …/publish` (`/activate` is an alias) | DRAFT | ACTIVE; `published_at` set once and never reset | `SUPPLY_LISTING_PUBLISHED` + audit, once |
| `POST …/reject` `{"reason"}` | DRAFT, ACTIVE | ARCHIVED, `rejected_at`, `rejection_reason`. Terminal: no republish. | `SUPPLY_LISTING_REJECTED` + audit |
| `GET …/media/{asset}` | any | the bytes of one of *this listing's* AVAILABLE assets, via the storage provider | — |

- **Repeats are no-ops:** publishing an ACTIVE listing or rejecting a rejected one returns 200 and writes nothing.
- **Stale tabs:** send the listing's `updated_at` back as `expected_updated_at`; if it changed meanwhile, the answer is 409 `STALE`.
- **Publication requires:**
  - a product, a price above 0, and NGN;
  - quantity above 0 and a non-blank location, if set;
  - unexpired evidence;
  - an ACTIVE vendor;
  - an existing source signal of the same vendor;
  - every media reference pointing at that vendor's asset, with an AVAILABLE primary image.

  Media is optional.
- **Evidence is never edited:** the signal, its messages and MediaAssets stay as they were. A rejected signal is not re-drafted when it gains late photos.
- **Duplicate refresh:** it also recognises a listing by its source signal's original product, variant and price. A corrected listing is therefore refreshed (freshness only) instead of duplicated, and the correction stays.

```sql
-- What happened to a listing
SELECT action, previous_value, new_value, created_at FROM audit_logs WHERE entity_id = '<listing id>' ORDER BY created_at;
SELECT event_type, payload, created_at FROM events WHERE entity_id = '<listing id>' ORDER BY created_at;
```

E2E: `scripts/e2e/listing_curation.py` drives the real dashboard with headless Chromium (Playwright) against the real backend. Its docstring lists the environment variables.

## Matching reads published listings (Phase 4A)

`routing.SuggestVendors` uses each vendor's **ACTIVE, unexpired** listings as supply evidence, alongside fresh signals. The weights are unchanged.

**Scoring:**
- A listing is scored like a fresh AVAILABLE signal: exact product + variant = full product points; exact = 0.9; family = 0.5; full availability points.
- A listing built from a CAN_SOURCE signal stays can-source supply when published: 0.67 product and 0.6 availability, with the reason `can_source_listing`.
- The product comparison uses the compact form of the listing's product, and also of its source signal's product when the listing's product is a refinement of it.
  - "bag" → "Imported ladies bags" still matches a request for "bag" exactly.
  - A correction to a different product ("iphone 13" → "iphone 14") drops the old name.
- The strongest evidence wins, and a listing wins a tie. Condition and budget are then judged on that evidence, so a corrected listing price is used.

**Which signals still count:**
- A signal whose listing is published (ACTIVE and unexpired) is represented by that listing. When that listing's evidence has expired, the signal counts again on its own.
- A signal whose listing was rejected is no longer evidence.
- Signals with only a DRAFT listing (not yet curated) count as before.

**Where the evidence is recorded:**
- `GET /api/requests/{id}/matches` shows `relevant_listing` (L-reference, price), plus the reasons `SUPPLY_CONFIRMED_active_listing` and `active_listing`.
- Routing stores `evidence`, `supply_listing_id` and `listing_reference` in `vendor_requests.match_breakdown`.
- `VENDORS_SUGGESTED` carries `listing_evidence_count`.

Matching still only suggests vendors to ask; nothing is sent or created by it.
