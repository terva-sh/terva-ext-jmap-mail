package mail

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"

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
	Account string
	IDs     []string
	// Fields projects the result down to named properties (empty = every one,
	// the pre-projection shape). It is the complete answer to "what do I want
	// back", bodies included: a projection naming neither bodyText nor
	// bodyHtml returns metadata only, whatever BodyFormat says.
	Fields          []string
	BodyFormat      string // text (default) | html | both | metadata; ignored under a projection
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
	fields, err := parseFullFields(p.Fields)
	if err != nil {
		return nil, err
	}
	// A projection names everything the caller wants, so it decides the bodies
	// too and bodyFormat stops applying. Refusing the combination instead
	// would be unusable: bodyFormat's inert value resolves to text, so a model
	// that pads every property would be told its projection conflicts with a
	// body format it never chose (the shape of TW-027).
	if fields.projected() {
		switch {
		case fields["bodyText"] && fields["bodyHtml"]:
			format = BodyBoth
		case fields["bodyText"]:
			format = BodyText
		case fields["bodyHtml"]:
			format = BodyHTML
		default:
			format = BodyMetadata
		}
	}
	budget := s.bodyBudget(p.MaxBodyBytes)

	accountID, sess, err := s.account(ctx, p.Account)
	if err != nil {
		return nil, err
	}

	props := fullProperties
	if fields.projected() {
		props = fields.properties()
	}
	args := map[string]any{
		"accountId":  accountID,
		"ids":        p.IDs,
		"properties": props,
	}
	if format != BodyMetadata {
		props = append(append([]string{}, props...), "bodyValues")
		if format == BodyText || format == BodyBoth {
			if !slices.Contains(props, "textBody") {
				props = append(props, "textBody")
			}
			args["fetchTextBodyValues"] = true
		}
		if format == BodyHTML || format == BodyBoth {
			if !slices.Contains(props, "htmlBody") {
				props = append(props, "htmlBody")
			}
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
		emails = append(emails, s.fullEmail(ctx, sess, accountID, e, format, budget, !p.IncludeFullUrls, fields))
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

func (s *Service) fullEmail(ctx context.Context, sess *jmap.Session, accountID string, e rawEmail, format string, budget int, redact bool, f fieldSet) EmailFull {
	// A projection that drops mailboxes drops the Mailbox/get behind them too:
	// the annotation is the one summary property that costs a second call.
	var refs []MailboxRef
	if f.has("mailboxes") {
		refs = s.mailboxRefsByID(ctx, sess, accountID, e.MailboxIDs)
	}
	full := e.fullWith(refs, f)
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
