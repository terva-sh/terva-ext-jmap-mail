package mail

// Unit tests for the destroy safety ladder against the scripted fake Caller.

import (
	"context"
	"strings"
	"testing"

	"terva-ext-jmap-mail/internal/jmap"
)

// destroyFake serves a store with e-trash (only in Trash), e-inbox (in Inbox),
// and echoes destroy requests back as destroyed.
func destroyFake() *fake {
	f := &fake{}
	f.handler = func(calls []jmap.Invocation) *jmap.Response {
		switch calls[0].Name {
		case "Mailbox/get":
			return response(result("Mailbox/get", calls[0].CallID, map[string]any{"list": mailboxFixture}))
		case "Email/get":
			ids := stringifyIDs(calls[0])
			list := []any{}
			var notFound []string
			for _, id := range ids {
				switch id {
				case "e-trash":
					list = append(list, map[string]any{"id": id, "subject": "Trashed", "mailboxIds": map[string]bool{"mb-trash": true}})
				case "e-inbox":
					list = append(list, map[string]any{"id": id, "subject": "Live", "mailboxIds": map[string]bool{"mb-inbox": true}})
				default:
					notFound = append(notFound, id)
				}
			}
			return response(result("Email/get", calls[0].CallID, map[string]any{"state": "st-snapshot", "list": list, "notFound": notFound}))
		case "Email/set":
			return response(result("Email/set", calls[0].CallID, map[string]any{
				"destroyed": destroyArg(calls[0]), "notDestroyed": map[string]any{},
			}))
		}
		panic("unexpected call " + calls[0].Name)
	}
	return f
}

func stringifyIDs(inv jmap.Invocation) []string {
	args, _ := inv.Args.(map[string]any)
	switch t := args["ids"].(type) {
	case []string:
		return t
	case []any:
		out := make([]string, 0, len(t))
		for _, v := range t {
			out = append(out, v.(string))
		}
		return out
	}
	return nil
}

func argsOfAny(inv jmap.Invocation) map[string]any {
	m, _ := inv.Args.(map[string]any)
	return m
}

// destroyArg reads the destroy id list the service passed (native []string,
// never JSON round-tripped in the fake Caller).
func destroyArg(inv jmap.Invocation) []string {
	ids, _ := argsOfAny(inv)["destroy"].([]string)
	return ids
}

func TestDestroyAlwaysNeedsConfirm(t *testing.T) {
	s := testService(destroyFake())
	// Even a single id, even one already in Trash.
	_, err := s.Destroy(context.Background(), DestroyParams{IDs: []string{"e-trash"}})
	want := destroyPhrase(DestroyParams{IDs: []string{"e-trash"}})
	if err == nil || !strings.Contains(err.Error(), `"`+want+`"`) {
		t.Fatalf("err = %v, want refusal naming %q", err, want)
	}
	// Wrong phrase → still refused.
	_, err = s.Destroy(context.Background(), DestroyParams{IDs: []string{"e-trash"}, Confirm: "yes"})
	if err == nil {
		t.Fatal("wrong confirm accepted")
	}
}

func TestDestroyConfirmCheckedBeforeNetwork(t *testing.T) {
	f := destroyFake()
	s := testService(f)
	s.Destroy(context.Background(), DestroyParams{IDs: []string{"e-trash"}})
	if len(f.recorded) != 0 {
		t.Errorf("confirm refusal made %d network calls, want 0", len(f.recorded))
	}
}

func TestDestroyDryRun(t *testing.T) {
	f := destroyFake()
	s := testService(f)
	res, err := s.Destroy(context.Background(), DestroyParams{IDs: []string{"e-trash", "e-inbox", "e-gone"}, DryRun: true})
	if err != nil {
		t.Fatal(err)
	}
	want := destroyPhrase(DestroyParams{IDs: []string{"e-trash", "e-inbox", "e-gone"}})
	if res.ConfirmPhrase != want || !strings.Contains(want, "[batch ") {
		t.Errorf("confirmPhrase = %q, want %q", res.ConfirmPhrase, want)
	}
	if len(res.Destroyed) != 1 || res.Destroyed[0].ID != "e-trash" {
		t.Errorf("would-destroy = %+v", res.Destroyed)
	}
	if len(res.NotInTrash) != 1 || res.NotInTrash[0].ID != "e-inbox" {
		t.Errorf("notInTrash = %+v", res.NotInTrash)
	}
	if len(res.NotFound) != 1 || res.NotFound[0] != "e-gone" {
		t.Errorf("notFound = %v", res.NotFound)
	}
	for _, calls := range f.recorded {
		for _, c := range calls {
			if c.Name == "Email/set" {
				t.Fatal("dry run must not send Email/set")
			}
		}
	}
}

func TestDestroyTrashGate(t *testing.T) {
	s := testService(destroyFake())
	// A non-trashed target blocks the whole run.
	_, err := s.Destroy(context.Background(), DestroyParams{
		IDs: []string{"e-trash", "e-inbox"}, Confirm: destroyPhrase(DestroyParams{IDs: []string{"e-trash", "e-inbox"}}),
	})
	if err == nil || !strings.Contains(err.Error(), "e-inbox") || !strings.Contains(err.Error(), "email_trash") {
		t.Fatalf("err = %v, want refusal naming the blocker and the alternative", err)
	}

	// allowNotInTrash overrides the gate (confirm still required).
	f := destroyFake()
	s2 := testService(f)
	override := DestroyParams{IDs: []string{"e-trash", "e-inbox"}, AllowNotInTrash: true}
	override.Confirm = destroyPhrase(override)
	if !strings.Contains(override.Confirm, "including outside Trash") {
		t.Fatalf("gate-skipping phrase must differ: %q", override.Confirm)
	}
	res, err := s2.Destroy(context.Background(), override)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Destroyed) != 2 {
		t.Errorf("destroyed = %+v", res.Destroyed)
	}
}

func TestDestroyExcludesNotFoundFromSet(t *testing.T) {
	f := destroyFake()
	s := testService(f)
	res, err := s.Destroy(context.Background(), DestroyParams{
		IDs: []string{"e-trash", "e-gone"}, Confirm: destroyPhrase(DestroyParams{IDs: []string{"e-trash", "e-gone"}}),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Destroyed) != 1 || len(res.NotFound) != 1 {
		t.Fatalf("res = %+v", res)
	}
	set := findBatch(t, f, "Email/set")
	ids := argsOfAny(set[0])["destroy"].([]string)
	if len(ids) != 1 || ids[0] != "e-trash" {
		t.Errorf("destroy ids = %v, must exclude not-found", ids)
	}
}

// The destroy is bound to the snapshot it was gated on (RFC 8620 §5.3).
func TestDestroySendsIfInStateFromSnapshot(t *testing.T) {
	f := destroyFake()
	s := testService(f)
	if _, err := s.Destroy(context.Background(), DestroyParams{IDs: []string{"e-trash"}, Confirm: destroyPhrase(DestroyParams{IDs: []string{"e-trash"}})}); err != nil {
		t.Fatal(err)
	}
	set := findBatch(t, f, "Email/set")
	if got := argsOfAny(set[0])["ifInState"]; got != "st-snapshot" {
		t.Errorf("ifInState = %v, want the Email/get snapshot state", got)
	}
}

func TestDestroyStateMismatchAbortsWithGuidance(t *testing.T) {
	f := destroyFake()
	base := f.handler
	f.handler = func(calls []jmap.Invocation) *jmap.Response {
		if calls[0].Name == "Email/set" {
			return response(jmap.InvocationResult{Name: "error", Args: []byte(`{"type":"stateMismatch","description":"state changed"}`), CallID: calls[0].CallID})
		}
		return base(calls)
	}
	s := testService(f)
	_, err := s.Destroy(context.Background(), DestroyParams{IDs: []string{"e-trash"}, Confirm: destroyPhrase(DestroyParams{IDs: []string{"e-trash"}})})
	if err == nil || !strings.Contains(err.Error(), "nothing deleted") || !strings.Contains(err.Error(), "dryRun") {
		t.Fatalf("err = %v, want abort guidance", err)
	}
}

func TestDestroyPartialFailure(t *testing.T) {
	f := destroyFake()
	base := f.handler
	f.handler = func(calls []jmap.Invocation) *jmap.Response {
		if calls[0].Name == "Email/set" {
			return response(result("Email/set", calls[0].CallID, map[string]any{
				"destroyed":    []string{},
				"notDestroyed": map[string]any{"e-trash": map[string]any{"type": "forbidden", "description": "shared account"}},
			}))
		}
		return base(calls)
	}
	s := testService(f)
	res, err := s.Destroy(context.Background(), DestroyParams{IDs: []string{"e-trash"}, Confirm: destroyPhrase(DestroyParams{IDs: []string{"e-trash"}})})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Destroyed) != 0 || res.Failed["e-trash"] != "forbidden: shared account" {
		t.Errorf("res = %+v", res)
	}
}
