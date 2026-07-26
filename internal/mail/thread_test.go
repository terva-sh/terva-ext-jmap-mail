package mail

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"terva-ext-jmap-mail/internal/jmap"
)

func threadFake() *fake {
	f := &fake{}
	f.handler = func(calls []jmap.Invocation) *jmap.Response {
		if calls[0].Name == "Mailbox/get" {
			return response(result("Mailbox/get", calls[0].CallID, map[string]any{"list": mailboxFixture}))
		}
		var results []jmap.InvocationResult
		for _, c := range calls {
			switch {
			case c.Name == "Email/get" && c.CallID == "e0":
				results = append(results, result("Email/get", "e0", map[string]any{"list": []any{map[string]any{"id": "e1", "threadId": "t1"}}}))
			case c.Name == "Thread/get":
				results = append(results, result("Thread/get", c.CallID, map[string]any{
					"list": []any{map[string]any{"id": "t1", "emailIds": []string{"e1", "e2"}}},
				}))
			case c.Name == "Email/get":
				second := map[string]any{}
				for k, v := range emailFixture {
					second[k] = v
				}
				second["id"] = "e2"
				results = append(results, result("Email/get", c.CallID, map[string]any{"list": []any{emailFixture, second}}))
			}
		}
		return response(results...)
	}
	return f
}

func TestGetThreadByEmailID(t *testing.T) {
	f := threadFake()
	s := testService(f)
	res, err := s.GetThread(context.Background(), ThreadParams{EmailID: "e1"})
	if err != nil {
		t.Fatal(err)
	}
	if res.ThreadID != "t1" || res.Count != 2 || len(res.Emails) != 2 {
		t.Fatalf("res = %+v", res)
	}

	// One batched request: Email/get → Thread/get → Email/get, chained by
	// result references (RFC 8620 §3.7 wildcard paths).
	batch := findBatch(t, f, "Email/get")
	if len(batch) != 3 {
		t.Fatalf("batch has %d calls, want 3", len(batch))
	}
	tArgs := stringify(argsOf(t, batch[1])["#ids"])
	if !strings.Contains(tArgs, `"resultOf":"e0"`) || !strings.Contains(tArgs, `"path":"/list/*/threadId"`) {
		t.Errorf("Thread/get #ids = %s", tArgs)
	}
	gArgs := stringify(argsOf(t, batch[2])["#ids"])
	if !strings.Contains(gArgs, `"resultOf":"t0"`) || !strings.Contains(gArgs, `"path":"/list/*/emailIds"`) {
		t.Errorf("final Email/get #ids = %s", gArgs)
	}
	// Summaries only by default — no body fetch.
	if strings.Contains(stringify(argsOf(t, batch[2])["properties"]), "bodyValues") {
		t.Errorf("default thread fetch must not request bodies")
	}
}

func TestGetThreadByThreadID(t *testing.T) {
	f := threadFake()
	s := testService(f)
	res, err := s.GetThread(context.Background(), ThreadParams{ThreadID: "t1"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Count != 2 {
		t.Fatalf("res = %+v", res)
	}
	batch := findBatch(t, f, "Thread/get")
	if len(batch) != 2 {
		t.Fatalf("batch = %v", batch)
	}
}

func TestGetThreadWithBodies(t *testing.T) {
	f := threadFake()
	s := testService(f)
	res, err := s.GetThread(context.Background(), ThreadParams{ThreadID: "t1", IncludeBodies: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Full) != 2 || len(res.Emails) != 0 {
		t.Fatalf("res = %+v", res)
	}
	batch := findBatch(t, f, "Thread/get")
	args := argsOf(t, batch[1])
	if args["fetchTextBodyValues"] != true {
		t.Errorf("includeBodies should fetch text bodies: %v", args)
	}
}

// bigThreadFake models a long mailing-list thread: n messages, emailIds in
// receivedAt order (RFC 8621 §3), and an Email/get list in REVERSE order,
// which a server is free to do (RFC 8620 §5.1).
func bigThreadFake(n int) *fake {
	f := &fake{}
	f.handler = func(calls []jmap.Invocation) *jmap.Response {
		if calls[0].Name == "Mailbox/get" {
			return response(result("Mailbox/get", calls[0].CallID, map[string]any{"list": mailboxFixture}))
		}
		ids := make([]string, n)
		list := make([]any, n)
		for i := range n {
			ids[i] = fmt.Sprintf("e-%d", i)
			e := map[string]any{}
			for k, v := range emailFixture {
				e[k] = v
			}
			e["id"] = ids[i]
			e["subject"] = fmt.Sprintf("Re: long thread (%d)", i)
			list[n-1-i] = e
		}
		var results []jmap.InvocationResult
		for _, c := range calls {
			switch c.Name {
			case "Thread/get":
				results = append(results, result("Thread/get", c.CallID, map[string]any{
					"list": []any{map[string]any{"id": "t1", "emailIds": ids}},
				}))
			case "Email/get":
				results = append(results, result("Email/get", c.CallID, map[string]any{"list": list}))
			}
		}
		return response(results...)
	}
	return f
}

// A thread is an unbounded object — the result must not be. The cap keeps the
// newest end, since that is the end a reader is asking about, and says how
// many older messages it left behind.
func TestGetThreadCapsResult(t *testing.T) {
	const total = 150
	for _, tc := range []struct {
		name   string
		params ThreadParams
		want   int
	}{
		{"summaries", ThreadParams{ThreadID: "t1"}, maxThreadSummaries},
		{"with bodies", ThreadParams{ThreadID: "t1", IncludeBodies: true}, maxThreadBodies},
		{"caller asks for fewer", ThreadParams{ThreadID: "t1", Limit: 5}, 5},
		{"caller cannot exceed the cap", ThreadParams{ThreadID: "t1", Limit: 5000}, maxThreadSummaries},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := testService(bigThreadFake(total))
			res, err := s.GetThread(context.Background(), tc.params)
			if err != nil {
				t.Fatal(err)
			}
			if res.Count != total {
				t.Errorf("count = %d, want the thread's real size %d", res.Count, total)
			}
			if res.Returned != tc.want || res.Omitted != total-tc.want {
				t.Errorf("returned/omitted = %d/%d, want %d/%d", res.Returned, res.Omitted, tc.want, total-tc.want)
			}
			// Newest end kept, in thread order — Email/get answered reversed.
			var got []string
			for _, e := range res.Emails {
				got = append(got, e.ID)
			}
			for _, e := range res.Full {
				got = append(got, e.ID)
			}
			if first, last := fmt.Sprintf("e-%d", total-tc.want), fmt.Sprintf("e-%d", total-1); got[0] != first || got[len(got)-1] != last {
				t.Errorf("kept %s … %s, want the newest %d (%s … %s)", got[0], got[len(got)-1], tc.want, first, last)
			}
		})
	}
}

func TestGetThreadShortThreadReportsNoOmission(t *testing.T) {
	s := testService(threadFake())
	res, err := s.GetThread(context.Background(), ThreadParams{ThreadID: "t1"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Count != 2 || res.Returned != 2 || res.Omitted != 0 {
		t.Errorf("res = count %d returned %d omitted %d, want 2/2/0", res.Count, res.Returned, res.Omitted)
	}
}

func TestGetThreadFieldsProjection(t *testing.T) {
	f := bigThreadFake(3)
	s := testService(f)
	res, err := s.GetThread(context.Background(), ThreadParams{ThreadID: "t1", Fields: []string{"subject"}})
	if err != nil {
		t.Fatal(err)
	}
	props := stringify(argsOf(t, findBatch(t, f, "Thread/get")[1])["properties"])
	if props != `["id","subject"]` {
		t.Errorf("properties = %s", props)
	}
	if res.Emails[0].Subject == "" || res.Emails[0].Preview != "" || len(res.Emails[0].From) != 0 {
		t.Errorf("projection not applied: %+v", res.Emails[0])
	}

	// With bodies the projection would save nothing, so it is refused rather
	// than silently ignored.
	_, err = s.GetThread(context.Background(), ThreadParams{ThreadID: "t1", Fields: []string{"subject"}, IncludeBodies: true})
	if err == nil || !strings.Contains(err.Error(), "includeBodies") {
		t.Errorf("err = %v, want a refusal naming the conflict", err)
	}
}

func TestGetThreadInputValidation(t *testing.T) {
	s := testService(threadFake())
	if _, err := s.GetThread(context.Background(), ThreadParams{}); err == nil {
		t.Error("want error when neither id given")
	}
	if _, err := s.GetThread(context.Background(), ThreadParams{ThreadID: "t1", EmailID: "e1"}); err == nil {
		t.Error("want error when both ids given")
	}
}

func TestGetThreadNotFound(t *testing.T) {
	f := &fake{}
	f.handler = func(calls []jmap.Invocation) *jmap.Response {
		return response(result("Thread/get", "t0", map[string]any{"list": []any{}, "notFound": []string{"t-gone"}}))
	}
	s := testService(f)
	_, err := s.GetThread(context.Background(), ThreadParams{ThreadID: "t-gone"})
	if err == nil || !strings.Contains(err.Error(), "no thread found") {
		t.Fatalf("err = %v", err)
	}
}
