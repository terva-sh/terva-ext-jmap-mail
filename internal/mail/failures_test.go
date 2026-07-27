package mail

// The failure round trip: a bulk run that could not change everything hands
// back handles, and those handles are usable by the tools that answer the two
// questions a caller actually has — "which ones?" and "try again".
//
// This is the pair of changes that makes a failure list unnecessary. Before
// them a 2,000-message run reported up to 2,000 ids in a map, and a caller who
// wanted to see what failed had to send them all back.

import (
	"context"
	"strings"
	"testing"

	"terva-ext-jmap-mail/internal/jmap"
)

// failFake fails a named subset of every Email/set, so a bulk run produces
// real, mixed-cause failures.
func failFake(n int, forbidden, gone map[string]bool) *handleFake {
	h := newHandleFake(n)
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
			var notFound []string
			for _, id := range ids {
				if gone[id] {
					notFound = append(notFound, id)
					continue
				}
				list = append(list, map[string]any{
					"id": id, "subject": "Subject " + id,
					"mailboxIds": map[string]bool{"mb-inbox": true},
					"keywords":   map[string]bool{},
				})
			}
			out := []jmap.InvocationResult{result("Email/get", calls[0].CallID, map[string]any{
				"list": list, "notFound": notFound,
			})}
			if len(calls) > 1 {
				updated := map[string]any{}
				notUpdated := map[string]any{}
				for _, id := range ids {
					switch {
					case gone[id]:
						// already reported by the get
					case forbidden[id]:
						notUpdated[id] = map[string]any{
							"type": "forbidden", "description": "read-only mailbox",
						}
					default:
						updated[id] = nil
					}
				}
				out = append(out, result("Email/set", calls[1].CallID, map[string]any{
					"updated": updated, "notUpdated": notUpdated,
					"oldState": "s1", "newState": "s2",
				}))
			}
			return response(out...)
		}
		panic("unexpected call " + calls[0].Name)
	}
	return h
}

// The end-to-end claim: move a bulk set, some fail, and the caller can both
// SEE and RETRY exactly those without an id crossing the boundary in either
// direction.
func TestFailuresComeBackAsUsableHandles(t *testing.T) {
	forbidden := map[string]bool{}
	gone := map[string]bool{}
	f := failFake(60, forbidden, gone)
	for i, id := range f.cohort {
		switch {
		case i%10 == 0:
			forbidden[id] = true
		case i%17 == 0:
			gone[id] = true
		}
	}
	s := testService(f.fake)
	ctx := context.Background()
	handle := seedBigSelection(s, f.cohort)

	res, err := s.Move(ctx, MoveParams{Handle: handle, ToMailbox: "archive",
		Confirm: movePhrase(len(f.cohort), "Archive", "A1", f.cohort)})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Failures) != 2 {
		t.Fatalf("failures = %+v, want a group per cause", res.Failures)
	}
	// The flat lists are gone; the groups carry everything they did.
	if len(res.Failed) != 0 || len(res.NotFound) != 0 {
		t.Errorf("bulk run shipped flat id lists alongside the groups")
	}

	var forbiddenGroup FailureGroup
	for _, g := range res.Failures {
		if g.Type == "forbidden" {
			forbiddenGroup = g
		}
		if g.SelectionID == "" {
			t.Errorf("group %q has no handle, so nothing can be done with it", g.Type)
		}
		if len(g.IDs) != 0 {
			t.Errorf("group %q listed ids on a bulk run: %v", g.Type, g.IDs)
		}
	}
	if forbiddenGroup.Count != len(forbidden) {
		t.Fatalf("forbidden count = %d, want %d", forbiddenGroup.Count, len(forbidden))
	}

	// Question one: which ones? email_get takes the handle and answers with
	// subjects, which is what a human needs — an id answers nothing.
	seen, err := s.Get(ctx, GetParams{Handle: forbiddenGroup.SelectionID, Fields: []string{"id", "subject"}})
	if err != nil {
		t.Fatalf("the failure handle is not readable: %v", err)
	}
	if len(seen.Emails) != forbiddenGroup.Count {
		t.Errorf("read %d of %d failed messages", len(seen.Emails), forbiddenGroup.Count)
	}
	for _, e := range seen.Emails {
		if !forbidden[e.ID] {
			t.Errorf("the failure handle named %s, which did not fail", e.ID)
		}
	}

	// Question two: try again. The handle goes straight back into the tool
	// that produced it, naming only the failures.
	dry, err := s.Move(ctx, MoveParams{Handle: forbiddenGroup.SelectionID, ToMailbox: "archive", DryRun: true})
	if err != nil {
		t.Fatalf("the failure handle is not retryable: %v", err)
	}
	if dry.Selection == nil || dry.Selection.Count != forbiddenGroup.Count {
		t.Errorf("retry covers %+v, want exactly the failed set", dry.Selection)
	}
}

// A small run keeps its ids: the list IS the answer at that size, and a handle
// would be ceremony around three strings.
func TestSmallRunsListFailureIDsInsteadOfMintingHandles(t *testing.T) {
	forbidden := map[string]bool{}
	f := failFake(5, forbidden, map[string]bool{})
	forbidden[f.cohort[0]] = true
	s := testService(f.fake)

	before := len(s.handles.selections)
	res, err := s.Move(context.Background(), MoveParams{
		IDs: f.cohort, ToMailbox: "archive",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Failures) != 1 {
		t.Fatalf("failures = %+v", res.Failures)
	}
	g := res.Failures[0]
	if g.SelectionID != "" {
		t.Errorf("a 5-message run minted a handle for one failure: %+v", g)
	}
	if len(g.IDs) != 1 || g.IDs[0] != f.cohort[0] {
		t.Errorf("ids = %v, want the one that failed", g.IDs)
	}
	// And the flat map is still there at this size, so nothing a caller relied
	// on below the threshold changed.
	if res.Failed[f.cohort[0]] == "" {
		t.Errorf("small run dropped the flat failed map: %v", res.Failed)
	}
	if len(s.handles.selections) != before {
		t.Errorf("minted %d handles for a run that needed none", len(s.handles.selections)-before)
	}
	if !strings.Contains(res.FailureNote, "notFound") {
		t.Errorf("note does not warn that one cause is unretryable: %q", res.FailureNote)
	}
}

// Causes get separate handles because the remedies differ, and one of them is
// "there is no remedy". Retrying a mixed handle would re-attempt the messages
// that are gone, every time.
func TestFailureCausesGetSeparateHandles(t *testing.T) {
	forbidden := map[string]bool{}
	gone := map[string]bool{}
	f := failFake(60, forbidden, gone)
	forbidden[f.cohort[1]] = true
	gone[f.cohort[2]] = true
	s := testService(f.fake)
	handle := seedBigSelection(s, f.cohort)

	res, err := s.Move(context.Background(), MoveParams{Handle: handle, ToMailbox: "archive",
		Confirm: movePhrase(len(f.cohort), "Archive", "A1", f.cohort)})
	if err != nil {
		t.Fatal(err)
	}
	byType := map[string]FailureGroup{}
	for _, g := range res.Failures {
		byType[g.Type] = g
	}
	if len(byType) != 2 {
		t.Fatalf("failures = %+v, want forbidden and notFound apart", res.Failures)
	}
	if byType["forbidden"].SelectionID == byType["notFound"].SelectionID {
		t.Fatal("one handle for both causes — retrying it re-attempts messages that are gone")
	}
	held, err := s.handles.getSelection(byType["notFound"].SelectionID)
	if err != nil {
		t.Fatal(err)
	}
	if len(held.IDs) != 1 || held.IDs[0] != f.cohort[2] {
		t.Errorf("notFound handle names %v, want the missing message", held.IDs)
	}
	if !strings.Contains(res.FailureNote, "email_get") {
		t.Errorf("nothing tells the caller the handles are usable: %q", res.FailureNote)
	}
}

// email_mark reports failures the same way. The two tools sharing one block is
// the point: a caller should not have to learn a second dialect.
func TestMarkReportsFailuresTheSameWay(t *testing.T) {
	forbidden := map[string]bool{}
	f := failFake(60, forbidden, map[string]bool{})
	forbidden[f.cohort[3]] = true
	s := testService(f.fake)
	handle := seedBigSelection(s, f.cohort)

	res, err := s.Mark(context.Background(), MarkParams{
		Handle: handle, Action: "read",
		Confirm: markPhrase(len(f.cohort), "read", "A1", f.cohort),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Failures) != 1 || res.Failures[0].SelectionID == "" {
		t.Fatalf("failures = %+v", res.Failures)
	}
	if res.Failures[0].Type != "forbidden" || res.Failures[0].Reason == "" {
		t.Errorf("group = %+v, want the type and a description", res.Failures[0])
	}
}
