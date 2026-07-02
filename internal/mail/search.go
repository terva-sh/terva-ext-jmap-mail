package mail

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"terva-ext-jmap-mail/internal/jmap"
)

// SearchParams are the email_search inputs. String fields map 1:1 onto the
// RFC 8621 §4.4.1 FilterCondition properties.
type SearchParams struct {
	Account         string
	Mailbox         string // id, role, path, or display name
	Text            string
	From            string
	To              string
	Cc              string
	Bcc             string
	Subject         string
	Body            string
	After           string // receivedAt lower bound (inclusive)
	Before          string // receivedAt upper bound (exclusive)
	HasAttachment   *bool
	Keyword         string
	NotKeyword      string
	FilterJSON      json.RawMessage // raw RFC 8621 §4.4 filter; exclusive with the fields above except Mailbox
	CollapseThreads bool
	IncludeTotal    bool
	Limit           int
	Position        int
	Sort            string // "newest" (default) | "oldest"
}

// SearchResult is the email_search output: bounded summaries, never bodies.
type SearchResult struct {
	AccountID string         `json:"accountId"`
	Query     map[string]any `json:"query,omitempty"` // echo of the applied filter
	Position  int            `json:"position"`
	Limit     int            `json:"limit"`
	Returned  int            `json:"returned"`
	HasMore   bool           `json:"hasMore"` // more matches beyond position+returned
	Total     *int           `json:"total,omitempty"`
	Emails    []EmailSummary `json:"emails"`
}

const (
	defaultSearchLimit = 20
	maxSearchLimit     = 100
)

// Search runs Email/query + Email/get as one batched request, the query's ids
// feeding the get via an RFC 8620 §3.7 result reference.
func (s *Service) Search(ctx context.Context, p SearchParams) (*SearchResult, error) {
	accountID, sess, err := s.account(ctx, p.Account)
	if err != nil {
		return nil, err
	}

	filter, err := s.buildFilter(ctx, sess, accountID, p)
	if err != nil {
		return nil, err
	}

	limit := p.Limit
	if limit <= 0 {
		limit = defaultSearchLimit
	}
	if limit > maxSearchLimit {
		limit = maxSearchLimit
	}
	position := p.Position
	if position < 0 {
		position = 0
	}
	var ascending bool
	switch p.Sort {
	case "", "newest":
		ascending = false
	case "oldest":
		ascending = true
	default:
		return nil, fmt.Errorf("invalid sort %q: use newest or oldest", p.Sort)
	}

	queryArgs := map[string]any{
		"accountId": accountID,
		"sort":      []map[string]any{{"property": "receivedAt", "isAscending": ascending}},
		"position":  position,
		// One past the requested page so hasMore needs no second query.
		"limit": limit + 1,
	}
	if len(filter) > 0 {
		queryArgs["filter"] = filter
	}
	if p.CollapseThreads {
		queryArgs["collapseThreads"] = true
	}
	if p.IncludeTotal {
		queryArgs["calculateTotal"] = true
	}

	resp, err := s.call(ctx, sess, []jmap.Invocation{
		{Name: "Email/query", Args: queryArgs, CallID: "q0"},
		{Name: "Email/get", Args: map[string]any{
			"accountId":  accountID,
			"#ids":       jmap.ResultReference{ResultOf: "q0", Name: "Email/query", Path: "/ids"},
			"properties": summaryProperties,
		}, CallID: "g1"},
	})
	if err != nil {
		return nil, err
	}

	qres, err := resp.Result("q0")
	if err != nil {
		return nil, err
	}
	var q struct {
		Position int      `json:"position"`
		IDs      []string `json:"ids"`
		Total    *int     `json:"total"`
	}
	if err := json.Unmarshal(qres.Args, &q); err != nil {
		return nil, fmt.Errorf("parse Email/query response: %v", err)
	}

	gres, err := resp.Result("g1")
	if err != nil {
		return nil, err
	}
	var g struct {
		List []rawEmail `json:"list"`
	}
	if err := json.Unmarshal(gres.Args, &g); err != nil {
		return nil, fmt.Errorf("parse Email/get response: %v", err)
	}

	// Email/get MAY return objects in any order (RFC 8620 §5.1); present them
	// in query order, dropping the hasMore-probe extra past the page limit.
	byID := make(map[string]rawEmail, len(g.List))
	for _, e := range g.List {
		byID[e.ID] = e
	}
	hasMore := len(q.IDs) > limit
	ids := q.IDs
	if hasMore {
		ids = ids[:limit]
	}
	emails := make([]EmailSummary, 0, len(ids))
	for _, id := range ids {
		e, ok := byID[id]
		if !ok {
			continue // destroyed between query and get
		}
		emails = append(emails, e.summary(s.mailboxRefsByID(ctx, sess, accountID, e.MailboxIDs)))
	}
	var total *int
	if p.IncludeTotal && q.Total != nil {
		total = q.Total
	}
	return &SearchResult{
		AccountID: accountID,
		Query:     filter,
		Position:  q.Position,
		Limit:     limit,
		Returned:  len(emails),
		HasMore:   hasMore,
		Total:     total,
		Emails:    emails,
	}, nil
}

// buildFilter maps params onto one RFC 8621 §4.4.1 FilterCondition (multiple
// properties in one condition AND together), or passes FilterJSON through
// verbatim — e.g. the query embedded in a Fastmail-generated jmapquery block.
func (s *Service) buildFilter(ctx context.Context, sess *jmap.Session, accountID string, p SearchParams) (map[string]any, error) {
	if len(p.FilterJSON) > 0 {
		return s.buildRawFilter(ctx, sess, accountID, p)
	}
	filter := map[string]any{}
	if p.Mailbox != "" {
		mb, err := s.resolveMailbox(ctx, sess, accountID, p.Mailbox)
		if err != nil {
			return nil, err
		}
		filter["inMailbox"] = mb.ID
	}
	for key, val := range map[string]string{
		"text": p.Text, "from": p.From, "to": p.To, "cc": p.Cc, "bcc": p.Bcc,
		"subject": p.Subject, "body": p.Body,
		"hasKeyword": p.Keyword, "notKeyword": p.NotKeyword,
	} {
		if val != "" {
			filter[key] = val
		}
	}
	if p.After != "" {
		utc, err := jmap.ToUTCDate(p.After)
		if err != nil {
			return nil, fmt.Errorf("after: %v", err)
		}
		filter["after"] = utc
	}
	if p.Before != "" {
		utc, err := jmap.ToUTCDate(p.Before)
		if err != nil {
			return nil, fmt.Errorf("before: %v", err)
		}
		filter["before"] = utc
	}
	if p.HasAttachment != nil {
		filter["hasAttachment"] = *p.HasAttachment
	}
	return filter, nil
}

// buildRawFilter validates the raw filter and combines it with a mailbox
// restriction (the only structured param it composes with) via an RFC 8620
// §5.5 AND operator.
func (s *Service) buildRawFilter(ctx context.Context, sess *jmap.Session, accountID string, p SearchParams) (map[string]any, error) {
	var conflicts []string
	for name, set := range map[string]bool{
		"text": p.Text != "", "from": p.From != "", "to": p.To != "", "cc": p.Cc != "",
		"bcc": p.Bcc != "", "subject": p.Subject != "", "body": p.Body != "",
		"after": p.After != "", "before": p.Before != "",
		"keyword": p.Keyword != "", "notKeyword": p.NotKeyword != "",
		"hasAttachment": p.HasAttachment != nil,
	} {
		if set {
			conflicts = append(conflicts, name)
		}
	}
	if len(conflicts) > 0 {
		sort.Strings(conflicts)
		return nil, fmt.Errorf("filterJson replaces the structured filter params — drop %s or fold them into the JSON (mailbox may combine)", strings.Join(conflicts, ", "))
	}
	var raw map[string]any
	if err := json.Unmarshal(p.FilterJSON, &raw); err != nil {
		return nil, fmt.Errorf("filterJson: not a JSON object: %v", err)
	}
	if raw == nil {
		return nil, fmt.Errorf("filterJson: not a JSON object")
	}
	if p.Mailbox == "" {
		return raw, nil
	}
	mb, err := s.resolveMailbox(ctx, sess, accountID, p.Mailbox)
	if err != nil {
		return nil, err
	}
	return map[string]any{
		"operator":   "AND",
		"conditions": []any{map[string]any{"inMailbox": mb.ID}, raw},
	}, nil
}
