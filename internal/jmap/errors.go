package jmap

import (
	"fmt"
	"regexp"
	"strings"
)

// AuthError is an HTTP 401/403 from the provider. Its text points the user at
// config, and never contains the token.
type AuthError struct {
	StatusCode int
}

func (e *AuthError) Error() string {
	return fmt.Sprintf("authentication failed (HTTP %d) — check the JMAP API token (and its scopes) in /extensions", e.StatusCode)
}

// RequestError is an RFC 8620 §3.6.1 request-level problem
// (application/problem+json, RFC 7807) — the whole request was rejected.
type RequestError struct {
	Type   string `json:"type"`
	Status int    `json:"status"`
	Detail string `json:"detail"`
	// Limit names the violated limit for urn:ietf:params:jmap:error:limit.
	Limit string `json:"limit,omitempty"`
}

func (e *RequestError) Error() string {
	t := strings.TrimPrefix(e.Type, "urn:ietf:params:jmap:error:")
	msg := fmt.Sprintf("jmap request rejected: %s", t)
	if e.Limit != "" {
		msg += fmt.Sprintf(" (limit %s)", e.Limit)
	}
	if e.Detail != "" {
		msg += " — " + e.Detail
	}
	return msg
}

// MethodError is an RFC 8620 §3.6.2 method-level error: one call in the batch
// failed while the request itself succeeded.
type MethodError struct {
	CallID      string `json:"-"`
	Type        string `json:"type"`
	Description string `json:"description"`
}

func (e *MethodError) Error() string {
	msg := fmt.Sprintf("jmap method error: %s", e.Type)
	if e.Description != "" {
		msg += " — " + e.Description
	}
	if e.CallID != "" {
		msg += fmt.Sprintf(" (call %s)", e.CallID)
	}
	return msg
}

// HTTPError is a non-2xx response that carried no parseable JMAP problem.
// The body snippet is bounded and for diagnostics only.
type HTTPError struct {
	StatusCode int
	Snippet    string
}

func (e *HTTPError) Error() string {
	if e.Snippet == "" {
		return fmt.Sprintf("jmap server returned HTTP %d", e.StatusCode)
	}
	return fmt.Sprintf("jmap server returned HTTP %d: %s", e.StatusCode, e.Snippet)
}

// snippet bounds free-form response text for inclusion in error messages,
// scrubbing credentials the body echoes back at us. Redaction runs BEFORE the
// length bound so a cut can only land inside an already-replaced marker,
// never part-way through a token.
func snippet(b []byte, secret string) string {
	s := strings.TrimSpace(string(b))
	s = strings.Join(strings.Fields(s), " ") // collapse newlines/whitespace
	s = redactSecrets(s, secret)
	const max = 200
	if len(s) > max {
		s = s[:max] + "…"
	}
	return s
}

// bearerPattern matches an Authorization-header echo, including of a
// credential we do not hold (an intermediary may quote a rewritten or
// upstream token). "[redacted]" is not matchable: the class needs a token
// character right after the space.
var bearerPattern = regexp.MustCompile(`(?i)bearer\s+[A-Za-z0-9._~+/=-]+`)

const redactedMark = "[redacted]"

// redactSecrets strips credentials from provider-supplied free text. A
// provider, proxy, or WAF that echoes the request's Authorization header into
// an error body would otherwise put the bearer token into an error string —
// and from there into the tool caller's transcript, logs, and bug reports.
func redactSecrets(s, secret string) string {
	if secret != "" {
		s = strings.ReplaceAll(s, secret, redactedMark)
	}
	return bearerPattern.ReplaceAllString(s, "Bearer "+redactedMark)
}
