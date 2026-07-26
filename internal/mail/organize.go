package mail

// Phase-2 organization: mark (keywords), move (mailboxIds), and trash — all
// via Email/set (RFC 8621 §4.6). Every operation supports dryRun, and bulk
// runs above bulkConfirmThreshold require an exact confirmation phrase that
// the refusal message spells out.

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"terva-ext-jmap-mail/internal/jmap"
)

const (
	// maxSetIDs bounds one mutating call (well under maxObjectsInSet).
	maxSetIDs = 200
	// bulkConfirmThreshold: above this many ids, a non-dry run requires the
	// generated confirm phrase.
	bulkConfirmThreshold = 20
)

// --- mark ---

// markActions maps the tool action onto the RFC 8621 §4.1.1 keyword patch.
var markActions = map[string]struct {
	keyword string
	set     bool
}{
	"read":   {"$seen", true},
	"unread": {"$seen", false},
	"flag":   {"$flagged", true},
	"unflag": {"$flagged", false},
}

// MarkParams are the email_mark inputs.
type MarkParams struct {
	Account string
	IDs     []string
	Action  string // read | unread | flag | unflag
	DryRun  bool
	Confirm string
	Verbose *bool // nil = enumerate only below the bulk threshold
}

// MarkChange identifies one affected message.
type MarkChange struct {
	ID      string `json:"id"`
	Subject string `json:"subject,omitempty"`
}

// MarkResult is the email_mark output. Changed lists messages whose state
// differed (and was/would be updated); AlreadySet lists no-ops. The lists are
// omitted on bulk runs (see enumerateChanges) — the counts always stand, and
// Failed/NotFound are never abridged.
type MarkResult struct {
	AccountID       string            `json:"accountId"`
	Action          string            `json:"action"`
	DryRun          bool              `json:"dryRun,omitempty"`
	ConfirmPhrase   string            `json:"confirmPhrase,omitempty"` // on bulk dry runs: the phrase the real run needs
	ChangedCount    int               `json:"changedCount"`
	AlreadySetCount int               `json:"alreadySetCount"`
	Changed         []MarkChange      `json:"changed,omitempty"`
	AlreadySet      []MarkChange      `json:"alreadySet,omitempty"`
	Failed          map[string]string `json:"failed,omitempty"`
	NotFound        []string          `json:"notFound,omitempty"`
}

// Mark sets or clears $seen/$flagged. One batched request: Email/get snapshots
// the current keywords (classifying changed vs already-set), then Email/set
// applies the patch — skipped entirely on dryRun.
func (s *Service) Mark(ctx context.Context, p MarkParams) (*MarkResult, error) {
	action, ok := markActions[p.Action]
	if !ok {
		return nil, fmt.Errorf("invalid action %q: use read, unread, flag, or unflag", p.Action)
	}
	if err := validateIDs(p.IDs); err != nil {
		return nil, err
	}
	accountID, sess, err := s.account(ctx, p.Account)
	if err != nil {
		return nil, err
	}
	phrase := fmt.Sprintf("mark %d emails %s in account %s", len(p.IDs), p.Action, accountID)
	if err := requireConfirm(len(p.IDs), p.DryRun, p.Confirm, phrase); err != nil {
		return nil, err
	}

	calls := []jmap.Invocation{{
		Name: "Email/get",
		Args: map[string]any{
			"accountId":  accountID,
			"ids":        p.IDs,
			"properties": []string{"id", "subject", "keywords"},
		},
		CallID: "g0",
	}}
	if !p.DryRun {
		update := map[string]any{}
		for _, id := range p.IDs {
			patch := map[string]any{}
			if action.set {
				patch["keywords/"+action.keyword] = true
			} else {
				patch["keywords/"+action.keyword] = nil
			}
			update[id] = patch
		}
		calls = append(calls, jmap.Invocation{
			Name:   "Email/set",
			Args:   map[string]any{"accountId": accountID, "update": update},
			CallID: "s1",
		})
	}

	resp, err := s.call(ctx, sess, calls)
	if err != nil {
		return nil, err
	}
	gres, err := resp.Result("g0")
	if err != nil {
		if !p.DryRun {
			// Methods in a batch execute independently (RFC 8620 §3.2): a
			// get-level error does not undo the set that followed it.
			return nil, fmt.Errorf("%w — note: the mutation in the same batch may still have applied; verify with email_search before retrying", err)
		}
		return nil, err
	}
	var got struct {
		List []struct {
			ID       string          `json:"id"`
			Subject  string          `json:"subject"`
			Keywords map[string]bool `json:"keywords"`
		} `json:"list"`
		NotFound []string `json:"notFound"`
	}
	if err := json.Unmarshal(gres.Args, &got); err != nil {
		return nil, fmt.Errorf("parse Email/get response: %v", err)
	}

	enumerate := enumerateChanges(p.Verbose, len(p.IDs))
	result := &MarkResult{AccountID: accountID, Action: p.Action, DryRun: p.DryRun, NotFound: got.NotFound}
	if p.DryRun && len(p.IDs) > bulkConfirmThreshold {
		result.ConfirmPhrase = phrase
	}
	var outcome *setOutcome
	if !p.DryRun {
		if outcome, err = parseSetResult(resp, "s1"); err != nil {
			return nil, err
		}
		var setNotFound []string
		result.Failed, setNotFound = outcome.failures()
		result.NotFound = mergeNotFound(result.NotFound, setNotFound)
	}
	for _, e := range got.List {
		if outcome != nil && !outcome.updatedOK(e.ID) {
			continue // reported in Failed
		}
		change := MarkChange{ID: e.ID, Subject: e.Subject}
		if e.Keywords[action.keyword] == action.set {
			result.AlreadySetCount++
			if enumerate {
				result.AlreadySet = append(result.AlreadySet, change)
			}
		} else {
			result.ChangedCount++
			if enumerate {
				result.Changed = append(result.Changed, change)
			}
		}
	}
	return result, nil
}

// --- move / trash ---

// MoveParams are the email_move inputs.
type MoveParams struct {
	Account         string
	IDs             []string
	ToMailbox       string // id, role, path, or display name
	KeepInMailboxes bool   // false (default): destination replaces all mailboxes
	DryRun          bool
	Confirm         string
	Verbose         *bool // nil = enumerate only below the bulk threshold
}

// MoveChange records one moved message with its origin mailboxes.
type MoveChange struct {
	ID      string       `json:"id"`
	Subject string       `json:"subject,omitempty"`
	From    []MailboxRef `json:"from,omitempty"`
}

// MoveResult is the email_move / email_trash output. Moved is omitted on bulk
// runs (see enumerateChanges); MovedCount and the MovedFrom breakdown always
// stand, and Failed/NotFound are never abridged.
type MoveResult struct {
	AccountID       string     `json:"accountId"`
	DryRun          bool       `json:"dryRun,omitempty"`
	ConfirmPhrase   string     `json:"confirmPhrase,omitempty"` // on bulk dry runs: the phrase the real run needs
	Destination     MailboxRef `json:"destination"`
	KeptInMailboxes bool       `json:"keptInMailboxes,omitempty"`
	MovedCount      int        `json:"movedCount"`
	// MovedFrom counts by source mailbox (display path). A message in several
	// mailboxes counts once per mailbox, so the total may exceed MovedCount.
	MovedFrom map[string]int    `json:"movedFrom,omitempty"`
	Moved     []MoveChange      `json:"moved,omitempty"`
	Failed    map[string]string `json:"failed,omitempty"`
	NotFound  []string          `json:"notFound,omitempty"`
}

// Move puts messages into a destination mailbox. By default the destination
// replaces every current mailbox (a true move); KeepInMailboxes adds it
// instead (label-style).
func (s *Service) Move(ctx context.Context, p MoveParams) (*MoveResult, error) {
	if strings.TrimSpace(p.ToMailbox) == "" {
		return nil, fmt.Errorf("toMailbox is required")
	}
	if err := validateIDs(p.IDs); err != nil {
		return nil, err
	}
	accountID, sess, err := s.account(ctx, p.Account)
	if err != nil {
		return nil, err
	}
	dest, err := s.resolveMailboxFresh(ctx, sess, accountID, p.ToMailbox)
	if err != nil {
		return nil, err
	}
	// The phrase binds the full intent — count, destination PATH (two folders
	// may share a display name), and account — so a phrase minted for one
	// target can never confirm a different one.
	phrase := fmt.Sprintf("move %d emails to %s in account %s", len(p.IDs), dest.Path, accountID)
	if err := requireConfirm(len(p.IDs), p.DryRun, p.Confirm, phrase); err != nil {
		return nil, err
	}
	res, err := s.moveInto(ctx, sess, accountID, p.IDs, dest, p.KeepInMailboxes, p.DryRun, enumerateChanges(p.Verbose, len(p.IDs)))
	if err == nil && p.DryRun && len(p.IDs) > bulkConfirmThreshold {
		res.ConfirmPhrase = phrase
	}
	return res, err
}

// TrashParams are the email_trash inputs.
type TrashParams struct {
	Account string
	IDs     []string
	DryRun  bool
	Confirm string
	Verbose *bool // nil = enumerate only below the bulk threshold
}

// Trash moves messages to the mailbox with role "trash", replacing all other
// mailboxes (the RFC 8621 delete-to-trash semantics). It never destroys.
func (s *Service) Trash(ctx context.Context, p TrashParams) (*MoveResult, error) {
	if err := validateIDs(p.IDs); err != nil {
		return nil, err
	}
	accountID, sess, err := s.account(ctx, p.Account)
	if err != nil {
		return nil, err
	}
	dest, err := s.resolveTrash(ctx, sess, accountID)
	if err != nil {
		return nil, err
	}
	phrase := fmt.Sprintf("trash %d emails in account %s", len(p.IDs), accountID)
	if err := requireConfirm(len(p.IDs), p.DryRun, p.Confirm, phrase); err != nil {
		return nil, err
	}
	res, err := s.moveInto(ctx, sess, accountID, p.IDs, dest, false, p.DryRun, enumerateChanges(p.Verbose, len(p.IDs)))
	if err == nil && p.DryRun && len(p.IDs) > bulkConfirmThreshold {
		res.ConfirmPhrase = phrase
	}
	return res, err
}

// moveInto is the shared engine: one batched request where Email/get snapshots
// each message's current mailboxes (the "from" report), then Email/set applies
// either a whole-mailboxIds replace or an additive patch — skipped on dryRun.
func (s *Service) moveInto(ctx context.Context, sess *jmap.Session, accountID string, ids []string, dest Mailbox, keep, dryRun, enumerate bool) (*MoveResult, error) {
	calls := []jmap.Invocation{{
		Name: "Email/get",
		Args: map[string]any{
			"accountId":  accountID,
			"ids":        ids,
			"properties": []string{"id", "subject", "mailboxIds"},
		},
		CallID: "g0",
	}}
	if !dryRun {
		update := map[string]any{}
		for _, id := range ids {
			if keep {
				update[id] = map[string]any{"mailboxIds/" + dest.ID: true}
			} else {
				update[id] = map[string]any{"mailboxIds": map[string]any{dest.ID: true}}
			}
		}
		calls = append(calls, jmap.Invocation{
			Name:   "Email/set",
			Args:   map[string]any{"accountId": accountID, "update": update},
			CallID: "s1",
		})
	}

	resp, err := s.call(ctx, sess, calls)
	if err != nil {
		return nil, err
	}
	gres, err := resp.Result("g0")
	if err != nil {
		if !dryRun {
			// Methods in a batch execute independently (RFC 8620 §3.2): a
			// get-level error does not undo the set that followed it.
			return nil, fmt.Errorf("%w — note: the mutation in the same batch may still have applied; verify with email_search before retrying", err)
		}
		return nil, err
	}
	var got struct {
		List []struct {
			ID         string          `json:"id"`
			Subject    string          `json:"subject"`
			MailboxIDs map[string]bool `json:"mailboxIds"`
		} `json:"list"`
		NotFound []string `json:"notFound"`
	}
	if err := json.Unmarshal(gres.Args, &got); err != nil {
		return nil, fmt.Errorf("parse Email/get response: %v", err)
	}

	result := &MoveResult{
		AccountID:       accountID,
		DryRun:          dryRun,
		Destination:     MailboxRef{ID: dest.ID, Name: dest.Name, Role: dest.Role},
		KeptInMailboxes: keep,
		NotFound:        got.NotFound,
	}
	var outcome *setOutcome
	if !dryRun {
		if outcome, err = parseSetResult(resp, "s1"); err != nil {
			return nil, err
		}
		var setNotFound []string
		result.Failed, setNotFound = outcome.failures()
		result.NotFound = mergeNotFound(result.NotFound, setNotFound)
	}
	for _, e := range got.List {
		if outcome != nil && !outcome.updatedOK(e.ID) {
			continue // reported in Failed
		}
		from := s.mailboxRefsByID(ctx, sess, accountID, e.MailboxIDs)
		result.MovedCount++
		for _, ref := range from {
			if result.MovedFrom == nil {
				result.MovedFrom = map[string]int{}
			}
			result.MovedFrom[mailboxKey(ref)]++
		}
		if enumerate {
			result.Moved = append(result.Moved, MoveChange{ID: e.ID, Subject: e.Subject, From: from})
		}
	}
	return result, nil
}

// --- shared plumbing ---

// mailboxKey labels a mailbox for the MovedFrom breakdown: display path when
// nested (two folders may share a display name), else name, else id.
func mailboxKey(r MailboxRef) string {
	switch {
	case r.Path != "":
		return r.Path
	case r.Name != "":
		return r.Name
	default:
		return r.ID
	}
}

func validateIDs(ids []string) error {
	if len(ids) == 0 {
		return fmt.Errorf("ids is required")
	}
	if len(ids) > maxSetIDs {
		return fmt.Errorf("too many ids (%d): operate on at most %d per call", len(ids), maxSetIDs)
	}
	return nil
}

// enumerateChanges decides whether a result carries its per-message lists.
// Unset (the default) enumerates small runs, where the list IS the answer,
// and reports counts above the bulk threshold, where two hundred subject
// lines are not what the caller is deciding on — and where the payload, paid
// twice because a bulk run needs a dry run first, is what pushes an agent
// into a context compaction mid-wave. verbose:true forces the lists back on
// at any size; verbose:false suppresses them at any size.
func enumerateChanges(verbose *bool, n int) bool {
	if verbose != nil {
		return *verbose
	}
	return n <= bulkConfirmThreshold
}

// requireConfirm gates bulk non-dry runs behind an exact, human-readable
// phrase. The refusal spells out the phrase so recovering is one re-run.
func requireConfirm(n int, dryRun bool, confirm, phrase string) error {
	if dryRun || n <= bulkConfirmThreshold {
		return nil
	}
	if strings.EqualFold(strings.TrimSpace(confirm), phrase) {
		return nil
	}
	return fmt.Errorf("bulk operation: %d messages is above the confirmation threshold (%d) — run with dryRun:true to preview, then re-run with confirm set exactly to %q", n, bulkConfirmThreshold, phrase)
}

// setOutcome is the parsed Email/set response (RFC 8620 §5.3 subset).
type setOutcome struct {
	Updated      map[string]json.RawMessage `json:"updated"`
	NotUpdated   map[string]setError        `json:"notUpdated"`
	Destroyed    []string                   `json:"destroyed"`
	NotDestroyed map[string]setError        `json:"notDestroyed"`
	OldState     string                     `json:"oldState"`
	NewState     string                     `json:"newState"`
}

type setError struct {
	Type        string `json:"type"`
	Description string `json:"description"`
}

func (e setError) String() string {
	if e.Description == "" {
		return e.Type
	}
	return e.Type + ": " + e.Description
}

func parseSetResult(resp *jmap.Response, callID string) (*setOutcome, error) {
	res, err := resp.Result(callID)
	if err != nil {
		return nil, err
	}
	var out setOutcome
	if err := json.Unmarshal(res.Args, &out); err != nil {
		return nil, fmt.Errorf("parse Email/set response: %v", err)
	}
	return &out, nil
}

// updatedOK reports whether the server accepted the update for id.
func (o *setOutcome) updatedOK(id string) bool {
	_, ok := o.Updated[id]
	return ok
}

// mergeNotFound appends set-level notFound ids that the get snapshot did
// not already report (a server typically reports a vanished id in both).
func mergeNotFound(got, fromSet []string) []string {
	seen := make(map[string]bool, len(got))
	for _, id := range got {
		seen[id] = true
	}
	for _, id := range fromSet {
		if !seen[id] {
			got = append(got, id)
			seen[id] = true
		}
	}
	return got
}

// failures flattens notUpdated into id → readable reason. notFound entries
// go to the second return instead: they belong in the result's NotFound list
// (a message destroyed between the batch's get and set would otherwise
// vanish from the report entirely).
func (o *setOutcome) failures() (map[string]string, []string) {
	if len(o.NotUpdated) == 0 {
		return nil, nil
	}
	out := map[string]string{}
	var notFound []string
	for id, serr := range o.NotUpdated {
		if serr.Type == "notFound" {
			notFound = append(notFound, id)
			continue
		}
		out[id] = serr.String()
	}
	if len(out) == 0 {
		out = nil
	}
	return out, notFound
}
