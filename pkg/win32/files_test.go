package win32

import (
	"os"
	"path/filepath"
	"testing"
)

func TestFopenFlags(t *testing.T) {
	for mode, want := range map[string]int{
		"r":   os.O_RDONLY,
		"rb":  os.O_RDONLY,
		"r+b": os.O_RDWR,
		"w":   os.O_WRONLY | os.O_CREATE | os.O_TRUNC,
		"wb+": os.O_RDWR | os.O_CREATE | os.O_TRUNC,
		"a":   os.O_WRONLY | os.O_CREATE | os.O_APPEND,
		"a+":  os.O_RDWR | os.O_CREATE | os.O_APPEND,
	} {
		if got := fopenFlags(mode); got != want {
			t.Errorf("fopenFlags(%q) = %#x, want %#x", mode, got, want)
		}
	}
}

// Writing to a game data file must leave DataRoot untouched and be visible to later reads.
func TestDataFilesCopyOnWrite(t *testing.T) {
	dir := t.TempDir()
	data, save := filepath.Join(dir, "data"), filepath.Join(dir, "save")
	if err := os.MkdirAll(filepath.Join(data, "Levels"), 0755); err != nil {
		t.Fatal(err)
	}
	orig := filepath.Join(data, "Levels", "Config.TXT")
	if err := os.WriteFile(orig, []byte("original"), 0644); err != nil {
		t.Fatal(err)
	}
	env := &Win32Env{DataRoot: data, SaveRoot: save}

	if got := env.ResolvePath(`C:\levels\config.txt`, false); got != orig {
		t.Fatalf("read path %q, want %q", got, orig)
	}
	f, _, err := env.openGuestFile(`levels\config.txt`, fopenFlags("r+b"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteAt([]byte("ORIG"), 0); err != nil {
		t.Fatal(err)
	}
	f.Close()
	if b, _ := os.ReadFile(orig); string(b) != "original" {
		t.Errorf("game data modified: %q", b)
	}
	if b, _ := os.ReadFile(env.ResolvePath(`levels\config.txt`, false)); string(b) != "ORIGinal" {
		t.Errorf("read after write = %q, want %q", b, "ORIGinal")
	}

	// Saves are written in place, and "a" appends.
	for _, s := range []string{"one", "two"} {
		f, _, err := env.openGuestFile(`C:\SaveData\slot.sav`, fopenFlags("ab"))
		if err != nil {
			t.Fatal(err)
		}
		f.Write([]byte(s))
		f.Close()
	}
	if b, _ := os.ReadFile(filepath.Join(save, "slot.sav")); string(b) != "onetwo" {
		t.Errorf("append gave %q", b)
	}
	if _, _, err := env.openGuestFile(`C:\SaveData\slot.sav`, os.O_RDWR|os.O_CREATE|os.O_EXCL); !os.IsExist(err) {
		t.Errorf("CREATE_NEW on an existing file: err = %v", err)
	}
}
