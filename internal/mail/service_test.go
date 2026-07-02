package mail

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"terva-ext-jmap-mail/internal/config"
	"terva-ext-jmap-mail/internal/jmap"
)

// fake implements Caller with a per-test dispatch function and records every
// request for envelope assertions.
type fake struct {
	session        *jmap.Session
	sessionErr     error
	sessionFetches int
	handler        func(calls []jmap.Invocation) *jmap.Response
	recorded       [][]jmap.Invocation
	lastUsing      []string
}

func (f *fake) FetchSession(_ context.Context) (*jmap.Session, error) {
	f.sessionFetches++
	if f.sessionErr != nil {
		return nil, f.sessionErr
	}
	return f.session, nil
}

func (f *fake) Call(_ context.Context, _ string, using []string, calls []jmap.Invocation) (*jmap.Response, error) {
	f.recorded = append(f.recorded, calls)
	f.lastUsing = using
	return f.handler(calls), nil
}

func testSession() *jmap.Session {
	mailCap := map[string]json.RawMessage{jmap.CapMail: json.RawMessage(`{}`)}
	return &jmap.Session{
		Capabilities: map[string]json.RawMessage{
			jmap.CapCore: json.RawMessage(`{"maxCallsInRequest": 16, "maxObjectsInGet": 500}`),
			jmap.CapMail: json.RawMessage(`{}`),
		},
		Accounts: map[string]jmap.Account{
			"A1": {Name: "user@example.com", IsPersonal: true, AccountCapabilities: mailCap},
			"A2": {Name: "Shared", AccountCapabilities: map[string]json.RawMessage{}},
		},
		PrimaryAccounts: map[string]string{jmap.CapMail: "A1"},
		Username:        "user@example.com",
		APIURL:          "https://server.example.com/api/",
	}
}

func testService(f *fake) *Service {
	if f.session == nil && f.sessionErr == nil {
		f.session = testSession()
	}
	return NewService(f, config.Normalize(config.Settings{APIToken: "tok"}))
}

// result builds one method response triple with marshaled args.
func result(name, callID string, args any) jmap.InvocationResult {
	b, err := json.Marshal(args)
	if err != nil {
		panic(err)
	}
	return jmap.InvocationResult{Name: name, Args: b, CallID: callID}
}

func response(results ...jmap.InvocationResult) *jmap.Response {
	return &jmap.Response{MethodResponses: results}
}

// findBatch returns the first recorded request whose opening call is method.
// (Mailbox annotation may lazily record a Mailbox/get before or after the
// batch under test, so position in `recorded` is not stable.)
func findBatch(t *testing.T, f *fake, method string) []jmap.Invocation {
	t.Helper()
	for _, calls := range f.recorded {
		if len(calls) > 0 && calls[0].Name == method {
			return calls
		}
	}
	t.Fatalf("no recorded batch starting with %s", method)
	return nil
}

// argsOf round-trips an invocation's Args through JSON for assertions.
func argsOf(t *testing.T, inv jmap.Invocation) map[string]any {
	t.Helper()
	b, err := json.Marshal(inv.Args)
	if err != nil {
		t.Fatalf("marshal args: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("unmarshal args: %v", err)
	}
	return m
}

func TestAccountSelection(t *testing.T) {
	cases := []struct {
		name       string
		requested  string
		cfgDefault string
		wantID     string
		wantErr    string
	}{
		{name: "primary fallback", wantID: "A1"},
		{name: "explicit id", requested: "A1", wantID: "A1"},
		{name: "by name case-insensitive", requested: "USER@EXAMPLE.COM", wantID: "A1"},
		{name: "config default", cfgDefault: "user@example.com", wantID: "A1"},
		{name: "request beats config default", requested: "A1", cfgDefault: "nope", wantID: "A1"},
		{name: "no mail capability", requested: "A2", wantErr: "does not support mail"},
		{name: "unknown", requested: "nobody", wantErr: "no account matches"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := &fake{session: testSession()}
			s := NewService(f, config.Normalize(config.Settings{APIToken: "tok", DefaultAccount: c.cfgDefault}))
			id, _, err := s.account(context.Background(), c.requested)
			if c.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), c.wantErr) {
					t.Fatalf("err = %v, want containing %q", err, c.wantErr)
				}
				return
			}
			if err != nil || id != c.wantID {
				t.Fatalf("id = %q err = %v, want %q", id, err, c.wantID)
			}
		})
	}
}

func TestAccountAmbiguousName(t *testing.T) {
	sess := testSession()
	acct := sess.Accounts["A1"]
	sess.Accounts["A3"] = acct // same name twice
	f := &fake{session: sess}
	s := testService(f)
	_, _, err := s.account(context.Background(), "user@example.com")
	if err == nil || !strings.Contains(err.Error(), "ambiguous") {
		t.Fatalf("err = %v, want ambiguity error", err)
	}
}

func TestSessionCached(t *testing.T) {
	f := &fake{session: testSession()}
	s := testService(f)
	for i := 0; i < 3; i++ {
		if _, _, err := s.account(context.Background(), ""); err != nil {
			t.Fatal(err)
		}
	}
	if f.sessionFetches != 1 {
		t.Errorf("session fetched %d times, want 1 (cached)", f.sessionFetches)
	}
}

func TestSessionErrorPropagates(t *testing.T) {
	f := &fake{sessionErr: errors.New("authentication failed (HTTP 401)")}
	s := testService(f)
	_, err := s.Accounts(context.Background())
	if err == nil || !strings.Contains(err.Error(), "401") {
		t.Fatalf("err = %v", err)
	}
}

func TestAccountInfos(t *testing.T) {
	infos := accountInfos(testSession())
	if len(infos) != 2 {
		t.Fatalf("got %d accounts", len(infos))
	}
	if infos[0].ID != "A1" || !infos[0].SupportsMail || !infos[0].IsPrimaryMail || infos[0].SupportsSubmission {
		t.Errorf("A1 info wrong: %+v", infos[0])
	}
	// Capability URNs are surfaced verbatim so vendor/newer-standard
	// capabilities (e.g. urn:ietf:params:jmap:sieve) are discoverable.
	if len(infos[0].CapabilityUrns) != 1 || infos[0].CapabilityUrns[0] != jmap.CapMail {
		t.Errorf("A1 capabilityUrns = %v", infos[0].CapabilityUrns)
	}
	if infos[1].ID != "A2" || infos[1].SupportsMail {
		t.Errorf("A2 info wrong: %+v", infos[1])
	}
}

func TestStatusReportsShape(t *testing.T) {
	f := &fake{session: testSession()}
	s := testService(f)
	st, err := s.Status(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !st.Configured || st.AccountID != "A1" || st.Username != "user@example.com" {
		t.Errorf("status = %+v", st)
	}
	if st.Limits == nil || st.Limits.MaxCallsInRequest != 16 {
		t.Errorf("limits = %+v", st.Limits)
	}
	if len(st.Capabilities) != 2 {
		t.Errorf("capabilities = %v", st.Capabilities)
	}
}

// The envelope of every mail call must declare core+mail capabilities
// (RFC 8620 §3.3, RFC 8621 §1.3).
func TestCallUsesMailCapabilities(t *testing.T) {
	f := &fake{}
	f.handler = func(calls []jmap.Invocation) *jmap.Response {
		return response(result("Mailbox/get", "m0", map[string]any{"list": []any{}}))
	}
	s := testService(f)
	if _, err := s.ListMailboxes(context.Background(), ListMailboxesParams{}); err != nil {
		t.Fatal(err)
	}
	if len(f.lastUsing) != 2 || f.lastUsing[0] != jmap.CapCore || f.lastUsing[1] != jmap.CapMail {
		t.Errorf("using = %v", f.lastUsing)
	}
}
