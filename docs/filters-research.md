# Filters/rules research spike (phase 4)

Date: 2026-07-02
Status: **research complete, probe answered** — no filter code yet.

## Probe result (2026-07-02, live Fastmail session, API-token auth)

Fastmail does **not** advertise `urn:ietf:params:jmap:sieve`. Observed
capabilities (session and primary-account level, token scoped for
mail/submission/contacts/maskedemail): `urn:ietf:params:jmap:core`, `:mail`,
`:submission`, `:contacts`, and `https://www.fastmail.com/dev/maskedemail`.
Notes: capability lists are scope-filtered (`vacationresponse` was absent too,
matching the token's scopes), but Fastmail's token UI offers no sieve scope at
all — so this is a genuine "not exposed", not a scoping artifact. The session
also carried a second, contacts-only account (master-user), confirming our
account-selection handling matters in practice.

**Decision (per the tree below):** Fastmail filter editing stays out of scope
(web UI only). RFC 9661 tools are deferred, to be built against **Stalwart**
(the interop layer) when prioritized — the implementation would light up for
Fastmail automatically if they ever expose the capability.

The product plan listed four candidate paths for filter/rules support. This
spike resolves three of them and leaves one fact to verify against a live
Fastmail session.

## Findings

### 1. There is now a standards-track API: RFC 9661 (JMAP for Sieve)

[RFC 9661](https://www.rfc-editor.org/rfc/rfc9661.html) (September 2024)
defines `urn:ietf:params:jmap:sieve`: a `SieveScript` data type with
`SieveScript/get`, `/query`, `/set`, and `/validate`, plus a
[capability object](https://www.rfc-editor.org/rfc/rfc9661.html) at both the
session and account level (`maxSizeScript`, `maxNumberScripts`,
`sieveExtensions`, …) and an `invalidSieve` SetError. Script *content* is a
**blob**: reading a script means downloading its `blobId` from the session's
`downloadUrl`; creating one means uploading a blob first — client surface we
don't have yet (our client only speaks the API endpoint).

Notably, the RFC's author is K. Murchison of **Fastmail** (also the Cyrus
implementer). This was the "provider-specific JMAP capability" path in the
product plan — it turned out to be a real standard.

### 2. Fastmail: no ManageSieve, web-UI-only editing — JMAP sieve unconfirmed

- The [Fastmail Sieve FAQ](https://www.fastmail.help/hc/en-us/articles/360058753814-Sieve-frequently-asked-questions)
  is explicit: no ManageSieve support, and "the only way to modify your
  script is by logging in to the web interface". **Path 3 (ManageSieve) is
  dead** for Fastmail.
- Fastmail's rules UI compiles to Sieve
  ([Using Sieve scripts in Fastmail](https://www.fastmail.help/hc/en-us/articles/1500000280481-Using-Sieve-scripts-in-Fastmail));
  user Sieve is edited in sections wrapped around the generated rules. Their
  deployed extension set includes `vnd.cyrus.*` extensions
  ([blog](https://www.fastmail.com/blog/more-sieve-extensions/)) — the
  backend is Cyrus, and
  [Cyrus implements RFC 9661](https://www.cyrusimap.org/imap/rfc-support.html).
- The [Fastmail dev docs](https://www.fastmail.com/dev/) list OAuth scopes for
  core/mail/submission/vacationresponse/contacts/maskedemail — **no sieve
  scope is documented**, and no public sighting of
  `urn:ietf:params:jmap:sieve` in a Fastmail session was found.

**The open fact:** does a Fastmail session (API-token auth) advertise
`urn:ietf:params:jmap:sieve` in `capabilities` / `accountCapabilities`?
Server support almost certainly exists (Cyrus + the RFC author works there);
exposure to customer tokens is the question. → see "Live probe" below.

### 3. Stalwart supports RFC 9661 (and ManageSieve)

Per [Stalwart's RFC list](https://stalw.art/docs/development/rfcs/), it
implements RFC 9661 and RFC 5804. So the planned Stalwart interop layer
doubles as a **real reference implementation for filter tools** regardless of
what Fastmail exposes — we can build and honestly test an RFC 9661 client
before/without Fastmail support.

### 4. MCP bridge: not needed for filters

Path 5 (Fastmail's remote MCP server) would only make sense if no native API
exists *and* terva gains HTTP MCP transport. With RFC 9661 + Stalwart
available, it is not the filters path. Dropped from consideration here.

## Live probe (blocked on the user's API token)

`email_status` and `email_list_accounts` now report **capability URNs** at
both session and account level (added in this spike for exactly this purpose).
Once the extension is configured with a Fastmail token:

1. Run `email_status` → check `capabilities` for `urn:ietf:params:jmap:sieve`
   (and note any `https://www.fastmail.com/dev/*` vendor URNs).
2. Run `email_list_accounts` → check each account's `capabilityUrns`.
3. Equivalent curl, if preferred:
   `curl -s -H "Authorization: Bearer $TOKEN" https://api.fastmail.com/jmap/session | jq '.capabilities | keys, (.accounts[].accountCapabilities | keys)'`

Also worth probing with a token created with **all scopes** enabled — the
sieve capability may be scope-gated.

## Decision tree

- **Fastmail advertises sieve** → implement the RFC 9661 client (below);
  Fastmail and Stalwart are both real targets.
- **Fastmail does not advertise it** → decide whether self-hosted providers
  (Stalwart, Cyrus) justify the feature anyway. Recommendation: yes, but at
  lower priority — the implementation is identical and Fastmail could enable
  the capability later; meanwhile Fastmail filter editing stays out of scope
  (web UI only), which we document honestly.

## Proposed design (when we build it — separate plan before code)

New client surface first: blob **download** (session `downloadUrl` template)
and blob **upload** (`uploadUrl`), both bounded and tested; then the
SieveScript methods.

Read-only first phase (`network-read`):

| tool | backing |
|---|---|
| `email_list_filter_scripts` | `SieveScript/get` — name, isActive, blobId |
| `email_get_filter_script` | blob download, bounded by `max_body_bytes`-style budget |
| `email_check_filter_script` | `SieveScript/validate` (server-side; no local Sieve parsing needed) |

Mutating phase, behind a new `allow_filter_edit` config key (default off),
`external-mutation` + `Sequential()`, mirroring the destroy ladder:

- `email_put_filter_script` — create/update a **non-active** named script
  (blob upload + `SieveScript/set`); dryRun = validate + diff against current.
- `email_activate_filter_script` — swap the active script; **always** backs up
  the currently-active script content into the result (and recommends saving
  it), requires an exact confirm phrase, and prefers deactivation
  (`onSuccessDeactivatescript`) over deletion. No delete tool initially.

Fastmail-specific caution (if enabled there): the active script embeds the
rules generated by their web UI. Whole-script replacement can clobber
UI-managed rules — the backup-in-result requirement exists for this reason,
and the tool description must say it plainly.

## Testing story

- `internal/jmaptest` grows a `SieveScript/*` + blob upload/download subset
  (RFC-cited, same pattern as `Email/set`).
- Stalwart docker-compose (the deferred interop layer) becomes the
  independent implementation check — it is the first feature where interop
  testing is *required* rather than nice-to-have, since Fastmail may not be
  testable at all.
- Go Sieve libraries (e.g. parser/interpreters) are **not** needed for this
  design: scripts are opaque text; validation is server-side.
