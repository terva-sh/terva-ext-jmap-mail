package mail

// email_destroy's selection and receipt protocol. The recoverable tools got
// this in v0.13.0; destroy kept transcribing up to 200 ids per call until
// v0.18.0, on the tool where a transcription slip is the one that cannot be
// walked back.
//
// What these tests are really guarding is that "same semantics as the other
// selections" did not quietly mean "same leniency". A destroy receipt refuses
// three things a move receipt allows.

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"terva-ext-jmap-mail/internal/config"
	"terva-ext-jmap-mail/internal/jmap"
)

// seedSelection mints a selection handle the way email_search would, without
// running a search — these tests are about what destroy does with one.
func seedSelection(s *Service, ids ...string) string {
	return s.handles.putSelection(&selection{AccountID: "A1", IDs: ids})
}

// dryRunDestroy previews and returns the receipt the preview minted.
func dryRunDestroy(t *testing.T, s *Service, p DestroyParams) *DestroyResult {
	t.Helper()
	p.DryRun = true
	res, err := s.Destroy(context.Background(), p)
	if err != nil {
		t.Fatalf("dry run: %v", err)
	}
	return res
}

// The whole point: preview, then apply by handle alone. No ids, no phrase.
func TestDestroyReceiptCarriesTheApply(t *testing.T) {
	s := testService(destroyFake())

	dry := dryRunDestroy(t, s, DestroyParams{IDs: []string{"e-trash"}})
	if dry.ReceiptID == "" {
		t.Fatal("dry run minted no receipt")
	}
	if !strings.HasPrefix(dry.ReceiptID, receiptPrefix) {
		t.Errorf("receiptId %q does not state its own kind", dry.ReceiptID)
	}
	if dry.ConfirmPhrase == "" {
		t.Error("dry run stopped returning the phrase; the ids path still needs it")
	}

	res, err := s.Destroy(context.Background(), DestroyParams{Handle: dry.ReceiptID})
	if err != nil {
		t.Fatalf("apply by receipt: %v", err)
	}
	if len(res.Destroyed) != 1 || res.Destroyed[0].ID != "e-trash" {
		t.Fatalf("destroyed = %v, want exactly the previewed message", res.Destroyed)
	}
	if res.DryRun {
		t.Error("the apply reported itself as a dry run")
	}
}

// A receipt from any other tool must not reach this one. Nothing else in the
// ladder would notice: the ids would be valid, the account would match, and
// the messages would be gone.
func TestDestroyRefusesAnotherToolsReceipt(t *testing.T) {
	s := testService(destroyFake())
	rcp := s.handles.putReceipt(&receipt{
		Kind: receiptTrash, AccountID: "A1", IDs: []string{"e-trash"},
	})
	_, err := s.Destroy(context.Background(), DestroyParams{Handle: rcp})
	if err == nil || !strings.Contains(err.Error(), "trash dry run") {
		t.Fatalf("err = %v, want a refusal naming the receipt's actual kind", err)
	}
}

// The gate is part of what was previewed. A preview that held messages back
// because they sat outside Trash must not be applied with the gate switched
// off — that apply destroys a set nobody looked at.
func TestDestroyReceiptPinsTheTrashGate(t *testing.T) {
	s := testService(destroyFake())
	dry := dryRunDestroy(t, s, DestroyParams{IDs: []string{"e-trash"}})

	_, err := s.Destroy(context.Background(), DestroyParams{
		Handle: dry.ReceiptID, AllowNotInTrash: true,
	})
	if err == nil || !strings.Contains(err.Error(), "allowNotInTrash") {
		t.Fatalf("err = %v, want a refusal naming the gate mismatch", err)
	}

	// And the other way: a permissive preview cannot be applied as a safe one,
	// because the narrower run would silently destroy less than was approved.
	wide := dryRunDestroy(t, s, DestroyParams{
		IDs: []string{"e-trash", "e-inbox"}, AllowNotInTrash: true,
	})
	if _, err := s.Destroy(context.Background(), DestroyParams{Handle: wide.ReceiptID}); err == nil {
		t.Fatal("a permissive preview applied under the safe gate")
	}
}

// When the gate holds messages back, the two authorizations in the same result
// cover different sets. The receipt covers what the preview said would go; the
// phrase covers every id asked about, and a run using it is refused. The result
// has to say which is which — discovering it by being refused costs a round
// trip on the tool with the steepest ladder.
func TestDestroyPreviewSaysWhichAuthorizationIsUsable(t *testing.T) {
	s := testService(destroyFake())
	dry := dryRunDestroy(t, s, DestroyParams{IDs: []string{"e-trash", "e-inbox"}})

	if len(dry.Destroyed) != 1 || len(dry.NotInTrash) != 1 {
		t.Fatalf("preview split wrong: destroyed=%v notInTrash=%v", dry.Destroyed, dry.NotInTrash)
	}
	if !strings.Contains(dry.Note, "receiptId covers only") {
		t.Errorf("note does not distinguish the two authorizations: %q", dry.Note)
	}

	// The receipt goes through, over the candidate only.
	res, err := s.Destroy(context.Background(), DestroyParams{Handle: dry.ReceiptID})
	if err != nil {
		t.Fatalf("receipt over the candidates was refused: %v", err)
	}
	if len(res.Destroyed) != 1 || res.Destroyed[0].ID != "e-trash" {
		t.Fatalf("destroyed = %v, want only the message the preview cleared", res.Destroyed)
	}

	// The phrase from the same preview does not, because it names the blocker
	// too. This is the asymmetry the note exists to explain.
	_, err = s.Destroy(context.Background(), DestroyParams{
		IDs: []string{"e-trash", "e-inbox"}, Confirm: dry.ConfirmPhrase,
	})
	if err == nil || !strings.Contains(err.Error(), "not (only) in Trash") {
		t.Fatalf("err = %v, want the Trash gate to refuse the phrase path", err)
	}
}

// A preview with nothing destroyable mints nothing. A receipt authorizing an
// empty set is a handle that reads as approval and means nothing.
func TestDestroyPreviewWithNoCandidatesMintsNoReceipt(t *testing.T) {
	s := testService(destroyFake())
	dry := dryRunDestroy(t, s, DestroyParams{IDs: []string{"e-inbox"}})
	if dry.ReceiptID != "" {
		t.Errorf("minted receipt %q over an empty candidate set", dry.ReceiptID)
	}
}

// driftFake moves e-trash back out of Trash on the second Email/get, which is
// exactly the window between a preview and its apply. It answers both the
// standalone gets destroy issues and the get+set batch trash issues, so the
// same drift can be put to both tools.
func driftFake() *fake {
	f := &fake{}
	gets := 0
	f.handler = func(calls []jmap.Invocation) *jmap.Response {
		if calls[0].Name == "Mailbox/get" {
			return response(result("Mailbox/get", calls[0].CallID, map[string]any{"list": mailboxFixture}))
		}
		var results []jmap.InvocationResult
		for _, c := range calls {
			switch c.Name {
			case "Email/get":
				gets++
				where := "mb-trash"
				if gets > 1 {
					where = "mb-inbox" // rescued between preview and apply
				}
				results = append(results, result("Email/get", c.CallID, map[string]any{
					"state": "st-snapshot",
					"list": []any{map[string]any{
						"id": "e-trash", "subject": "Rescued",
						"mailboxIds": map[string]bool{where: true},
					}},
				}))
			case "Email/set":
				update, _ := c.Args.(map[string]any)["update"].(map[string]any)
				updated := map[string]any{}
				for id := range update {
					updated[id] = nil
				}
				results = append(results, result("Email/set", c.CallID, map[string]any{
					"updated": updated, "destroyed": destroyArg(c),
					"notUpdated": map[string]any{}, "notDestroyed": map[string]any{},
				}))
			}
		}
		return response(results...)
	}
	return f
}

// Where destroy departs from move and trash. They report drift and act anyway,
// because the message still ends up where the caller wanted. Here the message
// has been pulled back out of Trash since the preview — someone rescued it —
// and destroying it regardless is not a thing that can be undone afterwards.
func TestDestroyAbortsWhenAMessageMovedSinceThePreview(t *testing.T) {
	s := testService(driftFake())
	dry := dryRunDestroy(t, s, DestroyParams{
		IDs: []string{"e-trash"}, AllowNotInTrash: true,
	})

	_, err := s.Destroy(context.Background(), DestroyParams{
		Handle: dry.ReceiptID, AllowNotInTrash: true,
	})
	if err == nil {
		t.Fatal("destroyed a message that had moved since the preview")
	}
	// Both placements, named: the comparison that caught this already knows
	// them, and "something changed" alone sends the caller back for them.
	for _, want := range []string{"nothing deleted", "Trash", "Inbox", "was in", "now in"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("abort message never says %q: %v", want, err)
		}
	}
}

// The recoverable tools do the opposite, and that difference is deliberate
// rather than an oversight in one of them. If email_trash ever starts refusing
// on drift, this pairing is where the choice was written down.
func TestTrashStillProceedsThroughDriftWhereDestroyRefuses(t *testing.T) {
	s := testService(driftFake())
	dry, err := s.Trash(context.Background(), TrashParams{IDs: []string{"e-trash"}, DryRun: true})
	if err != nil {
		t.Fatalf("trash dry run: %v", err)
	}
	res, err := s.Trash(context.Background(), TrashParams{Handle: dry.ReceiptID})
	if err != nil {
		t.Fatalf("trash refused on drift; only destroy should: %v", err)
	}
	if len(res.Drifted) != 1 {
		t.Errorf("trash acted through drift without reporting it: %v", res.Drifted)
	}
}

// Re-presenting an applied receipt returns the original outcome. For destroy
// this is the only thing standing between a lost response and a caller with no
// way left to ask what happened — the messages are gone, so nothing can be
// searched for to find out.
func TestDestroyReceiptReplaysRatherThanActingTwice(t *testing.T) {
	f := destroyFake()
	sets := 0
	base := f.handler
	f.handler = func(calls []jmap.Invocation) *jmap.Response {
		if calls[0].Name == "Email/set" {
			sets++
		}
		return base(calls)
	}
	s := testService(f)

	dry := dryRunDestroy(t, s, DestroyParams{IDs: []string{"e-trash"}})
	first, err := s.Destroy(context.Background(), DestroyParams{Handle: dry.ReceiptID})
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	again, err := s.Destroy(context.Background(), DestroyParams{Handle: dry.ReceiptID})
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	if sets != 1 {
		t.Fatalf("Email/set issued %d times; the second call destroyed again", sets)
	}
	if !again.Replayed || again.AppliedAt == "" {
		t.Errorf("replay is indistinguishable from a fresh run: %+v", again)
	}
	if first.Replayed {
		t.Error("the original result was marked as a replay")
	}
	if len(again.Destroyed) != len(first.Destroyed) {
		t.Errorf("replay reported %d destroyed, original %d", len(again.Destroyed), len(first.Destroyed))
	}
}

// lossyCaller drops the response to an Email/set after the request has gone
// out — the failure the plain fake Caller cannot express, since its handler
// can only return a response. It is the difference between "the server said
// no" and "we never found out", which is the whole question for an
// unrecoverable operation and for a bulk run that stops half-way.
type lossyCaller struct {
	fake *fake
	// failSetAt is the 1-based Email/set to fail; 0 never fails.
	failSetAt int
	sets      int
}

func (l *lossyCaller) FetchSession(ctx context.Context) (*jmap.Session, error) {
	return l.fake.FetchSession(ctx)
}

func (l *lossyCaller) Call(ctx context.Context, apiURL string, using []string, calls []jmap.Invocation) (*jmap.Response, error) {
	for _, c := range calls {
		if c.Name != "Email/set" {
			continue
		}
		l.sets++
		if l.failSetAt > 0 && l.sets == l.failSetAt {
			return nil, errors.New("connection reset after the request was sent")
		}
	}
	return l.fake.Call(ctx, apiURL, using, calls)
}

// The case the recoverable tools do not have to answer. The set reached the
// provider and the response was lost, so the receipt is neither safely
// retryable nor replayable. Re-sending would find the ids gone and report
// notFound — a result that reads as "nothing happened" on the one operation
// where everything may already have.
func TestDestroyReceiptRefusesAfterAnUnknownOutcome(t *testing.T) {
	lost := &lossyCaller{fake: destroyFake()}
	lost.fake.session = testSession()
	s := NewService(lost, config.Normalize(config.Settings{
		APIToken: "tok", AccessLevel: config.AccessOrganize,
	}))

	dry := dryRunDestroy(t, s, DestroyParams{IDs: []string{"e-trash"}})
	lost.failSetAt = 1
	if _, err := s.Destroy(context.Background(), DestroyParams{Handle: dry.ReceiptID}); err == nil {
		t.Fatal("the transport error was swallowed")
	}
	lost.failSetAt = 0

	_, err := s.Destroy(context.Background(), DestroyParams{Handle: dry.ReceiptID})
	if err == nil {
		t.Fatal("re-sent a destroy whose first attempt may already have landed")
	}
	for _, want := range []string{"already sent", "may or may not", "email_search"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal never says %q: %v", want, err)
		}
	}
}

// A selection from email_search feeds the preview, so the ids are never
// transcribed at any point in the chain: search → sel_ → dry run → rcp_ →
// apply.
func TestDestroyTakesASelectionForThePreview(t *testing.T) {
	s := testService(destroyFake())
	sel := seedSelection(s, "e-trash")

	dry := dryRunDestroy(t, s, DestroyParams{Handle: sel})
	if dry.Selection == nil || dry.Selection.ID != sel {
		t.Fatalf("preview did not report the selection slice it consumed: %+v", dry.Selection)
	}
	if len(dry.Destroyed) != 1 {
		t.Fatalf("preview over a selection saw %v", dry.Destroyed)
	}
	res, err := s.Destroy(context.Background(), DestroyParams{Handle: dry.ReceiptID})
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if res.Selection == nil || res.Selection.ID != sel {
		t.Errorf("the apply lost the selection the chain started from: %+v", res.Selection)
	}
}

// A selection is not an authorization. It names a set; it says nothing about
// anyone having looked at it.
func TestDestroySelectionStillNeedsThePhrase(t *testing.T) {
	s := testService(destroyFake())
	sel := seedSelection(s, "e-trash")
	_, err := s.Destroy(context.Background(), DestroyParams{Handle: sel})
	if err == nil || !strings.Contains(err.Error(), "permanent destroy refused") {
		t.Fatalf("err = %v, want the confirm gate to hold for a selection", err)
	}
	// And the phrase it names is over the selection's ids, which the caller
	// never sent — so it has to come back from the refusal to be usable at all.
	if !strings.Contains(err.Error(), idBatchDigest([]string{"e-trash"})) {
		t.Errorf("refusal does not name a phrase bound to the selection's ids: %v", err)
	}
}

// A receipt names an apply. Presenting one to a preview is a sign the caller
// has lost track of which half of the protocol it is in, and the safe reading
// is not "preview it again".
func TestDestroyRefusesAReceiptOnADryRun(t *testing.T) {
	s := testService(destroyFake())
	dry := dryRunDestroy(t, s, DestroyParams{IDs: []string{"e-trash"}})
	_, err := s.Destroy(context.Background(), DestroyParams{Handle: dry.ReceiptID, DryRun: true})
	if err == nil || !strings.Contains(err.Error(), "names an apply") {
		t.Fatalf("err = %v", err)
	}
}

// An expired receipt is its own case: the fix is to preview again, and a caller
// told "expired" does not go looking for a bug.
func TestDestroyReceiptExpires(t *testing.T) {
	s := testService(destroyFake())
	dry := dryRunDestroy(t, s, DestroyParams{IDs: []string{"e-trash"}})
	s.handles.now = func() time.Time { return time.Now().Add(handleTTL + time.Minute) }
	_, err := s.Destroy(context.Background(), DestroyParams{Handle: dry.ReceiptID})
	if err == nil || !strings.Contains(err.Error(), "expired") {
		t.Fatalf("err = %v, want an expiry refusal", err)
	}
}

// Padding: a model that fills every declared property sends handle:"" and
// ids:[] together, and both mean "not this one". The refusal has to be the
// one that names what to send, not a schema violation the model never reads.
func TestDestroyPaddingRefusalNamesBothSelectors(t *testing.T) {
	s := testService(destroyFake())
	_, err := s.Destroy(context.Background(), DestroyParams{
		Handle: "", IDs: []string{"", "   "}, SelectionOffset: 0,
	})
	if err == nil || !strings.Contains(err.Error(), "name the messages") {
		t.Fatalf("err = %v, want the both-inert refusal", err)
	}
	_, err = s.Destroy(context.Background(), DestroyParams{
		Handle: "sel_x", IDs: []string{"e-trash"},
	})
	if err == nil || !strings.Contains(err.Error(), "not both") {
		t.Fatalf("err = %v, want the two-selectors refusal", err)
	}
}
