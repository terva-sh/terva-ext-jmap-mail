package mail

import (
	"context"
	"strings"
	"testing"
)

// A model that fills in every declared property rather than omitting keys must
// still reach every tool's default behaviour. v0.13.0 learned this on the
// organize tools the expensive way — nineteen refusals and a 2,000-message
// wave that archived nothing — because `ids` had no representable empty value.
// These pin the same property everywhere else: the fully padded call, with
// every optional parameter carrying its inert value, does what omitting them
// all does.
//
// Where the padded value cannot mean "unset" because the parameter is
// genuinely required, the test asserts the second-best outcome instead: a
// refusal from the tool naming what to send, rather than a schema violation
// whose text the model never sees.

func TestPaddedSearchIsAnUnpaddedSearch(t *testing.T) {
	plain := testService(searchFake())
	want, err := plain.Search(context.Background(), SearchParams{Mailbox: "inbox"})
	if err != nil {
		t.Fatal(err)
	}

	pf := searchFake()
	padded := testService(pf)
	got, err := padded.Search(context.Background(), SearchParams{
		Mailbox: "inbox",
		// Every optional property, at the value a padding model sends.
		Text: "", From: "", To: "", Cc: "", Bcc: "", Subject: "", Body: "",
		After: "", Before: "", Keyword: "", NotKeyword: "",
		HasAttachment:   nil, // the string enum's "" resolves here — see parseHasAttachment
		CollapseThreads: false, IncludeTotal: false,
		Fields: []string{}, ReturnIDs: "", Limit: 0, Position: 0, Sort: "",
	})
	if err != nil {
		t.Fatalf("a fully padded search was refused: %v", err)
	}
	if got.Limit != want.Limit || got.Returned != want.Returned || len(got.Emails) != len(want.Emails) {
		t.Errorf("padded search differs: %+v vs %+v", got, want)
	}
	if got.Limit != defaultSearchLimit {
		t.Errorf("limit = %d, want the default %d — 0 must read as unset", got.Limit, defaultSearchLimit)
	}
	// The applied filter must be empty but for the mailbox: a padded value
	// that reached the filter would silently narrow every search made this way.
	q := argsOf(t, findBatch(t, pf, "Email/query")[0])
	filter, _ := q["filter"].(map[string]any)
	if len(filter) != 1 || filter["inMailbox"] != "mb-inbox" {
		t.Errorf("padding leaked into the filter: %v", filter)
	}
}

func TestPaddedThreadLimitIsTheCap(t *testing.T) {
	f := threadFake()
	s := testService(f)
	res, err := s.GetThread(context.Background(), ThreadParams{
		ThreadID: "t1", EmailID: "", IncludeBodies: false,
		Fields: []string{}, Limit: 0, MaxBodyBytes: 0, IncludeFullUrls: false,
	})
	if err != nil {
		t.Fatalf("a fully padded thread fetch was refused: %v", err)
	}
	if res.Returned == 0 {
		t.Errorf("limit 0 returned nothing; it must mean the cap: %+v", res)
	}
}

func TestPaddedMailboxSelectorsAreNoSelectors(t *testing.T) {
	all, err := testService(mailboxFake()).ListMailboxes(context.Background(), ListMailboxesParams{IncludeCounts: true})
	if err != nil {
		t.Fatal(err)
	}
	got, err := testService(mailboxFake()).ListMailboxes(context.Background(), ListMailboxesParams{
		IncludeCounts: true, Mailboxes: []string{""}, Fields: []string{""},
	})
	if err != nil {
		t.Fatalf("padded mailbox selectors were refused: %v", err)
	}
	if len(got.Mailboxes) != len(all.Mailboxes) {
		t.Fatalf("padded selectors narrowed the list: %d of %d", len(got.Mailboxes), len(all.Mailboxes))
	}
	if got.Mailboxes[0].Name == "" || got.Mailboxes[0].TotalEmails == nil {
		t.Errorf("padded fields projected the result down: %+v", got.Mailboxes[0])
	}
	// A ref that names a real mailbox nobody has is still an error — the point
	// is that blank is padding, not that misses became silent.
	if _, err := testService(mailboxFake()).ListMailboxes(context.Background(), ListMailboxesParams{
		Mailboxes: []string{"Nonexistent"},
	}); err == nil {
		t.Error("a mailbox reference matching nothing must still be an error")
	}
}

// action and origin are genuinely required, so their padded "" cannot mean
// anything — but the refusal must come from the tool, naming the choices,
// rather than from schema validation the model cannot read.
func TestPaddedRequiredEnumsRefuseByName(t *testing.T) {
	s := testService(organizeFake())
	_, err := s.Mark(context.Background(), MarkParams{IDs: []string{"e1"}, Action: ""})
	if err == nil {
		t.Fatal("empty action was accepted")
	}
	for _, want := range []string{"read", "unread", "flag", "unflag"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal %q does not name %q", err, want)
		}
	}
}

func TestPaddedBodyFormatIsTheDefault(t *testing.T) {
	f := getFake()
	s := testService(f)
	res, err := s.Get(context.Background(), GetParams{
		IDs: []string{"e1"}, Fields: []string{}, BodyFormat: "", MaxBodyBytes: 0, IncludeFullUrls: false,
	})
	if err != nil {
		t.Fatalf("a fully padded email_get was refused: %v", err)
	}
	if res.Emails[0].BodyText == "" {
		t.Errorf("bodyFormat \"\" must mean text: %+v", res.Emails[0])
	}
}
