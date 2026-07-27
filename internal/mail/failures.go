package mail

// What a mutating run says about the messages it could not change.
//
// Two things forced this. The first is that raising the handle cap to 2,000
// made "Failed and NotFound are never abridged" untenable: at 200 messages the
// worst case was a 200-entry map, which is large but survivable, and at 2,000
// it is the payload problem the handles were introduced to solve, arriving
// through the one field nobody had bounded.
//
// The second is that a list of failed ids is not actually what a caller wants.
// A bare id identifies nothing to a human and cannot be acted on without
// another call; what a caller wants is to SEE the failures, or to RETRY them.
// Both of those are "operate on this set", and this extension already has a
// way to name a set without transmitting it.
//
// So failures come back grouped by cause, each group carrying a count and a
// selection handle. email_get takes that handle to show what failed;
// email_move / email_mark / email_trash take it to retry exactly those. The
// ids themselves stay in the result only while the list is short enough to be
// the useful answer on its own.

import "sort"

// FailureGroup is one cause of failure within a run, with a handle naming the
// messages it affected.
//
// Grouped by cause rather than returned flat because the remedy differs by
// cause and by nothing else: a notFound is gone, a forbidden needs different
// credentials, an overQuota needs space. A single undifferentiated "failed"
// list makes the caller sort them out itself, from strings.
type FailureGroup struct {
	// Type is the JMAP SetError type (RFC 8620 §5.3), or "notFound" for ids the
	// snapshot could not find at all.
	Type  string `json:"type"`
	Count int    `json:"count"`
	// Reason is one representative description, when the server gave one. The
	// type is the machine-readable half and is often opaque on its own.
	Reason string `json:"reason,omitempty"`
	// SelectionID names exactly these messages. Present when the group is too
	// large for its ids to be in the result — pass it to email_get to see them,
	// or back to this tool to retry just them.
	SelectionID string `json:"selectionId,omitempty"`
	// IDs is the alternative, for a group small enough that the list IS the
	// answer and a handle would be ceremony.
	IDs []string `json:"ids,omitempty"`
}

// failureReport is the shared block both mutation results carry. Embedded
// rather than duplicated so email_mark and email_move cannot disagree about
// how a failure is described.
type failureReport struct {
	// Failures groups every message the run could not change, by cause.
	Failures []FailureGroup `json:"failures,omitempty"`
	// FailureNote says what the handles in Failures are for. A token whose use
	// is not stated is a token a model will read past.
	FailureNote string `json:"failureNote,omitempty"`
}

// attachFailures renders the accumulator onto a result, and — on a run too
// large to enumerate — clears the flat id lists the groups have replaced.
// Keeping both would defeat the point: the whole reason to group is that a
// 2,000-message run can fail 500 of them, and 500 map entries is the payload
// this extension has spent five releases removing.
func (s *Service) attachFailures(r *failureReport, accountID string, a *failureAccum, enumerate bool, flatFailed *map[string]string, flatNotFound *[]string) {
	r.Failures = s.failureGroups(accountID, a, enumerate)
	r.FailureNote = noteForFailures(r.Failures)
	if !enumerate {
		*flatFailed = nil
		*flatNotFound = nil
	}
}

// failureAccum collects failures across the chunks of one run.
type failureAccum struct {
	byType  map[string][]string
	reasons map[string]string
}

func newFailureAccum() *failureAccum {
	return &failureAccum{byType: map[string][]string{}, reasons: map[string]string{}}
}

func (a *failureAccum) add(kind, id, reason string) {
	a.byType[kind] = append(a.byType[kind], id)
	if reason != "" && a.reasons[kind] == "" {
		a.reasons[kind] = reason
	}
}

func (a *failureAccum) empty() bool { return len(a.byType) == 0 }

// groups renders the accumulated failures, minting a handle per cause when the
// run was too large to enumerate. The enumerate flag is the same one that
// decides whether a result lists its changed messages: below it the caller is
// reading individual messages and the ids are the answer; above it the caller
// is reading counts, and a handle is what makes the counts actionable.
//
// Handles are minted per cause, not one for everything, because retrying a
// mixed set would retry the notFound ones too — which is the one group where
// retrying is guaranteed to fail again.
func (s *Service) failureGroups(accountID string, a *failureAccum, enumerate bool) []FailureGroup {
	if a.empty() {
		return nil
	}
	kinds := make([]string, 0, len(a.byType))
	for kind := range a.byType {
		kinds = append(kinds, kind)
	}
	// Biggest first, then by name — the same ordering rule the source
	// breakdown uses, and for the same reason: stable bytes, useful first.
	sort.Slice(kinds, func(i, j int) bool {
		if len(a.byType[kinds[i]]) != len(a.byType[kinds[j]]) {
			return len(a.byType[kinds[i]]) > len(a.byType[kinds[j]])
		}
		return kinds[i] < kinds[j]
	})

	out := make([]FailureGroup, 0, len(kinds))
	for _, kind := range kinds {
		ids := a.byType[kind]
		g := FailureGroup{Type: kind, Count: len(ids), Reason: a.reasons[kind]}
		if enumerate {
			g.IDs = ids
		} else {
			g.SelectionID = s.handles.putSelection(&selection{
				AccountID: accountID,
				IDs:       append([]string(nil), ids...),
			})
		}
		out = append(out, g)
	}
	return out
}

// failureNote explains the handles. Without it a model reads a count and a
// token and has to work out that the token is usable, which is the difference
// between a result that can be acted on and one that has to be interpreted.
const failureNote = "each failure group carries a selectionId naming exactly those messages: pass it to email_get to see them, or back to this tool to retry just them. Retrying a notFound group will fail again — those messages are gone."

// failureIDNote covers the small case, where the ids are listed instead.
const failureIDNote = "failures are grouped by cause; the remedy differs by cause, and retrying a notFound group will fail again."

// noteForFailures picks the right one.
func noteForFailures(groups []FailureGroup) string {
	if len(groups) == 0 {
		return ""
	}
	for _, g := range groups {
		if g.SelectionID != "" {
			return failureNote
		}
	}
	return failureIDNote
}
