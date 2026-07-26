package mail_test

// Hermetic integration tests for the phase-2 organization tools: the real
// stack against the jmaptest server, verifying observable state changes.

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"terva-ext-jmap-mail/internal/jmaptest"
	"terva-ext-jmap-mail/internal/mail"
)

func TestIntegrationMark(t *testing.T) {
	seed, svc := startStack(t, 0)
	ctx := context.Background()

	unread := func() int {
		res, err := svc.Search(ctx, mail.SearchParams{Mailbox: "inbox", NotKeyword: "$seen"})
		if err != nil {
			t.Fatal(err)
		}
		return res.Returned
	}
	if n := unread(); n != 3 {
		t.Fatalf("seeded unread = %d", n)
	}

	// Dry run: classified but nothing changes.
	res, err := svc.Mark(ctx, mail.MarkParams{IDs: []string{seed.Planning2, seed.Welcome}, Action: "read", DryRun: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Changed) != 1 || res.Changed[0].ID != seed.Planning2 || len(res.AlreadySet) != 1 {
		t.Fatalf("dry run res = %+v", res)
	}
	if n := unread(); n != 3 {
		t.Errorf("dry run mutated state: unread = %d", n)
	}

	// Real run: unread drops, and the change is idempotent.
	res, err = svc.Mark(ctx, mail.MarkParams{IDs: []string{seed.Planning2}, Action: "read"})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Changed) != 1 || len(res.Failed) != 0 {
		t.Fatalf("res = %+v", res)
	}
	if n := unread(); n != 2 {
		t.Errorf("unread after mark = %d, want 2", n)
	}
	res, err = svc.Mark(ctx, mail.MarkParams{IDs: []string{seed.Planning2}, Action: "read"})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.AlreadySet) != 1 || len(res.Changed) != 0 {
		t.Errorf("re-mark res = %+v", res)
	}

	// Flag and verify via keyword search.
	if _, err := svc.Mark(ctx, mail.MarkParams{IDs: []string{seed.Newsletter}, Action: "flag"}); err != nil {
		t.Fatal(err)
	}
	flagged, err := svc.Search(ctx, mail.SearchParams{Keyword: "$flagged"})
	if err != nil {
		t.Fatal(err)
	}
	if flagged.Returned != 2 { // invoice (seeded) + newsletter
		t.Errorf("flagged = %v", ids(flagged.Emails))
	}
}

func TestIntegrationMove(t *testing.T) {
	seed, svc := startStack(t, 0)
	ctx := context.Background()

	// Dry run reports the origin without moving.
	res, err := svc.Move(ctx, mail.MoveParams{IDs: []string{seed.Invoice}, ToMailbox: "Archive", DryRun: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Moved) != 1 || res.Moved[0].From[0].Role != "inbox" {
		t.Fatalf("dry run = %+v", res.Moved)
	}

	// Real move: invoice leaves Inbox for Archive.
	res, err = svc.Move(ctx, mail.MoveParams{IDs: []string{seed.Invoice}, ToMailbox: "Archive"})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Moved) != 1 || len(res.Failed) != 0 {
		t.Fatalf("res = %+v", res)
	}
	got, err := svc.Get(ctx, mail.GetParams{IDs: []string{seed.Invoice}, BodyFormat: mail.BodyMetadata})
	if err != nil {
		t.Fatal(err)
	}
	mbs := got.Emails[0].Mailboxes
	if len(mbs) != 1 || mbs[0].Role != "archive" {
		t.Errorf("mailboxes after move = %+v", mbs)
	}

	// keepInMailboxes files into a second mailbox without leaving the first.
	if _, err := svc.Move(ctx, mail.MoveParams{IDs: []string{seed.Welcome}, ToMailbox: "Archive", KeepInMailboxes: true}); err != nil {
		t.Fatal(err)
	}
	got, err = svc.Get(ctx, mail.GetParams{IDs: []string{seed.Welcome}, BodyFormat: mail.BodyMetadata})
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Emails[0].Mailboxes) != 2 {
		t.Errorf("mailboxes after keep-move = %+v", got.Emails[0].Mailboxes)
	}

	// Partial failure: a bogus id lands in notFound, the rest move.
	res, err = svc.Move(ctx, mail.MoveParams{IDs: []string{seed.Newsletter, "msg-nope"}, ToMailbox: "Archive"})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Moved) != 1 || len(res.NotFound) != 1 || res.NotFound[0] != "msg-nope" {
		t.Errorf("partial res = %+v", res)
	}
}

func TestIntegrationTrash(t *testing.T) {
	seed, svc := startStack(t, 0)
	ctx := context.Background()

	res, err := svc.Trash(ctx, mail.TrashParams{IDs: []string{seed.Newsletter}})
	if err != nil {
		t.Fatal(err)
	}
	if res.Destination.Role != "trash" || len(res.Moved) != 1 {
		t.Fatalf("res = %+v", res)
	}

	// Gone from the inbox, present in Trash, and NOT destroyed.
	inbox, err := svc.Search(ctx, mail.SearchParams{Mailbox: "inbox"})
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range inbox.Emails {
		if e.ID == seed.Newsletter {
			t.Error("trashed email still in inbox")
		}
	}
	trash, err := svc.Search(ctx, mail.SearchParams{Mailbox: "trash"})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, e := range trash.Emails {
		if e.ID == seed.Newsletter {
			found = true
		}
	}
	if !found {
		t.Error("trashed email not in Trash")
	}
	got, err := svc.Get(ctx, mail.GetParams{IDs: []string{seed.Newsletter}, BodyFormat: mail.BodyMetadata})
	if err != nil || len(got.Emails) != 1 {
		t.Errorf("trashed email must still be retrievable: %v %+v", err, got)
	}
}

func TestIntegrationBulkConfirm(t *testing.T) {
	srv := jmaptest.New()
	t.Cleanup(srv.Close)
	seed := srv.SeedStandard()
	// Grow the inbox past the confirmation threshold.
	var bulk []string
	for i := 0; i < 25; i++ {
		bulk = append(bulk, srv.AddEmail(jmaptest.Email{
			MailboxIDs: []string{seed.InboxID},
			From:       []jmaptest.Address{{Name: "Bulk Sender", Email: "bulk@example.test"}},
			Subject:    fmt.Sprintf("Bulk message %d", i),
			TextBody:   "filler",
		}))
	}
	_, svc := startStackOn(t, srv)
	ctx := context.Background()

	// Refused without confirm; the refusal carries the exact phrase, which
	// names the destination, the account, and a digest of this exact batch.
	_, err := svc.Move(ctx, mail.MoveParams{IDs: bulk, ToMailbox: "Archive"})
	if err == nil || !strings.Contains(err.Error(), `"move 25 emails to Archive in account acc-test [batch `) {
		t.Fatalf("err = %v", err)
	}
	phrase := confirmPhraseFrom(t, err)
	// Re-running with that phrase succeeds and actually moves the mail. A run
	// this size reports counts, not 25 subject lines.
	res, err := svc.Move(ctx, mail.MoveParams{IDs: bulk, ToMailbox: "Archive", Confirm: phrase})
	if err != nil {
		t.Fatal(err)
	}
	if res.MovedCount != 25 || len(res.Moved) != 0 {
		t.Fatalf("movedCount = %d, moved = %d, want 25 and no enumeration", res.MovedCount, len(res.Moved))
	}
	if len(res.Sources) != 1 || res.Sources[0].Name != "Inbox" || res.Sources[0].Count != 25 {
		t.Errorf("sources = %+v, want 25 from Inbox", res.Sources)
	}
	if res.Sources[0].ID == "" {
		t.Error("source mailbox carries no id — the result cannot assert where mail came from on its own")
	}
	archived, err := svc.Search(ctx, mail.SearchParams{Mailbox: "archive", From: "bulk@example.test", Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	if archived.Returned != 25 {
		t.Errorf("archive contains %d bulk messages, want 25", archived.Returned)
	}
}

// startStackOn wires the real client+service onto an existing (pre-seeded)
// fake server.
func startStackOn(t *testing.T, srv *jmaptest.Server) (*jmaptest.Server, *mail.Service) {
	t.Helper()
	svc := newStackService(t, srv, 0)
	return srv, svc
}
