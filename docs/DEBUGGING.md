# Debugging and verification

Recipes first, then a reference of every environment variable. Paths assume the dev container
(`/workspace`); on the Mac the same tree is `~/code/rea_steam`.

## Build

```sh
go build -o /tmp/c32 ./cmd/catalina32           # the engine
go build -o /tmp/gpucheck ./cmd/gpucheck        # the stand-in viewer
C32_BIN_PATH=".../LEGO Star Wars Saga" go generate ./pkg/games/lswsaga   # AOT code (optional)
```

`run_on_mac.sh` (outside the repository) does all of this on the Mac and builds the viewer.

## Headless runs

```sh
scripts/runroute.sh EXE OUTDIR ROUTE MAXSTEPS [VAR=value...]          # software renderer
scripts/gpuroute.sh EXE GPUCHECK OUTDIR ROUTE MAXSTEPS [VAR=value...]  # GPU path + gpucheck
```

- **Routes** (`C32_AUTO_KEYS`, scripted keys in `pkg/games/lswsaga/input_script.go`):

  | Route | What it does |
  |---|---|
  | `boot` | title → main menu → New Game → save slot 1 → the Cantina |
  | `episode1` | `boot`, then walk into Episode I Chapter 1 (Negotiations) |
  | `explore` | `episode1`, then walk around the first room |
  | `outside` | `boot`, then walk out into the Mos Eisley courtyard |
  | `cantina` | `boot`, then attack in place (`C32_ATTACK_KEY`, default H) |
  | `loop` | periodic Enter/F2 |
  | `off` | no scripted input |

- **Budgets.** `MAXSTEPS` is guest instructions. On the `episode1` route: about 1.1 billion
  reaches the Cantina, 1.9 billion is about frame 1140 (the opening crawl), and 2.6 billion is
  about frame 1350 (inside Negotiations, where the level's bloom passes run). A run that long
  takes several minutes, so start it in the background.
- **Determinism check.** Headless runs are exactly reproducible. Two builds behave the same if
  their `Steps=` lines match:

  ```sh
  diff <(grep -o 'Frame #[0-9]* .*Steps=[0-9]*' A/log.txt | sed 's/(.*Steps/Steps/') \
       <(grep -o 'Frame #[0-9]* .*Steps=[0-9]*' B/log.txt | sed 's/(.*Steps/Steps/')
  ```

  The log prints a frame line every 30 frames. Rendering changes don't alter these counts;
  anything the game can observe (a COM method's results, the clock, input) can.
- **gpucheck's summary** reports: frames, draws, clears, target switches, libraries, bytes per
  frame, texture uploads by format (cube and mip-chain flags), cube-sampler bindings, scissor,
  partial-viewport and fog draws, and **errors**. Anything other than 0 errors after a
  stream-format change usually means `cmd/gpucheck`'s parser is out of date.

## Seeing what was drawn

- **Software renderer PNGs.** Headless runs write `OUTDIR/frames/frame_NNNN.png`: every 5th
  frame once in the game, plus `latest.png`. `C32_RENDER_EVERY=1` exports every rendered frame.
- **Draw-level log:** `C32_DRAW_LOG=<frame>[,<frame>...]` (`pkg/d3d9/drawlog.go`). For each
  listed frame it prints:
  - every draw: shaders, textures, blend/depth/alpha state, first vertex;
  - render-target switches with guest call sites, surface origins;
  - device calls such as `SetViewport`, `StretchRect` and `Clear`, with arguments;
  - per-draw pixel footprints and overdraw (`[DRAWCOST]`).

  It also dumps the frame's textures as PNGs. Footprints show which draw painted a pixel:
  look up a wrong pixel's coordinates there rather than guessing from addresses.
- **Shader sources:** `C32_DUMP_SHADERS=<dir>` writes every HLSL source given to
  `D3DXCompileShader`. Those dumps feed `HLSL_TEST_DIR` (below).
- **Viewer-side failures:** MSL compile and pipeline errors are logged by the viewer as
  `[Metal] …`, with the failing sources saved in
  `/tmp/catalina32-mac-preview/msl_errors/`. Their count is reported to the engine log as
  `[GPU] the viewer reported N …`.

## Tests

```sh
go test ./...                                    # everything that needs no game files
C32_BIN_PATH=".../LEGO Star Wars Saga" go test ./pkg/d3dsm ./pkg/games/lswsaga ./pkg/d3d9
HLSL_TEST_DIR=<C32_DUMP_SHADERS dir> go test ./pkg/hlsl -run TestMSLMatchesEvaluator
LZHUF_TEST_DIR=<C32_DUMP_LZHUF dir> C32_BIN_PATH=... go test ./pkg/games/lswsaga -run TestLZHUFMatchesGuest
```

- **Translators.** The MSL tests compile the generated Metal code as C++20 with clang,
  against `pkg/hlsl/testdata/metal_stub.h`, run it on random inputs, and compare with the
  evaluators. When generated code uses a new MSL function, add it to the stub.
  `TestFixedFunctionFog` runs the fog stage for every mode.
- **Other notable tests:**
  - `cmd/c32aot`: random x86 snippets through the interpreter, the decoded-instruction cache
    and the compiled code.
  - `TestLiftedMatchGuest`: lifted functions against the game's own code.
  - `TestWideMatchesScalar`: the batched pixel-shader evaluator against the scalar one.

## Profiling

- **Host CPU profile:** `C32_CPUPROF=<file>`. Combine with `C32_CPUPROF_AFTER=<seconds>` or
  `C32_CPUPROF_FRAMES=<first>-<last>` to profile one scene. Episode I route scenes: crawl
  ≈ 900–1100, cutscene 1150–1225, Negotiations 1250+.
- **Guest profile:** `C32_GUEST_PROF=<from step>` samples the game's call stack and prints the
  top functions at exit (`C32_PROF_TOP=N`). This is how the AOT function list in
  `pkg/games/lswsaga/aot.go` was chosen.
- **Live mode** prints FPS, x86 instructions per second and the share of time spent waiting
  for the renderer every 2 s.

## Throwaway probes

To count or print something once, build from a scratch copy so nothing leaks into the tree:

```sh
git archive HEAD | tar -x -C /tmp/probe && cp pkg/games/lswsaga/aot_gen.go /tmp/probe/pkg/games/lswsaga/
# edit /tmp/probe, then: (cd /tmp/probe && go build -o /tmp/c32probe ./cmd/catalina32)
```

## Environment variables

**Engine**

| Variable | Effect |
|---|---|
| `C32_BIN_PATH` | the game executable (or pass `-game`) |
| `C32_DATA_ROOT` | game data folder (default: the profile's folder next to the `.app`) |
| `C32_SAVE_ROOT` | save folder (default `~/Library/Application Support/catalina32/<profile's SaveDir, else its id>/SaveData`; for this game `.../catalina32/LEGO Star Wars Saga/SaveData`, as `run_on_mac.sh` sets it) |
| `C32_PROFILE` | force a game profile id instead of detecting it by SHA-256 |
| `C32_AUTO_KEYS` | scripted input route (see above); `off` for none |
| `C32_ATTACK_KEY` | key code (hex) the `cantina` route presses |
| `C32_MAX_STEPS` | stop after N guest instructions (0: no limit) |
| `C32_LIVE` | `1`: interactive session (wall clock, no step limit, quieter log) |
| `C32_SHM_FILE` | framebuffer/input/audio shared memory for the viewer |
| `C32_GPU_SHM` | GPU command stream shared memory; the GPU path runs once a viewer maps it |
| `C32_FRAMES_DIR` | where PNG frames go |
| `C32_RENDER_EVERY` | export every Nth rendered frame |
| `C32_RENDER_THREADS` | software rasterizer workers (default GOMAXPROCS−1) |
| `C32_SYNC_RENDER` | `1`: draw each frame before continuing (A/B tests of the pipelined renderer) |
| `C32_REAL_SHADERS` | `0`: legacy heuristic renderer instead of the game's shaders |
| `C32_REAL_HLSL` | `1`: run the engine's own HLSL generator even with real shaders off |
| `C32_PORT` | HTTP preview port (browser viewer); `0` picks a free one |
| `C32_NO_AOT` | `1`: don't install the AOT-compiled functions |
| `C32_NO_DCACHE` | `1`: disable the decoded-instruction cache |
| `C32_NO_NATIVE_LZHUF` | `1`: decompress level data with the game's own (emulated) decoder |

**Diagnostics**

| Variable | Effect |
|---|---|
| `C32_DRAW_LOG` | per-draw log for the listed frames |
| `C32_DUMP_SHADERS` | dump HLSL sources |
| `C32_DUMP_LZHUF` | save every compressed block the game decompresses (`NNNNN_<size>.lh5`) |
| `C32_CPUPROF`, `C32_CPUPROF_AFTER`, `C32_CPUPROF_FRAMES` | host CPU profile |
| `C32_GUEST_PROF`, `C32_PROF_TOP` | guest call-stack sampling |
| `C32_TRACE_CALLS` | track guest calls (slow) |
| `C32_WATCH` | `lo-hi`: log the first access to a guest address range from each instruction |
| `C32_VERIFY_MDCT` | run the native MDCT and the game's side by side and compare |

**Viewer (`C32Window`, `C32Metal.m`)**

| Variable | Effect |
|---|---|
| `C32_RENDERER` | `software`: show the software framebuffer instead of Metal (`run_on_mac.sh` then doesn't set `C32_GPU_SHM`) |
| `C32_METAL_WINDING` | `ccw`: flip the front-face convention |
| `C32_METAL_SYNC_SHADERS` | `1`: compile shaders on the render thread, waiting for each (by default they compile in the background and draws needing one are skipped until it is ready) |
| `C32_PAD_DEBUG` | `1`: print raw controller events |

**Tests**

| Variable | Effect |
|---|---|
| `HLSL_TEST_DIR` | a `C32_DUMP_SHADERS` directory for the HLSL tests |
| `LZHUF_TEST_DIR` | a `C32_DUMP_LZHUF` directory for `TestLZHUFMatchesGuest` |
| `C32_BIN_PATH` | enables tests that read shaders or code from the game executable |
