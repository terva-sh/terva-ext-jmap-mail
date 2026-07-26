package mail

// Phase-3 permanent destroy (Email/set destroy, RFC 8620 §5.3). Destroyed
// mail is unrecoverable, so the ladder is steeper than move/trash:
//
//  1. The extension config gate (access_level=read-organize-destroy) lives in
//     the glue layer — this service assumes the caller already enforced it.
//  2. Targets must already be in the Trash mailbox (and only there), unless
//     AllowNotInTrash explicitly overrides.
//  3. EVERY non-dry run — any size — requires the exact confirm phrase.
//     A dry run previews the outcome and returns the phrase to use.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	"terva-ext-jmap-mail/internal/jmap"
)

// destroyPhrase binds the full destructive intent, all computable before any
// network call: the count, the EXACT id batch (a digest — a phrase minted by
// one dry run cannot confirm different ids at the same count), the gate mode
// (skipping the Trash gate mints a different phrase than the safe preview
// did), and the account as requested.
func destroyPhrase(p DestroyParams) string {
	phrase := fmt.Sprintf("destroy %d emails permanently", len(p.IDs))
	if p.AllowNotInTrash {
		phrase += " including outside Trash"
	}
	if a := strings.TrimSpace(p.Account); a != "" {
		phrase += " in account " + a
	}
	return phrase + " [batch " + idBatchDigest(p.IDs) + "]"
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
	Account         string
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
	Note          string            `json:"note"`
}

const destroyNote = "destroy is PERMANENT and unrecoverable — email_trash is the recoverable alternative"

// stateMismatchHint translates the ifInState refusal into agent guidance:
// the mailbox changed between snapshot and destroy, so nothing was deleted.
func stateMismatchHint(err error) error {
	var me *jmap.MethodError
	if errors.As(err, &me) && me.Type == "stateMismatch" {
		return fmt.Errorf("destroy aborted, nothing deleted: the mailbox changed between the in-Trash check and the destroy (%v) — re-run with dryRun:true to re-verify the targets, then confirm again", err)
	}
	return err
}

// Destroy permanently removes messages. The confirm gate is checked before
// any network traffic; the in-Trash gate needs the messages' current
// mailboxes, so a real run is two round trips (snapshot, then destroy). The
// destroy is bound to the snapshot via ifInState (RFC 8620 §5.3): if ANY
// mailbox activity lands between the two requests the server refuses with
// stateMismatch instead of destroying — for the one unrecoverable operation,
// aborting on unrelated inbox churn is the right kind of conservative.
func (s *Service) Destroy(ctx context.Context, p DestroyParams) (*DestroyResult, error) {
	if err := validateIDs(p.IDs); err != nil {
		return nil, err
	}
	phrase := destroyPhrase(p)
	if !p.DryRun && !strings.EqualFold(strings.TrimSpace(p.Confirm), phrase) {
		return nil, fmt.Errorf("permanent destroy refused: run with dryRun:true (same ids and options) to preview, then re-run with confirm set exactly to %q — destroyed mail is unrecoverable", phrase)
	}
	accountID, sess, err := s.account(ctx, p.Account)
	if err != nil {
		return nil, err
	}
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
			"ids":        p.IDs,
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
	for _, e := range got.List {
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
	}
	if p.DryRun {
		result.Destroyed = candidates
		result.NotInTrash = blockers
		result.ConfirmPhrase = phrase
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
			len(blockers), len(p.IDs), strings.Join(where, "; "))
	}
	if len(candidates) == 0 {
		return result, nil // nothing found to destroy; NotFound tells the story
	}

	ids := make([]string, 0, len(candidates))
	for _, c := range candidates {
		ids = append(ids, c.ID)
	}
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
