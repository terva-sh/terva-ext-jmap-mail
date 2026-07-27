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

	"terva-ext-jmap-mail/internal/config"
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

// bigSelection mints a selection over a whole fixture, using the returnIds
// mode that lifts the page cap: nothing per-message comes back, so the page is
// one selectionId however many ids it names.
func bigSelection(t *testing.T, s *Service, n int) *SearchResult {
	t.Helper()
	res, err := s.Search(context.Background(), SearchParams{
		Mailbox: "inbox", Fields: []string{"id"}, ReturnIDs: ReturnIDsNone, Limit: n,
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.SelectionID == "" {
		t.Fatal("search minted no selectionId")
	}
	return res
}

// A whole search page is now one apply. This is the change TW-049 asked for,
// stated as the property rather than the constant: the page cap and the
// mutating cap are the same number, so "one search, one preview, one apply"
// holds for any page the search will return, and a 13,797-message backlog is
// seven of those rather than sixty-nine.
func TestAFullSearchPageIsOneApply(t *testing.T) {
	s, f := handleService(t, maxHandleSetIDs)
	page := bigSelection(t, s, maxHandleSetIDs)
	ctx := context.Background()

	dry, err := s.Move(ctx, MoveParams{Handle: page.SelectionID, ToMailbox: "archive", DryRun: true})
	if err != nil {
		t.Fatal(err)
	}
	if dry.Selection == nil || dry.Selection.Remaining != 0 {
		t.Fatalf("a full page did not fit one call: %+v", dry.Selection)
	}
	applied, err := s.Move(ctx, MoveParams{Handle: dry.ReceiptID})
	if err != nil {
		t.Fatal(err)
	}
	if applied.MovedCount != maxHandleSetIDs {
		t.Fatalf("movedCount = %d, want the whole page (%d)", applied.MovedCount, maxHandleSetIDs)
	}
	if applied.Aborted {
		t.Fatalf("a full page aborted: %+v", applied.partialRun)
	}
	// It reached the provider in chunks sized to what the server admits, not as
	// one oversized Email/set that would be refused outright.
	chunk := mutationChunk(testSession())
	var sets int
	for _, batch := range f.recorded {
		for _, c := range batch {
			if c.Name != "Email/set" {
				continue
			}
			sets++
			update, _ := argsOfAny(c)["update"].(map[string]any)
			if len(update) > chunk {
				t.Errorf("an Email/set carried %d objects, above the server's %d", len(update), chunk)
			}
		}
	}
	if sets < 2 {
		t.Errorf("a %d-message apply issued %d Email/set calls; it should have been chunked", maxHandleSetIDs, sets)
	}
}

// seedBigSelection mints a selection larger than any search page, which is the
// only way to reach the slicing path now that the two caps are equal.
func seedBigSelection(s *Service, ids []string) string {
	return s.handles.putSelection(&selection{AccountID: "A1", IDs: ids})
}

// --- the payload claim ---

func TestSelectionReplacesTheIDList(t *testing.T) {
	s, f := handleService(t, 30)
	page := searchSelection(t, s)
	ctx := context.Background()

	dry, err := s.Move(ctx, MoveParams{Handle: page.SelectionID, ToMailbox: "archive", DryRun: true})
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
	applied, err := s.Move(ctx, MoveParams{Handle: dry.ReceiptID})
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

// A selection larger than the handle cap is taken in slices — and because the
// ids were pinned at search time, moving the first slice does not shift the
// second.
//
// The cap this tiles against used to be maxSetIDs (200), which meant a 450-id
// page took three applies. It is maxHandleSetIDs now, so this fixture has to
// be bigger than that to slice at all — see
// TestSelectionAboveTheLiteralCapIsOneCall for the case that used to slice and
// no longer does, which is the point of the change.
func TestSelectionSlicesAtTheHandleCap(t *testing.T) {
	s, f := handleService(t, maxHandleSetIDs+250)
	handle := seedBigSelection(s, f.cohort)
	ctx := context.Background()

	var seen []string
	for offset := 0; ; {
		dry, err := s.Move(ctx, MoveParams{Handle: handle, SelectionOffset: offset, ToMailbox: "archive", DryRun: true})
		if err != nil {
			t.Fatal(err)
		}
		use := dry.Selection
		if use == nil {
			t.Fatal("sliced call reported no selection use")
		}
		if use.Count > maxHandleSetIDs {
			t.Fatalf("slice of %d exceeds the %d-id handle cap", use.Count, maxHandleSetIDs)
		}
		applied, err := s.Move(ctx, MoveParams{Handle: dry.ReceiptID})
		if err != nil {
			t.Fatal(err)
		}
		// The apply describes its own place in the selection, so a resuming
		// caller can read it off the record of what happened.
		if applied.Selection == nil || applied.Selection.Offset != offset {
			t.Fatalf("apply lost its place in the selection: %+v", applied.Selection)
		}
		// The page returned no id array at all — that is the mode that lifts
		// the cap — so the tiling is checked against what the server holds.
		seen = append(seen, f.cohort[offset:offset+use.Count]...)
		if use.Remaining == 0 {
			break
		}
		offset += use.Count
	}
	if want := maxHandleSetIDs + 250; len(seen) != want {
		t.Fatalf("slices covered %d ids, want %d", len(seen), want)
	}
	for i, id := range f.cohort {
		if seen[i] != id {
			t.Fatalf("slice %d = %s, want %s — the slices must tile the selection in order", i, seen[i], id)
		}
	}
	// Past the end is an error, not a silent empty run.
	if _, err := s.Move(ctx, MoveParams{Handle: handle, SelectionOffset: maxHandleSetIDs + 250, ToMailbox: "archive", DryRun: true}); err == nil {
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

	dry, err := s.Move(ctx, MoveParams{Handle: first.SelectionID, ToMailbox: "archive", DryRun: true})
	if err != nil {
		t.Fatal(err)
	}

	// A receipt carries its own ids: there is no argument by which a caller
	// could hand it different ones.
	if _, err := s.Move(ctx, MoveParams{Handle: dry.ReceiptID, IDs: []string{"other-1"}}); err == nil {
		t.Error("receipt accepted alongside ids — one of them would have to be ignored")
	}
	// Nor a different destination.
	if _, err := s.Move(ctx, MoveParams{Handle: dry.ReceiptID, ToMailbox: "trash"}); err == nil {
		t.Error("receipt accepted a destination other than the one previewed")
	}
	// Naming the same destination another way is fine.
	if _, err := s.Move(ctx, MoveParams{Handle: dry.ReceiptID, ToMailbox: "Archive"}); err != nil {
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

	moveDry, err := s.Move(ctx, MoveParams{Handle: page.SelectionID, ToMailbox: "archive", DryRun: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Trash(ctx, TrashParams{Handle: moveDry.ReceiptID}); err == nil || !strings.Contains(err.Error(), "move dry run") {
		t.Errorf("a move receipt applied as a trash: %v", err)
	}
	if _, err := s.Mark(ctx, MarkParams{Handle: moveDry.ReceiptID, Action: "read"}); err == nil {
		t.Error("a move receipt applied as a mark")
	}

	markDry, err := s.Mark(ctx, MarkParams{Handle: page.SelectionID, Action: "read", DryRun: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Mark(ctx, MarkParams{Handle: markDry.ReceiptID, Action: "flag"}); err == nil {
		t.Error("a receipt previewing 'read' applied as 'flag'")
	}
	if _, err := s.Mark(ctx, MarkParams{Handle: markDry.ReceiptID, Action: "read"}); err != nil {
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

	dry, err := s.Move(ctx, MoveParams{Handle: page.SelectionID, ToMailbox: "archive", DryRun: true})
	if err != nil {
		t.Fatal(err)
	}
	first, err := s.Move(ctx, MoveParams{Handle: dry.ReceiptID})
	if err != nil {
		t.Fatal(err)
	}
	if first.Replayed {
		t.Error("the first apply reported itself as a replay")
	}
	setsAfterFirst := countBatches(f.fake, "Email/set")

	second, err := s.Move(ctx, MoveParams{Handle: dry.ReceiptID})
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
	if third, _ := s.Move(ctx, MoveParams{Handle: dry.ReceiptID}); !third.Replayed {
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

	dry, err := s.Move(ctx, MoveParams{Handle: page.SelectionID, ToMailbox: "archive", DryRun: true})
	if err != nil {
		t.Fatal(err)
	}
	// Something else files two of them elsewhere between preview and apply.
	moved := []string{page.IDs[3], page.IDs[7]}
	for _, id := range moved {
		f.mailboxOf[id] = "mb-rec1"
	}

	applied, err := s.Move(ctx, MoveParams{Handle: dry.ReceiptID})
	if err != nil {
		t.Fatal(err)
	}
	if len(applied.Drifted) != 2 {
		t.Fatalf("drifted = %v, want the two messages that moved", applied.Drifted)
	}
	drifted := map[string]DriftedPlacement{}
	for _, d := range applied.Drifted {
		drifted[d.ID] = d
	}
	for _, id := range moved {
		d, ok := drifted[id]
		if !ok {
			t.Errorf("drifted list missing %s: %v", id, applied.Drifted)
			continue
		}
		// Both placements come back, so the caller can see what happened
		// without a second fetch — the comparison that found the drift
		// already had them.
		if len(d.Was) != 1 || d.Was[0].ID != "mb-inbox" {
			t.Errorf("%s was = %+v, want the inbox the dry run saw", id, d.Was)
		}
		if len(d.Now) != 1 || d.Now[0].ID != "mb-rec1" {
			t.Errorf("%s now = %+v, want where it actually is", id, d.Now)
		}
		if d.Now[0].Name == "" {
			t.Errorf("%s now = %+v, want the mailbox named as well as identified", id, d.Now)
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
	dry, err := s.Move(ctx, MoveParams{Handle: page.SelectionID, ToMailbox: "archive", DryRun: true})
	if err != nil {
		t.Fatal(err)
	}

	// Age both handles past the TTL without sleeping.
	s.handles.now = func() time.Time { return time.Now().Add(handleTTL + time.Minute) }

	_, err = s.Move(ctx, MoveParams{Handle: page.SelectionID, ToMailbox: "archive", DryRun: true})
	if err == nil || !strings.Contains(err.Error(), "expired") {
		t.Errorf("expired selection err = %v, want an expiry message", err)
	}
	_, err = s.Move(ctx, MoveParams{Handle: dry.ReceiptID})
	if err == nil || !strings.Contains(err.Error(), "expired") {
		t.Errorf("expired receipt err = %v, want an expiry message", err)
	}
	// An unknown handle is its own message, and neither says "bug".
	if _, err := s.Move(ctx, MoveParams{Handle: "sel_nope", ToMailbox: "archive", DryRun: true}); err == nil || !strings.Contains(err.Error(), "unknown selection") {
		t.Errorf("unknown selection err = %v", err)
	}
}

func TestHandleStoreIsBounded(t *testing.T) {
	s, _ := handleService(t, 2)
	for i := 0; i < maxSelections*2; i++ {
		searchSelection(t, s)
	}
	s.handles.mu.Lock()
	n := len(s.handles.selections)
	s.handles.mu.Unlock()
	if n > maxSelections {
		t.Errorf("store holds %d selections, cap is %d", n, maxSelections)
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
	if _, err := s.Move(ctx, MoveParams{IDs: page.IDs, Handle: page.SelectionID, ToMailbox: "archive"}); err == nil {
		t.Error("ids and selection accepted together")
	}
	// A receipt is an apply; previewing one makes no sense.
	dry, err := s.Move(ctx, MoveParams{Handle: page.SelectionID, ToMailbox: "archive", DryRun: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Move(ctx, MoveParams{Handle: dry.ReceiptID, DryRun: true}); err == nil {
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

	_, err := s.Move(context.Background(), MoveParams{Handle: page.SelectionID, ToMailbox: "archive", DryRun: true})
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

	dry, err := s.Mark(ctx, MarkParams{Handle: page.SelectionID, Action: "read", DryRun: true})
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

	applied, err := s.Mark(ctx, MarkParams{Handle: dry.ReceiptID, Action: "read"})
	if err != nil {
		t.Fatal(err)
	}
	if len(applied.Drifted) != 1 || applied.Drifted[0].ID != read {
		t.Fatalf("drifted = %+v, want just %s", applied.Drifted, read)
	}
	// The keyword and both states, so the drift is legible without refetching.
	if d := applied.Drifted[0]; d.Keyword != "$seen" || d.Was != "unset" || d.Now != "set" {
		t.Errorf("drifted = %+v, want $seen unset→set", d)
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

	dry, err := s.Trash(ctx, TrashParams{Handle: page.SelectionID, DryRun: true})
	if err != nil {
		t.Fatal(err)
	}
	if dry.ReceiptID == "" || dry.Destination.Role != "trash" {
		t.Fatalf("trash dry run = %+v", dry)
	}
	// Above the threshold and with no confirm phrase in sight: the receipt is
	// the authorization.
	applied, err := s.Trash(ctx, TrashParams{Handle: dry.ReceiptID})
	if err != nil {
		t.Fatal(err)
	}
	if applied.MovedCount != 25 {
		t.Errorf("movedCount = %d, want 25", applied.MovedCount)
	}
	if _, err := s.Move(ctx, MoveParams{Handle: dry.ReceiptID, ToMailbox: "archive"}); err == nil {
		t.Error("a trash receipt applied as a move")
	}
}

// A model that pads every declared property rather than omitting keys must
// still be able to use a handle. v0.13.0 shipped `ids` with minItems:1, so the
// empty array meaning "not this way" was schema-invalid and the model
// substituted ["placeholder"] — which read as a second selector and refused
// every selection-based call in a 2,000-message wave. These are the exact
// argument objects from that transcript.
func TestPaddedIDsDoNotDefeatAHandle(t *testing.T) {
	s, _ := handleService(t, 25)
	ctx := context.Background()
	page := searchSelection(t, s)

	// Every inert form the observed model reached for, plus the one it could
	// not express at the time.
	for _, padding := range [][]string{
		nil,
		{},
		{""},
		{"  "},
		{"", ""},
	} {
		dry, err := s.Move(ctx, MoveParams{
			IDs: padding, Handle: page.SelectionID, SelectionOffset: 0,
			ToMailbox: "archive", DryRun: true,
		})
		if err != nil {
			t.Fatalf("ids padded with %q refused the selection: %v", padding, err)
		}
		if dry.Selection == nil || dry.Selection.Count != 25 {
			t.Errorf("ids padded with %q resolved to %+v", padding, dry.Selection)
		}
		// And the receipt path takes the same padding.
		applied, err := s.Move(ctx, MoveParams{IDs: padding, Handle: dry.ReceiptID})
		if err != nil {
			t.Fatalf("ids padded with %q refused the receipt: %v", padding, err)
		}
		if applied.MovedCount != 25 {
			t.Errorf("movedCount = %d", applied.MovedCount)
		}
	}

	// Padded string selectors stay inert on the ids path, including whitespace.
	if _, err := s.Move(ctx, MoveParams{
		IDs: page.IDs[:2], Handle: " ", ToMailbox: "archive", DryRun: true,
	}); err != nil {
		t.Errorf("padded selection/receipt refused the ids path: %v", err)
	}
}

// The padding fix must not soften the actual ambiguity: two real selectors is
// still a refusal, because there is no way to know which was meant.
func TestTwoRealSelectorsStillRefused(t *testing.T) {
	s, _ := handleService(t, 25)
	ctx := context.Background()
	page := searchSelection(t, s)

	_, err := s.Move(ctx, MoveParams{
		IDs: []string{"placeholder"}, Handle: page.SelectionID, ToMailbox: "archive", DryRun: true,
	})
	if err == nil {
		t.Fatal("a non-empty ids alongside a selection was accepted")
	}
	// The refusal has to name the corrected calls, not just the rule — the
	// model that lands here is padding, and needs telling which inert value to
	// use. Without that it varies the placeholder and retries indefinitely.
	for _, want := range []string{"ids as []", page.SelectionID, "not this one"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal never mentions %q: %v", want, err)
		}
	}

	// Naming nothing at all is still its own, different error.
	_, err = s.Move(ctx, MoveParams{IDs: []string{}, Handle: "", ToMailbox: "archive"})
	if err == nil || !strings.Contains(err.Error(), "name the messages") {
		t.Errorf("naming nothing = %v, want the name-the-messages error", err)
	}
}

// Blank entries inside a real id list cannot name a message, so they are
// dropped rather than sent to the provider.
func TestBlankIDsAreDroppedFromARealList(t *testing.T) {
	s, f := handleService(t, 5)
	page := searchSelection(t, s)

	res, err := s.Move(context.Background(), MoveParams{
		IDs: []string{page.IDs[0], "", page.IDs[1], "   "}, ToMailbox: "archive", DryRun: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	sent := stringifyIDs(lastBatch(t, f.fake, "Email/get")[0])
	if len(sent) != 2 || sent[0] != page.IDs[0] || sent[1] != page.IDs[1] {
		t.Errorf("provider received %v, want the two real ids", sent)
	}
	if res.MovedCount != 2 {
		t.Errorf("movedCount = %d, want 2", res.MovedCount)
	}
}

// --- TW-045: the call states its own mode ---

// The point of merging selection and receipt into one `handle`: the prefix on
// the value says which protocol a call is using, so a reader does not classify
// a transcript by testing which of three fields is non-empty.
func TestHandlePrefixSelectsTheProtocol(t *testing.T) {
	s, _ := handleService(t, 10)
	ctx := context.Background()
	page := searchSelection(t, s)

	if !strings.HasPrefix(page.SelectionID, selectionPrefix) {
		t.Fatalf("selectionId %q does not carry the sel_ prefix the mode is read from", page.SelectionID)
	}
	dry, err := s.Move(ctx, MoveParams{Handle: page.SelectionID, ToMailbox: "archive", DryRun: true})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(dry.ReceiptID, receiptPrefix) {
		t.Fatalf("receiptId %q does not carry the rcp_ prefix", dry.ReceiptID)
	}
	// The same field, two protocols, told apart by the value alone.
	if _, err := s.Move(ctx, MoveParams{Handle: dry.ReceiptID}); err != nil {
		t.Fatalf("receipt handle refused: %v", err)
	}
}

// A handle that is neither kind must say what the two kinds look like: the
// caller has something in hand and needs to know which tool mints what.
func TestUnknownHandlePrefixIsExplained(t *testing.T) {
	s, _ := handleService(t, 5)
	_, err := s.Move(context.Background(), MoveParams{Handle: "xyz_deadbeef", ToMailbox: "archive", DryRun: true})
	if err == nil {
		t.Fatal("a handle with no recognised prefix was accepted")
	}
	for _, want := range []string{"sel_", "rcp_", "email_search", "dry run"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal %q does not mention %q", err, want)
		}
	}
}

// One selector or the other, and padding the unused one stays inert — the
// property TW-027 cost a 2,000-message wave to establish, which the merge must
// not quietly give back.
func TestHandleAndIDsRemainMutuallyExclusive(t *testing.T) {
	s, _ := handleService(t, 25)
	ctx := context.Background()
	page := searchSelection(t, s)

	// Padded ids alongside a real handle: inert, in every shape a model sends.
	for _, padding := range [][]string{nil, {}, {""}, {"   "}} {
		if _, err := s.Move(ctx, MoveParams{
			IDs: padding, Handle: page.SelectionID, SelectionOffset: 0,
			ToMailbox: "archive", DryRun: true,
		}); err != nil {
			t.Errorf("ids padded with %q refused the handle: %v", padding, err)
		}
	}
	// A padded handle alongside real ids: also inert.
	for _, h := range []string{"", "  "} {
		if _, err := s.Move(ctx, MoveParams{
			IDs: page.IDs[:2], Handle: h, ToMailbox: "archive", DryRun: true,
		}); err != nil {
			t.Errorf("handle padded with %q refused the ids path: %v", h, err)
		}
	}
	// Two real selectors still refuse, and the refusal quotes the caller's own
	// handle so a padding model is told which value to make inert.
	_, err := s.Move(ctx, MoveParams{IDs: []string{"e-real"}, Handle: page.SelectionID, ToMailbox: "archive"})
	if err == nil {
		t.Fatal("real ids alongside a real handle were accepted")
	}
	if !strings.Contains(err.Error(), page.SelectionID) || !strings.Contains(err.Error(), "[]") {
		t.Errorf("refusal %q must quote the handle and name the inert value", err)
	}
	// Naming nothing is still its own, different error.
	_, err = s.Move(ctx, MoveParams{IDs: []string{}, Handle: "", ToMailbox: "archive"})
	if err == nil || !strings.Contains(err.Error(), "name the messages") {
		t.Errorf("naming nothing = %v, want the name-the-messages error", err)
	}
}

// A receipt still cannot be used to preview, and a selection still cannot skip
// the preview — the merge changes how a set is named, not what each kind means.
func TestHandleKindsKeepTheirSemantics(t *testing.T) {
	s, _ := handleService(t, 25)
	ctx := context.Background()
	page := searchSelection(t, s)
	dry, err := s.Move(ctx, MoveParams{Handle: page.SelectionID, ToMailbox: "archive", DryRun: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Move(ctx, MoveParams{Handle: dry.ReceiptID, DryRun: true}); err == nil {
		t.Error("a receipt was accepted as a preview target")
	}
	// A selection above the bulk threshold still needs the phrase.
	if _, err := s.Move(ctx, MoveParams{Handle: page.SelectionID, ToMailbox: "archive"}); err == nil {
		t.Error("a bulk selection applied without a confirm phrase")
	}
	// A move receipt is still refused by mark.
	if _, err := s.Mark(ctx, MarkParams{Handle: dry.ReceiptID, Action: "read"}); err == nil {
		t.Error("a move receipt was accepted by email_mark")
	}
}

// toMailbox stays required for a selection and optional for a receipt, now
// decided by the prefix rather than by a separate field being non-empty.
func TestToMailboxRequirementFollowsTheHandleKind(t *testing.T) {
	s, _ := handleService(t, 5)
	ctx := context.Background()
	page := searchSelection(t, s)

	if _, err := s.Move(ctx, MoveParams{Handle: page.SelectionID, DryRun: true}); err == nil {
		t.Error("a selection was accepted with no destination")
	}
	dry, err := s.Move(ctx, MoveParams{Handle: page.SelectionID, ToMailbox: "archive", DryRun: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Move(ctx, MoveParams{Handle: dry.ReceiptID}); err != nil {
		t.Errorf("a receipt was asked for a destination it already carries: %v", err)
	}
}

// Raising the handle cap made a mutating call able to fail in the middle for
// the first time: at 200 messages a failure was the whole call, and at 2,000 it
// can be the fourth chunk of ten. Trading 69 recoverable turns for one
// unrecoverable one would be a bad bargain, so a stopped run has to say where
// it stopped and hand back what is left.
func TestBulkMoveReportsWhereItStopped(t *testing.T) {
	s, f := handleService(t, maxHandleSetIDs)
	handle := seedBigSelection(s, f.cohort)
	ctx := context.Background()

	chunk := mutationChunk(testSession())
	// The third chunk's request never comes back.
	lost := &lossyCaller{fake: f.fake, failSetAt: 3}
	f.fake.session = testSession()
	s = NewService(lost, config.Normalize(config.Settings{
		APIToken: "tok", AccessLevel: config.AccessOrganize,
	}))
	handle = seedBigSelection(s, f.cohort)

	res, err := s.Move(ctx, MoveParams{Handle: handle, ToMailbox: "archive",
		Confirm: movePhrase(len(f.cohort), "Archive", "A1", f.cohort)})
	if err != nil {
		t.Fatalf("a partial run reported no result at all: %v", err)
	}
	if !res.Aborted {
		t.Fatal("a run that lost a chunk reported success")
	}
	// Everything before the failed chunk landed, and it is reported as a
	// boundary rather than an estimate.
	want := chunk * 2
	if res.AppliedTo != want || res.MovedCount != want {
		t.Fatalf("appliedTo=%d movedCount=%d, want both %d", res.AppliedTo, res.MovedCount, want)
	}
	if res.RemainingCount != len(f.cohort)-want {
		t.Errorf("remainingCount = %d, want %d", res.RemainingCount, len(f.cohort)-want)
	}
	if res.AbortReason == "" || res.AbortNote == "" {
		t.Errorf("stop reported without a reason or a remedy: %+v", res.partialRun)
	}

	// The tail comes back as a handle, so finishing is the same two calls as
	// any other batch — with no ids to recover from a result that failed.
	if res.RemainingSelectionID == "" {
		t.Fatal("no handle named the untouched messages")
	}
	lost.failSetAt = 0
	dry, err := s.Move(ctx, MoveParams{Handle: res.RemainingSelectionID, ToMailbox: "archive", DryRun: true})
	if err != nil {
		t.Fatalf("the remainder handle is unusable: %v", err)
	}
	if dry.Selection.Count != len(f.cohort)-want {
		t.Errorf("remainder names %d messages, want %d", dry.Selection.Count, len(f.cohort)-want)
	}
	finish, err := s.Move(ctx, MoveParams{Handle: dry.ReceiptID})
	if err != nil {
		t.Fatal(err)
	}
	if finish.Aborted {
		t.Errorf("finishing the remainder aborted: %+v", finish.partialRun)
	}
	if res.MovedCount+finish.MovedCount != len(f.cohort) {
		t.Errorf("the two runs covered %d of %d messages",
			res.MovedCount+finish.MovedCount, len(f.cohort))
	}
}

// A failure in the FIRST chunk has no partial outcome to describe, and
// dressing it up as one would report a successful move of nothing.
func TestBulkMoveFailingImmediatelyIsAnError(t *testing.T) {
	_, f := handleService(t, maxHandleSetIDs)
	f.fake.session = testSession()
	s := NewService(&lossyCaller{fake: f.fake, failSetAt: 1},
		config.Normalize(config.Settings{APIToken: "tok", AccessLevel: config.AccessOrganize}))
	handle := seedBigSelection(s, f.cohort)

	_, err := s.Move(context.Background(), MoveParams{Handle: handle, ToMailbox: "archive",
		Confirm: movePhrase(len(f.cohort), "Archive", "A1", f.cohort)})
	if err == nil {
		t.Fatal("a run that did nothing returned a result instead of an error")
	}
}
