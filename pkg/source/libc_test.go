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

// Deleting a file in the game's folder (Source emptying its save folder) hides it for good, also
// in the next run, until the game writes it again; the game's folder stays as it was. Portal's
// folder can hold an old playthrough's level states (save/escape_02.hl1), which were restored
// in place of the level while they couldn't be deleted.
func TestDeleteGameFile(t *testing.T) {
	game, overlay := t.TempDir(), t.TempDir()
	old := filepath.Join(game, "portal", "save", "escape_02.hl1")
	os.MkdirAll(filepath.Dir(old), 0o755)
	if err := os.WriteFile(old, []byte("2021"), 0o644); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(game, "portal", "save", "half-life-000.sav"), nil, 0o644)
	m := &Machine{Overlay: overlay, cwd: game}
	visible := func(m *Machine, name string) bool {
		_, err := os.Stat(m.hostPath(filepath.Join("portal/save", name), false))
		names, _, _ := m.listDir("portal/save")
		if listed := slices.Contains(names, name); listed != (err == nil) {
			t.Errorf("%s: listed %v, opens %v", name, listed, err == nil)
		}
		return err == nil
	}

	if !visible(m, "escape_02.hl1") {
		t.Fatal("the game's file isn't there to begin with")
	}
	if e := m.removePath("portal/save/ESCAPE_02.HL1"); e != 0 {
		t.Fatalf("remove: errno %d", e)
	}
	if visible(m, "escape_02.hl1") || !visible(m, "half-life-000.sav") {
		t.Error("after removing: the file is still there, or another one went too")
	}
	if e := m.removePath("portal/save/escape_02.hl1"); e != eNOENT {
		t.Errorf("removing it again: errno %d, want ENOENT", e)
	}
	if b, err := os.ReadFile(old); err != nil || string(b) != "2021" {
		t.Error("the game's folder changed")
	}
	if e := m.removePath("portal"); e != 66 {
		t.Errorf("rmdir of a folder with files: errno %d, want ENOTEMPTY", e)
	}

	next := &Machine{Overlay: overlay, cwd: game} // the next run
	if visible(next, "escape_02.hl1") {
		t.Error("the next run sees the deleted file")
	}
	// Written again: a new file, not the old one.
	f, err := os.OpenFile(next.hostPath("portal/save/escape_02.hl1", true), os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	f.WriteString("new")
	f.Close()
	if b, _ := os.ReadFile(next.hostPath("portal/save/escape_02.hl1", false)); string(b) != "new" || !visible(next, "escape_02.hl1") {
		t.Errorf("written again: %q", b)
	}
	if visible(&Machine{Overlay: overlay, cwd: game}, "escape_02.hl1") != true {
		t.Error("the run after that doesn't see the new file")
	}
}
