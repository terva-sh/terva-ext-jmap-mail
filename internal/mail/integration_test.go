package mail_test

// Hermetic integration tests: the real stack (jmap.Client over HTTP →
// mail.Service) against the in-memory jmaptest server. These run in plain
// `just test` — no network, no credentials — and are the safety net the
// tag-gated live tests (live_test.go) verify against a real provider.

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"unicode/utf8"

	"terva-ext-jmap-mail/internal/config"
	"terva-ext-jmap-mail/internal/jmap"
	"terva-ext-jmap-mail/internal/jmaptest"
	"terva-ext-jmap-mail/internal/mail"
)

func startStack(t *testing.T, maxBodyBytes int) (jmaptest.Seed, *mail.Service) {
	t.Helper()
	srv := jmaptest.New()
	t.Cleanup(srv.Close)
	seed := srv.SeedStandard()
	return seed, newStackService(t, srv, maxBodyBytes)
}

// newStackService wires the real client+service onto a fake server.
func newStackService(t *testing.T, srv *jmaptest.Server, maxBodyBytes int) *mail.Service {
	t.Helper()
	// read-organize, because the organize paths are what these tests drive —
	// and because a search only mints a selectionId when the tools that could
	// consume it are available.
	cfg := config.Normalize(config.Settings{
		SessionURL:   srv.SessionURL(),
		APIToken:     srv.Token,
		MaxBodyBytes: maxBodyBytes,
		AccessLevel:  config.AccessOrganize,
	})
	if err := cfg.Validate(); err != nil {
		t.Fatal(err) // httptest is http://127.0.0.1 — loopback http is allowed
	}
	return mail.NewService(jmap.NewClient(cfg.SessionURL, cfg.APIToken), cfg)
}

func ids(emails []mail.EmailSummary) []string {
	out := make([]string, 0, len(emails))
	for _, e := range emails {
		out = append(out, e.ID)
	}
	return out
}

// payloadOf renders a result the way the tool layer does — indented, which is
// most of the reason a wrapped id costs about forty bytes and a bare one about
// twenty — so a measured size here is comparable to a real transcript's.
func payloadOf(t *testing.T, v any) string {
	t.Helper()
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		t.Fatalf("marshal result: %v", err)
	}
	return string(b)
}

func payloadSize(t *testing.T, v any) int {
	t.Helper()
	return len(payloadOf(t, v))
}

// confirmPhraseFrom pulls the quoted phrase out of a bulk refusal, the way a
// caller reading the message does. Phrases carry a digest of the id batch, so
// a test cannot spell one out in advance without recomputing it.
func confirmPhraseFrom(t *testing.T, err error) string {
	t.Helper()
	msg := err.Error()
	i := strings.Index(msg, `"`)
	j := strings.LastIndex(msg, `"`)
	if i < 0 || j <= i {
		t.Fatalf("no quoted phrase in refusal: %v", err)
	}
	return msg[i+1 : j]
}

func TestIntegrationStatus(t *testing.T) {
	_, svc := startStack(t, 0)
	st, err := svc.Status(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !st.Configured || st.AccountID != "acc-test" || st.Username != "tester@example.test" {
		t.Errorf("status = %+v", st)
	}
	if st.Limits == nil || st.Limits.MaxCallsInRequest != 16 || st.Limits.MaxObjectsInGet != 500 {
		t.Errorf("limits = %+v", st.Limits)
	}
	found := false
	for _, c := range st.Capabilities {
		if c == "urn:ietf:params:jmap:mail" {
			found = true
		}
	}
	if !found {
		t.Errorf("capabilities = %v", st.Capabilities)
	}
	if len(st.Accounts) != 1 || !st.Accounts[0].SupportsMail || !st.Accounts[0].IsPrimaryMail {
		t.Errorf("accounts = %+v", st.Accounts)
	}
	if len(st.Accounts[0].CapabilityUrns) == 0 {
		t.Errorf("account capabilityUrns not surfaced: %+v", st.Accounts[0])
	}
	// Config gating is visible so agents know WHY tools are absent.
	if st.AccessLevel != config.AccessOrganize || st.EnableSieveTools {
		t.Errorf("gating fields = %q/%v, want read-organize/false", st.AccessLevel, st.EnableSieveTools)
	}
}

func TestIntegrationAuthError(t *testing.T) {
	srv := jmaptest.New()
	t.Cleanup(srv.Close)
	srv.SeedStandard()
	cfg := config.Normalize(config.Settings{SessionURL: srv.SessionURL(), APIToken: "wrong-token"})
	svc := mail.NewService(jmap.NewClient(cfg.SessionURL, cfg.APIToken), cfg)

	_, err := svc.Status(context.Background())
	var ae *jmap.AuthError
	if !errors.As(err, &ae) {
		t.Fatalf("err = %v, want AuthError", err)
	}
	if strings.Contains(err.Error(), "wrong-token") {
		t.Errorf("error leaks token: %v", err)
	}
}

func TestIntegrationListMailboxes(t *testing.T) {
	seed, svc := startStack(t, 0)
	list, err := svc.ListMailboxes(context.Background(), mail.ListMailboxesParams{IncludeCounts: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(list.Mailboxes) != 5 {
		t.Fatalf("mailboxes = %+v", list.Mailboxes)
	}
	inbox := list.Mailboxes[0]
	if inbox.ID != seed.InboxID || inbox.Role != "inbox" {
		t.Fatalf("first mailbox = %+v, want Inbox (sortOrder)", inbox)
	}
	if inbox.Path != "Inbox" {
		t.Errorf("inbox path = %q, want Inbox", inbox.Path)
	}
	// Seeded inbox: 7 emails, 3 unread; 5 threads, 3 with unread mail.
	if inbox.TotalEmails == nil || *inbox.TotalEmails != 7 || *inbox.UnreadEmails != 3 {
		t.Errorf("inbox counts = %+v/%+v", inbox.TotalEmails, inbox.UnreadEmails)
	}
	if *inbox.TotalThreads != 5 || *inbox.UnreadThreads != 3 {
		t.Errorf("inbox thread counts = %d/%d", *inbox.TotalThreads, *inbox.UnreadThreads)
	}
}

func TestIntegrationSearch(t *testing.T) {
	seed, svc := startStack(t, 0)
	ctx := context.Background()

	t.Run("unread in inbox", func(t *testing.T) {
		res, err := svc.Search(ctx, mail.SearchParams{Mailbox: "inbox", NotKeyword: "$seen"})
		if err != nil {
			t.Fatal(err)
		}
		want := map[string]bool{seed.Planning2: true, seed.Newsletter: true, seed.LongRead: true}
		if res.Returned != 3 {
			t.Fatalf("returned %d: %v", res.Returned, ids(res.Emails))
		}
		for _, e := range res.Emails {
			if !want[e.ID] {
				t.Errorf("unexpected unread result %s (%s)", e.ID, e.Subject)
			}
			if e.Preview == "" {
				t.Errorf("summary %s missing preview", e.ID)
			}
		}
	})

	t.Run("from across mailboxes", func(t *testing.T) {
		res, err := svc.Search(ctx, mail.SearchParams{From: "alice"})
		if err != nil {
			t.Fatal(err)
		}
		if res.Returned != 4 { // welcome, planning 1+3, archived
			t.Errorf("returned %d: %v", res.Returned, ids(res.Emails))
		}
	})

	t.Run("flagged keyword", func(t *testing.T) {
		res, err := svc.Search(ctx, mail.SearchParams{Keyword: "$flagged"})
		if err != nil {
			t.Fatal(err)
		}
		if res.Returned != 1 || res.Emails[0].ID != seed.Invoice {
			t.Errorf("results = %v", ids(res.Emails))
		}
		if !res.Emails[0].HasAttachment {
			t.Error("invoice should report hasAttachment")
		}
	})

	t.Run("attachments in inbox", func(t *testing.T) {
		hasAtt := true
		res, err := svc.Search(ctx, mail.SearchParams{Mailbox: "inbox", HasAttachment: &hasAtt})
		if err != nil {
			t.Fatal(err)
		}
		if res.Returned != 2 { // planning3, invoice
			t.Errorf("returned %d: %v", res.Returned, ids(res.Emails))
		}
	})

	t.Run("date window", func(t *testing.T) {
		res, err := svc.Search(ctx, mail.SearchParams{After: "2026-06-09", Before: "2026-06-13"})
		if err != nil {
			t.Fatal(err)
		}
		if res.Returned != 2 { // invoice (06-10), newsletter (06-12)
			t.Errorf("returned %d: %v", res.Returned, ids(res.Emails))
		}
	})

	t.Run("collapse threads", func(t *testing.T) {
		res, err := svc.Search(ctx, mail.SearchParams{Mailbox: "Inbox", CollapseThreads: true})
		if err != nil {
			t.Fatal(err)
		}
		if res.Returned != 5 { // 7 emails, planning thread collapses 3→1
			t.Errorf("returned %d: %v", res.Returned, ids(res.Emails))
		}
	})

	t.Run("sort and paging", func(t *testing.T) {
		res, err := svc.Search(ctx, mail.SearchParams{Mailbox: "inbox", Sort: "oldest", Position: 2, Limit: 2})
		if err != nil {
			t.Fatal(err)
		}
		want := []string{seed.Planning2, seed.Planning3} // oldest order: welcome, p1, [p2, p3], ...
		got := ids(res.Emails)
		if len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
			t.Errorf("page = %v, want %v", got, want)
		}

		res, err = svc.Search(ctx, mail.SearchParams{Mailbox: "inbox", Limit: 1})
		if err != nil {
			t.Fatal(err)
		}
		if res.Emails[0].ID != seed.LongRead { // newest first by default
			t.Errorf("newest = %s, want %s", res.Emails[0].ID, seed.LongRead)
		}
	})

	t.Run("mailbox annotation and text search", func(t *testing.T) {
		res, err := svc.Search(ctx, mail.SearchParams{Text: "retro"})
		if err != nil {
			t.Fatal(err)
		}
		if res.Returned != 1 || res.Emails[0].ID != seed.Archived {
			t.Fatalf("results = %v", ids(res.Emails))
		}
		mbs := res.Emails[0].Mailboxes
		if len(mbs) != 1 || mbs[0].Role != "archive" || mbs[0].Name != "Archive" {
			t.Errorf("mailbox annotation = %+v", mbs)
		}
	})

	t.Run("unknown mailbox", func(t *testing.T) {
		_, err := svc.Search(ctx, mail.SearchParams{Mailbox: "Spam"})
		if err == nil || !strings.Contains(err.Error(), "no mailbox matches") {
			t.Errorf("err = %v", err)
		}
	})

	// The bulk-organization shape, against a real server: identical cohort,
	// ids only, and no Email/get at all.
	t.Run("id-only projection", func(t *testing.T) {
		full, err := svc.Search(ctx, mail.SearchParams{Mailbox: "inbox", Limit: 10})
		if err != nil {
			t.Fatal(err)
		}
		projected, err := svc.Search(ctx, mail.SearchParams{Mailbox: "inbox", Limit: 10, Fields: []string{"id"}})
		if err != nil {
			t.Fatal(err)
		}
		// The projected form carries its ids flat, in IDs, not as one object
		// per id in Emails.
		if got, want := strings.Join(projected.IDs, ","), strings.Join(ids(full.Emails), ","); got != want {
			t.Fatalf("projected cohort = %s, want %s", got, want)
		}
		if len(projected.Emails) != 0 {
			t.Errorf("projected result still carries %d email objects", len(projected.Emails))
		}
		fullBytes, projectedBytes := payloadSize(t, full), payloadSize(t, projected)
		if projectedBytes >= fullBytes/4 {
			t.Errorf("projected payload %d bytes vs full %d — expected a fraction of it", projectedBytes, fullBytes)
		}
		// Nothing but ids may reach the caller — assert against the rendered
		// payload, since there are no summary objects left to inspect.
		for _, leaked := range []string{"subject", "preview", "from", "mailboxes", "receivedAt"} {
			if strings.Contains(payloadOf(t, projected), `"`+leaked+`"`) {
				t.Errorf("projection leaked %s: %s", leaked, payloadOf(t, projected))
			}
		}
	})

	t.Run("hasMore and includeTotal across pages", func(t *testing.T) {
		res, err := svc.Search(ctx, mail.SearchParams{Mailbox: "inbox", Limit: 5, IncludeTotal: true})
		if err != nil {
			t.Fatal(err)
		}
		if !res.HasMore || res.Returned != 5 || res.Total == nil || *res.Total != 7 {
			t.Fatalf("page 1 = returned %d hasMore %v total %v, want 5/true/7", res.Returned, res.HasMore, res.Total)
		}
		res, err = svc.Search(ctx, mail.SearchParams{Mailbox: "inbox", Limit: 5, Position: 5})
		if err != nil {
			t.Fatal(err)
		}
		if res.HasMore || res.Returned != 2 || res.Total != nil {
			t.Errorf("page 2 = returned %d hasMore %v total %v, want 2/false/nil", res.Returned, res.HasMore, res.Total)
		}
	})

	t.Run("filterJson operator tree", func(t *testing.T) {
		// OR of two senders — the shape of a Fastmail jmapquery preview.
		res, err := svc.Search(ctx, mail.SearchParams{
			FilterJSON: []byte(`{"operator":"OR","conditions":[{"from":"billing@vendor.example.test"},{"text":"retro"}]}`),
		})
		if err != nil {
			t.Fatal(err)
		}
		want := map[string]bool{seed.Invoice: true, seed.Archived: true}
		if res.Returned != 2 || !want[res.Emails[0].ID] || !want[res.Emails[1].ID] {
			t.Fatalf("results = %v", ids(res.Emails))
		}

		// Mailbox restriction ANDs in: only the inbox half survives.
		res, err = svc.Search(ctx, mail.SearchParams{
			Mailbox:    "inbox",
			FilterJSON: []byte(`{"operator":"OR","conditions":[{"from":"billing@vendor.example.test"},{"text":"retro"}]}`),
		})
		if err != nil {
			t.Fatal(err)
		}
		if res.Returned != 1 || res.Emails[0].ID != seed.Invoice {
			t.Errorf("results = %v", ids(res.Emails))
		}

		// A NOT tree, nested one level.
		res, err = svc.Search(ctx, mail.SearchParams{
			Mailbox:    "inbox",
			FilterJSON: []byte(`{"operator":"NOT","conditions":[{"hasKeyword":"$seen"}]}`),
		})
		if err != nil {
			t.Fatal(err)
		}
		if res.Returned != 3 { // matches the notKeyword unread search above
			t.Errorf("NOT unread returned %d: %v", res.Returned, ids(res.Emails))
		}
	})
}

func TestIntegrationGet(t *testing.T) {
	seed, svc := startStack(t, 0)
	ctx := context.Background()

	t.Run("text body with attachments", func(t *testing.T) {
		res, err := svc.Get(ctx, mail.GetParams{IDs: []string{seed.Invoice, "msg-nope"}})
		if err != nil {
			t.Fatal(err)
		}
		if len(res.Emails) != 1 || len(res.NotFound) != 1 || res.NotFound[0] != "msg-nope" {
			t.Fatalf("res = %+v", res)
		}
		e := res.Emails[0]
		if !strings.Contains(e.BodyText, "invoice #42") || e.BodyTextTruncated {
			t.Errorf("body = %q truncated=%v", e.BodyText, e.BodyTextTruncated)
		}
		if len(e.Attachments) != 1 || e.Attachments[0].Name != "invoice-42.pdf" {
			t.Errorf("attachments = %+v", e.Attachments)
		}
	})

	t.Run("server-side truncation is UTF-8 safe", func(t *testing.T) {
		res, err := svc.Get(ctx, mail.GetParams{IDs: []string{seed.LongRead}, MaxBodyBytes: 100})
		if err != nil {
			t.Fatal(err)
		}
		e := res.Emails[0]
		if !e.BodyTextTruncated {
			t.Error("20KB body under a 100-byte budget must report truncation")
		}
		if len(e.BodyText) > 100 || !utf8.ValidString(e.BodyText) {
			t.Errorf("body: %d bytes valid=%v", len(e.BodyText), utf8.ValidString(e.BodyText))
		}
	})

	t.Run("config budget caps the request", func(t *testing.T) {
		seedSmall, svcSmall := startStack(t, 150) // fresh stack with a small configured cap
		got, err := svcSmall.Get(ctx, mail.GetParams{IDs: []string{seedSmall.LongRead}, MaxBodyBytes: 999999})
		if err != nil {
			t.Fatal(err)
		}
		if n := len(got.Emails[0].BodyText); n > 150 {
			t.Errorf("config cap ignored: %d bytes", n)
		}
		if !got.Emails[0].BodyTextTruncated {
			t.Error("want truncation under config cap")
		}
	})

	t.Run("previews redact unconditionally", func(t *testing.T) {
		res, err := svc.Search(ctx, mail.SearchParams{From: "news@list.example.test"})
		if err != nil {
			t.Fatal(err)
		}
		if res.Returned != 1 {
			t.Fatalf("results = %v", ids(res.Emails))
		}
		p := res.Emails[0].Preview
		if strings.Contains(p, "s3cr3tT0ken12345") {
			t.Errorf("summary preview leaked a token: %q", p)
		}
		if !strings.Contains(p, "news.example.test") {
			t.Errorf("preview should keep hosts: %q", p)
		}
	})

	t.Run("URL redaction defaults on", func(t *testing.T) {
		res, err := svc.Get(ctx, mail.GetParams{IDs: []string{seed.Newsletter}})
		if err != nil {
			t.Fatal(err)
		}
		e := res.Emails[0]
		if strings.Contains(e.BodyText, "s3cr3tT0ken12345") || strings.Contains(e.BodyText, "dXNlcjEyM3NlY3JldDQ1Njc4") {
			t.Errorf("tokens leaked: %q", e.BodyText)
		}
		if !strings.Contains(e.BodyText, "news.example.test") {
			t.Errorf("host should survive redaction: %q", e.BodyText)
		}
		if e.RedactedURLs != 2 {
			t.Errorf("redactedUrls = %d, want 2", e.RedactedURLs)
		}

		full, err := svc.Get(ctx, mail.GetParams{IDs: []string{seed.Newsletter}, IncludeFullUrls: true})
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(full.Emails[0].BodyText, "token=s3cr3tT0ken12345") || full.Emails[0].RedactedURLs != 0 {
			t.Errorf("includeFullUrls must return intact urls: %+v", full.Emails[0].RedactedURLs)
		}
	})

	t.Run("thread bodies redact too", func(t *testing.T) {
		th, err := svc.GetThread(ctx, mail.ThreadParams{EmailID: seed.Newsletter, IncludeBodies: true})
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(th.Full[0].BodyText, "s3cr3tT0ken12345") {
			t.Errorf("thread body leaked token")
		}
	})

	t.Run("metadata format", func(t *testing.T) {
		res, err := svc.Get(ctx, mail.GetParams{IDs: []string{seed.Invoice}, BodyFormat: mail.BodyMetadata})
		if err != nil {
			t.Fatal(err)
		}
		e := res.Emails[0]
		if e.BodyText != "" || e.BodyHTML != "" {
			t.Errorf("metadata fetched bodies: %q %q", e.BodyText, e.BodyHTML)
		}
		if e.Subject == "" || len(e.Attachments) != 1 {
			t.Errorf("metadata missing fields: %+v", e)
		}
	})

	t.Run("both formats", func(t *testing.T) {
		res, err := svc.Get(ctx, mail.GetParams{IDs: []string{seed.LongRead}, BodyFormat: mail.BodyBoth, MaxBodyBytes: 5000})
		if err != nil {
			t.Fatal(err)
		}
		e := res.Emails[0]
		if !strings.Contains(e.BodyHTML, "<p>") {
			t.Errorf("html body = %q", e.BodyHTML)
		}
		if e.BodyText == "" {
			t.Error("text body missing in both-format fetch")
		}
	})
}

func TestIntegrationThread(t *testing.T) {
	seed, svc := startStack(t, 0)
	ctx := context.Background()

	th, err := svc.GetThread(ctx, mail.ThreadParams{EmailID: seed.Planning2})
	if err != nil {
		t.Fatal(err)
	}
	if th.ThreadID != seed.PlanningThreadID || th.Count != 3 {
		t.Fatalf("thread = %+v", th)
	}
	if th.Emails[0].ID != seed.Planning1 || th.Emails[2].ID != seed.Planning3 {
		t.Errorf("thread order = %v", ids(th.Emails))
	}

	full, err := svc.GetThread(ctx, mail.ThreadParams{ThreadID: seed.PlanningThreadID, IncludeBodies: true, MaxBodyBytes: 2000})
	if err != nil {
		t.Fatal(err)
	}
	if len(full.Full) != 3 || full.Full[0].BodyText == "" {
		t.Errorf("bodies = %+v", full.Full)
	}

	if _, err := svc.GetThread(ctx, mail.ThreadParams{ThreadID: "thr-nope"}); err == nil {
		t.Error("want error for unknown thread")
	}
}
