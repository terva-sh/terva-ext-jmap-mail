package mail

// Phase-2 organization: mark (keywords), move (mailboxIds), and trash — all
// via Email/set (RFC 8621 §4.6). Every operation supports dryRun, and bulk
// runs above bulkConfirmThreshold require an exact confirmation phrase that
// the refusal message spells out.
//
// A set of messages can be named three ways: ids outright, a selection handle
// from email_search, or a receipt from this tool's own dry run. The last is
// the strongest — a receipt is the previewed set, so an apply presenting one
// cannot act on different messages than the preview showed. See handles.go.

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

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

// markAction is the RFC 8621 §4.1.1 keyword patch behind a tool action.
type markAction struct {
	keyword string
	set     bool
}

// markActions maps the tool action onto its keyword patch.
var markActions = map[string]markAction{
	"read":   {"$seen", true},
	"unread": {"$seen", false},
	"flag":   {"$flagged", true},
	"unflag": {"$flagged", false},
}

// MarkParams are the email_mark inputs.
type MarkParams struct {
	Account string
	// IDs, Selection, and Receipt are three ways to name the same thing and
	// are mutually exclusive; see resolveTargets.
	Selection       string
	SelectionOffset int
	Receipt         string
	IDs             []string
	Action          string // read | unread | flag | unflag
	DryRun          bool
	Confirm         string
	Verbose         *bool // nil = enumerate only below the bulk threshold
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

	// ReceiptID is minted by a dry run: present it to the real run in place of
	// the ids AND the confirm phrase.
	ReceiptID string `json:"receiptId,omitempty"`
	// Selection reports which slice of a selection this call consumed, so a
	// caller working through a page larger than maxSetIDs knows whether to
	// come back and at what offset.
	Selection *SelectionUse `json:"selection,omitempty"`
	// Drifted names messages whose keyword state at apply time differed from
	// what the dry run recorded. Never abridged: drift is rare, and when it
	// happens the ids are the answer.
	Drifted   []string `json:"drifted,omitempty"`
	DriftNote string   `json:"driftNote,omitempty"`
	// Replayed marks a result returned from a receipt that had already been
	// applied — nothing was re-sent to the provider.
	Replayed  bool   `json:"replayed,omitempty"`
	AppliedAt string `json:"appliedAt,omitempty"`

	// snapshot is the per-id keyword fingerprint this run observed, carried
	// out of markInto for the receipt to record. Never serialized.
	snapshot map[string]string
}

// Mark sets or clears $seen/$flagged. One batched request: Email/get snapshots
// the current keywords (classifying changed vs already-set), then Email/set
// applies the patch — skipped entirely on dryRun.
func (s *Service) Mark(ctx context.Context, p MarkParams) (*MarkResult, error) {
	action, ok := markActions[p.Action]
	if !ok {
		return nil, fmt.Errorf("invalid action %q: use read, unread, flag, or unflag", p.Action)
	}
	accountID, sess, err := s.account(ctx, p.Account)
	if err != nil {
		return nil, err
	}
	ts, err := s.resolveTargets(accountID, targetRefs{
		IDs: p.IDs, Selection: p.Selection, SelectionOffset: p.SelectionOffset,
		Receipt: p.Receipt, DryRun: p.DryRun,
	}, receiptMark)
	if err != nil {
		return nil, err
	}
	if err := validateIDs(ts.ids); err != nil {
		return nil, err
	}
	if r := ts.receipt; r != nil && r.Action != p.Action {
		return nil, fmt.Errorf("receipt %q previewed action %q, not %q — re-run the dry run for the action you mean", r.ID, r.Action, p.Action)
	}
	phrase := markPhrase(len(ts.ids), p.Action, accountID, ts.ids)
	// A receipt IS the preview the phrase exists to force, and it names the
	// exact set — so it authorizes the run in the phrase's place.
	if ts.receipt == nil {
		if err := requireConfirm(len(ts.ids), p.DryRun, p.Confirm, phrase); err != nil {
			return nil, err
		}
	} else if replay, err := s.handles.claimReceipt(ts.receipt); err != nil {
		return nil, err
	} else if replay != nil {
		return replayMark(replay, ts.receipt)
	}

	result, err := s.markInto(ctx, sess, accountID, ts, action, p)
	if ts.receipt != nil {
		s.handles.releaseReceipt(ts.receipt, result, err)
	}
	if err != nil {
		return nil, err
	}
	if p.DryRun {
		if len(ts.ids) > bulkConfirmThreshold {
			result.ConfirmPhrase = phrase
		}
		result.ReceiptID = s.handles.putReceipt(&receipt{
			Kind: receiptMark, AccountID: accountID, IDs: ts.ids,
			Action: p.Action, Snapshot: result.snapshot,
			Selection: ts.selection,
		})
	}
	result.Selection = ts.selection
	if ts.receipt != nil && ts.receipt.Selection != nil {
		result.Selection = ts.receipt.Selection
	}
	return result, nil
}

// markInto is Mark's network half: one batched request where Email/get
// snapshots the current keywords (classifying changed vs already-set), then
// Email/set applies the patch — skipped entirely on dryRun.
func (s *Service) markInto(ctx context.Context, sess *jmap.Session, accountID string, ts *targetSet, action markAction, p MarkParams) (*MarkResult, error) {
	ids := ts.ids
	calls := []jmap.Invocation{{
		Name: "Email/get",
		Args: map[string]any{
			"accountId":  accountID,
			"ids":        ids,
			"properties": []string{"id", "subject", "keywords"},
		},
		CallID: "g0",
	}}
	if !p.DryRun {
		update := map[string]any{}
		for _, id := range ids {
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

	enumerate := enumerateChanges(p.Verbose, len(ids))
	result := &MarkResult{
		AccountID: accountID, Action: p.Action, DryRun: p.DryRun, NotFound: got.NotFound,
		snapshot: make(map[string]string, len(got.List)),
	}
	for _, e := range got.List {
		result.snapshot[e.ID] = keywordKey(e.Keywords, action.keyword)
	}
	if r := ts.receipt; r != nil {
		result.Drifted = driftedIDs(r.Snapshot, r.IDs, result.snapshot)
		if len(result.Drifted) > 0 {
			result.DriftNote = driftNote
		}
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
	Account string
	// IDs, Selection, and Receipt are three ways to name the same thing and
	// are mutually exclusive; see resolveTargets.
	Selection       string
	SelectionOffset int
	Receipt         string
	IDs             []string
	// ToMailbox is an id, role, path, or display name. Required, except with a
	// Receipt — which already carries the destination its dry run resolved.
	ToMailbox       string
	KeepInMailboxes bool // false (default): destination replaces all mailboxes
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

	// ReceiptID is minted by a dry run: present it to the real run in place of
	// the ids AND the confirm phrase.
	ReceiptID string `json:"receiptId,omitempty"`
	// Selection reports which slice of a selection this call consumed, so a
	// caller working through a page larger than maxSetIDs knows whether to
	// come back and at what offset.
	Selection *SelectionUse `json:"selection,omitempty"`
	// Drifted names messages that were somewhere other than where the dry run
	// saw them. Never abridged: drift is rare, and when it happens the ids are
	// the answer.
	Drifted   []string `json:"drifted,omitempty"`
	DriftNote string   `json:"driftNote,omitempty"`
	// Replayed marks a result returned from a receipt that had already been
	// applied — nothing was re-sent to the provider.
	Replayed  bool   `json:"replayed,omitempty"`
	AppliedAt string `json:"appliedAt,omitempty"`

	// snapshot is the per-id placement fingerprint this run observed, carried
	// out of moveInto for the receipt to record. Never serialized.
	snapshot map[string]string
}

// Move puts messages into a destination mailbox. By default the destination
// replaces every current mailbox (a true move); KeepInMailboxes adds it
// instead (label-style).
func (s *Service) Move(ctx context.Context, p MoveParams) (*MoveResult, error) {
	if strings.TrimSpace(p.ToMailbox) == "" && p.Receipt == "" {
		return nil, fmt.Errorf("toMailbox is required")
	}
	accountID, sess, err := s.account(ctx, p.Account)
	if err != nil {
		return nil, err
	}
	ts, err := s.resolveTargets(accountID, targetRefs{
		IDs: p.IDs, Selection: p.Selection, SelectionOffset: p.SelectionOffset,
		Receipt: p.Receipt, DryRun: p.DryRun,
	}, receiptMove)
	if err != nil {
		return nil, err
	}
	if err := validateIDs(ts.ids); err != nil {
		return nil, err
	}

	// A receipt carries the destination its dry run resolved, so the apply
	// re-resolves nothing: a rename between preview and apply cannot re-target
	// the batch. A toMailbox passed alongside must agree — silently preferring
	// one over the other is exactly the confusion the receipt exists to end.
	var dest Mailbox
	if r := ts.receipt; r != nil {
		dest = Mailbox{ID: r.DestID, Name: r.DestName, Path: r.DestPath}
		if err := checkReceiptDestination(r, p.ToMailbox); err != nil {
			return nil, err
		}
		if r.Keep != p.KeepInMailboxes {
			return nil, fmt.Errorf("receipt %q previewed keepInMailboxes=%t, not %t — re-run the dry run for the behaviour you mean", r.ID, r.Keep, p.KeepInMailboxes)
		}
	} else if dest, err = s.resolveMailboxFresh(ctx, sess, accountID, p.ToMailbox); err != nil {
		return nil, err
	}

	// The phrase binds the full intent — count, the EXACT id set (a digest, so
	// a phrase minted for one batch cannot confirm a different batch the same
	// size), destination PATH (two folders may share a display name), and
	// account.
	phrase := movePhrase(len(ts.ids), dest.Path, accountID, ts.ids)
	// A receipt IS the preview the phrase exists to force, and it names the
	// exact set — so it authorizes the run in the phrase's place.
	if ts.receipt == nil {
		if err := requireConfirm(len(ts.ids), p.DryRun, p.Confirm, phrase); err != nil {
			return nil, err
		}
	} else if replay, err := s.handles.claimReceipt(ts.receipt); err != nil {
		return nil, err
	} else if replay != nil {
		return replayMove(replay, ts.receipt)
	}

	res, err := s.moveInto(ctx, sess, accountID, ts, dest, p.KeepInMailboxes, p.DryRun, enumerateChanges(p.Verbose, len(ts.ids)))
	if ts.receipt != nil {
		s.handles.releaseReceipt(ts.receipt, res, err)
	}
	if err != nil {
		return nil, err
	}
	if p.DryRun {
		if len(ts.ids) > bulkConfirmThreshold {
			res.ConfirmPhrase = phrase
		}
		res.ReceiptID = s.handles.putReceipt(&receipt{
			Kind: receiptMove, AccountID: accountID, IDs: ts.ids,
			DestID: dest.ID, DestName: dest.Name, DestPath: dest.Path,
			Keep: p.KeepInMailboxes, Snapshot: res.snapshot,
			Selection: ts.selection,
		})
	}
	res.Selection = ts.selection
	if ts.receipt != nil && ts.receipt.Selection != nil {
		res.Selection = ts.receipt.Selection
	}
	return res, nil
}

// TrashParams are the email_trash inputs.
type TrashParams struct {
	Account string
	// IDs, Selection, and Receipt are three ways to name the same thing and
	// are mutually exclusive; see resolveTargets.
	Selection       string
	SelectionOffset int
	Receipt         string
	IDs             []string
	DryRun          bool
	Confirm         string
	Verbose         *bool // nil = enumerate only below the bulk threshold
}

// Trash moves messages to the mailbox with role "trash", replacing all other
// mailboxes (the RFC 8621 delete-to-trash semantics). It never destroys.
func (s *Service) Trash(ctx context.Context, p TrashParams) (*MoveResult, error) {
	accountID, sess, err := s.account(ctx, p.Account)
	if err != nil {
		return nil, err
	}
	ts, err := s.resolveTargets(accountID, targetRefs{
		IDs: p.IDs, Selection: p.Selection, SelectionOffset: p.SelectionOffset,
		Receipt: p.Receipt, DryRun: p.DryRun,
	}, receiptTrash)
	if err != nil {
		return nil, err
	}
	if err := validateIDs(ts.ids); err != nil {
		return nil, err
	}
	// The Trash mailbox is resolved by role and cannot be re-pointed by a
	// rename, so unlike Move there is nothing a receipt needs to pin here —
	// re-resolving is both safe and one fewer thing to keep in step.
	dest, err := s.resolveTrash(ctx, sess, accountID)
	if err != nil {
		return nil, err
	}
	phrase := trashPhrase(len(ts.ids), accountID, ts.ids)
	if ts.receipt == nil {
		if err := requireConfirm(len(ts.ids), p.DryRun, p.Confirm, phrase); err != nil {
			return nil, err
		}
	} else if replay, err := s.handles.claimReceipt(ts.receipt); err != nil {
		return nil, err
	} else if replay != nil {
		return replayMove(replay, ts.receipt)
	}

	res, err := s.moveInto(ctx, sess, accountID, ts, dest, false, p.DryRun, enumerateChanges(p.Verbose, len(ts.ids)))
	if ts.receipt != nil {
		s.handles.releaseReceipt(ts.receipt, res, err)
	}
	if err != nil {
		return nil, err
	}
	if p.DryRun {
		if len(ts.ids) > bulkConfirmThreshold {
			res.ConfirmPhrase = phrase
		}
		res.ReceiptID = s.handles.putReceipt(&receipt{
			Kind: receiptTrash, AccountID: accountID, IDs: ts.ids,
			DestID: dest.ID, DestName: dest.Name, DestPath: dest.Path,
			Snapshot: res.snapshot, Selection: ts.selection,
		})
	}
	res.Selection = ts.selection
	if ts.receipt != nil && ts.receipt.Selection != nil {
		res.Selection = ts.receipt.Selection
	}
	return res, nil
}

// moveInto is the shared engine: one batched request where Email/get snapshots
// each message's current mailboxes (the "from" report), then Email/set applies
// either a whole-mailboxIds replace or an additive patch — skipped on dryRun.
func (s *Service) moveInto(ctx context.Context, sess *jmap.Session, accountID string, ts *targetSet, dest Mailbox, keep, dryRun, enumerate bool) (*MoveResult, error) {
	ids := ts.ids
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
		snapshot:        make(map[string]string, len(got.List)),
	}
	for _, e := range got.List {
		result.snapshot[e.ID] = placementKey(e.MailboxIDs)
	}
	// The Email/get above runs before the Email/set in the same batch, so this
	// snapshot is pre-mutation on both the preview and the apply — like for
	// like. A difference means something else moved the message in between.
	if r := ts.receipt; r != nil {
		result.Drifted = driftedIDs(r.Snapshot, r.IDs, result.snapshot)
		if len(result.Drifted) > 0 {
			result.DriftNote = driftNote
		}
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

// --- naming a set: ids, selection, or receipt ---

// SelectionUse reports which slice of a selection a call consumed. Remaining
// is what a follow-up call would find past this slice — the loop condition for
// a projected page larger than one mutating call can take.
type SelectionUse struct {
	ID        string `json:"id"`
	Offset    int    `json:"offset"`
	Count     int    `json:"count"`
	Remaining int    `json:"remaining"`
}

// targetRefs is the "which messages" half of every organize call, before
// resolution.
type targetRefs struct {
	IDs             []string
	Selection       string
	SelectionOffset int
	Receipt         string
	DryRun          bool
}

// targetSet is the resolved answer: the ids to operate on, plus whichever
// handle named them.
type targetSet struct {
	ids       []string
	selection *SelectionUse
	receipt   *receipt
}

// resolveTargets turns the three ways of naming a set into one id list. They
// are mutually exclusive because each is a complete answer on its own, and
// accepting two would mean guessing which the caller meant.
//
// A selection larger than maxSetIDs is not an error: it is sliced. That is how
// the 500-id projected search page and the 200-id mutating cap reconcile —
// one search feeds several batches, and because the ids are pinned at search
// time, the later slices are unaffected by what the earlier ones moved. (Which
// is the whole reason the re-query-from-position-0 discipline exists; within a
// selection it stops being necessary.)
func (s *Service) resolveTargets(accountID string, r targetRefs, kind receiptKind) (*targetSet, error) {
	named := 0
	for _, set := range []bool{len(r.IDs) > 0, r.Selection != "", r.Receipt != ""} {
		if set {
			named++
		}
	}
	switch {
	case named == 0:
		return nil, fmt.Errorf("name the messages: ids, selection (from an email_search result's selectionId), or receipt (from this tool's own dry run)")
	case named > 1:
		return nil, fmt.Errorf("pass exactly one of ids, selection, or receipt — each names the whole set on its own")
	}

	switch {
	case r.Receipt != "":
		if r.DryRun {
			return nil, fmt.Errorf("a receipt names an apply, not a preview — it was minted BY a dry run; pass selection or ids to preview again")
		}
		rcpt, err := s.handles.getReceipt(r.Receipt, kind)
		if err != nil {
			return nil, err
		}
		if rcpt.AccountID != accountID {
			return nil, fmt.Errorf("receipt %q was minted for account %s, not %s", r.Receipt, rcpt.AccountID, accountID)
		}
		return &targetSet{ids: rcpt.IDs, receipt: rcpt}, nil

	case r.Selection != "":
		sel, err := s.handles.getSelection(r.Selection)
		if err != nil {
			return nil, err
		}
		if sel.AccountID != accountID {
			return nil, fmt.Errorf("selection %q was minted for account %s, not %s — re-run email_search against the account you mean", r.Selection, sel.AccountID, accountID)
		}
		offset := r.SelectionOffset
		if offset < 0 {
			offset = 0
		}
		if offset >= len(sel.IDs) {
			return nil, fmt.Errorf("selectionOffset %d is at or past the end of selection %q (%d ids) — the selection is fully consumed", offset, sel.ID, len(sel.IDs))
		}
		ids := sel.IDs[offset:]
		if len(ids) > maxSetIDs {
			ids = ids[:maxSetIDs]
		}
		return &targetSet{
			ids: ids,
			selection: &SelectionUse{
				ID:        sel.ID,
				Offset:    offset,
				Count:     len(ids),
				Remaining: len(sel.IDs) - offset - len(ids),
			},
		}, nil
	}
	return &targetSet{ids: r.IDs}, nil
}

// checkReceiptDestination refuses a toMailbox that names something other than
// what the receipt's dry run resolved. Matching is against the values the
// receipt already holds, so this costs no network call.
func checkReceiptDestination(r *receipt, toMailbox string) error {
	ref := strings.TrimSpace(toMailbox)
	if ref == "" {
		return nil
	}
	for _, known := range []string{r.DestID, r.DestPath, r.DestName} {
		if known != "" && strings.EqualFold(known, strings.Trim(ref, "/")) {
			return nil
		}
	}
	return fmt.Errorf("receipt %q previewed a move to %s, but toMailbox says %q — drop toMailbox to use the receipt's destination, or re-run the dry run against the mailbox you mean", r.ID, r.DestPath, toMailbox)
}

// driftNote explains a non-empty Drifted list. The messages are still acted
// on: the caller named these exact ids, and refusing a whole batch because one
// message moved would fail far more often than it would help.
const driftNote = "these messages were somewhere other than where the dry run saw them — something moved them in between. They were still included in this run; check them if the placement mattered."

// replayMove returns an already-applied move/trash receipt's original result
// rather than repeating the work. The stored value is copied before the replay
// markers go on, so the receipt keeps returning the same thing however many
// times it is presented.
func replayMove(stored any, r *receipt) (*MoveResult, error) {
	res, ok := stored.(*MoveResult)
	if !ok || res == nil {
		return nil, replayLost(r)
	}
	out := *res
	out.Replayed = true
	out.AppliedAt = r.AppliedAt.UTC().Format(time.RFC3339)
	return &out, nil
}

// replayMark is replayMove for email_mark.
func replayMark(stored any, r *receipt) (*MarkResult, error) {
	res, ok := stored.(*MarkResult)
	if !ok || res == nil {
		return nil, replayLost(r)
	}
	out := *res
	out.Replayed = true
	out.AppliedAt = r.AppliedAt.UTC().Format(time.RFC3339)
	return &out, nil
}

// replayLost covers the case a correct store cannot produce: the receipt is
// marked applied but its result is missing or of the wrong type. Saying so is
// the only safe answer — reapplying would act twice on work that already
// landed, and returning nothing would read as success.
func replayLost(r *receipt) error {
	return fmt.Errorf("receipt %q was applied at %s but its recorded result is unusable — the work is done; verify with email_search rather than re-running it",
		r.ID, r.AppliedAt.UTC().Format(time.RFC3339))
}

// --- confirm phrases ---

// The phrases bind the full intent AND the subject: count, operation-specific
// target, account, and a digest of the EXACT id set. Without the digest a
// phrase minted by one preview would confirm any other batch of the same size
// to the same destination — the preview and the apply would agree on
// everything except which messages. (email_destroy has bound its ids this way
// since it shipped; this is the same guarantee for the recoverable
// operations.)

func markPhrase(n int, action, accountID string, ids []string) string {
	return fmt.Sprintf("mark %d emails %s in account %s [batch %s]", n, action, accountID, idBatchDigest(ids))
}

func movePhrase(n int, destPath, accountID string, ids []string) string {
	return fmt.Sprintf("move %d emails to %s in account %s [batch %s]", n, destPath, accountID, idBatchDigest(ids))
}

func trashPhrase(n int, accountID string, ids []string) string {
	return fmt.Sprintf("trash %d emails in account %s [batch %s]", n, accountID, idBatchDigest(ids))
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
