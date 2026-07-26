package mail

// Selections, receipts, and what each of them guarantees. The safety claims
// are the point: a receipt cannot name a set other than the one previewed, an
// applied receipt does not act twice, and a phrase minted for one batch cannot
// confirm another.

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"terva-ext-jmap-mail/internal/jmap"
)

// handleFake answers Mailbox/get, the search batch, and the organize batches,
// over a fixed cohort of ids. mailboxOf lets a test move a message underneath
// a receipt to exercise drift.
type handleFake struct {
	*fake
	cohort    []string
	mailboxOf map[string]string
}

func newHandleFake(n int) *handleFake {
	h := &handleFake{fake: &fake{}, mailboxOf: map[string]string{}}
	for i := 0; i < n; i++ {
		id := "e" + string(rune('a'+i%26)) + strings.Repeat("x", 1+i/26)
		h.cohort = append(h.cohort, id)
		h.mailboxOf[id] = "mb-inbox"
	}
	h.handler = func(calls []jmap.Invocation) *jmap.Response {
		switch calls[0].Name {
		case "Mailbox/get":
			return response(result("Mailbox/get", calls[0].CallID, map[string]any{"list": mailboxFixture}))
		case "Email/query":
			return response(result("Email/query", calls[0].CallID, map[string]any{
				"ids": h.cohort, "position": 0, "queryState": "qs-1",
			}))
		case "Email/get":
			ids, _ := argsOfAny(calls[0])["ids"].([]string)
			list := make([]any, 0, len(ids))
			for _, id := range ids {
				list = append(list, map[string]any{
					"id": id, "subject": "Subject " + id,
					"mailboxIds": map[string]bool{h.mailboxOf[id]: true},
					"keywords":   map[string]bool{},
				})
			}
			out := []jmap.InvocationResult{result("Email/get", calls[0].CallID, map[string]any{"list": list})}
			if len(calls) > 1 {
				updated := map[string]any{}
				for _, id := range ids {
					updated[id] = nil
				}
				out = append(out, result("Email/set", calls[1].CallID, map[string]any{
					"updated": updated, "oldState": "s1", "newState": "s2",
				}))
			}
			return response(out...)
		}
		panic("unexpected call " + calls[0].Name)
	}
	return h
}

func handleService(t *testing.T, n int) (*Service, *handleFake) {
	t.Helper()
	f := newHandleFake(n)
	return testService(f.fake), f
}

// searchSelection runs an id-only search and returns its handle.
func searchSelection(t *testing.T, s *Service) *SearchResult {
	t.Helper()
	res, err := s.Search(context.Background(), SearchParams{Mailbox: "inbox", Fields: []string{"id"}, Limit: 500})
	if err != nil {
		t.Fatal(err)
	}
	if res.SelectionID == "" {
		t.Fatal("search minted no selectionId")
	}
	return res
}

// --- the payload claim ---

func TestSelectionReplacesTheIDList(t *testing.T) {
	s, f := handleService(t, 30)
	page := searchSelection(t, s)
	ctx := context.Background()

	dry, err := s.Move(ctx, MoveParams{Selection: page.SelectionID, ToMailbox: "archive", DryRun: true})
	if err != nil {
		t.Fatal(err)
	}
	if dry.ReceiptID == "" {
		t.Fatal("dry run minted no receiptId")
	}
	if dry.Selection == nil || dry.Selection.Count != 30 || dry.Selection.Remaining != 0 {
		t.Fatalf("selection use = %+v", dry.Selection)
	}
	// The apply names neither the ids nor the phrase, and still moves exactly
	// the previewed set.
	applied, err := s.Move(ctx, MoveParams{Receipt: dry.ReceiptID})
	if err != nil {
		t.Fatal(err)
	}
	if applied.MovedCount != 30 {
		t.Errorf("movedCount = %d, want 30", applied.MovedCount)
	}
	// The mutating batch really did carry the selection's ids — the handle is
	// a way to name them, not a way to send fewer.
	sent := stringifyIDs(lastBatch(t, f.fake, "Email/set")[0])
	if len(sent) != 30 {
		t.Errorf("mutating batch carried %d ids, want the selection's 30", len(sent))
	}
	for i, id := range page.IDs {
		if sent[i] != id {
			t.Fatalf("id %d = %s, want %s", i, sent[i], id)
		}
	}
}

// lastBatch returns the most recent recorded batch containing method — the
// organize batches all lead with Email/get, so "which batch" has to be decided
// by what else is in it.
func lastBatch(t *testing.T, f *fake, method string) []jmap.Invocation {
	t.Helper()
	for i := len(f.recorded) - 1; i >= 0; i-- {
		for _, c := range f.recorded[i] {
			if c.Name == method {
				return f.recorded[i]
			}
		}
	}
	t.Fatalf("no recorded batch containing %s", method)
	return nil
}

// A 500-id page cannot be consumed by one 200-id mutating call, so it is taken
// in slices — and because the ids were pinned at search time, moving the first
// slice does not shift the second.
func TestSelectionSlicesAtTheMutatingCap(t *testing.T) {
	s, _ := handleService(t, 450)
	page := searchSelection(t, s)
	ctx := context.Background()

	var seen []string
	for offset := 0; ; {
		dry, err := s.Move(ctx, MoveParams{Selection: page.SelectionID, SelectionOffset: offset, ToMailbox: "archive", DryRun: true})
		if err != nil {
			t.Fatal(err)
		}
		use := dry.Selection
		if use == nil {
			t.Fatal("sliced call reported no selection use")
		}
		if use.Count > maxSetIDs {
			t.Fatalf("slice of %d exceeds the %d-id mutating cap", use.Count, maxSetIDs)
		}
		applied, err := s.Move(ctx, MoveParams{Receipt: dry.ReceiptID})
		if err != nil {
			t.Fatal(err)
		}
		// The apply describes its own place in the selection, so a resuming
		// caller can read it off the record of what happened.
		if applied.Selection == nil || applied.Selection.Offset != offset {
			t.Fatalf("apply lost its place in the selection: %+v", applied.Selection)
		}
		seen = append(seen, page.IDs[offset:offset+use.Count]...)
		if use.Remaining == 0 {
			break
		}
		offset += use.Count
	}
	if len(seen) != 450 {
		t.Fatalf("slices covered %d ids, want 450", len(seen))
	}
	for i, id := range page.IDs {
		if seen[i] != id {
			t.Fatalf("slice %d = %s, want %s — the slices must tile the selection in order", i, seen[i], id)
		}
	}
	// Past the end is an error, not a silent empty run.
	if _, err := s.Move(ctx, MoveParams{Selection: page.SelectionID, SelectionOffset: 450, ToMailbox: "archive", DryRun: true}); err == nil {
		t.Error("offset past the end accepted")
	}
}

// --- the safety claims ---

// The gap the field report named: preview one set, apply a different set the
// same size to the same destination. A receipt closes it structurally, and the
// phrase closes it for callers still passing ids.
func TestAppliedSetIsThePreviewedSet(t *testing.T) {
	s, _ := handleService(t, 25)
	ctx := context.Background()
	first := searchSelection(t, s)

	dry, err := s.Move(ctx, MoveParams{Selection: first.SelectionID, ToMailbox: "archive", DryRun: true})
	if err != nil {
		t.Fatal(err)
	}

	// A receipt carries its own ids: there is no argument by which a caller
	// could hand it different ones.
	if _, err := s.Move(ctx, MoveParams{Receipt: dry.ReceiptID, IDs: []string{"other-1"}}); err == nil {
		t.Error("receipt accepted alongside ids — one of them would have to be ignored")
	}
	// Nor a different destination.
	if _, err := s.Move(ctx, MoveParams{Receipt: dry.ReceiptID, ToMailbox: "trash"}); err == nil {
		t.Error("receipt accepted a destination other than the one previewed")
	}
	// Naming the same destination another way is fine.
	if _, err := s.Move(ctx, MoveParams{Receipt: dry.ReceiptID, ToMailbox: "Archive"}); err != nil {
		t.Errorf("receipt refused its own destination by name: %v", err)
	}

	// And the phrase, for the ids path: same count, same destination, same
	// account, different messages — must not confirm.
	other := make([]string, 25)
	for i := range other {
		other[i] = "different-" + string(rune('a'+i))
	}
	stale := movePhrase(25, "Archive", "A1", other)
	if _, err := s.Move(ctx, MoveParams{IDs: dry.snapshotIDs(), ToMailbox: "archive", Confirm: stale}); err == nil {
		t.Error("a phrase minted for a different batch of the same size confirmed this one")
	}
}

// snapshotIDs recovers the ids a result observed, for tests that need to feed
// the same set back through the plain-ids path.
func (r *MoveResult) snapshotIDs() []string {
	out := make([]string, 0, len(r.snapshot))
	for id := range r.snapshot {
		out = append(out, id)
	}
	return out
}

// A receipt minted by one tool's dry run must not apply through another's.
func TestReceiptIsBoundToItsTool(t *testing.T) {
	s, _ := handleService(t, 5)
	ctx := context.Background()
	page := searchSelection(t, s)

	moveDry, err := s.Move(ctx, MoveParams{Selection: page.SelectionID, ToMailbox: "archive", DryRun: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Trash(ctx, TrashParams{Receipt: moveDry.ReceiptID}); err == nil || !strings.Contains(err.Error(), "move dry run") {
		t.Errorf("a move receipt applied as a trash: %v", err)
	}
	if _, err := s.Mark(ctx, MarkParams{Receipt: moveDry.ReceiptID, Action: "read"}); err == nil {
		t.Error("a move receipt applied as a mark")
	}

	markDry, err := s.Mark(ctx, MarkParams{Selection: page.SelectionID, Action: "read", DryRun: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Mark(ctx, MarkParams{Receipt: markDry.ReceiptID, Action: "flag"}); err == nil {
		t.Error("a receipt previewing 'read' applied as 'flag'")
	}
	if _, err := s.Mark(ctx, MarkParams{Receipt: markDry.ReceiptID, Action: "read"}); err != nil {
		t.Errorf("receipt refused its own action: %v", err)
	}
}

// The interruption case TW-025 describes: the apply ran, the result was lost.
// Re-presenting the receipt must answer "already done, here is what happened"
// rather than doing it again.
func TestAppliedReceiptReplaysInsteadOfReapplying(t *testing.T) {
	s, f := handleService(t, 25)
	ctx := context.Background()
	page := searchSelection(t, s)

	dry, err := s.Move(ctx, MoveParams{Selection: page.SelectionID, ToMailbox: "archive", DryRun: true})
	if err != nil {
		t.Fatal(err)
	}
	first, err := s.Move(ctx, MoveParams{Receipt: dry.ReceiptID})
	if err != nil {
		t.Fatal(err)
	}
	if first.Replayed {
		t.Error("the first apply reported itself as a replay")
	}
	setsAfterFirst := countBatches(f.fake, "Email/set")

	second, err := s.Move(ctx, MoveParams{Receipt: dry.ReceiptID})
	if err != nil {
		t.Fatal(err)
	}
	if !second.Replayed || second.AppliedAt == "" {
		t.Errorf("re-presented receipt did not report itself as a replay: %+v", second)
	}
	if second.MovedCount != first.MovedCount {
		t.Errorf("replay reported %d moved, original %d", second.MovedCount, first.MovedCount)
	}
	if got := countBatches(f.fake, "Email/set"); got != setsAfterFirst {
		t.Errorf("replay issued %d more Email/set batches, want 0", got-setsAfterFirst)
	}
	// Replay markers must not leak back into the stored result.
	if third, _ := s.Move(ctx, MoveParams{Receipt: dry.ReceiptID}); !third.Replayed {
		t.Error("second replay lost its marker")
	}
	if first.Replayed {
		t.Error("replaying mutated the original result")
	}
}

// countBatches counts recorded batches whose first-or-second call is name.
func countBatches(f *fake, name string) int {
	n := 0
	for _, batch := range f.recorded {
		for _, c := range batch {
			if c.Name == name {
				n++
				break
			}
		}
	}
	return n
}

// The check that replaces the field report's queryState proposal: compare what
// the apply sees against what the preview saw, per message. Unrelated mail
// arriving does not trip it; a message actually moving does.
func TestReceiptReportsMessagesThatMovedSincePreview(t *testing.T) {
	s, f := handleService(t, 25)
	ctx := context.Background()
	page := searchSelection(t, s)

	dry, err := s.Move(ctx, MoveParams{Selection: page.SelectionID, ToMailbox: "archive", DryRun: true})
	if err != nil {
		t.Fatal(err)
	}
	// Something else files two of them elsewhere between preview and apply.
	moved := []string{page.IDs[3], page.IDs[7]}
	for _, id := range moved {
		f.mailboxOf[id] = "mb-rec1"
	}

	applied, err := s.Move(ctx, MoveParams{Receipt: dry.ReceiptID})
	if err != nil {
		t.Fatal(err)
	}
	if len(applied.Drifted) != 2 {
		t.Fatalf("drifted = %v, want the two messages that moved", applied.Drifted)
	}
	for _, id := range moved {
		if !contains(applied.Drifted, id) {
			t.Errorf("drifted list missing %s: %v", id, applied.Drifted)
		}
	}
	if applied.DriftNote == "" {
		t.Error("drift reported without an explanation of what it means")
	}
	// Reported, not refused: the caller named these exact messages, and they
	// were still moved.
	if applied.MovedCount != 25 {
		t.Errorf("movedCount = %d, want all 25 — drift is a report, not a veto", applied.MovedCount)
	}
}

// --- lifecycle ---

func TestHandlesExpire(t *testing.T) {
	s, _ := handleService(t, 5)
	ctx := context.Background()
	page := searchSelection(t, s)
	dry, err := s.Move(ctx, MoveParams{Selection: page.SelectionID, ToMailbox: "archive", DryRun: true})
	if err != nil {
		t.Fatal(err)
	}

	// Age both handles past the TTL without sleeping.
	s.handles.now = func() time.Time { return time.Now().Add(handleTTL + time.Minute) }

	_, err = s.Move(ctx, MoveParams{Selection: page.SelectionID, ToMailbox: "archive", DryRun: true})
	if err == nil || !strings.Contains(err.Error(), "expired") {
		t.Errorf("expired selection err = %v, want an expiry message", err)
	}
	_, err = s.Move(ctx, MoveParams{Receipt: dry.ReceiptID})
	if err == nil || !strings.Contains(err.Error(), "expired") {
		t.Errorf("expired receipt err = %v, want an expiry message", err)
	}
	// An unknown handle is its own message, and neither says "bug".
	if _, err := s.Move(ctx, MoveParams{Selection: "sel_nope", ToMailbox: "archive", DryRun: true}); err == nil || !strings.Contains(err.Error(), "unknown selection") {
		t.Errorf("unknown selection err = %v", err)
	}
}

func TestHandleStoreIsBounded(t *testing.T) {
	s, _ := handleService(t, 2)
	for i := 0; i < maxHandles*2; i++ {
		searchSelection(t, s)
	}
	s.handles.mu.Lock()
	n := len(s.handles.selections)
	s.handles.mu.Unlock()
	if n > maxHandles {
		t.Errorf("store holds %d selections, cap is %d", n, maxHandles)
	}
}

// The three ways of naming a set are alternatives, not ingredients.
func TestTargetsAreMutuallyExclusive(t *testing.T) {
	s, _ := handleService(t, 5)
	ctx := context.Background()
	page := searchSelection(t, s)

	if _, err := s.Move(ctx, MoveParams{ToMailbox: "archive"}); err == nil || !strings.Contains(err.Error(), "name the messages") {
		t.Errorf("naming nothing = %v", err)
	}
	if _, err := s.Move(ctx, MoveParams{IDs: page.IDs, Selection: page.SelectionID, ToMailbox: "archive"}); err == nil {
		t.Error("ids and selection accepted together")
	}
	// A receipt is an apply; previewing one makes no sense.
	dry, err := s.Move(ctx, MoveParams{Selection: page.SelectionID, ToMailbox: "archive", DryRun: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Move(ctx, MoveParams{Receipt: dry.ReceiptID, DryRun: true}); err == nil {
		t.Error("receipt accepted with dryRun")
	}
}

// A selection is scoped to the account it was minted against.
func TestSelectionIsBoundToItsAccount(t *testing.T) {
	s, _ := handleService(t, 5)
	page := searchSelection(t, s)
	s.handles.mu.Lock()
	s.handles.selections[page.SelectionID].AccountID = "other-account"
	s.handles.mu.Unlock()

	_, err := s.Move(context.Background(), MoveParams{Selection: page.SelectionID, ToMailbox: "archive", DryRun: true})
	if err == nil || !strings.Contains(err.Error(), "other-account") {
		t.Errorf("cross-account selection err = %v", err)
	}
}

// At read-only the organize tools are withdrawn, so a token naming a set they
// could operate on is noise the caller cannot act on.
func TestNoSelectionWithoutOrganizeAccess(t *testing.T) {
	f := newHandleFake(5)
	s := readOnlyService(f.fake)
	res, err := s.Search(context.Background(), SearchParams{Mailbox: "inbox", Fields: []string{"id"}})
	if err != nil {
		t.Fatal(err)
	}
	if res.SelectionID != "" {
		t.Errorf("read-only search minted selectionId %q", res.SelectionID)
	}
	if len(res.IDs) != 5 {
		t.Errorf("read-only search should still return its ids: %v", res.IDs)
	}
}

// Mark's drift check watches the keyword the run is about to change, not
// placement: the question for a mark is whether the message was already in the
// state the preview counted it in.
func TestMarkReceiptReportsKeywordDrift(t *testing.T) {
	s, f := handleService(t, 25)
	ctx := context.Background()
	page := searchSelection(t, s)

	dry, err := s.Mark(ctx, MarkParams{Selection: page.SelectionID, Action: "read", DryRun: true})
	if err != nil {
		t.Fatal(err)
	}
	if dry.ChangedCount != 25 || dry.AlreadySetCount != 0 {
		t.Fatalf("preview counted %d changed / %d already set", dry.ChangedCount, dry.AlreadySetCount)
	}
	// The user reads one of them on their phone between preview and apply.
	read := page.IDs[4]
	base := f.handler
	f.handler = func(calls []jmap.Invocation) *jmap.Response {
		resp := base(calls)
		if calls[0].Name != "Email/get" {
			return resp
		}
		return rewriteKeywords(t, resp, calls[0].CallID, read)
	}

	applied, err := s.Mark(ctx, MarkParams{Receipt: dry.ReceiptID, Action: "read"})
	if err != nil {
		t.Fatal(err)
	}
	if len(applied.Drifted) != 1 || applied.Drifted[0] != read {
		t.Errorf("drifted = %v, want just %s", applied.Drifted, read)
	}
	if applied.AlreadySetCount != 1 || applied.ChangedCount != 24 {
		t.Errorf("counts = %d changed / %d already set, want 24/1", applied.ChangedCount, applied.AlreadySetCount)
	}
}

// rewriteKeywords marks one id as $seen in an Email/get response.
func rewriteKeywords(t *testing.T, resp *jmap.Response, callID, seen string) *jmap.Response {
	t.Helper()
	res, err := resp.Result(callID)
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		List []map[string]any `json:"list"`
	}
	if err := json.Unmarshal(res.Args, &got); err != nil {
		t.Fatal(err)
	}
	for _, e := range got.List {
		if e["id"] == seen {
			e["keywords"] = map[string]bool{"$seen": true}
		}
	}
	out := []jmap.InvocationResult{result("Email/get", callID, map[string]any{"list": got.List})}
	for _, r := range resp.MethodResponses {
		if r.CallID != callID {
			out = append(out, r)
		}
	}
	return response(out...)
}

// Trash takes handles too, and its receipt is its own kind.
func TestTrashAcceptsSelectionAndReceipt(t *testing.T) {
	s, _ := handleService(t, 25)
	ctx := context.Background()
	page := searchSelection(t, s)

	dry, err := s.Trash(ctx, TrashParams{Selection: page.SelectionID, DryRun: true})
	if err != nil {
		t.Fatal(err)
	}
	if dry.ReceiptID == "" || dry.Destination.Role != "trash" {
		t.Fatalf("trash dry run = %+v", dry)
	}
	// Above the threshold and with no confirm phrase in sight: the receipt is
	// the authorization.
	applied, err := s.Trash(ctx, TrashParams{Receipt: dry.ReceiptID})
	if err != nil {
		t.Fatal(err)
	}
	if applied.MovedCount != 25 {
		t.Errorf("movedCount = %d, want 25", applied.MovedCount)
	}
	if _, err := s.Move(ctx, MoveParams{Receipt: dry.ReceiptID, ToMailbox: "archive"}); err == nil {
		t.Error("a trash receipt applied as a move")
	}
}
