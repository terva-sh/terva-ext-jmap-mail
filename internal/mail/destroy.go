package mail

// Phase-3 permanent destroy (Email/set destroy, RFC 8620 §5.3). Destroyed
// mail is unrecoverable, so the ladder is steeper than move/trash:
//
//  1. The extension config gate (access_level=read-organize-destroy) lives in
//     the glue layer — this service assumes the caller already enforced it.
//  2. Targets must already be in the Trash mailbox (and only there), unless
//     AllowNotInTrash explicitly overrides.
//  3. EVERY non-dry run — any size — requires either the exact confirm phrase
//     or a receipt from this tool's own dry run. A dry run previews the
//     outcome and returns both.
//
// The receipt is the stronger of the two authorizations, not a shortcut around
// the weaker one. A phrase is unforgeable only because it embeds a digest the
// caller cannot compute without having run the preview; a receipt cannot be
// produced at all except by a preview that actually happened, IS the id set
// rather than a fingerprint of it, carries the gate mode it ran under, cannot
// be presented to a different tool, and answers "did that land?" after a lost
// response. So it stands in the phrase's place, exactly as it does for
// move/trash/mark — see handles.go.
//
// Where destroy departs from its recoverable siblings: they report drift
// between preview and apply and proceed, because a message that moved still
// ends up where the caller wanted it. Destroy refuses. A message that left the
// place the preview saw it is a message something else acted on since, and
// acting against the more recent intent is not recoverable here.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"terva-ext-jmap-mail/internal/jmap"
)

// destroyPhrase binds the full destructive intent: the count, the EXACT id
// batch (a digest — a phrase minted by one dry run cannot confirm different
// ids at the same count), the gate mode (skipping the Trash gate mints a
// different phrase than the safe preview did), and the account as requested.
// It takes the resolved ids rather than the params, because a handle names a
// set the params never spelled out.
func destroyPhrase(ids []string, allowNotInTrash bool, account string) string {
	phrase := fmt.Sprintf("destroy %d emails permanently", len(ids))
	if allowNotInTrash {
		phrase += " including outside Trash"
	}
	if a := strings.TrimSpace(account); a != "" {
		phrase += " in account " + a
	}
	return phrase + " [batch " + idBatchDigest(ids) + "]"
}

// idBatchDigest is a short order-independent fingerprint of the id set.
func idBatchDigest(ids []string) string {
	sorted := append([]string(nil), ids...)
	sort.Strings(sorted)
	sum := sha256.Sum256([]byte(strings.Join(sorted, "\x00")))
	return hex.EncodeToString(sum[:3])
}

// DestroyParams are the email_destroy inputs.
type DestroyParams struct {
	Account string
	// Handle and IDs are two ways to name the same thing and are mutually
	// exclusive. Handle is a selectionId (sel_…) or a receiptId (rcp_…); the
	// prefix says which, so the call states its own mode. See resolveTargets.
	Handle          string
	SelectionOffset int
	IDs             []string
	AllowNotInTrash bool // skip the targets-must-be-in-Trash gate
	DryRun          bool
	Confirm         string
}

// DestroyChange identifies one message (would-be) destroyed.
type DestroyChange struct {
	ID      string `json:"id"`
	Subject string `json:"subject,omitempty"`
	// Mailboxes is set only on NotInTrash entries, where the gate refused
	// BECAUSE of where the message is: the check already read the placement,
	// and the remedy differs by what it finds — a message sitting in Inbox
	// needs email_trash, one in Trash AND Archive needs the other label off.
	// Saying "not (only) in Trash" without saying where is an answer the
	// caller has to make a second call to use, on the one tool that cannot be
	// undone.
	Mailboxes []MailboxRef `json:"mailboxes,omitempty"`
}

// DestroyResult is the email_destroy output.
type DestroyResult struct {
	AccountID     string            `json:"accountId"`
	DryRun        bool              `json:"dryRun,omitempty"`
	Destroyed     []DestroyChange   `json:"destroyed"`
	NotInTrash    []DestroyChange   `json:"notInTrash,omitempty"` // dry run: targets the Trash gate would refuse
	Failed        map[string]string `json:"failed,omitempty"`
	NotFound      []string          `json:"notFound,omitempty"`
	ConfirmPhrase string            `json:"confirmPhrase,omitempty"` // dry run: the phrase a real run requires

	// ReceiptID is minted by a dry run that found something to destroy: present
	// it to the real run in place of the ids AND the confirm phrase. It covers
	// the messages the preview listed under destroyed — never the notInTrash
	// ones, which is why it is usable where the phrase beside it is not when
	// the gate held some back.
	ReceiptID string `json:"receiptId,omitempty"`
	// Selection reports which slice of a selection this call consumed, so a
	// caller working through a page larger than maxSetIDs knows whether to
	// come back and at what offset.
	Selection *SelectionUse `json:"selection,omitempty"`
	// Replayed marks a result returned from a receipt that had already been
	// applied — nothing was re-sent to the provider, and nothing was destroyed
	// a second time.
	Replayed  bool   `json:"replayed,omitempty"`
	AppliedAt string `json:"appliedAt,omitempty"`
	Note      string `json:"note"`

	// snapshot is the per-id placement fingerprint this run observed, carried
	// out of destroyInto for the receipt to record. Never serialized.
	snapshot map[string]string
}

const destroyNote = "destroy is PERMANENT and unrecoverable — email_trash is the recoverable alternative"

// destroyBlockedNote is appended on a dry run the Trash gate held messages back
// on. The phrase in the same result covers every id that was asked about, so a
// real run presenting it would be refused by that same gate; the receipt covers
// only what the preview said would go. Saying which of the two to use is
// cheaper than letting the caller discover it by being refused.
const destroyBlockedNote = " — some targets are not in Trash and were left out of destroyed; the receiptId covers only the ones listed there, while confirmPhrase covers every id you named and a run using it would be refused"

// stateMismatchHint translates the ifInState refusal into agent guidance:
// the mailbox changed between snapshot and destroy, so nothing was deleted.
func stateMismatchHint(err error) error {
	var me *jmap.MethodError
	if errors.As(err, &me) && me.Type == "stateMismatch" {
		return fmt.Errorf("destroy aborted, nothing deleted: the mailbox changed between the in-Trash check and the destroy (%v) — re-run with dryRun:true to re-verify the targets, then confirm again", err)
	}
	return err
}

// Destroy permanently removes messages. Both authorization gates — the confirm
// phrase and the receipt claim — are settled before anything is read or
// written, save the session lookup a handle needs to be scoped against. The
// in-Trash gate needs the messages' current mailboxes, so a real run is two
// round trips (snapshot, then destroy). The
// destroy is bound to the snapshot via ifInState (RFC 8620 §5.3): if ANY
// mailbox activity lands between the two requests the server refuses with
// stateMismatch instead of destroying — for the one unrecoverable operation,
// aborting on unrelated inbox churn is the right kind of conservative.
func (s *Service) Destroy(ctx context.Context, p DestroyParams) (*DestroyResult, error) {
	accountID, sess, err := s.account(ctx, p.Account)
	if err != nil {
		return nil, err
	}
	ts, err := s.resolveTargets(accountID, targetRefs{
		IDs: p.IDs, Handle: p.Handle, SelectionOffset: p.SelectionOffset,
		DryRun: p.DryRun,
	}, receiptDestroy)
	if err != nil {
		return nil, err
	}
	if err := validateIDs(ts.ids, ts.byHandle); err != nil {
		return nil, err
	}
	// The gate the preview ran under is part of what it previewed. Flipping it
	// on at apply time would widen the set past what was shown; flipping it off
	// would narrow it, and a silently narrower destroy reads as a successful
	// one.
	if r := ts.receipt; r != nil && r.AllowNotInTrash != p.AllowNotInTrash {
		return nil, fmt.Errorf("receipt %q previewed allowNotInTrash=%t, not %t — re-run the dry run under the gate you mean", r.ID, r.AllowNotInTrash, p.AllowNotInTrash)
	}

	phrase := destroyPhrase(ts.ids, p.AllowNotInTrash, p.Account)
	// A receipt IS the preview the phrase exists to force, names the exact set
	// rather than fingerprinting it, and cannot have been minted without that
	// preview happening — so it authorizes the run in the phrase's place.
	if ts.receipt == nil {
		if !p.DryRun && !strings.EqualFold(strings.TrimSpace(p.Confirm), phrase) {
			return nil, fmt.Errorf("permanent destroy refused: run with dryRun:true (same ids and options) to preview, then either pass the returned receiptId as handle, or re-run with confirm set exactly to %q — destroyed mail is unrecoverable", phrase)
		}
	} else if replay, err := s.handles.claimReceipt(ts.receipt); err != nil {
		return nil, err
	} else if replay != nil {
		return replayDestroy(replay, ts.receipt)
	}

	result, err := s.destroyInto(ctx, sess, accountID, ts, p, phrase)
	if ts.receipt != nil {
		s.handles.releaseReceipt(ts.receipt, result, err)
	}
	if err != nil {
		return nil, err
	}
	if p.DryRun && len(result.Destroyed) > 0 {
		ids := make([]string, 0, len(result.Destroyed))
		for _, c := range result.Destroyed {
			ids = append(ids, c.ID)
		}
		// Over the candidates, not everything asked about: the receipt is the
		// promise the preview made, and the preview promised these.
		result.ReceiptID = s.handles.putReceipt(&receipt{
			Kind: receiptDestroy, AccountID: accountID, IDs: ids,
			AllowNotInTrash: p.AllowNotInTrash, Snapshot: result.snapshot,
			Selection: ts.selection,
		})
	}
	result.Selection = ts.selection
	if ts.receipt != nil && ts.receipt.Selection != nil {
		result.Selection = ts.receipt.Selection
	}
	return result, nil
}

// destroyInto is Destroy's network half: snapshot the targets, apply the Trash
// gate, check the previewed placements still hold, then destroy. Two round
// trips rather than one batch, because the gate has to read the placements
// before it can decide whether to send the set at all.
func (s *Service) destroyInto(ctx context.Context, sess *jmap.Session, accountID string, ts *targetSet, p DestroyParams, phrase string) (*DestroyResult, error) {
	var trashID string
	if !p.AllowNotInTrash {
		trash, err := s.resolveTrash(ctx, sess, accountID)
		if err != nil {
			return nil, fmt.Errorf("cannot verify targets are in Trash (%v) — set allowNotInTrash:true to skip the check", err)
		}
		trashID = trash.ID
	}

	// Snapshot the targets: subjects for reporting, mailboxIds for the gate.
	resp, err := s.call(ctx, sess, []jmap.Invocation{{
		Name: "Email/get",
		Args: map[string]any{
			"accountId":  accountID,
			"ids":        ts.ids,
			"properties": []string{"id", "subject", "mailboxIds"},
		},
		CallID: "g0",
	}})
	if err != nil {
		return nil, err
	}
	gres, err := resp.Result("g0")
	if err != nil {
		return nil, err
	}
	var got struct {
		State string `json:"state"`
		List  []struct {
			ID         string          `json:"id"`
			Subject    string          `json:"subject"`
			MailboxIDs map[string]bool `json:"mailboxIds"`
		} `json:"list"`
		NotFound []string `json:"notFound"`
	}
	if err := json.Unmarshal(gres.Args, &got); err != nil {
		return nil, fmt.Errorf("parse Email/get response: %v", err)
	}

	var candidates, blockers []DestroyChange
	snapshot := make(map[string]string, len(got.List))
	for _, e := range got.List {
		snapshot[e.ID] = placementKey(e.MailboxIDs)
		change := DestroyChange{ID: e.ID, Subject: e.Subject}
		// "In Trash" means Trash is the message's only mailbox — a message
		// still filed elsewhere has not been deleted-to-trash (RFC 8621 §4.6).
		if !p.AllowNotInTrash && !(len(e.MailboxIDs) == 1 && e.MailboxIDs[trashID]) {
			change.Mailboxes = s.mailboxRefsByID(ctx, sess, accountID, e.MailboxIDs)
			blockers = append(blockers, change)
		} else {
			candidates = append(candidates, change)
		}
	}

	result := &DestroyResult{
		AccountID: accountID,
		DryRun:    p.DryRun,
		Destroyed: []DestroyChange{},
		NotFound:  got.NotFound,
		Note:      destroyNote,
		snapshot:  snapshot,
	}
	if p.DryRun {
		result.Destroyed = candidates
		result.NotInTrash = blockers
		result.ConfirmPhrase = phrase
		if len(blockers) > 0 && len(candidates) > 0 {
			result.Note += destroyBlockedNote
		}
		return result, nil
	}
	if len(blockers) > 0 {
		// Name where each blocker actually is: "not in Trash" alone leaves the
		// caller to fetch the placement before it can pick a remedy.
		var where []string
		for _, b := range blockers {
			where = append(where, fmt.Sprintf("%s in %s", b.ID, mailboxLabels(b.Mailboxes)))
		}
		return nil, fmt.Errorf("refusing to destroy: %d of %d targets are not (only) in Trash (%s) — move them with email_trash first, or set allowNotInTrash:true to destroy regardless",
			len(blockers), len(ts.ids), strings.Join(where, "; "))
	}
	// Placement drift, checked after the gate so the gate's better-aimed
	// message wins the cases it covers. Under the default gate this can hardly
	// fire — a message that passed is Trash-only, which is the placement the
	// preview recorded. Under allowNotInTrash there is no gate at all, and this
	// is the only thing standing between a preview of mail in Trash and an
	// apply that destroys mail someone has since pulled back out of it.
	if r := ts.receipt; r != nil {
		if drift := driftedStates(r.Snapshot, r.IDs, snapshot); len(drift) > 0 {
			return nil, s.destroyDriftError(ctx, sess, accountID, drift)
		}
	}
	if len(candidates) == 0 {
		return result, nil // nothing found to destroy; NotFound tells the story
	}

	ids := make([]string, 0, len(candidates))
	for _, c := range candidates {
		ids = append(ids, c.ID)
	}
	// From here the outcome is unknown until the response lands, so a receipt
	// that is presented again must not quietly re-send. See claimReceipt.
	s.handles.markSubmitted(ts.receipt)
	setArgs := map[string]any{"accountId": accountID, "destroy": ids}
	if got.State != "" {
		setArgs["ifInState"] = got.State
	}
	resp, err = s.call(ctx, sess, []jmap.Invocation{{
		Name:   "Email/set",
		Args:   setArgs,
		CallID: "s0",
	}})
	if err != nil {
		return nil, stateMismatchHint(err)
	}
	outcome, err := parseSetResult(resp, "s0")
	if err != nil {
		return nil, stateMismatchHint(err)
	}
	destroyed := map[string]bool{}
	for _, id := range outcome.Destroyed {
		destroyed[id] = true
	}
	for _, c := range candidates {
		if destroyed[c.ID] {
			result.Destroyed = append(result.Destroyed, c)
		}
	}
	if len(outcome.NotDestroyed) > 0 {
		result.Failed = map[string]string{}
		for id, serr := range outcome.NotDestroyed {
			result.Failed[id] = serr.String()
		}
	}
	return result, nil
}

// destroyDriftError refuses an apply whose messages are not where the dry run
// saw them. It names both placements — the comparison that detected the drift
// already knows them, and a caller told only "something changed" would have to
// fetch what this error is holding to decide what to do next.
func (s *Service) destroyDriftError(ctx context.Context, sess *jmap.Session, accountID string, drift []driftPair) error {
	var where []string
	for _, d := range s.annotateDrift(ctx, sess, accountID, drift) {
		where = append(where, fmt.Sprintf("%s was in %s, now in %s", d.ID, mailboxLabels(d.Was), mailboxLabels(d.Now)))
	}
	return fmt.Errorf("destroy aborted, nothing deleted: %d of the previewed messages are no longer where the dry run saw them (%s) — something moved them in between, so the preview you approved is not what this call would destroy; re-run with dryRun:true to see the current state and decide again",
		len(drift), strings.Join(where, "; "))
}

// replayDestroy returns an already-applied receipt's original result rather
// than destroying anything a second time. The stored value is copied before the
// replay markers go on, so the receipt keeps answering with the same outcome
// however many times it is presented — which for this tool is the only record
// left of what the messages were.
func replayDestroy(stored any, r *receipt) (*DestroyResult, error) {
	res, ok := stored.(*DestroyResult)
	if !ok || res == nil {
		return nil, replayLost(r)
	}
	out := *res
	out.Replayed = true
	out.AppliedAt = r.AppliedAt.UTC().Format(time.RFC3339)
	return &out, nil
}
