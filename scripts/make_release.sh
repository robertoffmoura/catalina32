#!/usr/bin/env bash
# make_release.sh [--arch universal|native|x86_64|arm64] [--notes file]
# make_release.sh --publish [--final]
#
# Makes a Catalina32 release from a clean master checkout (macOS, Go, Xcode command-line tools),
# in two steps:
#
# 1. Without --publish: builds build/Catalina32.app with scripts/build-app.sh (never with the
#    AOT code), refuses an engine that contains AOT-compiled game code (a translation of the
#    game's code, which must never be shared), checks the architectures and the signature, and
#    writes into build/release/:
#      Catalina32-<version>.zip   the app (ditto keeps the signature)
#      NOTES-<version>.md         the release notes: edit them before publishing if you like
#      STAMP-<version>            the commit and the zip's checksum
#    Nothing leaves this Mac. Running it again replaces these files.
#      --arch   what to build (default universal: Apple Silicon included, untested so far)
#      --notes  a Markdown file added to the notes (what's new, known problems)
#
# 2. --publish: publishes exactly that zip and those notes, without rebuilding, once it has
#    checked they were made from the current commit and the zip is unchanged: tags v<version>,
#    pushes the tag and creates a GitHub release (needs gh, logged in), a pre-release unless
#    --final.
#
# The version is catalina32's own (cmd/catalina32/cli.go): bump it there before each release.
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
    *) sed -n '2,25p' "$0" >&2; exit 2 ;;
  esac
done
fail() { echo "make_release.sh: $*" >&2; exit 1; }
VERSION_HINT="[release] The version lives in cmd/catalina32/cli.go; bump it there before each later release."

[[ "$(uname -s)" == "Darwin" ]] || fail "needs macOS"
BRANCH="$(git rev-parse --abbrev-ref HEAD)"
[[ "$BRANCH" == "master" ]] || fail "releases come from master (this is $BRANCH)"
[[ -z "$(git status --porcelain --untracked-files=no)" ]] || fail "commit or set aside your changes first (git status)"
COMMIT="$(git rev-parse HEAD)"
VERSION="$(go run ./cmd/catalina32 version)"
TAG="v$VERSION"
if git rev-parse -q --verify "refs/tags/$TAG" >/dev/null; then
  fail "tag $TAG exists already: change the version in cmd/catalina32/cli.go"
fi

OUT="$REPO/build/release"
ZIP="$OUT/Catalina32-$VERSION.zip"
NOTES="$OUT/NOTES-$VERSION.md"
STAMP="$OUT/STAMP-$VERSION"
zipsum() { shasum -a 256 "$ZIP" | cut -d' ' -f1; }

if [[ "$PUBLISH" == 1 ]]; then
  # Step 2: publish what step 1 made, as it is.
  [[ -f "$ZIP" && -f "$NOTES" && -f "$STAMP" ]] || fail "nothing to publish for $VERSION: run scripts/make_release.sh (without --publish) first"
  STAMP_COMMIT="$(sed -n 's/^commit //p' "$STAMP")"
  STAMP_SUM="$(sed -n 's/^sha256 //p' "$STAMP")"
  [[ "$STAMP_COMMIT" == "$COMMIT" ]] || fail "the zip was built from ${STAMP_COMMIT:0:7}, not the current commit ${COMMIT:0:7}: run scripts/make_release.sh again first"
  [[ "$(zipsum)" == "$STAMP_SUM" ]] || fail "the zip changed since it was built: run scripts/make_release.sh again first"
  command -v gh >/dev/null || fail "--publish needs the GitHub CLI (brew install gh)"
  gh auth status >/dev/null 2>&1 || fail "--publish: log in first (gh auth login)"
  if git ls-remote --exit-code --tags origin "refs/tags/$TAG" >/dev/null 2>&1; then
    fail "tag $TAG exists on GitHub already"
  fi
  echo "[release] Publishing $ZIP ($(du -h "$ZIP" | cut -f1)) as $TAG"
  git tag -a "$TAG" -m "Catalina32 $VERSION"
  git push origin "$TAG"
  FLAGS=()
  if [[ "$PRERELEASE" == 1 ]]; then FLAGS+=(--prerelease); fi
  gh release create "$TAG" "$ZIP" --title "Catalina32 $VERSION" --notes-file "$NOTES" ${FLAGS[@]+"${FLAGS[@]}"}
  echo "[release] Published $TAG"
  echo "$VERSION_HINT"
  exit 0
fi

# Step 1: build, check, zip, write the notes.
[[ -z "$NOTES_EXTRA" || -f "$NOTES_EXTRA" ]] || fail "--notes: no such file: $NOTES_EXTRA"
echo "[release] Catalina32 $VERSION ($ARCH) from ${COMMIT:0:7}"
scripts/build-app.sh --arch "$ARCH"
APP="$REPO/build/Catalina32.app"
ENGINE="$APP/Contents/Helpers/catalina32"

# Never ship the game's code: the AOT translation registers itself as registerAOTGenerated.
if strings -a "$ENGINE" | grep -q 'registerAOTGenerated'; then
  fail "the engine contains AOT-compiled game code: not releasing it"
fi
# (No case inside $(...): macOS's bash 3.2 can't parse it.)
if [[ "$ARCH" == universal ]]; then
  WANT="x86_64 arm64"
elif [[ "$ARCH" == native ]]; then
  WANT="$(uname -m)"
else
  WANT="$ARCH"
fi
for a in $WANT; do
  for f in "$ENGINE" "$APP/Contents/MacOS/Catalina32"; do
    lipo -info "$f" | grep -qw "$a" || fail "$(basename "$f") lacks $a"
  done
done
codesign --verify --deep "$APP" || fail "the app's signature doesn't verify"

mkdir -p "$OUT"
rm -f "$ZIP" "$NOTES" "$STAMP" # this version's files from an earlier run, if any
ditto -c -k --keepParent "$APP" "$ZIP"

ARCHS_TEXT="Intel Macs"
if [[ "$WANT" == *arm64* ]]; then ARCHS_TEXT="Intel and Apple Silicon Macs (Apple Silicon hasn't been tested yet)"; fi
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
printf 'commit %s\nsha256 %s\narch %s\n' "$COMMIT" "$(zipsum)" "$ARCH" > "$STAMP"

echo "[release] $ZIP ($(du -h "$ZIP" | cut -f1))"
echo "[release] $NOTES"
echo "[release] Not published. Check both (edit the notes if you like), then: scripts/make_release.sh --publish"
echo "$VERSION_HINT"
