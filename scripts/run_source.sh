#!/usr/bin/env bash
# run_source.sh [GAME_FOLDER] [game options...]
#
# Builds the engine and the window app from this checkout and plays a Valve Source game
# (experimental; docs/PORTAL.md) from its folder, the one holding hl2_osx: by default the Portal
# folder next to the repository. macOS only. The engine's log goes to the terminal; what the
# game writes (settings, saves) goes to ~/Library/Application Support/catalina32/<folder name>.
set -euo pipefail
cd "$(dirname "$0")/.."
GAME="${1:-../Portal}"
[[ $# -gt 0 ]] && shift
if [[ "$(uname -s)" != Darwin ]]; then
  echo "run_source.sh: needs macOS (on Linux: go test ./pkg/source with C32_SOURCE_DIR and C32_GLCHECK=1)" >&2
  exit 2
fi
OUT="${TMPDIR:-/tmp}/catalina32-source"
mkdir -p "$OUT"
echo "[run_source] engine"
go build -o "$OUT/catalina32" ./cmd/catalina32
echo "[run_source] window"
flags=(-O2 -I viewer -mmacosx-version-min=10.15)
for f in C32Metal C32GL C32Launch C32Wizard; do
  clang "${flags[@]}" -fobjc-arc -c "viewer/$f.m" -o "$OUT/$f.o"
done
clang "${flags[@]}" -ObjC -fno-objc-arc viewer/C32Window.m "$OUT"/C32Metal.o "$OUT"/C32GL.o "$OUT"/C32Launch.o "$OUT"/C32Wizard.o \
  -o "$OUT/C32Window" -framework Cocoa -framework QuartzCore -framework Metal -framework OpenGL -framework AudioToolbox -framework IOKit
exec "$OUT/catalina32" source --window "$OUT/C32Window" "$GAME" "$@"
