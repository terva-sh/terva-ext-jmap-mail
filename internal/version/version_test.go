package version

import (
	"encoding/json"
	"os"
	"testing"
)

// TestManifestVersionMatchesCode pins extension.json's version (what the host
// shows in `ext list`) equal to Version (what ext.New sends in the protocol
// hello). They are two sources for the same fact; bump them together at release.
// This guard fails the build if they drift — the silent failure mode otherwise.
//
// The path is relative to this package dir, which is where `go test` runs it
// from, so it resolves the repo-root extension.json under `just test`/`just ci`
// (both run ./internal/...).
func TestManifestVersionMatchesCode(t *testing.T) {
	b, err := os.ReadFile("../../extension.json")
	if err != nil {
		t.Fatalf("read extension.json: %v", err)
	}
	var m struct {
		Version string `json:"version"`
	}
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("parse extension.json: %v", err)
	}
	if m.Version != Version {
		t.Errorf("extension.json version %q != internal/version.Version %q — bump them together", m.Version, Version)
	}
}

// TestLauncherIsExecutable guards the failure mode where run.sh loses (or
// never had) its executable bit: terva execs the manifest's `exec` verbatim,
// so a 644 launcher dies with EACCES before emitting a single byte — an
// installed extension that shows "off (not running)" with an empty log.
func TestLauncherIsExecutable(t *testing.T) {
	info, err := os.Stat("../../run.sh")
	if err != nil {
		t.Fatalf("stat run.sh: %v", err)
	}
	if info.Mode()&0o111 == 0 {
		t.Error("run.sh is not executable — chmod +x run.sh (and commit; git tracks the mode)")
	}
}
