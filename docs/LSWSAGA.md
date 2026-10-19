# LEGO Star Wars: The Complete Saga — what we know about the game

Facts about this binary that took effort to find and still matter. Profile id `lswsaga`
(`pkg/games/lswsaga`). Addresses are guest virtual addresses in the i386 executable.

## Startup and engine

- **Entry.** `WinMain` 0x00209d10 calls `NuEngineMain` 0x001607c0; the main loop is
  0x00160e10–0x001636df.
  - The executable has static initialisers (`__mod_init_func`); only those below
    `ModInitLimit` (0x002e3be0) are the game's, and catalina32 runs them before WinMain.
- **Data.** The game mounts its `.DAT` archives from `LEGOStarWarsSagaData` and streams
  `LEVELS\MAP` and `GAME.DAT`. It believes it is `C:\LEGOStarWarsSaga\LEGOStarWarsSaga.exe`.
- **Threads.** A background loader (`BGWorker` 0x0015ed40) and an I/O thread (`IOWorker`
  0x001e79d0) run as green threads. They switch on Sleep, blocking waits, contended critical
  sections and preemption every N instructions; switches happen only at `NestDepth == 0`, so
  `main` sets `vm.NestDepth = -1` to count WinMain as depth 0.
  - `0x001e7f48` (`g_ActiveSpinThreads`) is initialised to 1 so IOWorker sleeps instead of
    spinning.
  - The loader threads use a long quantum (`SetPreemptInterval`); that is what makes level
    loads fast.
- **Patch.** `NuTexRead` (0x00078df0) is patched at 0x00079088 (6× NOP) so scene textures in
  `.GSC` files with `flags & 0x3c0c` are not skipped (`engine.go`).
- **Native replacements.** A few hot functions are lifted to Go (`lifted.go`: vector/matrix
  helpers, `NuSqrt`, string compares).
  - libogg/libvorbis are statically linked; their fast paths are found by byte signature
    (`pkg/profile/signatures.go`), and the inverse MDCT runs natively (`pkg/crt`).
  - Hot game functions are AOT-compiled; the list (from guest profiles of Episode I) is the
    `go:generate` line in `aot.go`.
  - **Level data decompression** runs natively (`lzhuf.go`, `pkg/lh5`). The game streams
    LHA `-lh5-` compressed blocks (ar002's decoder, 32-bit bit buffer) through
    `NuLZHUF_Decompress` 0x002734f0 (src, dst, srcSize, dstSize) whenever the player moves
    between areas; emulated, that froze the frame rate for seconds. The decoder's state is in
    globals (addresses in `lzhuf.go`); the native version leaves them exactly as the game's
    code would, and hands input the game would mis-decode back to it.
- **Trace sensitivity.** The adapter description string must keep its length (26
  characters, "NVIDIA GeForce GTX 1080 Ti"). A different length shifts the startup
  instruction count, and with it every later preemption point, which breaks comparisons with
  older runs.

## Scenes

- **Detection.** The profile's `SceneHook` marks two moments, counting textures the game loads
  as cube maps:
  - `FirstMenuFrame`: the first interactive menu, when ≥5 cube maps are loaded and the frame
    has ≥10 draws.
  - `FirstGameFrame`: the first 3D gameplay draw, when ≥8 cube maps are loaded.

  Scripted routes time their key presses from these.
- **Headless `episode1` route timeline** (frame numbers):
  - title around 140–280;
  - Cantina playable around 490;
  - opening crawl around 840–1140;
  - pre-level cutscene 1150–1225;
  - Negotiations from about 1250.

## Input

- **Keyboard.** Through DirectInput 8 (`GetDeviceState(256)`, DIK codes). Default layout
  (table at 0x008e985c):
  - **Player 1:** `W A S D` move; `U` jump/confirm; `H` attack; `J` special/back/Force;
    `K` switch character; `Enter` pause/start; also `LCtrl` and `Space`.
  - **Player 2:** arrow keys, then `[ ] \ ' P ;` and `Backspace`.
- **Controllers.** The game enumerates controllers **once at startup** through DirectInput
  (`EnumDevices`, class GAMECTRL) and treats a device as an XInput pad only via that path.
  - The engine therefore reports every viewer-announced pad as an Xbox 360 controller
    (VID 045E, PID 028E).
  - With a viewer present, slots 0 and 1 are always announced, so pads plugged in later
    still work.
  - Whichever device drives the menus becomes player 1.

## Rendering

- **Shaders.**
  - Most draws use HLSL uber-shaders that the engine generates at runtime (`ps_3_0`/`vs_3_0`,
    with a `COLOR_FACTOR 2.0` convention: `texture × vertex colour × 2`, saturated). There
    are about 150 sources on the Episode I route.
  - Effects (light-sabre glows, studs, sparkles, the post-process composite) use 24 bytecode
    shaders (vs_1_1, ps_1_1, ps_2_0) embedded in the executable.
- **Textures.** All game textures load through D3DX from DDS files:
  - `D3DXCreateTextureFromFileInMemory(Ex)`: about 1650 loads by the Negotiations level, no
    `LockRect` at all.
  - About 1500 of the files have no mip levels of their own, but the game asks for the
    default full chain (D3DX generates it). About 87% of texture samples use linear mip
    filtering, which is why the GPU path generates mip chains.
  - Formats: DXT1 and DXT5 mostly, a few DXT3.
- **Cube maps.** 15 DDS cube maps (DXT3/DXT5, 64–256 px, no mips), loaded with
  `D3DXCreateCubeTextureFromFileInMemory`. 18 of the HLSL shaders sample them
  (`envmap_samplerCube`: chrome droids such as TC-14, shiny surfaces). None of those shaders
  is a skinned character shader.
- **Render targets and post-processing.**
  - In levels the scene is drawn into a 1280×720 texture, then composited onto the back
    buffer by a `ps_2_0` pass.
  - A bloom chain shrinks the scene into 512×512 buffers (`StretchRect`), blurs it by
    ping-ponging between two of them, and blends it back. In some rooms it uses sub-rectangle
    viewports.
  - Screen captures for transitions and the pause menu are `StretchRect`s into textures.
  - The engine clears depth mid-frame (before the HUD, between the two scenes of a door
    transition).
- **Things the game never does** on the Cantina and Episode I routes: enable fog, enable the
  scissor test, lock textures, draw without shaders, or use a viewport other than the whole
  target on the default routes.

## Saves

- The game asks `SHGetFolderPath` for its save folder and gets `C:\SaveData`. Everything under
  it maps to the save root, inside `LucasArts/LEGO Star Wars - The Complete Saga/`.
- Writes to game data files go to a copy-on-write overlay (`SaveRoot/DataOverlay`).
- Settings written through the registry-style calls (`MacdozeReg::WriteDword`) are discarded.
  The user is fine with settings resetting each launch.
