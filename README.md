# catalina32

Play 32-bit Mac games on modern macOS.

macOS Catalina (10.15) dropped 32-bit apps, and with them a whole library of Mac games that
players still own. catalina32 runs those games' original code on today's Macs: it emulates
the 32-bit x86 code, replaces the old system libraries underneath it, and draws with the
Mac's graphics (Metal, or OpenGL for games written for it).
You need your own copy of each game; catalina32 never copies or changes it.

> **Status: early.** Two games are supported. Download the app from
> [Releases](https://github.com/robertoffmoura/catalina32/releases), or build it yourself
> (see For developers).

## Supported games

| Game | Version |
|---|---|
| LEGO Star Wars: The Complete Saga | Feral Interactive Mac port, Steam |
| Portal | Valve's Mac version, Steam (its last 32-bit build) |

Two kinds of game: ports by Feral Interactive share one runtime layer, which catalina32
replaces, so each new Feral game should need little work; Valve made its Mac Source games
itself, and Half-Life 2 and its episodes should follow from the work on Portal.

## Playing

1. Download **Catalina32** from [Releases](https://github.com/robertoffmoura/catalina32/releases),
   unzip it, move *Catalina32.app* to Applications and open it (see First launch).
2. It finds your games (Steam library, Applications folders) and shows which are supported.
   A game somewhere else: **Choose game…**, or drop it on the window (for Portal, its folder:
   the one holding `hl2_osx`).
3. **Create app** makes a game app, such as *LEGO Star Wars Saga.app*, with the game's icon.
4. Open that app to play. Keep it in the Dock like any other.

**Saves** stay where the original game kept them, so saves from before macOS Catalina load as
they are; catalina32 backs them up before first use. Portal is the exception: it kept its saves
inside the game's folder, which catalina32 never writes to, so new saves and settings go to
`~/Library/Application Support/catalina32/Portal`; saves already in the game's folder still
load.

**Controllers:** USB and Bluetooth controllers work, and can be connected at any time. The one
that drives the menus is player 1; press a button on another to join as player 2.

**Requirements:** macOS 10.15 or later. Tested on Intel Macs; Apple Silicon is included in the
download but hasn't been tested yet.

## First launch

Catalina32 isn't notarised by Apple yet, so macOS blocks it the first time:

- **macOS 15 and later:** open it once and dismiss the warning, then go to *System Settings →
  Privacy & Security*, click **Open Anyway** next to Catalina32, and confirm.
- **macOS 10.15 to 14:** right-click *Catalina32.app* → **Open**, then **Open** again.

You only do this once. Game apps made by Catalina32 open normally.

## Performance

How smoothly a game runs depends on your Mac. The busiest scenes are the most demanding: the
emulator is the limit there, and that's where work continues. Portal's largest rooms run at
a lower frame rate than the rest of the game.

## Troubleshooting

- The game app says the game moved or changed: Steam updated it or the library moved.
  Create the game app again from Catalina32. (Portal: saves made with catalina32 belong to the
  game's folder where it was; if the folder moved, they're still in
  `~/Library/Application Support/catalina32/Portal`, under the old location.)
- Logs: `~/Library/Logs/catalina32/`. Please include them when reporting a problem.

## FAQ

- **Is this legal?** catalina32 is your own copy of the game running on a different
  implementation of the old system libraries. It contains no game code or data.
- **Does it change my game?** No. Game apps point to the installed game and never write to it.
- **Affiliation:** not affiliated with Feral Interactive, Valve, LucasArts, Traveller's Tales
  or any game publisher.

## For developers

- **Building and running:** `scripts/build-app.sh` builds `build/Catalina32.app` (macOS, Go,
  Xcode command-line tools). `go build ./cmd/catalina32` builds the engine and command line
  (`catalina32 list`, `info`, `wrap`, `run`, `source`) on any system. `run_on_mac.sh` (outside
  the repository) is the development loop: it builds both and starts a game with the
  developer HUD; `scripts/run_source.sh` does the same for a Source game, with your own game
  options (`+map escape_02`).
- **Releases:** `scripts/make_release.sh` builds, checks and zips the app and writes the release
  notes; `--publish` then tags it and creates the GitHub release.
- **Tests:** `go test ./...`; with a game executable (`C32_BIN_PATH`) or a Source game's folder
  (`C32_SOURCE_DIR`), more tests run against the game's own code.
- **Documentation:**
  - `docs/ARCHITECTURE.md`: the product and its layers;
  - `CONTEXT.md`: the engine, conventions, where things are;
  - `docs/DEBUGGING.md`: every switch, recipe and tool;
  - `docs/LSWSAGA.md`: what we know about the first game;
  - `docs/PORTAL.md`: Portal and Valve's Source games (running it, how it works);
  - `PROGRESS.md`: status and open work.
- **Adding a game:** a profile in `pkg/games/<id>` (checksum, folders, save location) and,
  until the Feral family layer lands, its table of runtime-layer addresses; a Source game is a
  line in `pkg/gamelib/source.go` (Steam app ID, mod folder, tested engine checksums). Native
  replacements of hot functions bind by signature (`cmd/sigmake`), so they reach every game
  with the same code.

## License

MIT: see [LICENSE](LICENSE).
