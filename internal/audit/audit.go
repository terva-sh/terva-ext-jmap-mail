// Package audit writes an append-only, machine-readable record of what the
// extension did with the user's mail.
//
// It exists because this extension reads and mutates a mailbox on the model's
// initiative: the transcript shows what the model *said* it did, and only a
// log the model cannot write shows what actually happened. The record is for
// three readers — an operator debugging a run, a reviewer monitoring
// behaviour across runs, and whatever compliance process a sensitive
// deployment answers to.
//
// # What is deliberately not in here
//
// No message subjects, senders, recipients, previews, or bodies. Ever. The
// mailbox is full of third-party content that nobody consented to have copied
// into a durable file, and an audit trail is the worst possible place to put
// it: long-lived, replicated by backups, read by more people than the
// transcript, and outside whatever retention the mail itself has. The record
// answers "which messages, by id, moved where, when, on whose authority" —
// which is what an audit is for — and stops there.
//
// That restraint is enforced structurally rather than by care: Record.Detail
// is populated by an ALLOW-LIST of known-safe result fields (see
// mail.AuditDetail). A field added to a tool result later is invisible to the
// log until someone admits it deliberately. A redaction pass would have the
// opposite default, and would have leaked the sender names and subject lines
// that email_get used to return before v0.14.0 projected them away.
//
// # Integrity, honestly stated
//
// This is a local append-only file owned by the same process it describes. It
// is evidence of what the extension believes it did, not tamper-proof
// evidence: anything that can write the data dir can rewrite it. A deployment
// that needs more should ship these records off-host — the format is JSON
// Lines precisely so a collector can tail the file without parsing help.
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
	"sync"
	"time"
)

// Record is one audited operation, rendered as a single JSON Lines entry.
type Record struct {
	// Time is when the operation STARTED, RFC 3339 in UTC. Records are
	// appended on completion, so a slow call can land after a fast one that
	// began later — sort by this field, not by file order, to see the order
	// things were initiated in. Local time in an audit log is a trap: the
	// reader is rarely in the writer's zone, and DST makes one hour a year
	// ambiguous.
	Time string `json:"ts"`
	// Session and Project correlate a record with the terva session that
	// caused it, so a run can be reconstructed across tools.
	Session string `json:"session,omitempty"`
	Project string `json:"project,omitempty"`
	Tool    string `json:"tool"`
	// Authority is the tool's declared authority (network-read,
	// external-mutation, local-read, local-data) — the honest statement of
	// what class of thing this call was permitted to do.
	Authority string `json:"authority"`
	// Access is the access_level in force, so a record shows the gate that
	// allowed it and not merely that it happened.
	Access  string `json:"access,omitempty"`
	Account string `json:"account,omitempty"`
	Outcome string `json:"outcome"` // ok | error
	Millis  int64  `json:"ms"`
	// Detail is the per-tool projection: counts, mailbox ids and names,
	// handle ids, and — for email_destroy alone — the ids destroyed.
	Detail map[string]any `json:"detail,omitempty"`
	// Error is the refusal or failure text, truncated. These are the
	// extension's own messages and the caller's own arguments echoed back,
	// never message content; the truncation bounds a pathological one.
	Error string `json:"error,omitempty"`
}

// maxErrorBytes bounds the error text a record carries. Long enough for the
// confirm-phrase refusals, short enough that a runaway message cannot turn
// the log into the thing it was meant to keep small.
const maxErrorBytes = 400

// Logger appends records to a rotating file. The zero value is a no-op
// logger, so every call site can hold one unconditionally and auditing being
// switched off costs a nil check rather than a branch at each site.
type Logger struct {
	mu       sync.Mutex
	dir      string
	maxSize  int64
	retain   int  // days of history to keep; 0 keeps everything
	compress bool // gzip files once they stop being appended to

	file *os.File
	day  string
	size int64

	// warn reports a write failure exactly once per logger, to the host log.
	// Auditing must never take the extension down or spam a run, but silence
	// about a broken audit trail is its own hazard.
	warn     func(format string, args ...any)
	warned   bool
	now      func() time.Time
	disabled bool
}

// New opens a logger writing under dir/. A nil Logger, or one built by New
// with an empty dir, silently discards — callers do not branch.
func New(dir string, maxSize int64, retainDays int, compress bool, warn func(string, ...any)) (*Logger, error) {
	if strings.TrimSpace(dir) == "" {
		return nil, fmt.Errorf("audit: no directory")
	}
	// 0700: the record names mailboxes and message ids belonging to one user,
	// on a machine that may have several.
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("audit: create %s: %w", dir, err)
	}
	if maxSize <= 0 {
		maxSize = defaultMaxSize
	}
	return &Logger{
		dir: dir, maxSize: maxSize, retain: retainDays, compress: compress,
		warn: warn, now: time.Now,
	}, nil
}

// defaultMaxSize bounds one file before a same-day roll. Records run 250–400
// bytes, so this is roughly 25,000 of them — about a hundred heavy sessions,
// since the busiest measured wave (4,000 messages, 205 tool calls) produces
// well under 100 KB. It is a backstop against a runaway loop rather than a
// routine event: normal use rolls on the date, not on size, and a file this
// size still opens in an editor.
const defaultMaxSize = 8 << 20 // 8 MiB

// Write appends one record. It never returns an error: an audit write that
// fails must not fail the mail operation the user asked for — the record is a
// description of the work, not part of it. The failure goes to the host log
// once, so a broken trail is noticed without a run being flooded.
func (l *Logger) Write(r Record) {
	if l == nil || l.disabled {
		return
	}
	if len(r.Error) > maxErrorBytes {
		r.Error = r.Error[:maxErrorBytes] + "…"
	}
	line, err := json.Marshal(r)
	if err != nil {
		l.reportOnce("audit: encode record: %v", err)
		return
	}
	line = append(line, '\n')

	l.mu.Lock()
	defer l.mu.Unlock()
	if err := l.openLocked(int64(len(line))); err != nil {
		l.reportOnce("audit: %v — auditing is OFF for the rest of this run", err)
		l.disabled = true
		return
	}
	n, err := l.file.Write(line)
	l.size += int64(n)
	if err != nil {
		l.reportOnce("audit: write: %v — auditing is OFF for the rest of this run", err)
		l.disabled = true
	}
}

// Close releases the current file. Safe on a nil logger.
func (l *Logger) Close() {
	if l == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.file != nil {
		_ = l.file.Close()
		l.file = nil
	}
}

// Path reports the file currently being appended to, for email_status to
// show. An audit trail nobody can confirm is running is not worth much.
func (l *Logger) Path() string {
	if l == nil {
		return ""
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.file == nil {
		return ""
	}
	return l.file.Name()
}

// Enabled reports whether records are actually being written — false once a
// write failure has switched the logger off, so status can say so.
func (l *Logger) Enabled() bool { return l != nil && !l.disabled }

// openLocked ensures an open file with room for the next line, rolling on the
// date first and on size second.
func (l *Logger) openLocked(next int64) error {
	day := l.now().UTC().Format("2006-01-02")
	if l.file != nil && l.day == day && l.size+next <= l.maxSize {
		return nil
	}
	if l.file != nil {
		_ = l.file.Close()
		l.file = nil
	}
	if l.day != day {
		l.day = day
		l.prune()
	}
	name, size, err := l.nextFile(day, next)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(name, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("open %s: %w", name, err)
	}
	l.file, l.size = f, size
	// Everything except the file now being appended to is finished, so it can
	// be compressed. Running the sweep here rather than on a timer means it
	// only finds work just after a roll, and a process that never rolls never
	// scans. JSON Lines with a fixed key set is highly repetitive: gzip takes
	// a day of records to roughly a tenth.
	l.compressClosed(name)
	return nil
}

// compressClosed gzips every finished audit file, leaving `current` alone.
// Failure is not reported: a file that stays uncompressed is still a complete,
// readable record, and an audit trail must not become noisy about its own
// housekeeping. The original is removed only after the .gz is safely in place.
func (l *Logger) compressClosed(current string) {
	if !l.compress {
		return
	}
	entries, err := os.ReadDir(l.dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".jsonl") {
			continue
		}
		if _, ok := fileDay(e.Name()); !ok {
			continue // not ours; never touch it
		}
		path := filepath.Join(l.dir, e.Name())
		if path == current {
			continue
		}
		_ = gzipFile(path)
	}
}

// gzipFile writes path.gz and removes path. It goes via a temp file so an
// interrupted run can never leave a truncated .gz standing in for a record
// that was complete — the rename is the commit point.
func gzipFile(path string) error {
	final := path + ".gz"
	if _, err := os.Stat(final); err == nil {
		// A previous attempt got as far as the rename; finish the job.
		return os.Remove(path)
	}
	src, err := os.Open(path)
	if err != nil {
		return err
	}
	defer src.Close()

	tmp, err := os.OpenFile(final+".tmp", os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	zw := gzip.NewWriter(tmp)
	_, copyErr := io.Copy(zw, src)
	closeErr := zw.Close()
	syncErr := tmp.Sync()
	tmp.Close()
	if copyErr != nil || closeErr != nil || syncErr != nil {
		os.Remove(final + ".tmp")
		return fmt.Errorf("compress %s: %v %v %v", path, copyErr, closeErr, syncErr)
	}
	if err := os.Rename(final+".tmp", final); err != nil {
		os.Remove(final + ".tmp")
		return err
	}
	return os.Remove(path)
}

// nextFile picks today's file, advancing past any that is already full.
//
// The sequence number is always present and zero-padded, even for the first
// file of a day, so that sorting the filenames sorts the records: an
// unsuffixed "audit-2026-07-26.jsonl" would sort AFTER its own continuation
// "audit-2026-07-26.2.jsonl", because '.' precedes 'j'. Reading an audit log
// in the order it was written should not require knowing that.
func (l *Logger) nextFile(day string, next int64) (string, int64, error) {
	for seq := 1; seq < 1000; seq++ {
		name := filepath.Join(l.dir, fmt.Sprintf("audit-%s.%03d.jsonl", day, seq))
		// A compressed file holds that sequence slot. Without this check a
		// restart later the same day would create a second .001.jsonl beside
		// the .001.jsonl.gz it had already archived, and two files would claim
		// to be the same stretch of the record.
		if _, err := os.Stat(name + ".gz"); err == nil {
			continue
		}
		info, err := os.Stat(name)
		switch {
		case os.IsNotExist(err):
			return name, 0, nil
		case err != nil:
			return "", 0, fmt.Errorf("stat %s: %w", name, err)
		case info.Size()+next <= l.maxSize:
			return name, info.Size(), nil
		}
	}
	return "", 0, fmt.Errorf("too many audit files for %s", day)
}

// prune deletes whole days older than the retention window. It runs on a date
// roll rather than on a timer, so a process that never rolls never scans, and
// it only ever removes files it recognises by name.
func (l *Logger) prune() {
	if l.retain <= 0 {
		return
	}
	entries, err := os.ReadDir(l.dir)
	if err != nil {
		return
	}
	cutoff := l.now().UTC().AddDate(0, 0, -l.retain).Format("2006-01-02")
	var stale []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		day, ok := fileDay(e.Name())
		if ok && day < cutoff {
			stale = append(stale, e.Name())
		}
	}
	sort.Strings(stale)
	for _, name := range stale {
		_ = os.Remove(filepath.Join(l.dir, name))
	}
}

// fileDay extracts the date from an audit filename, and reports false for
// anything this package did not write — retention must never delete a file it
// does not recognise.
func fileDay(name string) (string, bool) {
	name = strings.TrimSuffix(name, ".gz") // a compressed day is still that day
	if !strings.HasPrefix(name, "audit-") || !strings.HasSuffix(name, ".jsonl") {
		return "", false
	}
	rest := strings.TrimSuffix(strings.TrimPrefix(name, "audit-"), ".jsonl")
	if len(rest) < 10 {
		return "", false
	}
	day := rest[:10]
	if _, err := time.Parse("2006-01-02", day); err != nil {
		return "", false
	}
	// Either "YYYY-MM-DD" or "YYYY-MM-DD.<seq>".
	if tail := rest[10:]; tail != "" {
		if !strings.HasPrefix(tail, ".") {
			return "", false
		}
		for _, r := range tail[1:] {
			if r < '0' || r > '9' {
				return "", false
			}
		}
	}
	return day, true
}

func (l *Logger) reportOnce(format string, args ...any) {
	if l.warned || l.warn == nil {
		return
	}
	l.warned = true
	l.warn(format, args...)
}
