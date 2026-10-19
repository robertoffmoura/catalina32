#!/usr/bin/env bash
# runroute.sh EXE OUTDIR ROUTE MAXSTEPS [VAR=value...]
#
# A headless, deterministic run of the game: EXE (a catalina32 binary) plays the scripted
# input ROUTE (C32_AUTO_KEYS: boot, episode1, explore, outside, cantina, loop, off) for
# MAXSTEPS guest instructions. OUTDIR must not exist yet (one fresh directory per run); it
# receives log.txt, frames/ (PNG exports) and save/ (an empty save folder, so runs do not
# share saves). Extra VAR=value arguments are added to the environment. The game is looked up
# next to the repository (../Lego Star Wars Saga) unless C32_GAME_DIR says otherwise.
set -euo pipefail
if [[ $# -lt 4 ]]; then
  sed -n '2,10p' "$0" >&2
  exit 2
fi
EXE=$1 OUT=$2 ROUTE=$3 STEPS=$4
shift 4
GAME_DIR="${C32_GAME_DIR:-$(cd "$(dirname "$0")/../.." && pwd)/Lego Star Wars Saga}"
mkdir "$OUT"
mkdir "$OUT/frames" "$OUT/save"
env C32_BIN_PATH="$GAME_DIR/LEGO Star Wars Saga.app/Contents/MacOS/LEGO Star Wars Saga" \
  C32_DATA_ROOT="$GAME_DIR/LEGOStarWarsSagaData" C32_SAVE_ROOT="$OUT/save" \
  C32_FRAMES_DIR="$OUT/frames" C32_PORT=0 C32_AUTO_KEYS="$ROUTE" C32_MAX_STEPS="$STEPS" \
  "$@" "$EXE" >"$OUT/log.txt" 2>&1 || true
echo "done: $OUT/log.txt"
