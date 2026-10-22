package gamelib

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
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
	// Another app with the name (Steam's shortcut to the game): left alone, the game app gets
	// another name, which a second wrap replaces; with both names taken, nothing is made.
	other := fakeApp(t, filepath.Join(dir, "other"), "Game", []byte("x"))
	for i := 0; i < 2; i++ {
		got, err := Wrap(info, WrapOptions{OutDir: filepath.Dir(other), Window: window, Engine: engine})
		if err != nil || got != filepath.Join(dir, "other", "Game (Catalina32).app") {
			t.Fatalf("beside another app: %q %v", got, err)
		}
	}
	if b, err := os.ReadFile(filepath.Join(other, "Contents", "MacOS", "Game")); err != nil || string(b) != "x" {
		t.Errorf("the other app changed: %q %v", b, err)
	}
	third := fakeApp(t, filepath.Join(dir, "third"), "Game", []byte("x"))
	fakeApp(t, filepath.Join(dir, "third"), "Game (Catalina32)", []byte("y"))
	if _, err := Wrap(info, WrapOptions{OutDir: filepath.Dir(third), Window: window, Engine: engine}); err == nil {
		t.Error("made a game app with both names taken by other apps")
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

// fakeSource makes a Source game's folder in dir: the launcher (launcher's bytes), the engine,
// steam_appid.txt and the portal mod with its icon.
func fakeSource(t *testing.T, dir, appID string, launcher, engine []byte) string {
	t.Helper()
	for _, d := range []string{"bin", "portal/resource"} {
		if err := os.MkdirAll(filepath.Join(dir, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for p, b := range map[string][]byte{"hl2_osx": launcher, "bin/engine.dylib": engine, "steam_appid.txt": []byte(appID + "\n\x00"),
		"portal/gameinfo.txt": []byte(`"GameInfo" {}`), "portal/resource/game.icns": []byte("icns")} {
		if err := os.WriteFile(filepath.Join(dir, p), b, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// testEngine makes sourceGames accept engine for Portal while the test runs.
func testEngine(t *testing.T, engine []byte) {
	sum := sha256.Sum256(engine)
	saved := sourceGames[0].engines
	sourceGames[0].engines = append([]string{hex.EncodeToString(sum[:])}, saved...)
	t.Cleanup(func() { sourceGames[0].engines = saved })
}

func TestIdentifySource(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(dir, "config"))
	i386 := machHeader(mhMagic32, cpuTypeI386)
	engine := []byte("engine")
	portal := fakeSource(t, filepath.Join(dir, "common", "Portal"), "400", i386, engine)
	if info := Identify(portal); info.Status != Unsupported || info.Kind != KindSource || info.SHA256 == "" {
		t.Errorf("an untested engine: %+v", info)
	}
	testEngine(t, engine)
	info := Identify(portal)
	if info.Status != Supported || info.Profile != "portal" || info.Title != "Portal" || info.Executable != "hl2_osx" ||
		strings.Join(info.Args, " ") != "-game portal" || info.Icon != filepath.Join(portal, "portal", "resource", "game.icns") ||
		info.Saves != SourceOverlay(portal) || filepath.Base(info.Saves) != "Portal" {
		t.Errorf("Portal: %+v", info)
	}
	for name, c := range map[string]struct {
		appID    string
		launcher []byte
		want     Status
	}{
		"HL2": {"220", i386, Unsupported}, "Portal64": {"400", machHeader(mhMagic64, cpuTypeX64), Native},
	} {
		if info := Identify(fakeSource(t, filepath.Join(dir, "common", name), c.appID, c.launcher, engine)); info.Status != c.want {
			t.Errorf("%s: %s (%s), want %s", name, info.Status, info.Detail, c.want)
		}
	}
	if info := Identify(t.TempDir()); info.Status != Unreadable {
		t.Errorf("an empty folder: %+v", info)
	}
	// Scan lists Source folders beside apps (Portal64 runs natively: not listed).
	fakeApp(t, filepath.Join(dir, "common", "Lego"), "Game", i386)
	var got []string
	for _, i := range Scan([]string{filepath.Join(dir, "common")}, false) {
		got = append(got, i.DisplayName()+":"+string(i.Status))
	}
	if want := "Portal:supported Game & Co:unsupported HL2:unsupported"; strings.Join(got, " ") != want {
		t.Errorf("Scan: %q, want %q", strings.Join(got, " "), want)
	}
}

func TestWrapSource(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(dir, "config"))
	engine := []byte("engine")
	testEngine(t, engine)
	portal := fakeSource(t, filepath.Join(dir, "common", "Portal"), "400", machHeader(mhMagic32, cpuTypeI386), engine)
	runtime := filepath.Join(dir, "runtime")
	if err := os.MkdirAll(runtime, 0o755); err != nil {
		t.Fatal(err)
	}
	window, eng := filepath.Join(runtime, "Catalina32"), filepath.Join(runtime, "catalina32")
	for _, p := range []string{window, eng} {
		if err := os.WriteFile(p, []byte(filepath.Base(p)), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	app, err := Wrap(Identify(portal), WrapOptions{OutDir: filepath.Join(dir, "Applications"), Window: window, Engine: eng, Version: "0.2"})
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Base(app) != "Portal.app" {
		t.Fatalf("app at %s", app)
	}
	for _, f := range []string{"MacOS/Portal", "Helpers/catalina32", "Resources/AppIcon.icns"} {
		if !fileExists(filepath.Join(app, "Contents", f)) {
			t.Errorf("missing %s", f)
		}
	}
	p, err := readPlist(filepath.Join(app, "Contents", "Info.plist"))
	if err != nil || p["CFBundleExecutable"] != "Portal" || p["CFBundleIdentifier"] != "com.ffmoura.catalina32.portal" || p["CFBundleDisplayName"] != "Portal" {
		t.Fatalf("Info.plist: %v %v", p, err)
	}
	m, err := ReadManifest(filepath.Join(app, "Contents", "Resources", ManifestName))
	if err != nil || m.Kind != KindSource || m.App != portal || strings.Join(m.Args, " ") != "-game portal" || m.Saves != SourceOverlay(portal) {
		t.Fatalf("manifest: %+v %v", m, err)
	}
	if _, err := m.Check(); err != nil {
		t.Errorf("Check on an unchanged game: %v", err)
	}
	if err := os.WriteFile(filepath.Join(portal, "bin", "engine.dylib"), []byte("updated"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Check(); err == nil {
		t.Error("Check accepted a changed engine")
	}
}

// With the real game (C32_SOURCE_DIR, the Portal folder), Identify supports it.
func TestIdentifyRealSource(t *testing.T) {
	dir := os.Getenv("C32_SOURCE_DIR")
	if dir == "" {
		t.Skip("C32_SOURCE_DIR not set")
	}
	info := Identify(dir)
	if info.Status != Supported || info.Profile != "portal" || info.Icon == "" {
		t.Fatalf("%+v", info)
	}
	t.Logf("%s: %s, args %v, saves %s", info.DisplayName(), info.Detail, info.Args, info.Saves)
}
