package mail

// Unit tests for the organize operations: request construction (patch shapes,
// batching) and the safety gates, against the scripted fake Caller.

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"testing"

	"terva-ext-jmap-mail/internal/jmap"
)

// organizeFake answers Mailbox/get plus the Email/get(+Email/set) batches.
func organizeFake() *fake {
	f := &fake{}
	f.handler = func(calls []jmap.Invocation) *jmap.Response {
		if calls[0].Name == "Mailbox/get" {
			return response(result("Mailbox/get", calls[0].CallID, map[string]any{"list": mailboxFixture}))
		}
		var results []jmap.InvocationResult
		for _, c := range calls {
			switch c.Name {
			case "Email/get":
				results = append(results, result("Email/get", c.CallID, map[string]any{
					"list": []any{
						map[string]any{"id": "e1", "subject": "One", "keywords": map[string]bool{"$seen": true}, "mailboxIds": map[string]bool{"mb-inbox": true}},
						map[string]any{"id": "e2", "subject": "Two", "keywords": map[string]bool{}, "mailboxIds": map[string]bool{"mb-inbox": true, "mb-rec1": true}},
					},
					"notFound": []string{"e-gone"},
				}))
			case "Email/set":
				update, _ := c.Args.(map[string]any)["update"].(map[string]any)
				updated := map[string]any{}
				for id := range update {
					updated[id] = nil
				}
				delete(updated, "e-gone")
				results = append(results, result("Email/set", c.CallID, map[string]any{
					"updated": updated, "notUpdated": map[string]any{"e-gone": map[string]any{"type": "notFound"}},
					"oldState": "s1", "newState": "s2",
				}))
			}
		}
		return response(results...)
	}
	return f
}

func TestMarkPatchConstruction(t *testing.T) {
	f := organizeFake()
	s := testService(f)
	res, err := s.Mark(context.Background(), MarkParams{IDs: []string{"e1", "e2", "e-gone"}, Action: "read"})
	if err != nil {
		t.Fatal(err)
	}

	batch := findBatch(t, f, "Email/get")
	if len(batch) != 2 || batch[1].Name != "Email/set" {
		t.Fatalf("batch = %v", batch)
	}
	update := argsOf(t, batch[1])["update"].(map[string]any)
	patch := update["e1"].(map[string]any)
	if v, ok := patch["keywords/$seen"]; !ok || v != true {
		t.Errorf("read patch = %v, want keywords/$seen: true", patch)
	}

	// e1 already read → alreadySet; e2 unread → changed; e-gone → notFound.
	if len(res.Changed) != 1 || res.Changed[0].ID != "e2" {
		t.Errorf("changed = %+v", res.Changed)
	}
	if len(res.AlreadySet) != 1 || res.AlreadySet[0].ID != "e1" {
		t.Errorf("alreadySet = %+v", res.AlreadySet)
	}
	if len(res.NotFound) != 1 || res.NotFound[0] != "e-gone" {
		t.Errorf("notFound = %v", res.NotFound)
	}
}

func TestMarkUnsetUsesNull(t *testing.T) {
	f := organizeFake()
	s := testService(f)
	if _, err := s.Mark(context.Background(), MarkParams{IDs: []string{"e1"}, Action: "unflag"}); err != nil {
		t.Fatal(err)
	}
	batch := findBatch(t, f, "Email/get")
	patchJSON := stringify(argsOf(t, batch[1])["update"])
	if !strings.Contains(patchJSON, `"keywords/$flagged":null`) {
		t.Errorf("unflag patch = %s, want keywords/$flagged: null", patchJSON)
	}
}

func TestMarkDryRunSendsNoSet(t *testing.T) {
	f := organizeFake()
	s := testService(f)
	res, err := s.Mark(context.Background(), MarkParams{IDs: []string{"e1", "e2"}, Action: "read", DryRun: true})
	if err != nil {
		t.Fatal(err)
	}
	if !res.DryRun || len(res.Changed) != 1 {
		t.Errorf("res = %+v", res)
	}
	for _, calls := range f.recorded {
		for _, c := range calls {
			if c.Name == "Email/set" {
				t.Fatal("dry run must not send Email/set")
			}
		}
	}
}

func TestMarkValidation(t *testing.T) {
	s := testService(organizeFake())
	if _, err := s.Mark(context.Background(), MarkParams{IDs: []string{"e1"}, Action: "star"}); err == nil {
		t.Error("want error for bad action")
	}
	if _, err := s.Mark(context.Background(), MarkParams{Action: "read"}); err == nil {
		t.Error("want error for empty ids")
	}
	big := make([]string, maxSetIDs+1)
	for i := range big {
		big[i] = "e"
	}
	if _, err := s.Mark(context.Background(), MarkParams{IDs: big, Action: "read"}); err == nil {
		t.Error("want error above maxSetIDs")
	}
}

func TestMoveReplaceVsKeep(t *testing.T) {
	f := organizeFake()
	s := testService(f)
	res, err := s.Move(context.Background(), MoveParams{IDs: []string{"e1", "e2"}, ToMailbox: "archive"})
	if err != nil {
		t.Fatal(err)
	}
	batch := findBatch(t, f, "Email/get")
	update := stringify(argsOf(t, batch[1])["update"])
	if !strings.Contains(update, `"mailboxIds":{"mb-arch":true}`) {
		t.Errorf("move must replace mailboxIds: %s", update)
	}
	if res.Destination.ID != "mb-arch" || res.Destination.Role != "archive" {
		t.Errorf("destination = %+v", res.Destination)
	}
	if len(res.Moved) != 2 || len(res.Moved[0].From) == 0 {
		t.Errorf("moved = %+v", res.Moved)
	}

	f2 := organizeFake()
	s2 := testService(f2)
	if _, err := s2.Move(context.Background(), MoveParams{IDs: []string{"e1"}, ToMailbox: "archive", KeepInMailboxes: true}); err != nil {
		t.Fatal(err)
	}
	update2 := stringify(argsOf(t, findBatch(t, f2, "Email/get")[1])["update"])
	if !strings.Contains(update2, `"mailboxIds/mb-arch":true`) {
		t.Errorf("keep must patch additively: %s", update2)
	}
}

func TestMoveAmbiguousDestination(t *testing.T) {
	s := testService(organizeFake())
	_, err := s.Move(context.Background(), MoveParams{IDs: []string{"e1"}, ToMailbox: "Receipts"})
	if err == nil || !strings.Contains(err.Error(), "ambiguous") {
		t.Fatalf("err = %v", err)
	}
}

// --- result verbosity (bulk-organization payloads) ---

// bulkFake answers with one message per requested id, every fifth also filed
// under the nested Receipts folder so the movedFrom breakdown has something to
// disambiguate.
func bulkFake() *fake {
	f := &fake{}
	f.handler = func(calls []jmap.Invocation) *jmap.Response {
		if calls[0].Name == "Mailbox/get" {
			return response(result("Mailbox/get", calls[0].CallID, map[string]any{"list": mailboxFixture}))
		}
		ids, _ := argsOfAny(calls[0])["ids"].([]string)
		var results []jmap.InvocationResult
		list := make([]any, 0, len(ids))
		for i, id := range ids {
			boxes := map[string]bool{"mb-inbox": true}
			if i%5 == 0 {
				boxes["mb-rec2"] = true
			}
			list = append(list, map[string]any{
				"id": id, "subject": fmt.Sprintf("Bulk %d", i),
				"keywords": map[string]bool{}, "mailboxIds": boxes,
			})
		}
		results = append(results, result("Email/get", calls[0].CallID, map[string]any{"list": list}))
		for _, c := range calls[1:] {
			update, _ := c.Args.(map[string]any)["update"].(map[string]any)
			updated := map[string]any{}
			for id := range update {
				updated[id] = nil
			}
			results = append(results, result("Email/set", c.CallID, map[string]any{
				"updated": updated, "oldState": "s1", "newState": "s2",
			}))
		}
		return response(results...)
	}
	return f
}

func bulkIDs(n int) []string {
	ids := make([]string, n)
	for i := range ids {
		ids[i] = fmt.Sprintf("e-%d", i)
	}
	return ids
}

// Above the bulk threshold the enumeration is dropped by default: the counts
// are what the caller is deciding on, and the list is what forces a context
// compaction mid-wave. The confirm phrase must survive the abridgement — it is
// the whole point of the dry run.
func TestMoveCountsOnlyAboveThreshold(t *testing.T) {
	ids := bulkIDs(bulkConfirmThreshold + 1)
	s := testService(bulkFake())
	res, err := s.Move(context.Background(), MoveParams{IDs: ids, ToMailbox: "archive", DryRun: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Moved) != 0 {
		t.Errorf("moved enumerated %d messages, want counts only", len(res.Moved))
	}
	if res.MovedCount != len(ids) {
		t.Errorf("movedCount = %d, want %d", res.MovedCount, len(ids))
	}
	// The source breakdown is the whole of the origin information on a bulk
	// run, since Moved (and its per-message From refs) is omitted here.
	if got := sourceCount(res, "mb-inbox"); got != len(ids) {
		t.Errorf("sources = %+v, want %d from mb-inbox", res.Sources, len(ids))
	}
	if got := sourceCount(res, "mb-rec2"); got != 5 {
		t.Errorf("sources = %+v, want 5 from the nested Receipts (mb-rec2)", res.Sources)
	}
	if want := movePhrase(len(ids), "Archive", "A1", ids); res.ConfirmPhrase != want {
		t.Errorf("confirmPhrase = %q — the counts form must still hand back the phrase", res.ConfirmPhrase)
	}
	// The abridged form must not be mistakable for the full one.
	if payload := stringify(res); strings.Contains(payload, "Bulk 0") {
		t.Errorf("subjects leaked into the counts form: %s", payload)
	}
}

func TestOrganizeVerboseOverrides(t *testing.T) {
	yes, no := true, false
	s := testService(bulkFake())
	ctx := context.Background()

	bulk := bulkIDs(bulkConfirmThreshold + 1)
	res, err := s.Move(ctx, MoveParams{IDs: bulk, ToMailbox: "archive", DryRun: true, Verbose: &yes})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Moved) != len(bulk) || res.MovedCount != len(bulk) {
		t.Errorf("verbose:true at %d ids: moved = %d, want the full list", len(bulk), len(res.Moved))
	}

	small := bulkIDs(3)
	res, err = s.Move(ctx, MoveParams{IDs: small, ToMailbox: "archive", Verbose: &no})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Moved) != 0 || res.MovedCount != 3 {
		t.Errorf("verbose:false at 3 ids: moved = %d, movedCount = %d", len(res.Moved), res.MovedCount)
	}

	// Default at or below the threshold stays the full enumeration.
	res, err = s.Move(ctx, MoveParams{IDs: small, ToMailbox: "archive"})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Moved) != 3 {
		t.Errorf("small default: moved = %d, want the full list", len(res.Moved))
	}
}

func TestMarkCountsOnlyAboveThreshold(t *testing.T) {
	ids := bulkIDs(bulkConfirmThreshold + 1)
	s := testService(bulkFake())
	res, err := s.Mark(context.Background(), MarkParams{IDs: ids, Action: "read", DryRun: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Changed) != 0 || len(res.AlreadySet) != 0 {
		t.Errorf("changed/alreadySet enumerated, want counts only: %+v", res)
	}
	if res.ChangedCount != len(ids) || res.AlreadySetCount != 0 {
		t.Errorf("counts = %d changed / %d alreadySet, want %d / 0", res.ChangedCount, res.AlreadySetCount, len(ids))
	}
	if want := markPhrase(len(ids), "read", "A1", ids); res.ConfirmPhrase != want {
		t.Errorf("confirmPhrase = %q, want %q", res.ConfirmPhrase, want)
	}
}

// Failures and vanished ids are the actionable exceptions — they are never
// abridged, however large the run.
func TestBulkCountsKeepFailuresInFull(t *testing.T) {
	ids := bulkIDs(bulkConfirmThreshold + 1)
	f := &fake{}
	f.handler = func(calls []jmap.Invocation) *jmap.Response {
		if calls[0].Name == "Mailbox/get" {
			return response(result("Mailbox/get", calls[0].CallID, map[string]any{"list": mailboxFixture}))
		}
		return response(
			result("Email/get", calls[0].CallID, map[string]any{
				"list":     []any{map[string]any{"id": "e-0", "subject": "One", "mailboxIds": map[string]bool{"mb-inbox": true}}},
				"notFound": []string{"e-1"},
			}),
			result("Email/set", calls[1].CallID, map[string]any{
				"updated":    map[string]any{"e-0": nil},
				"notUpdated": map[string]any{"e-2": map[string]any{"type": "forbidden", "description": "read-only mailbox"}},
				"oldState":   "s1", "newState": "s2",
			}),
		)
	}
	s := testService(f)
	res, err := s.Move(context.Background(), MoveParams{
		IDs: ids, ToMailbox: "archive", Confirm: movePhrase(len(ids), "Archive", "A1", ids),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Moved) != 0 || res.MovedCount != 1 {
		t.Errorf("moved = %v, movedCount = %d", res.Moved, res.MovedCount)
	}
	// The flat lists give way to grouped handles above the enumerate threshold:
	// at 2,000 messages a run can fail 500 of them, and 500 map entries is the
	// payload this extension exists to keep out. What must NOT be lost is the
	// information — every failure is still reported, by cause, with a handle
	// naming exactly the affected messages.
	if len(res.Failed) != 0 || len(res.NotFound) != 0 {
		t.Errorf("a bulk run still shipped flat id lists: failed = %v, notFound = %v", res.Failed, res.NotFound)
	}
	byType := map[string]FailureGroup{}
	for _, g := range res.Failures {
		byType[g.Type] = g
	}
	if len(byType) != 2 {
		t.Fatalf("failures = %+v, want one group per cause", res.Failures)
	}
	forbidden, ok := byType["forbidden"]
	if !ok || forbidden.Count != 1 || forbidden.SelectionID == "" {
		t.Errorf("forbidden group = %+v, want a count and a handle", forbidden)
	}
	if forbidden.Reason == "" {
		t.Error("the group carries no description; the type alone is opaque")
	}
	notFound, ok := byType["notFound"]
	if !ok || notFound.Count != 1 || notFound.SelectionID == "" {
		t.Errorf("notFound group = %+v", notFound)
	}
	// The two causes get separate handles, because retrying a mixed set would
	// retry the notFound ones — the one group a retry can never help.
	if forbidden.SelectionID == notFound.SelectionID {
		t.Error("both causes share one handle; retrying it would retry the unretryable")
	}
	if !strings.Contains(res.FailureNote, "email_get") {
		t.Errorf("nothing says what the handles are for: %q", res.FailureNote)
	}
	// And the handle really names the failed message.
	held, err := s.handles.getSelection(forbidden.SelectionID)
	if err != nil {
		t.Fatal(err)
	}
	if len(held.IDs) != 1 || held.IDs[0] != "e-2" {
		t.Errorf("forbidden handle names %v, want [e-2]", held.IDs)
	}
}

func TestBulkConfirmGate(t *testing.T) {
	ids := make([]string, bulkConfirmThreshold+1)
	for i := range ids {
		ids[i] = "e1"
	}

	s := testService(organizeFake())
	// No confirm → refused, and the refusal names the exact phrase.
	_, err := s.Move(context.Background(), MoveParams{IDs: ids, ToMailbox: "archive"})
	if err == nil || !strings.Contains(err.Error(), strconv.Quote(movePhrase(len(ids), "Archive", "A1", ids))) {
		t.Fatalf("err = %v, want refusal with exact phrase", err)
	}
	// Wrong phrase → refused.
	if _, err := s.Move(context.Background(), MoveParams{IDs: ids, ToMailbox: "archive", Confirm: "yes please"}); err == nil {
		t.Error("wrong confirm accepted")
	}
	// Dry run → allowed without confirm.
	if _, err := s.Move(context.Background(), MoveParams{IDs: ids, ToMailbox: "archive", DryRun: true}); err != nil {
		t.Errorf("dry run refused: %v", err)
	}
	// Exact phrase (case-insensitive) → allowed.
	if _, err := s.Move(context.Background(), MoveParams{IDs: ids, ToMailbox: "archive", Confirm: strings.ToUpper(movePhrase(len(ids), "Archive", "A1", ids))}); err != nil {
		t.Errorf("correct confirm refused: %v", err)
	}
	// A stale phrase for a different destination or account must not carry.
	if _, err := s.Move(context.Background(), MoveParams{IDs: ids, ToMailbox: "archive", Confirm: movePhrase(len(ids), "Archive/Receipts", "A1", ids)}); err == nil {
		t.Error("phrase minted for another destination accepted")
	}
	if _, err := s.Move(context.Background(), MoveParams{IDs: ids, ToMailbox: "archive", Confirm: movePhrase(len(ids), "Archive", "A2", ids)}); err == nil {
		t.Error("phrase minted for another account accepted")
	}

	// Trash uses its own phrase.
	_, err = s.Trash(context.Background(), TrashParams{IDs: ids})
	if err == nil || !strings.Contains(err.Error(), strconv.Quote(trashPhrase(len(ids), "A1", ids))) {
		t.Fatalf("err = %v, want trash phrase", err)
	}
	if _, err := s.Trash(context.Background(), TrashParams{IDs: ids, Confirm: trashPhrase(len(ids), "A1", ids)}); err != nil {
		t.Errorf("correct trash confirm refused: %v", err)
	}
}

func TestTrashTargetsTrashRole(t *testing.T) {
	f := organizeFake()
	s := testService(f)
	res, err := s.Trash(context.Background(), TrashParams{IDs: []string{"e2"}})
	if err != nil {
		t.Fatal(err)
	}
	if res.Destination.Role != "trash" {
		t.Errorf("destination = %+v", res.Destination)
	}
	update := stringify(argsOf(t, findBatch(t, f, "Email/get")[1])["update"])
	if !strings.Contains(update, `"mailboxIds":{"mb-trash":true}`) {
		t.Errorf("trash must replace mailboxIds with trash only: %s", update)
	}
}

func TestMoveFailuresSurface(t *testing.T) {
	f := &fake{}
	f.handler = func(calls []jmap.Invocation) *jmap.Response {
		if calls[0].Name == "Mailbox/get" {
			return response(result("Mailbox/get", calls[0].CallID, map[string]any{"list": mailboxFixture}))
		}
		return response(
			result("Email/get", calls[0].CallID, map[string]any{
				"list": []any{map[string]any{"id": "e1", "subject": "One", "mailboxIds": map[string]bool{"mb-inbox": true}}},
			}),
			result("Email/set", calls[1].CallID, map[string]any{
				"updated":    map[string]any{},
				"notUpdated": map[string]any{"e1": map[string]any{"type": "forbidden", "description": "read-only share"}},
			}),
		)
	}
	s := testService(f)
	res, err := s.Move(context.Background(), MoveParams{IDs: []string{"e1"}, ToMailbox: "archive"})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Moved) != 0 {
		t.Errorf("moved = %+v, want none", res.Moved)
	}
	if res.Failed["e1"] != "forbidden: read-only share" {
		t.Errorf("failed = %v", res.Failed)
	}
}

// Mark is the mildest mutation but still information-destroying in bulk
// (which messages were unread is unrecoverable) — it gates like the rest.
func TestMarkBulkConfirmGate(t *testing.T) {
	ids := make([]string, bulkConfirmThreshold+1)
	for i := range ids {
		ids[i] = "e1"
	}
	f := organizeFake()
	s := testService(f)
	_, err := s.Mark(context.Background(), MarkParams{IDs: ids, Action: "read"})
	if err == nil || !strings.Contains(err.Error(), strconv.Quote(markPhrase(len(ids), "read", "A1", ids))) {
		t.Fatalf("err = %v, want refusal with exact phrase", err)
	}
	if len(f.recorded) != 0 {
		t.Errorf("refusal made %d network calls, want 0", len(f.recorded))
	}
	res, err := s.Mark(context.Background(), MarkParams{IDs: ids, Action: "read", DryRun: true})
	if err != nil {
		t.Fatal(err)
	}
	if want := markPhrase(len(ids), "read", "A1", ids); res.ConfirmPhrase != want {
		t.Errorf("dry-run confirmPhrase = %q, want %q", res.ConfirmPhrase, want)
	}
	if _, err := s.Mark(context.Background(), MarkParams{IDs: ids, Action: "read", Confirm: res.ConfirmPhrase}); err != nil {
		t.Errorf("correct confirm refused: %v", err)
	}
	// Small batches stay friction-free.
	if _, err := s.Mark(context.Background(), MarkParams{IDs: []string{"e1"}, Action: "read"}); err != nil {
		t.Errorf("single mark should need no confirm: %v", err)
	}
}
