package sievestore

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

var testKey = DocKey{Provider: "api.fastmail.com", Account: "uc9a140ba", Name: "area-2-below-spam"}

func newTestStore(t *testing.T) *Store {
	t.Helper()
	return New(t.TempDir())
}

func mustPut(t *testing.T, s *Store, k DocKey, content, origin, note string) Meta {
	t.Helper()
	meta, appended, err := s.Put(k, content, origin, note)
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	if !appended {
		t.Fatalf("Put %q did not append", note)
	}
	return meta
}

func TestPutAppendsVersions(t *testing.T) {
	s := newTestStore(t)
	m1 := mustPut(t, s, testKey, "keep;\n", "paste-in", "initial import")
	m2 := mustPut(t, s, testKey, "keep;\nfileinto \"Receipts\";\n", "edit", "file receipts")
	if m1.Version != 1 || m2.Version != 2 {
		t.Fatalf("versions = %d, %d", m1.Version, m2.Version)
	}
	if m2.Origin != "edit" || m2.Note != "file receipts" || m2.Bytes == 0 || m2.Hash == "" {
		t.Errorf("meta = %+v", m2)
	}

	content, meta, err := s.Get(testKey, 0) // head
	if err != nil || meta.Version != 2 || !strings.Contains(content, "Receipts") {
		t.Errorf("head get = v%d %q err=%v", meta.Version, content, err)
	}
	content, _, err = s.Get(testKey, 1)
	if err != nil || content != "keep;\n" {
		t.Errorf("v1 get = %q err=%v", content, err)
	}
}

func TestPutUnchangedDoesNotAppend(t *testing.T) {
	s := newTestStore(t)
	mustPut(t, s, testKey, "keep;\n", "paste-in", "")
	meta, appended, err := s.Put(testKey, "keep;\n", "paste-in", "same again")
	if err != nil {
		t.Fatal(err)
	}
	if appended || meta.Version != 1 {
		t.Errorf("unchanged put: appended=%v version=%d, want no-op at v1", appended, meta.Version)
	}
}

func TestPutValidation(t *testing.T) {
	s := newTestStore(t)
	if _, _, err := s.Put(DocKey{Provider: "p", Account: "a", Name: "../escape"}, "x", "edit", ""); err == nil {
		t.Error("traversal-ish name accepted")
	}
	if _, _, err := s.Put(DocKey{Provider: "p/q", Account: "a", Name: "n"}, "x", "edit", ""); err == nil {
		t.Error("slash in provider accepted")
	}
	if _, _, err := s.Put(testKey, "x", "restore", ""); err == nil {
		t.Error("caller-supplied restore origin accepted")
	}
	if _, _, err := s.Put(testKey, strings.Repeat("x", MaxContentBytes+1), "edit", ""); err == nil {
		t.Error("oversized content accepted")
	}
}

func TestRestoreAppendsCopy(t *testing.T) {
	s := newTestStore(t)
	mustPut(t, s, testKey, "v1 content\n", "paste-in", "")
	mustPut(t, s, testKey, "v2 content\n", "edit", "")
	mustPut(t, s, testKey, "v3 content\n", "edit", "")

	meta, err := s.Restore(testKey, 1, "")
	if err != nil {
		t.Fatal(err)
	}
	if meta.Version != 4 || meta.Origin != "restore" || meta.RestoredFrom != 1 {
		t.Fatalf("restore meta = %+v", meta)
	}
	if !strings.Contains(meta.Note, "restored from v1") {
		t.Errorf("note = %q", meta.Note)
	}
	// Head is the copy; the prior head and the source are both intact.
	for v, want := range map[int]string{0: "v1 content\n", 3: "v3 content\n", 1: "v1 content\n"} {
		content, _, err := s.Get(testKey, v)
		if err != nil || content != want {
			t.Errorf("Get(v%d) = %q err=%v, want %q", v, content, err, want)
		}
	}
	// Restoring the current head is refused.
	if _, err := s.Restore(testKey, 4, ""); err == nil {
		t.Error("restore of head accepted")
	}
}

func TestAppliedPointerAndPending(t *testing.T) {
	s := newTestStore(t)
	mustPut(t, s, testKey, "one\n", "paste-in", "")

	info, err := s.Info(testKey)
	if err != nil || !info.Pending || info.Applied != 0 {
		t.Fatalf("fresh doc info = %+v err=%v (want pending, never applied)", info, err)
	}

	if _, err := s.MarkApplied(testKey, 0, false); err != nil {
		t.Fatal(err)
	}
	info, _ = s.Info(testKey)
	if info.Pending || info.Applied != 1 {
		t.Fatalf("after mark: %+v", info)
	}

	mustPut(t, s, testKey, "two\n", "edit", "")
	info, _ = s.Info(testKey)
	if !info.Pending || info.Head != 2 || info.Applied != 1 {
		t.Fatalf("after edit: %+v", info)
	}

	// Restoring the applied content makes pending false again even though the
	// version numbers differ — pending is content-based.
	if _, err := s.Restore(testKey, 1, ""); err != nil {
		t.Fatal(err)
	}
	info, _ = s.Info(testKey)
	if info.Pending || info.Head != 3 {
		t.Fatalf("after restore-to-applied-content: %+v", info)
	}
}

func TestPruneKeepsWindowAndApplied(t *testing.T) {
	s := newTestStore(t)
	s.Retention = 3
	mustPut(t, s, testKey, "a\n", "paste-in", "")
	if _, err := s.MarkApplied(testKey, 1, false); err != nil {
		t.Fatal(err)
	}
	for _, c := range []string{"b\n", "c\n", "d\n", "e\n", "f\n"} {
		mustPut(t, s, testKey, c+"\n", "edit", "")
	}
	info, err := s.Info(testKey)
	if err != nil {
		t.Fatal(err)
	}
	if info.Head != 6 {
		t.Fatalf("head = %d", info.Head)
	}
	// Window keeps v4..v6; v1 survives because it's applied; v2, v3 pruned.
	if _, _, err := s.Get(testKey, 1); err != nil {
		t.Errorf("applied v1 was pruned: %v", err)
	}
	for _, v := range []int{2, 3} {
		if _, _, err := s.Get(testKey, v); err == nil {
			t.Errorf("v%d survived pruning", v)
		}
	}
	if _, _, err := s.Get(testKey, 4); err != nil {
		t.Errorf("v4 inside window was pruned: %v", err)
	}
	if info.Versions != 4 { // v1 + v4..v6
		t.Errorf("versions = %d, want 4", info.Versions)
	}
}

func TestListAndAccounts(t *testing.T) {
	s := newTestStore(t)
	other := DocKey{Provider: "api.fastmail.com", Account: "uc9a940ba", Name: "area-1"}
	stal := DocKey{Provider: "stalwart.local", Account: "acc1", Name: "main"}
	mustPut(t, s, testKey, "x\n", "paste-in", "")
	mustPut(t, s, other, "y\n", "paste-in", "")
	mustPut(t, s, stal, "z\n", "paste-in", "")

	docs, err := s.List(false)
	if err != nil || len(docs) != 3 {
		t.Fatalf("list = %+v err=%v", docs, err)
	}
	if docs[0].Account != "uc9a140ba" || docs[2].Provider != "stalwart.local" {
		t.Errorf("ordering: %+v", docs)
	}

	accounts, err := s.Accounts("api.fastmail.com")
	if err != nil || len(accounts) != 2 {
		t.Fatalf("accounts = %v err=%v", accounts, err)
	}
	none, err := s.Accounts("nowhere.example")
	if err != nil || len(none) != 0 {
		t.Errorf("unknown provider accounts = %v err=%v", none, err)
	}
}

func TestDiffDefaultsAndOutput(t *testing.T) {
	s := newTestStore(t)
	mustPut(t, s, testKey, "if header :contains \"from\" \"a@example.test\" {\n fileinto \"A\";\n}\n", "paste-in", "")
	mustPut(t, s, testKey, "if header :contains \"from\" \"a@example.test\" {\n fileinto \"Archive\";\n}\n", "edit", "")

	diff, err := s.Diff(testKey, 1, 2)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(diff, "--- area-2-below-spam@v1") || !strings.Contains(diff, "+++ area-2-below-spam@v2") {
		t.Errorf("labels missing:\n%s", diff)
	}
	if !strings.Contains(diff, "- fileinto \"A\";") || !strings.Contains(diff, "+ fileinto \"Archive\";") {
		t.Errorf("diff body wrong:\n%s", diff)
	}

	same, err := s.Diff(testKey, 2, 0) // v2 vs head (= v2)
	if err != nil || same != "" {
		t.Errorf("self-diff = %q err=%v", same, err)
	}
}

func TestGetMissing(t *testing.T) {
	s := newTestStore(t)
	if _, _, err := s.Get(testKey, 0); err == nil {
		t.Error("get on empty doc should fail")
	}
	mustPut(t, s, testKey, "x\n", "edit", "")
	if _, _, err := s.Get(testKey, 99); err == nil {
		t.Error("get of missing version should fail")
	}
}

func TestMarkAppliedRefusesTruncationWithoutForce(t *testing.T) {
	s := newTestStore(t)
	mustPut(t, s, testKey, "keep;\n[TRUNCATED IN TOOL IMPORT: rest elsewhere]\n", "paste-in", "big paste")
	if _, err := s.MarkApplied(testKey, 0, false); err == nil || !strings.Contains(err.Error(), "truncated placeholder") {
		t.Fatalf("err = %v, want truncation refusal", err)
	}
	if _, err := s.MarkApplied(testKey, 0, true); err != nil {
		t.Fatalf("force should override: %v", err)
	}

	// A marker in the NOTE alone also trips the guard.
	k2 := DocKey{Provider: "api.fastmail.com", Account: "uc9a140ba", Name: "noted"}
	mustPut(t, s, k2, "keep;\n", "paste-in", "truncated placeholder due to large chat paste")
	if _, err := s.MarkApplied(k2, 0, false); err == nil {
		t.Fatal("want refusal from note marker")
	}
}

func TestContextOnlyBlocksMarkApplied(t *testing.T) {
	s := newTestStore(t)
	mustPut(t, s, testKey, "# full export\n", "paste-in", "reference")
	if err := s.SetContextOnly(testKey, true); err != nil {
		t.Fatal(err)
	}
	info, err := s.Info(testKey)
	if err != nil || !info.ContextOnly {
		t.Fatalf("info = %+v err=%v, want contextOnly", info, err)
	}
	if _, err := s.MarkApplied(testKey, 0, true); err == nil || !strings.Contains(err.Error(), "context-only") {
		t.Fatalf("err = %v, want context-only refusal (even with force)", err)
	}
	if err := s.SetContextOnly(testKey, false); err != nil {
		t.Fatal(err)
	}
	if _, err := s.MarkApplied(testKey, 0, false); err != nil {
		t.Fatalf("cleared context-only should mark fine: %v", err)
	}
	if err := s.SetContextOnly(DocKey{Provider: "p1", Account: "a1", Name: "missing"}, true); err == nil {
		t.Error("SetContextOnly on a missing doc must error")
	}
}

func TestArchiveLifecycle(t *testing.T) {
	s := newTestStore(t)
	mustPut(t, s, testKey, "keep;\n", "paste-in", "")
	stale := DocKey{Provider: testKey.Provider, Account: testKey.Account, Name: "full-export"}
	mustPut(t, s, stale, "# truncated junk\n", "paste-in", "oops")

	if err := s.Archive(stale); err != nil {
		t.Fatal(err)
	}
	docs, err := s.List(false)
	if err != nil || len(docs) != 1 || docs[0].Name != testKey.Name {
		t.Fatalf("default list = %+v err=%v, want only the live doc", docs, err)
	}
	all, err := s.List(true)
	if err != nil || len(all) != 2 {
		t.Fatalf("includeArchived list = %+v", all)
	}
	var arch DocInfo
	for _, d := range all {
		if d.Name == "full-export" {
			arch = d
		}
	}
	if !arch.Archived || arch.Head != 1 {
		t.Errorf("archived info = %+v", arch)
	}

	// Operating on an archived doc explains its state.
	if _, _, err := s.Get(stale, 0); err == nil || !strings.Contains(err.Error(), "archived") {
		t.Errorf("get archived err = %v", err)
	}
	// Archiving again: gone from live set, named in the miss.
	if err := s.Archive(stale); err == nil {
		t.Error("re-archive should fail")
	}

	if err := s.Unarchive(stale); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Get(stale, 0); err != nil {
		t.Errorf("unarchived doc should read: %v", err)
	}
	if err := s.Unarchive(stale); err == nil {
		t.Error("unarchive of a live doc should fail")
	}
}

func TestMissingDocErrorListsExisting(t *testing.T) {
	s := newTestStore(t)
	// Empty store: calibration guidance.
	_, _, err := s.Get(testKey, 0)
	if err == nil || !strings.Contains(err.Error(), "calibration") {
		t.Fatalf("empty-store err = %v", err)
	}
	mustPut(t, s, testKey, "keep;\n", "paste-in", "")
	other := DocKey{Provider: testKey.Provider, Account: testKey.Account, Name: "area-1-before-spam"}
	_, _, err = s.Get(other, 0)
	if err == nil || !strings.Contains(err.Error(), testKey.Name) {
		t.Fatalf("miss err = %v, want existing names listed", err)
	}
}

// A torn head-pointer write (crash mid-update) must not hide the doc:
// listings surface the corruption instead of silently dropping it.
func TestCorruptHeadPointerIsSurfacedNotHidden(t *testing.T) {
	s := newTestStore(t)
	mustPut(t, s, testKey, "keep;\n", "paste-in", "")
	headPath := filepath.Join(s.root, testKey.Provider, testKey.Account, testKey.Name, "head")
	if err := os.WriteFile(headPath, nil, 0o644); err != nil { // zero-length = torn write
		t.Fatal(err)
	}
	if _, _, err := s.Get(testKey, 0); err == nil || !strings.Contains(err.Error(), "corrupt") {
		t.Fatalf("get err = %v, want corrupt-pointer error", err)
	}
	docs, err := s.List(false)
	if err != nil || len(docs) != 1 {
		t.Fatalf("list = %+v err=%v, want the broken doc listed", docs, err)
	}
	if docs[0].Error == "" || !strings.Contains(docs[0].Error, "corrupt") {
		t.Errorf("listed doc must carry the error: %+v", docs[0])
	}
}

// A corrupt applied pointer must disable pruning — the confirmed-live
// version may never be collected on the strength of an unreadable pointer.
func TestCorruptAppliedPointerSkipsPrune(t *testing.T) {
	s := newTestStore(t)
	s.Retention = 2
	mustPut(t, s, testKey, "v1;\n", "paste-in", "")
	if _, err := s.MarkApplied(testKey, 1, false); err != nil {
		t.Fatal(err)
	}
	appliedPath := filepath.Join(s.root, testKey.Provider, testKey.Account, testKey.Name, "applied")
	if err := os.WriteFile(appliedPath, []byte("garbage"), 0o644); err != nil {
		t.Fatal(err)
	}
	mustPut(t, s, testKey, "v2;\n", "edit", "")
	mustPut(t, s, testKey, "v3;\n", "edit", "")
	mustPut(t, s, testKey, "v4;\n", "edit", "") // healthy pointer would prune v1 here (applied, so kept) — corrupt must skip entirely
	if content, _, err := s.Get(testKey, 1); err != nil || content != "v1;\n" {
		t.Fatalf("v1 = %q err=%v, want applied version preserved despite corrupt pointer", content, err)
	}
}

// An orphaned version file (content written, head never advanced — crash or
// a concurrent writer) is never overwritten; the next append goes past it.
func TestAppendNeverOverwritesOrphanVersion(t *testing.T) {
	s := newTestStore(t)
	mustPut(t, s, testKey, "v1;\n", "paste-in", "")
	orphanPath := filepath.Join(s.root, testKey.Provider, testKey.Account, testKey.Name, "versions", "v000002.sieve")
	if err := os.WriteFile(orphanPath, []byte("orphan content;\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	meta := mustPut(t, s, testKey, "v-new;\n", "edit", "")
	if meta.Version != 3 {
		t.Fatalf("new version = %d, want 3 (past the orphan)", meta.Version)
	}
	if content, _, err := s.Get(testKey, 2); err != nil || content != "orphan content;\n" {
		t.Errorf("orphan = %q err=%v, must survive untouched", content, err)
	}
	info, err := s.Info(testKey)
	if err != nil || info.Head != 3 {
		t.Errorf("info = %+v err=%v", info, err)
	}
}

// Pointer writes are atomic: no partially-written pointer is ever observable
// (verified indirectly — the temp file never lingers and the value commits).
func TestPointerWriteLeavesNoTempDroppings(t *testing.T) {
	s := newTestStore(t)
	mustPut(t, s, testKey, "keep;\n", "paste-in", "")
	if _, err := s.MarkApplied(testKey, 0, false); err != nil {
		t.Fatal(err)
	}
	docDir := filepath.Join(s.root, testKey.Provider, testKey.Account, testKey.Name)
	entries, err := os.ReadDir(docDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.Contains(e.Name(), ".tmp-") {
			t.Errorf("leftover temp pointer file: %s", e.Name())
		}
	}
}

// Creating a live doc over an archived namesake would wedge both directions
// (unarchive and archive each collide) — Put refuses up front.
func TestPutRefusesOverArchivedNamesake(t *testing.T) {
	s := newTestStore(t)
	mustPut(t, s, testKey, "keep;\n", "paste-in", "")
	if err := s.Archive(testKey); err != nil {
		t.Fatal(err)
	}
	_, _, err := s.Put(testKey, "fresh;\n", "paste-in", "")
	if err == nil || !strings.Contains(err.Error(), "unarchive") {
		t.Fatalf("err = %v, want archived-namesake refusal", err)
	}
	if err := s.Unarchive(testKey); err != nil {
		t.Fatalf("unarchive after refusal must work: %v", err)
	}
	if _, appended, err := s.Put(testKey, "fresh;\n", "paste-in", ""); err != nil || !appended {
		t.Fatalf("put after unarchive: appended=%v err=%v", appended, err)
	}
}
