package jmaptest

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

// mailboxGet implements Mailbox/get (RFC 8621 §2.1).
func (s *Server) mailboxGet(args map[string]any) (any, *methodErr) {
	ids, all := idsArg(args)
	props := stringSlice(args["properties"])

	s.mu.Lock()
	defer s.mu.Unlock()
	byID := map[string]*Mailbox{}
	for _, mb := range s.mailboxes {
		byID[mb.ID] = mb
	}
	var targets []*Mailbox
	notFound := []string{}
	if all {
		targets = s.mailboxes
	} else {
		for _, id := range ids {
			if mb, ok := byID[id]; ok {
				targets = append(targets, mb)
			} else {
				notFound = append(notFound, id)
			}
		}
	}
	list := make([]any, 0, len(targets))
	for _, mb := range targets {
		total, unread, totalThr, unreadThr := s.countsLocked(mb.ID)
		m := map[string]any{
			"id":            mb.ID,
			"name":          mb.Name,
			"parentId":      nullableStr(mb.ParentID),
			"role":          nullableStr(mb.Role),
			"sortOrder":     mb.SortOrder,
			"totalEmails":   total,
			"unreadEmails":  unread,
			"totalThreads":  totalThr,
			"unreadThreads": unreadThr,
			"isSubscribed":  true,
		}
		list = append(list, filterProps(m, props))
	}
	return map[string]any{
		"accountId": s.AccountID,
		"state":     s.stateLocked(),
		"list":      list,
		"notFound":  notFound,
	}, nil
}

func nullableStr(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func (s *Server) countsLocked(mailboxID string) (total, unread, totalThreads, unreadThreads int) {
	threads := map[string]bool{}
	unreadThr := map[string]bool{}
	for _, e := range s.emails {
		if !containsStr(e.MailboxIDs, mailboxID) {
			continue
		}
		total++
		threads[e.ThreadID] = true
		if !containsStr(e.Keywords, "$seen") {
			unread++
			unreadThr[e.ThreadID] = true
		}
	}
	return total, unread, len(threads), len(unreadThr)
}

// emailQuery implements Email/query (RFC 8621 §4.4) for FilterConditions and
// nested AND/OR/NOT FilterOperator trees (RFC 8620 §5.5), with receivedAt
// sorting.
func (s *Server) emailQuery(args map[string]any) (any, *methodErr) {
	var filter map[string]any
	if v, present := args["filter"]; present && v != nil {
		m, ok := v.(map[string]any)
		if !ok {
			return nil, &methodErr{Type: "invalidArguments", Description: "filter must be an object"}
		}
		filter = m
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	var matched []*Email
	for _, e := range s.emails {
		ok, merr := s.matchLocked(filter, e)
		if merr != nil {
			return nil, merr
		}
		if ok {
			matched = append(matched, e)
		}
	}

	ascending, merr := sortDirection(args["sort"])
	if merr != nil {
		return nil, merr
	}
	sort.SliceStable(matched, func(i, j int) bool {
		if ascending {
			return matched[i].ReceivedAt.Before(matched[j].ReceivedAt)
		}
		return matched[j].ReceivedAt.Before(matched[i].ReceivedAt)
	})

	if boolArg(args, "collapseThreads") { // RFC 8621 §4.4.3
		seen := map[string]bool{}
		collapsed := matched[:0]
		for _, e := range matched {
			if seen[e.ThreadID] {
				continue
			}
			seen[e.ThreadID] = true
			collapsed = append(collapsed, e)
		}
		matched = collapsed
	}

	total := len(matched)
	position := intArg(args, "position")
	if position < 0 {
		position = 0
	}
	if position > len(matched) {
		position = len(matched)
	}
	matched = matched[position:]
	if limit := intArg(args, "limit"); limit > 0 && limit < len(matched) {
		matched = matched[:limit]
	}

	ids := make([]string, 0, len(matched))
	for _, e := range matched {
		ids = append(ids, e.ID)
	}
	resp := map[string]any{
		"accountId":           s.AccountID,
		"queryState":          "query-0",
		"canCalculateChanges": false,
		"position":            position,
		"ids":                 ids,
	}
	if boolArg(args, "calculateTotal") {
		resp["total"] = total
	}
	return resp, nil
}

// sortDirection reads the sort argument; only the receivedAt property is
// supported. isAscending defaults to true per RFC 8620 §5.5; absent sort
// defaults to newest-first (server-chosen order).
func sortDirection(v any) (bool, *methodErr) {
	comparators, ok := v.([]any)
	if !ok || len(comparators) == 0 {
		return false, nil
	}
	first, ok := comparators[0].(map[string]any)
	if !ok {
		return false, &methodErr{Type: "invalidArguments", Description: "sort comparator must be an object"}
	}
	if prop, _ := first["property"].(string); prop != "receivedAt" {
		return false, &methodErr{Type: "unsupportedSort", Description: "only receivedAt sorting is supported"}
	}
	if asc, present := first["isAscending"].(bool); present {
		return asc, nil
	}
	return true, nil
}

// matchLocked evaluates an RFC 8620 §5.5 Filter — a FilterOperator tree or
// one RFC 8621 §4.4.1 FilterCondition — against a message.
func (s *Server) matchLocked(filter map[string]any, e *Email) (bool, *methodErr) {
	if _, isOperator := filter["operator"]; isOperator {
		return s.matchOperatorLocked(filter, e)
	}
	for key, val := range filter {
		switch key {
		case "conditions":
			return false, &methodErr{Type: "unsupportedFilter", Description: "conditions requires an operator"}
		case "inMailbox":
			id, _ := val.(string)
			if !s.mailboxExistsLocked(id) {
				return false, &methodErr{Type: "invalidArguments", Description: fmt.Sprintf("no mailbox %q", id)}
			}
			if !containsStr(e.MailboxIDs, id) {
				return false, nil
			}
		case "text":
			q, _ := val.(string)
			if !(matchAddrs(e.From, q) || matchAddrs(e.To, q) || matchAddrs(e.Cc, q) || matchAddrs(e.Bcc, q) ||
				containsFold(e.Subject, q) || containsFold(e.TextBody, q)) {
				return false, nil
			}
		case "from":
			if q, _ := val.(string); !matchAddrs(e.From, q) {
				return false, nil
			}
		case "to":
			if q, _ := val.(string); !matchAddrs(e.To, q) {
				return false, nil
			}
		case "cc":
			if q, _ := val.(string); !matchAddrs(e.Cc, q) {
				return false, nil
			}
		case "bcc":
			if q, _ := val.(string); !matchAddrs(e.Bcc, q) {
				return false, nil
			}
		case "subject":
			if q, _ := val.(string); !containsFold(e.Subject, q) {
				return false, nil
			}
		case "body":
			q, _ := val.(string)
			if !containsFold(e.TextBody, q) && !containsFold(e.HTMLBody, q) {
				return false, nil
			}
		case "after": // inclusive lower bound on receivedAt
			t, merr := utcArg(val, key)
			if merr != nil {
				return false, merr
			}
			if e.ReceivedAt.Before(t) {
				return false, nil
			}
		case "before": // exclusive upper bound
			t, merr := utcArg(val, key)
			if merr != nil {
				return false, merr
			}
			if !e.ReceivedAt.Before(t) {
				return false, nil
			}
		case "hasKeyword":
			if kw, _ := val.(string); !containsStr(e.Keywords, kw) {
				return false, nil
			}
		case "notKeyword":
			if kw, _ := val.(string); containsStr(e.Keywords, kw) {
				return false, nil
			}
		case "hasAttachment":
			want, _ := val.(bool)
			if want != (len(e.Attachments) > 0) {
				return false, nil
			}
		default:
			return false, &methodErr{Type: "unsupportedFilter", Description: "unsupported filter property " + key}
		}
	}
	return true, nil
}

// matchOperatorLocked evaluates an RFC 8620 §5.5 FilterOperator: AND (all
// conditions match), OR (at least one), NOT (none). Conditions nest.
func (s *Server) matchOperatorLocked(filter map[string]any, e *Email) (bool, *methodErr) {
	op, _ := filter["operator"].(string)
	conds, ok := filter["conditions"].([]any)
	if !ok || len(conds) == 0 {
		return false, &methodErr{Type: "unsupportedFilter", Description: "operator requires a non-empty conditions array"}
	}
	if len(filter) != 2 {
		return false, &methodErr{Type: "unsupportedFilter", Description: "a FilterOperator holds exactly operator and conditions"}
	}
	if op != "AND" && op != "OR" && op != "NOT" {
		return false, &methodErr{Type: "unsupportedFilter", Description: "unknown operator " + op}
	}
	anyMatched, allMatched := false, true
	for _, c := range conds {
		cond, ok := c.(map[string]any)
		if !ok {
			return false, &methodErr{Type: "unsupportedFilter", Description: "conditions entries must be objects"}
		}
		matched, merr := s.matchLocked(cond, e)
		if merr != nil {
			return false, merr
		}
		anyMatched = anyMatched || matched
		allMatched = allMatched && matched
	}
	switch op {
	case "AND":
		return allMatched, nil
	case "OR":
		return anyMatched, nil
	default: // NOT
		return !anyMatched, nil
	}
}

func (s *Server) mailboxExistsLocked(id string) bool {
	for _, mb := range s.mailboxes {
		if mb.ID == id {
			return true
		}
	}
	return false
}

func utcArg(v any, key string) (time.Time, *methodErr) {
	str, _ := v.(string)
	t, err := time.Parse(time.RFC3339, str)
	if err != nil {
		return time.Time{}, &methodErr{Type: "invalidArguments", Description: key + " is not a UTCDate"}
	}
	return t, nil
}

func matchAddrs(addrs []Address, q string) bool {
	for _, a := range addrs {
		if containsFold(a.Name+" "+a.Email, q) {
			return true
		}
	}
	return false
}

func containsFold(haystack, needle string) bool {
	return strings.Contains(strings.ToLower(haystack), strings.ToLower(needle))
}

// emailGet implements Email/get (RFC 8621 §4.2) including bounded body-value
// fetching.
func (s *Server) emailGet(args map[string]any) (any, *methodErr) {
	ids, all := idsArg(args)
	props := stringSlice(args["properties"])
	fetchAll := boolArg(args, "fetchAllBodyValues")
	fetchText := boolArg(args, "fetchTextBodyValues") || fetchAll
	fetchHTML := boolArg(args, "fetchHTMLBodyValues") || fetchAll
	maxBytes := intArg(args, "maxBodyValueBytes")

	s.mu.Lock()
	defer s.mu.Unlock()
	var targets []*Email
	notFound := []string{}
	if all {
		targets = s.emails
	} else {
		byID := map[string]*Email{}
		for _, e := range s.emails {
			byID[e.ID] = e
		}
		for _, id := range ids {
			if e, ok := byID[id]; ok {
				targets = append(targets, e)
			} else {
				notFound = append(notFound, id)
			}
		}
	}
	list := make([]any, 0, len(targets))
	for _, e := range targets {
		list = append(list, filterProps(emailJSON(e, fetchText, fetchHTML, maxBytes), props))
	}
	return map[string]any{
		"accountId": s.AccountID,
		"state":     s.stateLocked(),
		"list":      list,
		"notFound":  notFound,
	}, nil
}

// emailJSON renders one message per RFC 8621 §4.1/§4.2.
func emailJSON(e *Email, fetchText, fetchHTML bool, maxBytes int) map[string]any {
	mailboxIDs := map[string]any{}
	for _, id := range e.MailboxIDs {
		mailboxIDs[id] = true
	}
	keywords := map[string]any{}
	for _, kw := range e.Keywords {
		keywords[kw] = true
	}
	m := map[string]any{
		"id":            e.ID,
		"threadId":      e.ThreadID,
		"mailboxIds":    mailboxIDs,
		"keywords":      keywords,
		"size":          e.size(),
		"receivedAt":    e.ReceivedAt.UTC().Format("2006-01-02T15:04:05Z"),
		"sentAt":        e.SentAt.Format(time.RFC3339),
		"from":          addrJSON(e.From),
		"to":            addrJSON(e.To),
		"cc":            addrJSON(e.Cc),
		"bcc":           addrJSON(e.Bcc),
		"replyTo":       addrJSON(e.ReplyTo),
		"subject":       e.Subject,
		"hasAttachment": len(e.Attachments) > 0,
		"preview":       preview(e.TextBody),
	}

	textParts := []any{}
	if e.TextBody != "" {
		textParts = append(textParts, map[string]any{
			"partId": "t:" + e.ID, "blobId": "b:t:" + e.ID,
			"type": "text/plain", "charset": "utf-8", "size": len(e.TextBody),
		})
	}
	htmlParts := []any{}
	if e.HTMLBody != "" {
		htmlParts = append(htmlParts, map[string]any{
			"partId": "h:" + e.ID, "blobId": "b:h:" + e.ID,
			"type": "text/html", "charset": "utf-8", "size": len(e.HTMLBody),
		})
	}
	attachments := []any{}
	for i, a := range e.Attachments {
		attachments = append(attachments, map[string]any{
			"partId": fmt.Sprintf("a:%s:%d", e.ID, i), "blobId": fmt.Sprintf("b:a:%s:%d", e.ID, i),
			"type": a.Type, "name": a.Name, "size": a.Size, "disposition": "attachment",
		})
	}
	m["textBody"] = textParts
	m["htmlBody"] = htmlParts
	m["attachments"] = attachments

	bodyValues := map[string]any{}
	if fetchText && e.TextBody != "" {
		bodyValues["t:"+e.ID] = bodyValueJSON(e.TextBody, maxBytes)
	}
	if fetchHTML && e.HTMLBody != "" {
		bodyValues["h:"+e.ID] = bodyValueJSON(e.HTMLBody, maxBytes)
	}
	m["bodyValues"] = bodyValues
	return m
}

func bodyValueJSON(value string, maxBytes int) map[string]any {
	truncated := false
	if maxBytes > 0 && len(value) > maxBytes {
		value = truncUTF8(value, maxBytes)
		truncated = true
	}
	return map[string]any{"value": value, "isTruncated": truncated, "isEncodingProblem": false}
}

func addrJSON(addrs []Address) any {
	if len(addrs) == 0 {
		return nil
	}
	out := make([]any, 0, len(addrs))
	for _, a := range addrs {
		m := map[string]any{"email": a.Email}
		if a.Name != "" {
			m["name"] = a.Name
		} else {
			m["name"] = nil
		}
		out = append(out, m)
	}
	return out
}

func preview(text string) string {
	collapsed := strings.Join(strings.Fields(text), " ")
	if len(collapsed) > 100 {
		collapsed = truncUTF8(collapsed, 100)
	}
	return collapsed
}

// threadGet implements Thread/get (RFC 8621 §3.1): emailIds sorted by
// receivedAt, oldest first.
func (s *Server) threadGet(args map[string]any) (any, *methodErr) {
	ids, all := idsArg(args)

	s.mu.Lock()
	defer s.mu.Unlock()
	if all {
		seen := map[string]bool{}
		for _, e := range s.emails {
			if !seen[e.ThreadID] {
				seen[e.ThreadID] = true
				ids = append(ids, e.ThreadID)
			}
		}
	}
	list := make([]any, 0, len(ids))
	notFound := []string{}
	for _, id := range ids {
		var members []*Email
		for _, e := range s.emails {
			if e.ThreadID == id {
				members = append(members, e)
			}
		}
		if len(members) == 0 {
			notFound = append(notFound, id)
			continue
		}
		sort.SliceStable(members, func(i, j int) bool { return members[i].ReceivedAt.Before(members[j].ReceivedAt) })
		emailIDs := make([]string, 0, len(members))
		for _, e := range members {
			emailIDs = append(emailIDs, e.ID)
		}
		list = append(list, map[string]any{"id": id, "emailIds": emailIDs})
	}
	return map[string]any{
		"accountId": s.AccountID,
		"state":     s.stateLocked(),
		"list":      list,
		"notFound":  notFound,
	}, nil
}
