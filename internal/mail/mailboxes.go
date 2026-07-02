package mail

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"terva-ext-jmap-mail/internal/jmap"
)

// Mailbox is the tool-facing mailbox shape (RFC 8621 §2, fields we surface).
type Mailbox struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	// Path is the display path from the root, e.g. "Inbox/Gaming/Star
	// Citizen" — computed from the parentId chain, not a server field.
	Path          string `json:"path"`
	ParentID      string `json:"parentId,omitempty"`
	Role          string `json:"role,omitempty"`
	SortOrder     int    `json:"sortOrder,omitempty"`
	TotalEmails   *int   `json:"totalEmails,omitempty"`
	UnreadEmails  *int   `json:"unreadEmails,omitempty"`
	TotalThreads  *int   `json:"totalThreads,omitempty"`
	UnreadThreads *int   `json:"unreadThreads,omitempty"`
}

// MailboxList is the email_list_mailboxes result.
type MailboxList struct {
	AccountID string    `json:"accountId"`
	Mailboxes []Mailbox `json:"mailboxes"`
}

// ListMailboxesParams are the email_list_mailboxes inputs.
type ListMailboxesParams struct {
	Account       string
	IncludeCounts bool
}

// mailboxCacheTTL bounds reuse of the mailbox list for name/role resolution;
// list calls always fetch fresh (counts go stale quickly).
const mailboxCacheTTL = 5 * time.Minute

var mailboxBaseProps = []string{"id", "name", "parentId", "role", "sortOrder"}
var mailboxCountProps = []string{"totalEmails", "unreadEmails", "totalThreads", "unreadThreads"}

// ListMailboxes fetches all mailboxes for the account via Mailbox/get
// (RFC 8621 §2.1), sorted parent-first then by sortOrder/name.
func (s *Service) ListMailboxes(ctx context.Context, p ListMailboxesParams) (*MailboxList, error) {
	accountID, sess, err := s.account(ctx, p.Account)
	if err != nil {
		return nil, err
	}
	list, err := s.fetchMailboxes(ctx, sess, accountID, p.IncludeCounts)
	if err != nil {
		return nil, err
	}
	return &MailboxList{AccountID: accountID, Mailboxes: list}, nil
}

func (s *Service) fetchMailboxes(ctx context.Context, sess *jmap.Session, accountID string, includeCounts bool) ([]Mailbox, error) {
	props := mailboxBaseProps
	if includeCounts {
		props = append(append([]string{}, mailboxBaseProps...), mailboxCountProps...)
	}
	resp, err := s.call(ctx, sess, []jmap.Invocation{{
		Name:   "Mailbox/get",
		Args:   map[string]any{"accountId": accountID, "ids": nil, "properties": props},
		CallID: "m0",
	}})
	if err != nil {
		return nil, err
	}
	res, err := resp.Result("m0")
	if err != nil {
		return nil, err
	}
	var out struct {
		List []Mailbox `json:"list"`
	}
	if err := json.Unmarshal(res.Args, &out); err != nil {
		return nil, fmt.Errorf("parse Mailbox/get response: %v", err)
	}
	computePaths(out.List)
	sortMailboxes(out.List)

	s.mu.Lock()
	s.mailboxes[accountID] = mailboxCache{list: out.List, fetchedAt: time.Now()}
	s.mu.Unlock()
	return out.List, nil
}

// computePaths fills each mailbox's display path by walking its parentId
// chain. A missing parent roots the path at the deepest known ancestor; a
// cycle (malformed server data) falls back to the bare name.
func computePaths(list []Mailbox) {
	byID := make(map[string]*Mailbox, len(list))
	for i := range list {
		byID[list[i].ID] = &list[i]
	}
	for i := range list {
		names := []string{list[i].Name}
		seen := map[string]bool{list[i].ID: true}
		for parent := list[i].ParentID; parent != ""; {
			p, ok := byID[parent]
			if !ok {
				break
			}
			if seen[p.ID] {
				names = names[len(names)-1:] // cycle: keep the bare name
				break
			}
			seen[p.ID] = true
			names = append([]string{p.Name}, names...)
			parent = p.ParentID
		}
		list[i].Path = strings.Join(names, "/")
	}
}

func sortMailboxes(list []Mailbox) {
	sort.SliceStable(list, func(i, j int) bool {
		if list[i].SortOrder != list[j].SortOrder {
			return list[i].SortOrder < list[j].SortOrder
		}
		return strings.ToLower(list[i].Name) < strings.ToLower(list[j].Name)
	})
}

// cachedMailboxes returns the account's mailbox list for resolution, from
// cache when fresh.
func (s *Service) cachedMailboxes(ctx context.Context, sess *jmap.Session, accountID string) ([]Mailbox, error) {
	s.mu.Lock()
	c, ok := s.mailboxes[accountID]
	s.mu.Unlock()
	if ok && time.Since(c.fetchedAt) < mailboxCacheTTL {
		return c.list, nil
	}
	return s.fetchMailboxes(ctx, sess, accountID, false)
}

// resolveMailbox maps a user/model-supplied reference — mailbox id, role
// (inbox, trash, sent, …), or display name — to one mailbox. Resolution order
// is id → role → name (both case-insensitive); a name matching several
// mailboxes is an error naming the candidates. A miss against a cached list
// refreshes once before failing.
func (s *Service) resolveMailbox(ctx context.Context, sess *jmap.Session, accountID, ref string) (Mailbox, error) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return Mailbox{}, fmt.Errorf("empty mailbox reference")
	}
	list, err := s.cachedMailboxes(ctx, sess, accountID)
	if err != nil {
		return Mailbox{}, err
	}
	mb, err := matchMailbox(list, ref)
	if err == nil {
		return mb, nil
	}
	var miss *mailboxMissError
	if !errors.As(err, &miss) {
		return Mailbox{}, err // ambiguity — refreshing won't help
	}
	// Miss: the mailbox may be newer than the cache; refresh once.
	if list, err2 := s.fetchMailboxes(ctx, sess, accountID, false); err2 == nil {
		if mb, err3 := matchMailbox(list, ref); err3 == nil {
			return mb, nil
		}
	}
	return Mailbox{}, err
}

type mailboxMissError struct{ ref, available string }

func (e *mailboxMissError) Error() string {
	return fmt.Sprintf("no mailbox matches %q (available: %s)", e.ref, e.available)
}

func matchMailbox(list []Mailbox, ref string) (Mailbox, error) {
	for _, mb := range list {
		if mb.ID == ref {
			return mb, nil
		}
	}
	var roleMatches, pathMatches, nameMatches []Mailbox
	for _, mb := range list {
		if mb.Role != "" && strings.EqualFold(mb.Role, ref) {
			roleMatches = append(roleMatches, mb)
		}
		if strings.Contains(ref, "/") && strings.EqualFold(mb.Path, strings.Trim(ref, "/")) {
			pathMatches = append(pathMatches, mb)
		}
		if strings.EqualFold(mb.Name, ref) {
			nameMatches = append(nameMatches, mb)
		}
	}
	// A role is unique per account (RFC 8621 §2: "at most one mailbox of a
	// given role"), but tolerate quirks by treating >1 as ambiguous.
	for _, matches := range [][]Mailbox{roleMatches, pathMatches, nameMatches} {
		switch len(matches) {
		case 1:
			return matches[0], nil
		case 0:
			continue
		default:
			var ids []string
			for _, m := range matches {
				ids = append(ids, fmt.Sprintf("%s (id %s)", m.Path, m.ID))
			}
			return Mailbox{}, fmt.Errorf("mailbox reference %q is ambiguous: %s — use the path or mailbox id", ref, strings.Join(ids, ", "))
		}
	}
	var names []string
	for _, mb := range list {
		if mb.Role != "" {
			names = append(names, fmt.Sprintf("%s [%s]", mb.Path, mb.Role))
		} else {
			names = append(names, mb.Path)
		}
	}
	return Mailbox{}, &mailboxMissError{ref: ref, available: strings.Join(names, ", ")}
}

// resolveMailboxFresh resolves a mutation target against a freshly fetched
// list, never the cache: a rename + name-reuse inside the cache TTL must not
// misroute a move (searches tolerate that staleness; mutations don't).
func (s *Service) resolveMailboxFresh(ctx context.Context, sess *jmap.Session, accountID, ref string) (Mailbox, error) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return Mailbox{}, fmt.Errorf("empty mailbox reference")
	}
	list, err := s.fetchMailboxes(ctx, sess, accountID, false)
	if err != nil {
		return Mailbox{}, err
	}
	return matchMailbox(list, ref)
}

// resolveTrash finds the role=trash mailbox from a fresh list — role ONLY,
// no name fallback: on a server without one, a user folder merely named
// "Trash" must not become the delete-to-trash target or the destroy gate.
func (s *Service) resolveTrash(ctx context.Context, sess *jmap.Session, accountID string) (Mailbox, error) {
	list, err := s.fetchMailboxes(ctx, sess, accountID, false)
	if err != nil {
		return Mailbox{}, err
	}
	for _, mb := range list {
		if strings.EqualFold(mb.Role, "trash") {
			return mb, nil
		}
	}
	return Mailbox{}, fmt.Errorf("this server has no role=trash mailbox — email_trash and the destroy gate need one (a folder merely named Trash does not count)")
}

// mailboxRefsByID builds the id → {name, role} annotations used in email
// summaries, best-effort from the cached list.
func (s *Service) mailboxRefsByID(ctx context.Context, sess *jmap.Session, accountID string, ids map[string]bool) []MailboxRef {
	list, err := s.cachedMailboxes(ctx, sess, accountID)
	if err != nil {
		list = nil // annotate with ids only
	}
	byID := map[string]Mailbox{}
	for _, mb := range list {
		byID[mb.ID] = mb
	}
	// If any id is unknown the cache may predate a new mailbox — refresh once.
	refresh := false
	for id := range ids {
		if _, ok := byID[id]; !ok {
			refresh = true
			break
		}
	}
	if refresh {
		if fresh, err := s.fetchMailboxes(ctx, sess, accountID, false); err == nil {
			for _, mb := range fresh {
				byID[mb.ID] = mb
			}
		}
	}
	var refs []MailboxRef
	for id := range ids {
		ref := MailboxRef{ID: id}
		if mb, ok := byID[id]; ok {
			ref.Name, ref.Role = mb.Name, mb.Role
			if mb.Path != mb.Name {
				ref.Path = mb.Path
			}
		}
		refs = append(refs, ref)
	}
	sort.Slice(refs, func(i, j int) bool { return refs[i].ID < refs[j].ID })
	return refs
}
