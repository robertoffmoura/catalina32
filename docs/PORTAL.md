# Portal (and Valve's Source games)

Status (2026-10-21, branch `source`): **playable, slowly.** On the user's Mac (Intel, Radeon
Pro 560X) Portal plays from the menu through the first chambers: New Game and Load Game,
mouse look, keyboard, a controller (connected at launch or later), sound and music, menus and
HUD text, and portals drawn correctly. No known bugs except speed: about 10 million guest
instructions per frame in a chamber against an interpreter doing ~75 million a second, so a
few frames per second interpreted. The JIT (`docs/JIT.md`, branch `jit`) runs gameplay at
about 280 million instructions a second headless (3.7×), with a byte-identical GL stream;
it hasn't been tried on the Mac yet.

## Running it

On the Mac, from the repository: `scripts/run_source.sh [path/to/Portal] [game options]`
(default: the `Portal` folder next to the repository). It builds the engine and the window,
then runs `catalina32 source --window <window> <folder>`, which finds the mod (`-game
portal`) and starts both. What the game writes (settings, saves) goes to
`~/Library/Application Support/catalina32/<folder name>`, never into the game folder; reads
see that folder merged over the game's (so a saved `cfg/config.cfg` shadows the game's own,
but listing `cfg/` still shows its chapter files). A game file the game deletes is recorded as
deleted (`.catalina32-deleted` in that folder) and hidden until the game writes it again. This
matters for saves: Source empties `save/` when a game starts or loads, and a level whose state
(`map.hl1`-`hl3`) is still there is restored from it instead of starting afresh. A Portal folder
copied from an old Mac held escape_02's state from 2021, past GLaDOS's door; while it couldn't
be deleted, every save carried it and walking into escape_02 restored it, door shut. Saves made
before the fix still carry it: `map escape_02` starts the level afresh. There is no game app yet
(see Next).

## How it works

The game's own code runs, all of it, in the emulator, and the system underneath it is
replaced:

- **Loading** (`pkg/dyld`): a dynamic linker for i386 Mach-O: 27 images and their 1600 static
  initialisers, two Mach-O generations (`LC_DYLD_INFO` opcodes and export tries; classic
  relocations for SDL2 and Miles), `dlopen`/`dlsym` and the old `NSModule` calls.
- **Memory** (`pkg/source/machine.go`, `pkg/cpu`): a 3 GB guest address space reserved with
  mmap: images 1–208 MB, heap 256 MB–3 GB, stack at the top.
- **System libraries** (`pkg/source`): the C library and POSIX (`libc.go`), pthreads on the
  green threads (`pthread.go`), the C++ runtime and RTTI (`cxx.go`), CoreFoundation, Gestalt,
  CG display queries and the Objective-C stubs (`frameworks.go`), IOKit's description of the
  GPU (`iokit.go`), Multiprocessing Services and iconv (`audio.go`), Steam absent (`steam.go`).
- **Graphics.** ToGL (the game's Direct3D 9 on OpenGL) stays in the guest; its OpenGL calls
  become a stream (`glspec.go` lists the calls, `glremote.go` encodes them) that the window
  replays with the Mac's OpenGL 2.1 (`viewer/C32GL.m`; its decoder `viewer/C32GLCalls.h` is
  generated: `go generate ./pkg/source`). The game draws into an offscreen "window" of the size
  it asked for, which each present letterboxes into the real one. Questions the game asks GL
  (uniform locations, texture readbacks, a few limits) wait for the window's answer; the GL
  strings the game sees are the window's.
- **Text** (`hostfont.go`, `fonts.go`, `patches.go`): vgui's fonts are drawn by catalina32 from
  the game's fonts or the Mac's, replacing ATSUI (32-bit only). This also fixes the current
  Mac build's broken font module (below).
- **Input** (`sdl.go`, `input.go`, `pads.go`): SDL2 implemented natively: the window, events
  (keyboard, mouse, focus), relative mouse mode with the pointer captured by the window, and
  the game controller API from the window's controller records.
- **Sound** (`openal.go`): OpenAL mixed into the window's audio ring (10 ms ticks live; on the
  instruction clock headless). Miles (the engine's sound decoder) runs emulated, including its
  MP3 plug-in (MMX code).

## Testing and seeing it

Runs are deterministic headless. With `C32_SOURCE_DIR=<Portal>` set, `go test ./pkg/source`
also runs the game tests (minutes each):

| Test | Checks |
|---|---|
| `TestBoot` | boots and runs `C32_STEPS` instructions; the developer harness (switches in `docs/DEBUGGING.md`) |
| `TestPlayIO` | first chamber: mouse look (the view turns), controller, sound reaching the window |
| `TestController` | a controller connected at launch moves the player and its triggers fire the portal gun (`+attack`, `+attack2`); also after loading a save (`C32_CTL_LOAD`) |
| `TestMenuSound` | the menu's MP3 music decodes through Miles' plug-in |
| `TestNewGameDialog` | the dialog lists the chapters, draws their pictures, and Start begins chapter 1 |
| `TestRendererClipMode` | the launcher sees an AMD GPU and turns off native clip-vertex mode |
| `TestSndDSPAgainstEngine` | the sound effects in Go against `engine.dylib`'s own code, on random delay lines, reverbs, diffusors and filters |
| `TestCMTraceAgainstEngine` | the collision traces in Go against `engine.dylib`'s own code, on random collision trees, brushes and box and point traces |

To **see** frames on Linux, `tools/glreplay` replays the stream through the window's own
decoding code into Mesa and saves PNGs; it can imitate AMD's Mac drivers
(`GLREPLAY_CLIP=position`). `C32_CMDS` types console commands in at chosen points (open a
portal, move the camera). Recipes: `docs/DEBUGGING.md`.

**Measuring large areas.** Speed problems show where the player looks into a large room full of
objects, so measure there: load a map, cheat, teleport, then compare builds with
`C32_FRAMESTATS` over alternating runs. The views used so far (`escape_02`, loaded by about 12
billion instructions):

```
C32_ARGS="+map escape_02" C32_STEPS=14100000000 C32_CMDS="12000000000:sv_cheats 1;noclip;god;\
hud_draw_fixed_reticle 0;setpos 10000 1663 420;setang 5 -159 0|\
12700000000:setpos -1832 -3108 -7364;setang 5 10 0|13400000000:setpos 7274 970 465;setang 5 -10 0"
```

GLaDOS's chamber, the storage room with the hanging spheres, and the foggy hall. Pick views from
screenshots (`glreplay`): positions taken from the map's entities often face a wall, or lie
outside the level. `testchmb_a_14` at `setpos 1504 384 1209; setang 5 60 0` faces a chamber
with turrets and platforms (`god`: the turrets shoot).

## What the game is

- **Not a Feral port.** Valve made its Mac Source games itself. There is no MacDoze layer; the
  game calls macOS through a few libraries of its own.
- **No `.app` bundle.** A Unix-style folder: `hl2.sh` sets `DYLD_LIBRARY_PATH=bin` and runs
  `hl2_osx`, a 9 KB launcher that `dlopen`s `bin/launcher.dylib` and calls into it.
- **About 40 i386 dylibs** in `bin/` (engine, materialsystem, shaderapidx9, vgui, filesystem,
  vphysics, SDL2, ToGL, Miles, Chromium (`libcef`, 37 MB)...) and `portal/bin/` (client,
  server). Only `libsteam_api.dylib` is universal.
- **Modules talk through C++ interfaces**, not linked symbols: most export only
  `CreateInterface`.
- **Game data** is in VPK archives (`hl2/*.vpk`, `portal/*.vpk`); compiled shaders (`.vcs`) are
  inside them.
- The user's copy: the Steam build of Nov 25 2024 (the 20th-anniversary update), 4.1 GB,
  `steam_appid.txt` 400, at `/workspace/Portal`. Nothing of it is committed or uploaded.

### Its boundaries

| Layer | Size | How catalina32 handles it |
|---|---|---|
| **ToGL** (Direct3D 9 on OpenGL) | 95 functions | kept in the guest; its ~150 OpenGL functions are streamed to the window |
| **SDL2** | 70 functions | implemented natively (`sdl.go`, `input.go`, `pads.go`); haptics unsupported |
| **OpenAL** | 28 functions | a mixer into the window's audio ring |
| **Steam** (`libsteam_api`) | 24 functions | `SteamAPI_InitSafe` fails: the game runs offline. A bridge to the real Steam client is for later |
| **libSystem** (C library, POSIX) | 322 functions | `libc.go`, `pthread.go`, `dl.go` |
| **C++ runtime** | 105 functions | `cxx.go`; no unwinding: a thrown exception stops the game with a backtrace (none seen so far) |
| **Fonts** (ATSUI, CoreGraphics) | 27 functions | replaced by host fonts |
| **Launcher GL probes** (CGL, IOKit) | 34 functions | one renderer, described as the window's GPU |
| **QuickTime** (menu videos) | 56 functions | its init fails: no background videos |
| **Chromium** (`libcef`) | 1248 imports | never loaded |
| Objective-C | `objc_msgSend` | answers nil (logged once per selector) |

## Things learned on the way

Each of these cost a bug; most are generic to Source on Mac.

- **ToGL needs the OS version:** it refuses to create a device until `Gestalt` answers
  (Mojave, the last macOS with 32-bit apps).
- **Steam's flat API** accessors must return the caller's context block, not NULL.
- **Miles** waits on Multiprocessing Services semaphores, finds its plug-ins next to itself
  through `NSModuleForSymbol`, and decodes MP3 with MMX and FWAIT.
- **The font module of this build is broken:** `CFontManager` was compiled against a newer
  font interface than the Mac font class, so on a real 32-bit Mac this build would most likely
  crash at startup. Host fonts replace it.
- **`dynamic_cast`** must follow base classes (`__si_class_type_info`, `__vmi_class_type_info`);
  without it GLaDOS's first line crashed.
- **Focus:** Source ignores mouse motion and mutes sound until the window reports focus.
- **Sound buffers** arrive through `alBufferDataStatic`, an extension the game looks up with
  `alGetProcAddress`.
- **The GPU's vendor matters.** The launcher's renderer table reads the PCI vendor from IOKit,
  and on AMD turns off "native clip vertex" mode: AMD's Mac drivers clip against
  `gl_Position`, so ToGL moves portal clip planes into that space itself. Without the vendor,
  walls and floors seen through portals were cut away on an AMD Mac.
- **Controllers at launch** (`pkg/source/pads.go`, `announcePads`). Three constraints:
  - Inputsystem enumerates controllers before the client exists. The client owns the `joystick`
    setting, and inputsystem caches its references to that setting the first time it sees a
    controller, so a controller found that early can never switch joystick input on.
  - Bindings are saved by name, and controller buttons are named differently once inputsystem
    knows an Xbox-style controller is there (`R_TRIGGER` against `Z AXIS NEG`).
  - `config.cfg` may set `joystick 0`, as the shipped one does.

  So a controller connected at launch is shown to the game just before the engine reads its
  configuration (`Host_ReadConfiguration`), after the client loaded. Then, at the first event
  poll, it is shown unplugged and plugged in again, which switches joystick input on.
  - Shown only at the first poll, the saved controller bindings didn't parse ("isn't a valid
    key"), so from the second launch on the triggers (the portal gun), A and more did nothing.
  - `TestController` with a kept `C32_OVERLAY` and `C32_CMDS=8800000000:host_writeconfig`,
    run twice, checks that.
- **Saved settings shadow the game's:** listing a folder must merge the game's files with
  those the game wrote, or the New Game dialog finds no chapters once `cfg/config.cfg` is saved.
- **The menu's loading picture is random** once every chapter is unlocked (the shipped
  `config.cfg` says `sv_unlockedchapters "15"`): 2 of 13 draws show the cake.
- **Large rooms are slow partly because of their reverb.** The engine picks a room's sound
  preset by its size, and a large room's reverb runs every mixed sample through up to 12
  delay lines, one `DLY_GetNext` call per sample per line: about a fifth of all the
  instructions in GLaDOS's chamber. `pkg/source/snddsp.go` runs those functions in Go, bound
  by symbol and only when their code bytes are the ones it follows; `C32_DSPCHECK=1` compares
  every call with the game's code.
- **GLaDOS's chamber is slow mostly because of collision traces.** Its turrets, lasers and
  physics trace boxes through the map all the time: the walk down the collision tree and the
  clipping against each brush (`CM_RecursiveHullCheckImpl`, `CM_TraceToLeaf`,
  `CM_ClipBoxToBrush`, `IntersectRayWithBoxBrush`) were a third of the translated code there,
  3–5% elsewhere. `pkg/source/cmtrace.go` runs them in Go, box and point traces, the same
  way as the sound effects: bound by symbol and code bytes, bit for bit the game's results
  (each single- and double-precision step as the game's, products rounded before the sums so
  that arm64 doesn't fuse them), leaves with displacements handed to the game's code;
  `C32_TRACECHECK=1` compares every trace. GLaDOS's chamber: 29 M instructions a frame to 18 M,
  about 1.25× faster.

## Next

1. **Speed:** play on the Mac. Measured on 22 October 2026 (`e2fc8ad`, before the speed work,
   against `dabd540`, run side by side, median frame): GLaDOS's chamber 154 → 80 ms (1.9×), the
   storage room 102 → 64 ms (1.6×), the hall 73 → 51 ms (1.4×), `testchmb_a_14` 81 → 73 ms
   (1.1×). What is left is spread out: translated code (about 60% of a frame), host calls
   (about 59,000 a frame, each a return to Go), the forms the cache doesn't decode
   (`docs/JIT.md`, Ideas), Source's sound mixing (6–14% of the translated code), the spatial
   partition's voxel walks (3–4%).
2. **Checked on 22 October 2026:** every story map loads and plays its first seconds headless
   (`-condebug +developer 1`) with no halts and no NULL-pointer stores; what the game's console
   complains about is missing from the shipped files too (`soundscapes_portal.txt`, the sign
   frames, `func_portal_volume`, HL2's turret sounds). Live, the sound runs out only on frames
   longer than about 93 ms: the engine's OpenAL device keeps 16 buffers of 256 frames queued, a
   constant (`snd_mixahead` doesn't change it), so it breaks up the same on a slow Mac.
3. **A game app for Portal:** teach `pkg/gamelib` (scan, identify, wrap) that a game can be a
   folder, identified by `steam_appid.txt`, `gameinfo.txt` and its engine's checksum
   (`hl2_osx` is the same launcher in every Source game).
4. **The family layer:** "Valve Source on Mac (SDL2 + ToGL)" covers Half-Life 2 and its
   episodes, probably Counter-Strike: Source, Day of Defeat: Source and Half-Life 2:
   Deathmatch. Portal 2 and Left 4 Dead 2 are a later engine branch: survey before assuming.
5. **A Steam bridge** (achievements, cloud saves) through the 64-bit slice of the same
   `libsteam_api.dylib`, talking to the real Steam client; no DRM bypass.
6. Smaller: controller rumble; Apple Silicon (its GL vendor, "Apple", keeps native
   clip-vertex mode, which is untested there).

## Legal and practical

- Same rules as LEGO Star Wars: the player's own copy; no game files, AOT code or translations
  committed or shipped.
- Steam: talk to the real Steam client; never stub or bypass its checks in anything shipped.
- Getting the files: Steam hides 32-bit Mac games on Catalina and later; this copy came from
  the user. A Mac depot download with SteamCMD's platform switch is the usual way.
