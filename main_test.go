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

		// Every mutually exclusive parameter needs a representable "not this
		// one" value. "" covers selection and receipt; for ids that is [], and
		// minItems:1 forbids it. v0.13.0 shipped with minItems:1 here and a
		// padding model — which sends every declared property rather than
		// omitting keys — had to substitute ["placeholder"], making a handle
		// unusable and refusing an entire 2,000-message wave.
		var ids struct {
			MinItems *int `json:"minItems"`
			MaxItems *int `json:"maxItems"`
		}
		if err := json.Unmarshal(out.Properties["ids"], &ids); err != nil {
			t.Fatal(err)
		}
		if ids.MinItems != nil {
			t.Errorf("%s schema sets minItems:%d on ids — [] must stay valid as the inert padding value", name, *ids.MinItems)
		}
		if ids.MaxItems == nil {
			t.Errorf("%s schema dropped maxItems on ids — that bound is real, unlike the floor", name)
		}
	}

	// email_destroy is the exception and must stay one: it accepts no handles,
	// so ids is genuinely mandatory there and no padding conflict can arise.
	var destroy struct {
		Properties map[string]json.RawMessage `json:"properties"`
		Required   []string                   `json:"required"`
	}
	if err := json.Unmarshal(schemas["destroy"], &destroy); err != nil {
		t.Fatal(err)
	}
	var destroyIDs struct {
		MinItems *int `json:"minItems"`
	}
	if err := json.Unmarshal(destroy.Properties["ids"], &destroyIDs); err != nil {
		t.Fatal(err)
	}
	if destroyIDs.MinItems == nil || *destroyIDs.MinItems != 1 {
		t.Errorf("email_destroy dropped minItems on ids; it takes no handles, so ids is genuinely required there")
	}
	if len(destroy.Required) == 0 || destroy.Required[0] != "ids" {
		t.Errorf("email_destroy no longer requires ids: %v", destroy.Required)
	}
}

// The context policy is the only place the model is told to prefer handles
// over retyping ids; the tool descriptions alone have not been enough before.
func TestContextPolicyTeachesHandles(t *testing.T) {
	for _, want := range []string{"selectionId", "receiptId", "selectionOffset", "NEVER retype", "never a placeholder id"} {
		if !strings.Contains(contextPolicy, want) {
			t.Errorf("contextPolicy never mentions %q", want)
		}
	}
}
