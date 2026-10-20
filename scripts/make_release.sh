#!/usr/bin/env bash
# make_release.sh [--arch universal|native|x86_64|arm64] [--notes file] [--final] [--publish]
#
# Makes a Catalina32 release from a clean master checkout (macOS, Go, Xcode command-line tools):
#   1. builds build/Catalina32.app with scripts/build-app.sh, never with the AOT code;
#   2. checks the engine holds no AOT-compiled game code (it is a translation of the game's code
#      and must never be shared) and has the architectures asked for;
#   3. zips it into build/release/Catalina32-<version>.zip (ditto keeps the signature);
#   4. writes build/release/NOTES-<version>.md (the text below, plus --notes, plus the changes
#      since the previous release tag).
# Nothing leaves this Mac until --publish: check the zip and the notes, then run again with it.
#   --publish  tag v<version>, push the tag and create a GitHub release with the zip (needs gh,
#              logged in); a pre-release unless --final
#   --arch     what to build (default universal: Apple Silicon included, untested so far)
#   --notes    a Markdown file added to the release notes (what's new, known problems)
# The version is catalina32's own (cmd/catalina32/cli.go): change it there first.
set -euo pipefail
cd "$(dirname "$0")/.."
REPO="$PWD"

ARCH=universal NOTES_EXTRA="" PUBLISH=0 PRERELEASE=1
while [[ $# -gt 0 ]]; do
  case "$1" in
    --arch) ARCH="$2"; shift 2 ;;
    --notes) NOTES_EXTRA="$2"; shift 2 ;;
    --publish) PUBLISH=1; shift ;;
    --final) PRERELEASE=0; shift ;;
    *) sed -n '2,20p' "$0" >&2; exit 2 ;;
  esac
done
fail() { echo "make_release.sh: $*" >&2; exit 1; }

[[ "$(uname -s)" == "Darwin" ]] || fail "needs macOS"
[[ -z "$NOTES_EXTRA" || -f "$NOTES_EXTRA" ]] || fail "--notes: no such file: $NOTES_EXTRA"
BRANCH="$(git rev-parse --abbrev-ref HEAD)"
[[ "$BRANCH" == "master" ]] || fail "releases come from master (this is $BRANCH)"
[[ -z "$(git status --porcelain --untracked-files=no)" ]] || fail "commit or set aside your changes first (git status)"
VERSION="$(go run ./cmd/catalina32 version)"
TAG="v$VERSION"
if git rev-parse -q --verify "refs/tags/$TAG" >/dev/null; then
  fail "tag $TAG exists already: change the version in cmd/catalina32/cli.go"
fi
if [[ "$PUBLISH" == 1 ]]; then
  command -v gh >/dev/null || fail "--publish needs the GitHub CLI (brew install gh)"
  gh auth status >/dev/null 2>&1 || fail "--publish: log in first (gh auth login)"
  if git ls-remote --exit-code --tags origin "refs/tags/$TAG" >/dev/null 2>&1; then
    fail "tag $TAG exists on GitHub already"
  fi
fi

echo "[release] Catalina32 $VERSION ($ARCH) from $(git rev-parse --short HEAD)"
scripts/build-app.sh --arch "$ARCH"
APP="$REPO/build/Catalina32.app"
ENGINE="$APP/Contents/Helpers/catalina32"

# Never ship the game's code: the AOT translation registers itself as registerAOTGenerated.
if strings -a "$ENGINE" | grep -q 'registerAOTGenerated'; then
  fail "the engine contains AOT-compiled game code: not releasing it"
fi
WANT=$(case "$ARCH" in universal) echo "x86_64 arm64" ;; native) uname -m ;; *) echo "$ARCH" ;; esac)
for a in $WANT; do
  for f in "$ENGINE" "$APP/Contents/MacOS/Catalina32"; do
    lipo -archs "$f" | tr ' ' '\n' | grep -qx "$a" || fail "$(basename "$f") lacks $a"
  done
done
codesign --verify --deep "$APP" || fail "the app's signature doesn't verify"

OUT="$REPO/build/release"
mkdir -p "$OUT"
ZIP="$OUT/Catalina32-$VERSION.zip"
NOTES="$OUT/NOTES-$VERSION.md"
[[ ! -e "$ZIP" ]] || fail "$ZIP exists already (from an earlier run?): move it away first"
ditto -c -k --keepParent "$APP" "$ZIP"

ARCHS_TEXT="Intel Macs"
[[ "$WANT" == *arm64* ]] && ARCHS_TEXT="Intel and Apple Silicon Macs (Apple Silicon hasn't been tested yet)"
{
  echo "Play 32-bit Mac games on modern macOS. Catalina32 runs the original game's code and makes"
  echo "a clickable app for each supported game you own. Supported games are listed in the"
  echo "[README](https://github.com/robertoffmoura/catalina32#supported-games)."
  echo
  echo "### Install"
  echo
  echo "1. Download **Catalina32-$VERSION.zip** below and unzip it."
  echo "2. Move *Catalina32.app* to Applications and open it. It isn't notarised by Apple yet, so the"
  echo "   first time macOS blocks it:"
  echo "   - macOS 15 and later: dismiss the warning, then *System Settings → Privacy & Security* →"
  echo "     **Open Anyway** next to Catalina32;"
  echo "   - macOS 10.15 to 14: right-click *Catalina32.app* → **Open**, then **Open** again."
  echo "3. Pick your game and click **Create App**, then open the game app it makes."
  echo
  echo "Requires macOS 10.15 or later. Runs on $ARCHS_TEXT. You need your own copy of each game;"
  echo "Catalina32 never copies or changes it."
  if [[ -n "$NOTES_EXTRA" ]]; then
    echo
    cat "$NOTES_EXTRA"
  fi
  PREV="$(git describe --tags --abbrev=0 --match 'v*' 2>/dev/null || true)"
  if [[ -n "$PREV" ]]; then
    echo
    echo "### Changes since $PREV"
    echo
    git log --no-merges --format='- %s' "$PREV..HEAD"
  fi
} > "$NOTES"

echo "[release] $ZIP ($(du -h "$ZIP" | cut -f1))"
echo "[release] $NOTES"
if [[ "$PUBLISH" != 1 ]]; then
  echo "[release] Not published. Check both, then run again with --publish (move the zip away first)."
  exit 0
fi

git tag -a "$TAG" -m "Catalina32 $VERSION"
git push origin "$TAG"
FLAGS=()
[[ "$PRERELEASE" == 1 ]] && FLAGS+=(--prerelease)
gh release create "$TAG" "$ZIP" --title "Catalina32 $VERSION" --notes-file "$NOTES" ${FLAGS[@]+"${FLAGS[@]}"}
echo "[release] Published $TAG"
