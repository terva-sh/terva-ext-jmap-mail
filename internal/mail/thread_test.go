package mail

import (
	"context"
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
