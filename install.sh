#!/bin/sh
# Install ka from a GitHub release on Linux or macOS.
#
#   curl -fsSL https://raw.githubusercontent.com/KanArtist/ka/main/install.sh | sh
#
# Environment:
#   KA_VERSION      a release to install, e.g. 1.0.3 (default: the latest), or
#                   "main" to build the newest commit with Go
#   KA_INSTALL_DIR  where to put the binary (default: /usr/local/bin as root,
#                   otherwise ~/.local/bin)
#   KA_FROM_SOURCE  1 to build with Go even when a release archive exists
#
# The archive is checked against the release's checksums.txt. If cosign is
# installed, checksums.txt is first verified against its Sigstore bundle, which
# proves it was built by this repository's release workflow.
#
# With Go 1.27 or newer installed, ka is built from source when there is no
# archive for this platform or version, or when asked. Run from a checkout of
# the ka repository (sh install.sh), the script installs the release that
# matches the checked-out tag, or builds the checkout itself when it is not on
# a tag, so the binary always fits the checkout's ui/.
# Running the script again upgrades ka in place.
set -eu

REPO=KanArtist/ka
IDENTITY='^https://github\.com/KanArtist/ka/\.github/workflows/release\.yml@refs/tags/v'
ISSUER=https://token.actions.githubusercontent.com

say() { printf '%s\n' "$*" >&2; }
die() { say "ka install: $*"; exit 1; }

if [ -n "${KA_INSTALL_DIR:-}" ]; then
	dir=$KA_INSTALL_DIR
elif [ "$(id -u)" = 0 ]; then
	dir=/usr/local/bin
else
	dir="$HOME/.local/bin"
fi

finish() {
	say "Installed $("$dir/ka" version 2>/dev/null || echo ka) to $dir/ka"
	case ":$PATH:" in
	*":$dir:"*) ;;
	*) say "Note: $dir is not on your PATH. Add it, e.g.: export PATH=\"$dir:\$PATH\"" ;;
	esac
	command -v git >/dev/null 2>&1 || say "Note: ka needs git 2.40 or newer at run time."
	exit 0
}

have_go() { command -v go >/dev/null 2>&1; }

prepare_dir() {
	mkdir -p "$dir" 2>/dev/null || die "cannot create $dir (set KA_INSTALL_DIR, or run as root)"
	[ -w "$dir" ] || die "cannot write to $dir (set KA_INSTALL_DIR, or run as root)"
}

# go_install REF builds github.com/KanArtist/ka/cmd/ka@REF into $dir.
go_install() {
	have_go || die "$1"
	prepare_dir
	say "Building ka $2 with $(go version | cut -d' ' -f3)"
	GOBIN="$dir" go install "github.com/$REPO/cmd/ka@$2" || die "go install failed"
	finish
}

# A checkout of this repository: install what fits its ui/.
here=$(cd "$(dirname "$0")" 2>/dev/null && pwd) || here=
if [ -n "$here" ] && [ -f "$here/embed.go" ] && [ -f "$here/ui/ka-ui.json" ] &&
	grep -q "^module github.com/$REPO\$" "$here/go.mod" 2>/dev/null && [ -z "${KA_VERSION:-}" ]; then
	if tag=$(git -C "$here" describe --tags --exact-match 2>/dev/null); then
		KA_VERSION=$tag
		say "This checkout is at $tag"
	elif have_go; then
		prepare_dir
		say "This checkout is not on a release tag; building it with $(go version | cut -d' ' -f3)"
		(cd "$here" && GOBIN="$dir" go install ./cmd/ka) || die "go install failed"
		finish
	else
		say "This checkout is not on a release tag and Go is not installed: installing the latest release."
		say "Its web UI may not fit that release; ka then serves the UI built into the binary."
	fi
fi

case "${KA_VERSION:-}" in
main | head | HEAD) go_install "building the newest commit needs Go 1.27 or newer (https://go.dev/dl/)" main ;;
esac

os=''
arch=''
case "$(uname -s)" in
Linux) os=linux ;;
Darwin) os=darwin ;;
MINGW* | MSYS* | CYGWIN*) die "on Windows, run install.ps1 in PowerShell (or: scoop install kanartist/ka, winget install KanArtist.ka)" ;;
esac
case "$(uname -m)" in
x86_64 | amd64) arch=amd64 ;;
aarch64 | arm64) arch=arm64 ;;
esac
if [ -z "$os" ] || [ -z "$arch" ]; then
	v=${KA_VERSION:-latest}
	case "$v" in latest) ;; *) v=v${v#v} ;; esac
	go_install "there is no ka release for $(uname -s) $(uname -m); install Go 1.27 or newer (https://go.dev/dl/) and run this again" "$v"
fi
if [ "${KA_FROM_SOURCE:-}" = 1 ]; then
	v=${KA_VERSION:-latest}
	case "$v" in latest) ;; *) v=v${v#v} ;; esac
	go_install "KA_FROM_SOURCE=1 needs Go 1.27 or newer (https://go.dev/dl/)" "$v"
fi

if command -v curl >/dev/null 2>&1; then
	fetch() { curl -fsSL --retry 3 -o "$2" "$1"; }
	redirect() { curl -fsS -o /dev/null -w '%{redirect_url}' "$1"; }
elif command -v wget >/dev/null 2>&1; then
	fetch() { wget -q -O "$2" "$1"; }
	redirect() { wget -S --max-redirect=0 -O /dev/null "$1" 2>&1 | sed -n 's/^ *[Ll]ocation: *//p' | tr -d '\r' | head -n 1; }
else
	die "needs curl or wget"
fi

version=${KA_VERSION:-}
version=${version#v}
if [ -z "$version" ]; then
	# Resolve "latest" once, so every file comes from the same release even
	# while a new one is being published.
	tag=$(redirect "https://github.com/$REPO/releases/latest") || tag=
	version=${tag##*/tag/v}
	case "$version" in
	"" | */*) die "could not find the latest release of $REPO" ;;
	esac
fi
base="https://github.com/$REPO/releases/download/v$version"

if command -v sha256sum >/dev/null 2>&1; then
	sha256() { sha256sum "$1" | cut -d' ' -f1; }
elif command -v shasum >/dev/null 2>&1; then
	sha256() { shasum -a 256 "$1" | cut -d' ' -f1; }
else
	die "needs sha256sum or shasum to check the download"
fi

prepare_dir

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT INT TERM

archive="ka_${os}_${arch}.tar.gz"
say "Downloading ka $version ($archive)"
if ! fetch "$base/$archive" "$tmp/$archive"; then
	have_go || die "could not download $base/$archive"
	say "No release archive $archive for $version; building it from source instead."
	go_install "" "v$version"
fi
fetch "$base/checksums.txt" "$tmp/checksums.txt" || die "could not download checksums.txt"

if command -v cosign >/dev/null 2>&1; then
	fetch "$base/checksums.txt.sigstore.json" "$tmp/checksums.txt.sigstore.json" ||
		die "could not download the checksums signature"
	cosign verify-blob --bundle "$tmp/checksums.txt.sigstore.json" \
		--certificate-identity-regexp "$IDENTITY" --certificate-oidc-issuer "$ISSUER" \
		"$tmp/checksums.txt" >/dev/null 2>&1 || die "checksums.txt failed signature verification"
	say "Verified the release signature"
fi

want=$(awk -v f="$archive" '$2 == f { print $1 }' "$tmp/checksums.txt")
[ -n "$want" ] || die "$archive is not listed in checksums.txt"
[ "$(sha256 "$tmp/$archive")" = "$want" ] || die "checksum mismatch for $archive"

tar -xzf "$tmp/$archive" -C "$tmp" ka
# Replace by rename so a running ka keeps its old binary.
cp "$tmp/ka" "$dir/.ka.new"
chmod 755 "$dir/.ka.new"
mv -f "$dir/.ka.new" "$dir/ka"

finish
