// Package sievestore is an append-only, versioned document store for sieve
// content, rooted in the extension's data directory. Every mutation appends a
// new version; restore appends a copy of an old version (nothing is ever
// discarded by a restore); an "applied" pointer records the version last
// confirmed live at the provider. No git, no network, no external tooling —
// pure logic over one directory tree (see docs/sieve-workspace-design.md).
//
// Layout: <root>/<provider-host>/<account-id>/<doc-name>/
//
//	versions/v000001.sieve + v000001.json   (immutable once written)
//	head                                    (current version number)
//	applied                                 (last confirmed version number)
package sievestore

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// DefaultRetention is how many versions each document keeps (oldest pruned;
// the applied version is never pruned). If size ever matters the agreed path
// is compressing aged versions, not lowering this.
const DefaultRetention = 50

// MaxContentBytes bounds one document version; sieve scripts are small and
// tool results share the wire's frame budget.
const MaxContentBytes = 256 * 1024

// putOrigins are the origins external callers may record; "restore" is
// reserved for Restore itself.
var putOrigins = map[string]bool{"paste-in": true, "edit": true, "pull": true}

// keyPartRe constrains every key part. Note macOS's default case-insensitive
// filesystem: "Area" and "area" share a doc dir there — harmless (shared
// history, both readable) but worth knowing; names from calibration should
// pick one casing and stick to it.
var keyPartRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._@-]{0,99}$`)

// DocKey identifies one document: provider host + account id + name.
type DocKey struct {
	Provider string `json:"provider"`
	Account  string `json:"account"`
	Name     string `json:"name"`
}

func (k DocKey) validate() error {
	for field, part := range map[string]string{"provider": k.Provider, "account": k.Account, "name": k.Name} {
		if !keyPartRe.MatchString(part) {
			return fmt.Errorf("invalid %s %q: use letters, digits, and . _ @ - (max 100 chars)", field, part)
		}
	}
	return nil
}

// Meta describes one immutable version.
type Meta struct {
	Version      int       `json:"version"`
	SavedAt      time.Time `json:"savedAt"`
	Origin       string    `json:"origin"` // paste-in | edit | pull | restore
	Note         string    `json:"note,omitempty"`
	RestoredFrom int       `json:"restoredFrom,omitempty"`
	Bytes        int       `json:"bytes"`
	Hash         string    `json:"hash"`
}

// DocInfo summarizes one document for listings. A document whose state can't
// be read (corrupt pointer, unreadable version) still appears, with Error set
// and the readable fields zeroed — broken docs must be visible, not hidden.
type DocInfo struct {
	DocKey
	Head        int    `json:"head"`
	Applied     int    `json:"applied,omitempty"`
	Versions    int    `json:"versions"`
	Oldest      int    `json:"oldestVersion"`
	Pending     bool   `json:"pendingChanges"` // head content differs from applied content
	ContextOnly bool   `json:"contextOnly,omitempty"`
	Archived    bool   `json:"archived,omitempty"`
	Error       string `json:"error,omitempty"`
	HeadMeta    Meta   `json:"headMeta"`
}

// archiveDir is the per-account subdirectory that holds archived documents.
// It starts with a dot, so it can never collide with a valid DocKey name.
const archiveDir = ".archive"

// contextOnlyMarker is the doc-dir marker file for reference-material
// documents (full provider exports, generated blocks): they hold context,
// are never edited for emission, and MarkApplied refuses them.
const contextOnlyMarker = "context-only"

// Store is safe for concurrent use; one instance per root.
type Store struct {
	root      string
	Retention int

	mu sync.Mutex
}

// New opens (lazily creating) a store rooted at dir.
func New(dir string) *Store {
	return &Store{root: dir, Retention: DefaultRetention}
}

func (s *Store) docDir(k DocKey) string {
	return filepath.Join(s.root, k.Provider, k.Account, k.Name)
}

func versionBase(v int) string { return fmt.Sprintf("v%06d", v) }

func contentHash(content string) string {
	sum := sha256.Sum256([]byte(content))
	return hex.EncodeToString(sum[:8])
}

// Put appends a new version, unless content is byte-identical to the current
// head — then it returns the head's meta with appended=false (a paste-in that
// matches local state is a no-op, not noise).
func (s *Store) Put(k DocKey, content, origin, note string) (Meta, bool, error) {
	if err := k.validate(); err != nil {
		return Meta{}, false, err
	}
	if !putOrigins[origin] {
		return Meta{}, false, fmt.Errorf("invalid origin %q: use paste-in or edit", origin)
	}
	if len(content) > MaxContentBytes {
		return Meta{}, false, fmt.Errorf("content is %d bytes; the per-version limit is %d", len(content), MaxContentBytes)
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	head, err := s.readPointer(k, "head")
	if err != nil {
		return Meta{}, false, err
	}
	if head > 0 {
		current, meta, err := s.readVersion(k, head)
		if err != nil {
			return Meta{}, false, err
		}
		if current == content {
			return meta, false, nil
		}
	} else if _, err := os.Stat(filepath.Join(s.root, k.Provider, k.Account, archiveDir, k.Name)); err == nil {
		// Creating a live doc over an archived namesake would wedge both
		// (unarchive and archive would each hit a collision).
		return Meta{}, false, fmt.Errorf("an archived document named %q exists — unarchive it first (email_sieve_archive unarchive:true) or choose another name", k.Name)
	}
	meta, err := s.appendLocked(k, head, content, origin, note, 0)
	return meta, err == nil, err
}

// Restore appends a new head whose content is a copy of an earlier version.
// The prior head and the restored-from version both remain in history.
func (s *Store) Restore(k DocKey, version int, note string) (Meta, error) {
	if err := k.validate(); err != nil {
		return Meta{}, err
	}
	if version <= 0 {
		return Meta{}, fmt.Errorf("version is required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	head, err := s.readPointer(k, "head")
	if err != nil {
		return Meta{}, err
	}
	if version == head {
		return Meta{}, fmt.Errorf("v%d is already the head — nothing to restore", version)
	}
	content, _, err := s.readVersion(k, version)
	if err != nil {
		return Meta{}, err
	}
	if note == "" {
		note = fmt.Sprintf("restored from v%d", version)
	}
	return s.appendLocked(k, head, content, "restore", note, version)
}

// appendLocked writes the next version and advances the head pointer.
// Version files are immutable once written: creation is O_EXCL and the
// number is taken past anything already on disk, so neither a crash-orphaned
// version (content written, head never advanced) nor a concurrent writer in
// another process is ever overwritten — worst case is an unreferenced
// version left in history, never lost content.
func (s *Store) appendLocked(k DocKey, head int, content, origin, note string, restoredFrom int) (Meta, error) {
	dir := filepath.Join(s.docDir(k), "versions")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return Meta{}, err
	}
	v := head
	if onDisk := maxVersionOnDisk(dir); onDisk > v {
		v = onDisk
	}
	var f *os.File
	for {
		v++
		var err error
		f, err = os.OpenFile(filepath.Join(dir, versionBase(v)+".sieve"), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
		if err == nil {
			break
		}
		if !os.IsExist(err) {
			return Meta{}, err
		}
		// Another process claimed this number between our scan and open.
	}
	if _, err := f.WriteString(content); err != nil {
		f.Close()
		return Meta{}, err
	}
	if err := f.Close(); err != nil {
		return Meta{}, err
	}
	meta := Meta{
		Version:      v,
		SavedAt:      time.Now().UTC().Truncate(time.Second),
		Origin:       origin,
		Note:         note,
		RestoredFrom: restoredFrom,
		Bytes:        len(content),
		Hash:         contentHash(content),
	}
	metaJSON, err := json.MarshalIndent(meta, "", " ")
	if err != nil {
		return Meta{}, err
	}
	if err := os.WriteFile(filepath.Join(dir, versionBase(v)+".json"), metaJSON, 0o644); err != nil {
		return Meta{}, err
	}
	if err := s.writePointer(k, "head", v); err != nil {
		return Meta{}, err
	}
	// A corrupt applied pointer must disable pruning, not read as "nothing
	// applied" — the confirmed-live version is the one prune may never take.
	if applied, err := s.readPointer(k, "applied"); err == nil {
		s.pruneLocked(k, v, applied)
	}
	return meta, nil
}

// maxVersionOnDisk scans a versions dir for the highest committed number.
func maxVersionOnDisk(dir string) int {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0
	}
	maxV := 0
	for _, e := range entries {
		if v, ok := parseVersionFile(e.Name()); ok && v > maxV {
			maxV = v
		}
	}
	return maxV
}

// pruneLocked removes versions older than the retention window, never
// touching the applied version.
func (s *Store) pruneLocked(k DocKey, head, applied int) {
	if s.Retention <= 0 {
		return // defensive: a zero window must keep everything, not prune head
	}
	cutoff := head - s.Retention
	if cutoff <= 0 {
		return
	}
	dir := filepath.Join(s.docDir(k), "versions")
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		v, ok := parseVersionFile(e.Name())
		if !ok || v > cutoff || v == applied {
			continue
		}
		os.Remove(filepath.Join(dir, e.Name()))
		os.Remove(filepath.Join(dir, versionBase(v)+".json"))
	}
}

func parseVersionFile(name string) (int, bool) {
	if !strings.HasSuffix(name, ".sieve") {
		return 0, false
	}
	v, err := strconv.Atoi(strings.TrimPrefix(strings.TrimSuffix(name, ".sieve"), "v"))
	if err != nil || v <= 0 {
		return 0, false
	}
	return v, true
}

// Get returns a version's content and meta; version 0 means head.
func (s *Store) Get(k DocKey, version int) (string, Meta, error) {
	if err := k.validate(); err != nil {
		return "", Meta{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	v, err := s.resolveVersion(k, version)
	if err != nil {
		return "", Meta{}, err
	}
	return s.readVersion(k, v)
}

// MarkApplied records that a version (0 = head) is now live at the provider.
// It refuses context-only documents always, and truncation-suspect content
// unless force overrides (a placeholder must never become the baseline).
func (s *Store) MarkApplied(k DocKey, version int, force bool) (Meta, error) {
	if err := k.validate(); err != nil {
		return Meta{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.contextOnlyLocked(k) {
		return Meta{}, fmt.Errorf("%s is a context-only document (reference material) — it is never pasted to the provider, so it cannot be marked applied", k.Name)
	}
	v, err := s.resolveVersion(k, version)
	if err != nil {
		return Meta{}, err
	}
	content, meta, err := s.readVersion(k, v)
	if err != nil {
		return Meta{}, err
	}
	if !force && (SuspectsTruncation(content) || SuspectsTruncation(meta.Note)) {
		return Meta{}, fmt.Errorf("v%d of %s looks like a truncated placeholder (marker in its content or note) — marking it applied would make a partial script the baseline; pass force:true only if it truly is what's live at the provider", v, k.Name)
	}
	if err := s.writePointer(k, "applied", v); err != nil {
		return Meta{}, err
	}
	return meta, nil
}

// SetContextOnly marks (or unmarks) a document as reference material.
func (s *Store) SetContextOnly(k DocKey, on bool) error {
	if err := k.validate(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if head, err := s.readPointer(k, "head"); err != nil {
		return err
	} else if head == 0 {
		return s.noDocErrorLocked(k)
	}
	marker := filepath.Join(s.docDir(k), contextOnlyMarker)
	if on {
		return os.WriteFile(marker, []byte("reference material — never mark applied\n"), 0o644)
	}
	err := os.Remove(marker)
	if os.IsNotExist(err) {
		return nil
	}
	return err
}

func (s *Store) contextOnlyLocked(k DocKey) bool {
	_, err := os.Stat(filepath.Join(s.docDir(k), contextOnlyMarker))
	return err == nil
}

// Archive moves a document out of the working set into the account's
// .archive/ subtree. Nothing is deleted; listings hide it unless asked.
func (s *Store) Archive(k DocKey) error {
	if err := k.validate(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	src := s.docDir(k)
	if _, err := os.Stat(src); os.IsNotExist(err) {
		return s.noDocErrorLocked(k)
	}
	dir := filepath.Join(s.root, k.Provider, k.Account, archiveDir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	dst := filepath.Join(dir, k.Name)
	if _, err := os.Stat(dst); err == nil {
		return fmt.Errorf("an archived document named %q already exists — unarchive it first, or put the live content under a new name and archive that", k.Name)
	}
	return os.Rename(src, dst)
}

// Unarchive moves a document back into the working set.
func (s *Store) Unarchive(k DocKey) error {
	if err := k.validate(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	src := filepath.Join(s.root, k.Provider, k.Account, archiveDir, k.Name)
	if _, err := os.Stat(src); os.IsNotExist(err) {
		archived, _ := readDirNames(filepath.Join(s.root, k.Provider, k.Account, archiveDir))
		if len(archived) == 0 {
			return fmt.Errorf("no archived document %q — the archive for %s/%s is empty", k.Name, k.Provider, k.Account)
		}
		return fmt.Errorf("no archived document %q — archived: %s", k.Name, strings.Join(archived, ", "))
	}
	dst := s.docDir(k)
	if _, err := os.Stat(dst); err == nil {
		return fmt.Errorf("a live document named %q already exists — archive the live one first if you mean to swap them", k.Name)
	}
	return os.Rename(src, dst)
}

// Diff renders a unified diff between two versions (0 = head for either).
func (s *Store) Diff(k DocKey, from, to int) (string, error) {
	if err := k.validate(); err != nil {
		return "", err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	fromV, err := s.resolveVersion(k, from)
	if err != nil {
		return "", err
	}
	toV, err := s.resolveVersion(k, to)
	if err != nil {
		return "", err
	}
	a, _, err := s.readVersion(k, fromV)
	if err != nil {
		return "", err
	}
	b, _, err := s.readVersion(k, toV)
	if err != nil {
		return "", err
	}
	return Unified(
		fmt.Sprintf("%s@v%d", k.Name, fromV),
		fmt.Sprintf("%s@v%d", k.Name, toV),
		a, b,
	), nil
}

// Info summarizes one document.
func (s *Store) Info(k DocKey) (DocInfo, error) {
	if err := k.validate(); err != nil {
		return DocInfo{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.infoAtLocked(k, s.docDir(k), false)
}

// List summarizes every document in the store, sorted by key. Archived
// documents are hidden unless includeArchived is set.
func (s *Store) List(includeArchived bool) ([]DocInfo, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []DocInfo
	providers, err := readDirNames(s.root)
	if err != nil {
		return nil, err
	}
	for _, p := range providers {
		accounts, err := readDirNames(filepath.Join(s.root, p))
		if err != nil {
			return nil, err
		}
		for _, a := range accounts {
			acctDir := filepath.Join(s.root, p, a)
			docs, err := readDirNames(acctDir)
			if err != nil {
				return nil, err
			}
			for _, d := range docs {
				k := DocKey{Provider: p, Account: a, Name: d}
				info, err := s.infoAtLocked(k, s.docDir(k), false)
				if err != nil {
					// A malformed doc must not break the listing — but hiding
					// it would make corruption invisible; surface it instead.
					info = DocInfo{DocKey: k, Error: err.Error()}
				}
				out = append(out, info)
			}
			if !includeArchived {
				continue
			}
			archived, _ := readDirNames(filepath.Join(acctDir, archiveDir))
			for _, d := range archived {
				k := DocKey{Provider: p, Account: a, Name: d}
				info, err := s.infoAtLocked(k, filepath.Join(acctDir, archiveDir, d), true)
				if err != nil {
					info = DocInfo{DocKey: k, Archived: true, Error: err.Error()}
				}
				out = append(out, info)
			}
		}
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.Provider != b.Provider {
			return a.Provider < b.Provider
		}
		if a.Account != b.Account {
			return a.Account < b.Account
		}
		if a.Name != b.Name {
			return a.Name < b.Name
		}
		return !a.Archived // live before archived on a name tie
	})
	return out, nil
}

// Accounts lists account ids that have documents under a provider.
func (s *Store) Accounts(provider string) ([]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return readDirNames(filepath.Join(s.root, provider))
}

// readDirNames lists subdirectory names, skipping dot-directories (the
// .archive subtree, editor droppings); a missing directory is an empty
// store, not an error.
func readDirNames(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var names []string
	for _, e := range entries {
		if e.IsDir() && !strings.HasPrefix(e.Name(), ".") {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	return names, nil
}

// noDocErrorLocked explains a missing document usefully: names come from
// calibration, so a miss lists what actually exists (the field report's
// agent guessed a skill example name and got no guidance).
func (s *Store) noDocErrorLocked(k DocKey) error {
	acctDir := filepath.Join(s.root, k.Provider, k.Account)
	archived, _ := readDirNames(filepath.Join(acctDir, archiveDir))
	for _, name := range archived {
		if name == k.Name {
			return fmt.Errorf("document %q is archived — email_sieve_archive with unarchive:true to bring it back", k.Name)
		}
	}
	docs, _ := readDirNames(acctDir)
	if len(docs) == 0 {
		return fmt.Errorf("no documents stored yet for %s/%s — put a version first (names come from first-use calibration)", k.Provider, k.Account)
	}
	return fmt.Errorf("no document %q for %s/%s — existing documents: %s", k.Name, k.Provider, k.Account, strings.Join(docs, ", "))
}

func (s *Store) infoAtLocked(k DocKey, dir string, archived bool) (DocInfo, error) {
	head, err := readPointerAt(dir, "head", k.Name)
	if err != nil {
		return DocInfo{}, err
	}
	if head == 0 {
		return DocInfo{}, s.noDocErrorLocked(k)
	}
	applied, _ := readPointerAt(dir, "applied", k.Name)
	_, headMeta, err := readVersionAt(dir, k.Name, head)
	if err != nil {
		return DocInfo{}, err
	}

	entries, _ := os.ReadDir(filepath.Join(dir, "versions"))
	count, oldest := 0, head
	for _, e := range entries {
		if v, ok := parseVersionFile(e.Name()); ok {
			count++
			if v < oldest {
				oldest = v
			}
		}
	}

	pending := true
	if applied > 0 {
		if _, appliedMeta, err := readVersionAt(dir, k.Name, applied); err == nil {
			pending = appliedMeta.Hash != headMeta.Hash
		}
	}
	_, ctxErr := os.Stat(filepath.Join(dir, contextOnlyMarker))
	return DocInfo{
		DocKey:      k,
		Head:        head,
		Applied:     applied,
		Versions:    count,
		Oldest:      oldest,
		Pending:     pending,
		ContextOnly: ctxErr == nil,
		Archived:    archived,
		HeadMeta:    headMeta,
	}, nil
}

func (s *Store) resolveVersion(k DocKey, version int) (int, error) {
	head, err := s.readPointer(k, "head")
	if err != nil {
		return 0, err
	}
	if head == 0 {
		return 0, s.noDocErrorLocked(k)
	}
	if version == 0 {
		return head, nil
	}
	return version, nil
}

func (s *Store) readVersion(k DocKey, v int) (string, Meta, error) {
	return readVersionAt(s.docDir(k), k.Name, v)
}

func readVersionAt(docDir, name string, v int) (string, Meta, error) {
	dir := filepath.Join(docDir, "versions")
	content, err := os.ReadFile(filepath.Join(dir, versionBase(v)+".sieve"))
	if err != nil {
		return "", Meta{}, fmt.Errorf("version v%d of %s not found — it never existed or was pruned (email_sieve_list shows head and oldestVersion)", v, name)
	}
	var meta Meta
	if raw, err := os.ReadFile(filepath.Join(dir, versionBase(v)+".json")); err == nil {
		json.Unmarshal(raw, &meta)
	}
	if meta.Version == 0 {
		meta = Meta{Version: v, Bytes: len(content), Hash: contentHash(string(content))}
	}
	return string(content), meta, nil
}

func (s *Store) readPointer(k DocKey, name string) (int, error) {
	return readPointerAt(s.docDir(k), name, k.Name)
}

func readPointerAt(docDir, pointer, docName string) (int, error) {
	raw, err := os.ReadFile(filepath.Join(docDir, pointer))
	if os.IsNotExist(err) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	v, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil || v < 0 {
		return 0, fmt.Errorf("corrupt %s pointer for %s", pointer, docName)
	}
	return v, nil
}

// writePointer commits a pointer atomically (temp file + rename): a crash
// mid-update leaves the previous pointer intact, never a torn/empty file —
// pointer files are the store's only mutable state, so this is what makes
// the whole layout crash-consistent.
func (s *Store) writePointer(k DocKey, name string, v int) error {
	dir := s.docDir(k)
	tmp, err := os.CreateTemp(dir, "."+name+".tmp-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name()) // no-op after a successful rename
	if _, err := tmp.WriteString(strconv.Itoa(v) + "\n"); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), filepath.Join(dir, name))
}
