package source

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"catalina32/pkg/cpu"
)

// C32_SOURCE_DIR=<Portal> go test ./pkg/source -run NewGameDialog -v: opens the New Game dialog
// (gamemenucommand OpenNewGameDialog) the way a player who has played before finds it (the game
// has saved its config.cfg), and checks that it lists the chapters, draws their pictures, and
// that its Start button starts chapter 1.
func TestNewGameDialog(t *testing.T) {
	dir := os.Getenv("C32_SOURCE_DIR")
	if dir == "" {
		t.Skip("C32_SOURCE_DIR not set")
	}
	if os.Getenv("C32_OVERLAY") == "" { // cfg/ written into: the chapters are listed from there
		overlay := t.TempDir()
		abs, _ := filepath.Abs(dir)
		cfg := filepath.Join(overlay, abs, "portal", "cfg")
		os.MkdirAll(cfg, 0o755)
		if err := os.WriteFile(filepath.Join(cfg, "config.cfg"), []byte("sv_unlockedchapters \"1\"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		t.Setenv("C32_OVERLAY", overlay)
	}
	m, finish := withWindow(t, dir, "+gamemenucommand", "OpenNewGameDialog")
	str := func(c *cpu.CPU, a uint32) string {
		if a == 0 || a >= cpu.ArenaSize {
			return "<nil>"
		}
		return c.ReadCString(a)
	}

	// The dialog and its chapters.
	var dialog uint32
	var panels []string
	m.Around(regexp.MustCompile(`CGameChapterPanelC[12]E|CNewGameDialogC[12]E`), func(name string, c *cpu.CPU) func(*cpu.CPU) {
		if strings.HasPrefix(name, "__ZN14CNewGameDialog") {
			dialog = c.Arg(0)
		} else {
			panels = append(panels, str(c, c.Arg(3))+" ("+str(c, c.Arg(6))+")")
		}
		return nil
	})

	// The chapter pictures it draws (vgui surface calls).
	pictures := map[uint32]string{} // surface texture id -> chapter picture
	drawn := map[string]int{}
	var bound uint32
	m.Watch(regexp.MustCompile(`^__ZN17CMatSystemSurface(18DrawSetTextureFileEiPKcib|14DrawSetTextureEi|16DrawTexturedRectEiiii)$`),
		func(name string, c *cpu.CPU) {
			switch {
			case strings.Contains(name, "TextureFile"):
				if f := str(c, c.Arg(2)); strings.Contains(f, "chapters/") {
					pictures[c.Arg(1)] = f
				}
			case strings.Contains(name, "SetTexture"):
				bound = c.Arg(1)
			default:
				if f, ok := pictures[bound]; ok {
					drawn[f]++
				}
			}
		})

	// Once it has drawn a while, its Start button (the "Play" command); the engine then starts the
	// map (after the menu's background map).
	var started []string
	m.Watch(regexp.MustCompile(`^__Z17HostState_NewGamePKcbb$`), func(_ string, c *cpu.CPU) {
		started = append(started, str(c, c.Arg(0)))
	})
	polls, pressedAt := 0, -1
	m.OnPoll = func(c *cpu.CPU) {
		polls++
		if dialog == 0 || len(drawn) == 0 || pressedAt >= 0 {
			return
		}
		if drawn["vgui/chapters/chapter1"] < 10 {
			return
		}
		pressedAt = polls
		onCommand := m.symbolAddr("__ZN14CNewGameDialog9OnCommandEPKc")
		if onCommand == 0 {
			t.Error("CNewGameDialog::OnCommand not found")
			return
		}
		play := c.Malloc(8)
		c.WriteCString(play, "Play")
		if _, err := c.CallGuest(onCommand, dialog, play); err != nil {
			t.Errorf("OnCommand(Play): %v", err)
		}
	}

	finish(9_000_000_000)
	t.Logf("%d chapter panels: %s", len(panels), strings.Join(panels, ", "))
	t.Logf("chapter pictures drawn: %v; Start pressed at poll %d of %d; new game: %v", drawn, pressedAt, polls, started)
	if len(panels) == 0 {
		t.Error("the dialog lists no chapters")
	}
	if drawn["vgui/chapters/chapter1"] == 0 {
		t.Error("chapter 1's picture was not drawn")
	}
	if len(started) == 0 || started[len(started)-1] != "testchmb_a_00" {
		t.Errorf("Start began %v, not chapter 1 (testchmb_a_00)", started)
	}
}
