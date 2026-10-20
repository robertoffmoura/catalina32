package source

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
)

// A directory the game has written into lists both its files and the game's own: Portal finds
// its chapters by listing cfg/, where it also saves config.cfg.
func TestListDirMergesOverlay(t *testing.T) {
	game, overlay := t.TempDir(), t.TempDir()
	write := func(p string) {
		os.MkdirAll(filepath.Dir(p), 0o755)
		if err := os.WriteFile(p, nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(filepath.Join(game, "portal", "cfg", "chapter1.cfg"))
	write(filepath.Join(game, "portal", "cfg", "Config.cfg"))
	write(filepath.Join(overlay, game, "portal", "cfg", "config.cfg"))
	write(filepath.Join(overlay, game, "portal", "cfg", "saved.cfg"))
	m := &Machine{Overlay: overlay, cwd: game}

	names, types, ok := m.listDir("portal/cfg")
	slices.Sort(names)
	if !ok || !slices.Equal(names, []string{"chapter1.cfg", "config.cfg", "saved.cfg"}) || len(types) != 3 {
		t.Errorf("portal/cfg: %v %v %v", names, types, ok)
	}
	if names, _, ok := m.listDir(filepath.Join(game, "PORTAL")); !ok || !slices.Equal(names, []string{"cfg"}) {
		t.Errorf("PORTAL (case ignored): %v %v", names, ok)
	}
	if _, _, ok := m.listDir("portal/cfg/chapter1.cfg"); ok {
		t.Error("a file listed as a directory")
	}
	if _, _, ok := m.listDir("missing"); ok {
		t.Error("a missing directory listed")
	}
}
