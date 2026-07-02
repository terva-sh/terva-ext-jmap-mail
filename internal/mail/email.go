package mail

import (
	"sort"
	"strings"
	"unicode/utf8"
)

// Address is a tool-facing email address.
type Address struct {
	Name  string `json:"name,omitempty"`
	Email string `json:"email"`
}

// MailboxRef annotates a mailbox id with its name/role when known.
type MailboxRef struct {
	ID   string `json:"id"`
	Name string `json:"name,omitempty"`
	Path string `json:"path,omitempty"` // only when nested (path != name)
	Role string `json:"role,omitempty"`
}

// EmailSummary is the bounded, body-free shape returned by search and thread
// listings (RFC 8621 §4.1 metadata + convenience properties).
type EmailSummary struct {
	ID            string       `json:"id"`
	ThreadID      string       `json:"threadId,omitempty"`
	Mailboxes     []MailboxRef `json:"mailboxes,omitempty"`
	Keywords      []string     `json:"keywords,omitempty"`
	From          []Address    `json:"from,omitempty"`
	To            []Address    `json:"to,omitempty"`
	Subject       string       `json:"subject,omitempty"`
	ReceivedAt    string       `json:"receivedAt,omitempty"`
	SentAt        string       `json:"sentAt,omitempty"`
	HasAttachment bool         `json:"hasAttachment,omitempty"`
	Size          uint64       `json:"size,omitempty"`
	Preview       string       `json:"preview,omitempty"`
}

// AttachmentInfo is attachment metadata only — no content download in MVP.
type AttachmentInfo struct {
	Name string `json:"name,omitempty"`
	Type string `json:"type,omitempty"`
	Size uint64 `json:"size,omitempty"`
}

// EmailFull extends the summary with recipients and (optionally) bounded body
// text for email_get / email_get_thread.
type EmailFull struct {
	EmailSummary
	Cc                []Address        `json:"cc,omitempty"`
	Bcc               []Address        `json:"bcc,omitempty"`
	ReplyTo           []Address        `json:"replyTo,omitempty"`
	Attachments       []AttachmentInfo `json:"attachments,omitempty"`
	BodyText          string           `json:"bodyText,omitempty"`
	BodyTextTruncated bool             `json:"bodyTextTruncated,omitempty"`
	BodyHTML          string           `json:"bodyHtml,omitempty"`
	BodyHTMLTruncated bool             `json:"bodyHtmlTruncated,omitempty"`
	// RedactedURLs counts links whose query strings / tokens were stripped
	// from the bodies above (the default; includeFullUrls disables).
	RedactedURLs int `json:"redactedUrls,omitempty"`
}

// summaryProperties are the Email/get properties for summaries — safe metadata
// only, never body parts.
var summaryProperties = []string{
	"id", "threadId", "mailboxIds", "keywords", "size", "receivedAt", "sentAt",
	"from", "to", "subject", "hasAttachment", "preview",
}

// fullProperties extend summaries for email_get (still body-free; body parts
// are requested separately by bodyFormat).
var fullProperties = append(append([]string{}, summaryProperties...),
	"cc", "bcc", "replyTo", "attachments")

// rawEmail mirrors the RFC 8621 §4.1 Email object fields we consume.
type rawEmail struct {
	ID            string               `json:"id"`
	ThreadID      string               `json:"threadId"`
	MailboxIDs    map[string]bool      `json:"mailboxIds"`
	Keywords      map[string]bool      `json:"keywords"`
	Size          uint64               `json:"size"`
	ReceivedAt    string               `json:"receivedAt"`
	SentAt        string               `json:"sentAt"`
	From          []rawAddress         `json:"from"`
	To            []rawAddress         `json:"to"`
	Cc            []rawAddress         `json:"cc"`
	Bcc           []rawAddress         `json:"bcc"`
	ReplyTo       []rawAddress         `json:"replyTo"`
	Subject       string               `json:"subject"`
	HasAttachment bool                 `json:"hasAttachment"`
	Preview       string               `json:"preview"`
	BodyValues    map[string]bodyValue `json:"bodyValues"`
	TextBody      []bodyPart           `json:"textBody"`
	HTMLBody      []bodyPart           `json:"htmlBody"`
	Attachments   []bodyPart           `json:"attachments"`
}

type rawAddress struct {
	Name  string `json:"name"`
	Email string `json:"email"`
}

// bodyPart is an RFC 8621 §4.1.4 EmailBodyPart (fields we consume).
type bodyPart struct {
	PartID string `json:"partId"`
	BlobID string `json:"blobId"`
	Type   string `json:"type"`
	Name   string `json:"name"`
	Size   uint64 `json:"size"`
}

// bodyValue is an RFC 8621 §4.1.4 EmailBodyValue.
type bodyValue struct {
	Value             string `json:"value"`
	IsTruncated       bool   `json:"isTruncated"`
	IsEncodingProblem bool   `json:"isEncodingProblem"`
}

func addresses(raw []rawAddress) []Address {
	if len(raw) == 0 {
		return nil
	}
	out := make([]Address, 0, len(raw))
	for _, a := range raw {
		out = append(out, Address{Name: a.Name, Email: a.Email})
	}
	return out
}

func keywordList(m map[string]bool) []string {
	var out []string
	for k, set := range m {
		if set {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}

func (e rawEmail) summary(mailboxes []MailboxRef) EmailSummary {
	// Previews are server-generated body snippets and routinely open with a
	// tokened unsubscribe/sign-in URL, so they redact unconditionally — a
	// summary is never the surface for retrieving a working link (email_get
	// with includeFullUrls is).
	preview, _ := redactURLs(e.Preview)
	return EmailSummary{
		ID:            e.ID,
		ThreadID:      e.ThreadID,
		Mailboxes:     mailboxes,
		Keywords:      keywordList(e.Keywords),
		From:          addresses(e.From),
		To:            addresses(e.To),
		Subject:       e.Subject,
		ReceivedAt:    e.ReceivedAt,
		SentAt:        e.SentAt,
		HasAttachment: e.HasAttachment,
		Size:          e.Size,
		Preview:       preview,
	}
}

// assembleBody concatenates the body parts' fetched values (RFC 8621 §4.2:
// walk textBody/htmlBody against bodyValues by partId) under a total byte
// budget. Truncation is reported when the server truncated any value
// (isTruncated) or the budget cut the concatenation locally.
func assembleBody(parts []bodyPart, values map[string]bodyValue, budget int) (string, bool) {
	var b strings.Builder
	truncated := false
	for _, p := range parts {
		v, ok := values[p.PartID]
		if !ok {
			continue
		}
		if v.IsTruncated {
			truncated = true
		}
		remaining := budget - b.Len()
		if remaining <= 0 {
			truncated = true
			break
		}
		val := v.Value
		if b.Len() > 0 && val != "" {
			b.WriteString("\n")
			remaining--
		}
		if len(val) > remaining {
			val = truncateUTF8(val, remaining)
			truncated = true
		}
		b.WriteString(val)
	}
	return b.String(), truncated
}

// truncateUTF8 cuts s to at most n bytes without splitting a rune.
func truncateUTF8(s string, n int) string {
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
