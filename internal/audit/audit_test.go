package audit

import (
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"
)

func read(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	sort.Strings(names)
	return names
}

func lines(t *testing.T, path string) []Record {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var out []Record
	for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		if line == "" {
			continue
		}
		var r Record
		if err := json.Unmarshal([]byte(line), &r); err != nil {
			t.Fatalf("line is not valid JSON: %q: %v", line, err)
		}
		out = append(out, r)
	}
	return out
}

// JSON Lines is the format precisely so a collector can tail the file without
// parsing help: one record per line, each line independently valid.
func TestWritesOneValidJSONObjectPerLine(t *testing.T) {
	dir := t.TempDir()
	lg, err := New(dir, 0, 30, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer lg.Close()

	for i := 0; i < 3; i++ {
		lg.Write(Record{
			Time: "2026-07-26T21:44:02Z", Tool: "email_move", Authority: "external-mutation",
			Access: "read-organize", Account: "u1", Outcome: "ok", Millis: 412,
			Detail: map[string]any{"movedCount": 200},
		})
	}
	names := read(t, dir)
	if len(names) != 1 || !strings.HasPrefix(names[0], "audit-") {
		t.Fatalf("files = %v", names)
	}
	recs := lines(t, filepath.Join(dir, names[0]))
	if len(recs) != 3 {
		t.Fatalf("wrote %d records, want 3", len(recs))
	}
	if recs[0].Tool != "email_move" || recs[0].Access != "read-organize" || recs[0].Millis != 412 {
		t.Errorf("record = %+v", recs[0])
	}
}

// The directory holds one user's mailbox activity on a machine that may have
// several accounts on it.
func TestFilesAreNotWorldReadable(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "audit")
	lg, err := New(dir, 0, 30, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer lg.Close()
	lg.Write(Record{Tool: "email_search"})

	di, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if perm := di.Mode().Perm(); perm&0o077 != 0 {
		t.Errorf("audit dir mode = %o, want no group/other access", perm)
	}
	fi, err := os.Stat(lg.Path())
	if err != nil {
		t.Fatal(err)
	}
	if perm := fi.Mode().Perm(); perm&0o077 != 0 {
		t.Errorf("audit file mode = %o, want no group/other access", perm)
	}
}

// A date roll starts a new file, so retention can work on whole days and a
// reader can find a run by the day it happened.
func TestRollsOnTheDate(t *testing.T) {
	dir := t.TempDir()
	lg, err := New(dir, 0, 30, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer lg.Close()

	day := time.Date(2026, 7, 26, 23, 59, 0, 0, time.UTC)
	lg.now = func() time.Time { return day }
	lg.Write(Record{Tool: "email_move"})
	lg.now = func() time.Time { return day.Add(2 * time.Minute) } // past midnight UTC
	lg.Write(Record{Tool: "email_move"})

	names := read(t, dir)
	if len(names) != 2 || names[0] != "audit-2026-07-26.001.jsonl" || names[1] != "audit-2026-07-27.001.jsonl" {
		t.Fatalf("files = %v, want one per UTC day", names)
	}
}

// Within a day, size is the backstop: a runaway run must not produce one file
// nothing can open.
func TestRollsOnSizeWithinADay(t *testing.T) {
	dir := t.TempDir()
	lg, err := New(dir, 400, 30, false, nil) // a few records per file
	if err != nil {
		t.Fatal(err)
	}
	defer lg.Close()
	lg.now = func() time.Time { return time.Date(2026, 7, 26, 12, 0, 0, 0, time.UTC) }

	for i := 0; i < 12; i++ {
		lg.Write(Record{Time: "2026-07-26T12:00:00Z", Tool: "email_search", Outcome: "ok"})
	}
	names := read(t, dir)
	if len(names) < 2 {
		t.Fatalf("files = %v, want a size roll", names)
	}
	// Sorting the filenames must sort the records: the sequence is always
	// present and padded so a continuation cannot sort before its own start.
	if names[0] != "audit-2026-07-26.001.jsonl" || names[1] != "audit-2026-07-26.002.jsonl" {
		t.Errorf("files = %v, want chronological == lexicographic", names)
	}
	// Nothing may be lost across a roll.
	var total int
	for _, n := range names {
		total += len(lines(t, filepath.Join(dir, n)))
	}
	if total != 12 {
		t.Errorf("wrote 12 records, %d survived the rolls", total)
	}
	for _, n := range names {
		fi, err := os.Stat(filepath.Join(dir, n))
		if err != nil {
			t.Fatal(err)
		}
		if fi.Size() > 400 {
			t.Errorf("%s is %d bytes, over the cap", n, fi.Size())
		}
	}
}

// Retention deletes whole days past the window — and nothing else in the
// directory, which is the part worth pinning.
func TestRetentionDeletesOldDaysAndNothingElse(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{
		"audit-2026-06-01.001.jsonl", "audit-2026-06-01.002.jsonl", "audit-2026-07-25.001.jsonl",
		"notes.txt", "audit-nonsense.jsonl", "audit-2026-13-99.jsonl", "sieve",
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("{}\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	lg, err := New(dir, 0, 7, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer lg.Close()
	lg.now = func() time.Time { return time.Date(2026, 7, 26, 12, 0, 0, 0, time.UTC) }
	lg.Write(Record{Tool: "email_move"})

	got := read(t, dir)
	want := []string{
		"audit-2026-07-25.001.jsonl", // inside the 7-day window
		"audit-2026-07-26.001.jsonl", // today's, just created
		"audit-2026-13-99.jsonl",     // not a real date: not ours, not touched
		"audit-nonsense.jsonl",       // not our naming: not ours, not touched
		"notes.txt", "sieve",
	}
	sort.Strings(want)
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("after retention:\n got %v\nwant %v", got, want)
	}
}

// Retention off means off: a compliance deployment that ships records
// elsewhere may legitimately want every day kept.
func TestRetentionZeroKeepsEverything(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "audit-2020-01-01.001.jsonl"), []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	lg, err := New(dir, 0, 0, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer lg.Close()
	lg.now = func() time.Time { return time.Date(2026, 7, 26, 12, 0, 0, 0, time.UTC) }
	lg.Write(Record{Tool: "email_move"})

	if names := read(t, dir); len(names) != 2 {
		t.Errorf("files = %v, want the ancient one kept", names)
	}
}

// An audit write that fails must not fail the mail operation the user asked
// for: the record describes the work, it is not part of it. But the failure
// has to be visible — a silently broken trail is worse than none.
func TestWriteFailureIsReportedOnceAndNeverPanics(t *testing.T) {
	dir := t.TempDir()
	var warnings []string
	lg, err := New(dir, 0, 30, false, func(f string, a ...any) { warnings = append(warnings, f) })
	if err != nil {
		t.Fatal(err)
	}
	// Make the directory unwritable by replacing it with a file: the next
	// open cannot succeed.
	os.RemoveAll(dir)
	if err := os.WriteFile(dir, []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 5; i++ {
		lg.Write(Record{Tool: "email_move"}) // must not panic
	}
	if len(warnings) != 1 {
		t.Errorf("warned %d times, want exactly one", len(warnings))
	}
	if lg.Enabled() {
		t.Error("logger still reports enabled after failing to write")
	}
	if !strings.Contains(strings.Join(warnings, " "), "auditing is OFF") {
		t.Errorf("warning does not say auditing stopped: %v", warnings)
	}
}

// Every call site holds a logger unconditionally; auditing being off must be
// a nil check, not a branch at each site.
func TestNilLoggerIsANoOp(t *testing.T) {
	var lg *Logger
	lg.Write(Record{Tool: "email_move"}) // must not panic
	lg.Close()
	if lg.Enabled() {
		t.Error("a nil logger reports enabled")
	}
	if lg.Path() != "" {
		t.Error("a nil logger reports a path")
	}
	if _, err := New("", 0, 30, false, nil); err == nil {
		t.Error("New(\"\") should refuse rather than silently discard")
	}
}

// Long refusal text is bounded, so one pathological message cannot turn the
// audit log into the thing it exists to keep small.
func TestErrorTextIsTruncated(t *testing.T) {
	dir := t.TempDir()
	lg, err := New(dir, 0, 30, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer lg.Close()
	lg.Write(Record{Tool: "email_move", Outcome: "error", Error: strings.Repeat("x", 5000)})

	recs := lines(t, lg.Path())
	if len(recs) != 1 {
		t.Fatal("no record")
	}
	if len(recs[0].Error) > maxErrorBytes+4 {
		t.Errorf("error text is %d bytes, want it bounded near %d", len(recs[0].Error), maxErrorBytes)
	}
	if !strings.HasSuffix(recs[0].Error, "…") {
		t.Error("truncation is not marked, so a reader cannot tell it happened")
	}
}

// Records from concurrent tool calls must each land as one intact line.
// Sequential() orders the mutating tools, but reads run concurrently.
func TestConcurrentWritesDoNotInterleave(t *testing.T) {
	dir := t.TempDir()
	lg, err := New(dir, 0, 30, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer lg.Close()

	done := make(chan struct{})
	for i := 0; i < 8; i++ {
		go func(n int) {
			defer func() { done <- struct{}{} }()
			for j := 0; j < 25; j++ {
				lg.Write(Record{Tool: "email_search", Account: "u1", Outcome: "ok",
					Detail: map[string]any{"worker": n, "seq": j}})
			}
		}(i)
	}
	for i := 0; i < 8; i++ {
		<-done
	}
	// lines() fails the test if any line is not independently valid JSON.
	if got := len(lines(t, lg.Path())); got != 200 {
		t.Errorf("got %d records, want 200", got)
	}
}

// --- compression ---

// Rotated files are gzipped; the one being appended to is not, so a collector
// can tail it. Nothing may be lost or become unreadable in the exchange.
func TestCompressesRotatedFilesAndLeavesTheCurrentOnePlain(t *testing.T) {
	dir := t.TempDir()
	lg, err := New(dir, 0, 30, true, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer lg.Close()

	day := time.Date(2026, 7, 26, 12, 0, 0, 0, time.UTC)
	lg.now = func() time.Time { return day }
	for i := 0; i < 20; i++ {
		lg.Write(Record{Time: "2026-07-26T12:00:00Z", Tool: "email_search", Account: "u1", Outcome: "ok"})
	}
	lg.now = func() time.Time { return day.AddDate(0, 0, 1) }
	lg.Write(Record{Time: "2026-07-27T12:00:00Z", Tool: "email_move", Account: "u1", Outcome: "ok"})

	names := read(t, dir)
	want := []string{"audit-2026-07-26.001.jsonl.gz", "audit-2026-07-27.001.jsonl"}
	if strings.Join(names, ",") != strings.Join(want, ",") {
		t.Fatalf("files = %v, want yesterday gzipped and today plain", names)
	}
	// The current file must stay appendable and readable as text.
	if lg.Path() != filepath.Join(dir, "audit-2026-07-27.001.jsonl") {
		t.Errorf("Path() = %q, want the plain current file", lg.Path())
	}
	// And the archive must still be every record, intact.
	f, err := os.Open(filepath.Join(dir, "audit-2026-07-26.001.jsonl.gz"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	zr, err := gzip.NewReader(f)
	if err != nil {
		t.Fatalf("archive is not readable gzip: %v", err)
	}
	body, err := io.ReadAll(zr)
	if err != nil {
		t.Fatal(err)
	}
	var n int
	for _, line := range strings.Split(strings.TrimSpace(string(body)), "\n") {
		var r Record
		if err := json.Unmarshal([]byte(line), &r); err != nil {
			t.Fatalf("archived line is not valid JSON: %q", line)
		}
		n++
	}
	if n != 20 {
		t.Errorf("archive holds %d records, want 20", n)
	}
}

// A sequence slot already archived must not be handed out again — otherwise a
// restart later the same day writes a second .001.jsonl beside the
// .001.jsonl.gz, and two files claim to be the same stretch of the record.
func TestCompressedFileHoldsItsSequenceSlot(t *testing.T) {
	dir := t.TempDir()
	day := time.Date(2026, 7, 26, 12, 0, 0, 0, time.UTC)

	first, err := New(dir, 300, 30, true, nil)
	if err != nil {
		t.Fatal(err)
	}
	first.now = func() time.Time { return day }
	for i := 0; i < 10; i++ {
		first.Write(Record{Time: "2026-07-26T12:00:00Z", Tool: "email_search"})
	}
	first.Close()

	// A fresh process, same day, after some files were archived.
	second, err := New(dir, 300, 30, true, nil)
	if err != nil {
		t.Fatal(err)
	}
	second.now = func() time.Time { return day }
	second.Write(Record{Time: "2026-07-26T12:00:00Z", Tool: "email_move"})
	defer second.Close()

	seen := map[string]bool{}
	for _, name := range read(t, dir) {
		slot := strings.TrimSuffix(strings.TrimSuffix(name, ".gz"), ".jsonl")
		if seen[slot] {
			t.Errorf("two files claim sequence slot %s: %v", slot, read(t, dir))
		}
		seen[slot] = true
	}
}

// Retention must recognise an archived day as that day, or compressed history
// would accumulate forever while plain files were pruned around it.
func TestRetentionPrunesCompressedDaysToo(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{
		"audit-2026-06-01.001.jsonl.gz", "audit-2026-06-02.001.jsonl",
		"audit-2026-07-25.001.jsonl.gz", "keepme.jsonl.gz",
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	lg, err := New(dir, 0, 7, false, nil) // compression off: only retention under test
	if err != nil {
		t.Fatal(err)
	}
	defer lg.Close()
	lg.now = func() time.Time { return time.Date(2026, 7, 26, 12, 0, 0, 0, time.UTC) }
	lg.Write(Record{Tool: "email_move"})

	want := []string{
		"audit-2026-07-25.001.jsonl.gz", // inside the window, still archived
		"audit-2026-07-26.001.jsonl",    // today's
		"keepme.jsonl.gz",               // not ours
	}
	if got := read(t, dir); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("after retention:\n got %v\nwant %v", got, want)
	}
}

// Compression is optional: a collector that cannot read .gz must be able to
// keep plain files.
func TestCompressionCanBeTurnedOff(t *testing.T) {
	dir := t.TempDir()
	lg, err := New(dir, 0, 30, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer lg.Close()
	day := time.Date(2026, 7, 26, 12, 0, 0, 0, time.UTC)
	lg.now = func() time.Time { return day }
	lg.Write(Record{Tool: "email_search"})
	lg.now = func() time.Time { return day.AddDate(0, 0, 1) }
	lg.Write(Record{Tool: "email_search"})

	for _, name := range read(t, dir) {
		if strings.HasSuffix(name, ".gz") {
			t.Errorf("compressed %s with compression off", name)
		}
	}
}

// A half-written archive must never stand in for a complete record: the
// rename is the commit point, so an interrupted compress leaves the original.
func TestInterruptedCompressLeavesTheOriginal(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "audit-2026-07-26.001.jsonl")
	if err := os.WriteFile(path, []byte(`{"tool":"email_move"}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// A leftover .tmp from a crashed run must not be mistaken for the archive.
	if err := os.WriteFile(path+".gz.tmp", []byte("truncated garbage"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := gzipFile(path); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Error("original still present after a successful compress")
	}
	f, err := os.Open(path + ".gz")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	zr, err := gzip.NewReader(f)
	if err != nil {
		t.Fatalf("archive is not valid gzip — the .tmp leftover was adopted: %v", err)
	}
	body, _ := io.ReadAll(zr)
	if !strings.Contains(string(body), "email_move") {
		t.Errorf("archive content = %q", body)
	}
}

// gzip is worth the complexity only if it actually shrinks these records.
func TestCompressionActuallyShrinksARealDay(t *testing.T) {
	dir := t.TempDir()
	lg, err := New(dir, 0, 30, true, nil)
	if err != nil {
		t.Fatal(err)
	}
	day := time.Date(2026, 7, 26, 12, 0, 0, 0, time.UTC)
	lg.now = func() time.Time { return day }
	// A heavy day: twenty waves of the measured 205-call session. The records
	// VARY — distinct timestamps, digests, handle ids and durations — because
	// identical lines would compress absurdly well and prove nothing about
	// real traffic.
	tools := []string{"email_search", "email_move", "email_get", "email_list_mailboxes"}
	for i := 0; i < 4000; i++ {
		lg.Write(Record{
			Time:    day.Add(time.Duration(i) * time.Second).Format(time.RFC3339),
			Session: "f35bb34caca0a141", Project: "proj-mail",
			Tool: tools[i%len(tools)], Authority: "external-mutation", Access: "read-organize",
			Account: "uc9a140ba", Outcome: "ok", Millis: int64(120 + i%900),
			Detail: map[string]any{
				"movedCount": i%200 + 1,
				"batch":      fmt.Sprintf("%06x", i*7919%0xffffff),
				"receipt":    fmt.Sprintf("rcp_%012x", i*104729%0xffffffffffff),
				"destination": map[string]any{
					"id": fmt.Sprintf("P%03d", i%37), "name": "Archive", "role": "archive",
				},
			},
		})
	}
	plainSize := func() int64 {
		fi, err := os.Stat(lg.Path())
		if err != nil {
			t.Fatal(err)
		}
		return fi.Size()
	}()
	lg.now = func() time.Time { return day.AddDate(0, 0, 1) }
	lg.Write(Record{Tool: "email_search"}) // rolls, which compresses the day
	lg.Close()

	fi, err := os.Stat(filepath.Join(dir, "audit-2026-07-26.001.jsonl.gz"))
	if err != nil {
		t.Fatal(err)
	}
	ratio := float64(fi.Size()) / float64(plainSize)
	if ratio > 0.2 {
		t.Errorf("compressed to %.0f%% of %d bytes; expected roughly a tenth", ratio*100, plainSize)
	}
	t.Logf("4,000 records: %d bytes plain, %d gzipped (%.1f%%)", plainSize, fi.Size(), ratio*100)
}
