package gamelib

import (
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// KindSource marks a Valve Source game: a folder holding the launcher hl2_osx, the engine's
// libraries in bin/ and the game's content in a mod folder (portal/, hl2/...), not an app.
const KindSource = "source"

// sourceGames are the Source games catalina32 has been played through: Steam's app ID
// (steam_appid.txt), the mod folder the game runs (-game), and the checksums of the
// bin/engine.dylib versions tested.
var sourceGames = []struct {
	appID, mod, id, title string
	engines               []string
}{
	{"400", "portal", "portal", "Portal", []string{
		"f6054951bdde6c4ec0c62654c075d8fb65321628da2e62e35d01785718a2bd71", // Steam's last 32-bit Mac build
	}},
}

// IsSourceFolder says whether dir is a Source game's folder.
func IsSourceFolder(dir string) bool {
	return fileExists(filepath.Join(dir, "hl2_osx")) && dirExists(filepath.Join(dir, "bin"))
}

// identifySource reads a Source game's folder: its launcher's architectures, its Steam app ID,
// which picks the game, and its engine's checksum, which must be one tested.
func identifySource(dir string) Info {
	info := Info{App: dir, Executable: "hl2_osx", Name: filepath.Base(dir), Kind: KindSource}
	head, err := readHead(filepath.Join(dir, "hl2_osx"), 4096)
	if err != nil {
		info.Status, info.Detail = Unreadable, "Its launcher (hl2_osx) can't be read: "+err.Error()
		return info
	}
	archs := machArchs(head)
	info.Arch = strings.Join(archs, ",")
	switch {
	case len(archs) == 0:
		info.Status, info.Detail = Unreadable, "Its launcher (hl2_osx) isn't a Mac program catalina32 recognises."
		return info
	case contains(archs, "x86_64") || contains(archs, "arm64"):
		info.Status, info.Detail = Native, "This game has 64-bit code: macOS runs it without catalina32."
		return info
	}
	appID := ""
	if b, err := os.ReadFile(filepath.Join(dir, "steam_appid.txt")); err == nil {
		appID = strings.Trim(string(b), " \t\r\n\x00") // Steam ends it with a newline and a NUL
	}
	for _, g := range sourceGames {
		if g.appID != appID || !fileExists(filepath.Join(dir, g.mod, "gameinfo.txt")) {
			continue
		}
		info.Title, info.Args = g.title, []string{"-game", g.mod}
		if icon := filepath.Join(dir, g.mod, "resource", "game.icns"); fileExists(icon) {
			info.Icon = icon
		}
		sum, err := fileSHA256(filepath.Join(dir, "bin", "engine.dylib"))
		if err != nil {
			info.Status, info.Detail = Unreadable, "Its engine (bin/engine.dylib) can't be read: "+err.Error()
			return info
		}
		info.SHA256 = sum
		if !contains(g.engines, sum) {
			info.Status, info.Detail = Unsupported, "This version of the game isn't supported yet."
			return info
		}
		info.Status, info.Profile, info.Detail = Supported, g.id, "Supported."
		info.Saves = SourceOverlay(dir)
		return info
	}
	info.Status, info.Detail = Unsupported, "Valve's Source games other than Portal aren't supported yet."
	return info
}

// SourceOverlay is where a Source game's writes go (settings, saves, caches): catalina32's own
// folder, named after the game's folder. The game's folder is never written; files the game
// had there before (saves from before macOS Catalina) still read through.
func SourceOverlay(dir string) string {
	return filepath.Join(ownFolder(), filepath.Base(dir))
}

func fileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
