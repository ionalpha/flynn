#!/bin/sh
# Flynn installer for Linux and macOS.
#
# Downloads a prebuilt release binary, verifies its SHA-256 checksum (and its
# cosign signature when cosign is available), and installs it. Refuses to
# install on any checksum or signature mismatch.
#
# Usage:
#   curl -fsSL https://raw.githubusercontent.com/ionalpha/flynn/main/install.sh | sh
#
# Environment overrides:
#   FLYNN_VERSION      install a specific tag (e.g. v0.1.0); default: the latest release
#   FLYNN_INSTALL_DIR  install directory;                   default: $HOME/.local/bin
#
# On Windows, use install.ps1 instead.
set -eu

REPO="ionalpha/flynn"
BINARY="flynn"

log() { printf '%s\n' "$*" >&2; }
err() {
	printf 'install: error: %s\n' "$*" >&2
	exit 1
}

have() { command -v "$1" >/dev/null 2>&1; }

# fetch URL FILE: download URL to FILE using whichever of curl or wget is present.
fetch() {
	if have curl; then
		curl -fsSL -o "$2" "$1"
	elif have wget; then
		wget -qO "$2" "$1"
	else
		err "need curl or wget to download"
	fi
}

# fetch_stdout URL: print URL's body to stdout (for the release-lookup API).
fetch_stdout() {
	if have curl; then
		curl -fsSL "$1"
	elif have wget; then
		wget -qO- "$1"
	else
		err "need curl or wget to download"
	fi
}

# sha256 FILE: print the file's lowercase SHA-256, using whichever tool is present.
sha256() {
	if have sha256sum; then
		sha256sum "$1" | awk '{print $1}'
	elif have shasum; then
		shasum -a 256 "$1" | awk '{print $1}'
	else
		err "need sha256sum or shasum to verify the download"
	fi
}

# --- detect OS and architecture -------------------------------------------
os=$(uname -s)
case "$os" in
Linux) os=linux ;;
Darwin) os=darwin ;;
*) err "unsupported OS '$os'; this installer supports Linux and macOS (on Windows use install.ps1)" ;;
esac

arch=$(uname -m)
case "$arch" in
x86_64 | amd64) arch=amd64 ;;
aarch64 | arm64) arch=arm64 ;;
*) err "unsupported architecture '$arch' (supported: amd64, arm64)" ;;
esac

# --- resolve the version to install ---------------------------------------
version="${FLYNN_VERSION:-}"
if [ -z "$version" ]; then
	version=$(fetch_stdout "https://api.github.com/repos/$REPO/releases/latest" |
		sed -n 's/.*"tag_name" *: *"\([^"]*\)".*/\1/p' | head -n 1)
	[ -n "$version" ] || err "could not resolve the latest release; set FLYNN_VERSION (e.g. FLYNN_VERSION=v0.1.0)"
fi

asset="${BINARY}_${os}_${arch}.tar.gz"
# FLYNN_BASE_URL overrides where the release files are fetched from (a private mirror,
# an air-gapped server); it defaults to the GitHub release for this version.
base="${FLYNN_BASE_URL:-https://github.com/$REPO/releases/download/$version}"

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT INT TERM

# --- download the archive and checksums -----------------------------------
log "Downloading $asset ($version)..."
fetch "$base/$asset" "$tmp/$asset" ||
	err "download failed: $base/$asset (does release $version include a $os/$arch build?)"
fetch "$base/checksums.txt" "$tmp/checksums.txt" ||
	err "could not download checksums.txt for $version"

# --- verify the SHA-256 (the mandatory integrity gate) --------------------
want=$(awk -v f="$asset" '$2 == f {print $1}' "$tmp/checksums.txt")
[ -n "$want" ] || err "no checksum is recorded for $asset in checksums.txt"
got=$(sha256 "$tmp/$asset")
[ "$want" = "$got" ] || err "checksum mismatch for $asset: expected $want, got $got; refusing to install"
log "Checksum verified."

# --- verify the cosign signature of checksums.txt when possible -----------
# The checksum file is signed keyless with cosign at release time. Verifying it
# proves the checksums themselves are authentic (not just that the archive
# matches whatever checksums.txt was served). When cosign is present a bad
# signature is fatal; when it is absent we proceed on the SHA-256 alone and
# print how to verify by hand.
if have cosign; then
	if fetch "$base/checksums.txt.pem" "$tmp/checksums.txt.pem" 2>/dev/null &&
		fetch "$base/checksums.txt.sig" "$tmp/checksums.txt.sig" 2>/dev/null; then
		if cosign verify-blob \
			--certificate "$tmp/checksums.txt.pem" \
			--signature "$tmp/checksums.txt.sig" \
			--certificate-oidc-issuer "https://token.actions.githubusercontent.com" \
			--certificate-identity-regexp "^https://github.com/$REPO/" \
			"$tmp/checksums.txt" >/dev/null 2>&1; then
			log "Signature verified (cosign)."
		else
			err "cosign signature verification failed; refusing to install"
		fi
	fi
else
	log "cosign not found; verified the SHA-256 only. To also verify the signature, install cosign and re-run."
fi

# --- extract and install --------------------------------------------------
tar -xzf "$tmp/$asset" -C "$tmp" || err "could not extract $asset"
[ -f "$tmp/$BINARY" ] || err "the archive did not contain a '$BINARY' binary"

dir="${FLYNN_INSTALL_DIR:-$HOME/.local/bin}"
mkdir -p "$dir" || err "could not create install directory $dir"
mv "$tmp/$BINARY" "$dir/$BINARY" ||
	err "could not install to $dir; set FLYNN_INSTALL_DIR to a writable directory"
chmod +x "$dir/$BINARY"

log ""
log "Installed $BINARY to $dir/$BINARY"
case ":$PATH:" in
*":$dir:"*) ;;
*)
	log "NOTE: $dir is not on your PATH. Add it to your shell profile:"
	log "  export PATH=\"$dir:\$PATH\""
	;;
esac
log "Get started:  $BINARY --version   then   $BINARY goal \"...\"   and   $BINARY spine verify <run>"
