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
	"sort"
	"strings"
	"time"

	"terva-ext-jmap-mail/internal/jmap"
)

const (
	// maxSetIDs bounds one mutating call named by literal ids. Two hundred
	// opaque strings is already a large tool call, and the cap is a bound on
	// ARGUMENT PAYLOAD — which is the whole reason it does not apply below.
	maxSetIDs = 200
	// maxHandleSetIDs bounds one mutating call named by a handle. A handle is
	// one short field: it carries no argument payload, and the set it names was
	// pinned server-side when the search ran, so the literal-id cap is
	// inherited from a shape the handle exists to replace.
	//
	// A fleet session archived 13,797 messages as 69 applies of 199.96 messages
	// plus 70 dry runs — 139 calls, 500 of 531 tool-bearing turns carrying
	// exactly one call, 4h45m. The protocol was executed perfectly; the turn
	// count was set by an argument-size limit the chosen calling convention
	// never hits. A wave costs turns × context, so that limit was most of the
	// bill (TW-049).
	//
	// Not unbounded: a call that moves everything must still be interruptible
	// and must report how far it got, so this is a real ceiling and the work is
	// chunked underneath it. Ten thousand would fit the timeout on a good day
	// and not on a bad one; two thousand removes ~90% of the turns and keeps
	// every failure local.
	maxHandleSetIDs = 2000
	// bulkConfirmThreshold: above this many ids, a non-dry run requires the
	// generated confirm phrase.
	bulkConfirmThreshold = 20
	// mutationChunkFallback is the per-request slice when the session
	// advertises no object limits.
	mutationChunkFallback = 200
)

// mutationChunk is how many messages one request may carry. A chunk is an
// Email/get and an Email/set over the same ids in one batch, so it must fit
// under BOTH server limits — exceeding either is a request-level error that
// would fail the whole wave rather than one slice of it.
func mutationChunk(sess *jmap.Session) int {
	core, ok := sess.CoreLimits()
	if !ok {
		return mutationChunkFallback
	}
	chunk := mutationChunkFallback
	if core.MaxObjectsInGet > 0 {
		chunk = int(core.MaxObjectsInGet)
	}
	if core.MaxObjectsInSet > 0 && int(core.MaxObjectsInSet) < chunk {
		chunk = int(core.MaxObjectsInSet)
	}
	if chunk < 1 {
		chunk = 1
	}
	return chunk
}

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
	// Handle and IDs are two ways to name the same thing and are mutually
	// exclusive. Handle is a selectionId (sel_…) or a receiptId (rcp_…); the
	// prefix says which, so the call states its own mode. See resolveTargets.
	Handle          string
	SelectionOffset int
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
	AccountID       string       `json:"accountId"`
	Action          string       `json:"action"`
	DryRun          bool         `json:"dryRun,omitempty"`
	ConfirmPhrase   string       `json:"confirmPhrase,omitempty"` // on bulk dry runs: the phrase the real run needs
	ChangedCount    int          `json:"changedCount"`
	AlreadySetCount int          `json:"alreadySetCount"`
	Changed         []MarkChange `json:"changed,omitempty"`
	AlreadySet      []MarkChange `json:"alreadySet,omitempty"`
	// Failed and NotFound list ids outright, and are populated only while the
	// run is small enough to enumerate. Above that they would be the payload
	// problem the handles exist to solve, arriving through the one field
	// nobody had bounded — see failures.go. Failures always carries the same
	// information grouped by cause, with a handle per group.
	Failed   map[string]string `json:"failed,omitempty"`
	NotFound []string          `json:"notFound,omitempty"`
	failureReport

	// ReceiptID is minted by a dry run: present it to the real run in place of
	// the ids AND the confirm phrase.
	ReceiptID string `json:"receiptId,omitempty"`
	// Selection reports which slice of a selection this call consumed, so a
	// caller working through a page larger than maxSetIDs knows whether to
	// come back and at what offset.
	Selection *SelectionUse `json:"selection,omitempty"`
	// Drifted names messages whose keyword state at apply time differed from
	// what the dry run recorded, with both states. Never abridged: drift is
	// rare, so the one moment it costs bytes is the moment they are worth
	// paying — withholding the states would send the caller back for the
	// comparison that detected the drift in the first place.
	Drifted   []DriftedKeyword `json:"drifted,omitempty"`
	DriftNote string           `json:"driftNote,omitempty"`
	// Replayed marks a result returned from a receipt that had already been
	// applied — nothing was re-sent to the provider.
	Replayed  bool   `json:"replayed,omitempty"`
	AppliedAt string `json:"appliedAt,omitempty"`
	// partialRun is populated only when the run stopped part-way through a
	// handle-named set too large for one request.
	partialRun

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
		IDs: p.IDs, Handle: p.Handle, SelectionOffset: p.SelectionOffset,
		DryRun: p.DryRun,
	}, receiptMark)
	if err != nil {
		return nil, err
	}
	if err := validateIDs(ts.ids, ts.byHandle); err != nil {
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
	enumerate := enumerateChanges(p.Verbose, len(ts.ids))
	result := &MarkResult{
		AccountID: accountID, Action: p.Action, DryRun: p.DryRun,
		snapshot: make(map[string]string, len(ts.ids)),
	}
	fails := newFailureAccum()
	chunk := mutationChunk(sess)
	for start := 0; start < len(ts.ids); start += chunk {
		end := start + chunk
		if end > len(ts.ids) {
			end = len(ts.ids)
		}
		err := s.markChunk(ctx, sess, accountID, ts.ids[start:end], action, p.DryRun, enumerate, result, fails)
		if err == nil {
			continue
		}
		if start == 0 {
			return nil, err
		}
		s.abortPartial(&result.partialRun, accountID, ts.ids, start, err)
		break
	}
	if r := ts.receipt; r != nil {
		for _, d := range driftedStates(r.Snapshot, r.IDs, result.snapshot) {
			result.Drifted = append(result.Drifted, DriftedKeyword{
				ID: d.ID, Keyword: action.keyword, Was: d.Was, Now: d.Now,
			})
		}
		if len(result.Drifted) > 0 {
			result.DriftNote = driftNote
		}
	}
	s.attachFailures(&result.failureReport, accountID, fails, enumerate, &result.Failed, &result.NotFound)
	return result, nil
}

// markChunk is one request's worth of markInto, accumulating into the caller's
// result the same way moveChunk does.
func (s *Service) markChunk(ctx context.Context, sess *jmap.Session, accountID string, ids []string, action markAction, dryRun, enumerate bool, result *MarkResult, fails *failureAccum) error {
	calls := []jmap.Invocation{{
		Name: "Email/get",
		Args: map[string]any{
			"accountId":  accountID,
			"ids":        ids,
			"properties": []string{"id", "subject", "keywords"},
		},
		CallID: "g0",
	}}
	if !dryRun {
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
		return err
	}
	gres, err := resp.Result("g0")
	if err != nil {
		if !dryRun {
			// Methods in a batch execute independently (RFC 8620 §3.2): a
			// get-level error does not undo the set that followed it.
			return fmt.Errorf("%w — note: the mutation in the same batch may still have applied; verify with email_search before retrying", err)
		}
		return err
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
		return fmt.Errorf("parse Email/get response: %v", err)
	}
	for _, id := range got.NotFound {
		fails.add("notFound", id, "the message was not found when the run snapshotted it")
	}
	result.NotFound = mergeNotFound(result.NotFound, got.NotFound)
	for _, e := range got.List {
		result.snapshot[e.ID] = keywordKey(e.Keywords, action.keyword)
	}

	var outcome *setOutcome
	if !dryRun {
		if outcome, err = parseSetResult(resp, "s1"); err != nil {
			return err
		}
		outcome.collectFailures(fails)
		failed, setNotFound := outcome.failures()
		for id, reason := range failed {
			if result.Failed == nil {
				result.Failed = map[string]string{}
			}
			result.Failed[id] = reason
		}
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
	return nil
}

// --- move / trash ---

// MoveParams are the email_move inputs.
type MoveParams struct {
	Account string
	// Handle and IDs are two ways to name the same thing and are mutually
	// exclusive. Handle is a selectionId (sel_…) or a receiptId (rcp_…); the
	// prefix says which, so the call states its own mode. See resolveTargets.
	Handle          string
	SelectionOffset int
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

// DriftedPlacement is one message whose mailboxes changed between an
// email_move / email_trash dry run and its apply, with both placements. The
// drift is detected by comparing them, so reporting the bare id would send the
// caller back to email_get for what this result already knows — and the
// remedy depends on where the message actually went.
type DriftedPlacement struct {
	ID  string       `json:"id"`
	Was []MailboxRef `json:"was"` // where the dry run saw it
	Now []MailboxRef `json:"now"` // where this run found it, before acting
}

// DriftedKeyword is the email_mark equivalent: the keyword the action turns on
// or off, and its state at preview and at apply.
type DriftedKeyword struct {
	ID      string `json:"id"`
	Keyword string `json:"keyword"`
	Was     string `json:"was"` // set | unset
	Now     string `json:"now"`
}

// MailboxCount is a mailbox with a message count — the source-side
// counterpart of a MoveResult's Destination, deliberately in the same shape.
// A count keyed by display name would be a weaker identifier than the one the
// destination gets: JMAP names are user-editable and not unique across
// parents, so a result carrying only names cannot assert where mail came from
// without a separate email_list_mailboxes to resolve them, and it degrades
// silently if one is renamed mid-wave.
type MailboxCount struct {
	MailboxRef
	Count int `json:"count"`
}

// MoveResult is the email_move / email_trash output. Moved is omitted on bulk
// runs (see enumerateChanges); MovedCount and the Sources breakdown always
// stand, and Failed/NotFound are never abridged.
type MoveResult struct {
	AccountID       string     `json:"accountId"`
	DryRun          bool       `json:"dryRun,omitempty"`
	ConfirmPhrase   string     `json:"confirmPhrase,omitempty"` // on bulk dry runs: the phrase the real run needs
	Destination     MailboxRef `json:"destination"`
	KeptInMailboxes bool       `json:"keptInMailboxes,omitempty"`
	MovedCount      int        `json:"movedCount"`
	// Sources counts by origin mailbox, identified the same way Destination is.
	// A message in several mailboxes counts once per mailbox, so the total may
	// exceed MovedCount. Ordered by count descending, so the mailbox a batch
	// mostly came from reads first; ties break on path then id, so the same
	// move always renders the same bytes. This is the whole of the source
	// information on a bulk run, where Moved (and the per-message From refs
	// inside it) is omitted.
	Sources []MailboxCount `json:"sources,omitempty"`
	Moved   []MoveChange   `json:"moved,omitempty"`
	// Failed and NotFound list ids outright, and are populated only while the
	// run is small enough to enumerate; Failures always carries the same
	// information grouped by cause, with a handle per group. See failures.go.
	Failed   map[string]string `json:"failed,omitempty"`
	NotFound []string          `json:"notFound,omitempty"`
	failureReport

	// ReceiptID is minted by a dry run: present it to the real run in place of
	// the ids AND the confirm phrase.
	ReceiptID string `json:"receiptId,omitempty"`
	// Selection reports which slice of a selection this call consumed, so a
	// caller working through a page larger than maxSetIDs knows whether to
	// come back and at what offset.
	Selection *SelectionUse `json:"selection,omitempty"`
	// Drifted names messages that were somewhere other than where the dry run
	// saw them, with both placements. Never abridged: drift is rare, so the one
	// moment it costs bytes is the moment they are worth paying — withholding
	// the placements would send the caller back for the comparison that
	// detected the drift in the first place.
	Drifted   []DriftedPlacement `json:"drifted,omitempty"`
	DriftNote string             `json:"driftNote,omitempty"`
	// Replayed marks a result returned from a receipt that had already been
	// applied — nothing was re-sent to the provider.
	Replayed  bool   `json:"replayed,omitempty"`
	AppliedAt string `json:"appliedAt,omitempty"`
	// partialRun is populated only when the run stopped part-way through a
	// handle-named set too large for one request.
	partialRun

	// snapshot is the per-id placement fingerprint this run observed, carried
	// out of moveInto for the receipt to record. Never serialized.
	snapshot map[string]string
}

// Move puts messages into a destination mailbox. By default the destination
// replaces every current mailbox (a true move); KeepInMailboxes adds it
// instead (label-style).
func (s *Service) Move(ctx context.Context, p MoveParams) (*MoveResult, error) {
	// A receipt already carries the destination its dry run resolved, so it is
	// the one shape that needs no toMailbox.
	if strings.TrimSpace(p.ToMailbox) == "" && !strings.HasPrefix(strings.TrimSpace(p.Handle), receiptPrefix) {
		return nil, fmt.Errorf("toMailbox is required (except with a receipt handle, which already carries the destination its dry run resolved)")
	}
	accountID, sess, err := s.account(ctx, p.Account)
	if err != nil {
		return nil, err
	}
	ts, err := s.resolveTargets(accountID, targetRefs{
		IDs: p.IDs, Handle: p.Handle, SelectionOffset: p.SelectionOffset,
		DryRun: p.DryRun,
	}, receiptMove)
	if err != nil {
		return nil, err
	}
	if err := validateIDs(ts.ids, ts.byHandle); err != nil {
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
	// Handle and IDs are two ways to name the same thing and are mutually
	// exclusive. Handle is a selectionId (sel_…) or a receiptId (rcp_…); the
	// prefix says which, so the call states its own mode. See resolveTargets.
	Handle          string
	SelectionOffset int
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
		IDs: p.IDs, Handle: p.Handle, SelectionOffset: p.SelectionOffset,
		DryRun: p.DryRun,
	}, receiptTrash)
	if err != nil {
		return nil, err
	}
	if err := validateIDs(ts.ids, ts.byHandle); err != nil {
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
	destRef := MailboxRef{ID: dest.ID, Name: dest.Name, Role: dest.Role}
	// Path only when it says something the name does not — the same rule
	// mailboxRefsByID applies, and the reason the confirm phrase quotes the
	// path rather than the name.
	if dest.Path != dest.Name {
		destRef.Path = dest.Path
	}
	result := &MoveResult{
		AccountID:       accountID,
		DryRun:          dryRun,
		Destination:     destRef,
		KeptInMailboxes: keep,
		snapshot:        make(map[string]string, len(ts.ids)),
	}
	sources := map[string]*MailboxCount{}
	fails := newFailureAccum()

	// One chunk per request, sized to the server's own object limits. Packing
	// several chunks into one request would be fewer round trips and a worse
	// failure story: when a request fails there would be no telling which of
	// its chunks landed, and on a run this size "somewhere in the middle" is
	// not an answer a caller can act on.
	chunk := mutationChunk(sess)
	for start := 0; start < len(ts.ids); start += chunk {
		end := start + chunk
		if end > len(ts.ids) {
			end = len(ts.ids)
		}
		err := s.moveChunk(ctx, sess, accountID, ts.ids[start:end], dest, keep, dryRun, enumerate, result, sources, fails)
		if err == nil {
			continue
		}
		if start == 0 {
			// Nothing landed, so there is no partial outcome to describe and
			// the error is the whole story.
			return nil, err
		}
		s.abortPartial(&result.partialRun, accountID, ts.ids, start, err)
		break
	}

	// Drift is compared once, over everything actually scanned. Each chunk's
	// Email/get runs before its own Email/set, so every fingerprint here is
	// pre-mutation on both the preview and the apply — like for like. A
	// difference means something else moved the message in between.
	if r := ts.receipt; r != nil {
		if drift := driftedStates(r.Snapshot, r.IDs, result.snapshot); len(drift) > 0 {
			result.Drifted = s.annotateDrift(ctx, sess, accountID, drift)
			result.DriftNote = driftNote
		}
	}
	result.Sources = sortedSources(sources)
	s.attachFailures(&result.failureReport, accountID, fails, enumerate, &result.Failed, &result.NotFound)
	return result, nil
}

// moveChunk is one request's worth of moveInto: Email/get snapshots the
// current mailboxes, then Email/set applies the patch in the same batch. It
// accumulates into the caller's result rather than returning its own, so a run
// that stops halfway still holds everything the completed chunks reported.
func (s *Service) moveChunk(ctx context.Context, sess *jmap.Session, accountID string, ids []string, dest Mailbox, keep, dryRun, enumerate bool, result *MoveResult, sources map[string]*MailboxCount, fails *failureAccum) error {
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
		return err
	}
	gres, err := resp.Result("g0")
	if err != nil {
		if !dryRun {
			// Methods in a batch execute independently (RFC 8620 §3.2): a
			// get-level error does not undo the set that followed it.
			return fmt.Errorf("%w — note: the mutation in the same batch may still have applied; verify with email_search before retrying", err)
		}
		return err
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
		return fmt.Errorf("parse Email/get response: %v", err)
	}
	for _, id := range got.NotFound {
		fails.add("notFound", id, "the message was not found when the run snapshotted it")
	}
	result.NotFound = mergeNotFound(result.NotFound, got.NotFound)
	for _, e := range got.List {
		result.snapshot[e.ID] = placementKey(e.MailboxIDs)
	}

	var outcome *setOutcome
	if !dryRun {
		if outcome, err = parseSetResult(resp, "s1"); err != nil {
			return err
		}
		outcome.collectFailures(fails)
		failed, setNotFound := outcome.failures()
		for id, reason := range failed {
			if result.Failed == nil {
				result.Failed = map[string]string{}
			}
			result.Failed[id] = reason
		}
		result.NotFound = mergeNotFound(result.NotFound, setNotFound)
	}
	for _, e := range got.List {
		if outcome != nil && !outcome.updatedOK(e.ID) {
			continue // reported in Failed
		}
		from := s.mailboxRefsByID(ctx, sess, accountID, e.MailboxIDs)
		result.MovedCount++
		for _, ref := range from {
			// Keyed by id, which is what makes the breakdown an assertion
			// rather than an inference; the name rides along for reading.
			if sources[ref.ID] == nil {
				sources[ref.ID] = &MailboxCount{MailboxRef: ref}
			}
			sources[ref.ID].Count++
		}
		if enumerate {
			result.Moved = append(result.Moved, MoveChange{ID: e.ID, Subject: e.Subject, From: from})
		}
	}
	return nil
}

// partialRun is the shared "this call stopped early" report, embedded in both
// mutation results. Raising the handle cap made a call able to fail in the
// middle for the first time: at 200 messages a failure was the whole call, and
// at 2,000 it can be the fourth chunk of ten. Trading 69 recoverable turns for
// one unrecoverable one would be a bad bargain, so a stopped run says how far
// it got and hands back a handle naming what is left.
type partialRun struct {
	Aborted bool `json:"aborted,omitempty"`
	// AppliedTo is how many of the named messages were actually reached before
	// the stop — a boundary, not an estimate: the chunks before it completed
	// and the chunks after it never went out.
	AppliedTo int `json:"appliedTo,omitempty"`
	// AbortReason is the underlying failure, verbatim.
	AbortReason string `json:"abortReason,omitempty"`
	// RemainingSelectionID names the messages that were NOT processed, as a
	// fresh selection handle. Finishing the job is a dry run and an apply
	// against it — the same two calls as any other batch, with no ids to
	// recover from a result that failed.
	RemainingSelectionID string `json:"remainingSelectionId,omitempty"`
	RemainingCount       int    `json:"remainingCount,omitempty"`
	AbortNote            string `json:"abortNote,omitempty"`
}

const abortNote = "this call stopped part-way: everything up to appliedTo was applied and nothing after it was attempted. remainingSelectionId names the untouched messages — dry-run and apply it to finish, rather than re-running the original set, which would redo work that already landed."

// abortPartial records a mid-run stop and mints a handle over the tail.
// Minting can fail to be useful (the ids are held in memory and the store may
// evict), so the count is reported either way: knowing how many are left is
// what tells a caller whether to chase them.
func (s *Service) abortPartial(p *partialRun, accountID string, ids []string, done int, err error) {
	p.Aborted = true
	p.AppliedTo = done
	p.AbortReason = err.Error()
	p.AbortNote = abortNote
	rest := ids[done:]
	p.RemainingCount = len(rest)
	if len(rest) > 0 {
		p.RemainingSelectionID = s.handles.putSelection(&selection{
			AccountID: accountID,
			IDs:       append([]string(nil), rest...),
		})
	}
}

// annotateDrift turns the raw placement keys into named mailboxes, resolving
// every mailbox the drift mentions in ONE pass. Per-message resolution would
// be a provider round-trip per drifted message in exactly the case that
// matters: a mailbox deleted between preview and apply is never in the
// refreshed list, so it misses the cache every time it is looked up.
func (s *Service) annotateDrift(ctx context.Context, sess *jmap.Session, accountID string, drift []driftPair) []DriftedPlacement {
	all := map[string]bool{}
	for _, d := range drift {
		for id := range placementIDs(d.Was) {
			all[id] = true
		}
		for id := range placementIDs(d.Now) {
			all[id] = true
		}
	}
	byID := make(map[string]MailboxRef, len(all))
	for _, ref := range s.mailboxRefsByID(ctx, sess, accountID, all) {
		byID[ref.ID] = ref
	}
	refsFor := func(key string) []MailboxRef {
		var out []MailboxRef
		for id := range placementIDs(key) {
			out = append(out, byID[id])
		}
		sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
		return out
	}
	out := make([]DriftedPlacement, 0, len(drift))
	for _, d := range drift {
		out = append(out, DriftedPlacement{ID: d.ID, Was: refsFor(d.Was), Now: refsFor(d.Now)})
	}
	return out
}

// sortedSources renders the source breakdown in a stable, useful order:
// biggest contributor first, then by the label a reader would sort on, then by
// id so the bytes never depend on map iteration.
func sortedSources(m map[string]*MailboxCount) []MailboxCount {
	if len(m) == 0 {
		return nil
	}
	out := make([]MailboxCount, 0, len(m))
	for _, mc := range m {
		out = append(out, *mc)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Count != out[j].Count {
			return out[i].Count > out[j].Count
		}
		if ki, kj := mailboxKey(out[i].MailboxRef), mailboxKey(out[j].MailboxRef); ki != kj {
			return ki < kj
		}
		return out[i].ID < out[j].ID
	})
	return out
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
	Handle          string
	SelectionOffset int
	DryRun          bool
}

// targetSet is the resolved answer: the ids to operate on, plus whichever
// handle named them.
type targetSet struct {
	ids       []string
	selection *SelectionUse
	receipt   *receipt
	// byHandle records that a handle named this set rather than literal ids,
	// which is what decides the size cap: the cap bounds argument payload, and
	// a handle is one field. See validateIDs.
	byHandle bool
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
	// Normalize before counting. A model that pads every declared property
	// rather than omitting keys sends the inert value it can express — "" for
	// the string selectors, and for ids an empty array or one holding nothing
	// usable. None of those name a message, so none of them may count as
	// naming one; a blank string is not a message id under any provider.
	ids := make([]string, 0, len(r.IDs))
	for _, id := range r.IDs {
		if id = strings.TrimSpace(id); id != "" {
			ids = append(ids, id)
		}
	}
	handle := strings.TrimSpace(r.Handle)

	switch {
	case handle == "" && len(ids) == 0:
		return nil, fmt.Errorf("name the messages: handle (a selectionId from email_search, or a receiptId from this tool's dry run), or ids")
	case handle != "" && len(ids) > 0:
		// Spell out the corrected calls. The model that lands here is usually
		// padding rather than confused about the contract, and it needs to be
		// told which inert value to pad with — otherwise it varies the
		// placeholder and retries forever (the v0.13.0 wave).
		return nil, fmt.Errorf("pass a handle or ids, not both — each names the whole set on its own. "+
			"To use the handle, send ids as []: {\"handle\": %q}. To use the ids, send handle as \"\". "+
			"An empty array, an empty string, and an absent key all mean \"not this one\"", handle)
	}

	// The handle's prefix says which protocol this is, so the call states its
	// own mode instead of leaving a reader to test fields for emptiness.
	switch {
	case strings.HasPrefix(handle, receiptPrefix):
		if r.DryRun {
			return nil, fmt.Errorf("a receipt names an apply, not a preview — it was minted BY a dry run; pass a selection handle or ids to preview again")
		}
		rcpt, err := s.handles.getReceipt(handle, kind)
		if err != nil {
			return nil, err
		}
		if rcpt.AccountID != accountID {
			return nil, fmt.Errorf("receipt %q was minted for account %s, not %s", handle, rcpt.AccountID, accountID)
		}
		return &targetSet{ids: rcpt.IDs, receipt: rcpt, byHandle: true}, nil

	case strings.HasPrefix(handle, selectionPrefix):
		held, err := s.handles.getSelection(handle)
		if err != nil {
			return nil, err
		}
		if held.AccountID != accountID {
			return nil, fmt.Errorf("selection %q was minted for account %s, not %s — re-run email_search against the account you mean", handle, held.AccountID, accountID)
		}
		offset := r.SelectionOffset
		if offset < 0 {
			offset = 0
		}
		if offset >= len(held.IDs) {
			return nil, fmt.Errorf("selectionOffset %d is at or past the end of selection %q (%d ids) — the selection is fully consumed", offset, held.ID, len(held.IDs))
		}
		slice := held.IDs[offset:]
		if len(slice) > maxHandleSetIDs {
			slice = slice[:maxHandleSetIDs]
		}
		return &targetSet{
			ids:      slice,
			byHandle: true,
			selection: &SelectionUse{
				ID:        held.ID,
				Offset:    offset,
				Count:     len(slice),
				Remaining: len(held.IDs) - offset - len(slice),
			},
		}, nil

	case handle != "":
		// A handle whose prefix names neither protocol. Say what the two look
		// like rather than "unknown handle": the caller has something in hand
		// and needs to know which tool mints the kind it wants.
		return nil, fmt.Errorf("handle %q is neither a selectionId (%s…, from an email_search result) nor a receiptId (%s…, from this tool's own dry run)",
			handle, selectionPrefix, receiptPrefix)
	}
	return &targetSet{ids: ids}, nil
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
const driftNote = "these messages were in a different state than the dry run saw — something changed them in between. They were still included in this run; each entry carries the state at preview and at apply, so no follow-up fetch is needed to see what happened."

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

// mailboxKey is the human-readable label for a mailbox: display path when
// nested (two folders may share a display name), else name, else id. It is a
// sort key and a phrase fragment — never an identifier, which is why the
// Sources breakdown is keyed by id and only ordered by this.
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

// mailboxLabels renders a set of mailbox refs for an error message: readable
// labels, but never bare ids where a name is known, and never a name alone
// where the id is what makes it checkable.
func mailboxLabels(refs []MailboxRef) string {
	if len(refs) == 0 {
		return "no mailbox"
	}
	out := make([]string, 0, len(refs))
	for _, r := range refs {
		if label := mailboxKey(r); label != r.ID {
			out = append(out, fmt.Sprintf("%s (%s)", label, r.ID))
		} else {
			out = append(out, r.ID)
		}
	}
	return strings.Join(out, " + ")
}

// validateIDs bounds a resolved target set. The bound depends on how the set
// was named, because the two caps protect different things: literal ids are
// argument payload the model had to produce, while a handle is one field
// naming a set the extension is already holding. byHandle carries that
// distinction from resolveTargets rather than re-deriving it here.
func validateIDs(ids []string, byHandle bool) error {
	if len(ids) == 0 {
		return fmt.Errorf("ids is required")
	}
	cap := maxSetIDs
	if byHandle {
		cap = maxHandleSetIDs
	}
	if len(ids) > cap {
		if !byHandle {
			return fmt.Errorf("too many ids (%d): operate on at most %d per call when naming them literally. A selectionId from email_search takes up to %d in one call, and costs one field instead of %d strings", len(ids), maxSetIDs, maxHandleSetIDs, len(ids))
		}
		return fmt.Errorf("too many ids (%d): operate on at most %d per call", len(ids), cap)
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

// collectFailures folds one chunk's notUpdated into the run's accumulator,
// keyed by cause. notFound is separated out because it is not a failure of the
// operation — the message is simply gone — and it is the one group a retry can
// never help.
func (o *setOutcome) collectFailures(a *failureAccum) {
	for id, serr := range o.NotUpdated {
		a.add(serr.Type, id, serr.Description)
	}
}
