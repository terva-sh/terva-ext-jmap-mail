package jmap

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const sessionFixture = `{
  "capabilities": {
    "urn:ietf:params:jmap:core": {
      "maxSizeUpload": 50000000,
      "maxConcurrentUpload": 4,
      "maxSizeRequest": 10000000,
      "maxConcurrentRequests": 4,
      "maxCallsInRequest": 16,
      "maxObjectsInGet": 500,
      "maxObjectsInSet": 500,
      "collationAlgorithms": ["i;ascii-numeric"]
    },
    "urn:ietf:params:jmap:mail": {}
  },
  "accounts": {
    "A1": {
      "name": "user@example.com",
      "isPersonal": true,
      "isReadOnly": false,
      "accountCapabilities": {
        "urn:ietf:params:jmap:mail": {"maxMailboxDepth": 10}
      }
    }
  },
  "primaryAccounts": {"urn:ietf:params:jmap:mail": "A1"},
  "username": "user@example.com",
  "apiUrl": "https://server.example.com/api/",
  "downloadUrl": "https://server.example.com/download/{accountId}/{blobId}/{name}?accept={type}",
  "uploadUrl": "https://server.example.com/upload/{accountId}/",
  "eventSourceUrl": "https://server.example.com/eventsource/",
  "state": "75128aab4b1b"
}`

func TestFetchSession(t *testing.T) {
	var gotAuth, gotAccept string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotAccept = r.Header.Get("Accept")
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(sessionFixture))
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "test-token")
	s, err := c.FetchSession(context.Background())
	if err != nil {
		t.Fatalf("FetchSession: %v", err)
	}
	if gotAuth != "Bearer test-token" {
		t.Errorf("Authorization = %q, want Bearer test-token", gotAuth)
	}
	if gotAccept != "application/json" {
		t.Errorf("Accept = %q", gotAccept)
	}
	if s.Username != "user@example.com" || s.APIURL != "https://server.example.com/api/" {
		t.Errorf("session parsed wrong: %+v", s)
	}
	if s.PrimaryAccounts[CapMail] != "A1" {
		t.Errorf("primaryAccounts[mail] = %q, want A1", s.PrimaryAccounts[CapMail])
	}
	acct, ok := s.Accounts["A1"]
	if !ok || !acct.HasCapability(CapMail) || acct.HasCapability(CapSubmission) {
		t.Errorf("account A1 parsed wrong: %+v ok=%v", acct, ok)
	}
	limits, ok := s.CoreLimits()
	if !ok || limits.MaxCallsInRequest != 16 || limits.MaxObjectsInGet != 500 || limits.MaxSizeRequest != 10000000 {
		t.Errorf("CoreLimits = %+v ok=%v", limits, ok)
	}
}

func TestFetchSessionAuthError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()

	_, err := c(srv).FetchSession(context.Background())
	var ae *AuthError
	if !errors.As(err, &ae) || ae.StatusCode != 401 {
		t.Fatalf("err = %v, want AuthError 401", err)
	}
	if strings.Contains(err.Error(), "secret-token") {
		t.Errorf("error leaks token: %v", err)
	}
}

func TestFetchSessionNotJMAP(t *testing.T) {
	cases := map[string]string{
		"html":    `<html>login page</html>`,
		"no-core": `{"capabilities": {"urn:example:other": {}}, "apiUrl": "https://x/api"}`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Write([]byte(body))
			}))
			defer srv.Close()
			if _, err := c(srv).FetchSession(context.Background()); err == nil {
				t.Fatal("want error for non-JMAP session resource")
			}
		})
	}
}

func TestCallRequestLevelError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/problem+json")
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(`{"type": "urn:ietf:params:jmap:error:limit", "limit": "maxSizeRequest", "status": 400, "detail": "The request is larger than the server is willing to process."}`))
	}))
	defer srv.Close()

	_, err := c(srv).Call(context.Background(), srv.URL, []string{CapCore}, []Invocation{{Name: "Core/echo", CallID: "c0"}})
	var re *RequestError
	if !errors.As(err, &re) {
		t.Fatalf("err = %T %v, want RequestError", err, err)
	}
	if re.Type != "urn:ietf:params:jmap:error:limit" || re.Limit != "maxSizeRequest" {
		t.Errorf("RequestError = %+v", re)
	}
}

func TestCallHTTPErrorSnippetBounded(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		w.Write([]byte(strings.Repeat("x", 5000)))
	}))
	defer srv.Close()

	_, err := c(srv).Call(context.Background(), srv.URL, []string{CapCore}, []Invocation{{Name: "Core/echo", CallID: "c0"}})
	var he *HTTPError
	if !errors.As(err, &he) || he.StatusCode != 502 {
		t.Fatalf("err = %v, want HTTPError 502", err)
	}
	if len(err.Error()) > 300 {
		t.Errorf("error text not bounded: %d bytes", len(err.Error()))
	}
}

func TestCallMethodLevelError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"methodResponses": [["error", {"type": "unknownMethod"}, "c0"]], "sessionState": "s"}`))
	}))
	defer srv.Close()

	resp, err := c(srv).Call(context.Background(), srv.URL, []string{CapCore}, []Invocation{{Name: "Bogus/get", CallID: "c0"}})
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	_, err = resp.Result("c0")
	var me *MethodError
	if !errors.As(err, &me) || me.Type != "unknownMethod" || me.CallID != "c0" {
		t.Fatalf("err = %v, want MethodError unknownMethod", err)
	}
}

func TestResultMissingCall(t *testing.T) {
	r := &Response{}
	if _, err := r.Result("nope"); err == nil {
		t.Fatal("want error for missing call id")
	}
}

func c(srv *httptest.Server) *Client {
	cl := NewClient(srv.URL, "secret-token")
	cl.HTTPClient = srv.Client()
	return cl
}

func TestCallParsesResponses(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"methodResponses": [["Email/query", {"ids": ["e1", "e2"], "position": 0}, "q0"]], "sessionState": "abc"}`))
	}))
	defer srv.Close()

	resp, err := c(srv).Call(context.Background(), srv.URL, []string{CapCore, CapMail}, []Invocation{{Name: "Email/query", Args: map[string]any{"accountId": "A1"}, CallID: "q0"}})
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	res, err := resp.Result("q0")
	if err != nil {
		t.Fatalf("Result: %v", err)
	}
	var out struct {
		IDs []string `json:"ids"`
	}
	if err := json.Unmarshal(res.Args, &out); err != nil || len(out.IDs) != 2 {
		t.Errorf("args parse: %v %+v", err, out)
	}
	if resp.SessionState != "abc" {
		t.Errorf("sessionState = %q", resp.SessionState)
	}
}

// apiUrl comes from the server, not the user — the https-or-loopback policy
// must apply to it too, before any request carries the token.
func TestCallRejectsNonLoopbackHTTPAPIURL(t *testing.T) {
	c := NewClient("https://mail.example.test/session", "tok")
	_, err := c.Call(context.Background(), "http://mail.example.test/jmap", []string{CapCore}, []Invocation{{Name: "Core/echo", Args: map[string]any{}, CallID: "c0"}})
	if err == nil || !strings.Contains(err.Error(), "plain http") {
		t.Fatalf("err = %v, want http apiUrl refusal", err)
	}
	if strings.Contains(err.Error(), "tok") {
		t.Errorf("error leaks token: %v", err)
	}
}

// A redirect to a non-https, non-loopback target must abort before the
// transport forwards the Authorization header (Go keeps it on same-host
// redirects even across an https→http downgrade).
func TestRedirectDowngradeRefused(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "http://mail.example.test/jmap", http.StatusFound)
	}))
	defer srv.Close()
	c := NewClient(srv.URL+"/session", "tok")
	_, err := c.FetchSession(context.Background())
	if err == nil || !strings.Contains(err.Error(), "plain http") {
		t.Fatalf("err = %v, want redirect target refusal", err)
	}
}

// Loopback http redirects (test servers) stay allowed.
func TestRedirectToLoopbackAllowed(t *testing.T) {
	var target *httptest.Server
	target = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"capabilities":{"urn:ietf:params:jmap:core":{}},"apiUrl":"` + target.URL + `/jmap","accounts":{}}`))
	}))
	defer target.Close()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+"/session", http.StatusFound)
	}))
	defer srv.Close()
	c := NewClient(srv.URL+"/session", "tok")
	if _, err := c.FetchSession(context.Background()); err != nil {
		t.Fatalf("loopback redirect refused: %v", err)
	}
}
