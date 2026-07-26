package mail_test

// The audit projection, exercised against the hermetic JMAP server rather
// than hand-written fixtures. The unit tests pin the allow-list against
// results a human wrote down; this one pins it against results the code
// actually produces, which is what would drift.

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"terva-ext-jmap-mail/internal/jmaptest"
	"terva-ext-jmap-mail/internal/mail"
)

// project runs a value through the same path app.go does: render it as the
// tool result, then take the audit projection of that JSON.
func project(t *testing.T, args string, result any) map[string]any {
	t.Helper()
	body, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	detail, _ := mail.AuditDetail(json.RawMessage(args), body)
	return detail
}

// The whole feature rests on this: real mail, with real subjects and senders
// from the seeded server, must leave no trace of itself in an audit record.
func TestIntegrationAuditRecordsCarryNoMailContent(t *testing.T) {
	srv := jmaptest.New()
	t.Cleanup(srv.Close)
	seed := srv.SeedStandard()
	const (
		subject = "Quarterly compensation review — confidential"
		sender  = "hr-person@third-party.test"
	)
	for i := 0; i < 5; i++ {
		srv.AddEmail(jmaptest.Email{
			MailboxIDs: []string{seed.InboxID},
			From:       []jmaptest.Address{{Name: "HR Person", Email: sender}},
			Subject:    subject,
			TextBody:   "Body text nobody consented to have copied into a durable log.",
		})
	}
	svc := newStackService(t, srv, 0)
	ctx := context.Background()

	leaks := []string{subject, "compensation", sender, "HR Person", "consented"}
	check := func(what string, detail map[string]any) {
		t.Helper()
		rendered, err := json.Marshal(detail)
		if err != nil {
			t.Fatal(err)
		}
		for _, bad := range leaks {
			if strings.Contains(string(rendered), bad) {
				t.Errorf("%s: audit record leaked %q\n%s", what, bad, rendered)
			}
		}
	}

	// A search that matches on the subject: the filter text is as revealing
	// as the result, and neither may be recorded.
	page, err := svc.Search(ctx, mail.SearchParams{Mailbox: "inbox", Subject: "compensation"})
	if err != nil {
		t.Fatal(err)
	}
	check("email_search", project(t, `{"mailbox":"inbox","subject":"compensation"}`, page))

	// A fetch with bodies — the largest content surface the extension has.
	got, err := svc.Get(ctx, mail.GetParams{IDs: []string{page.Emails[0].ID}, BodyFormat: mail.BodyBoth})
	if err != nil {
		t.Fatal(err)
	}
	check("email_get", project(t, `{"ids":["`+page.Emails[0].ID+`"],"bodyFormat":"both"}`, got))

	// A thread with bodies.
	thread, err := svc.GetThread(ctx, mail.ThreadParams{EmailID: page.Emails[0].ID, IncludeBodies: true})
	if err != nil {
		t.Fatal(err)
	}
	check("email_get_thread", project(t, `{"emailId":"x","includeBodies":true}`, thread))

	// A verbose move, which enumerates every message WITH its subject.
	ids := []string{page.Emails[0].ID, page.Emails[1].ID}
	verbose := true
	moved, err := svc.Move(ctx, mail.MoveParams{IDs: ids, ToMailbox: "Archive", Verbose: &verbose})
	if err != nil {
		t.Fatal(err)
	}
	if len(moved.Moved) == 0 {
		t.Fatal("verbose move enumerated nothing; the test is not exercising the leak path")
	}
	detail := project(t, `{"ids":["a","b"],"toMailbox":"Archive","verbose":true}`, moved)
	check("email_move", detail)

	// And it must still be a useful record of the mutation.
	if detail["movedCount"] != float64(2) {
		t.Errorf("movedCount = %v", detail["movedCount"])
	}
	dest, _ := detail["destination"].(map[string]any)
	if dest["id"] == "" || dest["name"] != "Archive" {
		t.Errorf("destination = %v, want the mailbox identified and named", detail["destination"])
	}
	src, _ := detail["sources"].([]any)
	if len(src) != 1 {
		t.Fatalf("sources = %v, want the inbox they came from", detail["sources"])
	}
	if first, _ := src[0].(map[string]any); first["id"] != seed.InboxID {
		t.Errorf("source = %v, want %s", src[0], seed.InboxID)
	}
	// Computed counts are Go ints; allow-listed ones arrive as JSON float64.
	// Both render identically, so the distinction never reaches the file.
	if detail["enumerated"] != 2 {
		t.Errorf("enumerated = %v, want the count of a verbose listing without its contents", detail["enumerated"])
	}
}

// email_destroy is the exception, and the one that matters most: the messages
// are gone, so the record is the only evidence they existed.
func TestIntegrationAuditKeepsDestroyedIDs(t *testing.T) {
	srv := jmaptest.New()
	t.Cleanup(srv.Close)
	seed := srv.SeedStandard()
	const subject = "Receipt for order 88213 — Acme Supplies"
	var ids []string
	for i := 0; i < 3; i++ {
		ids = append(ids, srv.AddEmail(jmaptest.Email{
			MailboxIDs: []string{seed.TrashID},
			From:       []jmaptest.Address{{Email: "orders@acme.test"}},
			Subject:    subject,
		}))
	}
	svc := newStackService(t, srv, 0)

	dry, err := svc.Destroy(context.Background(), mail.DestroyParams{IDs: ids, DryRun: true})
	if err != nil {
		t.Fatal(err)
	}
	detail := project(t, `{"ids":["a","b","c"],"dryRun":true}`, dry)

	rendered, _ := json.Marshal(detail)
	for _, bad := range []string{subject, "Acme", "orders@acme.test", "88213"} {
		if strings.Contains(string(rendered), bad) {
			t.Errorf("destroy record leaked %q\n%s", bad, rendered)
		}
	}
	destroyed, _ := detail["destroyedIds"].([]string)
	if len(destroyed) != 3 {
		t.Fatalf("destroyedIds = %v, want all three — they are the only surviving record", detail["destroyedIds"])
	}
	for _, want := range ids {
		var found bool
		for _, got := range destroyed {
			if got == want {
				found = true
			}
		}
		if !found {
			t.Errorf("destroyed id %s is not in the audit record", want)
		}
	}
}

// A refusal is as worth recording as a success — more so for monitoring,
// since a run full of refusals is the signal. The record must describe the
// attempt even though there is no result to project.
func TestIntegrationAuditDescribesARefusal(t *testing.T) {
	srv := jmaptest.New()
	t.Cleanup(srv.Close)
	seed := srv.SeedStandard()
	var ids []string
	for i := 0; i < 25; i++ {
		ids = append(ids, srv.AddEmail(jmaptest.Email{
			MailboxIDs: []string{seed.InboxID}, Subject: "SENTINEL-SUBJECT",
		}))
	}
	svc := newStackService(t, srv, 0)

	// Over the bulk threshold with no confirm: refused before anything moves.
	if _, err := svc.Move(context.Background(), mail.MoveParams{IDs: ids, ToMailbox: "Archive"}); err == nil {
		t.Fatal("bulk move without a confirm phrase was accepted")
	}
	args, _ := json.Marshal(map[string]any{"ids": ids, "toMailbox": "Archive"})
	detail, _ := mail.AuditDetail(args, nil) // an error result projects to nothing

	if detail["idCount"] != 25 {
		t.Errorf("idCount = %v, want the attempted batch size", detail["idCount"])
	}
	if detail["toMailbox"] != "Archive" {
		t.Errorf("toMailbox = %v, want the attempted destination", detail["toMailbox"])
	}
	if detail["batch"] == nil {
		t.Error("no batch digest: the refusal cannot be matched to the retry that follows it")
	}
	rendered, _ := json.Marshal(detail)
	if strings.Contains(string(rendered), "SENTINEL") {
		t.Errorf("refusal record leaked a subject: %s", rendered)
	}
}
