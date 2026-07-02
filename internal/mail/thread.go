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
	IncludeBodies   bool // fetch bounded text bodies instead of summaries
	MaxBodyBytes    int
	IncludeFullUrls bool // skip URL redaction in fetched bodies
}

// ThreadResult is the email_get_thread output.
type ThreadResult struct {
	AccountID string         `json:"accountId"`
	ThreadID  string         `json:"threadId"`
	Count     int            `json:"count"`
	Emails    []EmailSummary `json:"emails,omitempty"`
	Full      []EmailFull    `json:"emailsWithBodies,omitempty"`
}

// GetThread fetches every message of a thread in one batched request, chained
// with RFC 8620 §3.7 result references:
//
//	by threadId: Thread/get → Email/get (path /list/*/emailIds)
//	by emailId:  Email/get(threadId) → Thread/get (path /list/*/threadId) → Email/get
func (s *Service) GetThread(ctx context.Context, p ThreadParams) (*ThreadResult, error) {
	if (p.ThreadID == "") == (p.EmailID == "") {
		return nil, fmt.Errorf("provide exactly one of threadId or emailId")
	}
	accountID, sess, err := s.account(ctx, p.Account)
	if err != nil {
		return nil, err
	}
	budget := s.bodyBudget(p.MaxBodyBytes)

	finalProps := summaryProperties
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

	result := &ThreadResult{AccountID: accountID, ThreadID: t.List[0].ID, Count: len(g.List)}
	for _, e := range g.List {
		if p.IncludeBodies {
			result.Full = append(result.Full, s.fullEmail(ctx, sess, accountID, e, BodyText, budget, !p.IncludeFullUrls))
		} else {
			result.Emails = append(result.Emails, e.summary(s.mailboxRefsByID(ctx, sess, accountID, e.MailboxIDs)))
		}
	}
	return result, nil
}
