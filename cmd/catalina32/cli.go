package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"catalina32/pkg/gamelib"
)

// version is catalina32's version, recorded in the game apps it makes.
const version = "0.1.0"

func usage() {
	fmt.Fprint(os.Stderr, `catalina32 runs 32-bit Mac games on modern macOS.

  catalina32 list [--json] [--all] [folder...]   find games (Steam libraries, Applications)
  catalina32 info [--json] <Game.app or folder>  what catalina32 knows about a game
  catalina32 wrap [-o folder] [--json] <Game.app or folder>
                                                 make a game app (default ~/Applications)
  catalina32 run <Game.app or folder>            play a game without making a game app
  catalina32 run --manifest <game.json> --no-window
                                                 the engine for a game app's window
  catalina32 source [--window app] <folder> [game options]
                                                 a Valve Source game with your own options
  catalina32 -game <executable>                  the engine alone (development; C32_* variables)
`)
}

// runCommand runs a subcommand and returns the exit status.
func runCommand(name string, args []string) int {
	switch name {
	case "list":
		return cmdList(args)
	case "info":
		return cmdInfo(args)
	case "wrap":
		return cmdWrap(args)
	case "run":
		return cmdRun(args)
	case "source":
		return cmdSource(args)
	case "version":
		fmt.Println(version)
		return 0
	case "help":
		usage()
		return 0
	}
	fmt.Fprintf(os.Stderr, "catalina32: unknown command %q\n\n", name)
	usage()
	return 2
}

func newFlags(name string) *flag.FlagSet {
	fs := flag.NewFlagSet("catalina32 "+name, flag.ContinueOnError)
	fs.Usage = usage
	return fs
}

func printJSON(v any) int {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		fmt.Fprintln(os.Stderr, "catalina32:", err)
		return 1
	}
	return 0
}

func describe(i gamelib.Info) {
	fmt.Printf("%s\n  %s\n  %s\n", i.DisplayName(), i.App, i.Detail)
	if i.Status == gamelib.Supported {
		fmt.Printf("  profile: %s\n  saves: %s\n", i.Profile, i.Saves)
	}
}

func cmdList(args []string) int {
	fs := newFlags("list")
	asJSON := fs.Bool("json", false, "machine-readable output")
	all := fs.Bool("all", false, "show every app looked at, not only 32-bit games")
	if fs.Parse(args) != nil {
		return 2
	}
	folders := fs.Args()
	if len(folders) == 0 {
		folders = gamelib.SearchFolders()
	}
	found := gamelib.Scan(folders, *all)
	if *asJSON {
		if found == nil {
			found = []gamelib.Info{}
		}
		return printJSON(found)
	}
	if len(found) == 0 {
		fmt.Println("No 32-bit games found. Point catalina32 at one with: catalina32 info <Game.app>")
		return 0
	}
	for _, i := range found {
		describe(i)
	}
	return 0
}

func cmdInfo(args []string) int {
	fs := newFlags("info")
	asJSON := fs.Bool("json", false, "machine-readable output")
	if fs.Parse(args) != nil || fs.NArg() != 1 {
		usage()
		return 2
	}
	info := gamelib.Identify(fs.Arg(0))
	if *asJSON {
		return printJSON(info)
	}
	describe(info)
	return 0
}

// runtimeFiles returns the window app and the engine this catalina32 is part of: in
// Catalina32.app (or a game app) the engine is Contents/Helpers/catalina32 and the window app
// is the one executable in Contents/MacOS.
func runtimeFiles(window string) (string, string, error) {
	engine, err := os.Executable()
	if err != nil {
		return "", "", err
	}
	if engine, err = filepath.EvalSymlinks(engine); err != nil {
		return "", "", err
	}
	if window == "" {
		macos := filepath.Join(filepath.Dir(filepath.Dir(engine)), "MacOS")
		entries, _ := os.ReadDir(macos)
		for _, e := range entries {
			if e.Type().IsRegular() {
				window = filepath.Join(macos, e.Name())
				break
			}
		}
	}
	if window == "" {
		return "", engine, fmt.Errorf("the window app wasn't found next to %s (use --window)", engine)
	}
	return window, engine, nil
}

func cmdWrap(args []string) int {
	fs := newFlags("wrap")
	out := fs.String("o", "", "folder for the game app (default ~/Applications)")
	window := fs.String("window", "", "the window app executable (default: the one in this app)")
	asJSON := fs.Bool("json", false, "machine-readable output")
	if fs.Parse(args) != nil || fs.NArg() != 1 {
		usage()
		return 2
	}
	win, engine, err := runtimeFiles(*window)
	if err == nil {
		var app string
		app, err = gamelib.Wrap(gamelib.Identify(fs.Arg(0)), gamelib.WrapOptions{OutDir: *out, Window: win, Engine: engine, Version: version})
		if err == nil {
			if *asJSON {
				return printJSON(map[string]string{"app": app})
			}
			fmt.Println(app)
			return 0
		}
	}
	if *asJSON {
		printJSON(map[string]string{"error": err.Error()})
	} else {
		fmt.Fprintln(os.Stderr, "catalina32:", err)
	}
	return 1
}

func cmdRun(args []string) int {
	fs := newFlags("run")
	manifest := fs.String("manifest", "", "a game app's game.json")
	noWindow := fs.Bool("no-window", false, "don't start the window (the caller draws)")
	window := fs.String("window", "", "the window app executable (default: the one in this app)")
	if fs.Parse(args) != nil || (*manifest == "") == (fs.NArg() != 1) {
		usage()
		return 2
	}
	var m gamelib.Manifest
	if *manifest != "" {
		var err error
		m, err = gamelib.ReadManifest(*manifest)
		if err == nil {
			_, err = m.Check()
		}
		if err != nil {
			fmt.Fprintln(os.Stderr, "catalina32:", err)
			return 1
		}
	} else {
		info := gamelib.Identify(fs.Arg(0))
		if info.Status != gamelib.Supported {
			fmt.Fprintf(os.Stderr, "catalina32: %s: %s\n", info.DisplayName(), info.Detail)
			return 1
		}
		m = gamelib.ManifestFor(info)
	}
	profile, binPath, data, saves := m.Profile, filepath.Join(m.App, m.Executable), m.Data, m.Saves
	if v := os.Getenv("C32_SAVE_ROOT"); v != "" {
		saves = v
	}
	if b, err := gamelib.BackupSaves(profile, saves, time.Now()); err != nil {
		fmt.Fprintln(os.Stderr, "catalina32: backing up the saves failed:", err)
	} else if b != "" {
		fmt.Println("[catalina32] saves backed up to", b)
	}
	if err := os.MkdirAll(saves, 0o755); err != nil {
		fmt.Fprintln(os.Stderr, "catalina32:", err)
		return 1
	}
	if m.Kind == gamelib.KindSource { // a Valve Source game: its folder, its options, its overlay
		return runSource(m.App, m.Args, saves, *noWindow, *window)
	}
	setDefault := func(k, v string) {
		if os.Getenv(k) == "" {
			os.Setenv(k, v)
		}
	}
	setDefault("C32_SAVE_ROOT", saves)
	if data != "" {
		setDefault("C32_DATA_ROOT", data)
	}
	setDefault("C32_LIVE", "1")
	setDefault("C32_AUTO_KEYS", "off")
	if os.Getenv("C32_SHM_FILE") == "" {
		work, err := os.MkdirTemp("", "catalina32-")
		if err != nil {
			fmt.Fprintln(os.Stderr, "catalina32:", err)
			return 1
		}
		os.Setenv("C32_SHM_FILE", filepath.Join(work, "framebuffer.shm"))
		if os.Getenv("C32_RENDERER") != "software" {
			setDefault("C32_GPU_SHM", filepath.Join(work, "gpu.shm"))
		}
		setDefault("C32_FRAMES_DIR", filepath.Join(work, "frames"))
		_ = os.MkdirAll(os.Getenv("C32_FRAMES_DIR"), 0o755)
	}
	if !*noWindow {
		win, _, err := runtimeFiles(*window)
		if err != nil {
			fmt.Fprintln(os.Stderr, "catalina32:", err)
			return 1
		}
		cmd := exec.Command(win, os.Getenv("C32_SHM_FILE"))
		cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
		if err := cmd.Start(); err != nil {
			fmt.Fprintln(os.Stderr, "catalina32: starting the window:", err)
			return 1
		}
		go func() { // closing the window ends the game
			_ = cmd.Wait()
			os.Exit(0)
		}()
		defer func() { _ = cmd.Process.Kill() }()
	}
	engineMain(binPath)
	return 0
}
