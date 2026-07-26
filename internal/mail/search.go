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
	Fields          []string // projection: EmailSummary properties to return; empty = all
	// ReturnIDs governs whether the matched ids come back at all:
	// "" / "all" (the default) as today, "none" for the handle and the counts
	// alone, "boundaries" for the handle plus the page's first and last id.
	ReturnIDs string
	Limit     int
	Position  int
	Sort      string // "newest" (default) | "oldest"
}

// Values for SearchParams.ReturnIDs.
const (
	ReturnIDsAll        = "all"
	ReturnIDsNone       = "none"
	ReturnIDsBoundaries = "boundaries"
)

// SearchResult is the email_search output: bounded summaries, never bodies.
type SearchResult struct {
	AccountID string         `json:"accountId"`
	Query     map[string]any `json:"query,omitempty"` // echo of the applied filter
	Position  int            `json:"position"`
	Limit     int            `json:"limit"` // page size applied — below the request if the server capped it
	Returned  int            `json:"returned"`
	HasMore   bool           `json:"hasMore"` // more matches beyond position+returned
	Total     *int           `json:"total,omitempty"`
	// QueryState is the server's opaque state for this query (RFC 8620 §5.5).
	// It changes when the matching set changes, so a caller paging through a
	// backlog while mutating it can tell that its cohort moved underneath it
	// rather than discovering the gap later, or never.
	QueryState string `json:"queryState,omitempty"`
	// IDs carries an id-only projection flat, instead of as an array of
	// single-key objects. Indented, `[{"id":"…"}]` costs about forty bytes per
	// message against nineteen for the bare string — on the 200-id pages bulk
	// organization pages at, that difference is the larger part of the result.
	// Set only when the projection is exactly ["id"]; Emails is then omitted.
	// returnIds:"none" omits it too — a selection handle already names the set,
	// so on the bulk path the array is redundant with the one field that
	// replaced it.
	IDs    []string       `json:"ids,omitempty"`
	Emails []EmailSummary `json:"emails,omitempty"`
	// FirstID / LastID are the page's boundary ids under
	// returnIds:"boundaries". A bounded wave proves placement afterwards by
	// sampling where each batch began and ended, which costs two ids rather
	// than the two hundred that suppressing the array just saved.
	FirstID string `json:"firstId,omitempty"`
	LastID  string `json:"lastId,omitempty"`
	// SelectionID names this result's ordered id set so a following
	// email_move / email_mark / email_trash can operate on it without the
	// caller retransmitting the ids. Minted only when the organization tools
	// are available to use it. See handles.go.
	SelectionID string `json:"selectionId,omitempty"`
}

const (
	defaultSearchLimit = 20
	maxSearchLimit     = 100
	// maxProjectedSearchLimit applies when a projection drops preview — the
	// cap exists to bound payload, and preview is nearly all of it. Bulk
	// organization ("give me ids, move them") pages at maxSetIDs, so one
	// projected page covers more than one mutating batch.
	maxProjectedSearchLimit = 500
)

// Search runs Email/query + Email/get as one batched request, the query's ids
// feeding the get via an RFC 8620 §3.7 result reference. A Fields projection
// narrows the get's properties — and an id-only one drops the get entirely,
// which is the shape bulk organization actually needs.
func (s *Service) Search(ctx context.Context, p SearchParams) (*SearchResult, error) {
	accountID, sess, err := s.account(ctx, p.Account)
	if err != nil {
		return nil, err
	}

	filter, err := s.buildFilter(ctx, sess, accountID, p)
	if err != nil {
		return nil, err
	}

	fields, err := parseFields(p.Fields)
	if err != nil {
		return nil, err
	}
	returnIDs, err := parseReturnIDs(p.ReturnIDs)
	if err != nil {
		return nil, err
	}
	// Suppressing the ids means no per-message property is returned at all, so
	// the projection cannot also apply — it is overridden rather than in
	// conflict. Refusing the combination would refuse a caller that padded
	// fields with [], which is the inert value the schema promises.
	if returnIDs != ReturnIDsAll {
		fields = fieldSet{"id": true}
	}

	limit := p.Limit
	if limit <= 0 {
		limit = defaultSearchLimit
	}
	maxLimit := maxSearchLimit
	if fields.projected() && !fields.has("preview") {
		maxLimit = maxProjectedSearchLimit
	}
	if limit > maxLimit {
		limit = maxLimit
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

	// An id-only projection needs no Email/get at all: Email/query already
	// returns exactly what was asked for.
	calls := []jmap.Invocation{{Name: "Email/query", Args: queryArgs, CallID: "q0"}}
	if !fields.idOnly() {
		calls = append(calls, jmap.Invocation{Name: "Email/get", Args: map[string]any{
			"accountId":  accountID,
			"#ids":       jmap.ResultReference{ResultOf: "q0", Name: "Email/query", Path: "/ids"},
			"properties": fields.properties(),
		}, CallID: "g1"})
	}
	resp, err := s.call(ctx, sess, calls)
	if err != nil {
		return nil, err
	}

	qres, err := resp.Result("q0")
	if err != nil {
		return nil, err
	}
	var q struct {
		Position   int      `json:"position"`
		IDs        []string `json:"ids"`
		Total      *int     `json:"total"`
		QueryState string   `json:"queryState"`
		// Limit is the server's own cap, reported only when it enforced one
		// (RFC 8620 §5.5).
		Limit *int `json:"limit"`
	}
	if err := json.Unmarshal(qres.Args, &q); err != nil {
		return nil, fmt.Errorf("parse Email/query response: %v", err)
	}

	// The query asked for limit+1 as the hasMore probe. A server that enforces
	// a smaller limit of its own truncates the probe away, so reading "fewer
	// than limit+1 ids" as "no more matches" would silently end a paging loop
	// mid-backlog. Where the server capped, fall back to "a full page means
	// assume more" — over-reporting hasMore costs one empty page; the other
	// way round costs unnoticed messages.
	page := limit
	hasMore := len(q.IDs) > limit
	if q.Limit != nil && *q.Limit <= limit {
		page = *q.Limit
		hasMore = len(q.IDs) >= page
	}
	ids := q.IDs
	if len(ids) > page {
		ids = ids[:page]
	}

	var emails []EmailSummary
	returned := len(ids)
	if fields.idOnly() {
		// Nothing to shape: the ids go out flat, in IDs.
	} else {
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
		// Email/get MAY return objects in any order (RFC 8620 §5.1); present
		// them in query order.
		byID := make(map[string]rawEmail, len(g.List))
		for _, e := range g.List {
			byID[e.ID] = e
		}
		emails = make([]EmailSummary, 0, len(ids))
		for _, id := range ids {
			e, ok := byID[id]
			if !ok {
				continue // destroyed between query and get
			}
			var refs []MailboxRef
			if fields.has("mailboxes") {
				refs = s.mailboxRefsByID(ctx, sess, accountID, e.MailboxIDs)
			}
			emails = append(emails, e.summaryWith(refs, fields))
		}
		// A message destroyed between the query and the get is not in the
		// result, so it must not be in the selection either.
		surviving := make([]string, 0, len(emails))
		for _, e := range emails {
			surviving = append(surviving, e.ID)
		}
		ids, returned = surviving, len(emails)
	}
	var total *int
	if p.IncludeTotal && q.Total != nil {
		total = q.Total
	}
	result := &SearchResult{
		AccountID:  accountID,
		Query:      filter,
		Position:   q.Position,
		Limit:      page,
		Returned:   returned,
		HasMore:    hasMore,
		Total:      total,
		QueryState: q.QueryState,
		Emails:     emails,
	}
	if fields.idOnly() && returnIDs == ReturnIDsAll {
		result.IDs = ids
	}
	if returnIDs == ReturnIDsBoundaries && len(ids) > 0 {
		result.FirstID, result.LastID = ids[0], ids[len(ids)-1]
	}
	// Mint a handle for the set so the next mutating call can name it. Only
	// worth doing when that call is even possible: at read-only access level
	// the organization tools are withdrawn, and an unusable token in every
	// search result is noise.
	if len(ids) > 0 && s.cfg.AllowOrganize() {
		result.SelectionID = s.handles.putSelection(&selection{
			AccountID: accountID,
			IDs:       append([]string(nil), ids...),
		})
	}
	return result, nil
}

// parseReturnIDs normalizes the id-return mode. "" is deliberately a value
// rather than an absence: the flag exists to be left alone by a caller that
// fills every declared property, and its padded form must mean "as today".
func parseReturnIDs(mode string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(mode)) {
	case "", ReturnIDsAll:
		return ReturnIDsAll, nil
	case ReturnIDsNone:
		return ReturnIDsNone, nil
	case ReturnIDsBoundaries:
		return ReturnIDsBoundaries, nil
	default:
		return "", fmt.Errorf("invalid returnIds %q: use all (the default), none, or boundaries", mode)
	}
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
