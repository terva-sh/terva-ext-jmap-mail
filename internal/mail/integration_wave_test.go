package mail_test

// A bulk-organization wave, end to end against the hermetic server, with its
// tool output measured. The field report that motivated the projection and
// counts-only work put a 200-message archive batch at ~321KB (~80k tokens),
// about three batches to a 300k context window; this pins that it stays a
// rounding error, so a property added to EmailSummary or an enumeration put
// back into an organize result cannot quietly undo it.

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"terva-ext-jmap-mail/internal/jmaptest"
	"terva-ext-jmap-mail/internal/mail"
)

// maxWaveBytesPerMessage is the whole wave's tool output — every search, every
// dry run, every apply — divided by the messages organized. Fake ids are short
// (msg-N) where a real provider's are ~25 characters, so treat the number as a
// regression tripwire, not a promise: the shapes that matter are ~20 B/message
// here, and the pre-projection shapes were ~1,600.
const maxWaveBytesPerMessage = 60

func TestIntegrationBulkWavePayloadBudget(t *testing.T) {
	const total = 200

	srv := jmaptest.New()
	t.Cleanup(srv.Close)
	seed := srv.SeedStandard()
	for i := 0; i < total; i++ {
		srv.AddEmail(jmaptest.Email{
			MailboxIDs: []string{seed.InboxID},
			From:       []jmaptest.Address{{Name: "Newsletter Sender", Email: "news@example.test"}},
			Subject:    fmt.Sprintf("Weekly roundup %d — everything you missed", i),
			TextBody:   strings.Repeat("Newsletter body text that becomes a preview. ", 20),
			Keywords:   []string{"$seen"},
		})
	}
	svc := newStackService(t, srv, 0)
	ctx := context.Background()

	spent := 0
	organized := 0
	for batch := 0; batch < 10; batch++ {
		// Collect ids only — the cheapest result shape, and one page covers a
		// whole mutating batch.
		page, err := svc.Search(ctx, mail.SearchParams{
			Mailbox: "inbox", From: "news@example.test", Fields: []string{"id"}, Limit: 200,
		})
		if err != nil {
			t.Fatal(err)
		}
		spent += payloadSize(t, page)
		if page.Returned == 0 {
			break
		}
		ids := ids(page.Emails)

		dry, err := svc.Move(ctx, mail.MoveParams{IDs: ids, ToMailbox: "Archive", DryRun: true})
		if err != nil {
			t.Fatal(err)
		}
		spent += payloadSize(t, dry)
		// Above the bulk threshold (20) the dry run owes the caller a phrase
		// and no enumeration; at or below it, the list is the answer and a
		// phrase is not required — so the loop stays correct for any `total`.
		if len(ids) > 20 && (dry.ConfirmPhrase == "" || len(dry.Moved) != 0) {
			t.Fatalf("bulk dry run should hand back a phrase and no enumeration: %+v", dry)
		}

		applied, err := svc.Move(ctx, mail.MoveParams{IDs: ids, ToMailbox: "Archive", Confirm: dry.ConfirmPhrase})
		if err != nil {
			t.Fatal(err)
		}
		spent += payloadSize(t, applied)
		organized += applied.MovedCount

		// The cohort self-excludes (moved mail leaves the inbox filter), so the
		// next page is re-queried at position 0 — and queryState says the
		// matching set moved, which is the signal that discipline is required.
		next, err := svc.Search(ctx, mail.SearchParams{
			Mailbox: "inbox", From: "news@example.test", Fields: []string{"id"}, Limit: 1,
		})
		if err != nil {
			t.Fatal(err)
		}
		if next.QueryState == page.QueryState {
			t.Errorf("queryState unchanged across a mutation (%q) — a paging caller cannot detect cohort drift", next.QueryState)
		}
		spent += payloadSize(t, next)
	}

	if organized != total {
		t.Fatalf("organized %d of %d messages", organized, total)
	}
	if budget := total * maxWaveBytesPerMessage; spent > budget {
		t.Errorf("wave cost %d bytes of tool output for %d messages (%d B/message); budget is %d",
			spent, total, spent/total, budget)
	}

	// The load-bearing comparison, and the one that survives id length
	// changing: the entire wave must cost less than a single unprojected page.
	onePage, err := svc.Search(ctx, mail.SearchParams{Mailbox: "archive", Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	if unprojected := payloadSize(t, onePage); spent >= unprojected {
		t.Errorf("the whole %d-message wave cost %d bytes; one unprojected 100-message page costs %d — the projection is not paying for itself",
			total, spent, unprojected)
	}
	t.Logf("wave: %d messages, %d bytes of tool output (%d B/message)", total, spent, spent/total)
}
