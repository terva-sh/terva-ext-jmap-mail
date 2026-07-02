package jmaptest

import (
	"encoding/json"
	"reflect"
	"testing"
)

func decode(t *testing.T, s string) any {
	t.Helper()
	var v any
	if err := json.Unmarshal([]byte(s), &v); err != nil {
		t.Fatal(err)
	}
	return v
}

// RFC 8620 §3.7 pointer semantics, including the "*" wildcard with
// flattening and RFC 6901 escapes.
func TestEvalPointer(t *testing.T) {
	doc := decode(t, `{
		"ids": ["a", "b"],
		"list": [
			{"threadId": "t1", "emailIds": ["e1", "e2"]},
			{"threadId": "t2", "emailIds": ["e3"]}
		],
		"weird~key": {"a/b": 7},
		"nested": [[1, 2], [3]]
	}`)

	cases := []struct {
		path string
		want any
	}{
		{"/ids", []any{"a", "b"}},
		{"/ids/1", "b"},
		{"/list/*/threadId", []any{"t1", "t2"}},
		{"/list/*/emailIds", []any{"e1", "e2", "e3"}}, // arrays flatten
		{"/weird~0key/a~1b", float64(7)},              // ~0 → ~, ~1 → /
		{"/nested/*", []any{float64(1), float64(2), float64(3)}},
	}
	for _, c := range cases {
		got, err := evalPointer(doc, c.path)
		if err != nil {
			t.Errorf("evalPointer(%q): %v", c.path, err)
			continue
		}
		if !reflect.DeepEqual(got, c.want) {
			t.Errorf("evalPointer(%q) = %#v, want %#v", c.path, got, c.want)
		}
	}

	for _, bad := range []string{"/missing", "/ids/9", "/ids/x", "ids", "/ids/0/deep"} {
		if _, err := evalPointer(doc, bad); err == nil {
			t.Errorf("evalPointer(%q) should fail", bad)
		}
	}
}

func TestResolveRefs(t *testing.T) {
	priors := []prior{{
		name:   "Email/query",
		callID: "q0",
		args:   decode(t, `{"ids": ["e1", "e2"]}`),
	}}

	args := map[string]any{
		"accountId": "A",
		"#ids":      map[string]any{"resultOf": "q0", "name": "Email/query", "path": "/ids"},
	}
	if merr := resolveRefs(args, priors); merr != nil {
		t.Fatalf("resolveRefs: %+v", merr)
	}
	if !reflect.DeepEqual(args["ids"], []any{"e1", "e2"}) {
		t.Errorf("ids = %#v", args["ids"])
	}
	if _, still := args["#ids"]; still {
		t.Error("#ids must be removed after resolution")
	}

	// Unknown call id → invalidResultReference (§3.7).
	merr := resolveRefs(map[string]any{
		"#ids": map[string]any{"resultOf": "nope", "name": "Email/query", "path": "/ids"},
	}, priors)
	if merr == nil || merr.Type != "invalidResultReference" {
		t.Errorf("merr = %+v, want invalidResultReference", merr)
	}

	// Name mismatch on the referenced call → invalidResultReference.
	merr = resolveRefs(map[string]any{
		"#ids": map[string]any{"resultOf": "q0", "name": "Mailbox/get", "path": "/ids"},
	}, priors)
	if merr == nil || merr.Type != "invalidResultReference" {
		t.Errorf("merr = %+v, want invalidResultReference on name mismatch", merr)
	}

	// Both plain and referenced forms → invalidArguments.
	merr = resolveRefs(map[string]any{
		"ids":  []any{"x"},
		"#ids": map[string]any{"resultOf": "q0", "name": "Email/query", "path": "/ids"},
	}, priors)
	if merr == nil || merr.Type != "invalidArguments" {
		t.Errorf("merr = %+v, want invalidArguments on duplicate", merr)
	}
}
