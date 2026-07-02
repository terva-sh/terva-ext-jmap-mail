module terva-ext-jmap-mail

go 1.22

// The terva Go SDK is the public extension-author surface. Pinned to a published
// release and vendored: run.sh builds with -mod=vendor, so the binary builds
// offline and the installed copy is self-contained, never depending on a local
// checkout. v0.112.0 ships extension protocol 4 (set_withdrawn_tools — the tool
// visibility this extension uses when unconfigured) plus the full authority
// taxonomy (network-read, external-mutation, local-data). To move to a newer
// SDK: bump the require, then re-`go mod vendor` (just tidy).
require terva.sh/terva v0.112.0
