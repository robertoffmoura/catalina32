#!/usr/bin/env bash
# build.sh <out> [mesa root]: builds glreplay (glreplay.c) against Mesa's OSMesa. The mesa root
# is a folder holding usr/include and usr/lib/<arch> (e.g. libosmesa6, libosmesa6-dev,
# mesa-common-dev and libgl-dev unpacked with dpkg -x), or / for the system's.
set -euo pipefail
cd "$(dirname "$0")"
OUT="$1"
ROOT="${2:-/}"
LIB=$(dirname "$(find "$ROOT/usr/lib" -name 'libOSMesa.so*' | head -1)")
clang -O2 -std=gnu11 -x c -I . -I "$ROOT/usr/include" -Wno-int-conversion -Wno-incompatible-pointer-types \
  -Wno-pointer-sign -Wno-unused-function glreplay.c -o "$OUT" -L "$LIB" -lOSMesa -Wl,--disable-new-dtags,-rpath,"$LIB" # RPATH: also for its own libraries
