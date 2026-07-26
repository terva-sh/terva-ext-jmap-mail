# terva-ext-jmap-mail — implementation plan

Date: 2026-07-01
Derived from: the pre-build product plan (`jmap-mail-extension.md`) +
`terva-sh` extension conventions + terva SDK as of v0.112.0.

## Locked decisions

| Decision | Choice | Rationale |
|---|---|---|
| Tool prefix | `email_*` | The model reasons about "email", not the transport. JMAP is an implementation detail. Filter-era tools (stretch) will share the prefix. |
| Manifest `name` | `jmap-mail` | Registration name; independent of dir name `terva-ext-jmap-mail`. |
| Go module | `terva-ext-jmap-mail` | Matches sibling extensions (`terva-ext-memory`). |
| SDK pin | `terva.sh/terva v0.112.0`, **vendored** (`-mod=vendor`) | Newest release; ships the full authority taxonomy (incl. `local-data` since v0.109.0) and extension protocol 4 (tool withdrawal). Vendoring is the house convention: offline, self-contained installs. |
| Unconfigured behavior | Withdraw all tools at session boundary when protocol ≥ 4; clear `not configured` tool errors as the fallback / defense in depth | Resolves product-plan open question 2. |
| Authorities | Read tools: `ext.WithAuthority(ext.AuthorityNetworkRead)`. Mutating tools (phase 2+): `ext.AuthorityExternalMutate` + `ext.Sequential()` | Resolves open question 1 — the constants exist in the pinned SDK. |
| Context block | Yes, small (~6 lines) | See below. |
| Body caching | Never. Mailbox/session metadata cached in memory only for MVP (DataFS persistence deferred). | Resolves open question 6. |
| First delivery | Phase 0 + Phase 1 (read-only) only; stop for live-review against Fastmail before any mutating tool | User-confirmed checkpoint. |
| Config keys | Added per phase: MVP ships `session_url`, `api_token`, `default_account`, `max_body_bytes`. Later phases added `access_level` (superseding the v0.3.0 `allow_destructive` bool) and `enable_sieve_tools`; `allow_send` lands with the phase that reads it. | Keeps the `/extensions` form honest — no dead switches. |

### Context block (decided text, tune during review)

> The email_* tools give read-only access to the user's mailboxes over JMAP.
> Prefer email_search (summaries + previews) before fetching bodies; fetch
> bodies with email_get only for messages actually needed — bodies are
> truncated to a configured byte budget and results indicate truncation.
> Mailboxes may be referenced by role (inbox, trash, sent, …), display name,
> or id. Email ids are stable when messages move between mailboxes.

Why it earns its ~80 tokens: it encodes cross-tool *strategy* (search-then-get,
bounded bodies, mailbox addressing) that would otherwise be duplicated into
every tool description or rediscovered by trial and error each session. Tool
descriptions stay terse but mirror the essentials, since users can disable
context injection (`disable_context_extensions`) without losing the tools.

## Repository layout

```text
extension.json          manifest: name jmap-mail, exec ./run.sh, config schema
run.sh                  self-bootstrapping launcher, go build -mod=vendor
justfile                test / lint / fmt / build / install / try / ci / tidy
main.go                 ext.New, config schema-side wiring, tool registration, Run
app.go                  thin glue: args parse → internal/mail → JSON tool results,
                        tool visibility sync (OnSession/OnConfig)
internal/version/       Version const + test pinning it to extension.json
internal/config/        settings struct, validation, redaction (no SDK import)
internal/jmap/          protocol client: session discovery, request envelope,
                        result references, error taxonomy, limits (no SDK import)
internal/mail/          provider-neutral service over an interface around jmap:
                        status, accounts, mailboxes (+name/role resolution),
                        search, get, thread (no SDK import)
vendor/                 committed (go mod vendor)
docs/                   this plan; product plan stays at repo root
README.md               setup (Fastmail token), safety model, limitations, citations
```

`internal/*` packages never import the SDK — pure logic with unit tests;
`app.go`/`main.go` are the only SDK touchpoints (house invariant).

## Phase 0 — scaffold

1. Adapt template: manifest, launcher (vendored build, bin `./jmap-mail`),
   justfile (names + manifest-name-aware install recipe), `.gitignore`
   (`/jmap-mail`, `/bin/`).
2. `go.mod` → module `terva-ext-jmap-mail`, `require terva.sh/terva v0.112.0`,
   `go mod vendor`, commit `vendor/`.
3. `internal/version` + test against `extension.json`.
4. Minimal `main.go` registering nothing but proving handshake; `just test`,
   `just lint`, `just build` green.

Acceptance: loads under `terva --ext .`; no stdout writes; version test green.

## Phase 1 — read-only MVP

### Tools (all `AuthorityNetworkRead`)

| Tool | Backing JMAP calls | Notes |
|---|---|---|
| `email_status` | `GET session` | config state, chosen account, capabilities, core limits, mailbox-cache state. No mail content. |
| `email_list_accounts` | session object only | id, name, isPersonal, isReadOnly, mail/submission/vacation capability flags. |
| `email_list_mailboxes` | `Mailbox/get` | id, name, parentId, role, sortOrder, counts (optional), rights when present. |
| `email_search` | `Email/query` → back-reference → `Email/get` (one request) | one `FilterCondition` (mailbox, text, from/to/cc/bcc, subject, body, before/after, hasAttachment, keyword/notKeyword), sort newest/oldest by receivedAt, collapseThreads, position/limit (≤100, default 20). Summary fields + preview only — never bodies. |
| `email_get` | `Email/get` | ≤20 ids; bodyFormat text/html/both/metadata; `fetch*BodyValues` + `maxBodyValueBytes` bounded by min(request, config `max_body_bytes`); reports `isTruncated`. |
| `email_get_thread` | `Thread/get` → back-reference → `Email/get` (or `Email/get` first when given an email id) | summaries by default; bounded bodies opt-in. |

Input schemas as in the product plan (renamed). Results are compact JSON
objects; errors are `TextErrorResult` with actionable text, never tokens or
raw response dumps.

### internal/jmap client

- `Client{sessionURL, token, http.Client}`; `Session(ctx)` GET + parse + verify
  `urn:ietf:params:jmap:core`; `Call(ctx, using, invocations)` POST to `apiUrl`.
- `Invocation` marshals to the `[name, args, callId]` triple; `ResultReference`
  helper emits `{"#name": {resultOf, name, path}}` arguments.
- Error taxonomy: `AuthError` (401/403), `RequestError` (RFC 8620 §3.6.1
  problem JSON), `MethodError` (`error` invocations, §3.6.2), plus SetError
  surfacing for phase 2. Core limits parsed and enforced client-side
  (`maxObjectsInGet`, `maxCallsInRequest`, …).

### internal/mail service

- Owns account selection (id → name/email → `primaryAccounts[mail]` → error)
  and a mutex-guarded session + mailbox cache (in-memory; invalidated on
  config change, refreshed on resolution miss).
- Mailbox resolution order: exact id → role (case-insensitive) → display name
  (case-insensitive; ambiguity is an error listing candidates).
- Body assembly: walk `textBody`/`htmlBody` part lists against `bodyValues`,
  concatenate, propagate `isTruncated`.

### Wiring (app.go / main.go)

- `OnConfig`: rebuild client + invalidate caches; log only shape
  (`has_api_token=%v session_url=%s`), never values.
- `OnSession`: sync tool visibility (withdraw all when unconfigured,
  protocol ≥ 4); re-asserting unchanged state is a host-side no-op.
- Every handler re-checks configuredness (fallback error path).

## Testing

### Unit (default, `just test`, no network)

`httptest` server for `internal/jmap`; a fake `Caller` interface for
`internal/mail`. Coverage mirrors the product plan list: config
validation/redaction, session parse + account selection + capability checks,
envelope/result-reference construction, error taxonomy (401/403/request/method
errors, no secrets in messages), filter mapping, mailbox resolution +
ambiguity, body bounding/truncation, limits enforcement.

### Spec-compliance suite (user-requested)

A `speccompliance` test set inside `internal/jmap` asserting our wire behavior
against RFC 8620/8621 requirements, fixture-driven:

- Envelope: `using` lists exact capability URNs; `methodCalls` are ordered
  triples; `Content-Type: application/json`; bearer auth header present.
- Session: parse fixtures modeled on RFC 8620 §2 (accounts, primaryAccounts,
  capabilities, limits); reject sessions missing core capability.
- Result references per §3.7 (`#ids` shape, path syntax).
- Request-level error per §3.6.1 (`urn:ietf:params:jmap:error:*` problem
  types); method-level `error` per §3.6.2 (`unknownMethod`,
  `invalidArguments`, `accountNotFound`, …).
- Mail semantics per RFC 8621: `Email/query` filter property names (§4.4.1),
  `Email/get` body-fetch arguments (§4.2), keyword names (`$seen`,
  `$flagged`), and (phase 2) trash semantics — mailboxIds swap per §4.6 /
  the delete-to-trash guidance.

Each fixture cites the RFC section in a comment so drift is auditable.

### Hermetic integration suite (added 2026-07-02)

`internal/jmaptest`: an in-memory JMAP server (RFC 8620/8621 read subset —
session, envelope, result references with wildcard flattening, Core/echo,
Mailbox/get, Email/query, Email/get, Thread/get) over a seeded store.
Independence rule: it must never import `internal/jmap` — it parses raw JSON
against the RFC wire shapes so symmetric client/server bugs can't cancel out.
Its own tests pin protocol behaviors (problem types, method errors,
invalidResultReference, limit enforcement) with raw HTTP.
`internal/mail/integration_test.go` drives the real client+service stack
against it in plain `just test`. Phase 2 grows the fake with `Email/set`
(keyword patches, mailboxIds moves, destroy, SetError maps) before any
mutating tool touches a real account. Follow-up (deferred): a Stalwart
docker-compose as an independent reference implementation for interop tests.

### Live tests (opt-in, never CI)

Env-gated (`JMAP_TEST_SESSION_URL`, `JMAP_TEST_API_TOKEN`,
`JMAP_TEST_ACCOUNT`); **read-only assertions only** in phase 1 against the
user's real Fastmail account: session fetch, mailbox list, small inbox search,
single bounded body fetch. Mutating live tests wait for phase 2 + a dedicated
test mailbox (`JMAP_TEST_SAFE_MAILBOX`, `JMAP_TEST_ALLOW_DESTRUCTIVE=1`).

### Manual dogfood checklist

As in the product plan (§Testing): install, `email_status`, mailboxes, search
limit 5, bounded fetch, log audit for tokens/bodies, live config update.

## Phase 2+ (phase 2 delivered 2026-07-02)

- Phase 2 (done, v0.2.0): `email_mark`, `email_move`, `email_trash` —
  `AuthorityExternalMutate` + `Sequential()`, dryRun on everything, bulk
  threshold >20 requires the exact generated `confirm` phrase (case-insensitive;
  the refusal spells it out), SetError partial-result reporting split into
  `failed`/`notFound`. Move defaults to replacing mailboxIds; `keepInMailboxes`
  patches additively. Trash = mailboxIds swapped to the `role=trash` mailbox
  (RFC 8621 delete-to-trash). Each mutation batches Email/get (state snapshot →
  changed/alreadySet/from reporting) with Email/set in one request. jmaptest
  grew Email/set (atomic patches, orphan/unknown-mailbox rejection, destroy,
  ifInState/stateMismatch, requestTooLarge) and a live state counter.
- Phase 3 (done, v0.3.0): `email_destroy` behind the `allow_destructive`
  config key (default off). Ladder: withdrawn while disabled (protocol ≥ 4;
  handler refuses regardless), targets must be only-in-Trash unless
  `allowNotInTrash:true`, and every non-dry run requires the exact phrase
  `destroy N emails permanently` — checked before any network call; dryRun
  previews candidates/blockers and returns the phrase. Real runs are two round
  trips (mailbox snapshot → destroy) because the Trash gate needs current
  state. Visibility is now three-state: unconfigured → all withdrawn;
  configured → destroy-only withdrawn; + allow_destructive → all visible.
- Phase 4 (research done 2026-07-02, see
  [filters-research.md](filters-research.md)): the filters path is
  **RFC 9661 (JMAP for Sieve)** — ManageSieve is dead at Fastmail, the MCP
  bridge is unnecessary. **Probe answered:** Fastmail does not advertise
  `urn:ietf:params:jmap:sieve` (observed live: core, mail, submission,
  contacts, maskedemail). Fastmail filter editing is out of scope; RFC 9661
  tools are deferred and would target Stalwart (interop layer) first.
- Phase 1 live acceptance met 2026-07-02: the read-only suite passed against
  a real Fastmail account (status/account selection across two accounts, 51
  mailboxes, inbox search, 300-byte bounded fetch with truncation reported,
  thread fetch via chained wildcard result references).
- Phase 5: submission/vacation (`allow_send`), OAuth, autodiscovery — per
  product plan; each gets its own plan before code.

- v0.5.0 (2026-07-02): the `allow_destructive` bool is superseded by the
  **`access_level` select** — `read-only` (default) / `read-organize` /
  `read-organize-destroy` — a monotonic ladder driving both tool withdrawal
  and handler-level refusals. Local sieve tools remain available at every
  level.
- v0.6.0 (2026-07-02): sieve tools moved behind their own opt-in,
  `enable_sieve_tools` (default off), independent of `access_level`.

## Phase A–C: field-report improvements (2026-07-02)

Driven by the first real agent session against a live Fastmail account
(`terva_home/reports/jmap-mail-extension-findings-2026-07-02.md`) plus its
raw transcript. Three waves:

- **Phase A (done, v0.7.0) — read-side ergonomics + truth-in-docs.**
  `email_search`: `includeTotal` → RFC 8621 §4.4 `calculateTotal` (opt-in,
  exact `total`), always-on `hasMore` via a limit+1 probe, results assembled
  in query order (Email/get order is not guaranteed, RFC 8620 §5.1), and
  `filterJson` — a raw Email/query filter passthrough (FilterCondition or
  AND/OR/NOT FilterOperator; Fastmail's generated `jmapquery` blocks paste in
  verbatim, making rule preview mechanical). Only `mailbox` composes with it
  (ANDed in). Mailboxes gained computed display `path`s, accepted as input
  refs everywhere (`id → role → path → name`); ambiguity/miss errors list
  paths. `email_status` now reports `accessLevel`, `enableSieveTools`, and
  `unavailableTools` + hint, so an agent can tell config gating from
  malfunction (the report agent couldn't). jmaptest grew FilterOperator
  evaluation. Stale `allow_destructive` doc references corrected.
- **Phase B (done, v0.8.0) — sieve workflow hardening.** `email_sieve_put`
  `sourcePath` file import **jailed to the session CWD** (the transcript
  showed the host's own read tool is jailed; the extension must not become a
  bypass — symlinks resolved before the prefix check, regular files only,
  the 256KB cap, UTF-8 required). Advisory lint on every put (+ dryRun put =
  lint+diff without appending): comment/string/text:-block-aware balance
  checks, `require` coverage both directions, `discard` = error, `stop` =
  warning, `fileinto` targets returned as data for the agent to cross-check —
  the sieve tools stay zero-network. Truncation-marker guard on put content
  *and* note; `mark_applied` refuses suspect versions without `force` (the
  report agent's placeholder was 1.5KB of a 67KB export with a model-invented
  `[TRUNCATED …]` marker). `contextOnly` documents (mark_applied always
  refuses) + a seventh tool `email_sieve_archive` (moves under a per-account
  .archive/ subtree, hidden from list unless includeArchived, unarchive
  reverses, nothing deleted). Missing-document errors enumerate the account's
  documents (the agent had guessed the skill's example name). SKILL.md
  recalibrated to the observed 3-area Fastmail structure with the calibrated
  document names as the worked example.
- **Phase C (done, v0.9.0) — additive.** Body URL redaction in `email_get`
  and `email_get_thread` — **on by default** per user decision: query
  strings, fragments, userinfo, and token-like path segments (16+ chars,
  base64/hex-ish with a digit) stripped from every URL in fetched bodies;
  hosts and readable paths survive; `redactedUrls` counts changes;
  `includeFullUrls: true` opts out for verification-link tasks; bodies
  re-truncated if marks nudge them past the budget. Plus the **mail-triage**
  skill: survey (counts via includeTotal) → candidate rules → preview
  (filterJson for jmapquery, both-directions checks) → apply (UI rules vs
  sieve handoff to sieve-rules; dry-run organize for backlog) → verify
  (post-apply searches, before/after counts).

## Review waves (2026-07-02, v0.9.1–v0.10.1)

A four-lens comprehensive review (mutation safety, interface consistency,
sieve store durability, client/secrets/redaction — two lenses verified
findings by executing the suspect code) produced 23 findings, fixed in three
waves:

- **v0.9.1 (high):** previews redact unconditionally; case-insensitive +
  IPv6-aware URL regex; destroy bound to its snapshot via `ifInState`;
  atomic sieve pointer writes, prune disabled on corrupt applied pointer,
  O_EXCL never-overwrite version files, corrupt docs surfaced in listings.
- **v0.10.0 (medium):** confirm phrases bind count + destination path +
  account (destroy: + id-batch digest + gate mode); email_mark bulk gate;
  bulk dry runs return `confirmPhrase`; server-supplied apiUrl and redirect
  targets must pass the https-or-loopback policy; sieve Put refuses over an
  archived namesake; provider store key uses hostname (port-safe).
- **v0.10.1 (low batch):** role-only trash resolution (no name fallback, for
  email_trash and the destroy gate); mutations resolve mailboxes against a
  fresh fetch (cache staleness cannot misroute a move); set-level notFound
  ids reach the report; get-parse failures after a real set say the mutation
  may have applied; lint text:-terminator and fileinto scan-bound fixes;
  newline-only diffs render a marker instead of "identical"; log lines strip
  URL userinfo; docs/error-text consistency sweep.

Accepted, documented residuals: hard-wrapped URLs redact only their first
line; a body-budget cut inside a path token can drop it below the
opaque-segment threshold; same-host-different-port JMAP servers share one
sieve store; macOS case-insensitive filesystems fold doc-name case.

## Field-report wave 2 (2026-07-25, v0.11.0)

Three unrelated findings from an agent running the extension against a live
Fastmail account: a bulk-organization payload report (3,000 messages archived
in 200-message batches — ~321KB of tool output per batch, twelve operator
prompts, four compactions) plus two review findings read at the v0.10.1 tag.

- **Bulk payloads: ask for less.** `email_search` gained `fields`, a
  projection over `EmailSummary`'s own property names that maps onto the
  `Email/get` `properties` argument — so it saves provider bandwidth, not
  just context. `fields:["id"]` is the case that matters and is special:
  `Email/query` already returns exactly that, so the get is skipped
  altogether (ids destroyed between query and get therefore reach the caller
  and surface as `notFound` on the organize call — the right place for them).
  A projection that omits `preview` raises the page cap from 100 to 500
  (`maxProjectedSearchLimit`): the cap exists to bound payload, and preview
  is nearly all of it. `mailboxes` is the one projected field with a cost
  beyond the get — it drives the annotation lookup — so an id-only caller
  never pays it.
- **Paging correctness, a prerequisite for that cap.** `hasMore` came from a
  limit+1 overfetch probe, which a server enforcing its own smaller limit
  truncates away — at 500 that would read a provider-side cap as "no more
  matches" and end a paging loop mid-backlog, silently. The Email/query
  response's `limit` (RFC 8620 §5.5) is now honoured: where the server
  capped, the applied page size comes back in the result and a full page
  means "assume more". Over-reporting `hasMore` costs one empty page; the
  other way round costs unnoticed messages.
- **Organize results: counts above the bulk threshold.** `email_mark` /
  `email_move` / `email_trash` results carry `changedCount` /
  `alreadySetCount` / `movedCount` plus, for moves, a `movedFrom` breakdown
  keyed by source mailbox display path. At or below `bulkConfirmThreshold`
  the per-message lists stay (there the list IS the answer); above it they
  are dropped unless `verbose:true`, and `verbose:false` suppresses them at
  any size. `failed` and `notFound` are never abridged — they are the
  actionable exceptions — and the dry run still returns `confirmPhrase`,
  which was the only reason a bulk dry run was mandatory. Net effect on the
  reported workload: ~321KB per 200-message batch → a few KB.
  `email_destroy` deliberately keeps its full enumeration: small, always-
  confirmed batches where the list is the point.
- **`HTTPError` snippet can no longer reflect the bearer token.** A non-2xx
  response that is neither 401/403 nor parseable problem JSON quoted up to
  200 characters of the body into the error the tool caller sees; a provider,
  proxy, or WAF that echoes the request's `Authorization` header would put
  the token there. `snippet` now scrubs the token — and any `Bearer …` echo,
  including of a credential we do not hold — **before** the length bound, so
  a cut cannot leave a token fragment behind. `RequestError.Detail` gets the
  same treatment. Residual: `MethodError.Description` is not scrubbed; it
  comes from a well-formed JMAP method response, not the proxy-echo path.
- **`email_get_thread` was the same defect, unfixed.** `email_get` caps at 20
  ids because a result must be bounded; `GetThread` fetched every message of
  a thread with no equivalent, and `includeBodies` gave each one the full
  per-message budget — a long mailing-list thread was the largest payload
  this extension could produce, reachable at `read-only` with no bulk
  operation at all. Now capped at `maxThreadSummaries` (100) /
  `maxThreadBodies` (= `maxGetIDs`, 20), caller-lowerable via `limit`, with
  `count` (the thread's real size), `returned`, and `omitted` reported so a
  trimmed thread is visible rather than silent. Messages are ordered by the
  Thread's own `emailIds` (RFC 8621 §3: receivedAt order) instead of
  Email/get's unspecified order, and the cap keeps the newest end. `fields`
  projects the summary form; combined with `includeBodies` it is refused
  rather than silently ignored, since bodies dominate that result anyway.
  Residual: a result reference cannot be sliced, so the provider still sends
  the whole thread — the cap bounds what reaches the caller, which is where
  an unbounded thread does the damage.
- **`queryState` is echoed on search results.** RFC 8620 §5.5's opaque query
  state changes when the matching set changes, so a caller paging a backlog
  while mutating it can tell that its cohort moved rather than discovering
  the gap later, or never. Paired with a policy line naming the one real
  rule: if the change removes messages from the filter, re-query at position
  0 and never advance position; if it does not, advance position and don't
  re-query; mixing them puts holes in a wave. (jmaptest's Email/query now
  derives queryState from its mail state, which is what the spec requires and
  what makes the drift detectable in tests.)
- **A wave's payload is now a tested property.**
  `TestIntegrationBulkWavePayloadBudget` runs the real 200-message
  ids→dry-run→apply loop against the hermetic server, measures every tool
  result, and fails if the total passes a per-message budget or exceeds a
  single unprojected page. Measured: 4,176 bytes for 200 messages (~20
  B/message) against the field report's ~321KB per 200-message batch. The
  point is not the number but the ratchet — a property added to
  `EmailSummary`, or an enumeration put back into an organize result, now
  fails a test instead of quietly costing an operator a context window.
- **`contextPolicy` states that message content is untrusted.** It covered
  truncation, URL redaction, mailbox addressing, access levels, dryRun/confirm
  and destroy safety, but never framed subjects, previews, bodies, and sender
  names as data rather than instructions — and at `read-organize` the model
  holds mark/move/trash, with runs of 20 or fewer needing no confirm phrase.
  The extension is the only component that knows the content is
  attacker-supplied; a host persona may or may not say so, and a default
  install said nothing.
- **The README now says what the confirm phrase is not.** Following directly
  from the finding above: the phrase is a deterministic function of the
  operation and the refusal prints it, so anything that can drive a tool call
  can produce one — including a model acting on injected instructions. It is
  a deliberation gate against careless batches, not an authorization
  boundary; the boundaries are `access_level` and the host's gating on
  `external-mutation` authority. Documented rather than hardened: minting a
  per-dry-run nonce would close it, at the cost of the "refusal spells out
  the phrase, recovery is one re-run" ergonomic the field report valued.
  Open decision, deliberately not taken here.

## Distribution: prebuilt binaries (2026-07-25, v0.12.0)

Until now `run.sh` only ever compiled, so installing the extension required a
Go toolchain on the host, and the GitHub releases carried no assets at all
(v0.10.1: zero). The launcher now prefers a **verified** prebuilt binary and
keeps the source build as the fallback:

- **`.github/workflows/release.yml`** (the repo's first CI) runs on every `v*`
  tag push: re-checks the tag against `extension.json` and
  `internal/version.Version` and fails the release on a mismatch, runs the
  race tests, cross-builds darwin/linux × amd64/arm64 (`CGO_ENABLED=0`,
  `-mod=vendor -trimpath` — pure Go, so one runner covers every target), and
  uploads the four binaries plus `SHA256SUMS`. `workflow_dispatch` takes a tag
  for backfills; `just release-assets vX.Y.Z` is the local fallback. The job is
  gated on `github.server_url == 'https://github.com'`: the file ships, so it
  also reaches the private Forgejo remote, which reads `.github/workflows/` and
  has Actions enabled for this repo — without the gate, a runner registered
  there later would run release jobs against the wrong host.
- **`run.sh`** fetches `jmap-mail_<os>_<arch>` for the manifest's version and
  installs it **only** on a SHA-256 match against the release's `SHA256SUMS`,
  staging inside the install dir so the final move is an atomic rename — an
  unverified or half-written file is never in a position to be exec'd. A
  mismatch is reported loudly and discarded. Everything else — no asset, no
  network, no curl/wget, or a source tree with local `.go`/`go.mod`/`vendor`
  changes — falls through to `go build -mod=vendor`, so an offline host and a
  developer mid-edit both keep working. `JMAP_MAIL_BUILD_FROM_SOURCE=1` opts
  out entirely; `JMAP_MAIL_RELEASE_BASE` repoints the fetch at a fork or an
  internal mirror.

The trust posture is deliberately explicit: the extension now executes code it
downloaded, so the checksum gate, the loud refusal, the source fallback, and
the opt-out are the load-bearing parts — not the convenience. All five paths
(verified install, tampered asset, missing asset, opt-out, local edits) were
exercised against a `file://` release before this shipped.

Consequence for the release flow: **a tag is not finished until its assets
exist**, because until then every install silently falls back to compiling.
`docs/release-process.md` steps 6 and 8 cover waiting for them and verifying
the path a user actually takes.

## Field-report wave 3 (2026-07-25, v0.13.0)

The same fleet, re-measuring v0.11.0 against a live Fastmail account. The
projection and counts-only work held: a 2,000-message archive wave ran in one
context window on one instruction, no compactions, per-batch cost down ~17×.
What was left was a shape problem. An archive batch is `email_search` →
`email_move` dry run → `email_move` apply, and **all three carried the same 200
ids** — 88% of the batch payload, 81% of the session's output tokens, and a
34–39-second stall before each of the two calls the model had to *generate* the
list into. Twelve of the wave's seventeen minutes were the model retyping ids
it had just been handed.

- **Selection handles and apply receipts** (`internal/mail/handles.go`). A
  search result carries a `selectionId` naming its ordered id set; a dry run
  returns a `receiptId` naming the previewed set plus the resolved operation.
  `email_move` / `email_mark` / `email_trash` take `selection` or `receipt`
  in place of `ids`, and a receipt replaces the confirm phrase as well — it
  *is* the preview the phrase exists to force. State is in-memory,
  per-process, TTL-bounded (15 min), capacity-bounded (64 each), and dies with
  the service, which is to say with any config change. It never touches disk:
  a handle that outlived the reasoning that produced it would be worse than no
  handle. Losing one costs a re-search.
- **This closes the open decision left at v0.11.0.** That entry noted the
  confirm phrase binds count, destination and account but not *which
  messages*, and that a per-dry-run nonce would close it at the cost of the
  "refusal spells out the phrase, recovery is one re-run" ergonomic. Receipts
  are that nonce, and the trade turned out to be avoidable: the phrase path is
  untouched, so the recovery ergonomic survives for anyone passing ids, while
  the receipt path is both cheaper *and* strictly bound. The phrase itself now
  also carries `idBatchDigest` — the same order-independent fingerprint
  `email_destroy` has used since it shipped — so even the ids path cannot
  confirm a different batch of the same size to the same destination.
- **Placement-drift detection instead of the proposed `queryState` refusal.**
  The report asked for a selection whose `queryState` had advanced to be
  refused. Two problems: JMAP methods in a batch run in order, so an
  `Email/query` alongside the `Email/set` reports state *after* the mutation
  (a guard would need its own round trip); and on a live mailbox `queryState`
  advances whenever any mail arrives, so the guard would fire constantly for
  reasons unrelated to the named ids. What the receipt does instead is
  fingerprint each message's placement (or, for mark, the target keyword) as
  the dry run saw it, and compare at apply time — the actual subjects, no
  extra round trip, no false positives from unrelated churn. Drift is
  **reported** (`drifted`, never abridged, with a note) rather than refused:
  the caller named these exact ids, and vetoing 200 messages because one moved
  would fail far more often than it would help.
- **TW-025's `operationId` collapses into the receipt.** The report wanted
  every mutating call to return an operation id that could be re-presented to
  learn whether the work had landed. But its own failure case is "the tool
  result was lost", and a server-generated id is lost with it — the caller
  cannot present what it never saw. An idempotency key has to be something the
  caller held *before* the apply, which is exactly the receipt. So a receipt
  is consumed once: re-presenting an applied one returns the original result
  with `replayed: true` and `appliedAt`, touching no network. A concurrent
  second apply is refused while the first is in flight; a *failed* apply
  releases the claim, because move and mark are idempotent at the provider and
  retrying after an ambiguous error costs nothing.
- **Flat id serialisation.** An id-only projection returned
  `[{"id":"…"}, …]`; results are rendered with `MarshalIndent`, so that is ~40
  bytes per message against ~20 for the bare string — measured at 8,113 bytes
  for 200 ids, against the field report's 8,273 for the same page. It now
  returns a flat `ids` array and omits `emails` entirely (an empty `emails`
  beside a populated `ids` reads as "nothing found"). Every other projection
  is byte-identical; the one visible change elsewhere is that a zero-result
  search omits `"emails": []`, which `returned: 0` already said.
- **`maxProjectedSearchLimit` (500) reconciled with `maxSetIDs` (200).** Not by
  raising the mutating cap — that is one of the invariants the report itself
  asked not to be traded — but by slicing: a mutating call takes `selection`
  plus `selectionOffset`, consumes at most 200 from there, and reports
  `selection.remaining`. One 500-id search now feeds three batches. Because
  the ids were pinned at search time, the later slices are unaffected by what
  the earlier ones moved — which is the entire reason the
  re-query-from-position-0 discipline exists, so within one selection it stops
  being necessary.
- **`email_list_mailboxes` gained a selector and a projection** (TW-024).
  Reconciling a wave needs four integers and was costing 12,644 bytes of every
  folder on the account, twice per wave — 14.7% of the session's tool output.
  `mailboxes:` narrows by the same id/role/path/name resolution used
  everywhere else (a reference matching nothing is an error, never a silently
  shorter list, or a reconciliation compares the wrong numbers); `fields:`
  projects over `Mailbox`, and a projection naming no count field stops the
  server from computing counts at all. Both default to v0.12.0 behaviour. The
  fetch is still of the whole tree, because a display path is computed from the
  parentId chain — the narrowing is about what crosses back to the caller,
  which is where the cost was.
- **`email_destroy` deliberately takes no handles.** Symmetry would argue for
  it; the friction of naming ids outright is worth keeping on the one
  unrecoverable operation, and its phrase has always been id-bound. A model
  that tries `selection:` there gets a clean schema rejection.
- **The wave budget test now measures arguments too.** Results were the only
  thing `TestIntegrationBulkWavePayloadBudget` counted, which is precisely how
  a regression whose cost is 88% arguments went unnoticed. It now runs the
  same 200-message wave twice — once retyping ids, once naming a selection and
  a receipt — and fails if the handle path is not comfortably cheaper.
  Measured: 4,375 bytes against 10,082, with the short fake ids understating
  the gap since a real provider's are twice as long.

## Field-report wave 4 (2026-07-26, v0.13.1)

v0.13.0's handles were unusable by the model the fleet actually runs, and the
whole defect was one schema keyword. `targetProperties` declared `ids` with
`minItems: 1`, so `[]` — the value meaning "not naming ids this way" — was
schema-invalid. A model that pads every declared property rather than omitting
keys therefore had to put *something* in it, and everything it put there
(`["placeholder"]`, `["dummy"]`, `["x"]`, `[""]`) read as a real selector. Every
selection-based call was refused as naming two selectors at once. Wave 3 was
authorised and archived nothing: twenty `email_move` calls, nineteen rejected.

The validation was right, the error message was accurate, nothing upstream was
rewriting the schema. Absence was simply inexpressible for one of three mutually
exclusive parameters — and it was the one whose padded form is indistinguishable
from a real answer. `""` had made `selection` and `receipt` inert by accident of
being strings; `ids` got no such luck.

- **`minItems: 1` dropped from `ids`** in `targetProperties` (`maxItems: 200`
  stays — a real bound, not a floor), and the description now says outright that
  `[]` and an absent key are equivalent. `email_destroy` keeps both `minItems`
  and `required: ["ids"]`: it accepts no handles, so `ids` is genuinely
  mandatory there and no padding conflict is possible.
- **`resolveTargets` normalizes before counting.** Blank and whitespace-only
  entries are dropped from `ids` (no provider has an empty-string message id, so
  one can never name a message), and `selection`/`receipt` are trimmed. A list
  that held nothing usable counts as unset rather than as a competing selector.
- **The two-selector refusal now spells out the corrected calls**, quoting the
  caller's own handle: *"send ids as [] (or omit it): {"selection": "sel_…"}"*.
  A model that lands there is padding, not confused about the contract, so
  restating the rule leaves it varying the placeholder and retrying. This is the
  same lesson terva learned with `session_inspect`'s `expand: 0`, where zero was
  both a padding value and a meaningful one.
- **A schema test pins the invariant**, since the bug lived entirely in the
  schema and no Go test could have caught it: the organize schemas must not set
  `minItems` on `ids`, must keep `maxItems`, and `email_destroy` must keep both.
- **`contextPolicy` names the inert values**, because the schema description is
  not the only place a model looks.

The general rule, worth applying to anything added later: **when a tool offers
mutually exclusive parameters, every one of them needs a representable "not this
one" value, and the schema must permit it.**

## Field-report wave 5 (2026-07-26, v0.14.0)

Two payload findings (TW-032, TW-033) and — because the wave 4 rule had never
been checked against anything but the tools that produced it — a sweep of every
schema for the same defect.

**TW-032: the search still returned the ids the handle replaced.** v0.13.0's
selection handles meant a caller stopped *sending* two hundred ids; it kept
*receiving* them, because `fields: ["id"]` is the narrowest projection the tool
had and `id` is always included. Across two waves that was ~4,000 ids into the
transcript whose only consumer was the `selectionId` printed beside them.

- **`returnIds`** — `""`/`"all"` (today), `"none"` (the handle, the counts, the
  total and `queryState`, no ids), `"boundaries"` (adds `firstId`/`lastId`, so a
  bounded wave can still prove afterwards where each batch began and ended).
- **A string enum rather than the proposed boolean.** One parameter covers all
  three states, `""` is inert, and there is no interaction rule between two
  flags to get wrong. The report's own constraint — the flag must default to
  today's behaviour, so a padded value changes nothing — is what rules out
  `returnIds: true` as a boolean: the padded `false` would suppress.
- **It overrides `fields` rather than conflicting with it.** No per-message
  property is returned either way, so refusing the combination would only refuse
  a caller that padded `fields` with `[]` — wave 4's mistake, one release later.

Measured on the same 200-message wave the previous two waves used: **1,491 bytes
of tool traffic, against 4,375 with handles alone and 10,082 retyping ids** — 7
bytes per message organized, down from 50.

**TW-033: `email_get` had no projection.** It was the only message-returning
tool without one; `bodyFormat: "metadata"` reads like one and is not, dropping
bodies while returning every summary property. A four-message placement check
therefore cost ~3.5KB of third-party sender names, addresses, subject lines and
previews — untrusted content pulled into a durable session record for a question
that needed `id`, `mailboxes` and `keywords`.

- **`fields` over a superset of `email_search`'s vocabulary**, adding `cc`,
  `bcc`, `replyTo`, `attachments`, `bodyText` and `bodyHtml`. A projection valid
  on the search is valid here, so one list carries across the two tools, and the
  properties only `email_get` returns are nameable rather than all-or-nothing.
- **The projection decides the bodies, and `bodyFormat` stops applying.**
  Refusing the combination the way `email_get_thread` refuses `fields` +
  `includeBodies` would have been wave 4 again: `includeBodies` defaults to
  false and is safely padded, but `bodyFormat`'s inert `""` resolves to `text`,
  so every projected call from a padding model would have been told it conflicts
  with a body format it never chose.
- **A projection that omits `mailboxes` skips the `Mailbox/get` behind it** —
  the one summary property that costs a second provider call.

**The sweep.** Wave 4's rule generalizes past mutually exclusive parameters:
*anything the code reads as "unset" must be a value the schema permits, and no
parameter's padded value may be an active choice.* Applying it found five more
instances, none of which had been reported:

- **`limit` declared `minimum: 1`** on `email_search` and `email_get_thread`,
  while both handlers read `0` as "the default". Identical in mechanism to the
  `minItems: 1` that cost Wave 3, and identically silent — the model never sees
  the text of a schema violation.
- **Every string enum omitted `""`**, which each handler already resolves to the
  default (`sort`, `bodyFormat`) or refuses by name (`action`, `origin`). Adding
  it turns an unreadable validation failure into the tool's own error, which
  names the choices. `email_sieve_restore`'s `version` got the same treatment
  for the same reason.
- **`hasAttachment` was a presence-based boolean** — the one case where the
  padded value was not merely unrepresentable but *actively wrong*. `false` is a
  filter excluding every message that has an attachment, applied silently, with
  nothing in the result to notice it by. It is now a `"" | "yes" | "no"` string;
  booleans are still accepted so nothing that already works breaks.
- **Blank entries in `fields` and `mailboxes`** were "unknown field" and
  "matches nothing" errors. `[""]` now reads exactly like `[]`, as it already
  did for the organize `ids`.

Pinned by three tests that read the schemas rather than the code, since these
defects live entirely in JSON strings: no string enum without `""`, no integer
floor above zero, `hasAttachment` not a boolean. Plus `padding_test.go`, which
calls each tool with every optional parameter set to its inert value and asserts
the result equals the unpadded call's.

## Field-report wave 6 (2026-07-26, v0.15.0)

Waves 5 and 6 confirmed v0.14.0 in production on identical work — the same
4,000 messages in the same twenty batches as Waves 3–4. `email_search` results
fell 91% (88,568 → 8,179 B), the four-message placement check 72%, all mail
traffic 63% (128,388 → 47,467 B). One finding came back out of it.

**TW-036: `email_move` identified its destination by id and its source by
label.** The result carried `destination: {id, name, role}` and
`movedFrom: {"Inbox": 200}` — a display name and a count. JMAP names are
user-editable and repeat across parents; ids are neither, which is exactly why
the destination had the full triple. So a mutation's own output could assert
where mail went and not where it came from, and every ledger row of the two
waves asserted ``Inbox `P-F` → Archive `P3V` `` with the source half inferred
from a baseline `email_list_mailboxes` carried across twenty batches. That is
sound for a quiescent single-`Inbox` account, and it fails silently: a rename
between the baseline and batch 10 produces a record that is wrong and
internally consistent.

Sharper than the report put it: on a bulk run `Moved` is dropped, and the
per-message `From []MailboxRef` inside it — which *did* carry ids — goes with
it. The id information existed on the small-run path and was missing exactly
on the bulk path where the ledger gets written.

- **`movedFrom map[string]int` → `sources []MailboxCount`**, each entry a
  `MailboxRef` (id, name, path when nested, role) plus a count — the same shape
  as `Destination`, which is the report's own framing. Replaced rather than
  added beside: a parallel array would be redundant payload two releases after
  spending two of them cutting bytes. Ordered by count descending, ties on path
  then id, so an identical move always renders identical bytes and a committed
  ledger shows no spurious diff.
- **`Destination` now carries its `path`** when it differs from the name — the
  same rule `mailboxRefsByID` applies, and the reason the confirm phrase has
  always quoted the path rather than the name.
- **Costs +156 bytes on the 200-message wave** (1,491 → 1,647 B, +10%), all of
  it on the two move results. Worth it: the alternative is a record that cannot
  be checked without a second call that was never made.

**The confirm phrase deliberately does not name the source**, against that
acceptance criterion. The phrase is minted before any provider round-trip that
could reveal where the messages are, and the real run must recompute the
identical string to validate it — so embedding the sources costs an extra
`Email/get` on every bulk call, *including the refusal path whose only output
is an error*, or splits the atomic `Email/get` + `Email/set` batch and opens a
drift window the tool itself created. It also buys nothing: the phrase already
binds the exact id set through `idBatchDigest`, which is strictly stronger — a
batch may span several mailboxes and one message may sit in several at once, so
a source name would be lossy where the digest is not. The dry run reports
`sources` beside `confirmPhrase`, which is where an operator reads them.
`sources_test.go` pins this as a decision rather than an oversight.

**`email_mark` has no equivalent to fix**, against the report's "same for
`email_mark`". Marking changes keywords, not placement: `MarkResult` has
`changedCount`/`alreadySetCount` and `MarkChange{id, subject}`, and no source
mailbox concept to report. `email_trash` does route through `moveInto` and
gained `sources` with it.

### The self-review TW-036 prompted

TW-036 is an instance of a rule, so every serialized result was audited against
it: **a result must be sufficient on its own to say what happened, using
identifiers that cannot silently change meaning — never a label where the tool
already holds the id, and never a bare id where the tool already holds the
state that made it interesting.** Two more instances, neither reported:

- **`Drifted` reported bare ids.** Drift is detected by comparing the placement
  (or keyword state) the dry run recorded against the one the apply found, and
  the result threw both away — then told the caller to "check them if the
  placement mattered", i.e. to fetch what the comparison had just established.
  Now `[]DriftedPlacement{id, was, now}` for move/trash, with both sides as
  named `MailboxRef`s, and `[]DriftedKeyword{id, keyword, was, now}` for mark.
  The payload objection is weakest exactly here: drift is rare and deliberately
  never abridged, so the one moment it costs bytes is the moment they are worth
  paying.
- **`email_destroy`'s in-Trash gate said which messages were blocked, not where
  they were.** The check reads `mailboxIds` to make its decision and dropped
  them, on the one tool whose mistakes cannot be undone — and the remedy
  depends on the answer: a message in Inbox needs `email_trash`, one in Trash
  *and* Archive needs the other label removed. `NotInTrash` entries now carry
  their `mailboxes`, and the refusal names them inline. Candidates deliberately
  do not: they are in Trash by definition, so it would be bytes restating the
  gate that passed them.

Also fixed while there: the drift annotation resolves every mailbox it mentions
in **one** pass. Per-message resolution would be a provider round-trip per
drifted message in precisely the case that matters — a mailbox deleted between
preview and apply is never in the refreshed list, so it misses the cache on
every lookup rather than warming it.

Checked and deliberately **not** changed, recorded so they are not re-filed:

- **`SearchResult.Query`'s `inMailbox` is a bare id.** Unlike a move's source,
  the mailbox was *supplied by the caller*: the arguments say `mailbox: "inbox"`
  and the result says `inMailbox: "P-F"`, so the record establishes the mapping
  without inference. Nothing is discovered that the caller does not already
  hold.
- **`MarkResult` does not name the keyword** an action maps to. `action:
  "read"` is the tool's own fixed vocabulary, pinned by a schema enum, so no
  separate call and no fallible inference stands between the result and a
  record of it.
- **`LintResult.FileintoTargets` are mailbox names.** Sieve addresses mailboxes
  by name; there is no id in a sieve script to report. The cross-check against
  `email_list_mailboxes` belongs to the caller because the store is
  zero-network by design.
- **`Failed`, `NotFound`, `notUpdated`** are keyed by message id with the
  provider's own reason beside it — already the self-sufficient shape.
- **The sieve store's `DocKey.Name`** is the identifier: the user chose it,
  nothing else names the document, and there is no id it could be shadowing.

## Audit logging (2026-07-26, v0.16.0)

The extension reads and mutates a mailbox on the model's initiative. The
transcript shows what the model *said* it did; only a log the model cannot
write shows what happened. `internal/audit` writes one JSON Lines record per
tool call to `<dataDir>/audit/audit-YYYY-MM-DD.NNN.jsonl`.

**On by default** (`audit_log`), because the deployment most in need of a
record is the one that never thought to switch it on. It costs a few hundred
bytes per call, writes only inside the extension's own data directory, and
records no message content, so the default is cheap and the opt-out is one
setting.

That default forced a subtlety. `Settings` must stay comparable with `==`, so
`AuditLog` is a plain `bool` whose zero value is "off" — indistinguishable from
an explicit off. And the SDK documents `Config` as possibly *empty* when the
user set nothing, so relying on the host to overlay the manifest default would
make auditing depend on whether anyone had ever opened `/extensions`. The
settings that default ON are therefore read by **presence** (`Config.Raw`),
not by value: only a key the user actually set can turn them off, and an
explicit `audit_retain_days: 0` still means "keep everything" rather than
falling back to 30. Verified by driving the shipped binary with no `audit_*`
config at all — it audits.

**The content policy is the design.** No subjects, senders, recipients,
previews, or bodies, ever — and not the caller's search filter either, since a
query over the mailbox is a standing description of what it contains. A record
answers *which messages, by id, moved where, when, under which access level*.
The one exception is `email_destroy`, which records per-message ids because the
operation is unrecoverable and the ids are the only surviving evidence.

That restraint is **structural, not careful**: `mail.AuditDetail` is an
allow-list. A field added to a tool result later is invisible to the log until
someone names it. A redaction pass would have the opposite default and would
have shipped the sender names and subject lines `email_get` returned before
v0.14.0 projected them away — the exact regression this design makes
unavailable.

- **Wrapped at registration**, not inside handlers: `a.audited(name, authority,
  handler)` in `main()`. A tool added later is audited by construction.
  `TestEveryToolIsAudited` reads main.go and fails if any `e.Tool` skips the
  wrapper or audits under an authority that disagrees with its
  `ext.WithAuthority`.
- **Built from the raw JSON in both directions**, so the record describes what
  actually crossed the boundary rather than a parallel reconstruction that
  could drift from the result.
- **Lifecycle records too.** `session_start` opens a session's stretch of the
  trail with the permissions it begins under, and `config_change` records
  `access_level` and `enable_sieve_tools` moving. Raising access_level to
  read-organize-destroy is the most consequential thing that can happen here
  and it leaves no tool call behind; a log without it would show the destroys
  and not the permission that allowed them. The session_start record doubles
  as the baseline the diff is taken against — without it the *first* change of
  a process had nothing to compare to and went unrecorded. Found by driving the
  real binary, not by reading the code.
- **Failure is loud but never fatal.** An audit write that fails cannot fail
  the mail operation: the record describes the work, it is not part of it. The
  logger reports once to the host log, switches itself off, and says so through
  `email_status`, which reports what is *actually* happening rather than what
  was configured.
- **`email_status` carries the audit block whatever else is broken** —
  unconfigured, or with the provider returning 401. Verifying that activity is
  being recorded must not depend on the connection working, and `email_status`
  is what an operator reaches for precisely when something is wrong. A provider
  failure now degrades to a `hint` on a normal status result, which is what
  `Service.Status` already did for a failed account selection. Both gaps were
  found by demoing the feature, not by reading it.
- **`ts` is when the call started**, while records append on completion, so a
  slow call can land after a faster one that began later. Sort by `ts`.
- Files and the directory are `0600`/`0700`.

### Rotation, retention, compression

- **Date first, size second.** Normal use rolls at UTC midnight; the 8 MiB cap
  is a backstop against a runaway loop, not a routine event. Records run
  250–400 bytes, so 8 MiB is ~25,000 of them — the busiest measured wave (4,000
  messages, 205 tool calls) produces well under 100 KB, so a size roll means
  something is wrong.
- **Filenames sort chronologically.** The sequence number is always present and
  zero-padded, because `audit-2026-07-26.jsonl` would sort *after* its own
  continuation `audit-2026-07-26.2.jsonl` (`.` precedes `j`). Reading an audit
  log in written order should not require knowing that.
- **Rotated files are gzipped** (`audit_compress`, on); the file being appended
  to stays plain so a collector can tail it. Measured on 4,000 *varied* records
  — identical ones compress absurdly well and prove nothing — **1,368,920 →
  88,948 bytes, 6.5%**. A month of heavy use is single-digit megabytes.
- The sweep runs on roll, so a process that never rolls never scans. Compression
  goes via a temp file and a rename: the rename is the commit point, so an
  interrupted run leaves the complete original rather than a truncated archive
  standing in for it. A `.gz` also **holds its sequence slot** — without that, a
  restart later the same day would write a second `.001.jsonl` beside the
  `.001.jsonl.gz` it had already archived, and two files would claim the same
  stretch of the record.
- **Retention** deletes whole days past `audit_retain_days` (30; `0` keeps
  everything), recognising both plain and `.gz`, and only files matching this
  package's own naming — pinned by a test that seeds `notes.txt`,
  `audit-nonsense.jsonl` and `audit-2026-13-99.jsonl` and requires all three to
  survive.

**Stated limit:** this is a local append-only file owned by the same process it
describes. It is evidence of what the extension believes it did, not
tamper-proof evidence — anything that can write the data dir can rewrite it. A
deployment needing more should ship the records off-host, which is why the
format is JSON Lines a collector can tail without parsing help.

## Safety invariants (all phases)

- stdout is the wire; all logging via `Logf`/stderr.
- Never log `api_token`, `Authorization`, bodies, or full config maps.
- Tool results bounded; no attachment downloads in MVP.
- Mutating tools always `Sequential()` + honest authority; no global
  transaction assumptions across HTTP requests.
