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
~90% of milestone goal achieved: the game boots from `WinMain`, streams `LEVELS\MAP` + `GAME.DAT`, navigates past the Title Screen and Main Menu (`New Game` -> `Save Slot 1`), and renders the **full 3D Mos Eisley Cantina hub (`qui_moscant1.ogg`) + 3D character HUD** in ~20 seconds (`frames/cantina_latest.png`, `frames/frame_2510.png`). Prebuilt `darwin/arm64` and `darwin/amd64` binaries and a Universal 64-bit `.app` launcher bundle (`dist/LEGO Star Wars Saga 64-Bit.app` + `/workspace/run_on_mac.sh`) are ready for live testing on macOS.

| Milestone | State |
|---|---|
| Load Mach-O, run 18 static initialisers, WinMain, mount 7 .DAT archives | DONE |
| Title/attract loop renders (logo, text) via software D3D9 | DONE |
| Real threads (green threads) | DONE — fixed the hang at frame 420 and `BGWorker` (`0x0015ed40`) / `IOWorker` (`0x001e79d0`) cooperation |
| Keyboard input reaching the game | DONE (DirectInput keyboard via `EnumDevices`; browser `/key` forwarding + `LSW_AUTO_KEYS=boot`) |
| Get past "Press a key..." -> Main Menu -> New Game -> Save Slot 1 | DONE (automatic relative to `TitleFirstFrame` and `CantinaFirstFrame`) |
| Mos Eisley Cantina (`LEVELS\MAP`, `qui_moscant1.ogg`) 3D scene + HUD rendering | DONE (`frames/cantina_latest.png`, `frames/frame_2510.png` — ~20s boot-to-Cantina) |
| Audio | STUBBED (DirectSound8 shims with lazy buffer reuse) |
| Speed | ~150M guest steps (~20s wall time) from cold boot to playable 3D Mos Eisley Cantina |
| Universal macOS .app packaging & `/workspace/run_on_mac.sh` | DONE (`bin/lsw64-darwin-arm64`, `bin/lsw64-darwin-amd64`, `dist/LEGO Star Wars Saga 64-Bit.app`) |

## Layout
- `cmd/lsw64` — main: loads image, registers shims, runs WinMain. `LSW_WATCH_KEYS=1` enables a debug memory watch.
- `cmd/lift` — **disassembler/lifter; prints to stdout only (does not modify pkg/lifted)**. `go run ./cmd/lift -start=0x... -end=0x...`
  VAs are the Mach-O VAs (e.g. WinMain 0x00209d10). Linear sweep: start at a function boundary or output desyncs.
- `pkg/cpu` — x86 interpreter (`exec.go`), FPU/SSE, `Intercepts` (host thunks keyed by guest address), `context.go` (thread context).
- `pkg/win32` — MacDoze/Win32 shims. `threads.go` = green-thread scheduler. `win32.go` = files, registry, timers, window class, input injection.
- `pkg/d3d9` — IndirectX/D3D9 + DirectInput/DirectSound/XInput shims, software rasterizer (`raster.go`) writing `frames/frame_NNNN.png` and `frames/cantina_latest.png`.
- `pkg/lifted` — generated Go for some hot NuEngine functions.  `pkg/crt` — libc shims.  `pkg/macho32` — loader.
- Outside repo: `/workspace/run_on_mac.sh`, `/workspace/lego_star_wars_193_boundary_catalog.json`, `/workspace/macho32_host_loader.cpp`.

## Run
- On macOS (isolated in `/tmp/lsw64-mac-preview` on port `8088`):
  `/workspace/run_on_mac.sh`
- On Linux:
  `cd /workspace/lsw64 && ./bin/lsw64 -game "/workspace/Lego Star Wars Saga/LEGO Star Wars Saga.app/Contents/MacOS/LEGO Star Wars Saga"`

## Key findings (addresses are guest VAs)
- Entry: WinMain 0x00209d10 -> NuEngineMain 0x001607c0; main loop 0x00160e10-0x001636df. Splash 0x002d9ad0. BG loader job 0x002da4d0.
- Calling convention for host thunks: Mac cdecl; thunk does `c.Ret()` (caller cleans args). `c.Arg(n)` reads stack args.
- **3D Shader & Matrix Pipeline (`NuShaderSetConstant` `0x00232240` / `NuShaderSetMatrixArray` `0x00232530`)**:
  - In Shader Model 3.0 mode (`[ecx+0xb80] == 4`), NuEngine does NOT update the global parameter table at `*0x00b2842c` during 3D scene rendering (`tableBase` stays stale after the initial loading screen).
  - Instead, `0x00232240` and `0x00232530` transpose 4x4 matrices into `dp4` row-vector layout and pass them directly to `IDirect3DDevice9::SetVertexShaderConstantF`:
    - `VSConstants[16..19]` (`c16..c19`) = `world` matrix (explicitly set to Identity for static world-space Cantina geometry, and set to per-object world transforms for Qui-Gon, Obi-Wan, props, and 3D HUD items).
    - `VSConstants[0..3]` (`c0..c3`) = `viewProj` matrix.
  - `transformVertex` (`pkg/d3d9/raster.go`) transforms 3D vertices by `VSConstants[16..18]` (`world`) followed by `VSConstants[0..3]` (`viewProj`), decodes packed `UBYTE4N` vertex normals at `vp+12` for directional 3D lighting, and performs per-pixel Z-buffering (`s.ZBuffer`).
  - `DrawPrimitive` (`case 81`) passes `is2D := (stride == 24 || stride == 20)` so `NuFont`'s dynamic vertex buffer path (`0x00075d51`) uses 2D NDC `[-1, 1]` coordinates just like `DrawPrimitiveUP` (`0x00075d04`).
- Texture binding: DDS loaded by `D3DXCreateTextureFromFileInMemoryEx` (0x0007912b), copied to NuTex+0x24 in 0x00079560;
  render-command binding 0x00079620+. `s.BoundTextures[0]` in d3d9 is what the rasterizer samples.
- Post-process `NuPostProcessRender` 0x0001d900 is intercepted as a no-op.
- **Threads**: Fixed by green threads (`pkg/win32/threads.go`):
  switch on Sleep / blocking WaitForSingleObject / contended critical section / preemption every 20000 steps; switches only at
  `NestDepth==0` (main sets `vm.NestDepth=-1` so WinMain counts as depth 0).
  - Fixed `0x001e7f48` (`g_ActiveSpinThreads`) initialized to `1` so `IOWorker` (`0x001e79d0`) sleeps 1ms instead of busy-spinning.
  - Removed legacy synchronous `BGWorker` nested-call loop inside `thunkSetEvent` (`pkg/win32/win32.go`) so `BGWorker` (`0x0015ed40`) runs at `NestDepth == 0` and yields to `IOWorker`.
- **Input**: DirectInput8 keyboard (`EnumDevices` -> `CreateDevice` -> `GetDeviceState(256)`).
  - `TitleFirstFrame` is detected dynamically when `s.CubeTexCount >= 5 && s.FrameDrawCalls >= 10` (~Frame 1875).
  - `CantinaFirstFrame` is detected dynamically on the first 3D `DrawIndexedPrimitive` when `s.CubeTexCount >= 8` (~Frame 2467).
  - In `LSW_AUTO_KEYS=boot` mode, `GetDeviceState` taps `DIK_RETURN` at `df = 20, 60, 110` after `TitleFirstFrame` and `cf = 10, 40` after `CantinaFirstFrame` to advance automatically from Title Screen -> Main Menu -> New Game -> Save Slot 1 -> active Mos Eisley Cantina gameplay.
- **Guest Heap**: Lazy allocation (`guestBuf` on `LockRect`/`Lock`) covers `allocTexture`, `allocSurface`, and `IDirectSound8::CreateSoundBuffer`, and `DuplicateSoundBuffer` reuses `pOrig`.
- **macOS Quick-Run Preview (`/workspace/run_on_mac.sh`) & Universal `.app` Bundle**:
  - Cross-compiled `bin/lsw64-darwin-arm64` and `bin/lsw64-darwin-amd64` and packaged `dist/LEGO Star Wars Saga 64-Bit.app`.
  - `/workspace/run_on_mac.sh` writes frames/saves to `/tmp/lsw64-mac-preview`, listens on `http://127.0.0.1:8088`, and forwards browser keyboard/button input directly to DirectInput8.

