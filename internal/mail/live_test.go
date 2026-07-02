//go:build live

package mail

// Live, READ-ONLY tests against a real JMAP provider. Opt-in only — never run
// by `just test` / CI:
//
//	JMAP_TEST_SESSION_URL=https://api.fastmail.com/jmap/session \
//	JMAP_TEST_API_TOKEN=... [JMAP_TEST_ACCOUNT=...] just test-live
//
// These tests list and read bounded data only; nothing mutates. Phase 2 will
// add mutation tests gated on JMAP_TEST_SAFE_MAILBOX +
// JMAP_TEST_ALLOW_DESTRUCTIVE=1 against a dedicated test mailbox.

import (
	"context"
	"os"
	"testing"
	"time"

	"terva-ext-jmap-mail/internal/config"
	"terva-ext-jmap-mail/internal/jmap"
)

func liveService(t *testing.T) *Service {
	t.Helper()
	url := os.Getenv("JMAP_TEST_SESSION_URL")
	token := os.Getenv("JMAP_TEST_API_TOKEN")
	if url == "" || token == "" {
		t.Skip("set JMAP_TEST_SESSION_URL and JMAP_TEST_API_TOKEN to run live tests")
	}
	cfg := config.Normalize(config.Settings{
		SessionURL:     url,
		APIToken:       token,
		DefaultAccount: os.Getenv("JMAP_TEST_ACCOUNT"),
		MaxBodyBytes:   500, // keep live fetches tiny
	})
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	return NewService(jmap.NewClient(cfg.SessionURL, cfg.APIToken), cfg)
}

func liveCtx(t *testing.T) context.Context {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func TestLiveStatus(t *testing.T) {
	s := liveService(t)
	st, err := s.Status(liveCtx(t))
	if err != nil {
		t.Fatal(err)
	}
	if st.AccountID == "" {
		t.Fatalf("no account selected: %+v", st)
	}
	t.Logf("account %s (%s), %d capabilities", st.AccountID, st.AccountName, len(st.Capabilities))
}

func TestLiveListMailboxes(t *testing.T) {
	s := liveService(t)
	list, err := s.ListMailboxes(liveCtx(t), ListMailboxesParams{IncludeCounts: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(list.Mailboxes) == 0 {
		t.Fatal("no mailboxes")
	}
	var haveInbox bool
	for _, mb := range list.Mailboxes {
		if mb.Role == "inbox" {
			haveInbox = true
		}
	}
	if !haveInbox {
		t.Error("no mailbox with role inbox")
	}
	t.Logf("%d mailboxes", len(list.Mailboxes))
}

func TestLiveSearchAndGet(t *testing.T) {
	s := liveService(t)
	ctx := liveCtx(t)
	res, err := s.Search(ctx, SearchParams{Mailbox: "inbox", Limit: 5})
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("search returned %d summaries", res.Returned)
	if res.Returned == 0 {
		t.Skip("inbox is empty; nothing to fetch")
	}
	if res.Emails[0].Preview == "" {
		t.Log("note: first result has no preview (provider may omit)")
	}

	got, err := s.Get(ctx, GetParams{IDs: []string{res.Emails[0].ID}, MaxBodyBytes: 300})
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Emails) != 1 {
		t.Fatalf("get returned %d emails", len(got.Emails))
	}
	if n := len(got.Emails[0].BodyText); n > 500 {
		t.Errorf("body not bounded: %d bytes", n)
	}
	t.Logf("fetched %q (%d body bytes, truncated=%v)",
		got.Emails[0].Subject, len(got.Emails[0].BodyText), got.Emails[0].BodyTextTruncated)
}

func TestLiveThread(t *testing.T) {
	s := liveService(t)
	ctx := liveCtx(t)
	res, err := s.Search(ctx, SearchParams{Mailbox: "inbox", Limit: 1})
	if err != nil {
		t.Fatal(err)
	}
	if res.Returned == 0 {
		t.Skip("inbox is empty")
	}
	th, err := s.GetThread(ctx, ThreadParams{EmailID: res.Emails[0].ID})
	if err != nil {
		t.Fatal(err)
	}
	if th.Count == 0 {
		t.Fatal("thread has no messages")
	}
	t.Logf("thread %s has %d message(s)", th.ThreadID, th.Count)
}
