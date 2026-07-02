package sievestore

import (
	"strings"
	"testing"
)

func TestUnifiedIdentical(t *testing.T) {
	if d := Unified("a", "b", "same\ntext\n", "same\ntext\n"); d != "" {
		t.Errorf("identical texts produced a diff:\n%s", d)
	}
}

func TestUnifiedSimpleChange(t *testing.T) {
	a := "one\ntwo\nthree\n"
	b := "one\n2\nthree\n"
	want := "--- a@v1\n+++ a@v2\n@@ -1,3 +1,3 @@\n one\n-two\n+2\n three\n"
	if got := Unified("a@v1", "a@v2", a, b); got != want {
		t.Errorf("got:\n%q\nwant:\n%q", got, want)
	}
}

func TestUnifiedAddToEmpty(t *testing.T) {
	got := Unified("a", "b", "", "new line\n")
	if !strings.Contains(got, "@@ -0,0 +1,1 @@\n+new line\n") {
		t.Errorf("empty→content diff:\n%s", got)
	}
}

func TestUnifiedRemoveAll(t *testing.T) {
	got := Unified("a", "b", "gone\n", "")
	if !strings.Contains(got, "@@ -1,1 +0,0 @@\n-gone\n") {
		t.Errorf("content→empty diff:\n%s", got)
	}
}

func TestUnifiedMultipleHunks(t *testing.T) {
	// Two changes separated by far more than 2*ctx equal lines → two hunks.
	mid := strings.Repeat("same\n", 20)
	a := "first\n" + mid + "last\n"
	b := "FIRST\n" + mid + "LAST\n"
	got := Unified("a", "b", a, b)
	if strings.Count(got, "@@ -") != 2 {
		t.Fatalf("want 2 hunks:\n%s", got)
	}
	if !strings.Contains(got, "-first\n+FIRST\n") || !strings.Contains(got, "-last\n+LAST\n") {
		t.Errorf("hunk contents:\n%s", got)
	}
}

func TestUnifiedMergesNearbyChanges(t *testing.T) {
	// Changes 3 equal lines apart (≤ 2*ctx) merge into one hunk.
	a := "a\nx\nm1\nm2\nm3\ny\nb\n"
	b := "a\nX\nm1\nm2\nm3\nY\nb\n"
	got := Unified("a", "b", a, b)
	if strings.Count(got, "@@ -") != 1 {
		t.Errorf("want 1 merged hunk:\n%s", got)
	}
}

func TestUnifiedContextBounds(t *testing.T) {
	// Change in the middle of a long file: exactly 3 context lines each side.
	var lines []string
	for i := 1; i <= 11; i++ {
		lines = append(lines, strings.Repeat("l", i))
	}
	a := strings.Join(lines, "\n") + "\n"
	lines[5] = "CHANGED"
	b := strings.Join(lines, "\n") + "\n"
	got := Unified("a", "b", a, b)
	if !strings.Contains(got, "@@ -3,7 +3,7 @@") {
		t.Errorf("hunk header:\n%s", got)
	}
}

// A byte-level difference that splits to identical lines (trailing newline)
// must not render as "identical" while pendingChanges says true.
func TestUnifiedTrailingNewlineOnlyChange(t *testing.T) {
	out := Unified("a", "b", "keep;\n", "keep;")
	if out == "" || !strings.Contains(out, "trailing newline") {
		t.Errorf("newline-only diff = %q, want explicit marker", out)
	}
}
