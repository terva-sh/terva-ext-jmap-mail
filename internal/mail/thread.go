package mail

import (
	"context"
	"encoding/json"
	"fmt"

	"terva-ext-jmap-mail/internal/jmap"
)

// ThreadParams are the email_get_thread inputs: exactly one of ThreadID /
// EmailID identifies the thread.
type ThreadParams struct {
	Account         string
	ThreadID        string
	EmailID         string
	IncludeBodies   bool     // fetch bounded text bodies instead of summaries
	Fields          []string // projection over the summary form; not with IncludeBodies
	Limit           int      // messages to return, newest end of the thread; 0 = the cap
	MaxBodyBytes    int
	IncludeFullUrls bool // skip URL redaction in fetched bodies
}

const (
	// maxThreadSummaries / maxThreadBodies bound one email_get_thread result
	// the way maxGetIDs bounds email_get. A thread is an unbounded object: a
	// long mailing-list thread fetched with bodies is the largest payload
	// this extension can produce, and it needs neither a bulk operation nor
	// an elevated access_level to reach.
	maxThreadSummaries = 100
	maxThreadBodies    = maxGetIDs
)

// ThreadResult is the email_get_thread output.
type ThreadResult struct {
	AccountID string `json:"accountId"`
	ThreadID  string `json:"threadId"`
	Count     int    `json:"count"`    // messages in the thread
	Returned  int    `json:"returned"` // messages in this result
	// Omitted counts messages dropped by the cap — always the OLDEST ones, so
	// the reply you asked about is never the one that goes missing. Fetch them
	// by id with email_get if the older end of the thread matters.
	Omitted int            `json:"omitted,omitempty"`
	Emails  []EmailSummary `json:"emails,omitempty"`
	Full    []EmailFull    `json:"emailsWithBodies,omitempty"`
	// SelectionID names EVERY message in the thread — including the ones the
	// cap omitted from this result. That is safe here in a way it is not for a
	// grouped scan: Thread/get returns the complete emailIds list regardless of
	// how many messages are then fetched, so the handle is the whole thread and
	// not the part that was displayed. Archiving a 150-message thread after
	// reading the newest 100 of it is therefore one call, and correct.
	SelectionID string `json:"selectionId,omitempty"`
	// SelectionNote says so, because "acts on more than you were shown" is
	// exactly the kind of thing a caller must not have to infer.
	SelectionNote string `json:"selectionNote,omitempty"`
}

// threadSelectionNote is used when the display was capped, where the handle
// covering more than the result is the surprising and useful part.
const threadSelectionNote = "selectionId names all %d messages in the thread, including the %d not shown here — Thread/get returns the whole list whatever the display cap. Pass it to email_move / email_mark / email_trash to act on the entire thread."

const threadSelectionNoteFull = "selectionId names this thread's messages: pass it to email_move / email_mark / email_trash to act on the whole thread without resending ids."

// GetThread fetches a thread's messages in one batched request, chained with
// RFC 8620 §3.7 result references:
//
//	by threadId: Thread/get → Email/get (path /list/*/emailIds)
//	by emailId:  Email/get(threadId) → Thread/get (path /list/*/threadId) → Email/get
//
// At most limit messages are returned, newest end first-served. A result
// reference cannot be sliced, so the provider still sends the whole thread —
// the cap bounds what reaches the caller, which is where an unbounded thread
// actually does damage.
func (s *Service) GetThread(ctx context.Context, p ThreadParams) (*ThreadResult, error) {
	if (p.ThreadID == "") == (p.EmailID == "") {
		return nil, fmt.Errorf("provide exactly one of threadId or emailId")
	}
	fields, err := parseFields(p.Fields)
	if err != nil {
		return nil, err
	}
	if fields.projected() && p.IncludeBodies {
		return nil, fmt.Errorf("fields projects the summary form; with includeBodies the bodies dominate the result and a projection saves nothing — drop one")
	}
	accountID, sess, err := s.account(ctx, p.Account)
	if err != nil {
		return nil, err
	}
	budget := s.bodyBudget(p.MaxBodyBytes)

	limit := maxThreadSummaries
	if p.IncludeBodies {
		limit = maxThreadBodies
	}
	if p.Limit > 0 && p.Limit < limit {
		limit = p.Limit
	}

	finalProps := fields.properties()
	finalArgs := map[string]any{"accountId": accountID}
	if p.IncludeBodies {
		finalProps = append(append([]string{}, fullProperties...), "bodyValues", "textBody")
		finalArgs["fetchTextBodyValues"] = true
		finalArgs["maxBodyValueBytes"] = budget
	}
	finalArgs["properties"] = finalProps

	var calls []jmap.Invocation
	if p.ThreadID != "" {
		calls = append(calls, jmap.Invocation{
			Name:   "Thread/get",
			Args:   map[string]any{"accountId": accountID, "ids": []string{p.ThreadID}},
			CallID: "t0",
		})
	} else {
		calls = append(calls,
			jmap.Invocation{
				Name:   "Email/get",
				Args:   map[string]any{"accountId": accountID, "ids": []string{p.EmailID}, "properties": []string{"threadId"}},
				CallID: "e0",
			},
			jmap.Invocation{
				Name: "Thread/get",
				Args: map[string]any{
					"accountId": accountID,
					"#ids":      jmap.ResultReference{ResultOf: "e0", Name: "Email/get", Path: "/list/*/threadId"},
				},
				CallID: "t0",
			},
		)
	}
	finalArgs["#ids"] = jmap.ResultReference{ResultOf: "t0", Name: "Thread/get", Path: "/list/*/emailIds"}
	calls = append(calls, jmap.Invocation{Name: "Email/get", Args: finalArgs, CallID: "g9"})

	resp, err := s.call(ctx, sess, calls)
	if err != nil {
		return nil, err
	}

	tres, err := resp.Result("t0")
	if err != nil {
		return nil, err
	}
	var t struct {
		List []struct {
			ID       string   `json:"id"`
			EmailIDs []string `json:"emailIds"`
		} `json:"list"`
		NotFound []string `json:"notFound"`
	}
	if err := json.Unmarshal(tres.Args, &t); err != nil {
		return nil, fmt.Errorf("parse Thread/get response: %v", err)
	}
	if len(t.List) == 0 {
		ref := p.ThreadID
		if ref == "" {
			ref = p.EmailID
		}
		return nil, fmt.Errorf("no thread found for %q", ref)
	}

	gres, err := resp.Result("g9")
	if err != nil {
		return nil, err
	}
	var g struct {
		List []rawEmail `json:"list"`
	}
	if err := json.Unmarshal(gres.Args, &g); err != nil {
		return nil, fmt.Errorf("parse Email/get response: %v", err)
	}

	// RFC 8621 §3: a Thread's emailIds are ordered by receivedAt, so they are
	// both the order to present in (Email/get MAY answer in any order, RFC
	// 8620 §5.1) and the right axis to trim on.
	threadIDs := t.List[0].EmailIDs
	byID := make(map[string]rawEmail, len(g.List))
	for _, e := range g.List {
		byID[e.ID] = e
	}
	selected := threadIDs
	if len(selected) > limit {
		selected = selected[len(selected)-limit:] // keep the most recent
	}

	result := &ThreadResult{
		AccountID: accountID,
		ThreadID:  t.List[0].ID,
		Count:     len(threadIDs),
		Omitted:   len(threadIDs) - len(selected),
	}
	for _, id := range selected {
		e, ok := byID[id]
		if !ok {
			continue // destroyed between the thread read and the get
		}
		if p.IncludeBodies {
			result.Full = append(result.Full, s.fullEmail(ctx, sess, accountID, e, BodyText, budget, !p.IncludeFullUrls, nil))
			continue
		}
		var refs []MailboxRef
		if fields.has("mailboxes") {
			refs = s.mailboxRefsByID(ctx, sess, accountID, e.MailboxIDs)
		}
		result.Emails = append(result.Emails, e.summaryWith(refs, fields))
	}
	result.Returned = len(result.Emails) + len(result.Full)
	// A thread is a set worth naming, and the tools that consume a handle are
	// withdrawn at read-only — where the token would be noise in every result.
	if len(threadIDs) > 0 && s.cfg.AllowOrganize() {
		result.SelectionID = s.handles.putSelection(&selection{
			AccountID: accountID,
			IDs:       append([]string(nil), threadIDs...),
		})
		if result.Omitted > 0 {
			result.SelectionNote = fmt.Sprintf(threadSelectionNote, len(threadIDs), result.Omitted)
		} else {
			result.SelectionNote = threadSelectionNoteFull
		}
	}
	return result, nil
}
