package jmaptest

import (
	"strings"
	"time"
)

// Seed names the fixture objects SeedStandard created, for test assertions.
type Seed struct {
	InboxID, ArchiveID, DraftsID, SentID, TrashID string

	PlanningThreadID string

	// Email ids by fixture name.
	Welcome    string // Inbox, read, from Alice
	Planning1  string // Inbox, read, from Alice (thread 1/3)
	Planning2  string // Inbox, UNREAD, from Bob (thread 2/3)
	Planning3  string // Inbox, read, from Alice, attachment (thread 3/3)
	Invoice    string // Inbox, read+flagged, attachment
	Newsletter string // Inbox, UNREAD
	LongRead   string // Inbox, UNREAD, ~20KB multibyte text + HTML body
	Archived   string // Archive, read, from Alice
	Trashed    string // Trash, read
}

// SeedStandard populates the store with the standard fixture set: five role
// mailboxes and nine messages covering threads, unread/flagged keywords,
// attachments, long multibyte bodies, and non-inbox mailboxes.
//
// Inbox totals: 7 emails (3 unread), 5 threads (3 with unread mail).
func (s *Server) SeedStandard() Seed {
	var seed Seed
	seed.InboxID = s.AddMailbox(Mailbox{Name: "Inbox", Role: "inbox", SortOrder: 1})
	seed.ArchiveID = s.AddMailbox(Mailbox{Name: "Archive", Role: "archive", SortOrder: 2})
	seed.DraftsID = s.AddMailbox(Mailbox{Name: "Drafts", Role: "drafts", SortOrder: 3})
	seed.SentID = s.AddMailbox(Mailbox{Name: "Sent", Role: "sent", SortOrder: 4})
	seed.TrashID = s.AddMailbox(Mailbox{Name: "Trash", Role: "trash", SortOrder: 5})

	alice := Address{Name: "Alice Chen", Email: "alice@example.test"}
	bob := Address{Name: "Bob Osei", Email: "bob@example.test"}
	me := Address{Name: "Tester", Email: s.Username}
	day := func(d int, hour int) time.Time { return time.Date(2026, 6, d, hour, 0, 0, 0, time.UTC) }

	seed.Welcome = s.AddEmail(Email{
		MailboxIDs: []string{seed.InboxID},
		Keywords:   []string{"$seen"},
		From:       []Address{alice}, To: []Address{me},
		Subject:    "Welcome to the project",
		TextBody:   "Hello and welcome to the project. The onboarding notes are in the wiki; ping me with questions.",
		ReceivedAt: day(1, 10),
	})

	seed.PlanningThreadID = "thr-planning"
	seed.Planning1 = s.AddEmail(Email{
		ThreadID:   seed.PlanningThreadID,
		MailboxIDs: []string{seed.InboxID},
		Keywords:   []string{"$seen"},
		From:       []Address{alice}, To: []Address{me, bob},
		Subject:    "Planning kickoff",
		TextBody:   "Kicking off Q3 planning. Agenda to follow.",
		ReceivedAt: day(2, 9),
	})
	seed.Planning2 = s.AddEmail(Email{
		ThreadID:   seed.PlanningThreadID,
		MailboxIDs: []string{seed.InboxID},
		From:       []Address{bob}, To: []Address{me, alice},
		Subject:    "Re: Planning kickoff",
		TextBody:   "Can we move the slot an hour later?",
		ReceivedAt: day(3, 14),
	})
	seed.Planning3 = s.AddEmail(Email{
		ThreadID:   seed.PlanningThreadID,
		MailboxIDs: []string{seed.InboxID},
		Keywords:   []string{"$seen"},
		From:       []Address{alice}, To: []Address{me, bob},
		Subject:  "Re: Planning kickoff",
		TextBody: "Works for me — agenda attached.",
		Attachments: []Attachment{
			{Name: "agenda.pdf", Type: "application/pdf", Size: 48211},
		},
		ReceivedAt: day(4, 8),
	})

	seed.Invoice = s.AddEmail(Email{
		MailboxIDs: []string{seed.InboxID},
		Keywords:   []string{"$seen", "$flagged"},
		From:       []Address{{Name: "Vendor Billing", Email: "billing@vendor.example.test"}},
		To:         []Address{me},
		Subject:    "Invoice #42 for June",
		TextBody:   "Please find invoice #42 attached. Payment is due within 30 days.",
		Attachments: []Attachment{
			{Name: "invoice-42.pdf", Type: "application/pdf", Size: 102400},
		},
		ReceivedAt: day(10, 11),
	})

	seed.Newsletter = s.AddEmail(Email{
		MailboxIDs: []string{seed.InboxID},
		From:       []Address{{Name: "Example Weekly", Email: "news@list.example.test"}},
		To:         []Address{me},
		Subject:    "Weekly digest: protocols corner",
		TextBody: "Manage: https://news.example.test/prefs?u=drew&token=s3cr3tT0ken12345\n" +
			"This week in open protocols: JMAP adoption keeps growing.\n" +
			"Unsubscribe: https://news.example.test/unsub/dXNlcjEyM3NlY3JldDQ1Njc4/confirm",
		ReceivedAt: day(12, 7),
	})

	seed.LongRead = s.AddEmail(Email{
		MailboxIDs: []string{seed.InboxID},
		From:       []Address{bob}, To: []Address{me},
		Subject:    "The complete saga",
		TextBody:   strings.Repeat("All work and no play makes the agent a dull model. ", 400) + "Fin — café ☕.",
		HTMLBody:   "<p>All work and no play makes the agent a dull model.</p><p>Fin — café ☕.</p>",
		ReceivedAt: day(15, 16),
	})

	seed.Archived = s.AddEmail(Email{
		MailboxIDs: []string{seed.ArchiveID},
		Keywords:   []string{"$seen"},
		From:       []Address{alice}, To: []Address{me},
		Subject:    "Notes from the retro",
		TextBody:   "Archiving the retro notes for reference.",
		ReceivedAt: day(5, 13),
	})

	seed.Trashed = s.AddEmail(Email{
		MailboxIDs: []string{seed.TrashID},
		Keywords:   []string{"$seen"},
		From:       []Address{bob}, To: []Address{me},
		Subject:    "Old draft to discard",
		TextBody:   "This one can go.",
		ReceivedAt: day(6, 18),
	})

	return seed
}
