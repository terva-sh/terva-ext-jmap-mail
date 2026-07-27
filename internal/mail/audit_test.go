package mail

import (
	"encoding/json"
	"strings"
	"testing"
)

// The load-bearing test for the whole audit feature: real tool results, with
// real third-party content in them, must project to a record that contains
// none of it. Everything else about auditing is a convenience; this is the
// property that makes it safe to keep the file at all.
func TestAuditDetailNeverCarriesMessageContent(t *testing.T) {
	// Sentinels chosen to be unmistakable if they leak, one per content
	// channel a mail tool can return.
	const (
		subject = "SENTINEL-SUBJECT-Invoice-4432"
		sender  = "sentinel-sender@third-party.test"
		preview = "SENTINEL-PREVIEW body text that should never be logged"
		body    = "SENTINEL-BODY full message text"
		query   = "SENTINEL-QUERY-severance-agreement"
	)

	results := map[string]string{
		"email_search": `{"accountId":"u1","position":0,"limit":20,"returned":2,"hasMore":true,
			"queryState":"qs-1","selectionId":"sel_ab12",
			"query":{"text":"` + query + `","inMailbox":"P-F"},
			"ids":["Sx1","Sx2"],
			"emails":[{"id":"Sx1","subject":"` + subject + `","preview":"` + preview + `",
				"from":[{"name":"Sentinel","email":"` + sender + `"}]}]}`,

		"email_get": `{"accountId":"u1","emails":[{"id":"Sx1","subject":"` + subject + `",
			"bodyText":"` + body + `","preview":"` + preview + `",
			"from":[{"email":"` + sender + `"}]}],"notFound":["Sx9"]}`,

		"email_move": `{"accountId":"u1","dryRun":false,"movedCount":200,
			"destination":{"id":"P3V","name":"Archive","role":"archive"},
			"sources":[{"id":"P-F","name":"Inbox","role":"inbox","count":200}],
			"moved":[{"id":"Sx1","subject":"` + subject + `","from":[{"id":"P-F","name":"Inbox"}]}]}`,

		"email_mark": `{"accountId":"u1","action":"read","changedCount":24,"alreadySetCount":1,
			"changed":[{"id":"Sx1","subject":"` + subject + `"}],
			"drifted":[{"id":"Sx2","keyword":"$seen","was":"unset","now":"set"}]}`,

		"email_destroy": `{"accountId":"u1","dryRun":true,
			"destroyed":[{"id":"Sx1","subject":"` + subject + `"},{"id":"Sx2","subject":"other"}],
			"notInTrash":[{"id":"Sx3","subject":"` + subject + `",
				"mailboxes":[{"id":"mb-inbox","name":"Inbox"}]}],
			"confirmPhrase":"destroy 2 emails ... [batch abc123]"}`,

		"email_get_thread": `{"accountId":"u1","threadId":"T1","count":9,"returned":9,
			"emailsWithBodies":[{"id":"Sx1","subject":"` + subject + `","bodyText":"` + body + `"}]}`,

		"email_status": `{"configured":true,"sessionUrl":"https://api.fastmail.com/jmap/session",
			"username":"person@example.test","accountId":"u1","accessLevel":"read-organize"}`,

		"email_list_mailboxes": `{"accountId":"u1","mailboxes":[
			{"id":"mb-inbox","name":"Inbox","totalEmails":42},{"id":"mb-arch","name":"Archive"}]}`,
	}
	// The arguments carry content too — a search filter is a description of
	// the mail it matches, which in a durable log is a standing record of
	// what the mailbox holds.
	args := `{"accountId":"u1","mailbox":"inbox","text":"` + query + `","from":"` + sender + `",
		"subject":"` + subject + `","body":"` + body + `",
		"filterJson":{"text":"` + query + `"},
		"ids":["Sx1","Sx2"],"handle":"","confirm":"move 2 emails to Archive in account u1 [batch abc123]",
		"toMailbox":"Archive","dryRun":true,"fields":["id"],"limit":200}`

	sentinels := []string{subject, sender, preview, body, query,
		"SENTINEL", "person@example.test", "api.fastmail.com"}

	for tool, result := range results {
		detail, account := AuditDetail(json.RawMessage(args), json.RawMessage(result))
		rendered, err := json.Marshal(detail)
		if err != nil {
			t.Fatalf("%s: %v", tool, err)
		}
		for _, bad := range sentinels {
			if strings.Contains(string(rendered), bad) {
				t.Errorf("%s: audit detail leaked %q\n%s", tool, bad, rendered)
			}
		}
		if account != "u1" {
			t.Errorf("%s: account = %q, want the resolved account", tool, account)
		}
	}
}

// A destroy applied by receipt sends no ids at all, so the record has to be
// assembled from the other end. It is the only record that will ever exist of
// these messages, and "handle: rcp_…" alone would not be one.
func TestAuditDetailDescribesAReceiptDrivenDestroy(t *testing.T) {
	args := `{"accountId":"u1","handle":"rcp_5c81af","ids":[],"selectionOffset":0,
		"allowNotInTrash":false,"dryRun":false,"confirm":""}`
	result := `{"accountId":"u1","destroyed":[
			{"id":"Sx1","subject":"SENTINEL-A"},{"id":"Sx2","subject":"SENTINEL-B"}],
		"replayed":false,"note":"destroy is PERMANENT and unrecoverable"}`
	detail, account := AuditDetail(json.RawMessage(args), json.RawMessage(result))

	if account != "u1" {
		t.Errorf("account = %q", account)
	}
	if detail["mode"] != "receipt" || detail["handle"] != "rcp_5c81af" {
		t.Errorf("the record does not say the destroy was authorized by a receipt: %v", detail)
	}
	ids, _ := detail["destroyedIds"].([]string)
	if len(ids) != 2 {
		t.Fatalf("destroyedIds = %v — the ids are the only surviving evidence", detail["destroyedIds"])
	}
	// Nothing arrived on the args side to summarize, and inventing an idCount
	// from the result would make a receipt call look like an ids call.
	if _, ok := detail["idCount"]; ok {
		t.Errorf("a receipt-driven call was recorded as having named ids: %v", detail)
	}
	if rendered, _ := json.Marshal(detail); strings.Contains(string(rendered), "SENTINEL") {
		t.Errorf("subjects reached the destroy record: %s", rendered)
	}
}

// An allow-list only holds if it is actually an allow-list: a result field
// nobody has thought about must not appear, whatever it is called.
func TestAuditDetailIgnoresUnknownFields(t *testing.T) {
	result := `{"accountId":"u1","movedCount":3,
		"someFutureField":"SENTINEL-NEW-CONTENT",
		"attachmentNames":["SENTINEL-FILE.pdf"]}`
	detail, _ := AuditDetail(nil, json.RawMessage(result))
	rendered, _ := json.Marshal(detail)
	if strings.Contains(string(rendered), "SENTINEL") {
		t.Errorf("an unlisted field reached the log: %s", rendered)
	}
	if detail["movedCount"] != float64(3) {
		t.Errorf("allow-listed field lost: %v", detail)
	}
}

// email_destroy is the one exception, and it must actually work — the ids are
// the only surviving record that the messages existed.
func TestAuditDetailKeepsDestroyedIDsAndNotTheirSubjects(t *testing.T) {
	result := `{"accountId":"u1","destroyed":[
		{"id":"Sx1","subject":"SENTINEL-A"},{"id":"Sx2","subject":"SENTINEL-B"}]}`
	detail, _ := AuditDetail(nil, json.RawMessage(result))
	ids, _ := detail["destroyedIds"].([]string)
	if len(ids) != 2 || ids[0] != "Sx1" || ids[1] != "Sx2" {
		t.Fatalf("destroyedIds = %v, want both ids", detail["destroyedIds"])
	}
	if detail["destroyedCount"] != 2 {
		t.Errorf("destroyedCount = %v", detail["destroyedCount"])
	}
	rendered, _ := json.Marshal(detail)
	if strings.Contains(string(rendered), "SENTINEL") {
		t.Errorf("destroy record kept a subject: %s", rendered)
	}
}

// A mutation's record has to be enough to answer "what changed, where, on
// whose say-so" without the mailbox in front of you.
func TestAuditDetailDescribesAMutation(t *testing.T) {
	args := `{"accountId":"u1","handle":"rcp_9a1c4e","ids":[],"toMailbox":"Archive"}`
	result := `{"accountId":"u1","movedCount":200,
		"destination":{"id":"P3V","name":"Archive","role":"archive"},
		"sources":[{"id":"P-F","name":"Inbox","role":"inbox","count":200}],
		"selection":{"id":"sel_ab12","offset":0,"count":200,"remaining":300}}`
	detail, account := AuditDetail(json.RawMessage(args), json.RawMessage(result))

	if account != "u1" {
		t.Errorf("account = %q", account)
	}
	if detail["movedCount"] != float64(200) || detail["handle"] != "rcp_9a1c4e" || detail["mode"] != "receipt" {
		t.Errorf("detail = %v", detail)
	}
	dest, _ := detail["destination"].(map[string]any)
	if dest["id"] != "P3V" {
		t.Errorf("destination = %v, want the mailbox id", detail["destination"])
	}
	src, _ := detail["sources"].([]any)
	if len(src) != 1 {
		t.Fatalf("sources = %v", detail["sources"])
	}
	if first, _ := src[0].(map[string]any); first["id"] != "P-F" || first["count"] != float64(200) {
		t.Errorf("source = %v, want the origin mailbox id and count", src[0])
	}
	// ids:[] is the padded inert value, not an operation on nothing.
	if _, ok := detail["idCount"]; ok {
		t.Errorf("a padded empty ids array was recorded as a real one: %v", detail)
	}
	if _, ok := detail["selection"]; !ok {
		t.Error("the selection slice consumed is not recorded")
	}
}

// The digest is how a record is matched to the confirm phrase that authorized
// it, and it is what makes recording 200 ids unnecessary.
func TestAuditDetailSummarizesIDsRatherThanListingThem(t *testing.T) {
	ids := make([]string, 200)
	for i := range ids {
		ids[i] = "Sx" + strings.Repeat("a", i%5) + string(rune('0'+i%10))
	}
	raw, _ := json.Marshal(map[string]any{"ids": ids, "confirm": "move 200 emails ..."})
	detail, _ := AuditDetail(raw, nil)

	if detail["idCount"] != 200 {
		t.Errorf("idCount = %v", detail["idCount"])
	}
	if detail["batch"] != idBatchDigest(ids) {
		t.Errorf("batch = %v, want the same digest the confirm phrase binds", detail["batch"])
	}
	if detail["confirmed"] != true {
		t.Errorf("a confirmed bulk run is not marked as one: %v", detail)
	}
	rendered, _ := json.Marshal(detail)
	if len(rendered) > 300 {
		t.Errorf("a 200-id call rendered %d bytes of audit detail:\n%s", len(rendered), rendered)
	}
	// The phrase itself is operational noise; only that one was given matters.
	if strings.Contains(string(rendered), "move 200 emails") {
		t.Errorf("the confirm phrase was recorded verbatim: %s", rendered)
	}
}

// Blank and padded values must not fill the log with restated defaults.
func TestAuditDetailDropsPaddedArguments(t *testing.T) {
	args := `{"accountId":"","ids":["","  "],"handle":"","selectionOffset":0,
		"dryRun":false,"verbose":false,"fields":[],"mailboxes":[],"limit":0,"toMailbox":""}`
	detail, account := AuditDetail(json.RawMessage(args), nil)
	if account != "" {
		t.Errorf("account = %q, want empty", account)
	}
	if len(detail) != 0 {
		t.Errorf("padding was recorded as activity: %v", detail)
	}
}

// An unparseable result must not lose the record — a malformed result is
// itself worth knowing about.
func TestAuditDetailSurvivesGarbage(t *testing.T) {
	detail, account := AuditDetail(json.RawMessage(`{"toMailbox":"Archive"}`), json.RawMessage(`not json at all`))
	if detail["toMailbox"] != "Archive" {
		t.Errorf("readable args lost when the result was garbage: %v", detail)
	}
	if account != "" {
		t.Errorf("account = %q", account)
	}
}

// TW-045's first named cost was the audit record: reconstructing which
// protocol a wave used meant classifying 139 calls by testing fields for
// emptiness. A record now says which mode it was.
func TestAuditDetailStatesTheTargetMode(t *testing.T) {
	for _, tc := range []struct{ args, want string }{
		{`{"handle":"sel_84ee6b","selectionOffset":0,"ids":[]}`, "selection"},
		{`{"handle":"rcp_d18707","ids":[]}`, "receipt"},
		{`{"ids":["Sx1","Sx2"],"handle":""}`, "ids"},
		{`{"handle":"xyz_bogus","ids":[]}`, "unknown"},
		// A tool that names no target set must not have a mode invented for it.
		{`{"mailbox":"inbox","fields":["id"]}`, ""},
		// Nor a call that named nothing at all — it was refused.
		{`{"ids":[],"handle":"","toMailbox":"Archive"}`, ""},
	} {
		detail, _ := AuditDetail(json.RawMessage(tc.args), nil)
		got, _ := detail["mode"].(string)
		if got != tc.want {
			t.Errorf("%s → mode %q, want %q (detail %v)", tc.args, got, tc.want, detail)
		}
	}
}

// The aggregates are the first tools whose RESULTS are made of third-party
// content: a group key is a sender address, and a count's echoed filter is a
// description of what the mailbox holds. Both are exactly the material the
// allow-list exists to keep out of a durable log, and both arrive in fields
// that did not exist when it was written — which is the case a redaction pass
// would have failed and this one must not.
func TestAuditDetailKeepsAggregateShapeAndNotItsContent(t *testing.T) {
	group := `{"accountId":"u1","groupBy":"from","matched":3166,"scanned":3166,
		"queryState":"qs-9","distinctKeys":214,"otherTotal":812,
		"query":{"inMailbox":"P-F","text":"SENTINEL-QUERY"},
		"groups":[
			{"key":"SENTINEL-SENDER@third-party.test","total":412,"unread":9,
			 "newest":"2026-07-01T00:00:00Z","oldest":"2025-01-01T00:00:00Z"},
			{"key":"other@third-party.test","total":88,"unread":0}]}`
	detail, account := AuditDetail(nil, json.RawMessage(group))
	if account != "u1" {
		t.Errorf("account = %q", account)
	}
	if detail["matched"] != float64(3166) || detail["scanned"] != float64(3166) {
		t.Errorf("the shape of the scan was not recorded: %v", detail)
	}
	if detail["groupCount"] != 2 {
		t.Errorf("groupCount = %v, want the number of rows", detail["groupCount"])
	}
	if _, ok := detail["groups"]; ok {
		t.Error("the group rows themselves reached the log")
	}
	rendered, _ := json.Marshal(detail)
	if strings.Contains(string(rendered), "SENTINEL") || strings.Contains(string(rendered), "third-party") {
		t.Errorf("a sender address reached the audit record:\n%s", rendered)
	}

	count := `{"accountId":"u1","queryState":"qs-9","statesDiffered":true,
		"counts":[{"label":"SENTINEL-LABEL","total":42,
			"query":{"from":"SENTINEL-SENDER@third-party.test"}}]}`
	detail, _ = AuditDetail(nil, json.RawMessage(count))
	if detail["countRows"] != 1 {
		t.Errorf("countRows = %v", detail["countRows"])
	}
	if detail["statesDiffered"] != true {
		t.Error("a table that did not reconcile was recorded as if it had")
	}
	rendered, _ = json.Marshal(detail)
	if strings.Contains(string(rendered), "SENTINEL") {
		t.Errorf("a count's label or filter reached the audit record:\n%s", rendered)
	}
}

// A bulk run that stopped half-way is precisely what an operator needs from a
// log, and none of it is content.
func TestAuditDetailRecordsAPartialBulkRun(t *testing.T) {
	result := `{"accountId":"u1","movedCount":1000,"aborted":true,"appliedTo":1000,
		"remainingCount":1000,"remainingSelectionId":"sel_ff01",
		"abortReason":"connection reset","destination":{"id":"P3V","name":"Archive"}}`
	detail, _ := AuditDetail(json.RawMessage(`{"handle":"rcp_1","ids":[]}`), json.RawMessage(result))
	for _, key := range []string{"aborted", "appliedTo", "remainingCount", "movedCount"} {
		if _, ok := detail[key]; !ok {
			t.Errorf("a half-landed wave did not record %q: %v", key, detail)
		}
	}
}

// Failure groups reach the log as cause and count and nothing else. The group
// is the one result field that carries both a server-written string and raw
// message ids, so it is exactly the field an allow-list has to be careful with.
func TestAuditDetailRecordsFailureCausesNotTheirContents(t *testing.T) {
	result := `{"accountId":"u1","movedCount":1500,
		"failures":[
			{"type":"forbidden","count":400,"reason":"SENTINEL-SERVER-PROSE","selectionId":"sel_aa"},
			{"type":"notFound","count":100,"ids":["SENTINEL-ID-1","SENTINEL-ID-2"]}],
		"failureNote":"each failure group carries a selectionId ..."}`
	detail, _ := AuditDetail(nil, json.RawMessage(result))

	groups, _ := detail["failures"].([]map[string]any)
	if len(groups) != 2 {
		t.Fatalf("failures = %v, want both causes", detail["failures"])
	}
	if groups[0]["type"] != "forbidden" || groups[0]["count"] != 400 {
		t.Errorf("group = %v, want cause and count", groups[0])
	}
	rendered, _ := json.Marshal(detail)
	for _, bad := range []string{"SENTINEL", "sel_aa", "reason", "failureNote"} {
		if strings.Contains(string(rendered), bad) {
			t.Errorf("audit record carries %q:\n%s", bad, rendered)
		}
	}
}

// The group rows carrying the new per-group handles are made of sender
// addresses, so the rows must not reach the log. The top-level selectionId is
// a different case and stays: within one wave it ties the call that minted a
// handle to the call that consumed it, which is what makes a log of forty
// records readable as one operation.
func TestAuditDetailDropsHandlesAndGroupRows(t *testing.T) {
	group := `{"accountId":"u1","groupBy":"from","matched":412,"scanned":412,
		"handleNote":"each group carries a selectionId ...",
		"groups":[{"key":"SENTINEL-SENDER@x.test","total":412,"selectionId":"sel_zz"}]}`
	detail, _ := AuditDetail(nil, json.RawMessage(group))
	if detail["groupCount"] != 1 || detail["matched"] != float64(412) {
		t.Errorf("shape lost: %v", detail)
	}
	thread := `{"accountId":"u1","threadId":"T1","count":125,"returned":100,"omitted":25,
		"selectionId":"sel_yy","selectionNote":"names all 125 ...",
		"emails":[{"id":"Sx1","subject":"SENTINEL-SUBJECT"}]}`
	detail2, _ := AuditDetail(nil, json.RawMessage(thread))
	if detail2["omitted"] != float64(25) || detail2["messageCount"] != 1 {
		t.Errorf("thread shape lost: %v", detail2)
	}
	if detail2["selectionId"] != "sel_yy" {
		t.Errorf("the correlating handle was dropped: %v", detail2)
	}
	for _, d := range []map[string]any{detail, detail2} {
		rendered, _ := json.Marshal(d)
		// sel_zz rides inside a group row; those are never admitted, because a
		// group key is a sender address.
		for _, bad := range []string{"SENTINEL", "sel_zz", "handleNote", "selectionNote"} {
			if strings.Contains(string(rendered), bad) {
				t.Errorf("audit record carries %q:\n%s", bad, rendered)
			}
		}
	}
}
