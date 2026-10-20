package gamelib

import (
	"encoding/binary"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fakeApp makes Name.app in dir with an Info.plist and the given executable bytes.
func fakeApp(t *testing.T, dir, name string, exe []byte) string {
	t.Helper()
	app := filepath.Join(dir, name+".app")
	for _, d := range []string{"MacOS", "Resources"} {
		if err := os.MkdirAll(filepath.Join(app, "Contents", d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	plist := `<?xml version="1.0" encoding="UTF-8"?>
<plist version="1.0">
<dict>
	<key>CFBundleExecutable</key>
	<string>` + name + `</string>
	<key>CFBundleIdentifier</key>
	<string>com.example.game</string>
	<key>LSRequiresIPhoneOS</key>
	<false/>
	<key>CFBundleName</key>
	<string>` + name + ` &amp; Co</string>
	<key>CFBundleIconFile</key>
	<string>Game</string>
	<key>CFBundleDocumentTypes</key>
	<array><dict><key>CFBundleTypeName</key><string>Nested</string></dict></array>
</dict>
</plist>`
	write := func(p string, b []byte) {
		if err := os.WriteFile(p, b, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	write(filepath.Join(app, "Contents", "Info.plist"), []byte(plist))
	write(filepath.Join(app, "Contents", "MacOS", name), exe)
	write(filepath.Join(app, "Contents", "Resources", "Game.icns"), []byte("icns"))
	return app
}

func machHeader(magic, cpu uint32) []byte {
	b := make([]byte, 64)
	binary.LittleEndian.PutUint32(b, magic)
	binary.LittleEndian.PutUint32(b[4:], cpu)
	return b
}

func TestPlistAndArchs(t *testing.T) {
	app := fakeApp(t, t.TempDir(), "Game", machHeader(mhMagic32, cpuTypeI386))
	p, err := readPlist(filepath.Join(app, "Contents", "Info.plist"))
	if err != nil {
		t.Fatal(err)
	}
	if p["CFBundleName"] != "Game & Co" || p["CFBundleExecutable"] != "Game" || p["CFBundleTypeName"] != "" {
		t.Fatalf("plist: %v", p)
	}
	fat := make([]byte, 64)
	binary.BigEndian.PutUint32(fat, fatMagic)
	binary.BigEndian.PutUint32(fat[4:], 2)
	binary.BigEndian.PutUint32(fat[8:], cpuTypeI386)
	binary.BigEndian.PutUint32(fat[28:], cpuTypeX64)
	for _, c := range []struct {
		b    []byte
		want string
	}{{machHeader(mhMagic32, cpuTypeI386), "i386"}, {machHeader(mhMagic64, cpuTypeA64), "arm64"}, {fat, "i386,x86_64"}, {[]byte("#!/bin/sh"), ""}} {
		if got := strings.Join(machArchs(c.b), ","); got != c.want {
			t.Errorf("machArchs = %q, want %q", got, c.want)
		}
	}
}

func TestIdentifyStatuses(t *testing.T) {
	dir := t.TempDir()
	cases := []struct {
		exe  []byte
		want Status
	}{
		{machHeader(mhMagic32, cpuTypeI386), Unsupported}, // 32-bit, but no profile has this checksum
		{machHeader(mhMagic64, cpuTypeX64), Native},
		{machHeader(mhMagic32, cpuTypePPC), Unsupported},
		{[]byte("not a program"), Unreadable},
	}
	for i, c := range cases {
		info := Identify(fakeApp(t, dir, "G"+string(rune('a'+i)), c.exe))
		if info.Status != c.want {
			t.Errorf("case %d: %s (%s), want %s", i, info.Status, info.Detail, c.want)
		}
	}
}

// With the real game (C32_BIN_PATH, inside its .app), Identify finds the profile.
func TestIdentifyRealGame(t *testing.T) {
	exe := os.Getenv("C32_BIN_PATH")
	if exe == "" {
		t.Skip("C32_BIN_PATH not set")
	}
	app := filepath.Dir(filepath.Dir(filepath.Dir(exe)))
	info := Identify(app)
	if info.Status != Supported || info.Profile == "" || info.Title == "" || info.Saves == "" {
		t.Fatalf("%+v", info)
	}
	t.Logf("%s: %s, data %s, saves %s", info.DisplayName(), info.Detail, info.Data, info.Saves)
}

func TestWrap(t *testing.T) {
	dir := t.TempDir()
	game := fakeApp(t, filepath.Join(dir, "steam"), "Game", machHeader(mhMagic32, cpuTypeI386))
	runtime := filepath.Join(dir, "runtime")
	if err := os.MkdirAll(runtime, 0o755); err != nil {
		t.Fatal(err)
	}
	window, engine := filepath.Join(runtime, "Catalina32"), filepath.Join(runtime, "catalina32")
	for _, p := range []string{window, engine} {
		if err := os.WriteFile(p, []byte(filepath.Base(p)), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	info := Identify(game)
	info.Status, info.Profile, info.Title, info.Saves = Supported, "test", "The Game", filepath.Join(dir, "saves")
	out := filepath.Join(dir, "Applications")
	opt := WrapOptions{OutDir: out, Window: window, Engine: engine, Version: "0.1"}

	app, err := Wrap(info, opt)
	if err != nil {
		t.Fatal(err)
	}
	if app != filepath.Join(out, "Game.app") {
		t.Fatalf("app at %s", app)
	}
	for _, f := range []string{"MacOS/Game", "Helpers/catalina32", "Resources/AppIcon.icns", "Resources/game.json", "Info.plist"} {
		if !fileExists(filepath.Join(app, "Contents", f)) {
			t.Errorf("missing %s", f)
		}
	}
	p, err := readPlist(filepath.Join(app, "Contents", "Info.plist"))
	if err != nil || p["CFBundleExecutable"] != "Game" || p["CFBundleIdentifier"] != "com.ffmoura.catalina32.test" || p["CFBundleDisplayName"] != "The Game" {
		t.Fatalf("Info.plist: %v %v", p, err)
	}
	m, err := ReadManifest(filepath.Join(app, "Contents", "Resources", ManifestName))
	if err != nil || m.App != game || m.Profile != "test" || m.SHA256 != info.SHA256 {
		t.Fatalf("manifest: %+v %v", m, err)
	}
	if _, err := m.Check(); err != nil {
		t.Errorf("Check on an unchanged game: %v", err)
	}

	// Again: the earlier game app is replaced, and no temporary folders are left behind.
	if _, err := Wrap(info, opt); err != nil {
		t.Fatal(err)
	}
	entries, _ := os.ReadDir(out)
	if len(entries) != 1 {
		t.Errorf("Applications holds %d entries after a second wrap", len(entries))
	}
	// Never over the original game, nor over an app that isn't ours.
	if _, err := Wrap(info, WrapOptions{OutDir: filepath.Dir(game), Window: window, Engine: engine}); err == nil {
		t.Error("wrapped over the original game")
	}
	other := fakeApp(t, filepath.Join(dir, "other"), "Game", []byte("x"))
	if _, err := Wrap(info, WrapOptions{OutDir: filepath.Dir(other), Window: window, Engine: engine}); err == nil {
		t.Error("replaced an app that isn't a game app")
	}

	// A changed game fails the check.
	if err := os.WriteFile(filepath.Join(game, info.Executable), machHeader(mhMagic32, cpuTypeI386)[:40], 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Check(); err == nil {
		t.Error("Check accepted a changed game")
	}
}

func TestBackupSaves(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(dir, "config"))
	saves := filepath.Join(dir, "saves")
	if b, err := BackupSaves("g", saves, time.Now()); b != "" || err != nil {
		t.Fatalf("backed up a missing folder: %q %v", b, err)
	}
	if err := os.MkdirAll(filepath.Join(saves, "Game"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(saves, "Game", "slot0"), []byte("save"), 0o644); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 10, 20, 12, 0, 0, 0, time.Local)
	first, err := BackupSaves("g", saves, now)
	if err != nil || first == "" {
		t.Fatalf("first backup: %q %v", first, err)
	}
	if b, err := os.ReadFile(filepath.Join(first, "Game", "slot0")); err != nil || string(b) != "save" {
		t.Fatalf("backup content: %q %v", b, err)
	}
	if b, _ := BackupSaves("g", saves, now.Add(time.Hour)); b != "" {
		t.Error("backed up again within a week")
	}
	for k := 1; k <= backupsKept+2; k++ {
		if _, err := BackupSaves("g", saves, now.Add(time.Duration(k)*8*24*time.Hour)); err != nil {
			t.Fatal(err)
		}
	}
	names, _ := backupsIn(filepath.Dir(first))
	if len(names) != backupsKept {
		t.Errorf("%d backups kept, want %d", len(names), backupsKept)
	}
}
