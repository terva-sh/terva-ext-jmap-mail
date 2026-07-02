package jmap

// Spec-compliance tests: each case pins our wire behavior to a specific
// RFC 8620 / RFC 8621 requirement, cited inline, so drift from the standard is
// auditable. Fixtures are modeled on the RFCs' own examples.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
)

// RFC 8620 §2: capability URNs are exact registered strings.
func TestSpecCapabilityURNs(t *testing.T) {
	want := map[string]string{
		CapCore:             "urn:ietf:params:jmap:core",
		CapMail:             "urn:ietf:params:jmap:mail",
		CapSubmission:       "urn:ietf:params:jmap:submission",
		CapVacationResponse: "urn:ietf:params:jmap:vacationresponse",
	}
	for got, expect := range want {
		if got != expect {
			t.Errorf("capability constant %q != %q", got, expect)
		}
	}
}

// RFC 8620 §3.2: a method call serializes as the [name, arguments, callId]
// triple, with arguments always an object (never null).
func TestSpecInvocationTriple(t *testing.T) {
	b, err := json.Marshal(Invocation{Name: "Email/get", Args: map[string]any{"accountId": "A1"}, CallID: "c0"})
	if err != nil {
		t.Fatal(err)
	}
	var arr []json.RawMessage
	if err := json.Unmarshal(b, &arr); err != nil || len(arr) != 3 {
		t.Fatalf("not a 3-element array: %s", b)
	}
	if string(arr[0]) != `"Email/get"` || string(arr[2]) != `"c0"` {
		t.Errorf("triple order wrong: %s", b)
	}

	b, _ = json.Marshal(Invocation{Name: "Core/echo", CallID: "c1"}) // nil Args
	json.Unmarshal(b, &arr)
	if string(arr[1]) != "{}" {
		t.Errorf("nil args must serialize as {}: %s", b)
	}
}

// RFC 8620 §3.3: the request object carries `using` (capability URIs) and
// ordered `methodCalls`; the API request is a POST with a JSON content type.
func TestSpecRequestEnvelope(t *testing.T) {
	var captured struct {
		method, contentType string
		body                map[string]json.RawMessage
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		captured.method = r.Method
		captured.contentType = r.Header.Get("Content-Type")
		json.NewDecoder(r.Body).Decode(&captured.body)
		w.Write([]byte(`{"methodResponses": []}`))
	}))
	defer srv.Close()

	calls := []Invocation{
		{Name: "Email/query", Args: map[string]any{"accountId": "A1"}, CallID: "q0"},
		{Name: "Email/get", Args: map[string]any{"accountId": "A1"}, CallID: "g1"},
	}
	if _, err := c(srv).Call(context.Background(), srv.URL, []string{CapCore, CapMail}, calls); err != nil {
		t.Fatal(err)
	}
	if captured.method != http.MethodPost {
		t.Errorf("method = %q, want POST", captured.method)
	}
	if captured.contentType != "application/json" {
		t.Errorf("Content-Type = %q", captured.contentType)
	}
	var using []string
	json.Unmarshal(captured.body["using"], &using)
	if !reflect.DeepEqual(using, []string{CapCore, CapMail}) {
		t.Errorf("using = %v", using)
	}
	var mc []json.RawMessage
	json.Unmarshal(captured.body["methodCalls"], &mc)
	if len(mc) != 2 {
		t.Fatalf("methodCalls count = %d", len(mc))
	}
	var first InvocationResult
	if err := json.Unmarshal(mc[0], &first); err != nil || first.Name != "Email/query" || first.CallID != "q0" {
		t.Errorf("methodCalls[0] = %s (err %v)", mc[0], err)
	}
}

// RFC 8620 §3.7: a result reference replaces an argument with a
// {resultOf, name, path} object under the "#"-prefixed argument name.
func TestSpecResultReference(t *testing.T) {
	args := map[string]any{
		"accountId": "A1",
		"#ids":      ResultReference{ResultOf: "q0", Name: "Email/query", Path: "/ids"},
	}
	b, err := json.Marshal(Invocation{Name: "Email/get", Args: args, CallID: "g1"})
	if err != nil {
		t.Fatal(err)
	}
	var arr []json.RawMessage
	json.Unmarshal(b, &arr)
	var decoded map[string]json.RawMessage
	json.Unmarshal(arr[1], &decoded)
	if _, dup := decoded["ids"]; dup {
		t.Error("both ids and #ids present — the spec forbids sending both")
	}
	var ref map[string]string
	if err := json.Unmarshal(decoded["#ids"], &ref); err != nil {
		t.Fatalf("#ids not an object: %s", decoded["#ids"])
	}
	want := map[string]string{"resultOf": "q0", "name": "Email/query", "path": "/ids"}
	if !reflect.DeepEqual(ref, want) {
		t.Errorf("#ids = %v, want %v", ref, want)
	}
}

// RFC 8620 §1.4 (UTCDate): date-times passed to the server are UTC with the
// Z suffix (e.g. Email/query filter before/after per RFC 8621 §4.4.1).
func TestSpecUTCDate(t *testing.T) {
	cases := []struct {
		in, want string
		ok       bool
	}{
		{"2026-07-01T12:30:00Z", "2026-07-01T12:30:00Z", true},
		{"2026-07-01T12:30:00+02:00", "2026-07-01T10:30:00Z", true},
		{"2026-07-01", "2026-07-01T00:00:00Z", true},
		{"yesterday", "", false},
		{"2026-13-99", "", false},
	}
	for _, c := range cases {
		got, err := ToUTCDate(c.in)
		if c.ok && (err != nil || got != c.want) {
			t.Errorf("ToUTCDate(%q) = %q, %v; want %q", c.in, got, err, c.want)
		}
		if !c.ok && err == nil {
			t.Errorf("ToUTCDate(%q) should fail", c.in)
		}
	}
}

// RFC 8620 §3.6.1: a request-level error is an RFC 7807 problem-details
// object whose type is a urn:ietf:params:jmap:error:* URN.
func TestSpecRequestErrorTypes(t *testing.T) {
	for _, typ := range []string{
		"urn:ietf:params:jmap:error:unknownCapability",
		"urn:ietf:params:jmap:error:notJSON",
		"urn:ietf:params:jmap:error:notRequest",
		"urn:ietf:params:jmap:error:limit",
	} {
		body, _ := json.Marshal(map[string]any{"type": typ, "status": 400})
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/problem+json")
			w.WriteHeader(http.StatusBadRequest)
			w.Write(body)
		}))
		_, err := c(srv).Call(context.Background(), srv.URL, []string{CapCore}, []Invocation{{Name: "Core/echo", CallID: "c0"}})
		srv.Close()
		re, ok := err.(*RequestError)
		if !ok || re.Type != typ {
			t.Errorf("type %s: err = %v, want RequestError of that type", typ, err)
		}
	}
}

// RFC 8620 §2 example: the session resource fixture (accounts,
// primaryAccounts, capabilities object with limits) parses into our Session.
// The fixture in client_test.go mirrors the RFC's own example values.
func TestSpecSessionResourceShape(t *testing.T) {
	var s Session
	if err := json.Unmarshal([]byte(sessionFixture), &s); err != nil {
		t.Fatalf("session fixture: %v", err)
	}
	if len(s.Accounts) != 1 || s.Accounts["A1"].Name != "user@example.com" {
		t.Errorf("accounts parsed wrong: %+v", s.Accounts)
	}
	if !s.Accounts["A1"].IsPersonal || s.Accounts["A1"].IsReadOnly {
		t.Errorf("account flags parsed wrong: %+v", s.Accounts["A1"])
	}
	if s.State != "75128aab4b1b" {
		t.Errorf("state = %q", s.State)
	}
}
