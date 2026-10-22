# Progress

Status and open work. Read `CONTEXT.md` first. History lives in `git log` (commit messages
explain each change); keep this file to what is true now.

## Status (October 2026)

Two games, both played on the user's Mac (Intel MacBook Pro, Radeon Pro 560X) with the JIT:

| Area | LEGO Star Wars: The Complete Saga | Portal |
|---|---|---|
| Played | The Cantina hub and all levels of the first chapter, with the JIT; nothing past that yet. | From the first chamber to the ending, no errors or crashes (2026-10-22). Every story map loads and plays its first seconds headless with no halts. |
| Saves | The original game's folder (`~/Library/Application Support/Feral Interactive/…`), backed up weekly. | `~/Library/Application Support/catalina32/Portal` (an overlay: the game's folder is never written; its files, older saves included, read through). |
| Drawing | Metal (the game's Direct3D 9 translated). No known drawing bugs. | OpenGL (the game's ToGL calls streamed to the window). Portals correct, AMD clip planes included. |
| Sound, input | DirectSound mixing → AudioQueue. Keyboard, controllers (two players, hot-plug). | OpenAL in Go, Miles MP3. Mouse look, keyboard, controllers. |
| Speed | Levels 35–50 FPS, Cantina ~57. | Since the speed work (`e2fc8ad` → `dabd540`, headless, side by side): GLaDOS's chamber 1.9×, other large rooms 1.1–1.6×; see `docs/PORTAL.md`. |

The JIT (`docs/JIT.md`) translates hot guest code to x86-64 or arm64; on the Mac since
October 2026 (both games, Intel). Apple Silicon builds exist but won't be tested (the user's
decision): the JIT is off there unless `C32_JIT=1`.

## Open

1. **Portal's saves follow the game's folder path.** The overlay keeps the game's writes under
   their full path (`…/catalina32/Portal/<the folder's path>/portal/save`), so if the Portal
   folder moves (another Steam library), the game app made again doesn't see the saves made
   before. Keying the overlay by the path inside the game's folder fixes it; existing overlays
   (and their `.catalina32-deleted` list) need moving over.
2. **Heavy scenes:** what is left of a frame is spread out (`docs/PORTAL.md`, Next); more
   speed means general JIT work (fewer returns to Go), not more native ports.

## The app

Catalina32.app and game apps (`docs/ARCHITECTURE.md`) work on the user's Mac: the wizard, game
apps, saves in the original game's folder, quitting, the pointer in full screen. **Release
0.1.0** (LEGO Star Wars, interpreter) is published; **0.2.0** adds the JIT and Portal (a game
app made from its folder; `pkg/gamelib/source.go`).
- **Decided:** no Apple Developer ID or notarisation; the README and release notes explain the
  one-time Gatekeeper step instead.
- **Later:** an icon for Catalina32 itself.
- **Next games:** Half-Life 2 and its episodes (the same launcher and engine generation as
  Portal: a line in `pkg/gamelib/source.go` once played), then Portal 2 (a later engine
  branch: survey first); a second Feral game for the Feral family layer.

## Backlog

- **Play further:** other chapters and episodes, vehicle levels, Free Play, bonus areas. Expect
  unimplemented calls to show up as crashes or drawing errors (logged once by name).
- **Speed.** Everything here must work for users with no developer tools and no per-game work.
  - **The JIT** (hot guest code translated to machine code at run time, on the user's own
    copy; `docs/JIT.md`): x86-64 and arm64 backends, in Go and assembly, on the Mac since
    October 2026. Next: host calls without leaving translated code; Apple Silicon (off there
    unless `C32_JIT=1` until verified). The interpreter's dispatch loop is near what plain Go allows
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
| 2026-10-21 | The JIT (x86-64 and arm64): translated blocks, lazy flags, memory windows, XMM in host registers; Source's sound effects in Go |
| 2026-10-22 | Source's collision traces in Go; Portal played to the ending on the Mac (a game file the game deletes now stays deleted); LEGO Star Wars' first chapter with the JIT; Portal game apps; release 0.2.0 |
