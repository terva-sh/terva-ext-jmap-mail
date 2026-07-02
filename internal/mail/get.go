package mail

import (
	"context"
	"encoding/json"
	"fmt"

	"terva-ext-jmap-mail/internal/jmap"
)

// Body formats for email_get.
const (
	BodyText     = "text"
	BodyHTML     = "html"
	BodyBoth     = "both"
	BodyMetadata = "metadata" // headers/summary only, no body fetch
)

// maxGetIDs bounds one email_get call (also well under any server's
// maxObjectsInGet).
const maxGetIDs = 20

// GetParams are the email_get inputs.
type GetParams struct {
	Account         string
	IDs             []string
	BodyFormat      string // text (default) | html | both | metadata
	MaxBodyBytes    int    // per message; 0 → config default; capped at config max
	IncludeFullUrls bool   // skip URL redaction (tokens/queries stay in bodies)
}

// GetResult is the email_get output.
type GetResult struct {
	AccountID string      `json:"accountId"`
	Emails    []EmailFull `json:"emails"`
	NotFound  []string    `json:"notFound,omitempty"`
}

// Get fetches messages by id via Email/get (RFC 8621 §4.2), with body values
// bounded by maxBodyValueBytes and a local per-message budget.
func (s *Service) Get(ctx context.Context, p GetParams) (*GetResult, error) {
	if len(p.IDs) == 0 {
		return nil, fmt.Errorf("ids is required")
	}
	if len(p.IDs) > maxGetIDs {
		return nil, fmt.Errorf("too many ids (%d): fetch at most %d per call", len(p.IDs), maxGetIDs)
	}
	format := p.BodyFormat
	if format == "" {
		format = BodyText
	}
	switch format {
	case BodyText, BodyHTML, BodyBoth, BodyMetadata:
	default:
		return nil, fmt.Errorf("invalid bodyFormat %q: use text, html, both, or metadata", format)
	}
	budget := s.bodyBudget(p.MaxBodyBytes)

	accountID, sess, err := s.account(ctx, p.Account)
	if err != nil {
		return nil, err
	}

	args := map[string]any{
		"accountId":  accountID,
		"ids":        p.IDs,
		"properties": fullProperties,
	}
	if format != BodyMetadata {
		props := append(append([]string{}, fullProperties...), "bodyValues")
		if format == BodyText || format == BodyBoth {
			props = append(props, "textBody")
			args["fetchTextBodyValues"] = true
		}
		if format == BodyHTML || format == BodyBoth {
			props = append(props, "htmlBody")
			args["fetchHTMLBodyValues"] = true
		}
		args["properties"] = props
		args["maxBodyValueBytes"] = budget
	}

	resp, err := s.call(ctx, sess, []jmap.Invocation{{Name: "Email/get", Args: args, CallID: "g0"}})
	if err != nil {
		return nil, err
	}
	res, err := resp.Result("g0")
	if err != nil {
		return nil, err
	}
	var out struct {
		List     []rawEmail `json:"list"`
		NotFound []string   `json:"notFound"`
	}
	if err := json.Unmarshal(res.Args, &out); err != nil {
		return nil, fmt.Errorf("parse Email/get response: %v", err)
	}

	emails := make([]EmailFull, 0, len(out.List))
	for _, e := range out.List {
		emails = append(emails, s.fullEmail(ctx, sess, accountID, e, format, budget, !p.IncludeFullUrls))
	}
	return &GetResult{AccountID: accountID, Emails: emails, NotFound: out.NotFound}, nil
}

// bodyBudget resolves the effective per-message body byte budget: the
// requested value when smaller, the configured bound otherwise.
func (s *Service) bodyBudget(requested int) int {
	budget := s.cfg.MaxBodyBytes
	if requested > 0 && requested < budget {
		budget = requested
	}
	return budget
}

func (s *Service) fullEmail(ctx context.Context, sess *jmap.Session, accountID string, e rawEmail, format string, budget int, redact bool) EmailFull {
	full := EmailFull{
		EmailSummary: e.summary(s.mailboxRefsByID(ctx, sess, accountID, e.MailboxIDs)),
		Cc:           addresses(e.Cc),
		Bcc:          addresses(e.Bcc),
		ReplyTo:      addresses(e.ReplyTo),
	}
	for _, a := range e.Attachments {
		full.Attachments = append(full.Attachments, AttachmentInfo{Name: a.Name, Type: a.Type, Size: a.Size})
	}
	if format == BodyText || format == BodyBoth {
		full.BodyText, full.BodyTextTruncated = assembleBody(e.TextBody, e.BodyValues, budget)
		if redact {
			var n int
			full.BodyText, n = redactURLs(full.BodyText)
			full.RedactedURLs += n
		}
	}
	if format == BodyHTML || format == BodyBoth {
		full.BodyHTML, full.BodyHTMLTruncated = assembleBody(e.HTMLBody, e.BodyValues, budget)
		if redact {
			var n int
			full.BodyHTML, n = redactURLs(full.BodyHTML)
			full.RedactedURLs += n
		}
	}
	// Redaction marks can nudge a body a few bytes past the budget; keep the
	// "never more than max_body_bytes" contract honest.
	if len(full.BodyText) > budget {
		full.BodyText, full.BodyTextTruncated = truncateUTF8(full.BodyText, budget), true
	}
	if len(full.BodyHTML) > budget {
		full.BodyHTML, full.BodyHTMLTruncated = truncateUTF8(full.BodyHTML, budget), true
	}
	return full
}
