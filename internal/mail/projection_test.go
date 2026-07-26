package mail

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"terva-ext-jmap-mail/internal/jmap"
)

// getProjectionFake answers Email/get with exactly the properties asked for,
// so a test can tell a projection that narrowed the request from one that
// merely dropped fields on the way out. Mailbox/get is answered too, and
// recorded, so "the projection skipped the mailbox lookup" is checkable.
func getProjectionFake() *fake {
	f := &fake{}
	f.handler = func(calls []jmap.Invocation) *jmap.Response {
		switch calls[0].Name {
		case "Mailbox/get":
			return response(result("Mailbox/get", calls[0].CallID, map[string]any{"list": mailboxFixture}))
		case "Email/get":
			props := map[string]bool{}
			for _, p := range argsOfAny(calls[0])["properties"].([]string) {
				props[p] = true
			}
			e := map[string]any{}
			for k, v := range fullEmailFixture() {
				if props[k] {
					e[k] = v
				}
			}
			if props["bodyValues"] {
				e["bodyValues"] = fullEmailFixture()["bodyValues"]
			}
			return response(result("Email/get", calls[0].CallID, map[string]any{"list": []any{e}}))
		}
		panic("unexpected call " + calls[0].Name)
	}
	return f
}

// The placement check TW-033 was filed over: four messages, three properties,
// and none of the third-party sender names, subject lines or body previews
// that bodyFormat:"metadata" returns whether you wanted them or not.
func TestGetProjectsToNamedFields(t *testing.T) {
	f := getProjectionFake()
	s := testService(f)
	res, err := s.Get(context.Background(), GetParams{
		IDs: []string{"e1"}, Fields: []string{"mailboxes", "keywords"},
	})
	if err != nil {
		t.Fatal(err)
	}

	// The projection narrows the request, not just the response.
	args := argsOf(t, lastBatch(t, f, "Email/get")[0])
	props := stringify(args["properties"])
	for _, want := range []string{"id", "mailboxIds", "keywords"} {
		if !strings.Contains(props, `"`+want+`"`) {
			t.Errorf("properties %s is missing %q", props, want)
		}
	}
	for _, unwanted := range []string{"from", "subject", "preview", "cc", "bcc", "attachments"} {
		if strings.Contains(props, `"`+unwanted+`"`) {
			t.Errorf("properties %s still asks for %q", props, unwanted)
		}
	}

	if len(res.Emails) != 1 {
		t.Fatalf("emails = %+v", res.Emails)
	}
	e := res.Emails[0]
	if e.ID != "e1" || len(e.Mailboxes) == 0 || len(e.Keywords) == 0 {
		t.Fatalf("projection dropped what it was asked for: %+v", e)
	}
	// The whole point is what is NOT in the session record afterwards.
	payload := stringify(res)
	for _, leaked := range []string{"alice@example.com", "Hello", "Hi there", "subject", "preview", "from"} {
		if strings.Contains(payload, leaked) {
			t.Errorf("payload still carries %q:\n%s", leaked, payload)
		}
	}
	if strings.Contains(payload, "bodyText") {
		t.Errorf("a metadata projection fetched a body:\n%s", payload)
	}
}

// bodyFormat's inert value resolves to "text", so refusing fields alongside a
// body format would refuse the padding model this projection exists for — the
// same shape as the v0.13.0 selector trap. The projection wins instead.
func TestGetProjectionOverridesPaddedBodyFormat(t *testing.T) {
	for _, format := range []string{"", "text", "both"} {
		f := getProjectionFake()
		s := testService(f)
		res, err := s.Get(context.Background(), GetParams{
			IDs: []string{"e1"}, Fields: []string{"mailboxes"}, BodyFormat: format,
		})
		if err != nil {
			t.Fatalf("bodyFormat %q alongside fields was refused: %v", format, err)
		}
		args := argsOf(t, lastBatch(t, f, "Email/get")[0])
		for _, k := range []string{"fetchTextBodyValues", "fetchHTMLBodyValues", "maxBodyValueBytes"} {
			if _, ok := args[k]; ok {
				t.Errorf("bodyFormat %q: projection named no body, but %s was requested", format, k)
			}
		}
		if res.Emails[0].BodyText != "" || res.Emails[0].BodyHTML != "" {
			t.Errorf("bodyFormat %q: body returned under a metadata projection", format)
		}
	}
}

// A projection is the complete answer to what comes back, bodies included.
func TestGetProjectionCanNameBodies(t *testing.T) {
	f := getProjectionFake()
	s := testService(f)
	res, err := s.Get(context.Background(), GetParams{
		IDs: []string{"e1"}, Fields: []string{"subject", "bodyText"}, BodyFormat: BodyMetadata,
	})
	if err != nil {
		t.Fatal(err)
	}
	args := argsOf(t, lastBatch(t, f, "Email/get")[0])
	if args["fetchTextBodyValues"] != true {
		t.Errorf("fields named bodyText but no text fetch was requested: %v", args)
	}
	if _, ok := args["fetchHTMLBodyValues"]; ok {
		t.Errorf("fields named only bodyText: %v", args)
	}
	e := res.Emails[0]
	if e.BodyText != "Hello \nWorld" || e.Subject != "Hello" {
		t.Errorf("projected result = %+v", e)
	}
	if len(e.From) != 0 || e.Preview != "" {
		t.Errorf("projection leaked unrequested summary fields: %+v", e)
	}
}

// The full vocabulary is a superset of email_search's, so one field list works
// on both tools — and the email_get-only properties are nameable.
func TestGetProjectionCoversTheFullShape(t *testing.T) {
	f := getProjectionFake()
	s := testService(f)
	res, err := s.Get(context.Background(), GetParams{
		IDs: []string{"e1"}, Fields: []string{"cc", "attachments"},
	})
	if err != nil {
		t.Fatal(err)
	}
	e := res.Emails[0]
	if len(e.Cc) != 1 || len(e.Attachments) != 1 || e.Attachments[0].Name != "report.pdf" {
		t.Fatalf("full-only fields not projected: cc=%+v attachments=%+v", e.Cc, e.Attachments)
	}
	if e.Subject != "" || len(e.From) != 0 {
		t.Errorf("projection leaked summary fields: %+v", e)
	}
	// Every email_search projection must be valid here.
	for _, name := range fieldOrder {
		if _, err := parseFullFields([]string{name}); err != nil {
			t.Errorf("search field %q is not valid on email_get: %v", name, err)
		}
	}
}

// The mailbox annotation is the one summary property that costs a second
// provider call; a projection that drops it must drop the call too.
func TestGetProjectionSkipsTheMailboxLookup(t *testing.T) {
	f := getProjectionFake()
	s := testService(f)
	if _, err := s.Get(context.Background(), GetParams{IDs: []string{"e1"}, Fields: []string{"subject"}}); err != nil {
		t.Fatal(err)
	}
	for _, batch := range f.recorded {
		for _, call := range batch {
			if call.Name == "Mailbox/get" {
				t.Fatalf("projection omitted mailboxes but still resolved them: %v", f.recorded)
			}
		}
	}
}

// Passing no fields must return exactly what it returned before the
// projection existed — the acceptance criterion the fleet named.
func TestGetWithoutFieldsIsUnchanged(t *testing.T) {
	f := getFake()
	s := testService(f)
	res, err := s.Get(context.Background(), GetParams{IDs: []string{"e1"}})
	if err != nil {
		t.Fatal(err)
	}
	e := res.Emails[0]
	if e.Subject == "" || len(e.From) == 0 || len(e.Cc) == 0 || len(e.Attachments) == 0 ||
		e.BodyText == "" || len(e.Mailboxes) == 0 || e.Preview == "" {
		t.Errorf("unprojected email_get lost a field: %+v", e)
	}
}

func TestGetRejectsAnUnknownField(t *testing.T) {
	s := testService(getProjectionFake())
	_, err := s.Get(context.Background(), GetParams{IDs: []string{"e1"}, Fields: []string{"body"}})
	if err == nil || !strings.Contains(err.Error(), "unknown field") {
		t.Fatalf("err = %v, want an unknown-field refusal naming the choices", err)
	}
	if !strings.Contains(err.Error(), "bodyText") {
		t.Errorf("refusal does not list the full vocabulary: %v", err)
	}
}

// A projection padded with [""] names nothing, so it is no projection at all —
// the same reading [] gets. Erroring on it would be the v0.13.0 trap again.
func TestPaddedFieldsAreNoProjection(t *testing.T) {
	for _, names := range [][]string{{}, {""}, {"  "}} {
		set, err := parseFields(names)
		if err != nil {
			t.Fatalf("fields %q refused: %v", names, err)
		}
		if set.projected() {
			t.Errorf("fields %q read as a projection", names)
		}
	}
	set, err := parseFields([]string{" subject "})
	if err != nil || !set.projected() || !set["subject"] {
		t.Errorf("a real field with padding whitespace was lost: %v %v", set, err)
	}
}

// TW-032: the ids a selection handle exists to replace. The handle, the
// counts, the total and the query state all survive; the array does not.
func TestSearchReturnIDsNoneKeepsTheHandleAndDropsTheArray(t *testing.T) {
	f := &fake{}
	f.handler = func(calls []jmap.Invocation) *jmap.Response {
		switch calls[0].Name {
		case "Mailbox/get":
			return response(result("Mailbox/get", calls[0].CallID, map[string]any{"list": mailboxFixture}))
		case "Email/query":
			return response(result("Email/query", calls[0].CallID, map[string]any{
				"ids": []string{"e1"}, "position": 0, "queryState": "qs-1", "total": 4000,
			}))
		}
		panic("unexpected call " + calls[0].Name)
	}
	s := testService(f)
	res, err := s.Search(context.Background(), SearchParams{
		Mailbox: "inbox", ReturnIDs: ReturnIDsNone, IncludeTotal: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if batch := findBatch(t, f, "Email/query"); len(batch) != 1 {
		t.Fatalf("batch = %v, want Email/query alone", batch)
	}
	if len(res.IDs) != 0 || len(res.Emails) != 0 {
		t.Errorf("ids/emails survived returnIds:none: %+v %+v", res.IDs, res.Emails)
	}
	if res.SelectionID == "" {
		t.Error("no selectionId — the handle is the whole point of dropping the ids")
	}
	if res.Returned != 1 || res.QueryState != "qs-1" || res.Total == nil || *res.Total != 4000 {
		t.Errorf("returned = %d queryState = %q total = %v — all three must survive", res.Returned, res.QueryState, res.Total)
	}
	// No message id may reach the transcript, in any key.
	if payload := stringify(res); strings.Contains(payload, `"e1"`) {
		t.Errorf("payload still carries a message id:\n%s", payload)
	}

	// The handle must still name the set it suppressed.
	sel, err := s.handles.getSelection(res.SelectionID)
	if err != nil || len(sel.IDs) != 1 || sel.IDs[0] != "e1" {
		t.Fatalf("selection = %+v err=%v", sel, err)
	}
}

// Bounded waves prove placement afterwards by sampling where each batch began
// and ended: two ids, not two hundred.
func TestSearchReturnIDsBoundaries(t *testing.T) {
	f := &fake{}
	f.handler = func(calls []jmap.Invocation) *jmap.Response {
		switch calls[0].Name {
		case "Mailbox/get":
			return response(result("Mailbox/get", calls[0].CallID, map[string]any{"list": mailboxFixture}))
		case "Email/query":
			return response(result("Email/query", calls[0].CallID, map[string]any{
				"ids": []string{"e1", "e2", "e3"}, "position": 0, "queryState": "qs-1",
			}))
		}
		panic("unexpected call " + calls[0].Name)
	}
	s := testService(f)
	res, err := s.Search(context.Background(), SearchParams{Mailbox: "inbox", ReturnIDs: ReturnIDsBoundaries})
	if err != nil {
		t.Fatal(err)
	}
	if res.FirstID != "e1" || res.LastID != "e3" {
		t.Errorf("boundaries = %q..%q, want e1..e3", res.FirstID, res.LastID)
	}
	if len(res.IDs) != 0 {
		t.Errorf("boundaries must not also return the array: %+v", res.IDs)
	}
	if res.SelectionID == "" || res.Returned != 3 {
		t.Errorf("selectionId = %q returned = %d", res.SelectionID, res.Returned)
	}
	if payload := stringify(res); strings.Contains(payload, `"e2"`) {
		t.Errorf("payload carries the interior ids:\n%s", payload)
	}
}

// Suppressing the ids returns no per-message property at all, so a projection
// cannot also apply. Refusing the combination would refuse a caller that
// padded fields with [] — the value the schema promises is inert.
func TestReturnIDsOverridesAPaddedProjection(t *testing.T) {
	for _, fields := range [][]string{nil, {}, {""}, {"subject", "preview"}} {
		f := projectionFake()
		s := testService(f)
		res, err := s.Search(context.Background(), SearchParams{
			Mailbox: "inbox", ReturnIDs: ReturnIDsNone, Fields: fields,
		})
		if err != nil {
			t.Fatalf("fields %q alongside returnIds:none was refused: %v", fields, err)
		}
		if batch := findBatch(t, f, "Email/query"); len(batch) != 1 {
			t.Errorf("fields %q: still fetched per-message properties: %v", fields, batch)
		}
		if len(res.Emails) != 0 || len(res.IDs) != 0 {
			t.Errorf("fields %q: returned %+v / %+v", fields, res.Emails, res.IDs)
		}
	}
}

// Dropping the ids is the cheapest shape there is, so it must reach the same
// 500-id page the id-only projection does — one page, three mutating batches.
func TestReturnIDsNoneRaisesThePageLimit(t *testing.T) {
	f := projectionFake()
	s := testService(f)
	if _, err := s.Search(context.Background(), SearchParams{
		Mailbox: "inbox", ReturnIDs: ReturnIDsNone, Limit: maxProjectedSearchLimit,
	}); err != nil {
		t.Fatal(err)
	}
	q := argsOf(t, findBatch(t, f, "Email/query")[0])
	if q["limit"] != float64(maxProjectedSearchLimit+1) { // +1 is the hasMore probe
		t.Errorf("limit = %v, want the projected cap", q["limit"])
	}
}

// "" must mean today, because "" is what a model that fills every property
// sends when it has nothing to say.
func TestReturnIDsPaddedValueIsToday(t *testing.T) {
	for _, mode := range []string{"", "  ", "all", "ALL"} {
		f := projectionFake()
		s := testService(f)
		res, err := s.Search(context.Background(), SearchParams{
			Mailbox: "inbox", ReturnIDs: mode, Fields: []string{"id"},
		})
		if err != nil {
			t.Fatalf("returnIds %q refused: %v", mode, err)
		}
		if len(res.IDs) != 1 || res.IDs[0] != "e1" {
			t.Errorf("returnIds %q changed the default result: %+v", mode, res.IDs)
		}
		if res.FirstID != "" || res.LastID != "" {
			t.Errorf("returnIds %q added boundary ids to the default shape", mode)
		}
	}
	s := testService(projectionFake())
	_, err := s.Search(context.Background(), SearchParams{Mailbox: "inbox", ReturnIDs: "counts"})
	if err == nil || !strings.Contains(err.Error(), "returnIds") {
		t.Fatalf("err = %v, want a refusal naming the valid modes", err)
	}
}

// What TW-032 is worth, in the units the field report used. The handle wave
// still pays for one id array per search; dropping it is the rest.
func TestReturnIDsNoneShrinksTheSearchResult(t *testing.T) {
	const page = 200
	ids := make([]string, page)
	for i := range ids {
		ids[i] = "SuZdSx00000" + string(rune('a'+i%26))
	}
	render := func(r SearchResult) int {
		b, err := json.MarshalIndent(r, "", "  ") // exactly what jsonResult does
		if err != nil {
			t.Fatal(err)
		}
		return len(b)
	}
	base := SearchResult{AccountID: "u1", Position: 0, Limit: page, Returned: page,
		QueryState: "qs-1", SelectionID: "sel_dd82cf4eb8cf"}
	withIDs := base
	withIDs.IDs = ids
	countsOnly := base
	boundaries := base
	boundaries.FirstID, boundaries.LastID = ids[0], ids[page-1]

	full, none, bounded := render(withIDs), render(countsOnly), render(boundaries)
	if none >= full/10 {
		t.Errorf("returnIds:none is %d bytes against %d — expected an order of magnitude", none, full)
	}
	if bounded >= none+120 {
		t.Errorf("boundary sampling costs %d bytes over %d — that is more than two ids", bounded-none, none)
	}
	t.Logf("200-id page: all=%dB none=%dB boundaries=%dB", full, none, bounded)
}
