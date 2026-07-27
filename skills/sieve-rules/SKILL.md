---
name: sieve-rules
description: Manage the user's sieve mail filters through the jmap-mail extension's local versioned store — paste-in → edit → emit → confirm for providers without a sieve API (Fastmail). Use when the user wants to create, change, review, or restore mail filter/sieve rules.
---

# Managing sieve mail filters

The `email_sieve_*` tools keep a **local, append-only, versioned store** of
the user's sieve documents. They never contact the provider. They are an
opt-in: if they are missing or refuse, ask the user to turn on
`enable_sieve_tools` for jmap-mail in `/extensions` (press `c`). For Fastmail
(no sieve API), the user is the transport: they paste current state in, you
return updated sections for them to paste back into
**Settings → Mail rules → Edit custom Sieve code**.

## Store model (what the tools give you)

- A *document* = one editable unit, keyed provider/account/name. **Names come
  from calibration (below) — never guess one.** A miss lists the documents
  that actually exist.
- Every change **appends** a version (`email_sieve_put`); `head` is newest.
  Every put also returns **advisory lint findings** and the `fileinto`
  targets it saw — verify those against `email_list_mailboxes` paths before
  emitting. `dryRun: true` lints and diffs without storing.
- `applied` marks the version last confirmed live at the provider
  (`email_sieve_mark_applied`). It refuses truncation-suspect content
  (`force: true` only if the flagged content truly is live) and refuses
  context-only documents always.
- `email_sieve_diff` defaults to applied → head = **what's pending to paste**.
- `email_sieve_restore` appends a *copy* of an old version — nothing is ever
  lost by a restore.
- `email_sieve_archive` moves a mistaken or superseded document out of the
  working set (nothing deleted; `unarchive: true` reverses;
  `email_sieve_list includeArchived: true` shows them).
- Origins: `paste-in` (verbatim from the user — always store their paste
  before doing anything else), `edit` (your change), `restore` (automatic).

## Large exports: import files, never re-type

A full provider export is far too big to pass through your own output
without loss — a real session produced a 1.5KB "placeholder" of a 67KB
export that way. Instead: have the user drop the export file **inside the
session working directory**, then

    email_sieve_put name: full-export-YYYY-MM-DD, origin: paste-in,
                    sourcePath: <workspace-relative path>, contextOnly: true

`sourcePath` refuses paths outside the workspace (same jail as your own file
tools). `contextOnly: true` marks it reference material: it stays diffable
for drift checks but can never be marked applied.

## First use: calibration (once per provider account)

1. Run `email_status`; note the canonical account id — pass it as
   `accountId` on the first puts (afterwards the store defaults to it —
   but if the user has more than one mail account, keep passing
   `accountId` explicitly on every put: the default is just "the only
   account with stored documents", which silently misfiles otherwise).
2. Ask the user to open the Fastmail custom-sieve screen and describe what
   they see: locked generated blocks (`### … {{{ … }}}`) interleaved with
   editable areas. **Verify the actual structure — do not assume a count.**
   One real Fastmail account calibrated to **three editable areas**:
   - `area-1-after-require-before-generated` — after the top-level
     `require [...]` line, before `### Sieve generated for save-on-SMTP
     identities {{{`. Runs **before spam protection** — the place for
     exemptions (e.g. `set "spam" "N";` for a trusted sender).
   - `area-2-after-spam-filing-before-rule-actions` — between
     `### Execute spam filing … }}}` and `### Calculate rule actions {{{`.
     After spam filing, before the rules-UI output.
   - `area-3-end-after-backcompat` — at the very end, after the generated
     `if string :is "${stop}" "Y" { … }` block.
   Name each area to encode its placement like the examples above.
3. Store each editable area verbatim (`origin: paste-in`, note
   "initial import"), including empty ones (empty content is fine).
   Optionally import the full export as a `contextOnly` document (see
   above) for whole-script context — never edit generated blocks; they can
   only change via Fastmail's UI.
4. `email_sieve_mark_applied` each document, passing the `version` the `put`
   just returned — the paste IS the live state. Never leave `version` off: it
   is not defaulted to head, because a later `put` would make head a version
   the provider has never seen.

## The editing loop

1. **Refresh** (start of any session that will edit): ask for a fresh paste
   of the areas you'll touch; `email_sieve_put` them (`origin: paste-in`).
   `appended: false` → local state matches live, proceed. `appended: true` →
   the script changed out-of-band (web UI edits, rules regeneration) —
   surface the `email_sieve_diff` to the user and reconcile before editing.
2. **Edit**: compose the new section; `email_sieve_put` with `origin: edit`
   and a one-line note — use `dryRun: true` first to see lint findings and
   the diff. Resolve every `error`-level finding; treat `warning`s as
   questions to answer in the note. For any new/changed condition,
   **preview against real mail** with `email_search` (for Fastmail
   `jmapquery` blocks, pass the embedded JSON straight to `filterJson`;
   `includeTotal: true` sizes the impact).
3. **Emit**: show `email_sieve_diff` (applied → head) plus the **full new
   section content**, labeled exactly where it goes: "replace the contents
   of editable area 1 (after require, before the generated blocks)".
   Fastmail validates at save — if it rejects, fix and re-emit; the store
   keeps every attempt.
4. **Confirm**: only after the user says it's saved,
   `email_sieve_mark_applied` **with the version number you emitted in step 3**
   — the one `email_sieve_diff` reported as `to`, not "head". A `put` between
   the emit and the confirmation moves head to a version the provider has never
   seen, and marking that one is invisible afterwards: every later diff is
   taken from the applied pointer, so a wrong pointer makes the store agree
   with itself forever. The tool refuses a call that names no version.
   Never mark applied on your own initiative.

## Sieve rules for what you write

- **Never use `discard`.** Fastmail warns it permanently drops mail,
  bypassing their backups. `fileinto "Trash"` (or Junk) routes identically
  and stays recoverable. (The linter flags this as an error.)
- **Mind `stop`:** a `stop` in an early area prevents every later block from
  running — including Fastmail's generated rules. Only use it when that is
  the explicit intent, and say so in the note. (Linted as a warning.)
- **`require` lines:** keep them at the top of the section and only for
  extensions the section actually uses — the linter cross-checks both
  directions. Fastmail supports a rich set (fileinto, envelope, body, regex,
  relational, variables, imap4flags, editheader, date, duplicate, mailboxid,
  special-use, vacation-seconds, `vnd.cyrus.*`, …) — when in doubt, prefer
  basic constructs; Fastmail's save-time validation is the authority. Where
  `require` is accepted across areas: treat the first save as the test and
  record what you learn in the document notes.
- Use real mailbox names/paths from `email_list_mailboxes` for `fileinto`
  targets — never guess folder names; check the put result's
  `fileintoTargets` against the real list.
- Comment non-obvious rules (`# why`), not what they do.

## Recovery

`email_sieve_list` shows every document's history state. To roll back:
`email_sieve_restore` the known-good version, emit it, have the user paste,
then mark applied — naming the version `restore` returned, which is a NEW
version holding the old content, not the number you restored from. The bad
version stays in history for the postmortem.
A document that should never have existed (failed import, wrong name):
`email_sieve_archive` it — out of sight, never deleted.
