package gamelib

import (
	"encoding/json"
	"fmt"
	"os"
)

// ManifestName is the manifest's file name inside a game app's Contents/Resources.
const ManifestName = "game.json"

// Manifest tells a game app which game it runs and where things are.
type Manifest struct {
	Format     int      `json:"format"`
	Profile    string   `json:"profile"`
	Name       string   `json:"name"`
	App        string   `json:"app"`
	Executable string   `json:"executable"` // relative to App
	SHA256     string   `json:"sha256"`
	Data       string   `json:"data,omitempty"`
	Saves      string   `json:"saves"`
	Kind       string   `json:"kind,omitempty"` // KindSource: App is a Source game's folder; Saves its overlay
	Args       []string `json:"args,omitempty"` // the game's options
}

const manifestFormat = 1

// ManifestFor describes a supported game.
func ManifestFor(info Info) Manifest {
	return Manifest{Format: manifestFormat, Profile: info.Profile, Name: info.DisplayName(), App: info.App,
		Executable: info.Executable, SHA256: info.SHA256, Data: info.Data, Saves: info.Saves, Kind: info.Kind, Args: info.Args}
}

func ReadManifest(path string) (Manifest, error) {
	var m Manifest
	b, err := os.ReadFile(path)
	if err != nil {
		return m, err
	}
	if err := json.Unmarshal(b, &m); err != nil {
		return m, fmt.Errorf("%s: %w", path, err)
	}
	if m.Format != manifestFormat {
		return m, fmt.Errorf("%s: format %d (this catalina32 reads %d)", path, m.Format, manifestFormat)
	}
	return m, nil
}

func WriteManifest(path string, m Manifest) error {
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(b, '\n'), 0o644)
}

// Check confirms the game is still where the manifest says and unchanged. Its error is a
// sentence for players.
func (m Manifest) Check() (Info, error) {
	if !dirExists(m.App) {
		return Info{}, fmt.Errorf("%s isn't at %s any more. Make the game app again from Catalina32.", m.Name, m.App)
	}
	info := Identify(m.App)
	if info.SHA256 != m.SHA256 {
		if info.Status == Supported {
			return info, fmt.Errorf("%s has changed since this game app was made (an update?). Make the game app again from Catalina32.", m.Name)
		}
		return info, fmt.Errorf("%s has changed (an update?), and this version isn't supported yet.", m.Name)
	}
	return info, nil
}
