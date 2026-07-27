package mail_test

// email_count and email_group end to end against the hermetic server: the
// filters really filter, the totals really reconcile, and the distribution
// really is exact rather than sampled.

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"terva-ext-jmap-mail/internal/mail"
)

// The property a survey depends on: rows taken in one call sum to the total
// taken in the same call. Three times in two sessions an agent dropped into a
// shell to assert this about a mail tool's own arithmetic.
func TestIntegrationCountRowsReconcile(t *testing.T) {
	_, svc := startStack(t, 0)
	ctx := context.Background()

	res, err := svc.Count(ctx, mail.CountParams{Queries: []mail.CountQuery{
		{Label: "inbox", Filter: mail.SearchParams{Mailbox: "inbox"}},
		{Label: "inbox-read", Filter: mail.SearchParams{Mailbox: "inbox", Keyword: "$seen"}},
		{Label: "inbox-unread", Filter: mail.SearchParams{Mailbox: "inbox", NotKeyword: "$seen"}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Counts) != 3 {
		t.Fatalf("counts = %+v", res.Counts)
	}
	by := map[string]int{}
	for _, row := range res.Counts {
		if row.Error != "" {
			t.Fatalf("row %q: %s", row.Label, row.Error)
		}
		by[row.Label] = row.Total
	}
	if by["inbox"] == 0 {
		t.Fatal("the fixture inbox counted as empty")
	}
	if by["inbox-read"]+by["inbox-unread"] != by["inbox"] {
		t.Errorf("%d read + %d unread != %d total — the rows do not reconcile",
			by["inbox-read"], by["inbox-unread"], by["inbox"])
	}
	if res.QueryState == "" || res.StatesDiffered {
		t.Errorf("rows did not share one state: queryState=%q differed=%v", res.QueryState, res.StatesDiffered)
	}
	// A count against a real server cross-checks against the search that used
	// to be the only way to get it.
	search, err := svc.Search(ctx, mail.SearchParams{
		Mailbox: "inbox", IncludeTotal: true, Fields: []string{"id"}, ReturnIDs: mail.ReturnIDsNone,
	})
	if err != nil {
		t.Fatal(err)
	}
	if search.Total == nil || *search.Total != by["inbox"] {
		t.Errorf("email_count says %d, email_search says %v", by["inbox"], search.Total)
	}
}

// The distribution is exact and carries no message content, against a server
// that really does hold subjects and display names.
func TestIntegrationGroupBySenderIsExactAndContentFree(t *testing.T) {
	_, svc := startStack(t, 0)
	ctx := context.Background()

	res, err := svc.Group(ctx, mail.GroupParams{
		Filter: mail.SearchParams{Mailbox: "inbox"}, GroupBy: mail.GroupByFrom,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Groups) == 0 {
		t.Fatal("no groups over a seeded inbox")
	}
	if res.Truncated {
		t.Errorf("a small fixture reported truncation: matched=%d scanned=%d", res.Matched, res.Scanned)
	}
	// Every scanned message is accounted for by exactly one sender group here,
	// since the fixture has one From each.
	var sum int
	for _, g := range res.Groups {
		sum += g.Total
		if !strings.Contains(g.Key, "@") {
			t.Errorf("group key %q is not an address", g.Key)
		}
	}
	if sum+res.OtherTotal != res.Scanned {
		t.Errorf("groups sum to %d (+%d tail) but %d were scanned", sum, res.OtherTotal, res.Scanned)
	}

	// The seeded subjects and display names must not be anywhere in it.
	rendered, err := json.Marshal(res)
	if err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{"Welcome to the project", "Alice", "subject", "preview"} {
		if strings.Contains(string(rendered), bad) {
			t.Errorf("group result carries %q:\n%s", bad, rendered)
		}
	}

	// And it matches what counting each sender individually would have said —
	// which is the expensive way this replaces.
	top := res.Groups[0]
	check, err := svc.Count(ctx, mail.CountParams{Queries: []mail.CountQuery{
		{Label: "top", Filter: mail.SearchParams{Mailbox: "inbox", From: top.Key}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if check.Counts[0].Total != top.Total {
		t.Errorf("group says %s has %d, counting it says %d", top.Key, top.Total, check.Counts[0].Total)
	}
}

// A filter that matches nothing is a real answer, not an error — a survey asks
// questions whose answer is legitimately zero.
func TestIntegrationAggregatesHandleEmptyResults(t *testing.T) {
	_, svc := startStack(t, 0)
	ctx := context.Background()

	res, err := svc.Group(ctx, mail.GroupParams{
		Filter:  mail.SearchParams{Mailbox: "inbox", From: "nobody@nowhere.invalid"},
		GroupBy: mail.GroupByFrom,
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Matched != 0 || len(res.Groups) != 0 || res.Truncated {
		t.Errorf("empty grouping = %+v", res)
	}
	counts, err := svc.Count(ctx, mail.CountParams{Queries: []mail.CountQuery{
		{Label: "none", Filter: mail.SearchParams{From: "nobody@nowhere.invalid"}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if counts.Counts[0].Total != 0 || counts.Counts[0].Error != "" {
		t.Errorf("empty count = %+v", counts.Counts[0])
	}
}
