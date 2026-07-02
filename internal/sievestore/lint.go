package sievestore

// Advisory sieve lint (RFC 5228 + common extensions). Fastmail's save-time
// validation remains the authority — these checks exist to catch obvious
// problems before the user round-trips a paste, so everything here is
// advisory: findings never block a Put.

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// Finding is one advisory lint result.
type Finding struct {
	Level   string `json:"level"` // error | warning | info
	Line    int    `json:"line,omitempty"`
	Message string `json:"message"`
}

// LintResult is what Lint returns: findings plus extracted facts the agent
// can verify itself (fileinto targets against email_list_mailboxes — the
// store stays zero-network, so the cross-check belongs to the caller).
type LintResult struct {
	Findings        []Finding `json:"findings"`
	Requires        []string  `json:"requires,omitempty"`        // extensions the content declares
	FileintoTargets []string  `json:"fileintoTargets,omitempty"` // mailbox names/paths used by fileinto
}

// truncationRe spots model-invented truncation markers — bracketed mentions
// of truncation, or a bare ellipsis line — the footgun observed in the field
// (a 67KB export stored as 1.5KB ending in "[TRUNCATED IN TOOL IMPORT: …]").
var truncationRe = regexp.MustCompile(`(?i)\[[^\]\n]*truncat[^\]\n]*\]|\[\s*(\.\.\.|…)\s*\]`)

// SuspectsTruncation reports whether text carries a truncation marker.
func SuspectsTruncation(text string) bool {
	return truncationRe.MatchString(text) || strings.Contains(strings.ToLower(text), "truncated placeholder")
}

// extForToken maps commands/tests/tags to the extension that must be
// required for them (RFC 5228 §3.2 semantics). Conservative: only names
// here produce used-but-not-required findings.
var extForToken = map[string]string{
	"fileinto":     "fileinto",
	"envelope":     "envelope",
	"body":         "body",
	"set":          "variables",
	"string":       "variables",
	"setflag":      "imap4flags",
	"addflag":      "imap4flags",
	"removeflag":   "imap4flags",
	"hasflag":      "imap4flags",
	"addheader":    "editheader",
	"deleteheader": "editheader",
	"currentdate":  "date",
	"date":         "date",
	"duplicate":    "duplicate",
	"vacation":     "vacation",
	"notify":       "enotify",
	"reject":       "reject",
	"ereject":      "ereject",
	"include":      "include",
	"global":       "include",
	"environment":  "environment",
	":copy":        "copy",
	":create":      "mailbox",
	":mailboxid":   "mailboxid",
	":specialuse":  "special-use",
	":regex":       "regex",
	":count":       "relational",
	":value":       "relational",
	":user":        "subaddress",
	":detail":      "subaddress",
	":index":       "index",
}

// knownExtensions is the require-set Fastmail documents (plus core RFC
// families); anything else gets an info finding, not an error — providers
// vary and the save-time validator decides.
var knownExtensions = map[string]bool{
	"fileinto": true, "envelope": true, "envelope-deliverby": true, "body": true,
	"regex": true, "relational": true, "variables": true, "imap4flags": true,
	"editheader": true, "date": true, "index": true, "duplicate": true,
	"mailboxid": true, "special-use": true, "mailbox": true, "copy": true,
	"vacation": true, "vacation-seconds": true, "reject": true, "ereject": true,
	"enotify": true, "notify": true, "include": true, "environment": true,
	"subaddress": true, "encoded-character": true, "comparator-i;ascii-numeric": true,
	"fcc": true, "mime": true, "foreverypart": true, "extracttext": true,
	"replace": true, "enclose": true, "ihave": true, "imapsieve": true,
	"extlists": true, "spamtest": true, "virustest": true,
}

type token struct {
	kind string // word | tag | string | punct
	text string
	line int
}

// Lint runs every advisory check over one sieve document.
func Lint(content string) LintResult {
	var res LintResult
	if strings.TrimSpace(content) == "" {
		return res // an empty editable area is valid
	}
	if loc := truncationRe.FindStringIndex(content); loc != nil {
		res.Findings = append(res.Findings, Finding{
			Level: "error", Line: 1 + strings.Count(content[:loc[0]], "\n"),
			Message: "content carries a truncation marker — this is a partial paste, not a real script; re-import from a file (sourcePath) or store it contextOnly",
		})
	}

	tokens, scanFindings := scanSieve(content)
	res.Findings = append(res.Findings, scanFindings...)

	required := map[string]int{} // extension → declared line
	used := map[string]int{}     // extension → first use line
	for i := 0; i < len(tokens); i++ {
		t := tokens[i]
		switch {
		case t.kind == "word" && t.text == "require":
			for j := i + 1; j < len(tokens); j++ {
				if tokens[j].kind == "string" {
					if _, dup := required[tokens[j].text]; !dup {
						required[tokens[j].text] = tokens[j].line
					}
					res.Requires = append(res.Requires, tokens[j].text)
				} else if tokens[j].kind == "punct" && tokens[j].text == ";" {
					i = j
					break
				}
			}
		case t.kind == "word" && t.text == "discard":
			res.Findings = append(res.Findings, Finding{
				Level: "error", Line: t.line,
				Message: `discard permanently drops mail, bypassing Fastmail's backups — fileinto "Trash" routes identically and stays recoverable`,
			})
		case t.kind == "word" && t.text == "stop":
			res.Findings = append(res.Findings, Finding{
				Level: "warning", Line: t.line,
				Message: "stop halts every later rule, including the provider's generated blocks — keep only if that is the explicit intent",
			})
		case t.kind == "word" && t.text == "fileinto":
			var strs []token
			for j := i + 1; j < len(tokens); j++ {
				// A fileinto's arguments are tags and strings only; any
				// word, brace, or ";" means the statement ended (a missing
				// ";" must not swallow the NEXT statement's string).
				if tokens[j].kind == "word" ||
					(tokens[j].kind == "punct" && (tokens[j].text == ";" || tokens[j].text == "{" || tokens[j].text == "}")) {
					break
				}
				if tokens[j].kind == "string" {
					strs = append(strs, tokens[j])
				}
			}
			if len(strs) > 0 {
				// Tag string args (:mailboxid "id", :specialuse "use") come
				// first; the final string is the mailbox target.
				res.FileintoTargets = append(res.FileintoTargets, strs[len(strs)-1].text)
			} else {
				res.Findings = append(res.Findings, Finding{
					Level: "warning", Line: t.line, Message: "fileinto without a mailbox string",
				})
			}
		case t.kind == "word" && t.text == "vacation":
			// :seconds needs vacation-seconds on top of vacation.
			for j := i + 1; j < len(tokens) && !(tokens[j].kind == "punct" && tokens[j].text == ";"); j++ {
				if tokens[j].kind == "tag" && tokens[j].text == ":seconds" {
					if _, ok := used["vacation-seconds"]; !ok {
						used["vacation-seconds"] = tokens[j].line
					}
				}
			}
		}
		if ext, ok := extForToken[t.text]; ok && (t.kind == "word" || t.kind == "tag") {
			if _, seen := used[ext]; !seen {
				used[ext] = t.line
			}
		}
	}

	for ext, line := range used {
		if _, ok := required[ext]; !ok {
			res.Findings = append(res.Findings, Finding{
				Level: "warning", Line: line,
				Message: fmt.Sprintf("%q is used but not in a require line of this section — fine only if an earlier area already requires it", ext),
			})
		}
	}
	for ext, line := range required {
		if !knownExtensions[ext] && !strings.HasPrefix(ext, "vnd.") && !strings.HasPrefix(ext, "comparator-") {
			res.Findings = append(res.Findings, Finding{
				Level: "info", Line: line,
				Message: fmt.Sprintf("require %q is not in the documented Fastmail set — the save-time validator decides", ext),
			})
		}
		if _, ok := used[ext]; !ok && !strings.HasPrefix(ext, "vnd.") && !strings.HasPrefix(ext, "comparator-") {
			res.Findings = append(res.Findings, Finding{
				Level: "info", Line: line,
				Message: fmt.Sprintf("require %q is declared but nothing in this section uses it", ext),
			})
		}
	}

	sort.SliceStable(res.Findings, func(i, j int) bool { return res.Findings[i].Line < res.Findings[j].Line })
	return res
}

// scanSieve tokenizes sieve source, masking # and /* */ comments, quoted
// strings (kept as string tokens), and text: multiline blocks, while
// checking {} () [] balance and terminator sanity.
func scanSieve(src string) ([]token, []Finding) {
	var tokens []token
	var findings []Finding
	line := 1
	var stack []struct {
		ch   byte
		line int
	}
	pairs := map[byte]byte{'}': '{', ')': '(', ']': '['}

	i := 0
	for i < len(src) {
		c := src[i]
		switch {
		case c == '\n':
			line++
			i++
		case c == '#': // line comment
			for i < len(src) && src[i] != '\n' {
				i++
			}
		case c == '/' && i+1 < len(src) && src[i+1] == '*':
			start := line
			i += 2
			for {
				if i+1 >= len(src) {
					findings = append(findings, Finding{Level: "error", Line: start, Message: "unterminated /* comment"})
					i = len(src)
					break
				}
				if src[i] == '\n' {
					line++
				}
				if src[i] == '*' && src[i+1] == '/' {
					i += 2
					break
				}
				i++
			}
		case c == '"':
			start := line
			var sb strings.Builder
			i++
			closed := false
			for i < len(src) {
				if src[i] == '\\' && i+1 < len(src) {
					if src[i+1] == '\n' {
						line++
					}
					sb.WriteByte(src[i+1])
					i += 2
					continue
				}
				if src[i] == '"' {
					i++
					closed = true
					break
				}
				if src[i] == '\n' {
					line++
				}
				sb.WriteByte(src[i])
				i++
			}
			if !closed {
				findings = append(findings, Finding{Level: "error", Line: start, Message: "unterminated string"})
			}
			tokens = append(tokens, token{kind: "string", text: sb.String(), line: start})
		case c == '{' || c == '(' || c == '[':
			stack = append(stack, struct {
				ch   byte
				line int
			}{c, line})
			tokens = append(tokens, token{kind: "punct", text: string(c), line: line})
			i++
		case c == '}' || c == ')' || c == ']':
			if len(stack) == 0 || stack[len(stack)-1].ch != pairs[c] {
				findings = append(findings, Finding{Level: "error", Line: line, Message: fmt.Sprintf("unmatched %q", string(c))})
			} else {
				stack = stack[:len(stack)-1]
			}
			tokens = append(tokens, token{kind: "punct", text: string(c), line: line})
			i++
		case c == ';' || c == ',':
			tokens = append(tokens, token{kind: "punct", text: string(c), line: line})
			i++
		case c == ':' || isWordByte(c):
			start := i
			startLine := line
			kind := "word"
			if c == ':' {
				kind = "tag"
				i++
			}
			for i < len(src) && isWordByte(src[i]) {
				i++
			}
			text := strings.ToLower(src[start:i])
			if kind == "tag" && text == ":" {
				continue // stray colon (already consumed)
			}
			// A bare `text:` introduces a multiline literal running to a
			// line holding only "." — mask it entirely.
			if kind == "word" && text == "text" && i < len(src) && src[i] == ':' {
				i++
				for i < len(src) && src[i] != '\n' {
					i++ // rest of the text: line
				}
				closed := false
				for i < len(src) {
					line++
					i++ // consume the newline
					lineStart := i
					for i < len(src) && src[i] != '\n' {
						i++
					}
					// RFC 5228 §8.1: the terminator is a line holding ONLY
					// "." — an indented dot is body content, not the end.
					if strings.TrimSuffix(src[lineStart:i], "\r") == "." {
						closed = true
						break
					}
					if i >= len(src) {
						break
					}
				}
				if !closed {
					findings = append(findings, Finding{Level: "error", Line: startLine, Message: "unterminated text: block (needs a line holding only \".\")"})
				}
				continue
			}
			tokens = append(tokens, token{kind: kind, text: text, line: startLine})
		default:
			i++
		}
	}
	for _, open := range stack {
		findings = append(findings, Finding{Level: "error", Line: open.line, Message: fmt.Sprintf("unclosed %q", string(open.ch))})
	}
	return tokens, findings
}

func isWordByte(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_'
}
