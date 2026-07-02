package mail

// URL redaction for fetched bodies. Email bodies routinely embed URLs whose
// query strings or opaque path segments carry live credentials — unsubscribe
// tokens, one-click sign-ins, tracking ids. Fetched bodies land in agent
// context and transcripts, so redaction is ON by default; includeFullUrls
// opts out for tasks that genuinely need a link ("find my verification
// link"). Hosts and schemes survive, so the agent can still say where a
// link points.

import (
	"regexp"
	"strings"
)

const redactedMark = "…redacted…"

// urlRe finds http(s) URLs in plain text or HTML attributes, case-insensitive
// (mailers uppercase schemes), with bracketed IPv6 hosts matched explicitly —
// the general class must exclude "]" so it can't eat markdown/HTML context.
// Known residual gaps, accepted as low: a URL hard-wrapped across lines
// redacts only its first line's tail, and a body-budget truncation landing
// inside a path token can shrink it below the opaque-segment threshold.
var urlRe = regexp.MustCompile(`(?i)https?://(?:\[[0-9a-f:.]+\])?[^\s"'<>\\)\]}]*`)

// opaqueSegRe marks a path segment as token-like: 16+ chars drawn from
// base64/hex/uuid alphabets with at least one digit (rules out ordinary
// words like "notifications" or "unsubscribe-preferences").
var opaqueSegRe = regexp.MustCompile(`^[A-Za-z0-9_=.~-]{16,}$`)
var hasDigitRe = regexp.MustCompile(`[0-9]`)

// redactURLs rewrites every URL in s, dropping query strings, fragments,
// userinfo, and token-like path segments. Returns the rewritten text and
// how many URLs were altered.
func redactURLs(s string) (string, int) {
	changed := 0
	out := urlRe.ReplaceAllStringFunc(s, func(u string) string {
		r := redactOneURL(u)
		if r != u {
			changed++
		}
		return r
	})
	return out, changed
}

func redactOneURL(u string) string {
	// Trailing sentence punctuation is prose, not URL.
	trimmed := strings.TrimRight(u, ".,;:!")
	tail := u[len(trimmed):]
	u = trimmed

	rest := u
	// Fragment, then query — either may carry the token.
	if i := strings.IndexByte(rest, '#'); i >= 0 {
		rest = rest[:i] + "#" + redactedMark
	}
	if i := strings.IndexByte(rest, '?'); i >= 0 {
		frag := ""
		if j := strings.Index(rest, "#"); j > i {
			frag = rest[j:]
		}
		rest = rest[:i] + "?" + redactedMark + frag
	}

	// scheme://userinfo@host/path — drop userinfo, scrub opaque path segments.
	schemeEnd := strings.Index(rest, "://")
	if schemeEnd < 0 {
		return rest + tail
	}
	afterScheme := rest[schemeEnd+3:]
	pathStart := strings.IndexByte(afterScheme, '/')
	authority := afterScheme
	path := ""
	if pathStart >= 0 {
		authority = afterScheme[:pathStart]
		path = afterScheme[pathStart:]
	}
	if at := strings.LastIndexByte(authority, '@'); at >= 0 {
		authority = redactedMark + "@" + authority[at+1:]
	}
	if path != "" {
		segs := strings.Split(path, "/")
		for i, seg := range segs {
			// Strip ?/# suffixes already handled above from consideration.
			core := seg
			if j := strings.IndexAny(core, "?#"); j >= 0 {
				core = core[:j]
			}
			if opaqueSegRe.MatchString(core) && hasDigitRe.MatchString(core) {
				segs[i] = strings.Replace(seg, core, redactedMark, 1)
			}
		}
		path = strings.Join(segs, "/")
	}
	return rest[:schemeEnd+3] + authority + path + tail
}
