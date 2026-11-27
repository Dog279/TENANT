#!/bin/sh
# install.sh — download the Tenant release for this OS and CPU, verify its
# SHA-256 against the release's checksums.txt, and install the binary to a
# user-writable directory (no sudo). On macOS the binary is signed on this
# machine with the hardened runtime before it is moved into place. The release
# workflow stamps the repository name and the release's own version in; the
# tracked copy carries the __REPO__ and __VERSION__ placeholders, and asks for
# the latest release instead.
#
#   curl -fsSL https://github.com/OWNER/REPO/releases/download/vX.Y.Z/install.sh | sh
#
# Environment overrides:
#   TENANT_VERSION      version to install, without the leading v (default: the
#                       release this script came from; the latest release for
#                       the tracked copy)
#   TENANT_REPO         GitHub owner/name asked for the latest release
#   TENANT_BASE_URL     directory URL that holds the archive and checksums.txt
#   TENANT_INSTALL_DIR  where the tenant binary goes (default: $HOME/.local/bin)
#
# To make this machine a child of a fleet in the same paste, set TENANT_CHILD:
# the script goes straight on to `tenant child install`.
#
#   curl -fsSL .../install.sh | TENANT_CHILD=system TENANT_ENROLL_CODE='<code>' sudo --preserve-env=TENANT_CHILD,TENANT_ENROLL_CODE sh
#
# (`sudo VAR=value sh` would put the code on sudo's command line, which the
# process list shows; set before sudo and preserved, it stays in the
# environment. `tenant fleet enroll` prints this line with the code in.)
#
#   TENANT_CHILD        system: the child is a service that runs as root from
#                       boot (run the script as root; the binary then goes to
#                       /usr/local/bin, where only root can replace it). A
#                       development build of an older master says user, and
#                       gets the same
#   TENANT_ENROLL_CODE  the one-time code `tenant fleet enroll <name>` printed on the
#                       master. Left unset, it is asked for on the terminal. It is
#                       never passed as an argument, where the process list shows it.
set -eu

die() { printf 'install.sh: %s\n' "$*" >&2; exit 1; }

REPO="${TENANT_REPO:-__REPO__}"
CHILD="${TENANT_CHILD:-}"
case "$CHILD" in
  "") DIR="${TENANT_INSTALL_DIR:-$HOME/.local/bin}" ;;
  system | user)
    [ "$(id -u)" = 0 ] || die "a child is a service that runs as root from boot: run this script as root (put 'sudo --preserve-env=TENANT_CHILD,TENANT_ENROLL_CODE' before the last 'sh')"
    CHILD=system
    DIR="${TENANT_INSTALL_DIR:-/usr/local/bin}"
    ;;
  *) die "TENANT_CHILD is system, not $CHILD" ;;
esac

# macOS: the installed binary carries the hardened-runtime flag, which makes
# the system refuse a debugger outright (process protection). The copy that
# is about to be installed is signed here, ad hoc and with no entitlements,
# and the flag is checked on it, before it is moved into place: no unsigned
# binary is ever the one that runs. If any of it fails nothing is moved, and
# a Tenant installed before stays as it was. codesign is part of macOS; it is
# named by its full path so that another codesign on PATH is not what signs.
CODESIGN=/usr/bin/codesign

harden() { # $1: the copy that is about to be moved into place
  [ "$OS" = darwin ] || return 0
  [ -x "$CODESIGN" ] || die "$CODESIGN is missing, so the binary cannot be signed with the hardened runtime; nothing was installed or changed"
  "$CODESIGN" --force --sign - --options runtime "$1" ||
    die "codesign could not sign the binary with the hardened runtime; nothing was installed or changed"
  SIGNATURE=$("$CODESIGN" -dv "$1" 2>&1) ||
    die "codesign could not read the signature it just made; nothing was installed or changed"
  printf '%s\n' "$SIGNATURE" | grep -q '^CodeDirectory .*flags=0x[0-9a-fA-F]*(.*runtime.*)' ||
    die "the hardened-runtime flag is not on the signed binary; nothing was installed or changed"
}

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

VERSION="${TENANT_VERSION:-__VERSION__}"
# Unstamped: the tracked copy. `latest` skips prereleases, so a script taken
# from a prerelease must never get here, or it installs an older binary.
if case $VERSION in __*) true ;; *) false ;; esac; then
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
harden "$DIR/.tenant.$$"
mv -f "$DIR/.tenant.$$" "$DIR/tenant" || die "could not move the binary into place in $DIR"

echo "Installed $DIR/tenant"
if [ "$OS" = darwin ]; then echo "Signed on this machine with the hardened runtime (ad hoc, no entitlements)"; fi
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

if [ -n "$CHILD" ]; then
  set -- child install
  if [ "$CHILD" = system ]; then set -- "$@" --system; fi
  if [ -n "${TENANT_ENROLL_CODE:-}" ]; then
    "$DIR/tenant" "$@" # the code travels in the environment
  elif (: </dev/tty) 2>/dev/null; then
    "$DIR/tenant" "$@" </dev/tty # this script came in on standard input, so the code is typed
  else
    die "no terminal to ask for the enrollment code on: set TENANT_ENROLL_CODE"
  fi
  exit 0
fi
echo "Next: tenant setup"
