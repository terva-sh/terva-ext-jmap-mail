// Package jmaptest is an in-memory JMAP server (RFC 8620/8621 read subset)
// for hermetic tests: session resource, request envelope, result references,
// Core/echo, Mailbox/get, Email/query, Email/get, and Thread/get over a
// seeded mail store.
//
// It deliberately does NOT import internal/jmap: requests are parsed with
// plain encoding/json against the RFC wire shapes, so a marshaling bug in the
// client cannot be masked by the same bug here. It is a test double for the
// protocol, not a reuse of the client. Phase 2 will extend it with Email/set.
package jmaptest

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

const (
	capCore = "urn:ietf:params:jmap:core"
	capMail = "urn:ietf:params:jmap:mail"
)

// Mailbox is a stored mailbox (RFC 8621 §2).
type Mailbox struct {
	ID        string
	Name      string
	ParentID  string
	Role      string
	SortOrder int
}

// Address is a stored email address.
type Address struct {
	Name  string
	Email string
}

// Attachment is stored attachment metadata (content is never served).
type Attachment struct {
	Name string
	Type string
	Size int
}

// Email is a stored message.
type Email struct {
	ID          string
	ThreadID    string
	MailboxIDs  []string
	Keywords    []string
	From        []Address
	To          []Address
	Cc          []Address
	Bcc         []Address
	ReplyTo     []Address
	Subject     string
	ReceivedAt  time.Time
	SentAt      time.Time
	TextBody    string
	HTMLBody    string
	Attachments []Attachment
}

func (e *Email) size() int {
	n := 512 + len(e.TextBody) + len(e.HTMLBody)
	for _, a := range e.Attachments {
		n += a.Size
	}
	return n
}

// Server is one in-memory JMAP provider with a single account and bearer-token
// auth. Zero value is not usable; construct with New.
type Server struct {
	Token             string
	AccountID         string
	Username          string
	MaxCallsInRequest int

	mu        sync.Mutex
	mailboxes []*Mailbox
	emails    []*Email
	nextID    int
	stateN    int // bumped by every successful mutation

	hs *httptest.Server
}

// stateLocked is the current mail state string (RFC 8620 §5.1/§5.3); callers
// hold s.mu.
func (s *Server) stateLocked() string { return fmt.Sprintf("state-%d", s.stateN) }

// New starts a server with default credentials (Token "test-token") and an
// empty store. Callers own Close (t.Cleanup(srv.Close)).
func New() *Server {
	s := &Server{
		Token:             "test-token",
		AccountID:         "acc-test",
		Username:          "tester@example.test",
		MaxCallsInRequest: 16,
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/jmap", s.handleSession)
	mux.HandleFunc("/jmap", s.handleAPI)
	s.hs = httptest.NewServer(mux)
	return s
}

// Close shuts the HTTP server down.
func (s *Server) Close() { s.hs.Close() }

// SessionURL is the RFC 8620 §2.2 well-known session resource.
func (s *Server) SessionURL() string { return s.hs.URL + "/.well-known/jmap" }

// APIURL is the session's apiUrl.
func (s *Server) APIURL() string { return s.hs.URL + "/jmap" }

// AddMailbox stores a mailbox, assigning ID/SortOrder when unset, and returns
// the id.
func (s *Server) AddMailbox(m Mailbox) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if m.ID == "" {
		s.nextID++
		m.ID = fmt.Sprintf("mb-%d", s.nextID)
	}
	if m.SortOrder == 0 {
		m.SortOrder = len(s.mailboxes) + 1
	}
	s.mailboxes = append(s.mailboxes, &m)
	return m.ID
}

// AddEmail stores a message, assigning ID/ThreadID/timestamps when unset, and
// returns the id.
func (s *Server) AddEmail(e Email) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if e.ID == "" {
		s.nextID++
		e.ID = fmt.Sprintf("msg-%d", s.nextID)
	}
	if e.ThreadID == "" {
		e.ThreadID = "thr-" + e.ID
	}
	if e.ReceivedAt.IsZero() {
		e.ReceivedAt = time.Date(2026, 6, 1, 9, 0, 0, 0, time.UTC).Add(time.Duration(len(s.emails)) * time.Hour)
	}
	if e.SentAt.IsZero() {
		e.SentAt = e.ReceivedAt
	}
	s.emails = append(s.emails, &e)
	return e.ID
}

func (s *Server) authorized(r *http.Request) bool {
	return r.Header.Get("Authorization") == "Bearer "+s.Token
}

// handleSession serves the RFC 8620 §2 session resource.
func (s *Server) handleSession(w http.ResponseWriter, r *http.Request) {
	if !s.authorized(r) {
		w.Header().Set("WWW-Authenticate", `Bearer realm="jmaptest"`)
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	session := map[string]any{
		"capabilities": map[string]any{
			capCore: map[string]any{
				"maxSizeUpload":         50000000,
				"maxConcurrentUpload":   4,
				"maxSizeRequest":        10000000,
				"maxConcurrentRequests": 4,
				"maxCallsInRequest":     s.MaxCallsInRequest,
				"maxObjectsInGet":       500,
				"maxObjectsInSet":       500,
				"collationAlgorithms":   []string{"i;ascii-numeric", "i;ascii-casemap"},
			},
			capMail: map[string]any{},
		},
		"accounts": map[string]any{
			s.AccountID: map[string]any{
				"name":       s.Username,
				"isPersonal": true,
				"isReadOnly": false,
				"accountCapabilities": map[string]any{
					capMail: map[string]any{
						"maxMailboxDepth":          10,
						"mayCreateTopLevelMailbox": true,
					},
				},
			},
		},
		"primaryAccounts": map[string]any{capMail: s.AccountID},
		"username":        s.Username,
		"apiUrl":          s.APIURL(),
		"downloadUrl":     s.hs.URL + "/download/{accountId}/{blobId}/{name}?accept={type}",
		"uploadUrl":       s.hs.URL + "/upload/{accountId}/",
		"eventSourceUrl":  s.hs.URL + "/eventsource/",
		"state":           "session-0",
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(session)
}

// writeProblem emits an RFC 8620 §3.6.1 request-level problem (RFC 7807).
func writeProblem(w http.ResponseWriter, status int, typ, detail string, extra map[string]any) {
	body := map[string]any{
		"type":   "urn:ietf:params:jmap:error:" + typ,
		"status": status,
		"detail": detail,
	}
	for k, v := range extra {
		body[k] = v
	}
	w.Header().Set("Content-Type", "application/problem+json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(body)
}

// methodErr is an RFC 8620 §3.6.2 method-level error.
type methodErr struct {
	Type        string
	Description string
}

func (e *methodErr) asMap() map[string]any {
	m := map[string]any{"type": e.Type}
	if e.Description != "" {
		m["description"] = e.Description
	}
	return m
}

// handleAPI serves the RFC 8620 §3 API endpoint.
func (s *Server) handleAPI(w http.ResponseWriter, r *http.Request) {
	if !s.authorized(r) {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	// RFC 8620 §3.6.1 notJSON: content type must be application/json and the
	// body must parse.
	if ct := r.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		writeProblem(w, 400, "notJSON", "Content-Type must be application/json", nil)
		return
	}
	var req struct {
		Using       []string          `json:"using"`
		MethodCalls []json.RawMessage `json:"methodCalls"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeProblem(w, 400, "notJSON", "request did not parse as JSON", nil)
		return
	}
	if req.MethodCalls == nil {
		writeProblem(w, 400, "notRequest", "missing methodCalls", nil)
		return
	}
	// RFC 8620 §3.6.1 unknownCapability: every using entry must be supported.
	for _, urn := range req.Using {
		if urn != capCore && urn != capMail {
			writeProblem(w, 400, "unknownCapability", "unsupported capability "+urn, nil)
			return
		}
	}
	// RFC 8620 §3.6.1 limit.
	if len(req.MethodCalls) > s.MaxCallsInRequest {
		writeProblem(w, 400, "limit", "too many method calls", map[string]any{"limit": "maxCallsInRequest"})
		return
	}
	hasMail := false
	for _, urn := range req.Using {
		if urn == capMail {
			hasMail = true
		}
	}

	var priors []prior
	responses := make([][3]any, 0, len(req.MethodCalls))
	for _, raw := range req.MethodCalls {
		// RFC 8620 §3.2: each call is a [name, arguments, callId] triple.
		var triple []json.RawMessage
		if err := json.Unmarshal(raw, &triple); err != nil || len(triple) != 3 {
			writeProblem(w, 400, "notRequest", "method call is not a [name, args, callId] triple", nil)
			return
		}
		var name, callID string
		var args map[string]any
		if json.Unmarshal(triple[0], &name) != nil || json.Unmarshal(triple[2], &callID) != nil {
			writeProblem(w, 400, "notRequest", "method call name/callId must be strings", nil)
			return
		}
		if err := json.Unmarshal(triple[1], &args); err != nil {
			writeProblem(w, 400, "notRequest", "method call arguments must be an object", nil)
			return
		}
		if args == nil {
			args = map[string]any{}
		}

		respName, respArgs := s.dispatch(name, args, priors, hasMail)
		responses = append(responses, [3]any{respName, respArgs, callID})
		// Store the response as generic JSON values so result-reference paths
		// evaluate against what would be on the wire.
		priors = append(priors, prior{name: respName, callID: callID, args: toGeneric(respArgs)})
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"methodResponses": responses,
		"sessionState":    "session-0",
	})
}

func toGeneric(v any) any {
	b, err := json.Marshal(v)
	if err != nil {
		return nil
	}
	var out any
	json.Unmarshal(b, &out)
	return out
}

// dispatch resolves result references, then routes one method call.
func (s *Server) dispatch(name string, args map[string]any, priors []prior, hasMail bool) (string, any) {
	if merr := resolveRefs(args, priors); merr != nil {
		return "error", merr.asMap()
	}
	if name == "Core/echo" { // RFC 8620 §4
		return name, args
	}
	// A method whose capability is not declared in `using` is unknown to this
	// request (RFC 8620 §3.6.2 unknownMethod).
	if !hasMail {
		return "error", (&methodErr{Type: "unknownMethod", Description: capMail + " not declared in using"}).asMap()
	}
	var handler func(map[string]any) (any, *methodErr)
	switch name {
	case "Mailbox/get":
		handler = s.mailboxGet
	case "Email/query":
		handler = s.emailQuery
	case "Email/get":
		handler = s.emailGet
	case "Email/set":
		handler = s.emailSet
	case "Thread/get":
		handler = s.threadGet
	default:
		return "error", (&methodErr{Type: "unknownMethod"}).asMap()
	}
	if acct, _ := args["accountId"].(string); acct != s.AccountID {
		return "error", (&methodErr{Type: "accountNotFound", Description: fmt.Sprintf("no account %q", acct)}).asMap()
	}
	result, merr := handler(args)
	if merr != nil {
		return "error", merr.asMap()
	}
	return name, result
}

// --- shared arg/value helpers ---

// stringSlice coerces a decoded JSON value ([]any) or a resolved reference
// into []string.
func stringSlice(v any) []string {
	switch t := v.(type) {
	case []string:
		return t
	case []any:
		out := make([]string, 0, len(t))
		for _, item := range t {
			if s, ok := item.(string); ok {
				out = append(out, s)
			}
		}
		return out
	}
	return nil
}

// idsArg reads the standard /get `ids` argument: absent or null means all
// (RFC 8620 §5.1).
func idsArg(args map[string]any) (ids []string, all bool) {
	v, present := args["ids"]
	if !present || v == nil {
		return nil, true
	}
	return stringSlice(v), false
}

// filterProps applies the standard /get `properties` argument; id is always
// returned (RFC 8620 §5.1).
func filterProps(m map[string]any, props []string) map[string]any {
	if props == nil {
		return m
	}
	out := map[string]any{"id": m["id"]}
	for _, p := range props {
		if v, ok := m[p]; ok {
			out[p] = v
		}
	}
	return out
}

func boolArg(args map[string]any, key string) bool {
	b, _ := args[key].(bool)
	return b
}

func intArg(args map[string]any, key string) int {
	f, ok := args[key].(float64)
	if !ok {
		return 0
	}
	return int(f)
}

func containsStr(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}

func truncUTF8(s string, n int) string {
	if n <= 0 {
		return ""
	}
	if len(s) <= n {
		return s
	}
	cut := n
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut]
}
