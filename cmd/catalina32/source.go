package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"

	"catalina32/pkg/gamelib"
	"catalina32/pkg/source"
)

// cmdSource runs a Valve Source game from its folder (experimental: docs/PORTAL.md).
func cmdSource(args []string) int {
	fs := newFlags("source")
	noWindow := fs.Bool("no-window", false, "don't start the window (the caller draws)")
	window := fs.String("window", "", "the window app executable (default: the one in this app)")
	if fs.Parse(args) != nil || fs.NArg() < 1 {
		usage()
		return 2
	}
	dir, err := filepath.Abs(fs.Arg(0))
	if err == nil {
		_, err = os.Stat(filepath.Join(dir, "hl2_osx"))
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "catalina32: %s is not a Source game folder (no hl2_osx)\n", fs.Arg(0))
		return 1
	}
	gameArgs := fs.Args()[1:]
	if len(gameArgs) == 0 {
		for _, mod := range []string{"portal", "episodic", "ep2", "lostcoast", "hl2"} {
			if _, err := os.Stat(filepath.Join(dir, mod, "gameinfo.txt")); err == nil {
				gameArgs = []string{"-game", mod}
				break
			}
		}
	}
	return runSource(dir, gameArgs, gamelib.SourceOverlay(dir), *noWindow, *window)
}

// runSource plays a Source game: what it writes (settings, saves) goes to overlay, never into
// its folder dir.
func runSource(dir string, gameArgs []string, overlay string, noWindow bool, window string) int {
	work, err := os.MkdirTemp("", "catalina32-")
	if err != nil {
		fmt.Fprintln(os.Stderr, "catalina32:", err)
		return 1
	}
	name := filepath.Base(dir)
	for k, v := range map[string]string{"C32_SHM_FILE": "framebuffer.shm", "C32_GL_SHM": "gl.shm"} {
		if os.Getenv(k) == "" {
			os.Setenv(k, filepath.Join(work, v))
		}
	}
	os.Setenv("C32_LIVE", "1")
	os.Setenv("C32_TITLE", name)
	m, err := source.New(dir, gameArgs, overlay, os.Stdout)
	if err != nil {
		fmt.Fprintln(os.Stderr, "catalina32:", err)
		return 1
	}
	if !noWindow {
		win, _, err := runtimeFiles(window)
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
	if err := m.Run(0); err != nil && m.C.ExitCode == 0 {
		fmt.Fprintf(os.Stderr, "catalina32: %s stopped: %v\n", name, err)
		for _, line := range m.Backtrace(16) {
			fmt.Fprintln(os.Stderr, "  ", line)
		}
		return 1
	}
	return int(m.C.ExitCode)
}
