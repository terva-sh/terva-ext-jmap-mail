package main

import (
	"encoding/json"
	"strings"
	"testing"
)

// Every registered tool schema must be parseable JSON with a properties
// object. The organize schemas are built by concatenating a shared
// targetProperties fragment, so a stray comma there would otherwise reach a
// host as a broken tool definition rather than a failing test.
func TestToolSchemasAreValidJSON(t *testing.T) {
	schemas := map[string]json.RawMessage{
		"empty": emptySchema(), "mailboxes": schemaMailboxes(), "search": schemaSearch(),
		"get": schemaGet(), "mark": schemaMark(), "move": schemaMove(), "trash": schemaTrash(),
		"destroy": schemaDestroy(), "thread": schemaThread(),
		"sieveList": schemaSieveList(), "sieveGet": schemaSieveGet(), "sieveDiff": schemaSieveDiff(),
		"sievePut": schemaSievePut(), "sieveRestore": schemaSieveRestore(),
		"sieveMarkApplied": schemaSieveMarkApplied(), "sieveArchive": schemaSieveArchive(),
	}
	for name, raw := range schemas {
		var out map[string]any
		if err := json.Unmarshal(raw, &out); err != nil {
			t.Errorf("%s schema is not valid JSON: %v\n%s", name, err, raw)
			continue
		}
		if props, _ := out["properties"].(map[string]any); len(props) == 0 && name != "empty" {
			t.Errorf("%s schema has no properties", name)
		}
	}

	// The three organize tools must offer all three ways of naming a set —
	// a tool missing one silently forces the caller back to retyping ids.
	for _, name := range []string{"mark", "move", "trash"} {
		var out struct {
			Properties map[string]json.RawMessage `json:"properties"`
			Required   []string                   `json:"required"`
		}
		if err := json.Unmarshal(schemas[name], &out); err != nil {
			t.Fatal(err)
		}
		for _, want := range []string{"ids", "selection", "selectionOffset", "receipt"} {
			if _, ok := out.Properties[want]; !ok {
				t.Errorf("%s schema is missing %s", name, want)
			}
		}
		// ids cannot be required: selection and receipt are alternatives to it,
		// and the handler decides which was meant.
		for _, req := range out.Required {
			if req == "ids" || req == "toMailbox" {
				t.Errorf("%s schema still requires %q, which a selection or receipt replaces", name, req)
			}
		}
	}
}

// The context policy is the only place the model is told to prefer handles
// over retyping ids; the tool descriptions alone have not been enough before.
func TestContextPolicyTeachesHandles(t *testing.T) {
	for _, want := range []string{"selectionId", "receiptId", "selectionOffset", "NEVER retype"} {
		if !strings.Contains(contextPolicy, want) {
			t.Errorf("contextPolicy never mentions %q", want)
		}
	}
}
