package mail

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// sourceCount returns how many messages a move reported from one mailbox id,
// or -1 if that mailbox is not in the breakdown at all.
func sourceCount(res *MoveResult, mailboxID string) int {
	for _, s := range res.Sources {
		if s.ID == mailboxID {
			return s.Count
		}
	}
	return -1
}

// TW-036: a mutation's own output must be able to assert where mail came from,
// not only where it went. Names are user-editable and non-unique across
// parents; ids are neither, which is why the destination has always carried
// one. A ledger row built from a name-only breakdown is inference, and it
// degrades quietly — a rename mid-wave produces a record that is wrong and
// internally consistent.
func TestMoveReportsSourcesByID(t *testing.T) {
	ids := bulkIDs(30)
	f := bulkFake()
	s := testService(f)
	res, err := s.Move(context.Background(), MoveParams{
		IDs: ids, ToMailbox: "Archive", DryRun: true,
	})
	if err != nil {
		t.Fatal(err)
	}

	if len(res.Sources) == 0 {
		t.Fatal("no source breakdown at all")
	}
	for _, src := range res.Sources {
		if src.ID == "" {
			t.Errorf("source %+v carries no id", src)
		}
		if src.Name == "" {
			t.Errorf("source %+v carries no name — the id alone is unreadable", src)
		}
		if src.Count <= 0 {
			t.Errorf("source %+v has no count", src)
		}
	}

	// Acceptance: the result is sufficient on its own for both halves of
	// "Inbox <id> → Archive <id>", with no prior email_list_mailboxes.
	if res.Destination.ID == "" || res.Destination.Name == "" {
		t.Errorf("destination = %+v, want an id and a name", res.Destination)
	}
	if sourceCount(res, "mb-inbox") != len(ids) {
		t.Errorf("sources = %+v, want %d from mb-inbox", res.Sources, len(ids))
	}
}

// Acceptance: a move spanning two source mailboxes reports both, with counts.
func TestMoveReportsEverySourceMailbox(t *testing.T) {
	ids := bulkIDs(30)
	s := testService(bulkFake())
	res, err := s.Move(context.Background(), MoveParams{IDs: ids, ToMailbox: "Archive", DryRun: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Sources) != 2 {
		t.Fatalf("sources = %+v, want both mailboxes the fixture files under", res.Sources)
	}
	// Every fifth message is also filed under the nested Receipts folder, so a
	// message in two mailboxes must count once in each.
	if got := sourceCount(res, "mb-inbox"); got != 30 {
		t.Errorf("mb-inbox count = %d, want 30", got)
	}
	if got := sourceCount(res, "mb-rec2"); got != 6 {
		t.Errorf("mb-rec2 count = %d, want 6", got)
	}
	// The nested namesake must be distinguishable from mb-rec1, which shares
	// its display name — the reason a name is not an identifier.
	var nested MailboxCount
	for _, src := range res.Sources {
		if src.ID == "mb-rec2" {
			nested = src
		}
	}
	if nested.Path != "Archive/Receipts" {
		t.Errorf("nested source path = %q, want the disambiguating path", nested.Path)
	}
}

// Biggest contributor first, and the same move must always render the same
// bytes — a breakdown whose order depends on map iteration would show as a
// spurious diff in a committed ledger.
func TestSourceOrderIsStable(t *testing.T) {
	ids := bulkIDs(30)
	var first string
	for i := 0; i < 8; i++ {
		s := testService(bulkFake())
		res, err := s.Move(context.Background(), MoveParams{IDs: ids, ToMailbox: "Archive", DryRun: true})
		if err != nil {
			t.Fatal(err)
		}
		if res.Sources[0].Count < res.Sources[len(res.Sources)-1].Count {
			t.Errorf("sources not ordered by count: %+v", res.Sources)
		}
		got := stringify(res.Sources)
		if i == 0 {
			first = got
		} else if got != first {
			t.Fatalf("source order varies between identical moves:\n%s\n%s", first, got)
		}
	}
}

// The destination reports its path for the same reason the confirm phrase
// quotes one: two folders may share a display name.
func TestDestinationCarriesItsPathWhenNested(t *testing.T) {
	s := testService(bulkFake())
	res, err := s.Move(context.Background(), MoveParams{
		IDs: []string{"e-0"}, ToMailbox: "Archive/Receipts", DryRun: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Destination.ID != "mb-rec2" || res.Destination.Path != "Archive/Receipts" {
		t.Errorf("destination = %+v, want mb-rec2 with its full path", res.Destination)
	}
	// An unnested destination says nothing redundant.
	res, err = s.Move(context.Background(), MoveParams{
		IDs: []string{"e-0"}, ToMailbox: "Archive", DryRun: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Destination.Path != "" {
		t.Errorf("destination path = %q, want it omitted when it equals the name", res.Destination.Path)
	}
}

// email_trash routes through the same moveInto, so it owes the same account of
// where mail came from.
func TestTrashReportsSources(t *testing.T) {
	s := testService(bulkFake())
	res, err := s.Trash(context.Background(), TrashParams{IDs: bulkIDs(30), DryRun: true})
	if err != nil {
		t.Fatal(err)
	}
	if sourceCount(res, "mb-inbox") != 30 {
		t.Errorf("sources = %+v, want the inbox count", res.Sources)
	}
}

// The breakdown is worth its bytes only if it stays a breakdown. Two sources
// on a 200-message batch should cost a couple of hundred bytes, not scale with
// the messages.
func TestSourceBreakdownIsBounded(t *testing.T) {
	res := &MoveResult{
		AccountID:   "u1",
		Destination: MailboxRef{ID: "P3V", Name: "Archive", Role: "archive"},
		MovedCount:  200,
		Sources: []MailboxCount{
			{MailboxRef: MailboxRef{ID: "P-F", Name: "Inbox", Role: "inbox"}, Count: 200},
			{MailboxRef: MailboxRef{ID: "P-Q", Name: "Receipts", Path: "Archive/Receipts"}, Count: 12},
		},
	}
	b, err := json.MarshalIndent(res, "", "  ") // exactly what jsonResult does
	if err != nil {
		t.Fatal(err)
	}
	if len(b) > 600 {
		t.Errorf("a two-source move result is %d bytes:\n%s", len(b), b)
	}
	t.Logf("two-source 200-message move result: %d bytes", len(b))
}

// placementKey is the only encoding of "where a message was" that survives
// into a receipt, so a drift report can only name mailboxes if it round-trips.
func TestPlacementKeyRoundTrips(t *testing.T) {
	for _, ids := range []map[string]bool{
		{},
		{"mb-inbox": true},
		{"mb-inbox": true, "mb-arch": true},
		{"mb-inbox": true, "mb-gone": false}, // false means "not in it"
	} {
		want := map[string]bool{}
		for id, in := range ids {
			if in {
				want[id] = true
			}
		}
		got := placementIDs(placementKey(ids))
		if len(got) != len(want) {
			t.Errorf("placementIDs(placementKey(%v)) = %v", ids, got)
			continue
		}
		for id := range want {
			if !got[id] {
				t.Errorf("round trip lost %s from %v", id, ids)
			}
		}
	}
	// "In no mailbox at all" is a real state, not a missing one.
	if got := placementIDs(""); len(got) != 0 {
		t.Errorf("empty placement = %v, want an empty set", got)
	}
}

// An error naming a mailbox must be both readable and checkable: the label a
// person reads, and the id that label cannot be trusted to imply.
func TestMailboxLabels(t *testing.T) {
	for _, tc := range []struct {
		refs []MailboxRef
		want string
	}{
		{nil, "no mailbox"},
		{[]MailboxRef{{ID: "mb-inbox", Name: "Inbox"}}, "Inbox (mb-inbox)"},
		{[]MailboxRef{{ID: "mb-x"}}, "mb-x"}, // unresolvable: the id alone, not a lie
		{[]MailboxRef{
			{ID: "mb-inbox", Name: "Inbox"},
			{ID: "mb-rec2", Name: "Receipts", Path: "Archive/Receipts"},
		}, "Inbox (mb-inbox) + Archive/Receipts (mb-rec2)"},
	} {
		if got := mailboxLabels(tc.refs); got != tc.want {
			t.Errorf("mailboxLabels(%+v) = %q, want %q", tc.refs, got, tc.want)
		}
	}
}

// The confirm phrase deliberately does NOT name the source, and this pins the
// reason so it is a decision rather than an oversight.
//
// The phrase is minted before any provider round-trip that could reveal where
// the messages are, and the real run must recompute the identical string to
// validate it. Embedding the sources would therefore cost an extra Email/get
// on every bulk call — including the refusal path, which exists to produce an
// error — or split the atomic Email/get + Email/set batch and open a drift
// window the tool itself created.
//
// It costs nothing in safety: the phrase already binds the EXACT id set
// through idBatchDigest, which is strictly stronger than a source name. A
// batch may span several mailboxes and one message may be in several at once,
// so a source in the phrase would be lossy where the digest is not. The dry
// run reports the sources beside the phrase, which is where an operator reads
// them.
func TestConfirmPhraseBindsTheIDSetRatherThanTheSource(t *testing.T) {
	ids := bulkIDs(30)
	s := testService(bulkFake())
	_, err := s.Move(context.Background(), MoveParams{IDs: ids, ToMailbox: "Archive"})
	if err == nil {
		t.Fatal("a bulk move without a confirm phrase was accepted")
	}
	phrase := movePhrase(len(ids), "Archive", "A1", ids)
	if !strings.Contains(err.Error(), phrase) {
		t.Fatalf("refusal %v does not hand back the phrase %q", err, phrase)
	}
	if !strings.Contains(phrase, idBatchDigest(ids)) {
		t.Errorf("phrase %q does not bind the id set", phrase)
	}
	// A different batch of the same size to the same destination must not be
	// confirmable by this phrase — the property a source name would not give.
	other := bulkIDs(30)
	other[0] = "e-999"
	if _, err := s.Move(context.Background(), MoveParams{
		IDs: other, ToMailbox: "Archive", Confirm: phrase,
	}); err == nil {
		t.Error("a phrase minted for one batch confirmed a different one")
	}

	// And the dry run puts the sources next to the phrase it hands back.
	dry, err := s.Move(context.Background(), MoveParams{IDs: ids, ToMailbox: "Archive", DryRun: true})
	if err != nil {
		t.Fatal(err)
	}
	if dry.ConfirmPhrase == "" || len(dry.Sources) == 0 {
		t.Errorf("dry run gave phrase=%q sources=%+v — an operator needs both together", dry.ConfirmPhrase, dry.Sources)
	}
}
