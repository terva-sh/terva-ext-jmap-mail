package mail

// email_count: several labeled filters, counted in one request against one
// query state.
//
// A survey's deliverable is a table whose rows have to sum to a total. Taking
// those rows one search at a time gives each row its own queryState and its
// own moment, so a message arriving mid-survey lands in one row and not the
// total, and nothing in the results says so. A fleet session measured 3,166
// Inbox messages against a cleanup that had closed at 3,165 and caught it only
// because the agent had written itself a Python assertion in a shell to check
// a mail tool's arithmetic. Fifty-seven of that session's sixty-two searches
// were counters (TW-048).
//
// The shape being replaced is `{limit: 1, includeTotal: true, returnIds:
// "none"}` — fetch a message, read the envelope, discard the message, and mint
// a 15-minute selection handle nobody will use. Repeated once per row.
//
// What this does instead: one request, one Email/query per row with
// calculateTotal and no page, every row therefore evaluated against the same
// server state. No message fetched, no selection minted, no page size in the
// argument surface at all.
//
// Deliberately NOT built: redefining email_search's `limit: 0` as "count
// only". Zero is what a padding model sends for an integer, and a zero value
// that means something is the trap TW-027 and TW-031 were both filed about.
// `0 → the default page of 20` stays correct; counting simply does not belong
// in a tool that takes a page size.

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"terva-ext-jmap-mail/internal/jmap"
)

// maxCountQueries is the fallback bound on rows per call, used when the
// session advertises no maxCallsInRequest. The real bound is the server's,
// read per call — see countBatchLimit.
const maxCountQueries = 16

// CountQuery is one row: a label the caller chose, and the filter to count.
type CountQuery struct {
	Label string
	// Filter supplies the filter half of email_search's parameters. Only the
	// filtering fields are read (mailbox, text, from/to/cc/bcc, subject, body,
	// after/before, keyword/notKeyword, hasAttachment, filterJson,
	// collapseThreads); a page size or projection has no meaning for a count
	// and is ignored rather than refused, so the same decoder serves both
	// tools and cannot drift from it.
	Filter SearchParams
}

// CountRow is one counted row.
type CountRow struct {
	Label string `json:"label"`
	Total int    `json:"total"`
	// Query echoes the filter that produced the count, so a table can state
	// what each row actually measured rather than what the caller meant to ask.
	Query map[string]any `json:"query,omitempty"`
	// Error is set when this row alone failed — a bad filter in row 4 must not
	// discard the eleven rows that worked.
	Error string `json:"error,omitempty"`
}

// CountResult is the email_count output.
type CountResult struct {
	AccountID string     `json:"accountId"`
	Counts    []CountRow `json:"counts"`
	// QueryState is the state every row was evaluated against, and the reason
	// this tool exists: it is what lets a caller say "these numbers are one
	// observation" rather than "these numbers were true at various times".
	QueryState string `json:"queryState,omitempty"`
	// StatesDiffered is set when the rows did NOT all report the same state,
	// which means the mailbox changed mid-request. The counts are then still
	// each correct and no longer guaranteed to reconcile with each other, and
	// that is exactly the thing a caller must not have to infer.
	StatesDiffered bool   `json:"statesDiffered,omitempty"`
	Note           string `json:"note,omitempty"`
}

const countDriftNote = "the mailbox changed while these were being counted, so the rows are not one observation and may not sum — re-run to get a consistent set"

// Count evaluates every query in one request. Methods in a JMAP batch execute
// in order against the same store (RFC 8620 §3.2), so the rows share a moment
// in a way N separate calls cannot; the returned states are compared rather
// than assumed, because sharing a request is not the same as the spec
// promising a shared state.
func (s *Service) Count(ctx context.Context, p CountParams) (*CountResult, error) {
	accountID, sess, err := s.account(ctx, p.Account)
	if err != nil {
		return nil, err
	}
	queries, err := validateCountQueries(p.Queries, countBatchLimit(sess))
	if err != nil {
		return nil, err
	}

	// Filters resolve first, and a failure here is fatal rather than per-row:
	// resolving a mailbox name is a lookup the caller can fix, and half a
	// table is worse than a refusal that says which row is wrong.
	calls := make([]jmap.Invocation, 0, len(queries))
	filters := make([]map[string]any, len(queries))
	for i, q := range queries {
		filter, err := s.buildFilter(ctx, sess, accountID, q.Filter)
		if err != nil {
			return nil, fmt.Errorf("query %d (%q): %v", i+1, q.Label, err)
		}
		filters[i] = filter
		args := map[string]any{
			"accountId": accountID,
			// The whole point: a total without a page. limit 0 asks the server
			// for no ids at all (RFC 8620 §5.5 — limit is an UnsignedInt, and
			// zero results is what zero means). A lenient server that returns
			// them anyway costs bytes on the wire and nothing in the
			// transcript, because nothing here reads them.
			"limit":          0,
			"calculateTotal": true,
		}
		if len(filter) > 0 {
			args["filter"] = filter
		}
		if q.Filter.CollapseThreads {
			args["collapseThreads"] = true
		}
		calls = append(calls, jmap.Invocation{
			Name: "Email/query", Args: args, CallID: fmt.Sprintf("c%d", i),
		})
	}

	resp, err := s.call(ctx, sess, calls)
	if err != nil {
		return nil, err
	}

	result := &CountResult{AccountID: accountID, Counts: make([]CountRow, 0, len(queries))}
	states := map[string]bool{}
	for i, q := range queries {
		row := CountRow{Label: q.Label, Query: filters[i]}
		res, err := resp.Result(fmt.Sprintf("c%d", i))
		if err != nil {
			row.Error = err.Error()
			result.Counts = append(result.Counts, row)
			continue
		}
		var got struct {
			Total      *int   `json:"total"`
			QueryState string `json:"queryState"`
		}
		if err := json.Unmarshal(res.Args, &got); err != nil {
			row.Error = fmt.Sprintf("parse Email/query response: %v", err)
			result.Counts = append(result.Counts, row)
			continue
		}
		if got.Total == nil {
			// calculateTotal was asked for; a server that declines it leaves
			// the row without the one thing it exists to carry, and reporting
			// 0 would be a number the caller would put in a table.
			row.Error = "the server returned no total for this query (calculateTotal unsupported or declined)"
			result.Counts = append(result.Counts, row)
			continue
		}
		row.Total = *got.Total
		if got.QueryState != "" {
			states[got.QueryState] = true
			result.QueryState = got.QueryState
		}
		result.Counts = append(result.Counts, row)
	}
	if len(states) > 1 {
		result.StatesDiffered = true
		result.QueryState = ""
		result.Note = countDriftNote
	}
	return result, nil
}

// CountParams are the email_count inputs.
type CountParams struct {
	Account string
	Queries []CountQuery
}

// countBatchLimit is how many rows fit in one request, which is the real bound
// on this tool: the guarantee it sells is that every row shared a request, so
// splitting across two would silently sell something else. One slot is held
// back for headroom, since the session's figure is the whole request's budget.
func countBatchLimit(sess *jmap.Session) int {
	limit := maxCountQueries
	if core, ok := sess.CoreLimits(); ok && core.MaxCallsInRequest > 1 {
		limit = int(core.MaxCallsInRequest) - 1
	}
	if limit < 1 {
		limit = 1
	}
	return limit
}

// validateCountQueries drops the padded rows a model sends and refuses the
// ones that cannot be counted. A row with no label is refused rather than
// numbered: the label is how a caller matches a number to the question it
// asked, and an invented one would be matched to the wrong question.
func validateCountQueries(in []CountQuery, max int) ([]CountQuery, error) {
	out := make([]CountQuery, 0, len(in))
	labels := map[string]bool{}
	for i, q := range in {
		q.Label = strings.TrimSpace(q.Label)
		// A wholly blank row is padding, not a request to count everything.
		if q.Label == "" && !q.Filter.hasAnyFilter() {
			continue
		}
		if q.Label == "" {
			return nil, fmt.Errorf("query %d has a filter but no label — the label is how a number is matched back to the question, so it cannot be supplied by the tool", i+1)
		}
		if labels[q.Label] {
			return nil, fmt.Errorf("two queries are labeled %q — a table with two rows of the same name cannot be read", q.Label)
		}
		labels[q.Label] = true
		out = append(out, q)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("queries is empty: pass at least one {label, filter}. Counting a whole mailbox is one row with a filter naming it, e.g. {\"label\":\"inbox\",\"filter\":{\"mailbox\":\"inbox\"}}")
	}
	if len(out) > max {
		return nil, fmt.Errorf("too many queries (%d): this server takes at most %d per request, and the point of the tool is that every row shares one. Split the table into runs of %d — rows within a run still reconcile, and each run reports the state it was taken against",
			len(out), max, max)
	}
	return out, nil
}

// hasAnyFilter reports whether a filter block names anything at all, so a
// padded row of empty strings is recognised as padding rather than counted as
// "every message in the account".
func (p SearchParams) hasAnyFilter() bool {
	if len(p.FilterJSON) > 0 && strings.TrimSpace(string(p.FilterJSON)) != "{}" && strings.TrimSpace(string(p.FilterJSON)) != "null" {
		return true
	}
	if p.HasAttachment != nil {
		return true
	}
	for _, v := range []string{
		p.Mailbox, p.Text, p.From, p.To, p.Cc, p.Bcc, p.Subject, p.Body,
		p.After, p.Before, p.Keyword, p.NotKeyword,
	} {
		if strings.TrimSpace(v) != "" {
			return true
		}
	}
	return false
}
