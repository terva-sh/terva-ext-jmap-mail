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

// email_get takes a handle, which is what makes a failure group actionable —
// but its cap does NOT rise the way the organize tools' did. Theirs bounds the
// ids a caller sends; this one bounds the subjects and bodies coming back, and
// a handle does nothing about that.
func TestGetByHandleSlicesAtTheContentCap(t *testing.T) {
	f := newHandleFake(maxGetIDs * 3)
	s := testService(f.fake)
	handle := seedBigSelection(s, f.cohort)
	ctx := context.Background()

	var seen []string
	for offset := 0; ; {
		res, err := s.Get(ctx, GetParams{
			Handle: handle, SelectionOffset: offset,
			Fields: []string{"id", "subject"},
		})
		if err != nil {
			t.Fatal(err)
		}
		if res.Selection == nil {
			t.Fatal("a handle-named read did not report which slice it took")
		}
		if res.Selection.Count > maxGetIDs {
			t.Fatalf("read %d messages, above the %d cap — a handle must not lift a cap on RESULT size",
				res.Selection.Count, maxGetIDs)
		}
		for _, e := range res.Emails {
			seen = append(seen, e.ID)
		}
		if res.Selection.Remaining == 0 {
			break
		}
		offset += res.Selection.Count
	}
	if len(seen) != len(f.cohort) {
		t.Fatalf("slices covered %d of %d messages", len(seen), len(f.cohort))
	}
	for i, id := range f.cohort {
		if seen[i] != id {
			t.Fatalf("slice %d = %s, want %s", i, seen[i], id)
		}
	}
}

// A read authorizes nothing, so either handle kind names a set here — unlike
// the mutating path, where a receipt's kind is checked because it is an
// authorization. "Show me what my dry run covered" is a fair question.
func TestGetAcceptsEitherHandleKind(t *testing.T) {
	f := newHandleFake(5)
	s := testService(f.fake)
	ctx := context.Background()

	sel := seedBigSelection(s, f.cohort)
	if res, err := s.Get(ctx, GetParams{Handle: sel, Fields: []string{"id"}}); err != nil {
		t.Fatalf("selection handle: %v", err)
	} else if len(res.Emails) != 5 {
		t.Errorf("read %d of 5", len(res.Emails))
	}

	rcp := s.handles.putReceipt(&receipt{Kind: receiptMove, AccountID: "A1", IDs: f.cohort[:2]})
	res, err := s.Get(ctx, GetParams{Handle: rcp, Fields: []string{"id"}})
	if err != nil {
		t.Fatalf("receipt handle: %v", err)
	}
	if len(res.Emails) != 2 {
		t.Errorf("read %d of the receipt's 2", len(res.Emails))
	}
}

// The same padding contract the organize tools have: both inert values are
// representable, and naming both selectors is refused with the correction
// spelled out.
func TestGetRefusesBothSelectorsAndNeither(t *testing.T) {
	f := newHandleFake(3)
	s := testService(f.fake)
	ctx := context.Background()

	_, err := s.Get(ctx, GetParams{Handle: "", IDs: []string{"", "  "}})
	if err == nil || !strings.Contains(err.Error(), "name the messages") {
		t.Fatalf("err = %v", err)
	}
	_, err = s.Get(ctx, GetParams{Handle: "sel_x", IDs: []string{"e1"}})
	if err == nil || !strings.Contains(err.Error(), "not both") {
		t.Fatalf("err = %v", err)
	}
	// A handle neither prefix claims says so, rather than reading as unknown.
	if _, err := s.Get(ctx, GetParams{Handle: "xyz_bogus"}); err == nil ||
		!strings.Contains(err.Error(), "neither a selectionId") {
		t.Fatalf("err = %v", err)
	}
	// Literal ids keep their own cap and their own message.
	many := make([]string, maxGetIDs+1)
	for i := range many {
		many[i] = "e1"
	}
	if _, err := s.Get(ctx, GetParams{IDs: many}); err == nil || !strings.Contains(err.Error(), "too many ids") {
		t.Fatalf("err = %v", err)
	}
}
