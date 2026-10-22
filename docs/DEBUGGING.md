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

- **Source games (OpenGL): glreplay.** `tools/glreplay` plays a headless run's GL stream
  through the window's own decoding code (`viewer/C32GL.m`, built with `C32GL_REPLAY`) into
  Mesa's software OpenGL, and saves frames as PNGs. Build it with
  `tools/glreplay/build.sh <out> [mesa root]` (OSMesa: e.g. `libosmesa6`, `libosmesa6-dev`,
  `mesa-common-dev`, `libgl-dev`, unpacked with `dpkg -x` when not installed). Then:

  ```
  glreplay /tmp/gl.shm /tmp/shots every:20 &
  C32_GL_SHM=/tmp/gl.shm C32_SOURCE_DIR=<Portal> C32_STEPS=11000000000 \
    C32_ARGS="+sv_cheats 1 +map testchmb_a_00" \
    C32_CMDS="8000000000:ent_fire portal_red_0 SetActivatedState 1;setang 0 270 0" \
    go test ./pkg/source -run 'TestBoot$' -v
  ```

  `C32_CMDS` types console commands in at instruction counts (`steps:cmd;cmd|steps:...`).
  Mesa is not the Mac's GL: a picture right here and wrong on a Mac points at the Mac's
  driver or the window, not the stream. Some Mac drivers can be imitated:
  `GLREPLAY_CLIP=position` clips against `gl_Position` whatever the shader writes to
  `gl_ClipVertex`, as AMD's Mac drivers do, and reports an AMD GPU (portals on AMD Macs drew
  without walls and floors before the engine told ToGL the GPU's vendor).
  `GLREPLAY_SCREEN=WxH` is the screen size the game sees (default 1440x900). `C32_GLTRACE=<file>` (with `C32_GLCHECK=1`) writes
  every GL command for diffing two runs.

## Tests

```sh
go test ./...                                    # everything that needs no game files
C32_BIN_PATH=".../LEGO Star Wars Saga" go test ./pkg/d3dsm ./pkg/games/lswsaga ./pkg/d3d9
HLSL_TEST_DIR=<C32_DUMP_SHADERS dir> go test ./pkg/hlsl -run TestMSLMatchesEvaluator
LZHUF_TEST_DIR=<C32_DUMP_LZHUF dir> C32_BIN_PATH=... go test ./pkg/crt -run LZHUF
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
  - `TestJITMatchesInterpreter` (`pkg/cpu`): random programs, interpreter against JIT; the
    arm64 backend runs it under QEMU (`docs/JIT.md`, Testing).

## Profiling

- **Host CPU profile:** `C32_CPUPROF=<file>`. Combine with `C32_CPUPROF_AFTER=<seconds>` or
  `C32_CPUPROF_FRAMES=<first>-<last>` to profile one scene. Episode I route scenes: crawl
  ≈ 900–1100, cutscene 1150–1225, Negotiations 1250+.
- **Guest profile:** `C32_GUEST_PROF=<from step>` samples the game's call stack and prints the
  top functions at exit (`C32_PROF_TOP=N`). This is how the AOT function list in
  `pkg/games/lswsaga/aot.go` was chosen.
- **Live mode** prints FPS, x86 instructions per second and the share of time spent waiting
  for the renderer every 2 s.
- **Profile-guided optimisation:** `cmd/catalina32/default.pgo` is a CPU profile that `go build`
  uses automatically (hot paths inlined more aggressively; about 9% faster on the GPU path).
  Refresh it after large changes to `pkg/cpu`, from an interpreter-only GPU-path run (no
  `aot_gen.go`): `scripts/gpuroute.sh EXE GPUCHECK OUT cantina 1300000000 C32_CPUPROF=<file>`,
  then copy the file over `default.pgo`.
- **Replacing a hot function natively:** `go run ./cmd/lift -start A -end B` disassembles it;
  `go run ./cmd/sigmake -start A -end B [-list]` prints a signature for `pkg/profile` (with
  wildcards over operands that move between builds) and where its globals are addressed, so
  the replacement binds by signature in any binary that has the same code.

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
| `C32_NO_JIT` | `1`: no JIT (interpreter and block cache only) |
| `C32_JIT` | `1`: the JIT where it is opt-in (Apple Silicon, until verified there) |
| `C32_NO_NATIVE_LZHUF` | `1`: decompress level data with the game's own (emulated) decoder |

**Diagnostics**

| Variable | Effect |
|---|---|
| `C32_DRAW_LOG` | per-draw log for the listed frames |
| `C32_DUMP_SHADERS` | dump HLSL sources |
| `C32_DUMP_LZHUF` | save every compressed block the game decompresses (`NNNNN_<size>.lh5`) |
| `C32_CPUPROF`, `C32_CPUPROF_AFTER`, `C32_CPUPROF_FRAMES` | host CPU profile |
| `C32_GUEST_PROF`, `C32_PROF_TOP` | guest call-stack sampling |
| `C32_TRACE_CALLS` | track guest calls (slow; switches the JIT off) |
| `C32_JITPROF` | `1`: count runs of translated blocks (what their code costs: see `docs/JIT.md`) |
| `C32_WATCH` | `lo-hi`: log the first access to a guest address range from each instruction |
| `C32_VERIFY_MDCT` | run the native MDCT and the game's side by side and compare |

**Viewer (`C32Window`, `C32Metal.m`, `C32GL.m`)**

| Variable | Effect |
|---|---|
| `C32_RENDERER` | `software`: show the software framebuffer instead of Metal (`run_on_mac.sh` then doesn't set `C32_GPU_SHM`) |
| `C32_HUD` | `1`: a line of stats above the game in the developer window (frame, FPS, draw calls, triangles, instructions, controllers) |
| `C32_METAL_WINDING` | `ccw`: flip the front-face convention |
| `C32_METAL_SYNC_SHADERS` | `1`: compile shaders on the render thread, waiting for each (by default they compile in the background and draws needing one are skipped until it is ready) |
| `C32_PAD_DEBUG` | `1`: print raw controller events |
| `C32_GL_DEBUG` | `1` (Source games, `C32GL.m`): check for a GL error after every call and log it with its arguments |

**Source games** (`pkg/source`; `go test ./pkg/source -run <Test> -v`)

| Variable | Effect |
|---|---|
| `C32_SOURCE_DIR` | the game folder (the one holding `hl2_osx`); enables the game tests |
| `C32_STEPS`, `C32_LOG`, `C32_ARGS` | `TestBoot`: instructions to run, the game's log file, game options (e.g. `+map testchmb_a_00`) |
| `C32_OVERLAY` | the folder the game writes to (default: a fresh one per run); keep one to test saved settings or saves |
| `C32_GLCHECK`, `C32_GLSTATS` | `TestBoot`: consume the GL stream with `RunGLCheck` (protocol checks); print GL call counts |
| `C32_GLTRACE` | with `C32_GLCHECK`: write every GL command (name, hash, bytes or arguments, shader sources) to a file, to diff two runs |
| `C32_GL_SHM` | the stream file; with `TestBoot` and no `C32_GLCHECK`, another viewer (e.g. `glreplay`) reads it |
| `C32_VIEWER_SHM` | tests using `withWindow` (`TestPlayIO`, `TestController`...): the stream file for another viewer instead of the built-in checker |
| `C32_CMDS` | console commands typed in at instruction counts: `steps:cmd;cmd\|steps:...` (`TestBoot`, `TestController`) |
| `C32_CTL_LOAD` | `TestController`: load this save (in `C32_OVERLAY`) from the menu first |
| `C32_PROBE`, `C32_WATCH_NULL`, `C32_TRACECALLS` | log calls and returns of matching guest functions; catch NULL-pointer stores; keep a call stack for backtraces |
| `C32_FONT_PNG`, `C32_MP3_OUT` | write a font sample (`HostFontSample`); the menu music's decoded PCM (`TestMenuSound`) |
| `C32_LIVE` | `1`: wall clock (set by `catalina32 source`) |
| `C32_JITSTATS`, `C32_JITPROF`, `C32_JITDUMP`, `C32_SLOWOPS` | `TestBoot`: JIT counters (`C32_JITSTATS=<instruction>`: from then on, returns to Go by what runs where they go, the instructions handed to the interpreter, the blocks translated; `C32_SLOWOPS` counts from there too), per-block costs, a block's host code, the forms left to the interpreter's full decoder (`docs/JIT.md`) |
| `C32_FRAMESTATS` | `TestBoot`: a CSV line per frame: instructions, those the full decoder ran, host and CPU milliseconds, host calls, GL commands and KB, JIT slow and end exits, resets, milliseconds waiting for the GL stream's reader, blocks translated, and (`C32_INJIT=1`) the share of the frame translated code ran (sampled; the CPU profiler loses most samples taken in translated code) |
| `C32_NATIVE_TRACE`, `C32_TRACECHECK` | `0`: Source's collision traces run as the game's code; `1`: run every trace both ways and compare (`TestBoot` reports the counts; `pkg/source/cmtrace.go`) |
| `C32_NATIVE_DSP`, `C32_DSPCHECK` | `0`: Source's sound effects run as the game's code; `1`: run them both ways and compare every call (`TestBoot` reports the counts; `pkg/source/snddsp.go`) |
| `C32_CPUPROF`, `C32_CPUPROF_AFTER` | `TestBoot`: the host's CPU profile, from that instruction count on (e.g. after loading) |
| `GLREPLAY_CLIP`, `GLREPLAY_SCREEN` | `tools/glreplay`: `position` imitates AMD's clipping (and reports an AMD GPU); the screen size the game sees |

**Tests**

| Variable | Effect |
|---|---|
| `HLSL_TEST_DIR` | a `C32_DUMP_SHADERS` directory for the HLSL tests |
| `LZHUF_TEST_DIR` | a `C32_DUMP_LZHUF` directory for `TestLZHUFMatchesGuest` |
| `C32_JITN`, `C32_JITSEED` | `TestJITMatchesInterpreter`: how many random programs; `TestJITBisect`: the seed whose first difference to find |
| `C32_JITPLAIN`, `C32_JITFROM` | `1`: compare every program with the full decoder alone too (TestJITBisect: that comparison); the instruction count TestJITBisect starts at |
| `C32_BIN_PATH` | enables tests that read shaders or code from the game executable |
