# Architecture

How catalina32 is put together, from the app a player opens down to the emulated CPU. The
engine internals (CPU tiers, shaders, renderers) are in `CONTEXT.md`; this document is about
the product around them. Parts marked *(planned)* are designed but not built yet.

## The product in one paragraph

A player downloads **Catalina32.app** and opens it. It finds their 32-bit games (Steam
library, Applications folders, or a game they drag onto it), says which ones are supported,
and makes a **game app** for each: "LEGO Star Wars Saga.app", with the game's own icon. From
then on they open that app like any other. Nothing about the original game is copied or
modified, and saves go where the original game kept them, so older saves just work.

**Generic** here means it works for players with no developer tools, no LLM and no per-game
setup. Anything that needs someone to profile scenes or regenerate code per game (such as the
ahead-of-time compiler, `cmd/c32aot`) is a developer convenience, not part of the product.

## Layers

```
Front-ends   Catalina32.app: wizard (find games, make game apps)    catalina32 CLI
                         │                                               │
Game app     <Game>.app = window app + engine + game.json (game path, checksum, profile, saves)
                         │
Host         window · Metal renderer · OpenGL renderer · keyboard, mouse,    (viewer/, Objective-C)
             controllers · audio
                         │  shared memory: framebuffer, GPU command stream or OpenGL stream,
                         │  input, PCM audio
Engine       x86 CPU (interpreter, block cache, JIT) · Mach-O loader and            (Go)
             dynamic linker · Feral games: MacDoze (Win32) and IndirectX (D3D9,
             DirectInput, DirectSound) shims, shader translation · Valve's Source
             games: the system libraries (C library, pthreads, C++ runtime, SDL2,
             OpenAL, fonts) · native replacements found by signature
                         │
Families     a porting house's runtime layer, found by signature: Feral's MacDoze  (planned);
             Valve's Source on Mac (SDL2 + ToGL)                                   (Portal)
                         │
Games        profiles: checksums, names, folders, original save location, rare quirks
```

- **Engine** (`pkg/cpu`, `pkg/macho32`, `pkg/crt`, `pkg/win32`, `pkg/d3d9`, `pkg/hlsl`,
  `pkg/d3dsm`, `pkg/msl`, `pkg/lh5`): no game addresses. See `CONTEXT.md`.
- **Source games**: `pkg/dyld` links a game's dozens of
  dylibs; `pkg/source` replaces the macOS libraries under them. The game's Direct3D-on-OpenGL
  layer (ToGL) runs emulated and its OpenGL calls are streamed to the window, which draws
  them with the Mac's OpenGL. See `docs/PORTAL.md`.
- **Families** *(planned)*: the 32-bit Mac ports of one porting house share a runtime layer.
  catalina32 replaces Feral's (MacDoze, a Win32 look-alike) at its boundary, so the entry
  points of that layer are what a game must give us. Found by signature
  (`pkg/profile/signatures*.go`, made with `cmd/sigmake`), one family covers many games with
  little or no per-game data. Today the MacDoze addresses are still a per-game table.
- **Games** (`pkg/games/<id>`): one `profile.Profile` each, picked by the executable's
  SHA-256. Ideally pure data; game-specific Go (the `NuTexRead` patch, lifted helpers) is the
  exception, and test-only code (scripted input routes, scene detection) stays out of the
  product path.
- **Host** (`viewer/`): one Objective-C executable with two modes. In a game app it is the
  game's window: it starts the engine as a child process, maps the shared memory, draws with
  Metal (`C32Metal.m`) or, for Source games, OpenGL (`C32GL.m`), reads keyboard, mouse and
  controllers, plays audio, and stops the engine when the window closes. In full screen it
  hides the mouse pointer unless the game wants it; the engine decides that from what the
  game asks for (Win32 `ShowCursor`/`SetCursor`, a Direct3D device cursor, a windowed device),
  never from which game it is. For mouse look (SDL's relative mode) the window captures the
  pointer while it is the active window. In Catalina32.app it is
  the wizard. Its output goes to `~/Library/Logs/catalina32/<game>.log`.
- **Front-ends**: the `catalina32` binary is both the engine and the command line. The wizard
  calls the command line (`list --json`, `wrap`), so all the logic (finding games, deciding
  support, building game apps, save locations) is Go and testable without a Mac.

## User flows

1. **Wizard.** Open Catalina32 → it scans the Steam library folders
   (`libraryfolders.vdf`), `/Applications` and `~/Applications`, and lists what it finds with
   a status: supported, or not supported yet. **Create App** (in `~/Applications`) → **Play**
   or **Show in Finder** (from where it can go in the Dock). **Choose game…** or dropping a game
   on the window or the Dock icon covers games elsewhere. A Valve Source game is a folder, not
   an app (Steam's `steamapps/common/Portal`, holding `hl2_osx`): the scan finds it beside the
   apps, and choosing or dropping the folder works the same way. An unsupported game gets a plain
   message and its identity for a report; it is never run on a guess.
2. **Game app.** On launch it checks the game is still where it was and unchanged (same
   checksum). If Steam updated it or the library moved, it says so and offers to find it again.
3. **Command line.**

   ```sh
   catalina32 list [--json]              # scan for games and show which are supported
   catalina32 info <Game.app> [--json]   # what catalina32 knows about this game (or a folder)
   catalina32 wrap <Game.app> [-o dir]   # make a game app (default ~/Applications)
   catalina32 run <Game.app>             # play without making a game app
   catalina32 source <folder> [options]  # a Valve Source game with your own game options
   catalina32 -game <executable>         # the engine alone (developer flow, run_on_mac.sh)
   ```

## Bundles

```
Catalina32.app/Contents/
  MacOS/Catalina32           window app (wizard mode: no game.json)
  Helpers/catalina32         engine + command line
  Resources/AppIcon.icns
  Info.plist

<Game>.app/Contents/          made by `catalina32 wrap`
  MacOS/<Game>               the same window app (game mode: finds Resources/game.json)
  Helpers/catalina32         a copy of the engine: the game app keeps working if
                             Catalina32.app is moved or deleted (about 25 MB)
  Resources/game.json        the manifest (below)
  Resources/AppIcon.icns     copied from the game's own bundle
  Info.plist                 CFBundleIdentifier com.ffmoura.catalina32.<profile id>
```

Game apps are signed ad hoc when they are made (needed on Apple Silicon; `codesign` is on
every Mac). They are created locally, so Gatekeeper doesn't quarantine them. Catalina32.app
itself, downloaded from the internet, needs a Developer ID signature and notarisation for
players to open it without a trip to System Settings.

### game.json

```json
{
  "format": 1,
  "profile": "lswsaga",
  "name": "LEGO Star Wars: The Complete Saga",
  "app": "/Users/me/Library/Application Support/Steam/steamapps/common/Lego Star Wars Saga/LEGO Star Wars Saga.app",
  "executable": "Contents/MacOS/LEGO Star Wars Saga",
  "sha256": "c6ddb3bb…",
  "data": "/Users/me/…/Lego Star Wars Saga/LEGOStarWarsSagaData",
  "saves": "/Users/me/Library/Application Support/Feral Interactive/LEGO Star Wars Saga/…"
}
```

A Source game's manifest has `"kind": "source"`, its folder as `app`, `hl2_osx` as the
executable, `bin/engine.dylib`'s checksum, the game's options (`"args": ["-game", "portal"]`)
and its overlay as `saves`. The window then draws with OpenGL instead of Metal. Which Source
games are supported is a table in `pkg/gamelib/source.go`: Steam's app ID
(`steam_appid.txt`), the mod folder and the engine checksums tested.

## Saves

The game's own code writes the save files, so they are compatible with the original game.

- **Default: the original game's save folder** (a profile field). Saves made before macOS
  Catalina load as they are, and the folder Steam syncs stays the one in use. macOS doesn't
  stop non-sandboxed apps (the Steam versions of these games, and catalina32) from using each
  other's folders in `~/Library/Application Support`; only sandboxed apps' containers
  (`~/Library/Containers`, the Mac App Store versions) ask the player for permission first.
- **Backup.** Before a game starts, if the newest backup is more than a week old, the folder
  is copied to `~/Library/Application Support/catalina32/Backups/<profile>/<date>` (the five
  newest are kept).
- **Separate folder** (`~/Library/Application Support/catalina32/<game>/SaveData`): for a
  game whose original location isn't known, or by choice (`C32_SAVE_ROOT` overrides both).
- **Source games** kept their saves inside the game's folder, which catalina32 never writes:
  everything they write goes to an overlay, `~/Library/Application Support/catalina32/<folder
  name>`, and reads see it merged over the game's folder, so older saves there still load
  (`docs/PORTAL.md`). The overlay is what gets backed up.

## Building

- **Developers:** `go build ./cmd/catalina32` (the engine and CLI), the viewer with clang
  (`viewer/*.m`), tests with `go test ./...`. `docs/DEBUGGING.md` has everything else.
- **The app:** `scripts/build-app.sh [--arch native|x86_64|arm64|universal] [--with-aot]`
  builds both and assembles `build/Catalina32.app`. A build for your own use may include the
  AOT code generated from your own copy of a game (`--with-aot`); a build for others must not
  (it is a translation of the game's code), so by default it is left out (`-tags noaot`).
- **Releases:** `scripts/make_release.sh [--arch universal] [--notes file] [--publish]`, from a
  clean master: builds the app, refuses an engine that contains AOT-compiled game code, checks
  the architectures and signature, zips it with `ditto` and writes the release notes in
  `build/release/`. `--publish` then publishes exactly that zip and those notes, without
  rebuilding, once it has checked they come from the current commit and the zip is unchanged:
  it tags `v<version>` and creates a GitHub pre-release (`--final` for a normal one). The
  version is `cmd/catalina32/cli.go`'s.

## Distribution and legal

- Players need their own copy of each game. catalina32 never ships, copies or modifies game
  files, and game apps refer to the installed game in place.
- Not affiliated with Feral Interactive, LucasArts, Traveller's Tales or any game publisher;
  game names in game apps come from the player's own copy.
- Before a public release: a license for this code, and a Developer ID for notarisation.
