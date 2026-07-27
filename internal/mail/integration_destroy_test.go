package mail_test

// Hermetic integration tests for email_destroy: the full ladder against the
// jmaptest server, verifying mail is really gone (or really untouched).

import (
	"context"
	"strings"
	"testing"

	"terva-ext-jmap-mail/internal/mail"
)

func TestIntegrationDestroy(t *testing.T) {
	seed, svc := startStack(t, 0)
	ctx := context.Background()

	exists := func(id string) bool {
		res, err := svc.Get(ctx, mail.GetParams{IDs: []string{id}, BodyFormat: mail.BodyMetadata})
		if err != nil {
			t.Fatal(err)
		}
		return len(res.Emails) == 1
	}

	// Dry run: previews, returns the phrase, destroys nothing.
	res, err := svc.Destroy(ctx, mail.DestroyParams{IDs: []string{seed.Trashed, seed.Welcome}, DryRun: true})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.ConfirmPhrase, "destroy 2 emails permanently") || !strings.Contains(res.ConfirmPhrase, "[batch ") {
		t.Errorf("confirmPhrase = %q, want count + batch digest", res.ConfirmPhrase)
	}
	phrase2 := res.ConfirmPhrase
	if len(res.Destroyed) != 1 || res.Destroyed[0].ID != seed.Trashed {
		t.Errorf("would-destroy = %+v", res.Destroyed)
	}
	if len(res.NotInTrash) != 1 || res.NotInTrash[0].ID != seed.Welcome {
		t.Errorf("notInTrash = %+v", res.NotInTrash)
	}
	if !exists(seed.Trashed) {
		t.Fatal("dry run destroyed mail")
	}

	// Real run against a non-trashed target: refused, nothing changed.
	_, err = svc.Destroy(ctx, mail.DestroyParams{IDs: []string{seed.Trashed, seed.Welcome}, Confirm: phrase2})
	if err == nil || !strings.Contains(err.Error(), seed.Welcome) {
		t.Fatalf("err = %v, want in-Trash refusal naming the blocker", err)
	}
	if !exists(seed.Welcome) || !exists(seed.Trashed) {
		t.Fatal("refused destroy still removed mail")
	}

	// The happy path: already-trashed mail + exact phrase → permanently gone.
	res, err = svc.Destroy(ctx, mail.DestroyParams{IDs: []string{seed.Trashed}, Confirm: mintDestroyPhrase(t, svc, mail.DestroyParams{IDs: []string{seed.Trashed}})})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Destroyed) != 1 || res.Destroyed[0].ID != seed.Trashed {
		t.Fatalf("res = %+v", res)
	}
	if exists(seed.Trashed) {
		t.Error("destroyed email still retrievable")
	}
	trash, err := svc.Search(ctx, mail.SearchParams{Mailbox: "trash"})
	if err != nil {
		t.Fatal(err)
	}
	if trash.Returned != 0 {
		t.Errorf("trash still holds %d messages", trash.Returned)
	}
}

func TestIntegrationDestroyTrashFirstFlow(t *testing.T) {
	seed, svc := startStack(t, 0)
	ctx := context.Background()

	// The intended two-step: email_trash, then email_destroy.
	if _, err := svc.Trash(ctx, mail.TrashParams{IDs: []string{seed.Newsletter}}); err != nil {
		t.Fatal(err)
	}
	res, err := svc.Destroy(ctx, mail.DestroyParams{IDs: []string{seed.Newsletter}, Confirm: mintDestroyPhrase(t, svc, mail.DestroyParams{IDs: []string{seed.Newsletter}})})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Destroyed) != 1 {
		t.Fatalf("res = %+v", res)
	}

	// allowNotInTrash destroys live mail directly (with the phrase).
	res, err = svc.Destroy(ctx, mail.DestroyParams{IDs: []string{seed.LongRead}, AllowNotInTrash: true, Confirm: mintDestroyPhrase(t, svc, mail.DestroyParams{IDs: []string{seed.LongRead}, AllowNotInTrash: true})})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Destroyed) != 1 {
		t.Fatalf("res = %+v", res)
	}
	got, err := svc.Get(ctx, mail.GetParams{IDs: []string{seed.LongRead}, BodyFormat: mail.BodyMetadata})
	if err != nil {
		t.Fatal(err)
	}
	if len(got.NotFound) != 1 {
		t.Errorf("destroyed mail still present: %+v", got)
	}
}

// mintDestroyPhrase dry-runs the same params to obtain the bound phrase —
// exactly the workflow an agent follows.
func mintDestroyPhrase(t *testing.T, svc *mail.Service, p mail.DestroyParams) string {
	t.Helper()
	p.DryRun = true
	res, err := svc.Destroy(context.Background(), p)
	if err != nil {
		t.Fatalf("mint dry run: %v", err)
	}
	if res.ConfirmPhrase == "" {
		t.Fatal("dry run returned no confirmPhrase")
	}
	return res.ConfirmPhrase
}

// The whole chain against a real server, with the model never transcribing an
// id: search names the set, the preview names what it cleared, the apply names
// nothing at all. This is the flow the tool descriptions tell it to use, so it
// is the one that has to work end to end rather than only against a fake.
func TestIntegrationDestroyBySelectionThenReceipt(t *testing.T) {
	seed, svc := startStack(t, 0)
	ctx := context.Background()

	// Put a second message in Trash so the batch is not a single id.
	if _, err := svc.Trash(ctx, mail.TrashParams{IDs: []string{seed.Newsletter}}); err != nil {
		t.Fatal(err)
	}
	found, err := svc.Search(ctx, mail.SearchParams{Mailbox: "trash", Fields: []string{"id"}})
	if err != nil {
		t.Fatal(err)
	}
	if found.Returned != 2 || found.SelectionID == "" {
		t.Fatalf("search returned %d with selectionId %q", found.Returned, found.SelectionID)
	}

	dry, err := svc.Destroy(ctx, mail.DestroyParams{Handle: found.SelectionID, DryRun: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(dry.Destroyed) != 2 || dry.ReceiptID == "" {
		t.Fatalf("preview over a selection: %d cleared, receipt %q", len(dry.Destroyed), dry.ReceiptID)
	}
	if dry.Selection == nil || dry.Selection.Remaining != 0 {
		t.Errorf("preview did not report consuming the whole selection: %+v", dry.Selection)
	}

	res, err := svc.Destroy(ctx, mail.DestroyParams{Handle: dry.ReceiptID})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Destroyed) != 2 {
		t.Fatalf("destroyed %d, want both previewed messages", len(res.Destroyed))
	}
	trash, err := svc.Search(ctx, mail.SearchParams{Mailbox: "trash"})
	if err != nil {
		t.Fatal(err)
	}
	if trash.Returned != 0 {
		t.Errorf("trash still holds %d messages", trash.Returned)
	}

	// The receipt keeps answering with what it did. With the mail gone there is
	// nothing else left to ask.
	again, err := svc.Destroy(ctx, mail.DestroyParams{Handle: dry.ReceiptID})
	if err != nil {
		t.Fatal(err)
	}
	if !again.Replayed || len(again.Destroyed) != 2 {
		t.Errorf("replay = %+v", again)
	}
}
