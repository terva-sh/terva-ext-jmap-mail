// Package jmap is a minimal JMAP (RFC 8620) client: session discovery, the
// request/response envelope, result references, and the protocol error
// taxonomy. Mail-agnostic — method names and arguments are the caller's
// business (internal/mail). Pure logic, no SDK import.
package jmap

import (
	"encoding/json"
	"fmt"
	"time"
)

// Capability URNs (RFC 8620 §2, RFC 8621 §1.3).
const (
	CapCore             = "urn:ietf:params:jmap:core"
	CapMail             = "urn:ietf:params:jmap:mail"
	CapSubmission       = "urn:ietf:params:jmap:submission"
	CapVacationResponse = "urn:ietf:params:jmap:vacationresponse"
)

// Session is the RFC 8620 §2 session resource, limited to the fields we
// consume.
type Session struct {
	Capabilities    map[string]json.RawMessage `json:"capabilities"`
	Accounts        map[string]Account         `json:"accounts"`
	PrimaryAccounts map[string]string          `json:"primaryAccounts"`
	Username        string                     `json:"username"`
	APIURL          string                     `json:"apiUrl"`
	DownloadURL     string                     `json:"downloadUrl"`
	UploadURL       string                     `json:"uploadUrl"`
	EventSourceURL  string                     `json:"eventSourceUrl"`
	State           string                     `json:"state"`
}

// Account is one entry of the session's accounts map (RFC 8620 §2).
type Account struct {
	Name                string                     `json:"name"`
	IsPersonal          bool                       `json:"isPersonal"`
	IsReadOnly          bool                       `json:"isReadOnly"`
	AccountCapabilities map[string]json.RawMessage `json:"accountCapabilities"`
}

// HasCapability reports whether the account advertises the capability URN.
func (a Account) HasCapability(urn string) bool {
	_, ok := a.AccountCapabilities[urn]
	return ok
}

// CoreLimits are the urn:ietf:params:jmap:core capability limits
// (RFC 8620 §2).
type CoreLimits struct {
	MaxSizeUpload         uint64 `json:"maxSizeUpload"`
	MaxConcurrentUpload   uint64 `json:"maxConcurrentUpload"`
	MaxSizeRequest        uint64 `json:"maxSizeRequest"`
	MaxConcurrentRequests uint64 `json:"maxConcurrentRequests"`
	MaxCallsInRequest     uint64 `json:"maxCallsInRequest"`
	MaxObjectsInGet       uint64 `json:"maxObjectsInGet"`
	MaxObjectsInSet       uint64 `json:"maxObjectsInSet"`
}

// CoreLimits parses the core capability object, if present.
func (s *Session) CoreLimits() (CoreLimits, bool) {
	raw, ok := s.Capabilities[CapCore]
	if !ok {
		return CoreLimits{}, false
	}
	var l CoreLimits
	if err := json.Unmarshal(raw, &l); err != nil {
		return CoreLimits{}, false
	}
	return l, true
}

// Invocation is one method call: the [name, arguments, callId] triple of
// RFC 8620 §3.2.
type Invocation struct {
	Name   string
	Args   any
	CallID string
}

// MarshalJSON renders the RFC 8620 §3.2 triple form.
func (i Invocation) MarshalJSON() ([]byte, error) {
	args := i.Args
	if args == nil {
		args = map[string]any{}
	}
	return json.Marshal([3]any{i.Name, args, i.CallID})
}

// InvocationResult is one method response triple, with arguments kept raw for
// the caller to decode.
type InvocationResult struct {
	Name   string
	Args   json.RawMessage
	CallID string
}

// UnmarshalJSON parses the [name, arguments, callId] triple.
func (r *InvocationResult) UnmarshalJSON(b []byte) error {
	var arr []json.RawMessage
	if err := json.Unmarshal(b, &arr); err != nil {
		return err
	}
	if len(arr) != 3 {
		return fmt.Errorf("method response is not a [name, args, callId] triple (got %d elements)", len(arr))
	}
	if err := json.Unmarshal(arr[0], &r.Name); err != nil {
		return fmt.Errorf("method response name: %w", err)
	}
	r.Args = arr[1]
	if err := json.Unmarshal(arr[2], &r.CallID); err != nil {
		return fmt.Errorf("method response callId: %w", err)
	}
	return nil
}

// Request is the RFC 8620 §3.3 request object.
type Request struct {
	Using       []string     `json:"using"`
	MethodCalls []Invocation `json:"methodCalls"`
}

// Response is the RFC 8620 §3.4 response object.
type Response struct {
	MethodResponses []InvocationResult `json:"methodResponses"`
	SessionState    string             `json:"sessionState"`
}

// Result returns the method response for callID, translating an RFC 8620
// §3.6.2 method-level "error" invocation into a *MethodError. When a server
// answers one call with several responses (e.g. implicit changes), the first
// matching response is returned.
func (r *Response) Result(callID string) (*InvocationResult, error) {
	for i := range r.MethodResponses {
		mr := &r.MethodResponses[i]
		if mr.CallID != callID {
			continue
		}
		if mr.Name == "error" {
			me := &MethodError{CallID: callID}
			if err := json.Unmarshal(mr.Args, me); err != nil {
				return nil, fmt.Errorf("undecodable method error for call %q", callID)
			}
			return nil, me
		}
		return mr, nil
	}
	return nil, fmt.Errorf("server response is missing call %q", callID)
}

// ResultReference is the RFC 8620 §3.7 back-reference to an earlier method
// call's result within the same request. Place it in an args map under the
// "#"-prefixed argument name, e.g. args["#ids"] = ResultReference{...}.
type ResultReference struct {
	ResultOf string `json:"resultOf"`
	Name     string `json:"name"`
	Path     string `json:"path"`
}

// ToUTCDate converts a caller-supplied timestamp into the RFC 8620 UTCDate
// form ("2006-01-02T15:04:05Z"). Accepts RFC 3339 or a bare date
// ("2006-01-02", interpreted as midnight UTC).
func ToUTCDate(s string) (string, error) {
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t.UTC().Format("2006-01-02T15:04:05Z"), nil
	}
	if t, err := time.Parse("2006-01-02", s); err == nil {
		return t.UTC().Format("2006-01-02T15:04:05Z"), nil
	}
	return "", fmt.Errorf("invalid date %q: use RFC 3339 (2026-07-01T12:00:00Z) or a bare date (2026-07-01)", s)
}
