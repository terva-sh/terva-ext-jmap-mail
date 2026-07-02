package mail

// Unit tests for the organize operations: request construction (patch shapes,
// batching) and the safety gates, against the scripted fake Caller.

import (
	"context"
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

func TestBulkConfirmGate(t *testing.T) {
	ids := make([]string, bulkConfirmThreshold+1)
	for i := range ids {
		ids[i] = "e1"
	}

	s := testService(organizeFake())
	// No confirm → refused, and the refusal names the exact phrase.
	_, err := s.Move(context.Background(), MoveParams{IDs: ids, ToMailbox: "archive"})
	if err == nil || !strings.Contains(err.Error(), `"move 21 emails to Archive in account A1"`) {
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
	if _, err := s.Move(context.Background(), MoveParams{IDs: ids, ToMailbox: "archive", Confirm: "Move 21 Emails To Archive In Account A1"}); err != nil {
		t.Errorf("correct confirm refused: %v", err)
	}
	// A stale phrase for a different destination or account must not carry.
	if _, err := s.Move(context.Background(), MoveParams{IDs: ids, ToMailbox: "archive", Confirm: "move 21 emails to Receipts in account A1"}); err == nil {
		t.Error("phrase minted for another destination accepted")
	}
	if _, err := s.Move(context.Background(), MoveParams{IDs: ids, ToMailbox: "archive", Confirm: "move 21 emails to Archive in account A2"}); err == nil {
		t.Error("phrase minted for another account accepted")
	}

	// Trash uses its own phrase.
	_, err = s.Trash(context.Background(), TrashParams{IDs: ids})
	if err == nil || !strings.Contains(err.Error(), `"trash 21 emails in account A1"`) {
		t.Fatalf("err = %v, want trash phrase", err)
	}
	if _, err := s.Trash(context.Background(), TrashParams{IDs: ids, Confirm: "trash 21 emails in account A1"}); err != nil {
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
	if err == nil || !strings.Contains(err.Error(), `"mark 21 emails read in account A1"`) {
		t.Fatalf("err = %v, want refusal with exact phrase", err)
	}
	if len(f.recorded) != 0 {
		t.Errorf("refusal made %d network calls, want 0", len(f.recorded))
	}
	res, err := s.Mark(context.Background(), MarkParams{IDs: ids, Action: "read", DryRun: true})
	if err != nil {
		t.Fatal(err)
	}
	if res.ConfirmPhrase != "mark 21 emails read in account A1" {
		t.Errorf("dry-run confirmPhrase = %q", res.ConfirmPhrase)
	}
	if _, err := s.Mark(context.Background(), MarkParams{IDs: ids, Action: "read", Confirm: res.ConfirmPhrase}); err != nil {
		t.Errorf("correct confirm refused: %v", err)
	}
	// Small batches stay friction-free.
	if _, err := s.Mark(context.Background(), MarkParams{IDs: []string{"e1"}, Action: "read"}); err != nil {
		t.Errorf("single mark should need no confirm: %v", err)
	}
}
