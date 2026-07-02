// Package mail is the provider-neutral mail service: account selection,
// mailbox listing/resolution, search, bounded fetch, and threads, expressed as
// JMAP method batches. It talks to the protocol through the Caller interface
// so unit tests never touch HTTP. Pure logic, no SDK import.
package mail

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"terva-ext-jmap-mail/internal/config"
	"terva-ext-jmap-mail/internal/jmap"
)

// Caller abstracts internal/jmap for tests.
type Caller interface {
	FetchSession(ctx context.Context) (*jmap.Session, error)
	Call(ctx context.Context, apiURL string, using []string, calls []jmap.Invocation) (*jmap.Response, error)
}

// sessionTTL bounds how long a cached session resource is trusted before
// re-fetching (accounts and capabilities change rarely).
const sessionTTL = 15 * time.Minute

// Service executes mail operations against one configured provider. Rebuilt
// by the glue layer whenever config changes, which naturally drops all caches.
type Service struct {
	client Caller
	cfg    config.Settings

	mu        sync.Mutex
	session   *jmap.Session
	sessionAt time.Time
	mailboxes map[string]mailboxCache // accountID → last fetched list
}

type mailboxCache struct {
	list      []Mailbox
	fetchedAt time.Time
}

// NewService wires a service over a protocol client and validated settings.
func NewService(client Caller, cfg config.Settings) *Service {
	return &Service{client: client, cfg: cfg, mailboxes: map[string]mailboxCache{}}
}

// getSession returns the cached session resource, fetching when absent or
// stale. The lock is never held across the network call.
func (s *Service) getSession(ctx context.Context) (*jmap.Session, error) {
	s.mu.Lock()
	if s.session != nil && time.Since(s.sessionAt) < sessionTTL {
		sess := s.session
		s.mu.Unlock()
		return sess, nil
	}
	s.mu.Unlock()

	sess, err := s.client.FetchSession(ctx)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	s.session = sess
	s.sessionAt = time.Now()
	s.mu.Unlock()
	return sess, nil
}

// account resolves the acting account id: explicit request → configured
// default → the provider's primary mail account (RFC 8621 §1.3). Matching is
// by account id first, then account name (case-insensitive). The chosen
// account must advertise the mail capability.
func (s *Service) account(ctx context.Context, requested string) (string, *jmap.Session, error) {
	sess, err := s.getSession(ctx)
	if err != nil {
		return "", nil, err
	}
	want := strings.TrimSpace(requested)
	if want == "" {
		want = strings.TrimSpace(s.cfg.DefaultAccount)
	}
	if want == "" {
		id, ok := sess.PrimaryAccounts[jmap.CapMail]
		if !ok || id == "" {
			return "", nil, fmt.Errorf("provider reports no primary mail account — set default_account in /extensions (available: %s)", accountNames(sess))
		}
		want = id
	}

	if acct, ok := sess.Accounts[want]; ok {
		if !acct.HasCapability(jmap.CapMail) {
			return "", nil, fmt.Errorf("account %q does not support mail (%s)", acct.Name, jmap.CapMail)
		}
		return want, sess, nil
	}
	// Fall back to matching by account name/email.
	var matches []string
	for id, acct := range sess.Accounts {
		if strings.EqualFold(acct.Name, want) {
			matches = append(matches, id)
		}
	}
	switch len(matches) {
	case 1:
		if !sess.Accounts[matches[0]].HasCapability(jmap.CapMail) {
			return "", nil, fmt.Errorf("account %q does not support mail (%s)", want, jmap.CapMail)
		}
		return matches[0], sess, nil
	case 0:
		return "", nil, fmt.Errorf("no account matches %q (available: %s)", want, accountNames(sess))
	default:
		sort.Strings(matches)
		return "", nil, fmt.Errorf("account name %q is ambiguous (ids: %s) — use an account id", want, strings.Join(matches, ", "))
	}
}

func accountNames(sess *jmap.Session) string {
	var names []string
	for id, acct := range sess.Accounts {
		names = append(names, fmt.Sprintf("%s (%s)", acct.Name, id))
	}
	sort.Strings(names)
	if len(names) == 0 {
		return "none"
	}
	return strings.Join(names, ", ")
}

// call is the shared envelope: every mail request uses core+mail capabilities.
func (s *Service) call(ctx context.Context, sess *jmap.Session, calls []jmap.Invocation) (*jmap.Response, error) {
	return s.client.Call(ctx, sess.APIURL, []string{jmap.CapCore, jmap.CapMail}, calls)
}
