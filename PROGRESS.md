# Progress

Status and open work. Read `CONTEXT.md` first. History lives in `git log` (commit messages
explain each change); keep this file to what is true now.

## Status (October 2026)

| Area | State |
|---|---|
| Boot, menus, saving and loading | Works. Saves land in `~/Library/Application Support/catalina32/LEGO Star Wars Saga/SaveData` and load after relaunch. |
| Gameplay | Cantina hub, Episode I Chapter 1 (Negotiations) and the chapter after it played through by the user without crashes or drawing errors. Nothing past that has been tried. |
| Rendering (Metal) | Default on the Mac. The user confirmed: room and door transitions, pause-menu background, cut-scenes, TC-14's chrome (cube maps), mipmaps, scissor, viewport reset, fog, and the double-vision fix (the D3D9 half-pixel offset pointed the wrong way, so blur passes drifted). No known drawing bugs. |
| Rendering (software) | Reference renderer for headless runs and Linux. Doesn't draw render targets other than screen size (bloom buffers). |
| Audio | Works (DirectSound mixing → shared memory → AudioQueue). |
| Input | Keyboard and controllers (USB/Bluetooth, players 1 and 2, hot-plug) confirmed on the Mac. |
| Speed | Metal: levels 35–50 FPS, Cantina ~57. The emulator is the limit there (~110M x86 instructions/s; 60 FPS needs ~170M). Shaders now compile in the background (no more 0.4–0.7 s stalls). The near-zero freezes when moving between areas came from the game's level-data decompressor running emulated; it now runs natively. Profile-guided optimisation and lazy texture decoding: 10–13% less time for the same emulated work (headless, GPU path). |
| Platforms | Tested on an Intel MacBook Pro only. Apple Silicon builds exist but won't be tested (the user's decision). |

## Open

1. **Frame rate near zero when moving between areas** of the chapter after Negotiations.
   - **Cause:** the game streams LHA `-lh5-` compressed level data as the player moves
     between areas, and its decoder ran emulated. Profiled over the user's session: more
     than half the guest instructions of those frames, 2–7 s per freeze.
   - **Fix (confirmed on the Mac):** a native decoder (`pkg/lh5`)
     replaces `NuLZHUF_Decompress`, found by signature (`pkg/crt/lzhuf.go`, so any game built
     with this decoder gets it), leaving the game's state exactly as its own code would. `TestLZHUFMatchesGuest` checked all 3,724 blocks the session decompressed (output
     and decoder state identical) and, by default, a sample plus damaged and random input.
     About 145× faster than the emulated decoder there. `C32_NO_NATIVE_LZHUF=1` switches it off.
2. **Heavy areas run at 20–30 FPS** (the emulator at full speed). Compiled code already runs
   83% of the instructions there; the other 17% is spread over hundreds of interpreted
   functions and takes about 37% of the host time. The AOT compiler can't fix this for users
   (see Speed in the backlog); the generic answer is the JIT (branch `jit`, `docs/JIT.md`):
   without AOT, a headless route runs 2.2–2.5× faster. Not yet tried on the Mac.

## The app

Catalina32.app and game apps (`docs/ARCHITECTURE.md`) work on the user's Mac: the wizard, game
apps, saves in the original game's folder, quitting, the pointer in full screen. **Release 0.1.0
is published** (`scripts/make_release.sh`; GitHub pre-release, universal, without AOT code).
- **Decided:** no Apple Developer ID or notarisation; the README and release notes explain the
  one-time Gatekeeper step instead.
- **Later:** an icon for Catalina32 itself.
- **Speed:** release builds have no AOT code, so heavy scenes are choppier than with
  `run_on_mac.sh`. The JIT is the answer for players: built (branch `jit`, `docs/JIT.md`), to be
  tried on the Mac.
- **Next game: Portal** (then Portal 2): Valve's own Mac ports, not Feral. Branch `source`.
  On the user's Mac it plays from the menu through the first chambers with no known bugs:
  New Game and Load Game, mouse look, keyboard, a controller, sound and music, text, portals.
  Interpreted it runs at a few frames per second; the JIT runs its gameplay 3.7× faster
  headless (to be tried on the Mac). It runs from
  `scripts/run_source.sh` only (no game app yet). Details, tests and next steps:
  `docs/PORTAL.md`. The Feral family layer waits for a second Feral game.

## Backlog

- **Play further:** other chapters and episodes, vehicle levels, Free Play, bonus areas. Expect
  unimplemented calls to show up as crashes or drawing errors (logged once by name).
- **Speed.** Everything here must work for users with no developer tools and no per-game work.
  - **The JIT** (hot guest code translated to machine code at run time, on the user's own
    copy; `docs/JIT.md`): x86-64 and arm64 backends, in Go and assembly. Next: try it on the
    Mac; host calls without leaving translated code; Apple Silicon (off there unless
    `C32_JIT=1` until verified). The interpreter's dispatch loop is near what plain Go allows
    (precomputed addresses and batched step counts measured as noise).
  - **The AOT compiler (`cmd/c32aot`) is a developer tool.** It translates the game's code to
    Go, which needs Go on the user's machine and can't be distributed (it is game code), so a
    shipped app can't count on it; hand-picking or auto-choosing its function list doesn't
    change that.
  - Native replacements bind by signature (`cmd/sigmake`, `pkg/profile/signatures*.go`) so
    they reach every game with the same code; refresh `cmd/catalina32/default.pgo` after large
    `pkg/cpu` changes (`docs/DEBUGGING.md`).
- **Rendering gaps:**
  - mip chains for cube maps (they have one level; D3DX would generate more);
  - the game's own mip levels where a file has them (the viewer generates them instead);
  - compressed textures whose sizes aren't multiples of 4 are sent uncompressed;
  - fixed-function vertex fog without a vertex shader (not used by this game);
  - the legacy heuristic renderer (`C32_REAL_SHADERS=0`) still mis-places skies.
  - `StretchRect` with rectangles (the game never passes any): the viewer's copy discards the
    rest of the destination (`MTLLoadActionDontCare`) when copying into a sub-rectangle; empty or
    inverted rectangles aren't rejected (negative Metal viewport); the software path lost its
    plain whole-surface copy.
- **Robustness:** the lifted `NuLZHUF_DecodeC` (`pkg/games/lswsaga/lifted.go`), which the game
  falls back to when the native decoder declines corrupt input, can loop forever inside one
  step on such input. Real game data never reaches it.
- **Packaging:** `run_on_mac.sh --build-app` makes a `.app`, but it isn't polished for
  non-developers.
- **General tool:** only this game has a profile. Game-specific logic still inside
  `pkg/d3d9`: the legacy renderer's heuristics (2D detection by vertex stride, the skin
  matrix and c0..c3/c16..c19 layout in `transformVertex`) and the fixed 1280×720 back buffer.

## Milestones

| Date | Milestone |
|---|---|
| 2026-10-16 | Runtime snapshot: interpreter, Win32/D3D9 shims, green threads, software renderer; title screen and menus |
| 2026-10-16/17 | Cantina hub, audio (Ogg Vorbis music, sound effects), Episode I entry, controllers, real shaders (the game's HLSL and bytecode), AOT-compiled hot functions, decoded-instruction cache, pipelined parallel renderer |
| 2026-10-18 | Two code-review passes (crash safety, cross-host determinism, reference-counted resources, saves); HLSL and bytecode translation to Metal; GPU command stream; Metal viewer in the repository |
| 2026-10-19 | White-screen fixes (copies into textures, compressed uploads); SetViewport; cube maps; scissor, viewport reset, mipmaps, fog; double vision fixed (half-pixel offset); StretchRect rectangles; one save folder; docs split into `CONTEXT.md`, `PROGRESS.md`, `docs/` |
| 2026-10-20 | Portal (branch `source`): dynamic linker, 3 GB guest space, the system libraries in Go, OpenGL streamed to the window, host fonts; then on the Mac: mouse look, sound and music (OpenAL, Miles' MP3 plug-in on new MMX support), controllers, the New Game dialog, portals on AMD GPUs; `tools/glreplay` to see frames on Linux |
