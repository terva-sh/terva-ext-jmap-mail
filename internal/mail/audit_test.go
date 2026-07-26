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
		"ids":["Sx1","Sx2"],"confirm":"move 2 emails to Archive in account u1 [batch abc123]",
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
	args := `{"accountId":"u1","receipt":"rcp_9a1c4e","ids":[],"selection":"","toMailbox":"Archive"}`
	result := `{"accountId":"u1","movedCount":200,
		"destination":{"id":"P3V","name":"Archive","role":"archive"},
		"sources":[{"id":"P-F","name":"Inbox","role":"inbox","count":200}],
		"selection":{"id":"sel_ab12","offset":0,"count":200,"remaining":300}}`
	detail, account := AuditDetail(json.RawMessage(args), json.RawMessage(result))

	if account != "u1" {
		t.Errorf("account = %q", account)
	}
	if detail["movedCount"] != float64(200) || detail["receipt"] != "rcp_9a1c4e" {
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
	args := `{"accountId":"","ids":["","  "],"selection":"","receipt":"","selectionOffset":0,
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
