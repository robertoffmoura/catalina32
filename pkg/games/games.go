// Package games lists the game profiles (Layer 3) the runtime knows about and picks the one that
// matches a loaded executable.
package games

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"

	"lsw64/pkg/cpu"
	"lsw64/pkg/d3d9"
	"lsw64/pkg/games/lswsaga"
	"lsw64/pkg/profile"
)

// Game couples a profile with the game-specific hooks it installs.
type Game struct {
	ID      string
	Profile *profile.Profile
	Install func(c *cpu.CPU, d3d *d3d9.Subsystem, prof *profile.Profile)
}

var All = []Game{
	{ID: "lswsaga", Profile: lswsaga.Profile, Install: lswsaga.Install},
}

// Detect returns the game whose profile matches the executable's SHA-256, or the one named by
// force (a Game ID) when set.
func Detect(exe []byte, force string) (Game, error) {
	if force != "" {
		for _, g := range All {
			if strings.EqualFold(g.ID, force) {
				return g, nil
			}
		}
		return Game{}, fmt.Errorf("unknown profile %q", force)
	}
	sum := sha256.Sum256(exe)
	h := hex.EncodeToString(sum[:])
	for _, g := range All {
		if g.Profile.SHA256 == h {
			return g, nil
		}
	}
	return Game{}, fmt.Errorf("no profile for executable sha256 %s (set LSW_PROFILE to force one)", h)
}
