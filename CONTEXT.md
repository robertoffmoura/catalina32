# catalina32: context for new sessions

Read this first, then `PROGRESS.md` (status and open work). `docs/ARCHITECTURE.md` describes
the product (Catalina32.app, game apps, the command line, saves) and its layers;
`docs/DEBUGGING.md` lists every switch and tool; `docs/LSWSAGA.md` holds what we know about
the game itself. `README.md` is for players.

## What this is

catalina32 runs 32-bit (i386) Mac games on 64-bit macOS, which dropped 32-bit support in
Catalina. It loads the original i386 Mach-O executable, emulates the x86 code, and replaces
the libraries underneath it with Go:

- **Feral's "MacDoze"** layer (a Win32 look-alike: files, threads, timers, registry).
- **"IndirectX"** layer (Direct3D 9, D3DX, DirectInput, DirectSound, XInput).

The first and so far only target is **LEGO Star Wars: The Complete Saga** (Feral's Mac port of
Traveller's Tales' NuEngine game). The tool is meant to be general: everything specific to
one game lives in `pkg/games/<id>`, and the other packages contain no game addresses.

**Goal:** make the game fully playable on a modern Mac.

**State (October 2026):** it boots, plays with sound, controllers and saves, and draws
through Metal at about 35–57 FPS on the user's Intel MacBook Pro. The whole of Episode I
Chapter 1 (Negotiations) has been played through without crashes. See `PROGRESS.md` for open
bugs.

## People, machines, paths

- **The user**: Roberto Moura. Git author for commits: `Roberto Moura <roberto@ffmoura.com>`.
  Commit often, one topic per commit. **Never push.** Don't touch `dist/` (the user's).
- **Dev container (Linux)**: `/workspace`. It has Go and clang but no Mac, no Metal and no
  screen. Everything Metal-side is verified indirectly (see "Verifying changes").
- **The user's Mac**: Intel i9 MacBook Pro. `/workspace` is the Mac's `~/code/rea_steam`,
  shared both ways, so files written here are visible there. There is no Apple Silicon Mac
  to test on, and the user has decided not to pursue that.
- **The game** (the user's own copy, never committed or uploaded):
  - `/workspace/Lego Star Wars Saga/LEGO Star Wars Saga.app/Contents/MacOS/LEGO Star Wars Saga`
    is the executable (`C32_BIN_PATH`).
  - `/workspace/Lego Star Wars Saga/LEGOStarWarsSagaData` is the data folder.
- **`/workspace/run_on_mac.sh`** is how the user plays. It sits outside the repository (not
  version-controlled). On the Mac it:
  1. generates the AOT code;
  2. builds the engine into `catalina32/bin/`;
  3. compiles the viewer (`viewer/*.m`) into `/tmp/catalina32-mac-preview/C32Window`;
  4. starts both.

  `--build-app` builds an old-style `.app`; `scripts/build-app.sh` (in the repository) builds
  Catalina32.app, the product (`docs/ARCHITECTURE.md`). Saves go to
  `~/Library/Application Support/catalina32/LEGO Star Wars Saga/SaveData` (`run_on_mac.sh` sets
  it); game apps use the original game's folder (`OriginalSaves` in the profile).
- **Branches.** `master` is the product. `debug` is `master` plus debugging-only tools (a
  session journal to record a live session and replay it headless, and Metal frame snapshots
  without a window), documented in `debug`'s copy of these files; it stays ahead of `master`.
- **Other Claude sessions** sometimes work in `/workspace/catalina32` at the same time (for
  example a session running on the Mac). Before editing, check `git status` and
  `git worktree list`.
  - If someone else has uncommitted work there, use your own worktree: `git worktree add -b
    <branch> /workspace/catalina32-<topic> <base>`.
  - Copy `pkg/games/lswsaga/aot_gen.go` into the worktree if you need the AOT code.
  - **Remove the worktree as soon as the work is committed** (`git worktree remove`,
    `git worktree prune`). A branch left checked out in a worktree can't be checked out by
    the user.

## Architecture

```
i386 Mach-O ──► pkg/macho32 (loader) ──► pkg/cpu (x86 interpreter + caches + AOT code)
                                              │ calls into the game's imports/boundary
                                              ▼ functions are intercepted (Go "thunks")
   pkg/crt (libc/libm, libvorbis fast paths)  pkg/win32 (MacDoze: files, green threads, clock)
   pkg/d3d9 (IndirectX: D3D9/D3DX/DInput/DSound/XInput, renderers)
                                              │
               ┌──────────────────────────────┴───────────────────────────────┐
     software renderer (raster.go, prog*.go)                  GPU command stream (gpu.go)
     → framebuffer shm / PNGs                                 → shared memory → viewer/C32Metal.m
               └──────────────► viewer/C32Window.m (Cocoa window, keyboard, controllers, audio) ◄┘
```

- **Three layers.**
  - Layer 1: `pkg/macho32`, `pkg/cpu`, `pkg/crt`. No Win32 or game knowledge.
  - Layer 2: `pkg/win32`, `pkg/d3d9`. The generic MacDoze and IndirectX implementations.
  - Layer 3: `pkg/games/<id>`. A `profile.Profile` maps well-known names (`CreateFileA`,
    `WinMain`, …) to this binary's addresses, and layer 2 attaches shims with
    `prof.Bind(c, "Name", fn)`.

  `pkg/games` picks the game by the executable's SHA-256 (or `C32_PROFILE`). Prefer
  implementing the standard D3D9/DirectSound/Win32 interface over hooking engine internals.
- **CPU (`pkg/cpu`)** has three tiers:
  1. an interpreter;
  2. a decoded-instruction block cache (`dcache.go`);
  3. hot game functions compiled ahead of time to Go by `cmd/c32aot` into
     `pkg/games/lswsaga/aot_gen.go`.

  `aot_gen.go` is a translation of the game's code: **never commit it** (it is gitignored).
  `go generate ./pkg/games/lswsaga` with `C32_BIN_PATH` set regenerates it, and the build
  works without it. Threads are cooperative green threads (`pkg/win32/threads.go`).
- **Shaders.** The game generates HLSL at runtime and compiles it with `D3DXCompileShader`.
  Its effects use precompiled vs_1_1/ps_1_x/ps_2_0 bytecode.
  - `pkg/hlsl` compiles that HLSL subset and evaluates it.
  - `pkg/d3dsm` decodes and runs the bytecode.
  - Both translate to Metal Shading Language (`msl.go` in each), against the shared interface
    in `pkg/msl` (buffer layout, varyings, the fixed-function fog stage).
- **Renderers.**
  - The software renderer is the reference. It draws headless and on Linux, and outputs PNGs
    and the framebuffer shared memory.
  - The GPU path is active when the viewer has mapped `C32_GPU_SHM`. Each frame is encoded as
    a command stream (format documented at the top of `pkg/d3d9/gpu.go`; bump `gpuVersion`
    *and* the viewer's `kVersion` together whenever it changes). The viewer
    (`viewer/C32Metal.m`) replays it with Metal.
  - `cmd/gpucheck` stands in for the viewer on Linux: it consumes the stream and validates
    every command. **Keep its draw parser in sync with `encodeDraw`.**
- **Input, audio, clock.**
  - The viewer writes keys and controller state into the framebuffer shared memory, and plays
    the PCM ring with AudioQueue.
  - Headless runs use a deterministic clock (derived from the instruction count), live runs
    the wall clock.

## Working conventions

- **Headless runs are deterministic.** The same inputs give the same `Steps=` on every
  `Present() Frame #N` log line. To prove a change doesn't alter game behaviour, compare
  those lines between two runs (recipes in `docs/DEBUGGING.md`). Many commits state
  "instruction counts unchanged" for this reason.
- **Benchmarks on the container are noisy.** Compare variants with paired or concurrent runs,
  or with CPU profiles, never with single wall-clock times.
- **Verifying Metal-side changes without a Mac:**
  1. `go test ./...` (the translators compile their MSL as C++ against
     `pkg/hlsl/testdata/metal_stub.h` and run it against the evaluators). Run it with
     `HLSL_TEST_DIR` (dumped shaders) and `C32_BIN_PATH` set for full coverage.
  2. A headless route with `cmd/gpucheck` attached: 0 protocol errors, plus its statistics.
  3. A syntax check of the viewer: `clang -fsyntax-only -x objective-c -fobjc-arc` against
     stub headers.

  Then the user runs it on the Mac. Say clearly what was and wasn't verified.
- `gofmt` everything you touch. Don't reformat files another session is editing.
- **Background runs:** chain dependent steps in one command or `wait` on PIDs. Never poll
  with `pgrep -f "<pattern>"` (it matches its own shell). Give every run a `timeout`. When
  two runs each use `gpucheck`, give each checker binary a unique name, so that one run's
  `pkill -x` doesn't kill the other's checker.
- **Never `rm -rf` a path built from variables.** Write each run's output to a fresh directory.
- Temporary debugging code goes in a throwaway copy (`git archive HEAD | tar -x -C
  <scratch>`), not in the tree.

## Where to look

| Need | Look in |
|---|---|
| Status, open bugs, backlog | `PROGRESS.md` |
| Environment variables, routes, logs, profilers, gpucheck | `docs/DEBUGGING.md` |
| Game addresses, input layout, how it loads/draws things | `docs/LSWSAGA.md` |
| The JIT (designed, not started) | `docs/JIT.md` |
| Portal / Valve Source games: survey and plan | `docs/PORTAL.md` |
| GPU stream format | top of `pkg/d3d9/gpu.go` |
| History and reasons for past decisions | `git log` (commit messages explain the why) |
