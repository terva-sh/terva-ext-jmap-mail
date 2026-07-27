package mail

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"

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
	// Handle and IDs are two ways to name the same thing and are mutually
	// exclusive, exactly as on the organize tools. A handle here is only a
	// NAME — a read authorizes nothing — so either prefix is accepted: sel_
	// from a search, rcp_ from a dry run, or the failure and remainder handles
	// a mutating result hands back, which is what makes "show me the ones that
	// failed" a call rather than a reconstruction.
	Handle          string
	SelectionOffset int
	IDs             []string
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
	// Selection reports which slice of a handle this call read, when a handle
	// named the set. Unlike the organize tools, the cap here does NOT rise for
	// a handle: theirs bounds argument payload, which a handle avoids, while
	// this one bounds the bodies and subjects coming back, which it does not.
	// So a handle naming more than maxGetIDs is read in slices, and remaining
	// says whether to come back.
	Selection *SelectionUse `json:"selection,omitempty"`
}

// Get fetches messages by id via Email/get (RFC 8621 §4.2), with body values
// bounded by maxBodyValueBytes and a local per-message budget.
func (s *Service) Get(ctx context.Context, p GetParams) (*GetResult, error) {
	ids, use, err := s.resolveRead(p.Handle, p.SelectionOffset, p.IDs)
	if err != nil {
		return nil, err
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
		"ids":        ids,
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
	return &GetResult{AccountID: accountID, Emails: emails, NotFound: out.NotFound, Selection: use}, nil
}

// resolveRead turns the two ways of naming a read set into one id list. It is
// the read-side twin of resolveTargets, and differs in the two ways that
// matter: any handle kind is accepted, because a read authorizes nothing; and
// the slice is bounded by maxGetIDs rather than the handle cap, because what
// this call sends back is message content and a handle does nothing to shrink
// that.
func (s *Service) resolveRead(handle string, offset int, rawIDs []string) ([]string, *SelectionUse, error) {
	ids := make([]string, 0, len(rawIDs))
	for _, id := range rawIDs {
		if id = strings.TrimSpace(id); id != "" {
			ids = append(ids, id)
		}
	}
	handle = strings.TrimSpace(handle)
	switch {
	case handle == "" && len(ids) == 0:
		return nil, nil, fmt.Errorf("name the messages: handle (any sel_ or rcp_ id — including the failure handles a mutating result hands back), or ids")
	case handle != "" && len(ids) > 0:
		return nil, nil, fmt.Errorf("pass a handle or ids, not both — each names the whole set on its own. "+
			"To use the handle, send ids as []: {\"handle\": %q}. To use the ids, send handle as \"\". "+
			"An empty array, an empty string, and an absent key all mean \"not this one\"", handle)
	}
	if handle == "" {
		if len(ids) > maxGetIDs {
			return nil, nil, fmt.Errorf("too many ids (%d): fetch at most %d per call", len(ids), maxGetIDs)
		}
		return ids, nil, nil
	}

	_, held, err := s.handles.namedSet(handle)
	if err != nil {
		return nil, nil, err
	}
	if offset < 0 {
		offset = 0
	}
	if offset >= len(held) {
		return nil, nil, fmt.Errorf("selectionOffset %d is at or past the end of handle %q (%d ids) — it is fully read", offset, handle, len(held))
	}
	slice := held[offset:]
	if len(slice) > maxGetIDs {
		slice = slice[:maxGetIDs]
	}
	return slice, &SelectionUse{
		ID:        handle,
		Offset:    offset,
		Count:     len(slice),
		Remaining: len(held) - offset - len(slice),
	}, nil
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
