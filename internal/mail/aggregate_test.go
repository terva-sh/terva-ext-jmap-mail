package mail

// email_count and email_group: the two tools that answer a survey's questions
// without any message crossing the tool boundary.
//
// What these pin is mostly negative — what must NOT come back. A distribution
// whose rows carried subjects, or a count that minted a selection, would work
// perfectly and reintroduce the cost the tools exist to remove.

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"terva-ext-jmap-mail/internal/jmap"
)

// aggFake serves a cohort with senders, keywords and dates, and records every
// Email/query filter it is asked for.
type aggFake struct {
	*fake
	queries []map[string]any
	state   string
}

type aggMsg struct {
	id, from, received string
	seen               bool
}

func newAggFake(msgs []aggMsg) *aggFake {
	a := &aggFake{fake: &fake{}, state: "qs-1"}
	byID := map[string]aggMsg{}
	for _, m := range msgs {
		byID[m.id] = m
	}
	a.handler = func(calls []jmap.Invocation) *jmap.Response {
		var out []jmap.InvocationResult
		for _, c := range calls {
			switch c.Name {
			case "Mailbox/get":
				out = append(out, result("Mailbox/get", c.CallID, map[string]any{"list": mailboxFixture}))
			case "Email/query":
				args := argsOfAny(c)
				filter, _ := args["filter"].(map[string]any)
				a.queries = append(a.queries, filter)
				// The fixture matches everything; what is being tested is the
				// shape of the request and the fold, not the server's filter.
				ids := make([]string, 0, len(msgs))
				for _, m := range msgs {
					ids = append(ids, m.id)
				}
				if limit, ok := args["limit"].(int); ok && limit > 0 && limit < len(ids) {
					ids = ids[:limit]
				} else if ok && limit == 0 {
					ids = nil // a strict server returns no ids for limit 0
				}
				res := map[string]any{"position": 0, "ids": ids, "queryState": a.state}
				if args["calculateTotal"] == true {
					res["total"] = len(msgs)
				}
				out = append(out, result("Email/query", c.CallID, res))
			case "Email/get":
				ids, _ := argsOfAny(c)["ids"].([]string)
				list := make([]any, 0, len(ids))
				for _, id := range ids {
					m := byID[id]
					list = append(list, map[string]any{
						"id": m.id, "receivedAt": m.received,
						"from":     []any{map[string]any{"name": "Sender " + m.from, "email": m.from}},
						"keywords": map[string]bool{"$seen": m.seen},
					})
				}
				out = append(out, result("Email/get", c.CallID, map[string]any{"list": list}))
			default:
				panic("unexpected call " + c.Name)
			}
		}
		return response(out...)
	}
	return a
}

func aggCohort(n int) []aggMsg {
	senders := []string{"loud@lists.test", "loud@lists.test", "loud@lists.test", "quiet@corp.test", "mid@news.test"}
	out := make([]aggMsg, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, aggMsg{
			id:       fmt.Sprintf("e%04d", i),
			from:     senders[i%len(senders)],
			received: fmt.Sprintf("2026-0%d-%02dT10:00:00Z", 1+(i%3), 1+(i%28)),
			seen:     i%2 == 0,
		})
	}
	return out
}

// --- email_count ---

// The property the tool exists for: every row shares one request and reports
// one state, so a table built from them reconciles by construction. Ten
// separate searches cannot promise that.
func TestCountEvaluatesEveryRowInOneRequest(t *testing.T) {
	f := newAggFake(aggCohort(40))
	s := testService(f.fake)

	res, err := s.Count(context.Background(), CountParams{Queries: []CountQuery{
		{Label: "unread", Filter: SearchParams{NotKeyword: "$seen"}},
		{Label: "flagged", Filter: SearchParams{Keyword: "$flagged"}},
		{Label: "old", Filter: SearchParams{Before: "2026-01-01"}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Counts) != 3 {
		t.Fatalf("counts = %+v", res.Counts)
	}
	// One batch carrying three Email/query calls, not three batches.
	var queryBatches int
	for _, batch := range f.recorded {
		n := 0
		for _, c := range batch {
			if c.Name == "Email/query" {
				n++
			}
		}
		if n > 0 {
			queryBatches++
			if n != 3 {
				t.Errorf("a batch carried %d queries, want all 3 together", n)
			}
		}
	}
	if queryBatches != 1 {
		t.Errorf("rows were spread over %d requests; they must share one to share a state", queryBatches)
	}
	if res.QueryState != "qs-1" {
		t.Errorf("queryState = %q, want the one state every row was measured against", res.QueryState)
	}
	if res.StatesDiffered {
		t.Error("states reported as differing when the fixture never changed")
	}
	for _, row := range res.Counts {
		if row.Error != "" || row.Total != 40 {
			t.Errorf("row %+v", row)
		}
	}
}

// No message is fetched and no selection is minted — the two costs the
// {limit:1, includeTotal:true} counting shape paid fifty-seven times in one
// session.
func TestCountFetchesNothingAndMintsNothing(t *testing.T) {
	f := newAggFake(aggCohort(40))
	s := testService(f.fake)

	before := len(s.handles.selections)
	if _, err := s.Count(context.Background(), CountParams{Queries: []CountQuery{
		{Label: "all", Filter: SearchParams{Mailbox: "inbox"}},
	}}); err != nil {
		t.Fatal(err)
	}
	for _, batch := range f.recorded {
		for _, c := range batch {
			if c.Name == "Email/get" {
				t.Error("counting fetched a message")
			}
			if c.Name != "Email/query" {
				continue
			}
			if limit, ok := argsOfAny(c)["limit"].(int); !ok || limit != 0 {
				t.Errorf("query asked for limit %v, want 0 — a count has no page", argsOfAny(c)["limit"])
			}
		}
	}
	if len(s.handles.selections) != before {
		t.Errorf("counting minted %d selection handles; nobody will use them", len(s.handles.selections)-before)
	}
}

// A mailbox that changes mid-request breaks the one promise this tool makes,
// so it has to say so rather than hand back rows that quietly do not sum.
func TestCountReportsWhenTheStateMovedUnderIt(t *testing.T) {
	// A fake that reports a fresh state for every query, which is what a
	// mailbox changing between two invocations of the same request looks like.
	f := newAggFake(aggCohort(10))
	seen := 0
	f.handler = func(calls []jmap.Invocation) *jmap.Response {
		var out []jmap.InvocationResult
		for _, c := range calls {
			seen++
			out = append(out, result("Email/query", c.CallID, map[string]any{
				"position": 0, "ids": []string{}, "total": seen,
				"queryState": fmt.Sprintf("qs-%d", seen),
			}))
		}
		return response(out...)
	}
	s := testService(f.fake)
	res, err := s.Count(context.Background(), CountParams{Queries: []CountQuery{
		{Label: "a", Filter: SearchParams{Keyword: "$seen"}},
		{Label: "b", Filter: SearchParams{NotKeyword: "$seen"}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if !res.StatesDiffered {
		t.Fatal("rows measured against different states were reported as one observation")
	}
	if res.QueryState != "" {
		t.Errorf("queryState = %q — naming one state would assert the thing that just failed", res.QueryState)
	}
	if !strings.Contains(res.Note, "may not sum") {
		t.Errorf("note does not say what is wrong with the table: %q", res.Note)
	}
	// The counts themselves still come back: each is correct for its own
	// moment, and discarding them would lose real information.
	if len(res.Counts) != 2 {
		t.Errorf("counts dropped on drift: %+v", res.Counts)
	}
}

// A padded row — a model filling every declared property — is padding, not a
// request to count the entire account.
func TestCountDropsPaddedRowsAndRefusesUnlabeledOnes(t *testing.T) {
	s := testService(newAggFake(aggCohort(5)).fake)
	ctx := context.Background()

	res, err := s.Count(ctx, CountParams{Queries: []CountQuery{
		{Label: "real", Filter: SearchParams{Mailbox: "inbox"}},
		{Label: "", Filter: SearchParams{}},
		{Label: "   ", Filter: SearchParams{Mailbox: "  "}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Counts) != 1 || res.Counts[0].Label != "real" {
		t.Fatalf("padded rows survived: %+v", res.Counts)
	}

	// A filter with no label is a different mistake and must not be numbered
	// for the caller: the label is how the number is matched to the question.
	if _, err := s.Count(ctx, CountParams{Queries: []CountQuery{
		{Label: "", Filter: SearchParams{Mailbox: "inbox"}},
	}}); err == nil || !strings.Contains(err.Error(), "no label") {
		t.Fatalf("err = %v", err)
	}
	// Two rows of the same name make an unreadable table.
	if _, err := s.Count(ctx, CountParams{Queries: []CountQuery{
		{Label: "dup", Filter: SearchParams{Mailbox: "inbox"}},
		{Label: "dup", Filter: SearchParams{Mailbox: "archive"}},
	}}); err == nil || !strings.Contains(err.Error(), "labeled") {
		t.Fatalf("err = %v", err)
	}
	// And nothing at all is refused with an example, not a bare complaint.
	if _, err := s.Count(ctx, CountParams{}); err == nil || !strings.Contains(err.Error(), "\"label\"") {
		t.Fatalf("err = %v, want a refusal showing the shape", err)
	}
}

// The bound is the server's request budget, and the refusal has to explain why
// splitting is not just permitted but changes what the answer means.
func TestCountRefusesMoreRowsThanOneRequestHolds(t *testing.T) {
	s := testService(newAggFake(aggCohort(5)).fake)
	var many []CountQuery
	for i := 0; i < 40; i++ {
		many = append(many, CountQuery{
			Label: fmt.Sprintf("row%d", i), Filter: SearchParams{From: fmt.Sprintf("a%d@x.test", i)},
		})
	}
	_, err := s.Count(context.Background(), CountParams{Queries: many})
	if err == nil {
		t.Fatal("accepted more rows than one request can hold")
	}
	for _, want := range []string{"at most", "shares one", "runs of"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal never says %q: %v", want, err)
		}
	}
}

// --- email_group ---

// The whole point: a distribution, exactly, with no message summary in it.
func TestGroupRanksSendersWithoutReturningMessages(t *testing.T) {
	f := newAggFake(aggCohort(100))
	s := testService(f.fake)

	res, err := s.Group(context.Background(), GroupParams{
		Filter: SearchParams{Mailbox: "inbox"}, GroupBy: GroupByFrom,
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Matched != 100 || res.Scanned != 100 || res.Truncated {
		t.Fatalf("matched=%d scanned=%d truncated=%v", res.Matched, res.Scanned, res.Truncated)
	}
	if len(res.Groups) != 3 || res.DistinctKeys != 3 {
		t.Fatalf("groups = %+v", res.Groups)
	}
	// Ranked biggest first, and the counts are exact rather than sampled: the
	// cohort is 3/5 loud, 1/5 each of the others.
	if res.Groups[0].Key != "loud@lists.test" || res.Groups[0].Total != 60 {
		t.Errorf("top group = %+v, want the exact 60 from loud@lists.test", res.Groups[0])
	}
	if res.Groups[0].Unread != 30 {
		t.Errorf("unread = %d, want half of 60", res.Groups[0].Unread)
	}
	if res.Groups[0].Newest == "" || res.Groups[0].Oldest == "" {
		t.Errorf("group carries no time bounds: %+v", res.Groups[0])
	}

	// Nothing a third party wrote may be in the result. The display name is
	// the trap: the fake supplies one for every message.
	rendered, err := json.Marshal(res)
	if err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{"Sender ", "subject", "preview", "\"id\""} {
		if strings.Contains(string(rendered), bad) {
			t.Errorf("group result carries %q:\n%s", bad, rendered)
		}
	}
}

// A ranking over part of a mailbox is not a ranking of the mailbox, and the
// failure of the pattern this replaces was that nothing said which one it was.
func TestGroupSaysWhenItRankedAWindow(t *testing.T) {
	f := newAggFake(aggCohort(300))
	s := testService(f.fake)

	res, err := s.Group(context.Background(), GroupParams{
		Filter: SearchParams{Mailbox: "inbox"}, GroupBy: GroupByFrom, MaxMessages: 100,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !res.Truncated {
		t.Fatal("a ranking over 100 of 300 did not report itself as truncated")
	}
	if res.Matched != 300 || res.Scanned != 100 {
		t.Errorf("matched=%d scanned=%d, want the pair that makes truncation legible", res.Matched, res.Scanned)
	}
	if !strings.Contains(res.Note, "not the whole set") {
		t.Errorf("note does not warn against reading the order as the mailbox's: %q", res.Note)
	}
}

// The tail is weighed, not listed: a survey acts on the head, and the rest is
// a number rather than two hundred rows.
func TestGroupCountsTheTailItDoesNotList(t *testing.T) {
	s := testService(newAggFake(aggCohort(100)).fake)
	res, err := s.Group(context.Background(), GroupParams{
		Filter: SearchParams{Mailbox: "inbox"}, GroupBy: GroupByFrom, GroupLimit: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Groups) != 1 {
		t.Fatalf("groupLimit ignored: %+v", res.Groups)
	}
	if res.DistinctKeys != 3 {
		t.Errorf("distinctKeys = %d, want every group found", res.DistinctKeys)
	}
	if res.OtherTotal != 40 {
		t.Errorf("otherTotal = %d, want the 40 messages outside the returned row", res.OtherTotal)
	}
	if res.Groups[0].Total+res.OtherTotal != res.Scanned {
		t.Errorf("returned rows plus tail (%d) does not account for the scan (%d)",
			res.Groups[0].Total+res.OtherTotal, res.Scanned)
	}
}

// The age histogram every one of these sessions was building by hand, one
// search per bucket.
func TestGroupBucketsByAge(t *testing.T) {
	s := testService(newAggFake(aggCohort(90)).fake)
	res, err := s.Group(context.Background(), GroupParams{
		Filter: SearchParams{Mailbox: "inbox"}, GroupBy: GroupByReceivedAt, Interval: IntervalMonth,
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Interval != IntervalMonth {
		t.Errorf("interval = %q", res.Interval)
	}
	// The fixture spans three months, bucketed to their first day.
	if len(res.Groups) != 3 {
		t.Fatalf("groups = %+v, want one per month", res.Groups)
	}
	var sum int
	for _, g := range res.Groups {
		if !strings.HasSuffix(g.Key, "-01") {
			t.Errorf("bucket key %q is not a month start", g.Key)
		}
		sum += g.Total
	}
	if sum != res.Scanned {
		t.Errorf("buckets sum to %d, scan was %d — a histogram that does not add up is worse than none", sum, res.Scanned)
	}
}

// groupBy is required and has no default, for the same reason mark's action
// does: there is no distribution that is the obviously-intended one, so a
// padded call must read a sentence rather than get an arbitrary answer.
func TestGroupRefusesAPaddedGroupBy(t *testing.T) {
	s := testService(newAggFake(aggCohort(5)).fake)
	ctx := context.Background()

	_, err := s.Group(ctx, GroupParams{Filter: SearchParams{Mailbox: "inbox"}, GroupBy: ""})
	if err == nil || !strings.Contains(err.Error(), "groupBy is required") {
		t.Fatalf("err = %v", err)
	}
	for _, want := range []string{GroupByFrom, GroupByReceivedAt} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal does not name %q", want)
		}
	}
	if _, err := s.Group(ctx, GroupParams{GroupBy: "sender"}); err == nil || !strings.Contains(err.Error(), "invalid groupBy") {
		t.Fatalf("err = %v", err)
	}
	// An interval alongside from-grouping is inert, not a conflict.
	if _, err := s.Group(ctx, GroupParams{
		Filter: SearchParams{Mailbox: "inbox"}, GroupBy: GroupByFrom, Interval: IntervalWeek,
	}); err != nil {
		t.Fatalf("an inert interval was refused: %v", err)
	}
}

// The scan is chunked to the server's maxObjectsInGet: a single get over 5,000
// ids is a request-level error, and the survey would fail rather than page.
func TestGroupChunksTheScanToServerLimits(t *testing.T) {
	f := newAggFake(aggCohort(1200))
	s := testService(f.fake)
	if _, err := s.Group(context.Background(), GroupParams{
		Filter: SearchParams{Mailbox: "inbox"}, GroupBy: GroupByFrom,
	}); err != nil {
		t.Fatal(err)
	}
	limits, _ := testSession().CoreLimits()
	var gets int
	for _, batch := range f.recorded {
		for _, c := range batch {
			if c.Name != "Email/get" {
				continue
			}
			gets++
			ids, _ := argsOfAny(c)["ids"].([]string)
			if uint64(len(ids)) > limits.MaxObjectsInGet {
				t.Errorf("an Email/get asked for %d ids, above the server's %d", len(ids), limits.MaxObjectsInGet)
			}
			// Only the properties the fold needs — a survey that pulled
			// subjects would be the payload problem again, one layer down.
			props, _ := argsOfAny(c)["properties"].([]string)
			for _, p := range props {
				switch p {
				case "id", "receivedAt", "keywords", "from":
				default:
					t.Errorf("the scan asked for %q, which no grouping needs", p)
				}
			}
		}
	}
	if gets < 3 {
		t.Errorf("a 1200-message scan issued %d gets; it should have chunked", gets)
	}
}

// The fold already holds each group's ids; throwing them away meant "archive
// everything from the top sender" cost another search per row — the shape this
// tool exists to remove, one level up.
func TestGroupNamesEachGroupWithAHandle(t *testing.T) {
	f := newAggFake(aggCohort(100))
	s := testService(f.fake)
	ctx := context.Background()

	res, err := s.Group(ctx, GroupParams{
		Filter: SearchParams{Mailbox: "inbox"}, GroupBy: GroupByFrom,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Groups) != 3 {
		t.Fatalf("groups = %+v", res.Groups)
	}
	seen := map[string]bool{}
	for _, g := range res.Groups {
		if g.SelectionID == "" {
			t.Fatalf("group %q carries no handle, so acting on it needs a re-search", g.Key)
		}
		if seen[g.SelectionID] {
			t.Errorf("two groups share a handle")
		}
		seen[g.SelectionID] = true
		held, err := s.handles.getSelection(g.SelectionID)
		if err != nil {
			t.Fatalf("group %q: %v", g.Key, err)
		}
		if len(held.IDs) != g.Total {
			t.Errorf("group %q says %d but its handle names %d", g.Key, g.Total, len(held.IDs))
		}
	}
	if !strings.Contains(res.HandleNote, "email_move") {
		t.Errorf("nothing says the handles are actionable: %q", res.HandleNote)
	}

	// And the handle really works: the top sender's messages archive in one call.
	top := res.Groups[0]
	dry, err := s.Move(ctx, MoveParams{Handle: top.SelectionID, ToMailbox: "archive", DryRun: true})
	if err != nil {
		t.Fatalf("a group handle is not usable by the organize tools: %v", err)
	}
	if dry.Selection == nil || dry.Selection.Count != top.Total {
		t.Errorf("move over the group covers %+v, want its %d messages", dry.Selection, top.Total)
	}
	// The ids still never entered the transcript.
	rendered, _ := json.Marshal(res)
	if strings.Contains(string(rendered), "\"e0001\"") {
		t.Errorf("group result leaked message ids:\n%s", rendered)
	}
}

// The trap the whole feature has to avoid: a handle over a truncated scan
// would name part of a group while reading as all of it, so "archive
// everything from this sender" would archive some of it, silently. That is the
// sampling failure this tool was built to end, reintroduced through a token.
func TestGroupMintsNoHandlesOnATruncatedScan(t *testing.T) {
	s := testService(newAggFake(aggCohort(300)).fake)
	res, err := s.Group(context.Background(), GroupParams{
		Filter: SearchParams{Mailbox: "inbox"}, GroupBy: GroupByFrom, MaxMessages: 100,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !res.Truncated {
		t.Fatal("fixture did not truncate")
	}
	for _, g := range res.Groups {
		if g.SelectionID != "" {
			t.Errorf("group %q got a handle over a scanned window: %+v", g.Key, g)
		}
	}
	for _, want := range []string{"truncated", "maxMessages"} {
		if !strings.Contains(res.HandleNote, want) {
			t.Errorf("handleNote does not explain the absence (%q): %q", want, res.HandleNote)
		}
	}
}

// Grouping a named set answers a question no filter can express, because JMAP
// has no id condition: "are my four hundred failures all one sender?"
func TestGroupAcceptsAHandleInsteadOfAFilter(t *testing.T) {
	f := newAggFake(aggCohort(50))
	s := testService(f.fake)
	ctx := context.Background()

	subset := make([]string, 0, 10)
	for i := 0; i < 10; i++ {
		subset = append(subset, fmt.Sprintf("e%04d", i))
	}
	handle := s.handles.putSelection(&selection{AccountID: "A1", IDs: subset})

	res, err := s.Group(ctx, GroupParams{Handle: handle, GroupBy: GroupByFrom})
	if err != nil {
		t.Fatal(err)
	}
	if res.Matched != 10 || res.Scanned != 10 || res.Truncated {
		t.Errorf("matched=%d scanned=%d truncated=%v, want the handle's 10", res.Matched, res.Scanned, res.Truncated)
	}
	var sum int
	for _, g := range res.Groups {
		sum += g.Total
	}
	if sum != 10 {
		t.Errorf("groups sum to %d over a 10-message handle", sum)
	}
	// No query ran, so there is no state to report and none is invented.
	if res.QueryState != "" {
		t.Errorf("queryState = %q, but no query was issued", res.QueryState)
	}
	var queries int
	for _, batch := range f.recorded {
		for _, c := range batch {
			if c.Name == "Email/query" {
				queries++
			}
		}
	}
	if queries != 0 {
		t.Errorf("grouping a handle issued %d queries; the set was already named", queries)
	}

	// A handle and a filter are two ways to name the set, so both is refused.
	if _, err := s.Group(ctx, GroupParams{
		Handle: handle, Filter: SearchParams{Mailbox: "inbox"}, GroupBy: GroupByFrom,
	}); err == nil || !strings.Contains(err.Error(), "not both") {
		t.Fatalf("err = %v", err)
	}
}
