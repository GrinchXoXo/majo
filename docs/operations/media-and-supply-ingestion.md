# Media ingestion and supply grouping (Phase 4A.1 / 4A.2)

How vendor WhatsApp media becomes durable `MediaAsset`s, and how vendor photos, captions and text become one `SupplySignal`. Nothing here is buyer-visible: stored is not published, and a signal is evidence, not a listing.

## How it runs

There is one Go process. The in-process worker polls the Postgres `jobs` table and handles two job types:

| Job type | Created by | Does |
|---|---|---|
| `MEDIA_DOWNLOAD` | the webhook, for vendor image/video; also the startup backfill | Graph download → validate → SHA-256 → `storage.Provider` → `media_assets` + `MEDIA_ASSET_STORED` |
| `SUPPLY_GROUPING` | supply intake (window timer, closing text, media finished) | evaluates the submission a message belongs to → `supply_signals` + `supply_signal_sources` + `SUPPLY_SIGNAL_CREATED` / `SUPPLY_SIGNAL_UPDATED` |

Execution is **at-least-once with idempotent effects**:
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
- **Status:** always **DRAFT**. Publishing stays `POST /api/supply-listings/{id}/activate`.
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
