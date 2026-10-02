#!/bin/sh
# install.sh — download the Tenant release for this OS and CPU, verify its
# SHA-256 against the release's checksums.txt, and install the binary to a
# user-writable directory (no sudo). The release workflow stamps the repository
# name in; the tracked copy carries the __REPO__ placeholder.
#
#   curl -fsSL https://github.com/OWNER/REPO/releases/latest/download/install.sh | sh
#
# Environment overrides:
#   TENANT_VERSION      version to install, without the leading v (default: latest release)
#   TENANT_REPO         GitHub owner/name asked for the latest release
#   TENANT_BASE_URL     directory URL that holds the archive and checksums.txt
#   TENANT_INSTALL_DIR  where the tenant binary goes (default: $HOME/.local/bin)
set -eu

REPO="${TENANT_REPO:-__REPO__}"
DIR="${TENANT_INSTALL_DIR:-$HOME/.local/bin}"

die() { printf 'install.sh: %s\n' "$*" >&2; exit 1; }

case "$(uname -s)" in
  Darwin) OS=darwin ;;
  Linux) OS=linux ;;
  *) die "unsupported OS $(uname -s); on Windows use install.ps1" ;;
esac
case "$(uname -m)" in
  x86_64 | amd64) ARCH=x86_64 ;;
  arm64 | aarch64) ARCH=aarch64 ;;
  *) die "unsupported CPU $(uname -m)" ;;
esac

if command -v curl >/dev/null 2>&1; then
  fetch() { curl -fsSL -o "$2" "$1"; }
elif command -v wget >/dev/null 2>&1; then
  fetch() { wget -q -O "$2" "$1"; }
else
  die "curl or wget is required"
fi

TMP=$(mktemp -d)
trap 'rm -rf "$TMP" "$DIR/.tenant.$$"' EXIT

VERSION="${TENANT_VERSION:-}"
if [ -z "$VERSION" ]; then
  API="https://api.github.com/repos/$REPO/releases/latest"
  fetch "$API" "$TMP/release.json" || die "could not query $API"
  VERSION=$(sed -n 's/.*"tag_name":[[:space:]]*"v\{0,1\}\([^"]*\)".*/\1/p' "$TMP/release.json" | head -n 1)
  [ -n "$VERSION" ] || die "no tag_name in the response from $API"
fi

BASE="${TENANT_BASE_URL:-https://github.com/$REPO/releases/download/v$VERSION}"
ARCHIVE="tenant_${VERSION}_${OS}_${ARCH}.tar.gz"
echo "Downloading $BASE/$ARCHIVE"
fetch "$BASE/$ARCHIVE" "$TMP/$ARCHIVE" || die "download failed: $BASE/$ARCHIVE"
fetch "$BASE/checksums.txt" "$TMP/checksums.txt" || die "download failed: $BASE/checksums.txt"

if command -v sha256sum >/dev/null 2>&1; then
  GOT=$(sha256sum "$TMP/$ARCHIVE")
elif command -v shasum >/dev/null 2>&1; then
  GOT=$(shasum -a 256 "$TMP/$ARCHIVE")
else
  die "sha256sum or shasum is required"
fi
GOT=${GOT%% *}
WANT=$(awk -v f="$ARCHIVE" '$2 == f { print $1 }' "$TMP/checksums.txt")
[ -n "$WANT" ] || die "$ARCHIVE is not listed in checksums.txt"
[ "$GOT" = "$WANT" ] || die "checksum mismatch for $ARCHIVE: got $GOT, want $WANT"

tar -xzf "$TMP/$ARCHIVE" -C "$TMP" tenant || die "could not extract tenant from $ARCHIVE"
mkdir -p "$DIR" || die "could not create $DIR"
cp "$TMP/tenant" "$DIR/.tenant.$$" || die "could not write to $DIR"
chmod 0755 "$DIR/.tenant.$$"
mv -f "$DIR/.tenant.$$" "$DIR/tenant"

echo "Installed $DIR/tenant"
"$DIR/tenant" version
case ":$PATH:" in
  *":$DIR:"*) ;;
  *)
    echo "$DIR is not on your PATH. Add it, then open a new shell:"
    case "$(basename "${SHELL:-sh}")" in
      fish) echo "  fish_add_path $DIR" ;;
      zsh) echo "  echo 'export PATH=\"$DIR:\$PATH\"' >> ~/.zshrc" ;;
      *) echo "  echo 'export PATH=\"$DIR:\$PATH\"' >> ~/.bashrc" ;;
    esac
    ;;
esac
echo "Next: tenant setup"
