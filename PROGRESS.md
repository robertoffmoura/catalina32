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
| Audio | DONE — DirectSound8 buffer mixing -> shm PCM ring -> macOS `AudioQueue` |
| Story level entry (Episode I Ch.1 *Negotiations*) | DONE — `LSW_AUTO_KEYS=episode1`; in-level gameplay renders + music |
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
  - **3D vs. 2D `DrawPrimitiveUP` (`case 83`)**: In the Cantina, `DrawPrimitiveUP` (`stride == 24`) is called during the 3D pass for character blob shadows (`v0 = (-26.52, 0.00, -50.79)`) and 3D in-world hologram save-slot text (`0.0%` and `0:03:39.95` with 3D world matrix `w` at `(-26.71, 0.71..0.87, -48.78)`), and later during the 2D HUD pass for `Press F2 or any button to start`. `transformVertex` checks `is3DUP` so 3D `DrawPrimitiveUP` calls go through the 3D `world` + `viewProj` transform instead of being plastered across the center of the screen.
- **Skeletal Character Animation (`NuUploadSkinMatrices` at `0x00070110`)**:
  - `0x00070110(buf)` uploads `n = [buf]` 4x4 bone matrices at `buf + 4` before drawing skinned character submeshes (`stride == 40` `decl=0x14242270` and `stride == 44`).
  - `thunkNuUploadSkinMatrices` captures `s.SkinMatrices[0..n-1]`, `SetVertexShaderConstantF(startReg == 16)` resets `s.NumSkinMatrices = 0` for subsequent rigid meshes, and `transformVertex` applies 3-bone weighted skinning (`UBYTE4N` weights at `vd.BlendWOffset`, `UBYTE4` indices at `vd.BlendIOffset`) to both vertex positions and normals.
- **`IDirect3DVertexDeclaration9` Parsing & `FLOAT4` `TEXCOORD0` Atlas UVs**:
  - `CreateVertexDeclaration` (`case 86`) and `SetVertexDeclaration` (`case 87`) track the active `D3DVERTEXELEMENT9` layout (`s.VertexDecls[s.CurrentVertexDecl]`).
  - Static Cantina surface/lightmapped meshes (`stride == 36, 48, 52, 64`) declare `u5.0` (`TEXCOORD0`) as `D3DDECLTYPE_FLOAT4` (`t3`), where `.xy` (`+0, +4`) holds normal-map tiling UVs (`surface_sampler`) and `.zw` (`+8, +12`) holds the `1024x1024` Cantina atlas UVs (`layer0_sampler`, `0x1230b9d0`), or `(0.0, -1.0)` for untextured sandstone/stucco surfaces.
  - `NuTexRead` (`0x00078df0`) is patched at `0x00079088` (`6x NOP`) so `.GSC` scene textures with `flags & 0x3c0c != 0` are never skipped, and `thunkNuShaderParseConstantTable` (`0x001535c0`) links `layer0_sampler` (36), `layer1_sampler` (37), `surface_sampler` (42), `surface2_sampler` (43), `layer0_diffuse` (51 -> `PSConstants[112]`), `ambientColor` (0 -> `PSConstants[113]`), and `incandescentGlow` (2 -> `VSConstants[114]`).
- **HUD Character Rings & Mip-Level Locking**:
  - `IDirect3DTexture9::LockRect` (`case 19`) routes `Level > 0` locks to `s.mipScratchBuf` so smaller mip levels in `NuTexRead` (`0x00079250`) never overwrite mip level 0 (`res.DataPtr`).
  - `rasterizeTriangle` discards transparent `(0,0,0,0)` texels on non-DXT1 textures (`D3DFMT_DXT5` HUD rings `0x11f90c30`/`0x11f90c50` and hearts `0x11f90c90`) and modulates RGB by texel color on all formats so black DXT1 outlines/eyes render cleanly.
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
- **macOS Native Cocoa/Quartz Window (`/workspace/run_on_mac.sh`) & Cantina FPS Optimizations**:
  - Replaced the `swiftc` `WKWebView` / browser fallback in `/workspace/run_on_mac.sh` with a pure Objective-C Cocoa/Quartz native window (`LSWWindow.m`, compiled via `/usr/bin/cc -O2 -ObjC -fno-objc-arc -framework Cocoa -framework QuartzCore`) backed by a `3,686,688`-byte shared memory file (`LSW_SHM_FILE=/tmp/lsw64-mac-preview/framebuffer.shm` mapped with `mmap(MAP_SHARED)`).
  - `LSWWindow` blits raw `1280x720` RGBA pixels from `shm + 288` directly via `CGDataProviderCreateWithData` + `CGImageCreate` at 60Hz (zero HTTP polling, zero PNG encoding/decoding) and writes native macOS `keyDown:`/`keyUp:` events directly into `shm + 32 + dik` (`0ms` input latency).
  - Eliminated 6 major rendering/scheduling bottlenecks:
    1. **Zero-PNG Live Export (`exportFramePNG`)**: Removed synchronous `png.Encode` from `Present()` in live mode; `Present()` now only copies raw RGBA into `s.LatestRGBA` / `s.ShmFile` (when `s.FrameDrawCalls > 0`) and encodes PNG lazily on demand in `/latest.png`.
    2. **Host-Side `NuShaderBindMaterial` (`0x001538f0`)**: `thunkNuShaderBindMaterial` resolves `layer0_sampler` (`[mtl+0xb8]` -> `*0x02c25d08` -> `s.BoundTextures[0]` and `*0x02b5af74`) and unpacks `layer0_diffuse` (`[mtl+0xc8]` -> `PSConstants[112]`), `ambientColor` (`[mtl+0x11c]` -> `PSConstants[113]`), and `incandescentGlow` (`[mtl+0x120]` -> `VSConstants[114]`) in Go, eliminating ~3,500 COM traps and ~80,000 guest x86 instructions per Cantina frame.
    3. **Triangle Strip Vertex Reuse (`case 5`) & Cached State**: `rasterizeIndexedBatch` and `rasterizePrimitiveBatch` reuse the previous 2 transformed strip vertices (`v0, v1`) so each strip triangle transforms 1 vertex instead of 3 (`40k` vs `120k` `transformVertex` calls/frame), use cached `s.CurrentVD`, and replace the `s.RenderStates` map with `[256]uint32`.
    4. **Incremental Scanline Rasterizer (`rasterizeTriangle`)**: Steps barycentric coordinates (`w0 += dw0dx`, `w1 += dw1dx`) incrementally across scanlines, performs early Z-rejection before color/UV math, and uses power-of-two bitmasks (`wMask`, `hMask`) instead of per-pixel `math.Floor` and `%`.
    5. **Dirty Framebuffer Tracking (`s.fbDirty`)**: Skips redundant `clearFramebuffer`/`clearZBuffer` loops and empty-frame exports during loading screens when `s.FrameDrawCalls == 0`.
    6. **Green-Thread Quantum Reset (`SetPreemptInterval`)**: Updated `switchTo` (`pkg/win32/threads.go`) to advance `c.nextPreempt = c.Steps + interval` (`400,000` steps for background `.GSC` loader threads), cutting Title Screen boot time to `4.5s` (`Frame #139`, `68 FPS` menu) and Mos Eisley Cantina load time to `19.5s` (`Frame #490`, `14–22+ FPS` in Cantina).

## Generalization Architecture (Open-Source 32-Bit Feral / IndirectX Mac Game Runner)
To ensure this runtime can be open-sourced on GitHub as a general runner for 32-bit Feral Interactive (`MacDoze` + `IndirectX`) and Traveller's Tales (`NuEngine`) macOS games (*LEGO Star Wars*, *LEGO Indiana Jones*, *LEGO Batman*, *LEGO Harry Potter*, *LEGO Lord of the Rings*, *BioShock*, *Borderlands*, etc.), all future work must adhere to these separation-of-concerns rules:
1. **Strict 3-Layer Separation**:
   - **Layer 1 (`pkg/macho32`, `pkg/cpu`, `pkg/crt`)**: Pure 32-bit `i386` Mach-O loader, CPU/FPU/SSE2 emulator, and `libSystem.B.dylib` (`libc`/`libm`/`pthread`) shims — zero game-specific or Win32-specific assumptions.
   - **Layer 2 (`pkg/win32`, `pkg/d3d9`)**: Generic Feral `MacDoze` (Win32 Kernel32/User32/Advapi32, cooperative green threads, VFS) and `IndirectX` (`IDirect3D9`, `IDirect3DDevice9`, `D3DX9`, `IDirectInput8A`, `IDirectSound8`, `Bink`) COM interfaces.
   - **Layer 3 (Per-Binary / Engine Profile)**: All binary-specific addresses (`WinMain`, `NuEngineSplitAddr`, boundary function catalog, and optional lifted hot functions in `pkg/lifted`) should be driven by a binary profile or symbol/signature scanner rather than scattered across `pkg/cpu` or `pkg/win32`.
2. **Prefer Standard D3D9 / DirectSound8 / Win32 Interfaces Over Internal Engine Hooks**:
   - Whenever implementing a missing feature (audio, shaders, input, rendering), implement the standard `IDirect3DDevice9` / `IDirectSoundBuffer8` / `D3DXCompileShader` / `ID3DXConstantTable` interface rather than adding new hardcoded NuEngine internal address hooks.

## Remaining Roadmap Toward Full Objective
1. ~~Real Audio Output~~ — DONE: `IDirectSoundBuffer8` circular `Lock`/`Play`/`GetStatus`/`DuplicateSoundBuffer` + `mixAudioFrame` -> shm PCM ring -> `AudioQueue` in `LSWWindow` (`AudioSamples` grows continuously in-level).
2. ~~Story Level Entry~~ — DONE: `LSW_AUTO_KEYS=episode1` walks Cantina -> Episode I room -> Chapter 1 door -> `Story` -> opening crawl -> playable *Negotiations* (`ep1_1_action.ogg`, studs collected, Force hint HUD). Next: play deeper (Force objects, combat, droids) and fix what breaks.
3. **Opening-crawl projection bug**: crawl text lines near the camera render as huge text at the *top* of the screen plus streaks — crawl probably uses a different VS constant layout / projection than c0..c3 + c16..c18. Investigate the crawl's `SetVertexShaderConstantF` calls.
4. **General D3D9 Shader / Multi-Core & Hardware-Accelerated Rendering**:
   - Parallelize scanline band rasterization across host CPU cores (and/or offload triangles to Metal/OpenGL in `LSWWindow`) and support general `D3DXCompileShader` constant tables (`ID3DXConstantTable`).

## Session log (latest first)
- **Parallel band rasterizer, vertex cache, guest profiler**:
  - `rasterizeScreenTriangle` now queues `queuedTri`s into `s.frameTris`; `Present()` calls `flushFrameTriangles()`, which rasterizes 8 horizontal 90px bands on parallel goroutines (bands own disjoint framebuffer/Z rows, so no locks). Safe because `Clear` only acts when `FrameDrawCalls == 0` (no mid-frame clears); the queue is reset on clears and on the first-Cantina-DIP clear.
  - Additive blending (`ALPHABLENDENABLE` + `DESTBLEND == D3DBLEND_ONE`) no longer writes Z; Cantina band stage lights now glow instead of drawing black quads.
  - `rasterizeIndexedBatch` uses a 256-entry post-transform vertex cache. Live mode renders every frame (was every 2nd).
  - `CallStack`/`VisitedFuncs` bookkeeping only when `LSW_TRACE_CALLS=1`; `CallGuestNested` stop address uses `NestDepth`.
  - Benchmark (`LSW_AUTO_KEYS=outside`, headless renders every 5th frame): Cantina frames 540→600 take 5.07s vs 6.16s before (−18%); frames bit-identical except the additive lights.
  - New `LSW_GUEST_PROF=<fromStep>`: samples the guest call stack every 997 steps and prints top self/inclusive guest functions at exit (`[PROF]` lines) — use it to pick functions to lift.
- **Ogg Vorbis Music Streaming, 3D DirectSound Separation, 60 FPS Menu Clock, Indoor/Outdoor Multi-Layer Textures & Vorbis/3D Fast-Paths**:
  - **Ogg Vorbis Music Streaming (`titles.ogg`, `qui_moscant1.ogg`, `qui_mos_a.ogg`)**: Fixed `0x0060b8a0` (`GetFileSize`, previously mis-registered as `FlushFileBuffers` returning `1` byte), implemented `timeSetEvent` (`0x00624310`) and `timeKillEvent` (`0x006245f0`, previously mis-registered as `CreateThread`), added missing `libm` functions (`_rintf`, `_rint`, `_atan`, `_log`, `_exp`, `_frexp`, `_ldexp`) in `pkg/crt/crt.go`, and fixed `IDirectSoundBuffer8::Lock` (`dwBytes == 0` guard) so background Ogg Vorbis tracks decode and stream cleanly into the circular `IDirectSoundBuffer8`.
  - **3D Sound Effects & Duplicate Voice Cleanup (`IDirectSoundBuffer8::QueryInterface`)**: Separated `IID_IDirectSound3DBuffer8` (`0x279afa86`) onto its own COM object (`s.dummy3DObj`, `s.vtDS3DBuffer`) so `IDirectSound3DBuffer8::SetMode` (slot 18) no longer collides with `IDirectSoundBuffer8::Stop` (slot 18) every frame, and added `RefCount`/`IsDuplicate` cleanup on `Release` so one-shot SFX buffers (like death/fall screams) are removed once released.
  - **Real-Time 1.0x Clock & 60 FPS Menu Cap**: Enabled real wall-clock `QueryPerformanceCounter` and `timeGetTime` when `env.LiveClock` is active (`LSW_SHM_FILE` / `LSW_LIVE=1`), added a 60 FPS (`16.667ms`) frame limiter in `IDirect3DDevice9::Present`, and paced `mixAudioFrame` sample generation by real elapsed `dt`.
  - **Indoor Cantina & Outdoor Courtyard Textures (`NuBindLightmaps` + `FLOAT4` `.xy`/`.zw` UVs + 2-Layer `COLOR1` Blending)**:
    - Intercepted `NuBindLightmaps` (`0x0006f840`, `0x00070730`, `0x000708a0`) so lightmap atlases no longer overwrite stage 0 (`s.BoundTextures[0]`) after `NuShaderBindMaterial`.
    - Fixed `transformVertex` (`pkg/d3d9/raster.go`) to read `.xy` (`vp + vd.Tex0Offset`) for primary tiled diffuse UVs (`layer0_sampler`) on `D3DDECLTYPE_FLOAT4` vertex declarations instead of `.zw` (lightmap / layer 1 UVs), and raised the batch primitive cap from `4,096` to `32,768`.
    - Added 2-layer material blending (`layer0_sampler` at `s.BoundTextures[0]` + `layer1_sampler` at `s.BoundTextures[1]` using `COLOR1.r` vertex alpha and `TEXCOORD0.zw` / `TEXCOORD0.xy`), fixing both the outdoor Mos Eisley courtyard ground (`tid=566` gravel + `tid=577` desert sand) and 2-layer Cantina archway/wall materials (`tid=562` + `tid=565`).
  - **Vorbis & 3D Math Fast-Paths**: Added native 64-bit Go fast-paths in `pkg/crt/crt.go` for `oggpack_look` (`0x008be6c0`), `oggpack_adv` (`0x008be770`), `oggpack_read` (`0x008be7a0`), `decode_packed_entry_number` (`0x008c17a0`), `vorbis_book_decodev_set` (`0x008c1ef0`), and `vorbis_book_decodevv_add` (`0x008c1fe0`), plus `NuVecMtxTransform` (`0x00102c40`), `NuVecMtxRotate` (`0x00102dd0`), `NuSqrt` (`0x0016ff70`), `NuInitMtlItem` (`0x0006e050`), and `NuHasRenderData` (`0x0002dd60`) in `pkg/lifted/lifted.go`.
- **Episode I entry + rasterizer clipping**:
  - Default keyboard layout (table at VA `0x008e985c`, DIK codes): P1 block = `A D W S` (left/right/up/down), then `LCtrl`, `Space`, `K`, `J`, `U`, `H`, `Enter` (U = jump, Enter = pause/start, on-screen prompts show J = Back/Force, U = Select); P2 block = arrow keys + `[ ] \\ ' P ;` + `Backspace`. `LSW_AUTO_KEYS=episode1` now uses `W`/`D`/`A` + `U` (was wrongly pressing arrow keys = Player 2).
  - Removed `defaultInputScript` from `thunkPeekMessageA`: it injected `WM_KEYDOWN VK_RETURN` every 120 peeks unconditionally, which (a) auto-confirmed menus (the "main menu skips straight to New Game" bug) and (b) paused gameplay. Scripted window-message input now only happens if `env.InputScript` is set explicitly.
  - Rasterizer: 3D vertices keep clip-space `cx..cw`; `rasterizeTriangle` clips against `w = nearW (0.05)` (Sutherland–Hodgman) and fans the result into `rasterizeScreenTriangle`. Removed per-vertex `cw<=0.08 / cw>1000 / |ndc|>4` rejection and the ">660px 3D triangle" hack — fixes the black wedge over the Cantina floor near the camera. Texture UVs are now perspective-correct (u/w, v/w, 1/w).

