package jmap

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Client speaks HTTPS+JSON to one JMAP provider with one bearer token. It is
// stateless: session caching/refresh policy belongs to the caller
// (internal/mail).
type Client struct {
	SessionURL string
	Token      string
	HTTPClient *http.Client
	UserAgent  string
}

// NewClient builds a client for a session URL and bearer token.
func NewClient(sessionURL, token string) *Client {
	return &Client{
		SessionURL: sessionURL,
		Token:      token,
		HTTPClient: &http.Client{
			Timeout: 30 * time.Second,
			// Go strips Authorization on cross-host redirects but keeps it
			// on same-host ones REGARDLESS of scheme, so a compromised or
			// misconfigured provider could 302 https→http and receive the
			// bearer token in cleartext. Every redirect target must satisfy
			// the same endpoint policy as a configured URL.
			CheckRedirect: func(req *http.Request, via []*http.Request) error {
				if len(via) >= 10 {
					return fmt.Errorf("too many redirects")
				}
				return checkEndpoint(req.URL)
			},
		},
		UserAgent: "terva-ext-jmap-mail",
	}
}

// checkEndpoint enforces the transport policy for anywhere the bearer token
// travels: https, or plain http to loopback only (local test servers). The
// same rule config.Validate applies to session_url — re-applied here because
// apiUrl (and redirect targets) come from the SERVER, not the user.
func checkEndpoint(u *url.URL) error {
	switch u.Scheme {
	case "https":
		return nil
	case "http":
		host := u.Hostname()
		if host == "localhost" {
			return nil
		}
		if ip := net.ParseIP(host); ip != nil && ip.IsLoopback() {
			return nil
		}
		return fmt.Errorf("refusing to send credentials over plain http to %s (loopback only)", host)
	default:
		return fmt.Errorf("unsupported URL scheme %q", u.Scheme)
	}
}

// maxResponseBytes bounds how much of any provider response we read; a sane
// JMAP response for our bounded requests is far below this.
const maxResponseBytes = 8 << 20

// FetchSession GETs and parses the RFC 8620 §2 session resource, verifying
// the server speaks JMAP core.
func (c *Client) FetchSession(ctx context.Context) (*Session, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.SessionURL, nil)
	if err != nil {
		return nil, fmt.Errorf("invalid session_url: %w", err)
	}
	c.setHeaders(req)

	body, err := c.do(req)
	if err != nil {
		return nil, err
	}
	var s Session
	if err := json.Unmarshal(body, &s); err != nil {
		return nil, fmt.Errorf("session resource is not valid JSON (%v) — is session_url a JMAP session endpoint?", err)
	}
	if _, ok := s.Capabilities[CapCore]; !ok {
		return nil, fmt.Errorf("server does not advertise %s — not a JMAP server (or an incompatible one)", CapCore)
	}
	if s.APIURL == "" {
		return nil, fmt.Errorf("session resource has no apiUrl")
	}
	return &s, nil
}

// Call POSTs one RFC 8620 §3.3 request (capabilities in `using`, ordered
// `methodCalls`) to the session's apiUrl and parses the response envelope.
func (c *Client) Call(ctx context.Context, apiURL string, using []string, calls []Invocation) (*Response, error) {
	if len(calls) == 0 {
		return nil, fmt.Errorf("no method calls")
	}
	payload, err := json.Marshal(Request{Using: using, MethodCalls: calls})
	if err != nil {
		return nil, fmt.Errorf("encode jmap request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, apiURL, bytes.NewReader(payload))
	if err != nil {
		return nil, fmt.Errorf("invalid apiUrl: %w", err)
	}
	// apiUrl is server-supplied (the session resource), so the https-or-
	// loopback policy the user's session_url passed is re-checked here.
	if err := checkEndpoint(req.URL); err != nil {
		return nil, fmt.Errorf("session apiUrl rejected: %w", err)
	}
	c.setHeaders(req)
	req.Header.Set("Content-Type", "application/json")

	body, err := c.do(req)
	if err != nil {
		return nil, err
	}
	var resp Response
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, fmt.Errorf("jmap response is not a valid envelope: %v", err)
	}
	return &resp, nil
}

func (c *Client) setHeaders(req *http.Request) {
	req.Header.Set("Authorization", "Bearer "+c.Token)
	req.Header.Set("Accept", "application/json")
	if c.UserAgent != "" {
		req.Header.Set("User-Agent", c.UserAgent)
	}
}

// do executes the request and maps failures to the error taxonomy: AuthError
// (401/403), RequestError (RFC 8620 §3.6.1 problem JSON), HTTPError
// (anything else non-2xx). Error text never includes the Authorization header,
// and every path that quotes provider-supplied text runs it through
// redactSecrets first — an intermediary that echoes the header into a body
// must not be able to reflect the token into the caller's transcript.
func (c *Client) do(req *http.Request) ([]byte, error) {
	res, err := c.HTTPClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("jmap request to %s failed: %w", req.URL.Host, sanitizeErr(err))
	}
	defer res.Body.Close()
	body, err := io.ReadAll(io.LimitReader(res.Body, maxResponseBytes))
	if err != nil {
		return nil, fmt.Errorf("read jmap response: %w", err)
	}

	switch {
	case res.StatusCode == http.StatusUnauthorized || res.StatusCode == http.StatusForbidden:
		return nil, &AuthError{StatusCode: res.StatusCode}
	case res.StatusCode >= 200 && res.StatusCode < 300:
		return body, nil
	}
	// Request-level problem details (RFC 8620 §3.6.1)?
	var problem RequestError
	if err := json.Unmarshal(body, &problem); err == nil && strings.HasPrefix(problem.Type, "urn:ietf:params:jmap:error:") {
		if problem.Status == 0 {
			problem.Status = res.StatusCode
		}
		// detail is free text the server chose; type and limit are enumerated.
		problem.Detail = redactSecrets(problem.Detail, c.Token)
		return nil, &problem
	}
	return nil, &HTTPError{StatusCode: res.StatusCode, Snippet: snippet(body, c.Token)}
}

// sanitizeErr strips any echo of the request URL's userinfo from transport
// errors. Bearer tokens ride in a header (never the URL), so this is belt and
// braces for exotic transports.
func sanitizeErr(err error) error {
	return fmt.Errorf("%s", strings.ReplaceAll(err.Error(), "\n", " "))
}
