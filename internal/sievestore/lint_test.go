package sievestore

import (
	"strings"
	"testing"
)

func findingsAt(res LintResult, level string) []Finding {
	var out []Finding
	for _, f := range res.Findings {
		if f.Level == level {
			out = append(out, f)
		}
	}
	return out
}

func hasFinding(res LintResult, level, substr string) bool {
	for _, f := range findingsAt(res, level) {
		if strings.Contains(f.Message, substr) {
			return true
		}
	}
	return false
}

func TestLintCleanSection(t *testing.T) {
	res := Lint(`require ["fileinto", "envelope"];

# healthcare reminders are legitimate
if envelope :is "from" "yourprovider@simplepractice.com" {
  fileinto "Inbox/Healthcare";
}
`)
	if len(res.Findings) != 0 {
		t.Fatalf("clean section produced findings: %+v", res.Findings)
	}
	if len(res.Requires) != 2 || res.Requires[0] != "fileinto" {
		t.Errorf("requires = %v", res.Requires)
	}
	if len(res.FileintoTargets) != 1 || res.FileintoTargets[0] != "Inbox/Healthcare" {
		t.Errorf("targets = %v", res.FileintoTargets)
	}
}

func TestLintEmptyContentIsValid(t *testing.T) {
	if res := Lint("  \n\t\n"); len(res.Findings) != 0 {
		t.Errorf("empty area should lint clean: %+v", res.Findings)
	}
}

func TestLintDiscardIsError(t *testing.T) {
	res := Lint(`if header :contains "subject" "spam" { discard; }`)
	if !hasFinding(res, "error", "discard") {
		t.Errorf("findings = %+v, want discard error", res.Findings)
	}
}

func TestLintStopIsWarning(t *testing.T) {
	res := Lint(`if true { stop; }`)
	if !hasFinding(res, "warning", "stop halts") {
		t.Errorf("findings = %+v, want stop warning", res.Findings)
	}
}

func TestLintRequireCoverage(t *testing.T) {
	// fileinto used without require → warning naming the extension.
	res := Lint(`if header :contains "subject" "x" { fileinto "Inbox"; }`)
	if !hasFinding(res, "warning", `"fileinto" is used but not in a require line`) {
		t.Errorf("findings = %+v", res.Findings)
	}

	// Declared but unused → info; unknown extension → info.
	res = Lint(`require ["fileinto", "frobnicate"];` + "\n" + `keep;`)
	if !hasFinding(res, "info", `"fileinto" is declared but nothing`) {
		t.Errorf("findings = %+v, want unused-require info", res.Findings)
	}
	if !hasFinding(res, "info", `"frobnicate" is not in the documented Fastmail set`) {
		t.Errorf("findings = %+v, want unknown-extension info", res.Findings)
	}

	// Tag-driven requirements: :copy needs copy, :regex needs regex.
	res = Lint(`require "fileinto";` + "\n" + `if header :regex "subject" ".*" { fileinto :copy "Inbox"; }`)
	if !hasFinding(res, "warning", `"copy"`) || !hasFinding(res, "warning", `"regex"`) {
		t.Errorf("findings = %+v, want copy+regex warnings", res.Findings)
	}

	// vacation :seconds needs vacation-seconds too.
	res = Lint(`require "vacation";` + "\n" + `vacation :seconds 600 "away";`)
	if !hasFinding(res, "warning", `"vacation-seconds"`) {
		t.Errorf("findings = %+v, want vacation-seconds warning", res.Findings)
	}

	// vnd.* requires never warn as unknown.
	res = Lint(`require "vnd.cyrus.imip";` + "\n" + `keep;`)
	if hasFinding(res, "info", "vnd.cyrus.imip") {
		t.Errorf("findings = %+v, vnd.* should pass silently", res.Findings)
	}
}

func TestLintBalance(t *testing.T) {
	res := Lint("if true {\n  keep;\n")
	if !hasFinding(res, "error", `unclosed "{"`) {
		t.Errorf("findings = %+v, want unclosed brace error", res.Findings)
	}
	res = Lint("if allof (true, false)) { keep; }")
	if !hasFinding(res, "error", `unmatched ")"`) {
		t.Errorf("findings = %+v, want unmatched paren", res.Findings)
	}
	res = Lint(`if header :is "subject" "oops { keep; }`)
	if !hasFinding(res, "error", "unterminated string") {
		t.Errorf("findings = %+v, want unterminated string", res.Findings)
	}
}

func TestLintMasking(t *testing.T) {
	// Braces, discard, and stop inside comments/strings/text: blocks are
	// NOT findings; unbalanced reality is.
	res := Lint(`require ["fileinto", "vacation"];
# discard { ( [ in a comment
/* stop {{{
   more } */
if header :is "subject" "discard { stop" {
  fileinto "Keep/Braces{}";
}
vacation text:
this body mentions discard and stop and { unbalanced [
.
;
`)
	if len(findingsAt(res, "error")) != 0 || len(findingsAt(res, "warning")) != 0 {
		t.Fatalf("masked constructs produced findings: %+v", res.Findings)
	}
	if len(res.FileintoTargets) != 1 || res.FileintoTargets[0] != "Keep/Braces{}" {
		t.Errorf("targets = %v", res.FileintoTargets)
	}
}

func TestLintUnterminatedTextBlock(t *testing.T) {
	res := Lint("require \"vacation\";\nvacation text:\nnever closed\n")
	if !hasFinding(res, "error", "unterminated text:") {
		t.Errorf("findings = %+v", res.Findings)
	}
}

func TestLintFileintoTagForms(t *testing.T) {
	res := Lint(`require ["fileinto", "mailboxid"];
if true { fileinto :mailboxid "M6789" "Inbox/Healthcare"; }
`)
	if len(res.FileintoTargets) != 1 || res.FileintoTargets[0] != "Inbox/Healthcare" {
		t.Errorf("targets = %v, want the final string (tag args precede)", res.FileintoTargets)
	}
}

func TestLintTruncationMarker(t *testing.T) {
	res := Lint("keep;\n\n[TRUNCATED IN TOOL IMPORT: full paste elsewhere]\n")
	if !hasFinding(res, "error", "truncation marker") {
		t.Errorf("findings = %+v", res.Findings)
	}
	if !SuspectsTruncation("[TRUNCATED …]") || !SuspectsTruncation("truncated placeholder due to paste") {
		t.Error("SuspectsTruncation should match the observed field markers")
	}
	if SuspectsTruncation(`if header :contains "subject" "truncated" { keep; }`) {
		t.Error("plain use of the word truncated must not trip the guard")
	}
}

func TestLintLineNumbers(t *testing.T) {
	res := Lint("keep;\nkeep;\ndiscard;\n")
	errs := findingsAt(res, "error")
	if len(errs) != 1 || errs[0].Line != 3 {
		t.Errorf("errs = %+v, want discard at line 3", errs)
	}
}

// RFC 5228 §8.1: only a line holding exactly "." ends a text: block — an
// indented dot is content (previously ended the block early → phantom errors).
func TestLintTextBlockIndentedDotIsContent(t *testing.T) {
	res := Lint("require \"vacation\";\nvacation text:\nline one\n . \nstill body { [\n.\n;\n")
	if len(findingsAt(res, "error")) != 0 {
		t.Errorf("indented dot ended the block early: %+v", res.Findings)
	}
}

// A fileinto missing its ";" must not swallow the next statement's string.
func TestLintFileintoMissingSemicolonScanBounds(t *testing.T) {
	res := Lint("require \"fileinto\";\nif true {\n  fileinto \"A\"\n  fileinto \"B\";\n}\n")
	if len(res.FileintoTargets) != 2 || res.FileintoTargets[0] != "A" || res.FileintoTargets[1] != "B" {
		t.Errorf("targets = %v, want [A B]", res.FileintoTargets)
	}
}
