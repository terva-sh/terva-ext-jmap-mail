# terva-ext-jmap-mail

A [terva](https://terva.sh) extension for reading and searching mail over
**JMAP** (JSON Meta Application Protocol), with
[Fastmail](https://www.fastmail.com/dev/) as the primary target provider.
Registers as `jmap-mail`; tools use the `email_*` prefix.

Current scope: **read + safe organization + gated destroy +
sieve filter management** — status, accounts, mailboxes, search, bounded
message fetch (URLs redacted by default), threads; mark/move/trash with
dry-run and bulk-confirmation gates; permanent destroy at the top of the
`access_level` ladder (off by default); and a local, append-only versioned
store for sieve filter documents. Two bundled skills: **sieve-rules** (the
paste-in → edit → emit → confirm loop for providers without a sieve API,
i.e. Fastmail) and **mail-triage** (survey → candidate rules → preview →
apply → verify). Sending remains absent. See
[`docs/implementation-plan.md`](docs/implementation-plan.md).

## Sieve filter management (opt-in: `enable_sieve_tools`)

Fastmail exposes no sieve API
([research](docs/filters-research.md)), so the `email_sieve_*` tools keep a
**local** versioned home for your filter sections
([design](docs/sieve-workspace-design.md)): every change appends a version
(last 50 kept; the applied version is never pruned), restore appends a copy
so nothing is ever lost, an `applied` pointer separates *pending edits* from
*out-of-band drift*, and diffs are built in. Every put runs an **advisory
sieve lint** (balance, `require` coverage, `discard`/`stop` traps, `fileinto`
target extraction; `dryRun` previews without storing), large provider exports
import intact from a workspace file via `sourcePath` (jailed to the session
working directory), reference material can be marked `contextOnly`, mistakes
can be archived (never deleted), and `mark_applied` refuses
truncation-suspect placeholders. The tools never contact the provider — the
bundled **sieve-rules** skill walks the agent through calibrating against
your account's editable areas, previewing rule conditions against real mail
with `email_search` (raw `filterJson` accepts Fastmail's `jmapquery` blocks
verbatim), and emitting labeled sections for you to paste into Fastmail's web
UI (which validates on save).

## Setup (Fastmail)

1. Create an API token: Fastmail **Settings → Privacy & Security → Manage API
   tokens**. Read-only scope is enough for phase 1.
2. Install the extension:

   ```bash
   just install        # or: terva ext install <this repo / its git URL>
   ```

3. In terva, run `/extensions`, select **jmap-mail**, press `c`, and set:

   | key | value |
   |---|---|
   | `session_url` | `https://api.fastmail.com/jmap/session` (default) |
   | `api_token` | the token from step 1 |
   | `default_account` | optional — account id or name; empty uses the provider's primary mail account |
   | `max_body_bytes` | per-message body budget (default 12000, capped at 1,000,000) |
   | `access_level` | `read-only` (default) → `read-organize` (adds mark/move/trash) → `read-organize-destroy` (adds permanent destroy) |
   | `enable_sieve_tools` | opt-in for the local `email_sieve_*` filter-management tools (default off) |

Any RFC 8620-compliant provider works: point `session_url` at its session
resource. Generic autodiscovery (`/.well-known/jmap`, DNS SRV) is a later
phase.

> **Credential note:** terva stores `secret` config values in plaintext in
> `$TERVA_HOME/config.json` (masked in the UI, never logged by the host).
> Treat that file as a credential store; do not commit it.

## Tools

| tool | authority | purpose |
|---|---|---|
| `email_status` | network-read | config + account + capabilities + server limits + which tools config keeps off; no mail content |
| `email_list_accounts` | network-read | accounts reachable with the credentials |
| `email_list_mailboxes` | network-read | folders/labels with roles, display paths, and optional counts |
| `email_search` | network-read | filtered search (structured params or a raw JMAP `filterJson`); bounded summaries + previews, never bodies; `hasMore` paging and opt-in exact totals |
| `email_get` | network-read | fetch ≤20 messages; bounded text/html bodies with truncation flags; URL tokens/queries redacted by default (`includeFullUrls` opts out) |
| `email_get_thread` | network-read | whole thread by threadId or member email id; bodies redact like `email_get` |
| `email_mark` | external-mutation | mark read/unread, flag/unflag (`$seen`/`$flagged`); dryRun |
| `email_move` | external-mutation | move to a mailbox (or add with `keepInMailboxes`); dryRun + bulk confirm |
| `email_trash` | external-mutation | move to Trash — **not** a permanent delete; dryRun + bulk confirm |
| `email_destroy` | external-mutation | **permanent, unrecoverable** delete; requires `access_level: read-organize-destroy`, in-Trash targets, and an exact confirm phrase |
| `email_sieve_list` / `_get` / `_diff` | local-read | inspect the local sieve document store (versions, pending diffs, archived/context-only flags) |
| `email_sieve_put` / `_restore` / `_mark_applied` / `_archive` | local-data | append versions (with lint + file import), lossless restore, record what's live, shelve mistakes without deleting |

Tool availability follows the configured **`access_level`** ladder:
`read-only` (default) offers only the read tools; `read-organize` adds
mark/move/trash; `read-organize-destroy` adds permanent destroy. Tools above
the level — and every tool while unconfigured — are withdrawn from the model
(hosts speaking extension protocol ≥ 4; older hosts get clear refusals
instead), and every provider-facing handler refuses regardless (the local
sieve tools need no provider config, only their own opt-in). The local `email_sieve_*`
tools are an independent opt-in (`enable_sieve_tools`, default off);
when enabled they are available at every access level, since they never
touch the provider. `email_status` reports both knobs and the exact list of
tools they keep unavailable, so an agent can tell gating from malfunction.
Mailboxes can be referenced by role (`inbox`, `trash`, …), display path
(`Inbox/Gaming`), display name, or id; ambiguous names are refused with the
candidate paths and ids.

## Safety model

- **Honest authority.** Read tools declare `network-read`; the organization
  tools declare `external-mutation` and run `Sequential()`, so terva's
  permission/plan modes gate them correctly and the model's issue order is
  preserved instead of racing.
- **Dry-run everything, confirm bulk.** Every mutating tool takes
  `dryRun:true` and reports what would change (with each message's current
  state). A non-dry run over **20 messages** — mark included — is refused
  unless `confirm` matches an exact generated phrase (e.g. `move 25 emails
  to Inbox/Receipts in account u123`); the refusal spells it out and a bulk
  dry run returns it as `confirmPhrase`. Phrases bind the count, destination
  path, and account, so a phrase minted for one operation can never confirm
  a different one.
- **Trash is not destroy.** `email_trash` re-files messages into the
  `role=trash` mailbox per RFC 8621's delete-to-trash semantics and is always
  recoverable. Partial failures are reported per message id.
- **Destroy climbs a steeper ladder.** `email_destroy` is (1) withdrawn and
  refused unless the user raises `access_level` to `read-organize-destroy`
  in `/extensions`;
  (2) restricted to messages already in Trash — and only Trash — unless
  `allowNotInTrash:true` is passed explicitly; (3) gated on an exact confirm
  phrase on **every** run, any size — bound to the exact id batch (a digest)
  and options, so a phrase from one preview cannot authorize different ids
  or a gate-skipping rerun (`dryRun` previews the outcome, lists blockers,
  and returns the phrase); and (4) bound to its own preview via JMAP
  `ifInState` — if the mailbox changes between the in-Trash check and the
  destroy, the server refuses and nothing is deleted.
- **Bounded content.** Search returns summaries and previews only. Body
  fetches are capped by `max_body_bytes` (tool calls may request less, never
  more) and report truncation. Attachments are metadata-only.
- **URL redaction by default.** Fetched bodies routinely embed live
  credentials — unsubscribe tokens, one-click sign-ins, tracking ids — and
  fetched bodies land in agent context and transcripts. Query strings,
  fragments, userinfo, and token-like path segments are stripped from every
  URL (hosts and readable paths survive; `redactedUrls` counts the changes).
  `includeFullUrls: true` opts out for tasks that need a working link.
- **No secret leakage.** The API token is never logged (config changes log
  `has_api_token=true/false` only) and never appears in error text.
- **No caching of message content.** Only session/mailbox metadata is cached,
  in memory, with short TTLs.

## Development

```bash
just test        # unit tests (httptest + fakes; no network)
just lint        # go vet + gofmt
just build       # build ./jmap-mail (vendored, offline)
just try         # load into a one-off terva session
just install     # install into $TERVA_HOME/extensions + stage the binary
```

Layout follows the house conventions: pure logic in `internal/` (`config`,
`jmap`, `mail`, `version` — unit-tested, SDK-free), thin glue in `app.go`,
registration in `main.go`. The terva Go SDK is pinned and vendored
(`go build -mod=vendor`), so installs are offline and self-contained.

### Spec compliance

`internal/jmap/spec_test.go` pins the wire behavior to the standards, one
cited RFC section per test: request envelope and `using` capabilities
(RFC 8620 §3.3), invocation triples (§3.2), result references (§3.7),
request-level problem details (§3.6.1), method-level errors (§3.6.2), the
session resource shape (§2), and UTCDate rendering used by `Email/query`
filters (RFC 8621 §4.4.1).

### Hermetic integration tests (`internal/jmaptest`)

`internal/jmaptest` is an in-memory JMAP server implementing the read subset
of RFC 8620/8621 — session resource, request envelope, result references
(incl. `*` wildcard flattening), `Core/echo`, `Mailbox/get`, `Email/query`,
`Email/get`, `Thread/get` — over a seeded fixture store (threads, keywords,
attachments, long multibyte bodies). It deliberately does **not** import the
client package: it parses raw JSON against the RFC shapes, so a client
marshaling bug can't be masked by the same bug server-side.

`internal/mail/integration_test.go` runs the real stack (HTTP client →
service) against it in plain `just test` — no network, no credentials. Phase 2
extends the fake with `Email/set` so mutations are developed against a server
that can't hurt a real mailbox. A containerized reference implementation
(Stalwart) is planned as a later interop layer.

### Live tests (opt-in, read-only)

Never run by default or in CI:

```bash
JMAP_TEST_SESSION_URL=https://api.fastmail.com/jmap/session \
JMAP_TEST_API_TOKEN=... \
just test-live
```

Optional: `JMAP_TEST_ACCOUNT` to pin the account. Phase 2 mutation tests will
additionally require `JMAP_TEST_SAFE_MAILBOX` and
`JMAP_TEST_ALLOW_DESTRUCTIVE=1` against a dedicated test mailbox.

## References

- [RFC 8620 — JMAP core](https://www.rfc-editor.org/rfc/rfc8620)
- [RFC 8621 — JMAP for Mail](https://www.rfc-editor.org/rfc/rfc8621)
- [JMAP crash course](https://jmap.io/crash-course/index.html)
- [Fastmail developer docs](https://www.fastmail.com/dev/)

## License

MIT — see [LICENSE](LICENSE).
