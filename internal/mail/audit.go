package mail

// The audit projection: what an operation is allowed to say about itself in a
// durable log.
//
// This is an ALLOW-LIST, and the choice is load-bearing. A redaction pass —
// "log the result, minus the fields we know are sensitive" — fails open: the
// day someone adds a field, it ships to disk until a reviewer notices. This
// fails closed. A new field on any result is invisible to the audit log until
// it is named here, which is a deliberate act by someone who has read this
// comment.
//
// The rule for admitting one: it must be an identifier, a count, a flag, or a
// mailbox — never anything a third party wrote. Subjects, senders, recipients,
// previews and bodies are permanently out, and so is the caller's own search
// text, which is a query over that content and just as revealing. The one
// exception is email_destroy's message ids: the operation is unrecoverable, so
// the ids are the only surviving evidence of what existed.

import (
	"encoding/json"
	"strings"
)

// auditArgFields are the request fields safe to record: what was asked for,
// with nothing that quotes or queries message content.
//
// Deliberately absent: text, from, to, cc, bcc, subject, body and filterJson
// (a search filter describes the content it matches, which in a log is a
// standing record of what the mailbox contains); content and note (the user's
// own prose); confirm (recorded as a boolean instead — the phrase itself is
// operational noise); and ids, which are summarized as a count and a digest.
var auditArgFields = []string{
	"dryRun", "toMailbox", "action", "keepInMailboxes", "allowNotInTrash", "verbose",
	"handle", "selectionOffset",
	"mailbox", "mailboxes", "fields", "returnIds", "limit", "position", "sort",
	"collapseThreads", "includeTotal", "hasAttachment", "keyword", "notKeyword",
	"after", "before", "bodyFormat", "maxBodyBytes", "includeFullUrls", "includeBodies",
	"threadId", "emailId",
	"name", "version", "origin", "unarchive", "includeArchived", "force", "contextOnly", "sourcePath",
	"groupBy", "interval", "groupLimit", "maxMessages",
}

// auditResultFields are the outcome fields safe to record: what happened.
//
// Deliberately absent: emails, emailsWithBodies, moved, changed, alreadySet,
// destroyed and notInTrash (all carry subjects), query (the echoed filter),
// ids / firstId / lastId (a search's matches are not a mutation and do not
// belong in a per-message trail), confirmPhrase, and every email_status field
// — the account username and session URL are configuration, not activity.
var auditResultFields = []string{
	"dryRun", "movedCount", "changedCount", "alreadySetCount", "keptInMailboxes",
	"destination", "sources", "selection", "receiptId", "selectionId",
	"replayed", "appliedAt", "drifted", "driftNote", "notFound", "failed",
	"returned", "position", "limit", "hasMore", "total", "queryState",
	"count", "omitted", "threadId",
	// The aggregates report only shape: how many rows, how much was scanned,
	// whether it was truncated. `counts` and `groups` are absent on purpose —
	// a group key IS a sender address, and a count's label plus its echoed
	// filter is a standing description of what the mailbox holds.
	"statesDiffered", "matched", "scanned", "truncated", "distinctKeys", "otherTotal",
	// A thread's own shape. The top-level selectionId above IS admitted, and
	// deliberately: the token is dead fifteen minutes later, but within one
	// wave it is what ties the search that minted a handle to the move that
	// consumed it, which is the only way to read a log of forty calls as one
	// operation. The per-group and per-failure handles are a different matter
	// — they ride inside rows that are not allow-listed at all, because a
	// group's key is a sender address.
	"omitted", "returned",
	// The bulk-mutation stop report. How far a run got is exactly the thing an
	// operator needs from a log when a wave half-landed.
	"aborted", "appliedTo", "remainingCount",
	"head", "applied", "versions", "pendingChanges", "archived", "contextOnly",
}

// AuditDetail projects a tool call into the fields an audit record may carry.
// args and result are the raw JSON that crossed the tool boundary, so the
// projection describes exactly what the model sent and saw — not a
// reconstruction that could drift from it.
//
// It never fails: unparseable JSON yields whatever was readable, because a
// malformed result is itself worth having a record of.
func AuditDetail(args, result json.RawMessage) (detail map[string]any, account string) {
	detail = map[string]any{}

	var in map[string]json.RawMessage
	_ = json.Unmarshal(args, &in)
	copyAllowed(detail, in, auditArgFields)

	// ids are summarized, never listed: a 200-id array per call would make the
	// log larger than the traffic it describes. The digest is the same one the
	// confirm phrase binds, so a record can be matched to the phrase that
	// authorized it.
	if raw, ok := in["ids"]; ok {
		var ids []string
		if json.Unmarshal(raw, &ids) == nil {
			if ids = nonBlank(ids); len(ids) > 0 {
				detail["idCount"] = len(ids)
				detail["batch"] = idBatchDigest(ids)
			}
		}
	}
	// State the target-selection mode rather than leaving a reader to derive
	// it from which selector survived the inert-value drop. TW-045's first
	// named cost was exactly that: classifying 139 calls by testing fields for
	// emptiness. The handle's prefix already carries this, so the record only
	// has to say out loud what the call now states.
	if mode := targetMode(detail); mode != "" {
		detail["mode"] = mode
	}
	if raw, ok := in["confirm"]; ok {
		var phrase string
		if json.Unmarshal(raw, &phrase) == nil && strings.TrimSpace(phrase) != "" {
			detail["confirmed"] = true
		}
	}
	if raw, ok := in["accountId"]; ok {
		_ = json.Unmarshal(raw, &account)
	}

	var out map[string]json.RawMessage
	_ = json.Unmarshal(result, &out)
	copyAllowed(detail, out, auditResultFields)

	if raw, ok := out["accountId"]; ok {
		// The resolved account wins over the requested one: "" means default,
		// and a log that records "" has recorded nothing.
		var resolved string
		if json.Unmarshal(raw, &resolved) == nil && resolved != "" {
			account = resolved
		}
	}

	// Counts for the result arrays that are themselves too content-bearing to
	// record. How many messages came back is an operational fact; which
	// messages, with their subjects, is the mailbox.
	for key, name := range map[string]string{
		"emails":           "messageCount",
		"emailsWithBodies": "messageCount",
		"mailboxes":        "mailboxCount",
		"moved":            "enumerated",
		"changed":          "enumerated",
		"groups":           "groupCount",
		"counts":           "countRows",
	} {
		if n, ok := arrayLen(out[key]); ok {
			detail[name] = n
		}
	}

	// email_destroy is the exception this package exists to be careful about.
	// The operation cannot be undone, so the ids are the only record that the
	// messages ever existed — and the subjects beside them in the result are
	// still not admitted.
	if ids, ok := objectIDs(out["destroyed"]); ok && len(ids) > 0 {
		detail["destroyedIds"] = ids
		detail["destroyedCount"] = len(ids)
	}
	if ids, ok := objectIDs(out["notInTrash"]); ok && len(ids) > 0 {
		detail["blockedIds"] = ids
	}
	// Failure groups are recorded as shape only: which causes, how many each.
	// That is what an operator reviewing a half-landed wave needs. The rest of
	// the group is not admitted — `reason` is server prose, `ids` are message
	// ids outside the one destroy exception, and `selectionId` names an
	// in-memory handle that is dead fifteen minutes after the record is
	// written, so it would only ever read as a lead that goes nowhere.
	if groups, ok := failureShape(out["failures"]); ok {
		detail["failures"] = groups
	}

	if len(detail) == 0 {
		return nil, account
	}
	return detail, account
}

// targetMode names how an organize call named its messages, from the handle
// prefix the caller supplied. Empty for tools that take no handle, and for a
// call that named neither — a record should not assert a mode for an operation
// that had none.
func targetMode(detail map[string]any) string {
	handle, _ := detail["handle"].(string)
	switch {
	case strings.HasPrefix(handle, selectionPrefix):
		return "selection"
	case strings.HasPrefix(handle, receiptPrefix):
		return "receipt"
	case handle != "":
		return "unknown" // a handle neither prefix claims; the call was refused
	case detail["idCount"] != nil:
		return "ids"
	}
	return ""
}

// failureShape projects a result's failure groups down to cause and count.
func failureShape(raw json.RawMessage) ([]map[string]any, bool) {
	if len(raw) == 0 {
		return nil, false
	}
	var groups []struct {
		Type  string `json:"type"`
		Count int    `json:"count"`
	}
	if json.Unmarshal(raw, &groups) != nil || len(groups) == 0 {
		return nil, false
	}
	out := make([]map[string]any, 0, len(groups))
	for _, g := range groups {
		if g.Type == "" {
			continue
		}
		out = append(out, map[string]any{"type": g.Type, "count": g.Count})
	}
	return out, len(out) > 0
}

func copyAllowed(dst map[string]any, src map[string]json.RawMessage, allowed []string) {
	for _, key := range allowed {
		raw, ok := src[key]
		if !ok || string(raw) == "null" {
			continue
		}
		var v any
		if json.Unmarshal(raw, &v) != nil {
			continue
		}
		// Drop the inert values a padding model sends, so a log of forty calls
		// is not forty copies of every default in the schema.
		switch t := v.(type) {
		case string:
			if t == "" {
				continue
			}
		case bool:
			if !t {
				continue
			}
		case float64:
			if t == 0 {
				continue
			}
		case []any:
			if len(t) == 0 {
				continue
			}
		case map[string]any:
			if len(t) == 0 {
				continue
			}
		}
		dst[key] = v
	}
}

func arrayLen(raw json.RawMessage) (int, bool) {
	if len(raw) == 0 {
		return 0, false
	}
	var arr []json.RawMessage
	if json.Unmarshal(raw, &arr) != nil {
		return 0, false
	}
	return len(arr), true
}

// objectIDs pulls just the id out of an array of message objects, leaving the
// subject beside it where it belongs.
func objectIDs(raw json.RawMessage) ([]string, bool) {
	if len(raw) == 0 {
		return nil, false
	}
	var arr []struct {
		ID string `json:"id"`
	}
	if json.Unmarshal(raw, &arr) != nil {
		return nil, false
	}
	out := make([]string, 0, len(arr))
	for _, e := range arr {
		if e.ID != "" {
			out = append(out, e.ID)
		}
	}
	return out, true
}

func nonBlank(ids []string) []string {
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		if id = strings.TrimSpace(id); id != "" {
			out = append(out, id)
		}
	}
	return out
}
