// Package config holds the extension's resolved settings: parsing rules,
// validation, and redaction. Pure logic — no SDK import; app.go maps the SDK's
// ext.Config into Settings.
package config

import (
	"fmt"
	"net/url"
	"strings"
)

const (
	// DefaultSessionURL is Fastmail's JMAP session resource, the primary
	// target provider. Any RFC 8620 session URL works here.
	DefaultSessionURL = "https://api.fastmail.com/jmap/session"

	// DefaultMaxBodyBytes bounds body text fetched per message when the tool
	// call doesn't ask for less.
	DefaultMaxBodyBytes = 12000

	// maxMaxBodyBytes is a sanity ceiling on the configurable bound: tool
	// results share the model's context window and the wire has a 1 MiB
	// tool-result budget.
	maxMaxBodyBytes = 1_000_000
)

// Access levels form a monotonic ladder; tools above the configured level
// are withdrawn from the model and refused by their handlers. Local sieve
// tools are available at every level (they never touch the provider).
const (
	AccessReadOnly = "read-only"             // search/fetch only
	AccessOrganize = "read-organize"         // + mark/move/trash
	AccessDestroy  = "read-organize-destroy" // + permanent destroy
)

// Settings are the resolved extension config values. All fields are scalars so
// two Settings can be compared with == to detect config changes.
type Settings struct {
	SessionURL     string
	APIToken       string
	DefaultAccount string
	MaxBodyBytes   int
	// AccessLevel is one of the Access* constants; defaults to read-only.
	AccessLevel string
	// EnableSieveTools opts in to the local sieve document store tools
	// (email_sieve_*); off by default — not everyone manages filters.
	EnableSieveTools bool
}

// AllowOrganize reports whether mark/move/trash are enabled.
func (s Settings) AllowOrganize() bool {
	return s.AccessLevel == AccessOrganize || s.AccessLevel == AccessDestroy
}

// AllowDestroy reports whether permanent destroy is enabled.
func (s Settings) AllowDestroy() bool { return s.AccessLevel == AccessDestroy }

// Normalize fills defaults for unset fields. The host already overlays
// manifest defaults onto user values, so this is defense in depth (and what
// unit tests exercise directly).
func Normalize(s Settings) Settings {
	if strings.TrimSpace(s.SessionURL) == "" {
		s.SessionURL = DefaultSessionURL
	}
	if s.MaxBodyBytes <= 0 {
		s.MaxBodyBytes = DefaultMaxBodyBytes
	}
	if s.MaxBodyBytes > maxMaxBodyBytes {
		s.MaxBodyBytes = maxMaxBodyBytes
	}
	if strings.TrimSpace(s.AccessLevel) == "" {
		s.AccessLevel = AccessReadOnly
	}
	return s
}

// Configured reports whether the required fields are present. Tools withdraw
// (or refuse with a clear message) when false.
func (s Settings) Configured() bool {
	return strings.TrimSpace(s.APIToken) != "" && strings.TrimSpace(s.SessionURL) != ""
}

// Validate checks field shapes. Error text never contains the token.
func (s Settings) Validate() error {
	u, err := url.Parse(s.SessionURL)
	if err != nil {
		return fmt.Errorf("session_url is not a valid URL: %v", err)
	}
	switch {
	case u.Scheme == "https":
	case u.Scheme == "http" && isLoopback(u.Hostname()):
		// http allowed only against loopback, for local test servers.
	default:
		return fmt.Errorf("session_url must be https (got %q) — JMAP carries credentials on every request", u.Scheme)
	}
	if u.Host == "" {
		return fmt.Errorf("session_url has no host")
	}
	switch s.AccessLevel {
	case AccessReadOnly, AccessOrganize, AccessDestroy:
	default:
		return fmt.Errorf("invalid access_level %q: use %s, %s, or %s", s.AccessLevel, AccessReadOnly, AccessOrganize, AccessDestroy)
	}
	return nil
}

func isLoopback(host string) bool {
	return host == "localhost" || host == "127.0.0.1" || host == "::1"
}

// Redacted renders the settings for logging: shape only, never the token.
func (s Settings) Redacted() string {
	return fmt.Sprintf("session_url=%s default_account=%q max_body_bytes=%d access_level=%s enable_sieve_tools=%v has_api_token=%v",
		stripUserinfo(s.SessionURL), s.DefaultAccount, s.MaxBodyBytes, s.AccessLevel, s.EnableSieveTools, s.APIToken != "")
}

// stripUserinfo drops any user:pass@ embedded in a URL before it reaches a
// log line. Tokens ride in a header, so this only matters for unusual
// configs — but a log must never be the place credentials surface.
func stripUserinfo(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil || u.User == nil {
		return rawURL
	}
	u.User = nil
	return u.String() + " (userinfo stripped)"
}
