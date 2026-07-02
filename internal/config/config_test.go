package config

import (
	"strings"
	"testing"
)

func TestNormalizeDefaults(t *testing.T) {
	s := Normalize(Settings{})
	if s.SessionURL != DefaultSessionURL {
		t.Errorf("SessionURL = %q, want default", s.SessionURL)
	}
	if s.MaxBodyBytes != DefaultMaxBodyBytes {
		t.Errorf("MaxBodyBytes = %d, want %d", s.MaxBodyBytes, DefaultMaxBodyBytes)
	}
}

func TestNormalizeClampsBodyBytes(t *testing.T) {
	s := Normalize(Settings{MaxBodyBytes: 10_000_000})
	if s.MaxBodyBytes != maxMaxBodyBytes {
		t.Errorf("MaxBodyBytes = %d, want clamped to %d", s.MaxBodyBytes, maxMaxBodyBytes)
	}
}

func TestAccessLevels(t *testing.T) {
	s := Normalize(Settings{SessionURL: DefaultSessionURL, APIToken: "t"})
	if s.AccessLevel != AccessReadOnly || s.AllowOrganize() || s.AllowDestroy() {
		t.Errorf("default level = %+v, want read-only with nothing allowed", s)
	}
	if s.EnableSieveTools {
		t.Error("sieve tools must default off")
	}
	s.AccessLevel = AccessOrganize
	if !s.AllowOrganize() || s.AllowDestroy() {
		t.Errorf("organize level wrong: %+v", s)
	}
	s.AccessLevel = AccessDestroy
	if !s.AllowOrganize() || !s.AllowDestroy() {
		t.Errorf("destroy level wrong: %+v", s)
	}
	if err := s.Validate(); err != nil {
		t.Errorf("valid level rejected: %v", err)
	}
	s.AccessLevel = "yolo"
	if err := s.Validate(); err == nil {
		t.Error("invalid access_level accepted")
	}
}

func TestConfigured(t *testing.T) {
	if (Settings{SessionURL: DefaultSessionURL}).Configured() {
		t.Error("configured without token")
	}
	if !(Settings{SessionURL: DefaultSessionURL, APIToken: "tok"}).Configured() {
		t.Error("not configured with url+token")
	}
}

func TestValidate(t *testing.T) {
	cases := []struct {
		url string
		ok  bool
	}{
		{"https://api.fastmail.com/jmap/session", true},
		{"http://127.0.0.1:8080/jmap/session", true},
		{"http://localhost:8080/jmap/session", true},
		{"http://example.com/jmap/session", false},
		{"ftp://example.com/x", false},
		{"://bad", false},
	}
	for _, c := range cases {
		err := Normalize(Settings{SessionURL: c.url, APIToken: "tok"}).Validate()
		if (err == nil) != c.ok {
			t.Errorf("Validate(%q) err=%v, want ok=%v", c.url, err, c.ok)
		}
	}
}

// The token must never appear in logged/rendered forms or validation errors.
func TestNoTokenLeaks(t *testing.T) {
	const token = "fmu1-SECRET-TOKEN-VALUE"
	s := Normalize(Settings{SessionURL: "ftp://bad", APIToken: token, DefaultAccount: "u@example.com", MaxBodyBytes: 5})
	if strings.Contains(s.Redacted(), token) {
		t.Errorf("Redacted() leaks token: %s", s.Redacted())
	}
	if !strings.Contains(s.Redacted(), "has_api_token=true") {
		t.Errorf("Redacted() should note token presence: %s", s.Redacted())
	}
	if err := s.Validate(); err != nil && strings.Contains(err.Error(), token) {
		t.Errorf("Validate() error leaks token: %v", err)
	}
}
