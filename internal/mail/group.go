package mail

// email_group: the distribution of a filtered set, computed here, returned as
// numbers.
//
// "Who is filling this mailbox, and how much" is the first question a triage
// session asks and the one email_search cannot answer — a search counts one
// filter at a time, and a distribution is not a count. So an agent approximates
// it: dump the 200 newest, the 200 oldest and a slice of the list-tagged mail
// with a from/subject/receivedAt projection, eyeball the frequent senders, then
// issue a counter per sender to turn the guesses into numbers. One fleet
// session spent 244,664 bytes on that — 92% of its entire mail payload — and
// 42 of its 62 searches, and the listings stayed resident in context for every
// turn afterwards: 81% of the session's cost fell after they landed (TW-047).
//
// The sample is also wrong in a way nobody can see. Ranking senders from the
// newest 200 and oldest 200 of a 3,166-message mailbox ranks them from 12% of
// it, taken from both ends. A steady mid-volume sender appears in neither
// window, so no counter is ever issued for it, and the nineteen exact counts
// that come back say nothing about the choice of nineteen.
//
// Fastmail's JMAP has no faceting method, so this aggregates over the matched
// set here. That is precisely the argument for doing it in the extension: the
// same work in the agent is what costs 244,664 bytes, because there every
// intermediate message crosses the tool boundary. Here nothing crosses it but
// the groups.
//
// The result therefore carries no message content beyond the grouping key
// itself, and always states what it was computed over, so a ranking taken from
// a truncated scan is visibly a ranking taken from a truncated scan.

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"terva-ext-jmap-mail/internal/jmap"
)

// Values for GroupParams.GroupBy.
const (
	GroupByFrom       = "from"
	GroupByReceivedAt = "receivedAt"
)

// Values for GroupParams.Interval (receivedAt grouping).
const (
	IntervalMonth = "month"
	IntervalWeek  = "week"
	IntervalDay   = "day"
)

const (
	// defaultGroupScan bounds how many messages are aggregated when the caller
	// names no bound. Chosen to cover the mailbox sizes these surveys actually
	// run against (the reported Inbox was 3,166) inside one tool timeout,
	// while leaving a five-figure archive visibly truncated rather than
	// silently slow.
	defaultGroupScan = 5000
	// maxGroupScan is the ceiling a caller can raise maxMessages to. Past this
	// the scan stops being a survey and starts being an export.
	maxGroupScan = 20000
	// defaultGroupLimit bounds the ranked rows returned. A survey acts on the
	// head of the distribution; the tail is a number (otherGroups), not a list.
	defaultGroupLimit = 25
	maxGroupLimit     = 200
	// groupGetChunk is the fallback per-Email/get batch when the session
	// advertises no maxObjectsInGet.
	groupGetChunk = 500
)

// GroupParams are the email_group inputs. The filter half is email_search's,
// so a caller that has already framed a search can group the same set without
// restating it in another dialect.
type GroupParams struct {
	Account string
	Filter  SearchParams
	// GroupBy names the distribution. "" is accepted by the schema so a
	// padding model reaches the tool and reads which values exist, and refused
	// here: there is no sensible default distribution.
	GroupBy string
	// Interval buckets receivedAt grouping. Ignored for from.
	Interval    string
	GroupLimit  int
	MaxMessages int
	// Handle groups a set that is already named instead of running a query:
	// any sel_ or rcp_ id, including a failure group's. "Are my four hundred
	// failures all one sender" is a question about a set nobody can express as
	// a filter — JMAP has no id condition — so without this it cannot be asked
	// at all. Mutually exclusive with the filter fields.
	Handle string
}

// Group is one row of a distribution.
type Group struct {
	// Key is the sender address, or the bucket start date (YYYY-MM-DD) for
	// receivedAt. Display names are deliberately absent: they are third-party
	// text, they vary between messages from the same sender, and the address
	// is both shorter and the thing a filter rule would match on.
	Key    string `json:"key"`
	Total  int    `json:"total"`
	Unread int    `json:"unread"`
	// Newest and Oldest bound the group in time, which is what says whether a
	// sender is a live problem or a settled backlog — the distinction a survey
	// is trying to draw, and one a bare count cannot.
	Newest string `json:"newest,omitempty"`
	Oldest string `json:"oldest,omitempty"`
	// SelectionID names this group's messages, so acting on a row is the next
	// call rather than a re-search. The fold already held these ids; throwing
	// them away meant "archive everything from the top sender" cost another
	// query per row, which is the shape this tool exists to remove, one level
	// up. Absent on a truncated scan — see groupHandleNote.
	SelectionID string `json:"selectionId,omitempty"`

	// ids accumulates the members while folding. Never serialized: the whole
	// point is that they leave as a handle or not at all.
	ids []string
}

// GroupResult is the email_group output. It is counts only: no subject, no
// preview, no message id, and no display name.
type GroupResult struct {
	AccountID  string         `json:"accountId"`
	Query      map[string]any `json:"query,omitempty"`
	GroupBy    string         `json:"groupBy"`
	Interval   string         `json:"interval,omitempty"`
	QueryState string         `json:"queryState,omitempty"`
	// Matched is how many messages the filter matched; Scanned is how many were
	// actually aggregated. They differ only when the scan was bounded, and the
	// pair is what makes a truncated ranking legible as one. A survey that
	// reports a ranking without them is the sampling problem again, dressed as
	// an aggregate.
	Matched   int  `json:"matched"`
	Scanned   int  `json:"scanned"`
	Truncated bool `json:"truncated,omitempty"`
	// Groups is the ranked head, biggest first.
	Groups []Group `json:"groups"`
	// DistinctKeys is how many groups exist in the scanned set; OtherTotal is
	// how many messages fall outside the returned rows. Together they say what
	// the tail weighs without listing it.
	DistinctKeys int    `json:"distinctKeys"`
	OtherTotal   int    `json:"otherTotal,omitempty"`
	Note         string `json:"note,omitempty"`
	// HandleNote explains the per-group selectionIds, or their absence.
	HandleNote string `json:"handleNote,omitempty"`
}

const groupTruncatedNote = "the ranking was computed over the newest %d of %d matching messages, so it ranks that window and not the whole set — raise maxMessages, or narrow the filter, before treating the order as the mailbox's"

// groupHandleNote explains what the per-group handles are for.
const groupHandleNote = "each group carries a selectionId naming exactly its messages: pass it to email_move / email_mark / email_trash to act on that sender or bucket, or to email_get to look at some of them. No re-search needed."

// groupTruncatedHandleNote is why they are absent on a truncated scan, and it
// is a correctness note rather than a limitation. A handle over a scanned
// window would name part of a group while reading as the whole of it — so
// "archive everything from this sender" would archive some of it, silently,
// which is the sampling failure this tool was built to end. Refusing to mint
// is the only version of this that cannot mislead.
const groupTruncatedHandleNote = "no per-group selectionIds: the scan was truncated, so a handle would name only the messages inside the scanned window while reading as the whole group. Raise maxMessages or narrow the filter to get handles you can safely act on."

// Group aggregates the matched set into a ranked distribution. One Email/query
// establishes the set and its state, then the ids are fetched in chunks with
// only the properties the grouping needs, and everything is folded here.
func (s *Service) Group(ctx context.Context, p GroupParams) (*GroupResult, error) {
	groupBy, interval, err := parseGrouping(p.GroupBy, p.Interval)
	if err != nil {
		return nil, err
	}
	handle := strings.TrimSpace(p.Handle)
	if handle != "" && p.Filter.hasAnyFilter() {
		return nil, fmt.Errorf("pass a handle or a filter, not both — each names the whole set on its own. "+
			"To group the handle's messages, send no filter fields: {\"handle\": %q}. To group a query, send handle as \"\"", handle)
	}
	accountID, sess, err := s.account(ctx, p.Account)
	if err != nil {
		return nil, err
	}
	scan := boundScan(p.MaxMessages)
	groupLimit := boundGroupLimit(p.GroupLimit)

	result := &GroupResult{
		AccountID: accountID, GroupBy: groupBy, Interval: interval, Groups: []Group{},
	}
	var ids []string
	if handle != "" {
		held, err := s.groupNamedSet(accountID, handle)
		if err != nil {
			return nil, err
		}
		// No query ran, so there is no queryState to report and nothing to
		// pretend one against: a handle IS the set, pinned when it was minted.
		result.Matched = len(held)
		ids = held
		if len(ids) > scan {
			ids = ids[:scan]
		}
	} else {
		filter, err := s.buildFilter(ctx, sess, accountID, p.Filter)
		if err != nil {
			return nil, err
		}
		result.Query = filter
		queryArgs := map[string]any{
			"accountId":      accountID,
			"sort":           []map[string]any{{"property": "receivedAt", "isAscending": false}},
			"limit":          scan,
			"calculateTotal": true,
		}
		if len(filter) > 0 {
			queryArgs["filter"] = filter
		}
		if p.Filter.CollapseThreads {
			queryArgs["collapseThreads"] = true
		}
		resp, err := s.call(ctx, sess, []jmap.Invocation{{
			Name: "Email/query", Args: queryArgs, CallID: "q0",
		}})
		if err != nil {
			return nil, err
		}
		qres, err := resp.Result("q0")
		if err != nil {
			return nil, err
		}
		var q struct {
			IDs        []string `json:"ids"`
			Total      *int     `json:"total"`
			QueryState string   `json:"queryState"`
		}
		if err := json.Unmarshal(qres.Args, &q); err != nil {
			return nil, fmt.Errorf("parse Email/query response: %v", err)
		}
		result.QueryState = q.QueryState
		result.Matched = len(q.IDs)
		if q.Total != nil {
			result.Matched = *q.Total
		}
		ids = q.IDs
	}

	result.Scanned = len(ids)
	if result.Matched > result.Scanned {
		result.Truncated = true
		result.Note = fmt.Sprintf(groupTruncatedNote, result.Scanned, result.Matched)
	}
	if len(ids) == 0 {
		return result, nil
	}

	buckets, err := s.foldGroups(ctx, sess, accountID, ids, groupBy, interval)
	if err != nil {
		return nil, err
	}
	result.DistinctKeys = len(buckets)
	ranked := rankGroups(buckets)
	if len(ranked) > groupLimit {
		for _, g := range ranked[groupLimit:] {
			result.OtherTotal += g.Total
		}
		ranked = ranked[:groupLimit]
	}
	s.mintGroupHandles(result, accountID, ranked)
	result.Groups = ranked
	return result, nil
}

// groupNamedSet resolves a handle for grouping, refusing one minted for a
// different account the same way the mutating path does.
func (s *Service) groupNamedSet(accountID, handle string) ([]string, error) {
	owner, ids, err := s.handles.namedSet(handle)
	if err != nil {
		return nil, err
	}
	if owner != accountID {
		return nil, fmt.Errorf("handle %q was minted for account %s, not %s", handle, owner, accountID)
	}
	return ids, nil
}

// mintGroupHandles names each returned group, so acting on a row is one call.
//
// Not on a truncated scan: a handle over a window would name part of a group
// while reading as all of it, and "archive everything from this sender" would
// then archive some of it, silently. And not at read-only access level, where
// the tools that could consume a handle are withdrawn and the token would be
// noise in every result.
func (s *Service) mintGroupHandles(result *GroupResult, accountID string, groups []Group) {
	if len(groups) == 0 {
		return
	}
	if result.Truncated {
		result.HandleNote = groupTruncatedHandleNote
		return
	}
	if !s.cfg.AllowOrganize() {
		return
	}
	for i := range groups {
		groups[i].SelectionID = s.handles.putSelection(&selection{
			AccountID: accountID,
			IDs:       append([]string(nil), groups[i].ids...),
		})
	}
	result.HandleNote = groupHandleNote
}

// foldGroups fetches only the properties the grouping needs and accumulates
// them, never holding a message beyond the fold. The chunk size is the
// server's own maxObjectsInGet: exceeding it is a request-level error, and a
// survey that fails at message 501 is worse than one that takes two round
// trips.
func (s *Service) foldGroups(ctx context.Context, sess *jmap.Session, accountID string, ids []string, groupBy, interval string) (map[string]*Group, error) {
	props := []string{"id", "receivedAt", "keywords"}
	if groupBy == GroupByFrom {
		props = append(props, "from")
	}
	chunk := groupGetChunk
	if core, ok := sess.CoreLimits(); ok && core.MaxObjectsInGet > 0 {
		chunk = int(core.MaxObjectsInGet)
	}
	perRequest := 1
	if core, ok := sess.CoreLimits(); ok && core.MaxCallsInRequest > 1 {
		perRequest = int(core.MaxCallsInRequest) - 1
	}

	buckets := map[string]*Group{}
	for start := 0; start < len(ids); {
		// Pack as many gets into one request as the server allows, so a
		// 5,000-message scan is a handful of round trips rather than ten.
		var calls []jmap.Invocation
		for len(calls) < perRequest && start < len(ids) {
			end := start + chunk
			if end > len(ids) {
				end = len(ids)
			}
			calls = append(calls, jmap.Invocation{
				Name: "Email/get",
				Args: map[string]any{
					"accountId": accountID, "ids": ids[start:end], "properties": props,
				},
				CallID: fmt.Sprintf("g%d", len(calls)),
			})
			start = end
		}
		resp, err := s.call(ctx, sess, calls)
		if err != nil {
			return nil, err
		}
		for i := range calls {
			res, err := resp.Result(fmt.Sprintf("g%d", i))
			if err != nil {
				return nil, err
			}
			var got struct {
				List []rawEmail `json:"list"`
			}
			if err := json.Unmarshal(res.Args, &got); err != nil {
				return nil, fmt.Errorf("parse Email/get response: %v", err)
			}
			for _, e := range got.List {
				foldOne(buckets, e, groupBy, interval)
			}
		}
	}
	return buckets, nil
}

// foldOne adds one message to its bucket. A message with several From
// addresses counts once per address, the way a message in several mailboxes
// counts once per mailbox in a move's source breakdown — the alternative is
// picking one silently.
func foldOne(buckets map[string]*Group, e rawEmail, groupBy, interval string) {
	unread := !e.Keywords["$seen"]
	add := func(key string) {
		if key == "" {
			key = "(none)"
		}
		g := buckets[key]
		if g == nil {
			g = &Group{Key: key}
			buckets[key] = g
		}
		g.ids = append(g.ids, e.ID)
		g.Total++
		if unread {
			g.Unread++
		}
		if e.ReceivedAt != "" {
			if g.Newest == "" || e.ReceivedAt > g.Newest {
				g.Newest = e.ReceivedAt
			}
			if g.Oldest == "" || e.ReceivedAt < g.Oldest {
				g.Oldest = e.ReceivedAt
			}
		}
	}
	switch groupBy {
	case GroupByFrom:
		if len(e.From) == 0 {
			add("")
			return
		}
		seen := map[string]bool{}
		for _, a := range e.From {
			key := strings.ToLower(strings.TrimSpace(a.Email))
			if seen[key] {
				continue
			}
			seen[key] = true
			add(key)
		}
	case GroupByReceivedAt:
		add(bucketDate(e.ReceivedAt, interval))
	}
}

// bucketDate truncates an RFC 3339 receivedAt to its interval start. Day and
// month need only the fixed-width date portion; week needs a real calendar, so
// only that case parses. A value too short or unparseable buckets as
// "(unknown)" rather than being dropped — a histogram that quietly omits
// messages does not sum to the total beside it.
func bucketDate(receivedAt, interval string) string {
	if len(receivedAt) < 10 {
		return "(unknown)"
	}
	date := receivedAt[:10] // YYYY-MM-DD
	switch interval {
	case IntervalDay:
		return date
	case IntervalWeek:
		t, err := time.Parse("2006-01-02", date)
		if err != nil {
			return "(unknown)"
		}
		// ISO weeks start Monday; Go's Weekday puts Sunday at 0.
		back := (int(t.Weekday()) + 6) % 7
		return t.AddDate(0, 0, -back).Format("2006-01-02")
	default: // month
		return date[:7] + "-01"
	}
}

// rankGroups orders biggest first, then by key, so the same mailbox always
// renders the same bytes and a tie is not decided by map iteration.
func rankGroups(buckets map[string]*Group) []Group {
	out := make([]Group, 0, len(buckets))
	for _, g := range buckets {
		out = append(out, *g)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Total != out[j].Total {
			return out[i].Total > out[j].Total
		}
		return out[i].Key < out[j].Key
	})
	return out
}

// parseGrouping normalizes the two enums. "" is a value the schema permits so
// that a padding model gets this sentence instead of a validation failure it
// never sees, and an interval on a from-grouping is dropped rather than
// refused — it is inert there, not a conflicting instruction.
func parseGrouping(groupBy, interval string) (string, string, error) {
	switch strings.TrimSpace(strings.ToLower(groupBy)) {
	case GroupByFrom:
		return GroupByFrom, "", nil
	case strings.ToLower(GroupByReceivedAt):
		switch strings.TrimSpace(strings.ToLower(interval)) {
		case "", IntervalMonth:
			return GroupByReceivedAt, IntervalMonth, nil
		case IntervalWeek:
			return GroupByReceivedAt, IntervalWeek, nil
		case IntervalDay:
			return GroupByReceivedAt, IntervalDay, nil
		default:
			return "", "", fmt.Errorf("invalid interval %q: use month, week, or day (or \"\" for month)", interval)
		}
	case "":
		return "", "", fmt.Errorf("groupBy is required: use %q for the sender distribution, or %q with an interval for an age histogram. There is no default — a distribution nobody asked for is not one worth returning", GroupByFrom, GroupByReceivedAt)
	default:
		return "", "", fmt.Errorf("invalid groupBy %q: use %q or %q", groupBy, GroupByFrom, GroupByReceivedAt)
	}
}

func boundScan(n int) int {
	if n <= 0 {
		return defaultGroupScan
	}
	if n > maxGroupScan {
		return maxGroupScan
	}
	return n
}

func boundGroupLimit(n int) int {
	if n <= 0 {
		return defaultGroupLimit
	}
	if n > maxGroupLimit {
		return maxGroupLimit
	}
	return n
}
