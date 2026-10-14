# Majo pilot-prep 2 report (mimo/pilot-prep-2)

Branch: `mimo/pilot-prep-2`, created from `phase-4c-hardening` after the
`mimo/pilot-prep` merge (which was already fast-forwarded in the previous
session; `--ff-only` reported "Already up to date"). Not merged, not
pushed. Only files under `docs/pilot/` (plus this report) were changed;
no code touched.

## Step 1 — merge and branch

- `git merge --ff-only mimo/pilot-prep`: already up to date (merge
  completed earlier as `2a78cf8..83b81b3`).
- `git switch -c mimo/pilot-prep-2`: created.

## Files created and commits

| Step | File | Commit |
|------|------|--------|
| 2 | `docs/pilot/buyer-message-samples.md` | `484dd15` "Pilot prep 2: buyer message samples" |
| 3 | `docs/pilot/tone-phrasebook.md` | `0b2d25d` "Pilot prep 2: tone phrasebook" |
| 4 | `docs/pilot/vendor-tracker-template.csv` | `2f13cb9` "Pilot prep 2: vendor tracker template" |
| report | `docs/implementation/mimo-pilot-prep-2-report.md` | this commit |

## What each file contains

- **buyer-message-samples.md** — 60 test messages in a table (#,
  category, message, what the buyer clearly wants). ~8 each across
  phones/electronics, fashion, food/catering, second-hand, services,
  school items, and 12 random. Styles mixed: Pidgin, campus slang, plain
  English, typos, abbreviations, emojis, a voice-note-style run-on (row
  20), and prices as `650k`, `250,000 naira`, `#85,000`, `6.5` and
  ranges. 7 rows (49–54, 60) are non-requests (hi, thanks, ok,
  complaint, 2 jokes, greeting); rows 55–59 are deliberately vague. No
  real names, phone numbers or brand promotions.
- **tone-phrasebook.md** — Section A: 15 greetings (casual Pidgin →
  polite English). Section B: 4–6 options each for found offers / no
  offers yet / one clarifying question / confirming the request / vendor
  replied late / goodbye / reply STOP. Section C: "Never say" list
  (delivery promises, vendor guarantees, money Majo doesn't handle,
  mocking slang, heavy slang to formal writers, fake familiarity, etc.).
  Section D: empty TODO(owner) slang list with 10 blank bullets. All
  phrases under 15 words; no prices, names or product details inside
  phrases.
- **vendor-tracker-template.csv** — header row plus 2 obviously fake
  example rows (`Example Phones`, `+234000000000`; `Example Styles`,
  `+234000000001` — the second has an empty `added_to_majo_on` to show
  an invited-but-not-yet-added vendor).

## Anything unsure

- "Brand promotions": I allowed product model names buyers commonly ask
  for (iPhone, PS5, Tecno, fx-991EX) as part of requests, since those
  are buyer needs, not promotions. No store or brand advertising appears.
- Naira price formats: followed the brief's examples (`650k`,
  `₦650k` style via `#85,000`, `6.5`); "6.5" is included once (row 8)
  as a slang-style budget.
- The phrasebook's Section D is intentionally left with 10 blank bullets
  for the owner to fill with current campus slang.
- CSV example dates use October 2026 to match the project timeline; both
  rows are marked "Fake example row - delete before use".
- Non-request count is 7 (brief said "about 6").
