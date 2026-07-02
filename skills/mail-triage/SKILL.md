---
name: mail-triage
description: Plan and execute mailbox cleanup with the jmap-mail extension — survey volume, identify candidate filter rules, preview their impact against real mail, organize the backlog with dry-runs, and verify results. Use when the user wants to get their inbox under control, reduce noise, or plan mail filters.
---

# Mail triage planning

A loop for turning "my inbox is a mess" into measured, verified changes.
Ground every step in real numbers — never guess at volume when
`includeTotal` can count it.

Check `email_status` first: it reports the account, the configured
`access_level`, and which tools are unavailable. At `read-only` you can plan
and preview everything but apply nothing — deliver recommendations plus the
exact `/extensions` change the user would make to let you execute
(`read-organize` adds mark/move/trash; nothing here ever needs destroy).

## 1. Survey — where is the volume?

- `email_list_mailboxes` (counts on by default): unread hotspots, oversized
  folders. Use the returned `path`s when reporting — `Inbox/Gaming/Star
  Citizen` reads better than an id.
- Slice the backlog with `email_search` + `includeTotal: true`:
  unread (`notKeyword: "$seen"`), stale unread (`before` a cutoff),
  mailing lists (`keyword: "$ismailinglist"`), high-volume senders.
- For each repeated sender worth a rule, get the real count:
  `email_search {from: X, includeTotal: true, limit: 1}` is a cheap counter.

## 2. Identify candidates

Good rule candidates: recurring senders the user never reads (file or
mark-read), newsletters (file to a folder), notifications that only matter
when fresh (file + let the user sweep), misfiled mail a provider rule
already targets but never reaches (spam filing runs first — check Junk for
false positives from senders the user does read).

Rank by measured volume × user annoyance. Present the shortlist with counts
and a proposed action each; let the user pick.

## 3. Preview — before any rule is written

- Translate each candidate condition into `email_search` and report exact
  totals plus 2–3 sample subjects (summaries only, never bodies).
- Fastmail UI rules embed their query as JSON in `jmapquery` blocks — pass
  that JSON verbatim to `email_search filterJson` to see precisely what an
  existing rule matches.
- Check both directions: what the rule catches (`filterJson` or from/subject
  filters) and what it would wrongly catch (search the same condition across
  `inbox` vs the target folder; `collapseThreads: true` approximates
  conversations affected).

## 4. Apply

Two independent tracks:

- **Filter rules** (future mail): hand off to the **sieve-rules** skill for
  sieve sections, or emit exact Fastmail UI rule instructions (Settings →
  Mail rules) when the UI rule builder suffices. Prefer UI rules for simple
  sender→folder cases — they survive Fastmail's regeneration; custom sieve
  for anything the builder can't express.
- **Backlog cleanup** (existing mail): `email_mark` / `email_move` /
  `email_trash`, always `dryRun: true` first and show the preview. For more
  than 20 messages the real run needs `confirm` — a bulk dry run includes
  the phrase as `confirmPhrase` (and a refused real run spells it out); copy
  it verbatim. The phrase is bound to the exact count, destination path, and
  account, so re-preview whenever the plan changes. Batch by search result
  pages (`hasMore` tells you when you're done; ids are stable across moves).
  Never `email_destroy` for triage — Trash is recoverable, destroy is not
  offered by this workflow.

## 5. Verify

After rules go live: on the next arrivals, search the target folder for the
sender/condition with `after` set to the apply date and confirm placement
(the field-tested example: a Junk-filed healthcare sender, exempted in
sieve, verified by searching Junk and the Healthcare folder for the next
reminder). Re-run the step-1 counts after a cleanup and report the delta —
"Inbox unread 2,763 → 214" is the deliverable.
