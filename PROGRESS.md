# Progress

Status and open work. Read `CONTEXT.md` first. History lives in `git log` (commit messages
explain each change); keep this file to what is true now.

## Status (October 2026)

| Area | State |
|---|---|
| Boot, menus, saving and loading | Works. Saves land in `~/Library/Application Support/catalina32/LEGO Star Wars Saga/SaveData` and load after relaunch. |
| Gameplay | Cantina hub and all of Episode I Chapter 1 (Negotiations) played through by the user without crashes. Nothing past that has been tried. |
| Rendering (Metal) | Default on the Mac. The user confirmed: room and door transitions, pause-menu background, cut-scenes, TC-14's chrome (cube maps), the `metal-gaps` work (mipmaps, scissor, viewport reset, fog). One open bug (double vision, below). |
| Rendering (software) | Reference renderer for headless runs and Linux. Doesn't draw render targets other than screen size (bloom buffers). |
| Audio | Works (DirectSound mixing → shared memory → AudioQueue). |
| Input | Keyboard and controllers (USB/Bluetooth, players 1 and 2, hot-plug) confirmed on the Mac. |
| Speed | Metal: levels 35–50 FPS, Cantina ~57. The emulator is the limit there (~110M x86 instructions/s; 60 FPS needs ~170M). Short hitches when new shaders compile or textures upload. |
| Platforms | Tested on an Intel MacBook Pro only. Apple Silicon builds exist but won't be tested (the user's decision). |

## Open

1. **Double vision in Negotiations** (being worked on in a separate session).
   - **Symptom:** from the room with the blue energy-beam pillars on, the image looks doubled
     and blurry; faces and hands are overexposed (clothing is not).
   - **Ruled out:**
     - the shader translation (bit-exact against the bytecode);
     - sampler modes;
     - constant register assignment (including the indexed `skinMatrix[24]`);
     - light constants;
     - `incandescentGlow`;
     - blend indices;
     - lock/discard staleness.
   - **Fixed along the way** (not the cause): `SetViewport` was ignored, which made the bloom
     chain smear.
   - **Next:** find the draw that paints the overexposed pixels with the software renderer's
     per-pixel footprints (`C32_DRAW_LOG=<frame>`; see `docs/DEBUGGING.md`), using a recorded
     session (`run_on_mac.sh --record`) to reach the room headless.
2. **Unmerged branches.** The newest Metal work is on `metal-gaps`, ahead of `debug`:
   - scissor rectangles;
   - viewport reset on `SetRenderTarget`;
   - mipmaps;
   - fixed-function fog.

   The user tested it on the Mac: no crashes or new bugs; the double vision is still there.
   `docs` contains `metal-gaps`. `debug` is ahead of `master`. Nothing is pushed.
3. **Two save locations.** `run_on_mac.sh` saves under `.../catalina32/LEGO Star Wars Saga/`,
   the bare binary under `.../catalina32/lswsaga/`. Pick one.

## Backlog

- **Play further:** other chapters and episodes, vehicle levels, Free Play, bonus areas. Expect
  unimplemented calls to show up as crashes or drawing errors (logged once by name).
- **Speed** (on hold until the user asks):
  - compile shaders in the background and keep Metal's shader cache between runs (fixes the
    hitches; generic);
  - choose the AOT function list automatically from a profile instead of the hand-picked
    list in `pkg/games/lswsaga/aot.go`;
  - better AOT code (values kept in registers, flags skipped across blocks);
  - a JIT, much later if ever (likely `pkg/cpu` rewritten in Rust behind the same
    interface).
- **Rendering gaps:**
  - mip chains for cube maps (they have one level; D3DX would generate more);
  - the game's own mip levels where a file has them (the viewer generates them instead);
  - compressed textures whose sizes aren't multiples of 4 are sent uncompressed;
  - fixed-function vertex fog without a vertex shader (not used by this game);
  - the legacy heuristic renderer (`C32_REAL_SHADERS=0`) still mis-places skies.
- **Packaging:** `run_on_mac.sh --build-app` makes a `.app`, but it isn't polished for
  non-developers and can't record sessions.
- **General tool:** only this game has a profile. Game-specific logic still inside
  `pkg/d3d9`: the legacy renderer's heuristics (2D detection by vertex stride, the skin
  matrix and c0..c3/c16..c19 layout in `transformVertex`) and the fixed 1280×720 back buffer.

## Milestones

| Date | Milestone |
|---|---|
| 2026-10-16 | Runtime snapshot: interpreter, Win32/D3D9 shims, green threads, software renderer; title screen and menus |
| 2026-10-16/17 | Cantina hub, audio (Ogg Vorbis music, sound effects), Episode I entry, controllers, real shaders (the game's HLSL and bytecode), AOT-compiled hot functions, decoded-instruction cache, pipelined parallel renderer |
| 2026-10-18 | Two code-review passes (crash safety, cross-host determinism, reference-counted resources, saves); HLSL and bytecode translation to Metal; GPU command stream; Metal viewer in the repository; record/replay journal |
| 2026-10-19 | White-screen fixes (copies into textures, compressed uploads); SetViewport; cube maps; scissor, viewport reset, mipmaps, fog (`metal-gaps`); docs split into `CONTEXT.md`, `PROGRESS.md`, `docs/` |
