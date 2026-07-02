package mail

import (
	"context"
	"strings"
	"testing"

	"terva-ext-jmap-mail/internal/config"
	"terva-ext-jmap-mail/internal/jmap"
)

func fullEmailFixture() map[string]any {
	m := map[string]any{}
	for k, v := range emailFixture {
		m[k] = v
	}
	m["cc"] = []map[string]string{{"email": "cc@example.com"}}
	m["attachments"] = []map[string]any{{"partId": "p9", "name": "report.pdf", "type": "application/pdf", "size": 9999}}
	m["textBody"] = []map[string]any{{"partId": "p1", "type": "text/plain"}, {"partId": "p2", "type": "text/plain"}}
	m["htmlBody"] = []map[string]any{{"partId": "p3", "type": "text/html"}}
	m["bodyValues"] = map[string]any{
		"p1": map[string]any{"value": "Hello ", "isTruncated": false},
		"p2": map[string]any{"value": "World", "isTruncated": false},
		"p3": map[string]any{"value": "<p>Hello</p>", "isTruncated": false},
	}
	return m
}

func getFake() *fake {
	f := &fake{}
	f.handler = func(calls []jmap.Invocation) *jmap.Response {
		switch calls[0].Name {
		case "Mailbox/get":
			return response(result("Mailbox/get", calls[0].CallID, map[string]any{"list": mailboxFixture}))
		case "Email/get":
			return response(result("Email/get", calls[0].CallID, map[string]any{
				"list": []any{fullEmailFixture()}, "notFound": []string{"e-gone"},
			}))
		}
		panic("unexpected call " + calls[0].Name)
	}
	return f
}

func TestGetTextBody(t *testing.T) {
	f := getFake()
	s := testService(f)
	res, err := s.Get(context.Background(), GetParams{IDs: []string{"e1", "e-gone"}})
	if err != nil {
		t.Fatal(err)
	}

	args := argsOf(t, f.recorded[0][0])
	if args["fetchTextBodyValues"] != true {
		t.Errorf("fetchTextBodyValues missing: %v", args)
	}
	if _, ok := args["fetchHTMLBodyValues"]; ok {
		t.Errorf("html fetch requested for text format")
	}
	if args["maxBodyValueBytes"] != float64(config.DefaultMaxBodyBytes) {
		t.Errorf("maxBodyValueBytes = %v", args["maxBodyValueBytes"])
	}

	if len(res.Emails) != 1 {
		t.Fatalf("emails = %+v", res.Emails)
	}
	e := res.Emails[0]
	if e.BodyText != "Hello \nWorld" || e.BodyTextTruncated {
		t.Errorf("bodyText = %q truncated=%v", e.BodyText, e.BodyTextTruncated)
	}
	if e.BodyHTML != "" {
		t.Errorf("unexpected html body: %q", e.BodyHTML)
	}
	if len(e.Cc) != 1 || len(e.Attachments) != 1 || e.Attachments[0].Name != "report.pdf" {
		t.Errorf("cc/attachments = %+v %+v", e.Cc, e.Attachments)
	}
	if len(res.NotFound) != 1 || res.NotFound[0] != "e-gone" {
		t.Errorf("notFound = %v", res.NotFound)
	}
}

func TestGetMetadataOnly(t *testing.T) {
	f := getFake()
	s := testService(f)
	if _, err := s.Get(context.Background(), GetParams{IDs: []string{"e1"}, BodyFormat: BodyMetadata}); err != nil {
		t.Fatal(err)
	}
	args := argsOf(t, f.recorded[0][0])
	for _, k := range []string{"fetchTextBodyValues", "fetchHTMLBodyValues", "maxBodyValueBytes"} {
		if _, ok := args[k]; ok {
			t.Errorf("metadata format must not request %s", k)
		}
	}
	if strings.Contains(stringify(args["properties"]), "bodyValues") {
		t.Errorf("metadata format must not request bodyValues: %v", args["properties"])
	}
}

func TestGetBothFormats(t *testing.T) {
	f := getFake()
	s := testService(f)
	res, err := s.Get(context.Background(), GetParams{IDs: []string{"e1"}, BodyFormat: BodyBoth})
	if err != nil {
		t.Fatal(err)
	}
	args := argsOf(t, f.recorded[0][0])
	if args["fetchTextBodyValues"] != true || args["fetchHTMLBodyValues"] != true {
		t.Errorf("both formats should fetch both: %v", args)
	}
	if res.Emails[0].BodyHTML != "<p>Hello</p>" {
		t.Errorf("html = %q", res.Emails[0].BodyHTML)
	}
}

func TestGetBodyBudget(t *testing.T) {
	// The requested budget wins when smaller than config; config caps otherwise.
	s := NewService(&fake{session: testSession()}, config.Normalize(config.Settings{APIToken: "t", MaxBodyBytes: 1000}))
	if got := s.bodyBudget(200); got != 200 {
		t.Errorf("bodyBudget(200) = %d", got)
	}
	if got := s.bodyBudget(5000); got != 1000 {
		t.Errorf("bodyBudget(5000) = %d, want config cap", got)
	}
	if got := s.bodyBudget(0); got != 1000 {
		t.Errorf("bodyBudget(0) = %d, want config default", got)
	}
}

func TestGetInputValidation(t *testing.T) {
	s := testService(getFake())
	if _, err := s.Get(context.Background(), GetParams{}); err == nil {
		t.Error("want error for missing ids")
	}
	ids := make([]string, maxGetIDs+1)
	for i := range ids {
		ids[i] = "e"
	}
	if _, err := s.Get(context.Background(), GetParams{IDs: ids}); err == nil {
		t.Error("want error for too many ids")
	}
	if _, err := s.Get(context.Background(), GetParams{IDs: []string{"e1"}, BodyFormat: "raw"}); err == nil {
		t.Error("want error for bad bodyFormat")
	}
}

func TestAssembleBodyTruncation(t *testing.T) {
	parts := []bodyPart{{PartID: "p1"}, {PartID: "p2"}}
	values := map[string]bodyValue{
		"p1": {Value: "Hello "},
		"p2": {Value: "World"},
	}
	// Local budget cuts inside the second part (6 + newline → 1 byte left).
	body, truncated := assembleBody(parts, values, 8)
	if body != "Hello \nW" || !truncated {
		t.Errorf("body = %q truncated = %v", body, truncated)
	}
	// Server-side truncation propagates even under budget.
	values["p2"] = bodyValue{Value: "World", IsTruncated: true}
	_, truncated = assembleBody(parts, values, 1000)
	if !truncated {
		t.Error("server isTruncated must propagate")
	}
	// Unfetched parts are skipped without error.
	body, _ = assembleBody([]bodyPart{{PartID: "p1"}, {PartID: "missing"}}, values, 1000)
	if body != "Hello " {
		t.Errorf("body = %q", body)
	}
}

func TestTruncateUTF8(t *testing.T) {
	s := "héllo" // é is 2 bytes
	if got := truncateUTF8(s, 2); got != "h" {
		t.Errorf("truncateUTF8(%q, 2) = %q, must not split the rune", s, got)
	}
	if got := truncateUTF8(s, 3); got != "hé" {
		t.Errorf("truncateUTF8(%q, 3) = %q", s, got)
	}
	if got := truncateUTF8(s, 100); got != s {
		t.Errorf("no-op truncation changed string: %q", got)
	}
}
