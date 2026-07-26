#!/usr/bin/env bash
# jmap-mail launcher.
#
# terva runs an extension by executing the manifest's `exec` verbatim — it never
# compiles Go. This wrapper makes sure a binary exists, then execs it, so
# `terva ext install <path|git-url>` works whether or not the host has a Go
# toolchain:
#
#   1. a prebuilt binary for this version and platform, from the tag's GitHub
#      release, accepted only if its SHA-256 matches the published SHA256SUMS;
#   2. otherwise `go build` against the committed vendor/ tree (offline).
#
# The download is a convenience, never a requirement: a checksum that does not
# match, a missing asset, no network, or no curl/wget all fall through to the
# source build, and JMAP_MAIL_BUILD_FROM_SOURCE=1 skips step 1 entirely.
#
# IMPORTANT: stdout is the protocol wire. Every byte of build chatter must go to
# stderr (terva captures it to $TERVA_HOME/logs/ext-jmap-mail.log); a stray
# stdout write corrupts the JSON stream.
set -euo pipefail
cd "$(dirname "$0")"

bin="./jmap-mail"
release_base="${JMAP_MAIL_RELEASE_BASE:-https://github.com/terva-sh/terva-ext-jmap-mail/releases/download}"

needs_build() {
	[ -x "$bin" ] || return 0
	if [ -n "$(find . -name '*.go' -newer "$bin" -print -quit 2>/dev/null)" ]; then
		return 0
	fi
	if [ go.mod -nt "$bin" ]; then
		return 0
	fi
	if [ -f go.sum ] && [ go.sum -nt "$bin" ]; then
		return 0
	fi
	return 1
}

# extension.json's version is the one the release tag is named after; a test
# keeps internal/version.Version equal to it, so either would do.
manifest_version() {
	sed -n 's/.*"version"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' extension.json | head -1
}

# release_platform prints the <os>_<arch> half of an asset name, or nothing on a
# platform the release does not build for.
release_platform() {
	local os arch
	case "$(uname -s)" in
	Darwin) os=darwin ;;
	Linux) os=linux ;;
	*) return 1 ;;
	esac
	case "$(uname -m)" in
	x86_64 | amd64) arch=amd64 ;;
	arm64 | aarch64) arch=arm64 ;;
	*) return 1 ;;
	esac
	echo "${os}_${arch}"
}

# locally_modified reports a source tree that no longer matches its release, so
# a developer's edits are never silently replaced by the published binary.
locally_modified() {
	command -v git >/dev/null 2>&1 || return 1
	git rev-parse --is-inside-work-tree >/dev/null 2>&1 || return 1
	[ -n "$(git status --porcelain -- '*.go' go.mod go.sum vendor 2>/dev/null)" ]
}

# fetch stays quiet on failure: a missing asset is an ordinary fallback, not an
# incident, and the launcher explains itself in its own words below.
fetch() { # url dest
	if command -v curl >/dev/null 2>&1; then
		curl -fsL --retry 2 --connect-timeout 10 --max-time 300 -o "$2" "$1" 2>/dev/null
	elif command -v wget >/dev/null 2>&1; then
		wget -q -O "$2" "$1" 2>/dev/null
	else
		return 1
	fi
}

sha256_of() { # file
	if command -v shasum >/dev/null 2>&1; then
		shasum -a 256 "$1" | awk '{print $1}'
	elif command -v sha256sum >/dev/null 2>&1; then
		sha256sum "$1" | awk '{print $1}'
	else
		return 1
	fi
}

# try_prebuilt installs the released binary for this version+platform, or
# returns non-zero having installed nothing. It never leaves an unverified file
# where the launcher would exec it.
try_prebuilt() {
	local version platform asset url tmp want got
	version="$(manifest_version)" || return 1
	[ -n "$version" ] || return 1
	platform="$(release_platform)" || return 1
	asset="jmap-mail_${platform}"
	url="${release_base}/v${version}"

	# Staged inside the install dir so the final move is an atomic rename on
	# the same filesystem — a half-written binary is never exec'd.
	tmp="$(mktemp -d ./.jmap-mail-dl.XXXXXX)" || return 1
	if ! fetch "$url/$asset" "$tmp/$asset" || ! fetch "$url/SHA256SUMS" "$tmp/SHA256SUMS"; then
		echo "[jmap-mail] no prebuilt $asset for v${version} (missing, or the host is offline)." >&2
		rm -rf "$tmp"
		return 1
	fi
	want="$(awk -v f="$asset" '$2 == f || $2 == "*" f {print $1; exit}' "$tmp/SHA256SUMS")"
	got="$(sha256_of "$tmp/$asset")" || { rm -rf "$tmp"; return 1; }
	if [ -z "$want" ]; then
		echo "[jmap-mail] $asset is not listed in the release's SHA256SUMS; building from source instead." >&2
		rm -rf "$tmp"
		return 1
	fi
	if [ "$want" != "$got" ]; then
		echo "[jmap-mail] CHECKSUM MISMATCH for $asset (expected $want, got $got)." >&2
		echo "[jmap-mail] Discarding the download and building from source." >&2
		rm -rf "$tmp"
		return 1
	fi
	chmod +x "$tmp/$asset"
	mv -f "$tmp/$asset" "$bin"
	rm -rf "$tmp"
	echo "[jmap-mail] installed prebuilt v${version} for ${platform} (sha256 verified)." >&2
}

build_from_source() {
	if ! command -v go >/dev/null 2>&1; then
		echo "[jmap-mail] No usable binary: no verified download, and no Go toolchain on PATH." >&2
		echo "[jmap-mail] Either install Go 1.22+ (https://go.dev/dl/) or make sure this host can reach" >&2
		echo "[jmap-mail] ${release_base} to fetch the prebuilt binary, then relaunch terva." >&2
		exit 1
	fi
	echo "[jmap-mail] building $bin (first launch or sources changed)…" >&2
	# -mod=vendor builds against the committed vendor/ tree: offline, no module
	# download — so the installed copy is self-contained and the first launch
	# can't hang on a fetch.
	go build -mod=vendor -o "$bin" . >&2
	echo "[jmap-mail] build complete." >&2
}

if needs_build; then
	installed=0
	if [ "${JMAP_MAIL_BUILD_FROM_SOURCE:-0}" = "1" ]; then
		echo "[jmap-mail] JMAP_MAIL_BUILD_FROM_SOURCE=1 — skipping the prebuilt download." >&2
	elif locally_modified; then
		echo "[jmap-mail] source tree has local changes — building them instead of downloading." >&2
	elif try_prebuilt; then
		installed=1
	fi
	[ "$installed" -eq 1 ] || build_from_source
fi

exec "$bin" "$@"
