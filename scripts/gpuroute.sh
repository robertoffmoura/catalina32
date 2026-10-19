#!/usr/bin/env bash
# gpuroute.sh EXE GPUCHECK OUTDIR ROUTE MAXSTEPS [VAR=value...]
#
# runroute.sh with cmd/gpucheck standing in for the Metal viewer, so the engine takes the GPU
# path and every command it streams is validated. GPUCHECK is a built gpucheck binary; it is
# copied into OUTDIR under a unique name and stopped by PID, so concurrent gpuroute runs do
# not stop each other's checker. Prints gpucheck's summary (also in OUTDIR/check.txt).
set -euo pipefail
if [[ $# -lt 5 ]]; then
  sed -n '2,8p' "$0" >&2
  exit 2
fi
EXE=$1 CHECK=$2 OUT=$3 ROUTE=$4 STEPS=$5
shift 5
HERE="$(cd "$(dirname "$0")" && pwd)"
STAGE="$(mktemp -d)" # gpucheck needs the shm path before OUTDIR exists (runroute.sh creates it)
cp "$CHECK" "$STAGE/gpucheck-$$"
"$STAGE/gpucheck-$$" -shm "$STAGE/gpu.shm" >"$STAGE/check.txt" 2>&1 &
CHK=$!
trap 'kill -INT $CHK 2>/dev/null || true' EXIT
"$HERE/runroute.sh" "$EXE" "$OUT" "$ROUTE" "$STEPS" C32_GPU_SHM="$STAGE/gpu.shm" "$@"
kill -INT $CHK 2>/dev/null || true
wait $CHK 2>/dev/null || true
trap - EXIT
mv "$STAGE/check.txt" "$OUT/check.txt"
rm -f "$STAGE/gpu.shm" "$STAGE/gpucheck-$$" && rmdir "$STAGE"
command grep -v '^gpucheck: frame ' "$OUT/check.txt" || true
