// Command terva-ext-jmap-mail is a terva extension for reading, searching, and
// (in later phases) safely organizing mail over JMAP providers such as
// Fastmail. Registration lives here; the thin SDK glue is app.go; all logic is
// in internal/ (pure, unit-tested, SDK-free).
//
// Protocol references: RFC 8620 (JMAP core), RFC 8621 (JMAP mail),
// https://jmap.io/crash-course/index.html, https://www.fastmail.com/dev/.
package main

import (
	"encoding/json"
	"fmt"
	"os"

	"terva-ext-jmap-mail/internal/version"

	"terva.sh/terva/packages/agent/ext"
)

func main() {
	// version.Version is pinned equal to extension.json's "version" by a test;
	// bump both together.
	e := ext.New("jmap-mail", version.Version)

	// Standing policy folded into the cached system prompt: cross-tool strategy
	// (search before fetch, bounded bodies, mailbox addressing). Tool
	// descriptions stay terse but mirror the essentials, since a user/project
	// can disable context injection while keeping the tools.
	e.ContributeContext(contextPolicy)

	a := newApp(e)

	// Read tools touch provider state over the network and mutate nothing —
	// the honest declaration is network-read (not read_only, which claims a
	// local, side-effect-free call).
	netRead := ext.WithAuthority(ext.AuthorityNetworkRead)
	e.Tool("email_status", descStatus, emptySchema(), a.handleStatus, netRead)
	e.Tool("email_list_accounts", descAccounts, emptySchema(), a.handleListAccounts, netRead)
	e.Tool("email_list_mailboxes", descMailboxes, schemaMailboxes(), a.handleListMailboxes, netRead)
	e.Tool("email_search", descSearch, schemaSearch(), a.handleSearch, netRead)
	e.Tool("email_get", descGet, schemaGet(), a.handleGet, netRead)
	e.Tool("email_get_thread", descThread, schemaThread(), a.handleGetThread, netRead)

	// Organization tools mutate provider state: external-mutation authority,
	// and Sequential() so the SDK preserves the model's issue order instead of
	// racing goroutines (e.g. mark-then-trash on the same messages).
	extMutate := ext.WithAuthority(ext.AuthorityExternalMutate)
	e.Tool("email_mark", descMark, schemaMark(), a.handleMark, extMutate, ext.Sequential())
	e.Tool("email_move", descMove, schemaMove(), a.handleMove, extMutate, ext.Sequential())
	e.Tool("email_trash", descTrash, schemaTrash(), a.handleTrash, extMutate, ext.Sequential())

	// Registered always, but withdrawn below access_level=read-organize-destroy
	// (see app.syncVisibility) and refused by its handler regardless.
	e.Tool("email_destroy", descDestroy, schemaDestroy(), a.handleDestroy, extMutate, ext.Sequential())

	// Sieve document store: a local, append-only versioned home for the
	// user's filter scripts (docs/sieve-workspace-design.md). These tools
	// never touch the network or the provider — reads are local-read, writes
	// local-data (the extension's own data dir only), so the declarations
	// stay honest. Sequential() on writes keeps model-issued order.
	localRead := ext.WithAuthority(ext.AuthorityLocalRead)
	localData := ext.WithAuthority(ext.AuthorityLocalData)
	e.Tool("email_sieve_list", descSieveList, schemaSieveList(), a.handleSieveList, localRead)
	e.Tool("email_sieve_get", descSieveGet, schemaSieveGet(), a.handleSieveGet, localRead)
	e.Tool("email_sieve_diff", descSieveDiff, schemaSieveDiff(), a.handleSieveDiff, localRead)
	e.Tool("email_sieve_put", descSievePut, schemaSievePut(), a.handleSievePut, localData, ext.Sequential())
	e.Tool("email_sieve_restore", descSieveRestore, schemaSieveRestore(), a.handleSieveRestore, localData, ext.Sequential())
	e.Tool("email_sieve_mark_applied", descSieveMarkApplied, schemaSieveMarkApplied(), a.handleSieveMarkApplied, localData, ext.Sequential())
	e.Tool("email_sieve_archive", descSieveArchive, schemaSieveArchive(), a.handleSieveArchive, localData, ext.Sequential())

	// Config changes rebuild the service lazily (handlers re-read config per
	// call); here we only log shape — never values — and re-sync visibility.
	e.OnConfig(a.onConfig)

	// Session boundary: the cache-safe moment to withdraw the tools when
	// unconfigured (they could only refuse) or restore them once configured.
	e.OnSession(a.onSession)

	if err := e.Run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

const contextPolicy = "The email_* tools read and organize the user's mailboxes over JMAP. " +
	"Prefer email_search (summaries + previews) before fetching bodies; fetch bodies with email_get " +
	"only for messages actually needed — bodies are truncated to a byte budget and results indicate " +
	"truncation, and URLs in them are redacted by default (queries/tokens stripped) — includeFullUrls " +
	"opts out when the user needs a working link. Search results report hasMore; pass includeTotal for " +
	"an exact match count (e.g. when sizing a filter rule), and filterJson for raw JMAP AND/OR/NOT " +
	"filters. Mailboxes may be referenced " +
	"by role (inbox, trash, sent, drafts, junk, archive), display path (Inbox/Gaming), display name, " +
	"or id; email ids are stable across moves. If an expected tool is absent, email_status names the " +
	"config gate that hides it (and if every email_* tool is missing, jmap-mail itself needs configuring " +
	"in /extensions). The organization tools (email_mark, " +
	"email_move, email_trash) support dryRun — preview before bulk changes; runs over 20 messages " +
	"require the exact confirm phrase given in the refusal. email_trash moves mail to the Trash " +
	"mailbox and is recoverable. email_destroy is PERMANENT and unrecoverable: restricted to mail " +
	"already in Trash by default, and every run needs the exact confirm phrase a dryRun returns — " +
	"always dryRun first. Which tools are offered follows the user's configured access_level " +
	"(read-only / read-organize / read-organize-destroy); only the user can raise it, in /extensions. " +
	"There is no send capability. " +
	"The email_sieve_* tools (an opt-in via enable_sieve_tools) keep a LOCAL append-only versioned store " +
	"of the user's sieve filter documents (head/applied pointers, diffs, lossless restore); they never " +
	"contact the provider — follow the sieve-rules skill for the paste-in → edit → emit → confirm workflow."

const (
	descStatus = "Check JMAP mail configuration: chosen account, provider capabilities, server limits, " +
		"and which tools the user's access_level / enable_sieve_tools settings keep unavailable. No mail content."

	descAccounts = "List the JMAP accounts reachable with the configured credentials."

	descMailboxes = "List mailboxes (folders/labels) with roles, display paths, and optional message counts."

	descSearch = "Search email; returns bounded summaries with previews, never full bodies. " +
		"Filter by mailbox (role, path, name, or id), text, from/to/cc/bcc, subject, body, date range, attachments, keywords — " +
		"or pass a raw JMAP filter via filterJson (supports AND/OR/NOT). " +
		"hasMore reports whether matches remain past the page; includeTotal adds an exact match count."

	descGet = "Fetch up to 20 emails by id with bounded bodies (bodyFormat: text, html, both, or metadata for headers only). " +
		"Results report body truncation. URL query strings and token-like segments in bodies are redacted by default " +
		"(redactedUrls counts them) — pass includeFullUrls only when the task needs a working link."

	descThread = "Fetch every message in a thread, by threadId or any member email id. " +
		"Summaries by default; includeBodies adds bounded text bodies (URLs redacted like email_get; includeFullUrls opts out)."

	descMark = "Mark emails read/unread or flagged/unflagged. Supports dryRun; more than 20 ids requires confirm " +
		"(the exact phrase from the refusal or the dry run's confirmPhrase)."

	descMove = "Move emails to a mailbox (role, path, name, or id), removing them from other mailboxes " +
		"unless keepInMailboxes is true. Supports dryRun; more than 20 ids requires confirm."

	descTrash = "Move emails to the Trash mailbox — NOT a permanent delete; mail stays recoverable. " +
		"Supports dryRun; more than 20 ids requires confirm."

	descDestroy = "PERMANENTLY destroy emails — unrecoverable; email_trash is the recoverable alternative. " +
		"Targets must already be in Trash (unless allowNotInTrash). Every run requires the exact confirm " +
		"phrase; dryRun previews the outcome and returns it. The phrase is bound to the exact id batch " +
		"and options — any change means a fresh dryRun."

	descSieveList = "List locally stored sieve filter documents: head/applied versions, origins, pending changes, context-only/archived flags. Local only."

	descSieveGet = "Read a stored sieve document (head or a specific version). Local only."

	descSieveDiff = "Unified diff between two versions of a sieve document. Defaults: applied → head (what's pending to paste). Local only."

	descSievePut = "Append a new version of a sieve document (origin: paste-in for verbatim user pastes, edit for changes; " +
		"sourcePath imports a workspace file, e.g. a large provider export). Every put returns advisory lint findings " +
		"plus fileinto targets to verify against email_list_mailboxes; dryRun lints and diffs without storing. " +
		"Identical content is a no-op. Local only — never sent to the provider."

	descSieveRestore = "Restore an earlier version of a sieve document by appending a copy of it as the new head. Nothing is discarded. Local only."

	descSieveMarkApplied = "Record that a version (default: head) is now live at the provider, after the user confirms pasting it. " +
		"Refuses context-only documents and truncation-suspect content (force overrides the latter). Local only."

	descSieveArchive = "Move a sieve document out of (or back into, with unarchive) the working set. " +
		"Archived documents keep every version on disk and stay listable via includeArchived — nothing is deleted. Local only."
)

func emptySchema() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{}}`)
}

func schemaMailboxes() json.RawMessage {
	return json.RawMessage(`{
  "type": "object",
  "properties": {
    "accountId": {"type": "string", "description": "Account id or name; empty for the default account."},
    "includeCounts": {"type": "boolean", "default": true, "description": "Include total/unread message counts."}
  }
}`)
}

func schemaSearch() json.RawMessage {
	return json.RawMessage(`{
  "type": "object",
  "properties": {
    "accountId": {"type": "string", "description": "Account id or name; empty for the default account."},
    "mailbox": {"type": "string", "description": "Restrict to one mailbox: role (inbox, trash, ...), path (Inbox/Gaming), display name, or id."},
    "text": {"type": "string", "description": "Match anywhere: from/to/cc/bcc, subject, body."},
    "from": {"type": "string"},
    "to": {"type": "string"},
    "cc": {"type": "string"},
    "bcc": {"type": "string"},
    "subject": {"type": "string"},
    "body": {"type": "string"},
    "after": {"type": "string", "description": "receivedAt lower bound, RFC 3339 or YYYY-MM-DD (inclusive)."},
    "before": {"type": "string", "description": "receivedAt upper bound, RFC 3339 or YYYY-MM-DD (exclusive)."},
    "hasAttachment": {"type": "boolean"},
    "keyword": {"type": "string", "description": "Require a JMAP keyword, e.g. $seen or $flagged."},
    "notKeyword": {"type": "string", "description": "Exclude a JMAP keyword, e.g. $seen for unread."},
    "filterJson": {"type": "object", "additionalProperties": true, "description": "Raw RFC 8621 Email/query filter (FilterCondition or AND/OR/NOT FilterOperator), e.g. from a Fastmail jmapquery block. Replaces the structured filter params; only mailbox may combine (ANDed in)."},
    "collapseThreads": {"type": "boolean", "default": false, "description": "At most one result per thread."},
    "includeTotal": {"type": "boolean", "default": false, "description": "Also return the exact total match count (may cost the server extra work)."},
    "limit": {"type": "integer", "minimum": 1, "maximum": 100, "default": 20},
    "position": {"type": "integer", "minimum": 0, "default": 0, "description": "Offset for paging."},
    "sort": {"type": "string", "enum": ["newest", "oldest"], "default": "newest"}
  }
}`)
}

func schemaGet() json.RawMessage {
	return json.RawMessage(`{
  "type": "object",
  "properties": {
    "accountId": {"type": "string", "description": "Account id or name; empty for the default account."},
    "ids": {"type": "array", "items": {"type": "string"}, "minItems": 1, "maxItems": 20},
    "bodyFormat": {"type": "string", "enum": ["text", "html", "both", "metadata"], "default": "text"},
    "maxBodyBytes": {"type": "integer", "minimum": 0, "description": "Per-message body byte budget; capped by the configured maximum."},
    "includeFullUrls": {"type": "boolean", "default": false, "description": "Keep URLs intact (query strings, tokens). Default strips them — use only when the user needs a working link."}
  },
  "required": ["ids"]
}`)
}

func schemaMark() json.RawMessage {
	return json.RawMessage(`{
  "type": "object",
  "properties": {
    "accountId": {"type": "string", "description": "Account id or name; empty for the default account."},
    "ids": {"type": "array", "items": {"type": "string"}, "minItems": 1, "maxItems": 200},
    "action": {"type": "string", "enum": ["read", "unread", "flag", "unflag"]},
    "dryRun": {"type": "boolean", "default": false, "description": "Report what would change without changing it."},
    "confirm": {"type": "string", "description": "Required above 20 ids: the exact phrase from the refusal message or the dry run's confirmPhrase, verbatim."}
  },
  "required": ["ids", "action"]
}`)
}

func schemaMove() json.RawMessage {
	return json.RawMessage(`{
  "type": "object",
  "properties": {
    "accountId": {"type": "string", "description": "Account id or name; empty for the default account."},
    "ids": {"type": "array", "items": {"type": "string"}, "minItems": 1, "maxItems": 200},
    "toMailbox": {"type": "string", "description": "Destination mailbox: role, path (Inbox/Gaming), display name, or id."},
    "keepInMailboxes": {"type": "boolean", "default": false, "description": "Add the destination instead of replacing the current mailboxes (label-style)."},
    "dryRun": {"type": "boolean", "default": false, "description": "Report what would move without moving it."},
    "confirm": {"type": "string", "description": "Required above 20 ids: the exact phrase from the refusal message or the dry run's confirmPhrase, verbatim."}
  },
  "required": ["ids", "toMailbox"]
}`)
}

func schemaTrash() json.RawMessage {
	return json.RawMessage(`{
  "type": "object",
  "properties": {
    "accountId": {"type": "string", "description": "Account id or name; empty for the default account."},
    "ids": {"type": "array", "items": {"type": "string"}, "minItems": 1, "maxItems": 200},
    "dryRun": {"type": "boolean", "default": false, "description": "Report what would be trashed without trashing it."},
    "confirm": {"type": "string", "description": "Required above 20 ids: the exact phrase from the refusal message or the dry run's confirmPhrase, verbatim."}
  },
  "required": ["ids"]
}`)
}

func schemaDestroy() json.RawMessage {
	return json.RawMessage(`{
  "type": "object",
  "properties": {
    "accountId": {"type": "string", "description": "Account id or name; empty for the default account."},
    "ids": {"type": "array", "items": {"type": "string"}, "minItems": 1, "maxItems": 200},
    "allowNotInTrash": {"type": "boolean", "default": false, "description": "Also destroy targets that are not already in Trash."},
    "dryRun": {"type": "boolean", "default": false, "description": "Preview the outcome and get the required confirm phrase."},
    "confirm": {"type": "string", "description": "Required on every real run: the exact phrase the dryRun returns, verbatim — it is bound to these exact ids and options, so re-run dryRun if anything changes."}
  },
  "required": ["ids"]
}`)
}

func schemaSieveGet() json.RawMessage {
	return json.RawMessage(`{
  "type": "object",
  "properties": {
    "accountId": {"type": "string", "description": "Account id; optional when only one account has stored documents."},
    "name": {"type": "string", "description": "Document name from first-use calibration — never guess; a miss lists what exists."},
    "version": {"type": "integer", "minimum": 0, "description": "Version to read; 0/absent = head."}
  },
  "required": ["name"]
}`)
}

func schemaSieveDiff() json.RawMessage {
	return json.RawMessage(`{
  "type": "object",
  "properties": {
    "accountId": {"type": "string", "description": "Account id; optional when only one account has stored documents."},
    "name": {"type": "string"},
    "from": {"type": "integer", "minimum": 0, "description": "Base version; 0/absent = the applied version."},
    "to": {"type": "integer", "minimum": 0, "description": "Target version; 0/absent = head."}
  },
  "required": ["name"]
}`)
}

func schemaSievePut() json.RawMessage {
	return json.RawMessage(`{
  "type": "object",
  "properties": {
    "accountId": {"type": "string", "description": "Account id; required on first use (see email_status), optional once documents exist."},
    "name": {"type": "string", "description": "Document name from first-use calibration, e.g. area-1-after-require-before-generated."},
    "content": {"type": "string", "description": "Full document content. For large provider exports prefer sourcePath."},
    "sourcePath": {"type": "string", "description": "Import content from a file INSIDE the session working directory (large exports survive intact instead of being re-typed). Mutually exclusive with content."},
    "origin": {"type": "string", "enum": ["paste-in", "edit"], "description": "paste-in: verbatim from the user/provider. edit: an agent change."},
    "note": {"type": "string", "description": "One-line reason, shown in history."},
    "contextOnly": {"type": "boolean", "description": "Mark the document as reference material (full exports, generated blocks): kept and diffable, but mark_applied refuses it."},
    "dryRun": {"type": "boolean", "default": false, "description": "Lint and diff against the current head without storing anything."}
  },
  "required": ["name", "origin"]
}`)
}

func schemaSieveRestore() json.RawMessage {
	return json.RawMessage(`{
  "type": "object",
  "properties": {
    "accountId": {"type": "string", "description": "Account id; optional when only one account has stored documents."},
    "name": {"type": "string"},
    "version": {"type": "integer", "minimum": 1, "description": "Version whose content becomes the new head (as a copy)."},
    "note": {"type": "string"}
  },
  "required": ["name", "version"]
}`)
}

func schemaSieveMarkApplied() json.RawMessage {
	return json.RawMessage(`{
  "type": "object",
  "properties": {
    "accountId": {"type": "string", "description": "Account id; optional when only one account has stored documents."},
    "name": {"type": "string"},
    "version": {"type": "integer", "minimum": 0, "description": "Version confirmed live; 0/absent = head."},
    "force": {"type": "boolean", "default": false, "description": "Override the truncation-marker guard — only when a flagged version truly is what's live at the provider."}
  },
  "required": ["name"]
}`)
}

func schemaSieveList() json.RawMessage {
	return json.RawMessage(`{
  "type": "object",
  "properties": {
    "includeArchived": {"type": "boolean", "default": false, "description": "Also list archived documents."}
  }
}`)
}

func schemaSieveArchive() json.RawMessage {
	return json.RawMessage(`{
  "type": "object",
  "properties": {
    "accountId": {"type": "string", "description": "Account id; optional when only one account has stored documents."},
    "name": {"type": "string"},
    "unarchive": {"type": "boolean", "default": false, "description": "Bring an archived document back into the working set."}
  },
  "required": ["name"]
}`)
}

func schemaThread() json.RawMessage {
	return json.RawMessage(`{
  "type": "object",
  "properties": {
    "accountId": {"type": "string", "description": "Account id or name; empty for the default account."},
    "threadId": {"type": "string", "description": "Thread to fetch. Provide this or emailId."},
    "emailId": {"type": "string", "description": "Any email in the thread. Provide this or threadId."},
    "includeBodies": {"type": "boolean", "default": false, "description": "Also fetch bounded text bodies."},
    "maxBodyBytes": {"type": "integer", "minimum": 0, "description": "Per-message body byte budget; capped by the configured maximum."},
    "includeFullUrls": {"type": "boolean", "default": false, "description": "Keep URLs intact (query strings, tokens). Default strips them — use only when the user needs a working link."}
  }
}`)
}
