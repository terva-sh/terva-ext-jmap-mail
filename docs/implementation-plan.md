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

## Safety invariants (all phases)

- stdout is the wire; all logging via `Logf`/stderr.
- Never log `api_token`, `Authorization`, bodies, or full config maps.
- Tool results bounded; no attachment downloads in MVP.
- Mutating tools always `Sequential()` + honest authority; no global
  transaction assumptions across HTTP requests.
