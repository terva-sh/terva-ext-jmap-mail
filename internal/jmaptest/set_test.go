package jmaptest

// Raw-HTTP tests pinning Email/set behavior to RFC 8620 §5.3 / RFC 8621 §4.6.

import (
	"fmt"
	"strings"
	"testing"
)

// setCall posts one Email/set and returns its response args.
func setCall(t *testing.T, s *Server, argsJSON string) map[string]any {
	t.Helper()
	status, body := post(t, s, "application/json", s.Token,
		`{"using": `+usingBoth+`, "methodCalls": [["Email/set", `+argsJSON+`, "s0"]]}`)
	if status != 200 {
		t.Fatalf("status = %d body = %v", status, body)
	}
	name, args, _ := firstResponse(t, body)
	if name == "error" {
		t.Fatalf("method error: %v", args)
	}
	return args
}

// getEmail fetches one message's mailboxIds+keywords via Email/get.
func getEmail(t *testing.T, s *Server, id string) (map[string]any, bool) {
	t.Helper()
	_, body := post(t, s, "application/json", s.Token,
		`{"using": `+usingBoth+`, "methodCalls": [["Email/get", {"accountId": "`+s.AccountID+`", "ids": ["`+id+`"], "properties": ["id", "keywords", "mailboxIds"]}, "g0"]]}`)
	_, args, _ := firstResponse(t, body)
	list, _ := args["list"].([]any)
	if len(list) == 0 {
		return nil, false
	}
	m, _ := list[0].(map[string]any)
	return m, true
}

func TestSetKeywordPatch(t *testing.T) {
	s, seed := startSeeded(t)

	// Add $seen to an unread message via the patch pointer.
	args := setCall(t, s, `{"accountId": "acc-test", "update": {"`+seed.Planning2+`": {"keywords/$seen": true}}}`)
	updated, _ := args["updated"].(map[string]any)
	if _, ok := updated[seed.Planning2]; !ok {
		t.Fatalf("updated = %v", updated)
	}
	if args["oldState"] == args["newState"] {
		t.Error("state must change after a successful set")
	}
	e, _ := getEmail(t, s, seed.Planning2)
	kw, _ := e["keywords"].(map[string]any)
	if kw["$seen"] != true {
		t.Errorf("keywords = %v", kw)
	}

	// Remove it again with null.
	setCall(t, s, `{"accountId": "acc-test", "update": {"`+seed.Planning2+`": {"keywords/$seen": null}}}`)
	e, _ = getEmail(t, s, seed.Planning2)
	kw, _ = e["keywords"].(map[string]any)
	if _, present := kw["$seen"]; present {
		t.Errorf("$seen still present: %v", kw)
	}
}

func TestSetMailboxReplaceAndPatch(t *testing.T) {
	s, seed := startSeeded(t)

	// Whole-property replace: move invoice to Archive only.
	setCall(t, s, `{"accountId": "acc-test", "update": {"`+seed.Invoice+`": {"mailboxIds": {"`+seed.ArchiveID+`": true}}}}`)
	e, _ := getEmail(t, s, seed.Invoice)
	mbs, _ := e["mailboxIds"].(map[string]any)
	if len(mbs) != 1 || mbs[seed.ArchiveID] != true {
		t.Fatalf("mailboxIds = %v", mbs)
	}

	// Additive patch: also file into Inbox, keeping Archive.
	setCall(t, s, `{"accountId": "acc-test", "update": {"`+seed.Invoice+`": {"mailboxIds/`+seed.InboxID+`": true}}}`)
	e, _ = getEmail(t, s, seed.Invoice)
	mbs, _ = e["mailboxIds"].(map[string]any)
	if len(mbs) != 2 {
		t.Fatalf("mailboxIds = %v", mbs)
	}
}

func TestSetRejectsOrphaningAndUnknowns(t *testing.T) {
	s, seed := startSeeded(t)
	cases := []struct {
		name  string
		patch string
	}{
		{"remove last mailbox", `{"mailboxIds/` + seed.InboxID + `": null}`},
		{"empty mailboxIds", `{"mailboxIds": {}}`},
		{"unknown mailbox", `{"mailboxIds": {"mb-nope": true}}`},
		{"unsupported property", `{"subject": "rewritten"}`},
		{"non-true set value", `{"mailboxIds": {"` + seed.InboxID + `": false}}`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			args := setCall(t, s, `{"accountId": "acc-test", "update": {"`+seed.Welcome+`": `+c.patch+`}}`)
			notUpdated, _ := args["notUpdated"].(map[string]any)
			serr, _ := notUpdated[seed.Welcome].(map[string]any)
			if serr == nil || serr["type"] != "invalidProperties" {
				t.Fatalf("notUpdated = %v, want invalidProperties", notUpdated)
			}
		})
	}
	// And nothing changed along the way.
	e, _ := getEmail(t, s, seed.Welcome)
	mbs, _ := e["mailboxIds"].(map[string]any)
	if len(mbs) != 1 || mbs[seed.InboxID] != true {
		t.Errorf("welcome was mutated by failing patches: %v", mbs)
	}
}

// A patch mixing one valid and one invalid key must change nothing
// (RFC 8620 §5.3: the update for that object MUST be rejected entirely).
func TestSetPatchAtomicity(t *testing.T) {
	s, seed := startSeeded(t)
	args := setCall(t, s, `{"accountId": "acc-test", "update": {"`+seed.Planning2+`": {"keywords/$seen": true, "bogus": 1}}}`)
	notUpdated, _ := args["notUpdated"].(map[string]any)
	if _, ok := notUpdated[seed.Planning2]; !ok {
		t.Fatalf("notUpdated = %v", notUpdated)
	}
	e, _ := getEmail(t, s, seed.Planning2)
	kw, _ := e["keywords"].(map[string]any)
	if kw["$seen"] == true {
		t.Error("partial patch applied despite invalid sibling key")
	}
}

func TestSetDestroy(t *testing.T) {
	s, seed := startSeeded(t)
	args := setCall(t, s, `{"accountId": "acc-test", "destroy": ["`+seed.Trashed+`", "msg-nope"]}`)
	destroyed, _ := args["destroyed"].([]any)
	if len(destroyed) != 1 || destroyed[0] != seed.Trashed {
		t.Fatalf("destroyed = %v", destroyed)
	}
	notDestroyed, _ := args["notDestroyed"].(map[string]any)
	serr, _ := notDestroyed["msg-nope"].(map[string]any)
	if serr == nil || serr["type"] != "notFound" {
		t.Errorf("notDestroyed = %v", notDestroyed)
	}
	if _, found := getEmail(t, s, seed.Trashed); found {
		t.Error("destroyed email still retrievable")
	}
}

func TestSetIfInState(t *testing.T) {
	s, seed := startSeeded(t)
	_, body := post(t, s, "application/json", s.Token,
		`{"using": `+usingBoth+`, "methodCalls": [["Email/set", {"accountId": "acc-test", "ifInState": "state-999", "update": {"`+seed.Welcome+`": {"keywords/$flagged": true}}}, "s0"]]}`)
	name, args, _ := firstResponse(t, body)
	if name != "error" || args["type"] != "stateMismatch" {
		t.Fatalf("response = %s %v, want stateMismatch", name, args)
	}
}

func TestSetRequestTooLarge(t *testing.T) {
	s, _ := startSeeded(t)
	var sb strings.Builder
	for i := 0; i <= maxObjectsInSet; i++ {
		if i > 0 {
			sb.WriteString(",")
		}
		fmt.Fprintf(&sb, `"id-%d": {"keywords/$seen": true}`, i)
	}
	_, body := post(t, s, "application/json", s.Token,
		`{"using": `+usingBoth+`, "methodCalls": [["Email/set", {"accountId": "acc-test", "update": {`+sb.String()+`}}, "s0"]]}`)
	name, args, _ := firstResponse(t, body)
	if name != "error" || args["type"] != "requestTooLarge" {
		t.Fatalf("response = %s %v, want requestTooLarge", name, args)
	}
}
