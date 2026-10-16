# lsw64 — porting LEGO Star Wars: The Complete Saga (macOS i386) to 64-bit

> READ THIS FIRST after any restart / context reset. Keep it updated; commit often (`git -C /workspace/lsw64 commit -am ...`).

## Goal
User owns the 32-bit (i386) macOS copy: `/workspace/Lego Star Wars Saga/LEGO Star Wars Saga.app`
(binary `Contents/MacOS/LEGO Star Wars Saga`, data in `/workspace/Lego Star Wars Saga/LEGOStarWarsSagaData`).
Modern macOS can't run 32-bit. Make it playable on a 64-bit Mac.

Approach taken: **host runtime in Go** (`/workspace/lsw64`) that loads the i386 Mach-O, interprets x86
(plus a few statically "lifted" functions), and replaces the Win32-ish "MacDoze" layer and Direct3D9 "IndirectX"
layer with Go shims; renders with a software rasterizer. Eventual deliverable: a universal (amd64+arm64) Go binary
placed in the .app, original binary kept as `LEGO Star Wars Saga.i386`.

## HONEST STATUS (update the numbers when they change)
~20% of "actually playable". Nothing has been tested on macOS (this box is Linux). Not packaged yet.

| Milestone | State |
|---|---|
| Load Mach-O, run 18 static initialisers, WinMain, mount 7 .DAT archives | DONE |
| Title/attract loop renders (logo, text) via software D3D9 | DONE but crude (3D scene not right) |
| Real threads (green threads) | DONE (this session) — fixed the hang at frame 420 |
| Keyboard input reaching the game | DONE (DirectInput keyboard via EnumDevices; scripted Enter/F2 taps) |
| Get past "Press a key..." | DONE -> game then showed "Press F2 or any button to start", then started loading LEVELS\MAP (hub map) |
| Level load finishing / menus / gameplay | IN PROGRESS: load is just very slow in the interpreter (~6M instr/s); long run in scratch/run_long.log |
| Audio | NOT STARTED (DirectSound shims are stubs) |
| Speed | UNSOLVED: interpreter ~10M instr/s; gameplay likely needs 50-100x. Probably needs much more recompilation (lift more code) or a JIT. This is the biggest risk to "playable". |
| Universal macOS .app packaging | NOT STARTED |

## Layout
- `cmd/lsw64` — main: loads image, registers shims, runs WinMain. `LSW_WATCH_KEYS=1` enables a debug memory watch.
- `cmd/lift` — **disassembler/lifter; prints to stdout only (does not modify pkg/lifted)**. `go run ./cmd/lift -start=0x... -end=0x...`
  VAs are the Mach-O VAs (e.g. WinMain 0x00209d10). Linear sweep: start at a function boundary or output desyncs.
- `pkg/cpu` — x86 interpreter (`exec.go`), FPU/SSE, `Intercepts` (host thunks keyed by guest address), `context.go` (thread context).
- `pkg/win32` — MacDoze/Win32 shims. `threads.go` = green-thread scheduler. `win32.go` = files, registry, timers, window class, input injection.
- `pkg/d3d9` — IndirectX/D3D9 + DirectInput/DirectSound/XInput shims, software rasterizer (`raster.go`) writing `frames/frame_NNNN.png`.
- `pkg/lifted` — generated Go for some hot NuEngine functions.  `pkg/crt` — libc shims.  `pkg/macho32` — loader.
- Outside repo: `/workspace/lego_star_wars_193_boundary_catalog.json`, `/workspace/macho32_host_loader.cpp`.

## Run
`cd /workspace/lsw64 && go run ./cmd/lsw64` (MaxSteps is set in main.go, currently 1.2B; ~1.5 min). Frames land in `frames/` (git-ignored).
Don't dump raw hex in the terminal; use `cmd/lift`. A full disassembly dump (`scratch/dump.txt`, 57MB, git-ignored) exists but
is desynced at the start (header data); regenerate per-range if needed.

## Key findings (addresses are guest VAs)
- Entry: WinMain 0x00209d10 -> NuEngineMain 0x001607c0; main loop 0x00160e10-0x001636df. Splash 0x002d9ad0. BG loader job 0x002da4d0.
- Calling convention for host thunks: Mac cdecl; thunk does `c.Ret()` (caller cleans args). `c.Arg(n)` reads stack args.
- Shader parameter table: pointer at `*0x00b2842c`, 92-byte entries, matrices at entry+0x1c. Useful ids: 17 world, 18 view, 19 viewProj,
  20 worldViewProj, 21 worldView, 85 worldParams. Sampler/param registers resolved via `D3DXCompileShader` constant table, which we
  return as NULL -> `NuShaderParseConstantTable` (0x001535c0) is intercepted (`thunkNuShaderParseConstantTable`) to fake bindings.
  `NuShaderSetConstant` 0x00232240 / SetTexture path 0x00232530 check `[ecx+0xb80]==4` to use the built-in table.
- Texture binding: DDS loaded by `D3DXCreateTextureFromFileInMemoryEx` (0x0007912b), copied to NuTex+0x24 in 0x00079560;
  render-command binding 0x00079620+. `s.BoundTextures[0]` in d3d9 is what the rasterizer samples.
- Post-process `NuPostProcessRender` 0x0001d900 is intercepted as a no-op.
- **Threads**: before this session `CreateThread` never ran the thread and `Sleep` was a no-op, so the main thread spun forever in
  0x001601e0 (wait-for-job loop polling 0x001e8fa0) after frame 420. Fixed by green threads (`pkg/win32/threads.go`):
  switch on Sleep / blocking WaitForSingleObject / contended critical section / preemption every 20000 steps; switches only at
  `NestDepth==0` (main sets `vm.NestDepth=-1` so WinMain counts as depth 0). Threads from CreateThread are created suspended by the game
  (0x001e79d0 x2) and resumed via ResumeThread. Mapped `WaitForSingleObjectEx` (0x00623c30). ResetEvent = 0x00623c30 (1 arg; NOT WaitForSingleObjectEx - that was a misidentification fixed 2026-10-16)
  (previously this caused a worker busy-spin: ~28M Enter/Leave/Wait calls).
  **RESOLVED (2026-10-16)**: Removed the legacy synchronous `BGWorker` nested-call loop inside `thunkSetEvent` (`pkg/win32/win32.go`). Running `BGWorker` jobs (`fn1=0x00132340`) via `c.CallGuestNested` inside `thunkSetEvent` set `c.NestDepth = 1`, which prevented `WaitForSingleObject`/`Sleep` inside `0x00132340` from yielding to the `IOWorker` thread (`0x001e79d0`) and caused `WaitForSingleObject` to return immediately without waiting for async file I/O. Now `thunkSetEvent` simply signals the event and yields so the real `0x0015ed40` (`BGWorker`) green thread runs at `NestDepth == 0` and cooperates with `0x001e79d0`.
- **Input**: WndProc = 0x00208b60 (captured from RegisterClassExA). WM_KEYDOWN(0x100)/WM_KEYUP(0x101) take scancode from lParam bits 16-23
  (+ extended bit) and call 0x000c0390 (down) / 0x000c04d0 (up), which maintain a key table at 0x02C31C86 (+scancode) and a queue.
  **RESOLVED (2026-10-16)**: the game's real keyboard path is DirectInput, not WM_KEY*: it calls `IDirectInput8::EnumDevices(DI8DEVCLASS_KEYBOARD)`,
  and in the callback (0x000be3d0) creates the device via `CreateDevice(guidInstance)`, then SetDataFormat (256 objs) + Acquire; each frame
  0x000bb9f0 polls `GetDeviceState(256)` (skips if the device pointer [kbobj+0x24] is NULL). Fixed in d3d9.go (`EnumDevices` reports a keyboard;
  `GetDeviceState` supports interactive browser keys via `/key` + boot-only auto-keys `LSW_AUTO_KEYS=boot` so it doesn't spam Pause Menu after frame 380).
- **Guest Heap**: Lazy allocation (`guestBuf` on `LockRect`/`Lock`) now covers `allocTexture`, `allocSurface`, and `IDirectSound8::CreateSoundBuffer`,
  and `DuplicateSoundBuffer` reuses `pOrig` instead of allocating 64 KB per sound effect (which previously exhausted the heap around frame 3,480).
- **macOS Quick-Run Preview (`/workspace/run_on_mac.sh`)**:
  - Cross-compiled `bin/lsw64-darwin-arm64` and `bin/lsw64-darwin-amd64`.
  - `/workspace/run_on_mac.sh` writes frames/saves to `/tmp/lsw64-mac-preview` (zero interference with `/workspace/lsw64/frames`), listens on `http://127.0.0.1:8088`, and forwards browser keyboard/button input directly to DirectInput8.

## Run notes (long runs)
`go build -o bin/lsw64 ./cmd/lsw64 && LSW_MAX_STEPS=40000000000 ./bin/lsw64 > scratch/run_bgthread.log 2>&1`
Look at `frames/latest.png` for the current screen.

## Next steps (priority order)
1. Verify `LEVELS\MAP` completion with the real `0x0015ed40` (`BGWorker`) green thread running at `NestDepth == 0` alongside `0x001e79d0` (`IOWorker`).
2. Follow the game into Mos Eisley Cantina (`LEVELS\MAP`) / interactive gameplay and fix any remaining Win32/MacDoze or 3D shader/transform issues.
3. Package the universal 64-bit `.app` bundle once Cantina / gameplay is verified.

## Working agreements
- Commit to git regularly; keep this file current (status table, findings, next steps).
- Be honest about progress; earlier "DONE" claims overstated (frame 420 was just the attract loop and then the game hung).
