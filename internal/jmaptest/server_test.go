package jmaptest

// These tests drive the fake over raw HTTP (no client package involved) and
// pin its protocol behaviors to RFC 8620, so the fake itself stays honest.

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
)

func post(t *testing.T, s *Server, contentType, token, body string) (int, map[string]any) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, s.APIURL(), strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", contentType)
	req.Header.Set("Authorization", "Bearer "+token)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(res.Body)
	var decoded map[string]any
	json.Unmarshal(raw, &decoded)
	return res.StatusCode, decoded
}

func startSeeded(t *testing.T) (*Server, Seed) {
	t.Helper()
	s := New()
	t.Cleanup(s.Close)
	return s, s.SeedStandard()
}

const usingBoth = `["urn:ietf:params:jmap:core", "urn:ietf:params:jmap:mail"]`

// firstResponse unpacks methodResponses[0] into (name, args, callId).
func firstResponse(t *testing.T, body map[string]any) (string, map[string]any, string) {
	t.Helper()
	responses, _ := body["methodResponses"].([]any)
	if len(responses) == 0 {
		t.Fatalf("no methodResponses in %v", body)
	}
	triple, _ := responses[0].([]any)
	if len(triple) != 3 {
		t.Fatalf("response is not a triple: %v", responses[0])
	}
	name, _ := triple[0].(string)
	args, _ := triple[1].(map[string]any)
	callID, _ := triple[2].(string)
	return name, args, callID
}

func TestAuthRequired(t *testing.T) {
	s, _ := startSeeded(t)

	res, err := http.Get(s.SessionURL())
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != 401 {
		t.Errorf("session without token: %d, want 401", res.StatusCode)
	}

	status, _ := post(t, s, "application/json", "wrong-token",
		`{"using": `+usingBoth+`, "methodCalls": [["Core/echo", {}, "c0"]]}`)
	if status != 401 {
		t.Errorf("api with wrong token: %d, want 401", status)
	}
}

// RFC 8620 §3.6.1 request-level problems.
func TestRequestLevelProblems(t *testing.T) {
	s, _ := startSeeded(t)
	cases := []struct {
		name        string
		contentType string
		body        string
		wantType    string
	}{
		{"wrong content type", "text/plain", `{"using": [], "methodCalls": []}`, "notJSON"},
		{"malformed json", "application/json", `{"using": [`, "notJSON"},
		{"missing methodCalls", "application/json", `{"using": []}`, "notRequest"},
		{"bad triple", "application/json", `{"using": ` + usingBoth + `, "methodCalls": [["only-name"]]}`, "notRequest"},
		{"unknown capability", "application/json",
			`{"using": ["urn:example:bogus"], "methodCalls": [["Core/echo", {}, "c0"]]}`, "unknownCapability"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			status, body := post(t, s, c.contentType, s.Token, c.body)
			typ, _ := body["type"].(string)
			if status != 400 || typ != "urn:ietf:params:jmap:error:"+c.wantType {
				t.Errorf("status=%d type=%q, want 400 %s", status, typ, c.wantType)
			}
		})
	}
}

func TestMaxCallsLimit(t *testing.T) {
	s, _ := startSeeded(t)
	call := `["Core/echo", {}, "c"]`
	calls := call
	for range s.MaxCallsInRequest { // one over the limit
		calls += ", " + call
	}
	status, body := post(t, s, "application/json", s.Token,
		`{"using": `+usingBoth+`, "methodCalls": [`+calls+`]}`)
	if status != 400 || body["type"] != "urn:ietf:params:jmap:error:limit" || body["limit"] != "maxCallsInRequest" {
		t.Errorf("status=%d body=%v, want limit problem", status, body)
	}
}

func TestCoreEcho(t *testing.T) {
	s, _ := startSeeded(t)
	_, body := post(t, s, "application/json", s.Token,
		`{"using": ["urn:ietf:params:jmap:core"], "methodCalls": [["Core/echo", {"hello": true}, "c0"]]}`)
	name, args, callID := firstResponse(t, body)
	if name != "Core/echo" || callID != "c0" || args["hello"] != true {
		t.Errorf("echo = %s %v %s", name, args, callID)
	}
}

// RFC 8620 §3.6.2 method-level errors.
func TestMethodLevelErrors(t *testing.T) {
	s, _ := startSeeded(t)
	cases := []struct {
		name     string
		using    string
		call     string
		wantType string
	}{
		{"unknown method", usingBoth, `["Bogus/frobnicate", {}, "c0"]`, "unknownMethod"},
		{"mail method without mail capability", `["urn:ietf:params:jmap:core"]`,
			`["Email/query", {"accountId": "acc-test"}, "c0"]`, "unknownMethod"},
		{"account not found", usingBoth, `["Email/query", {"accountId": "acc-nope"}, "c0"]`, "accountNotFound"},
		{"invalid result reference", usingBoth,
			`["Email/get", {"accountId": "acc-test", "#ids": {"resultOf": "nope", "name": "Email/query", "path": "/ids"}}, "c0"]`,
			"invalidResultReference"},
		{"unsupported filter operator", usingBoth,
			`["Email/query", {"accountId": "acc-test", "filter": {"operator": "AND", "conditions": []}}, "c0"]`,
			"unsupportedFilter"},
		{"unsupported sort", usingBoth,
			`["Email/query", {"accountId": "acc-test", "sort": [{"property": "subject"}]}, "c0"]`,
			"unsupportedSort"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			status, body := post(t, s, "application/json", s.Token,
				`{"using": `+c.using+`, "methodCalls": [`+c.call+`]}`)
			if status != 200 {
				t.Fatalf("status = %d, want 200 (method errors ride a successful response)", status)
			}
			name, args, _ := firstResponse(t, body)
			if name != "error" || args["type"] != c.wantType {
				t.Errorf("response = %s %v, want error/%s", name, args, c.wantType)
			}
		})
	}
}

// A raw query→get chain through a result reference, end to end.
func TestQueryGetChain(t *testing.T) {
	s, seed := startSeeded(t)
	_, body := post(t, s, "application/json", s.Token, `{
		"using": `+usingBoth+`,
		"methodCalls": [
			["Email/query", {"accountId": "acc-test",
				"filter": {"hasKeyword": "$flagged"}}, "q0"],
			["Email/get", {"accountId": "acc-test",
				"#ids": {"resultOf": "q0", "name": "Email/query", "path": "/ids"},
				"properties": ["id", "subject"]}, "g1"]
		]
	}`)
	responses, _ := body["methodResponses"].([]any)
	if len(responses) != 2 {
		t.Fatalf("got %d responses", len(responses))
	}
	get, _ := responses[1].([]any)
	args, _ := get[1].(map[string]any)
	list, _ := args["list"].([]any)
	if len(list) != 1 {
		t.Fatalf("list = %v", list)
	}
	first, _ := list[0].(map[string]any)
	if first["id"] != seed.Invoice || first["subject"] != "Invoice #42 for June" {
		t.Errorf("email = %v", first)
	}
	if _, hasPreview := first["preview"]; hasPreview {
		t.Error("properties filter ignored: preview present")
	}
}

// The session resource advertises what the store actually serves.
func TestSessionResource(t *testing.T) {
	s, _ := startSeeded(t)
	req, _ := http.NewRequest(http.MethodGet, s.SessionURL(), nil)
	req.Header.Set("Authorization", "Bearer "+s.Token)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	var session map[string]any
	if err := json.NewDecoder(res.Body).Decode(&session); err != nil {
		t.Fatal(err)
	}
	caps, _ := session["capabilities"].(map[string]any)
	if _, ok := caps[capCore]; !ok {
		t.Error("session missing core capability")
	}
	primary, _ := session["primaryAccounts"].(map[string]any)
	if primary[capMail] != s.AccountID {
		t.Errorf("primaryAccounts = %v", primary)
	}
	if session["apiUrl"] != s.APIURL() {
		t.Errorf("apiUrl = %v", session["apiUrl"])
	}
}
