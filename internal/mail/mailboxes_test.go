package mail

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"terva-ext-jmap-mail/internal/jmap"
)

func intp(i int) *int { return &i }

var mailboxFixture = []Mailbox{
	{ID: "mb-inbox", Name: "Inbox", Role: "inbox", SortOrder: 1, TotalEmails: intp(42), UnreadEmails: intp(3)},
	{ID: "mb-arch", Name: "Archive", Role: "archive", SortOrder: 3},
	{ID: "mb-trash", Name: "Trash", Role: "trash", SortOrder: 5},
	{ID: "mb-rec1", Name: "Receipts", SortOrder: 10},
	{ID: "mb-rec2", Name: "Receipts", ParentID: "mb-arch", SortOrder: 11},
}

func mailboxFake() *fake {
	f := &fake{}
	f.handler = func(calls []jmap.Invocation) *jmap.Response {
		if calls[0].Name == "Mailbox/get" {
			return response(result("Mailbox/get", calls[0].CallID, map[string]any{"list": mailboxFixture}))
		}
		panic("unexpected call " + calls[0].Name)
	}
	return f
}

func TestListMailboxes(t *testing.T) {
	f := mailboxFake()
	s := testService(f)
	list, err := s.ListMailboxes(context.Background(), ListMailboxesParams{IncludeCounts: true})
	if err != nil {
		t.Fatal(err)
	}
	if list.AccountID != "A1" || len(list.Mailboxes) != 5 {
		t.Fatalf("list = %+v", list)
	}
	if list.Mailboxes[0].ID != "mb-inbox" {
		t.Errorf("not sorted by sortOrder: %+v", list.Mailboxes[0])
	}
	if list.Mailboxes[0].TotalEmails == nil || *list.Mailboxes[0].TotalEmails != 42 {
		t.Errorf("counts not parsed: %+v", list.Mailboxes[0])
	}

	// includeCounts drives the requested properties.
	args := argsOf(t, f.recorded[0][0])
	props, _ := args["properties"].([]any)
	joined := make([]string, len(props))
	for i, p := range props {
		joined[i] = p.(string)
	}
	if !contains(joined, "unreadEmails") {
		t.Errorf("properties missing counts: %v", joined)
	}

	f2 := mailboxFake()
	s2 := testService(f2)
	s2.ListMailboxes(context.Background(), ListMailboxesParams{IncludeCounts: false})
	args2 := argsOf(t, f2.recorded[0][0])
	if strings.Contains(stringify(args2["properties"]), "unreadEmails") {
		t.Errorf("counts requested despite includeCounts=false: %v", args2["properties"])
	}
}

func contains(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}

func stringify(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

func TestResolveMailbox(t *testing.T) {
	cases := []struct {
		ref     string
		wantID  string
		wantErr string
	}{
		{ref: "mb-trash", wantID: "mb-trash"},          // by id
		{ref: "inbox", wantID: "mb-inbox"},             // by role
		{ref: "INBOX", wantID: "mb-inbox"},             // role, case-insensitive
		{ref: "archive", wantID: "mb-arch"},            // role beats name
		{ref: "trash", wantID: "mb-trash"},             // role
		{ref: "Archive/Receipts", wantID: "mb-rec2"},   // path disambiguates
		{ref: "archive/receipts", wantID: "mb-rec2"},   // path, case-insensitive
		{ref: "/Archive/Receipts/", wantID: "mb-rec2"}, // stray slashes tolerated
		{ref: "Receipts", wantErr: "ambiguous"},        // duplicate display name
		{ref: "Trash/Receipts", wantErr: "no mailbox matches"},
		{ref: "nope", wantErr: "no mailbox matches"},
		{ref: "", wantErr: "empty mailbox"},
	}
	for _, c := range cases {
		t.Run(c.ref, func(t *testing.T) {
			s := testService(mailboxFake())
			sess, _ := s.getSession(context.Background())
			mb, err := s.resolveMailbox(context.Background(), sess, "A1", c.ref)
			if c.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), c.wantErr) {
					t.Fatalf("err = %v, want containing %q", err, c.wantErr)
				}
				return
			}
			if err != nil || mb.ID != c.wantID {
				t.Fatalf("mb = %+v err = %v, want id %q", mb, err, c.wantID)
			}
		})
	}
}

// An ambiguity error names the candidates — as paths — so the caller can
// switch to a path or id.
func TestResolveMailboxAmbiguityListsIDs(t *testing.T) {
	s := testService(mailboxFake())
	sess, _ := s.getSession(context.Background())
	_, err := s.resolveMailbox(context.Background(), sess, "A1", "Receipts")
	if err == nil || !strings.Contains(err.Error(), "mb-rec1") || !strings.Contains(err.Error(), "mb-rec2") {
		t.Fatalf("err = %v, want candidate ids", err)
	}
	if !strings.Contains(err.Error(), "Archive/Receipts") {
		t.Errorf("err = %v, want candidate paths", err)
	}
}

func TestComputePaths(t *testing.T) {
	list := []Mailbox{
		{ID: "a", Name: "A"},
		{ID: "b", Name: "B", ParentID: "a"},
		{ID: "c", Name: "C", ParentID: "b"},
		{ID: "o", Name: "Orphan", ParentID: "gone"},
		{ID: "x", Name: "X", ParentID: "y"}, // cycle x↔y
		{ID: "y", Name: "Y", ParentID: "x"},
	}
	computePaths(list)
	want := map[string]string{"a": "A", "b": "A/B", "c": "A/B/C", "o": "Orphan", "x": "X", "y": "Y"}
	for _, mb := range list {
		if mb.Path != want[mb.ID] {
			t.Errorf("path(%s) = %q, want %q", mb.ID, mb.Path, want[mb.ID])
		}
	}
}

// Summary annotations carry the path only when it adds information.
func TestMailboxRefPaths(t *testing.T) {
	s := testService(mailboxFake())
	sess, _ := s.getSession(context.Background())
	refs := s.mailboxRefsByID(context.Background(), sess, "A1", map[string]bool{"mb-rec2": true, "mb-inbox": true})
	for _, ref := range refs {
		switch ref.ID {
		case "mb-rec2":
			if ref.Path != "Archive/Receipts" {
				t.Errorf("nested ref path = %q, want Archive/Receipts", ref.Path)
			}
		case "mb-inbox":
			if ref.Path != "" {
				t.Errorf("top-level ref should omit path, got %q", ref.Path)
			}
		}
	}
}

// A miss error lists what exists (with roles) to guide the model.
func TestResolveMailboxMissListsAvailable(t *testing.T) {
	s := testService(mailboxFake())
	sess, _ := s.getSession(context.Background())
	_, err := s.resolveMailbox(context.Background(), sess, "A1", "Spam")
	if err == nil || !strings.Contains(err.Error(), "Inbox [inbox]") {
		t.Fatalf("err = %v, want available list with roles", err)
	}
}

// A resolution miss against a cached list refreshes once before failing.
func TestResolveMailboxRefreshOnMiss(t *testing.T) {
	f := mailboxFake()
	s := testService(f)
	sess, _ := s.getSession(context.Background())
	if _, err := s.cachedMailboxes(context.Background(), sess, "A1"); err != nil {
		t.Fatal(err) // prime the cache
	}
	s.resolveMailbox(context.Background(), sess, "A1", "brand-new")
	if len(f.recorded) != 2 {
		t.Errorf("recorded %d Mailbox/get calls, want 2 (prime + refresh)", len(f.recorded))
	}
}
