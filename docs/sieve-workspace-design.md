# Sieve store design (phase 4b — Fastmail-compatible filter management)

Date: 2026-07-02
Status: **SHIPPED** in v0.4.0 (store + six tools + sieve-rules skill;
build-plan items 1–3). Since v0.6.0 the tools are an opt-in behind the
`enable_sieve_tools` config flag (default off) — withdrawn and refusing
when disabled, independent of `access_level`. v0.8.0 hardened the workflow
from field-report findings (implementation-plan.md "Phase B"): advisory lint
on every put (+ dryRun), `sourcePath` file import jailed to the session CWD,
`contextOnly` reference documents, a seventh tool `email_sieve_archive`
(move aside, never delete), truncation-marker guards on `mark_applied`, and
missing-document errors that list what exists. RFC 9661 API transport
(item 4) remains pending the Stalwart interop layer. State lives in the
**extension data directory** behind extension-owned tools (no git
dependency), with append-only versioning. Implementation note: the tools
make **zero network calls** — account defaults resolve from the store's own
contents, keeping the local-read/local-data authority declarations strict
(first use passes an explicit accountId from email_status; lint reports
`fileinto` targets as data for the agent to cross-check with
email_list_mailboxes rather than resolving them itself).

## Problem

Fastmail exposes no sieve API ([filters-research.md](filters-research.md)):
editing happens in a web UI structured as locked, system-generated blocks
alternating with editable text areas (their docs suggested ~four of each;
first-use calibration against a real account found **three** editable areas —
structure varies, always verify). The agent can still manage filters
with the human as transport — paste current state in, get updated sections
out — but only if the sieve has a durable local home with history, instead of
being re-supplied every session. RFC 9661 providers (Stalwart, Cyrus) can
later sync whole scripts over the API; one design should serve both.

## Architecture: three roles

| role | owner | notes |
|---|---|---|
| **State** | extension data dir (`$TERVA_HOME/ext-data/jmap-mail/sieve/…`), behind extension tools | no git or any external tooling required; versioning/backup is built in |
| **Transport** | the extension | today: grounding (mailbox names, `email_search` previews). Later: RFC 9661 tools. Fastmail's transport is *the user pasting* |
| **Workflow** | a bundled skill (`skills/sieve-rules/`) | drives the loop through the store tools; works from any cwd since tools carry the state |

Decisions from review: data-dir storage (a non-git extension must not depend
on git); therefore extension-owned store tools rather than agent file
manipulation; therefore no `sieve_workspace` config key (discovery is moot —
the tools are the interface).

## The store (`internal/sievestore`)

Pure, unit-testable package rooted at a directory (the extension passes its
data dir; tests pass `t.TempDir()`).

### Data model: append-only versioned documents

A **document** is a named sieve unit, keyed by provider host + account id +
name — e.g. `api.fastmail.com/uc9a140ba/section-2-below-spam`, or a whole
script `stalwart.local/acc1/main` for API providers later.

```text
sieve/<provider-host>/<account-id>/<doc-name>/
  versions/v000001.sieve     # content, immutable once written
  versions/v000001.json      # {ts, origin, note, appliedMarker?}
  head                       # the current version number
  applied                    # version number last confirmed live at the provider
```

- **Every mutation appends** a new version; *current* is simply the head.
  Nothing is ever rewritten.
- **Restore is an append**: `restore(v12)` writes a new head whose content is
  a *copy* of v12, annotated `restored from v12`. The prior head remains the
  previous version; v12 stays where it was; no state is discarded. (This
  replaces any move-based scheme — append-only gives the same "restore loses
  nothing" guarantee with one mechanism.)
- **Origins** label how a version arrived: `paste-in` (verbatim user paste —
  the evidence/snapshot role), `edit` (agent change), `restore`, later
  `pull` (API providers).
- **Retention**: keep the last **50** versions per document (constant until
  real use demands a knob), pruning oldest only. Sieve files are tiny; 50
  versions ≈ noise. If size ever matters, the agreed path is compressing
  aged versions in place rather than lowering the cap.
- The **applied pointer** records the version last confirmed live at the
  provider. The three-state model becomes: *pending changes* = diff(applied,
  head); *out-of-band drift* = new paste-in content ≠ content(applied).

### Diffing

Line-based unified diff implemented in-package (small LCS/Myers, ~100 lines,
unit-tested; no vendored dependency for this). Agents reason over diffs far
more cheaply than over two full scripts.

## Tool surface (all operate only on the extension's own data dir)

Authority: reads declare `local-read`; writes declare `local-data` (both in
the pinned SDK). No `Sequential()` races matter for reads; the two mutating
tools are `Sequential()`.

| tool | authority | purpose |
|---|---|---|
| `email_sieve_list` | local-read | documents with head/applied versions, origins, timestamps |
| `email_sieve_get` | local-read | content of head or any version of a document |
| `email_sieve_diff` | local-read | unified diff between two versions (defaults: applied → head, i.e. "what's pending") |
| `email_sieve_put` | local-data, Sequential | append a version (content or CWD-jailed sourcePath; lint + dryRun; contextOnly) |
| `email_sieve_restore` | local-data, Sequential | append-restore of a prior version |
| `email_sieve_mark_applied` | local-data, Sequential | set the applied pointer to an explicitly named version after the user confirms pasting (never defaulted to head — v0.19.0); refuses context-only and truncation-suspect versions |
| `email_sieve_archive` | local-data, Sequential | move a document aside (or back) — hidden, never deleted (v0.8.0) |

Seven small tools; schemas stay terse. Named `email_sieve_*` per the
org-wide tool-naming convention (one domain prefix per extension;
sub-features nest under it — `terva-sh/docs` → extensions/conventions.md).
They are generic versioned-document tools scoped to sieve — deliberately not
Fastmail-shaped, so RFC 9661 whole-script sync reuses them unchanged.

## The loop

### Fastmail (manual transport)

1. **Refresh** — user pastes current editable sections; agent `sieve_put`s
   each verbatim (`origin: paste-in`), then checks drift: pasted content vs
   `content(applied)`. Locked/generated blocks may also be pasted into
   `generated-N` documents for whole-script context (optional but useful).
2. **Edit** — agent composes the new section, lints (below), previews new
   conditions against real mail via `email_search`, then `sieve_put`s with
   `origin: edit` and a note. `sieve_diff` shows the user what changed.
3. **Emit** — the changed sections, each labeled with its editable-area
   placement ("replace the contents of area 2, below spam protection").
   Fastmail validates at save — the syntax gate at the end of the loop.
4. **Confirm** — user says "applied"; agent `sieve_mark_applied`s **the version
   it emitted in step 3**, by number. Not head: step 3 and step 4 are separated
   by a human pasting into a web UI, and any `sieve_put` in that gap moves head
   without anything reaching the provider. Marking head then records a version
   nobody pasted, and because every later diff is taken from the applied
   pointer, the mistake reads as correct from that point on. The tool refuses a
   call that does not name a version.

### RFC 9661 providers (future, Stalwart-first)

Same store and tools; steps 3–4 become API calls
(put → validate → activate-with-backup) with `origin: pull` versions written
on download and `applied` advanced automatically on successful push.

## Lint & safety rules (encoded in the skill)

- **Never emit `discard`** — Fastmail warns it permanently drops mail
  *bypassing their backups*; `fileinto` Trash/Junk is the recoverable
  equivalent.
- **`stop`-starvation analysis** across the assembled section order: a `stop`
  early in the script silently disables all later generated rules.
- **`require` coverage**: every extension used is required and within
  Fastmail's supported set (incl. `vnd.cyrus.*`); where Fastmail accepts
  `require` lines across sections — verify on first real use.
- **Paste-in before edit** (the drift check depends on it); **diff with every
  emit**; **preview every new condition** via `email_search`.

## Multi-account / multi-provider

All existing tools take `accountId`; the store keys documents by provider
host + account id, so shared/delegated accounts coexist naturally. The
extension config still holds one provider (one session_url/token) at a time —
multi-provider profiles remain the phase-5 portability item, and the store
layout already anticipates it.

## Build plan (after design sign-off)

1. `internal/sievestore`: versioned store + unified diff, pure + unit-tested.
2. The tools in app.go/main.go (seven since v0.8.0) (`local-read`/`local-data`,
   `Sequential()` on writes); visibility follows the existing configured
   gate. Version bump.
3. `skills/sieve-rules/SKILL.md`: the loop, lint rules, emit format, and the
   first-use calibration step (map the real editable areas of the user's
   account to document names).
4. Later, with Stalwart interop: RFC 9661 client surface + API transport.

## Resolved in review (2026-07-02)

1. Tool prefix: **`email_sieve_*`** — sub-features nest under the
   extension's one domain prefix; rationale codified org-wide in
   `terva-sh/docs` → extensions/conventions.md ("Tool naming").
2. Retention: **50 confirmed** (generous is fine); compress aged versions
   rather than lower the cap if size ever matters.
3. First-use calibration **confirmed as the standard onboarding pattern for
   any provider without a sieve API**: map the provider's real editable
   structure (area count/placement, `require` acceptance) to document names
   before the first edit — the skill owns this step.
