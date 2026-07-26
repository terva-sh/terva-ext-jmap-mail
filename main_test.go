package main

import (
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"strings"
	"testing"

	"terva-ext-jmap-mail/internal/config"

	"terva.sh/terva/packages/agent/ext"
)

// Every registered tool schema must be parseable JSON with a properties
// object. The organize schemas are built by concatenating a shared
// targetProperties fragment, so a stray comma there would otherwise reach a
// host as a broken tool definition rather than a failing test.
func TestToolSchemasAreValidJSON(t *testing.T) {
	schemas := allSchemas()
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

// The agent that reported email_get's missing projection concluded no
// projection existed anywhere, having used two tools that had one. The policy
// is where that generalization has to be stated.
func TestContextPolicyTeachesProjections(t *testing.T) {
	for _, want := range []string{"fields", "email_get", "email_get_thread", "email_list_mailboxes", "returnIds"} {
		if !strings.Contains(contextPolicy, want) {
			t.Errorf("contextPolicy never mentions %q", want)
		}
	}
}

// Every schema in the extension, keyed the way a host sees it.
func allSchemas() map[string]json.RawMessage {
	return map[string]json.RawMessage{
		"empty": emptySchema(), "mailboxes": schemaMailboxes(), "search": schemaSearch(),
		"get": schemaGet(), "mark": schemaMark(), "move": schemaMove(), "trash": schemaTrash(),
		"destroy": schemaDestroy(), "thread": schemaThread(),
		"sieveList": schemaSieveList(), "sieveGet": schemaSieveGet(), "sieveDiff": schemaSieveDiff(),
		"sievePut": schemaSievePut(), "sieveRestore": schemaSieveRestore(),
		"sieveMarkApplied": schemaSieveMarkApplied(), "sieveArchive": schemaSieveArchive(),
	}
}

type schemaProp struct {
	Type     string            `json:"type"`
	Enum     []json.RawMessage `json:"enum"`
	Minimum  *float64          `json:"minimum"`
	MinItems *int              `json:"minItems"`
	Items    *struct {
		Type string            `json:"type"`
		Enum []json.RawMessage `json:"enum"`
	} `json:"items"`
}

func schemaProps(t *testing.T, raw json.RawMessage) (map[string]schemaProp, []string) {
	t.Helper()
	var out struct {
		Properties map[string]schemaProp `json:"properties"`
		Required   []string              `json:"required"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("schema does not parse: %v", err)
	}
	return out.Properties, out.Required
}

// A model that fills in every declared property rather than omitting keys can
// only leave a parameter alone if the schema permits the value the code reads
// as "unset". v0.13.0 shipped one floor that forbade it — minItems:1 on the
// organize ids — and refused a 2,000-message wave twenty times over. This
// pins the same rule across every tool: no string enum may omit "", because
// the handlers all resolve "" to the default or refuse it by name, and a
// schema violation reaches the model as a validation failure whose text it
// never sees.
func TestEveryStringEnumAdmitsTheInertValue(t *testing.T) {
	for name, raw := range allSchemas() {
		props, _ := schemaProps(t, raw)
		for prop, spec := range props {
			admits := func(enum []json.RawMessage) bool {
				for _, v := range enum {
					if string(v) == `""` {
						return true
					}
				}
				return false
			}
			if spec.Type == "string" && len(spec.Enum) > 0 && !admits(spec.Enum) {
				t.Errorf("%s.%s is a string enum without \"\": a padding model cannot leave it alone", name, prop)
			}
			if spec.Items != nil && spec.Items.Type == "string" && len(spec.Items.Enum) > 0 && !admits(spec.Items.Enum) {
				t.Errorf("%s.%s items enum has no \"\": a padded [\"\"] is then a schema violation", name, prop)
			}
		}
	}
}

// The same rule for numeric floors. A parameter whose handler reads 0 as
// "not chosen" may not declare minimum:1 — the model has no other way to say
// it isn't choosing.
func TestNoNumericFloorForbidsTheInertValue(t *testing.T) {
	// prop → the tool that declares it. Every integer in the extension either
	// reads 0 as unset (search/thread limit, the body budgets, the paging
	// offsets) or refuses it by name in the handler (sieve versions).
	for name, raw := range allSchemas() {
		props, _ := schemaProps(t, raw)
		for prop, spec := range props {
			if spec.Type != "integer" || spec.Minimum == nil {
				continue
			}
			if *spec.Minimum > 0 {
				t.Errorf("%s.%s declares minimum:%v — 0 is what a padding model sends, and the handler already reads or refuses it",
					name, prop, *spec.Minimum)
			}
		}
	}
}

// hasAttachment is the one parameter whose padded value would have been an
// active choice rather than an inert one: false is a filter excluding every
// message that has an attachment, applied silently with nothing in the result
// to notice it by. It must not be a boolean.
func TestHasAttachmentIsNotABoolean(t *testing.T) {
	props, _ := schemaProps(t, schemaSearch())
	spec, ok := props["hasAttachment"]
	if !ok {
		t.Fatal("email_search lost its hasAttachment filter")
	}
	if spec.Type != "string" {
		t.Fatalf("hasAttachment is %q: a padded false silently excludes every message with an attachment", spec.Type)
	}
}

// email_get must offer the projection its sibling tools have, over a
// vocabulary that is a superset of email_search's so one field list works on
// both.
func TestGetSchemaProjectsLikeItsSiblings(t *testing.T) {
	get, required := schemaProps(t, schemaGet())
	spec, ok := get["fields"]
	if !ok {
		t.Fatal("email_get has no fields projection")
	}
	if spec.Items == nil {
		t.Fatal("email_get fields declares no item enum")
	}
	have := map[string]bool{}
	for _, v := range spec.Items.Enum {
		var s string
		if err := json.Unmarshal(v, &s); err != nil {
			t.Fatal(err)
		}
		have[s] = true
	}
	search, _ := schemaProps(t, schemaSearch())
	for _, v := range search["fields"].Items.Enum {
		var s string
		if err := json.Unmarshal(v, &s); err != nil {
			t.Fatal(err)
		}
		if !have[s] {
			t.Errorf("email_search projects %q but email_get does not — a caller cannot reuse one list", s)
		}
	}
	for _, want := range []string{"cc", "bcc", "replyTo", "attachments", "bodyText", "bodyHtml"} {
		if !have[want] {
			t.Errorf("email_get fields cannot name %q, which only email_get returns", want)
		}
	}
	// ids stays genuinely required here, as on email_destroy: email_get takes
	// no handles, so there is nothing for an inert value to mean.
	if len(required) == 0 || required[0] != "ids" {
		t.Errorf("email_get no longer requires ids: %v", required)
	}
}

// TW-032: the search result must be able to stop returning the ids the
// selection handle replaced, and to keep just the batch boundaries.
func TestSearchSchemaOffersReturnIDs(t *testing.T) {
	props, _ := schemaProps(t, schemaSearch())
	spec, ok := props["returnIds"]
	if !ok {
		t.Fatal("email_search has no returnIds control")
	}
	have := map[string]bool{}
	for _, v := range spec.Enum {
		var s string
		if err := json.Unmarshal(v, &s); err != nil {
			t.Fatal(err)
		}
		have[s] = true
	}
	for _, want := range []string{"", "all", "none", "boundaries"} {
		if !have[want] {
			t.Errorf("returnIds cannot express %q", want)
		}
	}
}

// Auditing is wrapped at registration so a tool added later is audited by
// construction. That only holds if nobody registers one bare, and the compiler
// cannot say so — main() is a sequence of calls, not a table. This reads the
// source instead: every e.Tool(...) must route its handler through a.audited,
// and the audit authority string must match the ext.WithAuthority it is
// registered with, or a record would claim a weaker permission than the tool
// actually holds.
func TestEveryToolIsAudited(t *testing.T) {
	src, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatal(err)
	}
	// Option identifier → the audit constant that must appear on the same
	// line. Matched by name, since that is what the source says; the values
	// behind them are checked against the SDK below.
	authorityFor := map[string]string{
		"netRead": "authNetRead", "extMutate": "authExtMutate",
		"localRead": "authLocalRead", "localData": "authLocalData",
	}
	registration := regexp.MustCompile(`e\.Tool\("([a-z_]+)",[^\n]*\)`)
	found := 0
	for _, line := range strings.Split(string(src), "\n") {
		m := registration.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		found++
		tool := m[1]
		if !strings.Contains(line, "a.audited(") {
			t.Errorf("%s is registered without a.audited — it would leave no audit record", tool)
			continue
		}
		if !strings.Contains(line, `a.audited("`+tool+`"`) {
			t.Errorf("%s is audited under a different name: %s", tool, line)
		}
		for opt, want := range authorityFor {
			if !strings.Contains(line, ", "+opt+")") && !strings.Contains(line, ", "+opt+",") {
				continue
			}
			if !strings.Contains(line, want) {
				t.Errorf("%s is registered %s but audited as something else — the record would misstate its authority", tool, opt)
			}
		}
	}
	if found < 17 {
		t.Errorf("found only %d tool registrations; the scan is not seeing them all", found)
	}

	// The strings a record carries must be the SDK's own authority values. A
	// log claiming "network-read" for a tool the host granted mutation rights
	// to would misrepresent the permission the call ran under.
	for _, pair := range []struct{ ours, sdk string }{
		{authNetRead, ext.AuthorityNetworkRead},
		{authExtMutate, ext.AuthorityExternalMutate},
		{authLocalRead, ext.AuthorityLocalRead},
		{authLocalData, ext.AuthorityLocalData},
	} {
		if pair.ours != pair.sdk {
			t.Errorf("audit authority %q does not match the SDK's %q", pair.ours, pair.sdk)
		}
	}
}

// The audit config must be declared in the manifest, or the host never
// surfaces it and the feature is unreachable.
func TestAuditConfigIsDeclared(t *testing.T) {
	raw, err := os.ReadFile("extension.json")
	if err != nil {
		t.Fatal(err)
	}
	var manifest struct {
		Config []struct {
			Key     string          `json:"key"`
			Type    string          `json:"type"`
			Default json.RawMessage `json:"default"`
		} `json:"config"`
	}
	if err := json.Unmarshal(raw, &manifest); err != nil {
		t.Fatal(err)
	}
	byKey := map[string]string{}
	for _, f := range manifest.Config {
		byKey[f.Key] = f.Type + ":" + string(f.Default)
	}
	// The manifest default and the code default must agree. They are applied
	// in different places — the host overlays the manifest, and
	// app.currentSettings applies the code constant when the host sent nothing
	// — so a disagreement would make the effective default depend on whether
	// the user had ever opened /extensions.
	for _, tc := range []struct{ key, want string }{
		{"audit_log", fmt.Sprintf("bool:%v", config.DefaultAuditLog)},
		{"audit_compress", fmt.Sprintf("bool:%v", config.DefaultAuditCompress)},
		{"audit_retain_days", fmt.Sprintf("int:%d", config.DefaultAuditRetainDays)},
	} {
		if got := byKey[tc.key]; got != tc.want {
			t.Errorf("manifest %s = %q, but the code default is %q", tc.key, got, tc.want)
		}
	}
	// Auditing is ON by default, and that is the whole point of the setting:
	// the deployment most in need of a record is the one that never thought to
	// switch it on.
	if !config.DefaultAuditLog {
		t.Error("audit_log no longer defaults on")
	}
}

// The audit settings default ON, so they must be read by presence rather than
// by value — a host that sends no config at all must still audit, while a user
// who explicitly turned it off must be obeyed. Config.Bool cannot tell those
// apart, which is exactly the bug this guards.
func TestAuditDefaultsSurviveAnEmptyHostConfig(t *testing.T) {
	for _, tc := range []struct {
		name       string
		cfg        ext.Config
		wantLog    bool
		wantRetain int
	}{
		{"host sent nothing", ext.Config{}, true, config.DefaultAuditRetainDays},
		{"user turned it off", ext.Config{"audit_log": json.RawMessage(`false`)}, false, config.DefaultAuditRetainDays},
		{"user turned it on", ext.Config{"audit_log": json.RawMessage(`true`)}, true, config.DefaultAuditRetainDays},
		{"retention kept forever", ext.Config{"audit_retain_days": json.RawMessage(`0`)}, true, 0},
		{"retention set", ext.Config{"audit_retain_days": json.RawMessage(`90`)}, true, 90},
	} {
		if got := configBool(tc.cfg, "audit_log", config.DefaultAuditLog); got != tc.wantLog {
			t.Errorf("%s: audit_log = %v, want %v", tc.name, got, tc.wantLog)
		}
		if got := configInt(tc.cfg, "audit_retain_days", config.DefaultAuditRetainDays); got != tc.wantRetain {
			t.Errorf("%s: audit_retain_days = %d, want %d", tc.name, got, tc.wantRetain)
		}
	}
	// An explicit 0 must mean "keep forever", not "fall back to the default" —
	// the distinction a value-based read would lose.
	if got := configInt(ext.Config{"audit_retain_days": json.RawMessage(`0`)}, "audit_retain_days", 30); got != 0 {
		t.Errorf("explicit 0 retention became %d; a compliance deployment cannot ask to keep everything", got)
	}
}

// The tri-state attachment filter, in the shapes a caller can actually send.
func TestParseHasAttachment(t *testing.T) {
	for _, tc := range []struct {
		raw  string
		want *bool
	}{
		{``, nil}, {`""`, nil}, {`"  "`, nil}, {`null`, nil}, {`"any"`, nil},
		{`"yes"`, boolPtr(true)}, {`"YES"`, boolPtr(true)}, {`"no"`, boolPtr(false)},
		{`true`, boolPtr(true)}, {`false`, boolPtr(false)}, // the older boolean shape still works
	} {
		got, err := parseHasAttachment(json.RawMessage(tc.raw))
		if err != nil {
			t.Errorf("hasAttachment %s: %v", tc.raw, err)
			continue
		}
		switch {
		case tc.want == nil && got != nil:
			t.Errorf("hasAttachment %s = %v, want no filter", tc.raw, *got)
		case tc.want != nil && (got == nil || *got != *tc.want):
			t.Errorf("hasAttachment %s = %v, want %v", tc.raw, got, *tc.want)
		}
	}
	if _, err := parseHasAttachment(json.RawMessage(`"maybe"`)); err == nil {
		t.Error("want a refusal naming the valid values")
	}
}
