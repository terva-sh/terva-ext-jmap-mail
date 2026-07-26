package mail

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"terva-ext-jmap-mail/internal/jmap"
)

var emailFixture = map[string]any{
	"id":         "e1",
	"threadId":   "t1",
	"mailboxIds": map[string]bool{"mb-inbox": true},
	"keywords":   map[string]bool{"$seen": true, "$flagged": true},
	"size":       12345,
	"receivedAt": "2026-06-30T10:00:00Z",
	"from":       []map[string]string{{"name": "Alice", "email": "alice@example.com"}},
	"to":         []map[string]string{{"email": "user@example.com"}},
	"subject":    "Hello",
	"preview":    "Hi there — quick question…",
}

// searchFake answers Mailbox/get (resolution) and the Email/query+Email/get
// batch.
func searchFake() *fake {
	f := &fake{}
	f.handler = func(calls []jmap.Invocation) *jmap.Response {
		switch calls[0].Name {
		case "Mailbox/get":
			return response(result("Mailbox/get", calls[0].CallID, map[string]any{"list": mailboxFixture}))
		case "Email/query":
			return response(
				result("Email/query", calls[0].CallID, map[string]any{"ids": []string{"e1"}, "position": 0}),
				result("Email/get", calls[1].CallID, map[string]any{"list": []any{emailFixture}}),
			)
		}
		panic("unexpected call " + calls[0].Name)
	}
	return f
}

func TestSearchBatchAndSummaries(t *testing.T) {
	f := searchFake()
	s := testService(f)
	hasAtt := true
	res, err := s.Search(context.Background(), SearchParams{
		Mailbox: "inbox", Text: "invoice", From: "alice", Subject: "Hello",
		After: "2026-06-01", Before: "2026-07-01T00:00:00Z",
		HasAttachment: &hasAtt, Keyword: "$flagged", NotKeyword: "$seen",
		CollapseThreads: true, Limit: 5, Sort: "oldest",
	})
	if err != nil {
		t.Fatal(err)
	}

	// The batch: Email/query then Email/get chained by a result reference.
	batch := findBatch(t, f, "Email/query")
	if len(batch) != 2 || batch[0].Name != "Email/query" || batch[1].Name != "Email/get" {
		t.Fatalf("batch = %v", batch)
	}
	q := argsOf(t, batch[0])
	filter, _ := q["filter"].(map[string]any)
	want := map[string]any{
		"inMailbox": "mb-inbox", "text": "invoice", "from": "alice", "subject": "Hello",
		"after": "2026-06-01T00:00:00Z", "before": "2026-07-01T00:00:00Z",
		"hasAttachment": true, "hasKeyword": "$flagged", "notKeyword": "$seen",
	}
	for k, v := range want {
		if filter[k] != v {
			t.Errorf("filter[%s] = %v, want %v", k, filter[k], v)
		}
	}
	if q["collapseThreads"] != true || q["limit"] != float64(6) { // requested 5 + the hasMore probe
		t.Errorf("query args = %v", q)
	}
	sortArg := stringify(q["sort"])
	if !strings.Contains(sortArg, `"receivedAt"`) || !strings.Contains(sortArg, `"isAscending":true`) {
		t.Errorf("sort = %s", sortArg)
	}
	g := argsOf(t, batch[1])
	ref := stringify(g["#ids"])
	if !strings.Contains(ref, `"resultOf":"q0"`) || !strings.Contains(ref, `"path":"/ids"`) {
		t.Errorf("#ids = %s", ref)
	}
	if strings.Contains(stringify(g["properties"]), "bodyValues") {
		t.Errorf("search must not fetch bodies: %v", g["properties"])
	}

	// Summary mapping.
	if res.Returned != 1 || len(res.Emails) != 1 {
		t.Fatalf("res = %+v", res)
	}
	e := res.Emails[0]
	if e.ID != "e1" || e.ThreadID != "t1" || e.Subject != "Hello" {
		t.Errorf("summary = %+v", e)
	}
	if len(e.Keywords) != 2 || e.Keywords[0] != "$flagged" || e.Keywords[1] != "$seen" {
		t.Errorf("keywords not sorted: %v", e.Keywords)
	}
	if len(e.From) != 1 || e.From[0].Email != "alice@example.com" || e.From[0].Name != "Alice" {
		t.Errorf("from = %+v", e.From)
	}
	if len(e.Mailboxes) != 1 || e.Mailboxes[0].Name != "Inbox" || e.Mailboxes[0].Role != "inbox" {
		t.Errorf("mailbox annotation = %+v", e.Mailboxes)
	}
}

func TestSearchDefaults(t *testing.T) {
	f := searchFake()
	s := testService(f)
	if _, err := s.Search(context.Background(), SearchParams{}); err != nil {
		t.Fatal(err)
	}
	q := argsOf(t, findBatch(t, f, "Email/query")[0])
	if q["limit"] != float64(defaultSearchLimit+1) || q["position"] != float64(0) {
		t.Errorf("defaults: %v", q)
	}
	if _, hasFilter := q["filter"]; hasFilter {
		t.Errorf("empty search should send no filter: %v", q["filter"])
	}
	if !strings.Contains(stringify(q["sort"]), `"isAscending":false`) {
		t.Errorf("default sort should be newest-first: %v", q["sort"])
	}
}

func TestSearchLimitClamped(t *testing.T) {
	f := searchFake()
	s := testService(f)
	if _, err := s.Search(context.Background(), SearchParams{Limit: 5000}); err != nil {
		t.Fatal(err)
	}
	q := argsOf(t, findBatch(t, f, "Email/query")[0])
	if q["limit"] != float64(maxSearchLimit+1) {
		t.Errorf("limit = %v, want clamped %d + probe", q["limit"], maxSearchLimit)
	}
}

// --- field projection (bulk-organization payloads) ---

// projectionFake answers Email/query alone (id-only projections skip the get)
// or the query+get pair, echoing back only the requested properties the way a
// server does.
func projectionFake() *fake {
	f := &fake{}
	f.handler = func(calls []jmap.Invocation) *jmap.Response {
		switch calls[0].Name {
		case "Mailbox/get":
			return response(result("Mailbox/get", calls[0].CallID, map[string]any{"list": mailboxFixture}))
		case "Email/query":
			qres := result("Email/query", calls[0].CallID, map[string]any{"ids": []string{"e1"}, "position": 0})
			if len(calls) == 1 {
				return response(qres)
			}
			props := map[string]bool{}
			for _, p := range argsOfAny(calls[1])["properties"].([]string) {
				props[p] = true
			}
			e := map[string]any{}
			for k, v := range emailFixture {
				if props[k] {
					e[k] = v
				}
			}
			return response(qres, result("Email/get", calls[1].CallID, map[string]any{"list": []any{e}}))
		}
		panic("unexpected call " + calls[0].Name)
	}
	return f
}

// The case that matters: ids and nothing else, in one method call.
func TestSearchIDOnlyProjectionSkipsGet(t *testing.T) {
	f := projectionFake()
	s := testService(f)
	res, err := s.Search(context.Background(), SearchParams{Mailbox: "inbox", Fields: []string{"id"}})
	if err != nil {
		t.Fatal(err)
	}
	if batch := findBatch(t, f, "Email/query"); len(batch) != 1 {
		t.Fatalf("batch = %v, want Email/query alone", batch)
	}
	if len(res.Emails) != 1 || res.Emails[0].ID != "e1" {
		t.Fatalf("emails = %+v", res.Emails)
	}
	// Nothing but the id may survive into the payload.
	if got := stringify(res.Emails); got != `[{"id":"e1"}]` {
		t.Errorf("emails = %s, want ids only", got)
	}
}

func TestSearchProjectionNarrowsGetProperties(t *testing.T) {
	f := projectionFake()
	s := testService(f)
	res, err := s.Search(context.Background(), SearchParams{Fields: []string{"subject", "receivedAt"}})
	if err != nil {
		t.Fatal(err)
	}
	batch := findBatch(t, f, "Email/query")
	if len(batch) != 2 {
		t.Fatalf("batch = %v, want query + get", batch)
	}
	// The projection maps onto Email/get properties, so the saving is provider
	// bandwidth too — id is always along for the ride.
	if props := stringify(argsOf(t, batch[1])["properties"]); props != `["id","subject","receivedAt"]` {
		t.Errorf("properties = %s", props)
	}
	e := res.Emails[0]
	if e.Subject != "Hello" || e.ReceivedAt == "" {
		t.Errorf("requested fields missing: %+v", e)
	}
	if e.Preview != "" || len(e.From) != 0 || e.ThreadID != "" || e.Size != 0 {
		t.Errorf("unrequested fields returned: %+v", e)
	}
}

// mailboxes is the one projected field with a cost beyond the get: it drives a
// mailbox annotation lookup, which an id-only caller must not pay for.
func TestSearchProjectionMailboxAnnotation(t *testing.T) {
	s := testService(projectionFake())
	with, err := s.Search(context.Background(), SearchParams{Fields: []string{"mailboxes"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(with.Emails[0].Mailboxes) != 1 || with.Emails[0].Mailboxes[0].Name != "Inbox" {
		t.Errorf("mailboxes = %+v", with.Emails[0].Mailboxes)
	}
	without, err := s.Search(context.Background(), SearchParams{Fields: []string{"subject"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(without.Emails[0].Mailboxes) != 0 {
		t.Errorf("mailboxes = %+v, want none", without.Emails[0].Mailboxes)
	}
}

func TestSearchRejectsUnknownField(t *testing.T) {
	s := testService(projectionFake())
	_, err := s.Search(context.Background(), SearchParams{Fields: []string{"id", "bodyText"}})
	if err == nil || !strings.Contains(err.Error(), `unknown field "bodyText"`) {
		t.Fatalf("err = %v, want a refusal naming the field", err)
	}
	if !strings.Contains(err.Error(), "receivedAt") {
		t.Errorf("refusal should list the valid fields: %v", err)
	}
}

func TestSearchProjectedLimitCap(t *testing.T) {
	for _, tc := range []struct {
		name   string
		fields []string
		want   int
	}{
		{"unprojected clamps to 100", nil, maxSearchLimit},
		{"id-only reaches 500", []string{"id"}, maxProjectedSearchLimit},
		{"preview-free projection reaches 500", []string{"subject", "from"}, maxProjectedSearchLimit},
		{"projection keeping preview stays at 100", []string{"subject", "preview"}, maxSearchLimit},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := projectionFake()
			s := testService(f)
			res, err := s.Search(context.Background(), SearchParams{Fields: tc.fields, Limit: 5000})
			if err != nil {
				t.Fatal(err)
			}
			q := argsOf(t, findBatch(t, f, "Email/query")[0])
			if q["limit"] != float64(tc.want+1) { // +1 is the hasMore probe
				t.Errorf("query limit = %v, want %d", q["limit"], tc.want+1)
			}
			if res.Limit != tc.want {
				t.Errorf("reported limit = %d, want %d", res.Limit, tc.want)
			}
		})
	}
}

// A server MAY enforce a smaller limit than the one asked for and report it
// (RFC 8620 §5.5). Reading the truncated probe as "no more matches" would end
// a paging loop mid-backlog, silently.
func TestSearchServerCappedLimitStillReportsHasMore(t *testing.T) {
	const serverCap = 50
	f := &fake{}
	f.handler = func(calls []jmap.Invocation) *jmap.Response {
		ids := make([]string, serverCap)
		for i := range ids {
			ids[i] = fmt.Sprintf("e-%d", i)
		}
		return response(result("Email/query", calls[0].CallID, map[string]any{
			"ids": ids, "position": 0, "limit": serverCap,
		}))
	}
	s := testService(f)
	res, err := s.Search(context.Background(), SearchParams{Fields: []string{"id"}, Limit: 500})
	if err != nil {
		t.Fatal(err)
	}
	if !res.HasMore {
		t.Error("hasMore = false — a server-side cap must not read as the end of the results")
	}
	if res.Returned != serverCap || res.Limit != serverCap {
		t.Errorf("returned/limit = %d/%d, want the server's cap %d so paging stays consistent", res.Returned, res.Limit, serverCap)
	}
}

func TestSearchRejectsBadInput(t *testing.T) {
	s := testService(searchFake())
	if _, err := s.Search(context.Background(), SearchParams{Sort: "sideways"}); err == nil {
		t.Error("want error for bad sort")
	}
	if _, err := s.Search(context.Background(), SearchParams{After: "not-a-date"}); err == nil {
		t.Error("want error for bad after date")
	}
	if _, err := s.Search(context.Background(), SearchParams{Mailbox: "no-such-box"}); err == nil {
		t.Error("want error for unresolvable mailbox")
	}
}

// pagedFake answers Email/query with n ids (e-0 … e-n-1) plus a matching
// Email/get list in REVERSE order, exercising the query-order reassembly.
func pagedFake(n int, total int) *fake {
	f := &fake{}
	f.handler = func(calls []jmap.Invocation) *jmap.Response {
		switch calls[0].Name {
		case "Mailbox/get":
			return response(result("Mailbox/get", calls[0].CallID, map[string]any{"list": mailboxFixture}))
		case "Email/query":
			ids := make([]string, n)
			list := make([]any, n)
			for i := range n {
				ids[i] = fmt.Sprintf("e-%d", i)
				list[n-1-i] = map[string]any{"id": ids[i], "threadId": "t1", "mailboxIds": map[string]bool{"mb-inbox": true}}
			}
			qargs := map[string]any{"ids": ids, "position": 0}
			if boolArg(calls[0], "calculateTotal") {
				qargs["total"] = total
			}
			return response(
				result("Email/query", calls[0].CallID, qargs),
				result("Email/get", calls[1].CallID, map[string]any{"list": list}),
			)
		}
		panic("unexpected call " + calls[0].Name)
	}
	return f
}

func boolArg(inv jmap.Invocation, key string) bool {
	v, _ := argsOfAny(inv)[key].(bool)
	return v
}

func TestSearchHasMoreOverfetch(t *testing.T) {
	// The server has limit+1 matches: page is truncated to limit, hasMore set.
	f := pagedFake(4, 0)
	s := testService(f)
	res, err := s.Search(context.Background(), SearchParams{Limit: 3})
	if err != nil {
		t.Fatal(err)
	}
	q := argsOf(t, findBatch(t, f, "Email/query")[0])
	if q["limit"] != float64(4) {
		t.Errorf("query limit = %v, want requested+1 = 4", q["limit"])
	}
	if !res.HasMore || res.Returned != 3 || res.Limit != 3 || len(res.Emails) != 3 {
		t.Fatalf("res = %+v, want 3 results with hasMore", res)
	}
	// Email/get returned reverse order; results must follow query order.
	for i, e := range res.Emails {
		if want := fmt.Sprintf("e-%d", i); e.ID != want {
			t.Errorf("emails[%d].ID = %s, want %s (query order)", i, e.ID, want)
		}
	}
	if _, ok := q["calculateTotal"]; ok {
		t.Errorf("calculateTotal sent without includeTotal")
	}
	if res.Total != nil {
		t.Errorf("total = %v, want nil", *res.Total)
	}
}

func TestSearchLastPageHasNoMore(t *testing.T) {
	f := pagedFake(2, 0)
	s := testService(f)
	res, err := s.Search(context.Background(), SearchParams{Limit: 3})
	if err != nil {
		t.Fatal(err)
	}
	if res.HasMore || res.Returned != 2 {
		t.Fatalf("res = %+v, want 2 results without hasMore", res)
	}
}

func TestSearchIncludeTotal(t *testing.T) {
	f := pagedFake(2, 41)
	s := testService(f)
	res, err := s.Search(context.Background(), SearchParams{Limit: 3, IncludeTotal: true})
	if err != nil {
		t.Fatal(err)
	}
	q := argsOf(t, findBatch(t, f, "Email/query")[0])
	if q["calculateTotal"] != true {
		t.Errorf("calculateTotal not requested: %v", q)
	}
	if res.Total == nil || *res.Total != 41 {
		t.Fatalf("total = %v, want 41", res.Total)
	}
}

func TestSearchFilterJSONPassthrough(t *testing.T) {
	f := searchFake()
	s := testService(f)
	rawFilter := `{"operator":"OR","conditions":[{"from":"a@x.com"},{"from":"b@x.com"}]}`
	res, err := s.Search(context.Background(), SearchParams{FilterJSON: []byte(rawFilter)})
	if err != nil {
		t.Fatal(err)
	}
	q := argsOf(t, findBatch(t, f, "Email/query")[0])
	if stringify(q["filter"]) != stringify(res.Query) || !strings.Contains(stringify(q["filter"]), `"operator":"OR"`) {
		t.Errorf("filter = %s", stringify(q["filter"]))
	}
}

func TestSearchFilterJSONWithMailboxANDs(t *testing.T) {
	f := searchFake()
	s := testService(f)
	_, err := s.Search(context.Background(), SearchParams{Mailbox: "inbox", FilterJSON: []byte(`{"from":"a@x.com"}`)})
	if err != nil {
		t.Fatal(err)
	}
	filter := stringify(argsOf(t, findBatch(t, f, "Email/query")[0])["filter"])
	for _, want := range []string{`"operator":"AND"`, `"inMailbox":"mb-inbox"`, `"from":"a@x.com"`} {
		if !strings.Contains(filter, want) {
			t.Errorf("filter %s missing %s", filter, want)
		}
	}
}

func TestSearchFilterJSONRejectsConflictsAndGarbage(t *testing.T) {
	s := testService(searchFake())
	_, err := s.Search(context.Background(), SearchParams{From: "a@x.com", FilterJSON: []byte(`{}`)})
	if err == nil || !strings.Contains(err.Error(), "from") {
		t.Errorf("want conflict error naming the param, got %v", err)
	}
	for _, bad := range []string{`"just a string"`, `[1,2]`, `null`, `{broken`} {
		if _, err := s.Search(context.Background(), SearchParams{FilterJSON: []byte(bad)}); err == nil {
			t.Errorf("filterJson %s: want error", bad)
		}
	}
}

func TestSearchMethodErrorSurfaces(t *testing.T) {
	f := &fake{}
	f.handler = func(calls []jmap.Invocation) *jmap.Response {
		return response(jmap.InvocationResult{Name: "error", Args: []byte(`{"type":"invalidArguments","description":"bad filter"}`), CallID: calls[0].CallID})
	}
	s := testService(f)
	_, err := s.Search(context.Background(), SearchParams{Text: "x"})
	if err == nil || !strings.Contains(err.Error(), "invalidArguments") {
		t.Fatalf("err = %v, want method error", err)
	}
}
