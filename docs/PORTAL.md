# Portal (and Valve's Source games): findings and plan

Status: **planning**. Survey of the user's copy at `/workspace/Portal` (the Steam build, 4.1 GB;
`steam_appid.txt` 400). Nothing of it is committed or uploaded. Portal 2 has not been surveyed.

## What the game is

- **Not a Feral port.** Valve made its Mac Source games itself. There is no MacDoze layer; the
  game calls macOS through a few libraries of its own.
- **No `.app` bundle.** A Unix-style folder: `hl2.sh` sets `DYLD_LIBRARY_PATH=bin` and runs
  `hl2_osx`, a 9 KB launcher that `dlopen`s `bin/launcher.dylib` and calls into it.
- **About 40 i386 dylibs** in `bin/` (engine, materialsystem, shaderapidx9, vgui, filesystem,
  vphysics, SDL2, ToGL, Miles, Chromium (`libcef`, 37 MB)...) and `portal/bin/` (client,
  server). Only `libsteam_api.dylib` is universal (i386, x86_64 and arm64 in one file).
- **Modules talk through C++ interfaces**, not linked symbols: most export only
  `CreateInterface` (2–3 symbols), so linking between them is simple.
- **Game data** is in VPK archives (`hl2/*.vpk`, `portal/*.vpk`); compiled shaders (`.vcs`) are
  inside them.

### Its boundaries (imports counted over all game binaries except `libcef` and tools)

| Layer | Who calls it | Size | What we would do |
|---|---|---|---|
| **ToGL** (`libtogl`: Direct3D 9 on OpenGL) | shaderapidx9, materialsystem | 95 functions: `IDirect3DDevice9` (58), `IDirect3D9`, textures, buffers, queries, `Direct3DCreate9` | Intercept its exported C++ methods and map them onto our Direct3D 9 implementation → Metal. Shaders arrive as D3D9 bytecode (SM 2.0/2.0b/3.0): extend `pkg/d3dsm` |
| **SDL2** (`libSDL2`) | launcher, engine, inputsystem... | 70 functions: window, events, GL context, mouse, joystick/game controller, haptics, message boxes | Implement natively (not emulate): our window app already does windows, keyboard, controllers; add the mouse (Portal needs mouse look: relative mode) |
| **OpenAL** | engine | 28 functions | A small OpenAL (streaming sources/buffers) into our audio ring |
| **Steam** (`libsteam_api`) | 7 modules | 24 functions (`SteamAPI_InitSafe`, callbacks, `SteamInternal_*` interfaces) | Bridge to the real, running Steam client through the 64-bit slice of the same `libsteam_api.dylib`; no DRM bypass |
| **libSystem** (C library, POSIX) | 37 binaries | 322 functions: threads 38, files 53, strings 72, math 29, time 16, network 15, memory 9, mach 11, `dlopen`/`dlsym`... | Extend `pkg/crt`; pthreads on our green threads; files through `pkg/win32`'s path mapping, generalised |
| **C++ runtime** (libstdc++, libc++) | 33 binaries | 105 functions, including exceptions (`__cxa_throw`, `_Unwind_Resume`) | New shims; exceptions need a DWARF unwinder over the guest's `__eh_frame` (the loader already reads FDEs) |
| **Fonts** (ATSUI + CoreGraphics) | vguimatsurface | 27 functions | ATSUI only ever existed for 32-bit apps: replace it, rasterising glyphs in Go from the Mac's system fonts (Verdana, Tahoma...) |
| **Launcher GL probes** (CGL, immediate GL) | launcher | 34 functions | Report one capable renderer; stub the rest |
| **QuickTime** | video_quicktime (menu videos) | 56 functions | Fail its init: the game runs without background videos |
| **Chromium** (`libcef`, chromehtml) | HTML panels | 1248 imports | Don't load it; the game should treat HTML panels as unavailable |
| Objective-C | SDL2 (replaced), engine 2, launcher 1, ToGL (replaced) | `objc_msgSend`, `sel_registerName` | A dispatcher for the few selectors used |
| Others | — | CoreFoundation 49, CoreServices 42, IOKit 22, AudioToolbox 18, Carbon 17, ForceFeedback 13... | Mostly small stubs; handle as they come up |

## How it fits catalina32

A second **family**: "Valve Source on Mac (SDL2 + ToGL)". Everything above except a profile
is shared by Valve's Mac Source games of this generation (Half-Life 2 and its episodes, Portal,
probably Counter-Strike: Source, Day of Defeat: Source, Half-Life 2: Deathmatch). Portal 2 and
Left 4 Dead 2 are a later engine branch: survey before assuming.

New pieces, in order of dependency:

1. **A dynamic linker** in `pkg/macho32`: load many images into the guest address space, apply
   rebases, resolve binds (`LC_DYLD_INFO` opcodes, lazy and weak binds, two-level namespace,
   `@loader_path`/`@executable_path`), exports tries, run each image's initialisers, and
   implement `dlopen`/`dlsym`. Today the loader takes one thin executable with everything
   statically linked (LEGO Star Wars).
2. **A bigger guest address space.** The arena is 512 MB (`cpu.ArenaSize`); Source wants much
   more (40 libraries plus a large heap). Raise it towards 2–3 GB (reserve-on-demand memory) and
   check every place that assumes the old size.
3. **Families and game identity.** Today a game is picked by its main executable's SHA-256 and
   must be an `.app`. `hl2_osx` is the same tiny launcher in every Source game, so identify Source
   games by folder (`steam_appid.txt`, `gameinfo.txt`, the checksum of `engine.dylib` or the game
   module) and teach `pkg/gamelib` (scan, identify, wrap) that a game can be a folder.
4. **The C library and C++ runtime shims** (table above), with pthreads mapped onto the green
   threads (Source uses worker threads heavily).
5. **Boot:** `hl2_osx` → `launcher.dylib` → engine init, with stubs for the launcher's GL
   probes, QuickTime, Chromium, Objective-C.
6. **SDL2 natively**, including mouse input (relative mode, deltas through the shared memory).
7. **Graphics through ToGL's interface** onto `pkg/d3d9`: the existing Direct3D 9 implementation
   is shaped around COM vtables (Feral's); add a front end for ToGL's C++ methods (i386
   Itanium ABI: `this` is the first stack argument). Extend shader support to SM 2.0b/3.0.
8. **Audio** (OpenAL), **fonts** (ATSUI replacement).
9. **Steam bridge.**
10. **Speed:** Source is far heavier than LEGO Star Wars. The interpreter alone will likely be
    too slow to enjoy; plan for the JIT (`docs/JIT.md`), and use developer AOT builds of the hot
    functions to keep testing pleasant meanwhile.

## First experiments (cheap, in this order)

Each answers a question that decides the plan; stop and re-plan if one surprises.

1. **Link everything:** load all dylibs, apply rebases and binds, and count unresolved imports
   per library (expect exactly the table's system functions). Validates the linker.
2. **Initialisers:** run every image's static initialisers with logging stubs. Shows the C++
   runtime needs early (`__cxa_atexit`, guards, locales, iostreams).
3. **To `LauncherMain` and engine init**, logging every unimplemented call: the real order of
   work.
4. **Steam:** with `SteamAPI_InitSafe` returning failure, does Portal still start (it may run
   offline), or stop with "Steam must be running"? Decides whether the Steam bridge is needed
   before a first menu.
5. **First frame:** ToGL's device created and a frame presented through Metal: the main menu.

Milestones: a rendered main menu (big: steps 1–7); then the first test chamber playable with
keyboard, mouse and sound; then the Steam bridge (achievements, cloud saves); then speed.

## Legal and practical

- Same rules as LEGO Star Wars: the player's own copy; no game files, AOT code or translations
  committed or shipped.
- Steam: talk to the real Steam client; never stub or bypass its checks in anything shipped.
- Getting the files: Steam hides 32-bit Mac games on Catalina and later; this copy came from
  the user. A Mac depot download with SteamCMD's platform switch is the usual way.
