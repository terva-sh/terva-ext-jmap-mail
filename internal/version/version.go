// Package version is the single code-side source of the extension's version
// string (sent in the protocol hello via ext.New). It is kept equal to
// extension.json's "version" by a test in this package, so the two can't drift.
package version

// Version is the extension version. Keep it equal to extension.json's "version"
// (TestManifestVersionMatchesCode enforces this) and bump BOTH on every release
// — minor for features, patch for fixes — then tag the release to match.
//
// A var (not a const) so a release build could inject it via
// -ldflags "-X terva-ext-jmap-mail/internal/version.Version=..." if goreleaser
// is added later; source builds (run.sh, just build) report this committed
// default.
var Version = "0.10.1"
