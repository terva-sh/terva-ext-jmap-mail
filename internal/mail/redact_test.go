package mail

import (
	"strings"
	"testing"
)

func TestRedactURLs(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string // substring that must be present
		gone string // substring that must be absent
		n    int
	}{
		{
			name: "query string stripped",
			in:   "Manage: https://github.com/settings/notifications?token=abc123secret&user=drew now",
			want: "https://github.com/settings/notifications?…redacted…",
			gone: "abc123secret",
			n:    1,
		},
		{
			name: "unsubscribe token path segment",
			in:   "click https://link.example.com/unsub/dXNlcjEyM3Rva2VuNDU2Nzg5MGFi/confirm",
			want: "/unsub/…redacted…/confirm",
			gone: "dXNlcjEyM3Rva2VuNDU2Nzg5MGFi",
			n:    1,
		},
		{
			name: "fragment stripped",
			in:   "see https://app.example.com/reset#access_token=eyJhbGciOi",
			want: "https://app.example.com/reset#…redacted…",
			gone: "eyJhbGciOi",
			n:    1,
		},
		{
			name: "userinfo stripped",
			in:   "ftp-ish https://drew:hunter2@files.example.com/report.pdf ok",
			want: "https://…redacted…@files.example.com/report.pdf",
			gone: "hunter2",
			n:    1,
		},
		{
			name: "plain urls untouched",
			in:   "Docs at https://jmap.io/crash-course/index.html and https://example.com/blog/my-first-post",
			want: "https://jmap.io/crash-course/index.html",
			n:    0,
		},
		{
			name: "ordinary word segments survive",
			in:   "https://example.com/unsubscribe-preferences/notifications",
			want: "/unsubscribe-preferences/notifications",
			n:    0,
		},
		{
			name: "html attribute url",
			in:   `<a href="https://mail.example.com/click?id=zZ9x8y7w6v5u4t3s2r1q">Unsubscribe</a>`,
			want: `href="https://mail.example.com/click?…redacted…"`,
			gone: "zZ9x8y7w6v5u4t3s2r1q",
			n:    1,
		},
		{
			name: "trailing punctuation stays prose",
			in:   "Visit https://example.com/page?q=1.",
			want: "?…redacted….",
			gone: "q=1",
			n:    1,
		},
		{
			name: "uppercase scheme is not a bypass",
			in:   "HTTPS://EXAMPLE.COM/reset?token=SECRET123",
			want: "?…redacted…",
			gone: "SECRET123",
			n:    1,
		},
		{
			name: "bracketed IPv6 host is not a bypass",
			in:   "https://[2001:db8::1]:8443/reset?token=SECRET123",
			want: "[2001:db8::1]:8443/reset?…redacted…",
			gone: "SECRET123",
			n:    1,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, n := redactURLs(c.in)
			if !strings.Contains(got, c.want) {
				t.Errorf("got %q, want substring %q", got, c.want)
			}
			if c.gone != "" && strings.Contains(got, c.gone) {
				t.Errorf("got %q, %q should be redacted", got, c.gone)
			}
			if n != c.n {
				t.Errorf("redacted %d urls, want %d (got %q)", n, c.n, got)
			}
		})
	}
}

func TestRedactURLsHostSurvives(t *testing.T) {
	got, _ := redactURLs("https://user:pw@evil.example.com/a1b2c3d4e5f6a7b8c9d0/x?t=s#f")
	for _, keep := range []string{"evil.example.com", "https://"} {
		if !strings.Contains(got, keep) {
			t.Errorf("host/scheme must survive redaction: %q", got)
		}
	}
	for _, gone := range []string{"pw", "a1b2c3d4e5f6a7b8c9d0", "t=s", "#f"} {
		if strings.Contains(got, gone) && gone != "#f" {
			t.Errorf("%q must be gone: %q", gone, got)
		}
	}
}
