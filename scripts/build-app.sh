#!/usr/bin/env bash
# build-app.sh [--arch native|x86_64|arm64|universal] [--with-aot] [--out folder]
#
# Builds Catalina32.app (the wizard, and the runtime it copies into every game app) in
# build/ (or --out). Needs macOS with Go and the Xcode command-line tools.
#
#   --arch       native (default), x86_64, arm64, or universal (both in one app)
#   --with-aot   also compile the AOT code generated from your own copy of the game
#                ($C32_BIN_PATH). For your own use only: it is a translation of the game's code,
#                so an app built with it must never be shared.
#   C32_SIGN_IDENTITY  sign with this Developer ID (default: ad hoc, for this Mac only)
set -euo pipefail
cd "$(dirname "$0")/.."
REPO="$PWD"

ARCH=native AOT=0 OUT="$REPO/build"
while [[ $# -gt 0 ]]; do
  case "$1" in
    --arch) ARCH="$2"; shift 2 ;;
    --with-aot) AOT=1; shift ;;
    --out) OUT="$2"; shift 2 ;;
    *) sed -n '2,12p' "$0" >&2; exit 2 ;;
  esac
done
if [[ "$(uname -s)" != "Darwin" ]]; then
  echo "build-app.sh: needs macOS (the window app uses Cocoa and Metal)" >&2
  exit 1
fi
case "$ARCH" in
  native) [[ "$(uname -m)" == "arm64" ]] && ARCHS=(arm64) || ARCHS=(x86_64) ;;
  x86_64|arm64) ARCHS=("$ARCH") ;;
  universal) ARCHS=(x86_64 arm64) ;;
  *) echo "build-app.sh: unknown --arch $ARCH" >&2; exit 2 ;;
esac

AOT_FILE="$REPO/pkg/games/lswsaga/aot_gen.go"
if [[ "$AOT" == 1 ]]; then
  : "${C32_BIN_PATH:?--with-aot needs C32_BIN_PATH, the path to the game executable}"
  (cd pkg/games/lswsaga && go generate .)
elif [[ -f "$AOT_FILE" ]]; then
  # Never let generated game code into an app by accident: build without it.
  if ! grep -q '^//go:build !noaot' "$AOT_FILE"; then
    echo "build-app.sh: $AOT_FILE is from an older c32aot and can't be left out of the build." >&2
    echo "  Regenerate it (run_on_mac.sh does, or: cd pkg/games/lswsaga && go generate .), or use --with-aot." >&2
    exit 1
  fi
  echo "build-app.sh: building without $AOT_FILE (use --with-aot to include it)" >&2
  AOT_TAGS=(-tags noaot)
fi

WORK="$(mktemp -d)"
trap 'rm -r "$WORK"' EXIT
APP="$WORK/Catalina32.app"
mkdir -p "$APP/Contents/MacOS" "$APP/Contents/Helpers" "$APP/Contents/Resources"

echo "[build-app] engine (${ARCHS[*]})"
ENGINES=() WINDOWS=()
for a in "${ARCHS[@]}"; do
  goarch=$([[ "$a" == x86_64 ]] && echo amd64 || echo arm64)
  # (Intermediate names must differ by more than case: macOS file systems usually ignore it.)
  CGO_ENABLED=0 GOOS=darwin GOARCH=$goarch go build ${AOT_TAGS[@]+"${AOT_TAGS[@]}"} -trimpath -o "$WORK/engine-$a" ./cmd/catalina32
  ENGINES+=("$WORK/engine-$a")

  echo "[build-app] window app ($a)"
  flags=(-arch "$a" -mmacosx-version-min=10.15 -O2 -I viewer)
  for f in C32Metal C32GL C32Launch C32Wizard; do
    clang "${flags[@]}" -fobjc-arc -c "viewer/$f.m" -o "$WORK/$f-$a.o"
  done
  clang "${flags[@]}" -ObjC -fno-objc-arc viewer/C32Window.m "$WORK"/C32Metal-$a.o "$WORK"/C32GL-$a.o "$WORK"/C32Launch-$a.o "$WORK"/C32Wizard-$a.o \
    -o "$WORK/window-$a" -framework Cocoa -framework QuartzCore -framework Metal -framework OpenGL -framework AudioToolbox -framework IOKit
  WINDOWS+=("$WORK/window-$a")
done
lipo -create "${ENGINES[@]}" -output "$APP/Contents/Helpers/catalina32"
lipo -create "${WINDOWS[@]}" -output "$APP/Contents/MacOS/Catalina32"
VERSION="$("$APP/Contents/Helpers/catalina32" version 2>/dev/null || echo 0)"

cat > "$APP/Contents/Info.plist" <<PLIST
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>CFBundleExecutable</key>
	<string>Catalina32</string>
	<key>CFBundleIdentifier</key>
	<string>com.ffmoura.catalina32</string>
	<key>CFBundleName</key>
	<string>Catalina32</string>
	<key>CFBundlePackageType</key>
	<string>APPL</string>
	<key>CFBundleShortVersionString</key>
	<string>$VERSION</string>
	<key>LSMinimumSystemVersion</key>
	<string>10.15</string>
	<key>NSHighResolutionCapable</key>
	<true/>
	<key>LSApplicationCategoryType</key>
	<string>public.app-category.games</string>
	<key>CFBundleDocumentTypes</key>
	<array>
		<dict>
			<key>CFBundleTypeName</key>
			<string>Game</string>
			<key>CFBundleTypeRole</key>
			<string>Viewer</string>
			<key>LSHandlerRank</key>
			<string>None</string>
			<key>LSItemContentTypes</key>
			<array>
				<string>com.apple.application-bundle</string>
				<string>public.folder</string>
			</array>
		</dict>
	</array>
</dict>
</plist>
PLIST

if [[ -n "${C32_SIGN_IDENTITY:-}" ]]; then
  codesign --force --deep --options runtime --timestamp --sign "$C32_SIGN_IDENTITY" "$APP"
else
  codesign --force --deep --sign - "$APP"
fi

mkdir -p "$OUT"
DEST="$OUT/Catalina32.app"
if [[ -e "$DEST" ]]; then
  if [[ ! -x "$DEST/Contents/Helpers/catalina32" ]]; then
    echo "build-app.sh: $DEST exists and isn't a Catalina32 build; not replacing it" >&2
    exit 1
  fi
  rm -r "$DEST"
fi
mv "$APP" "$DEST"
echo "[build-app] $DEST ($VERSION, ${ARCHS[*]}$([[ "$AOT" == 1 ]] && echo ', with AOT code: for your own use only'))"
