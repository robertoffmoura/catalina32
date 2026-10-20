// Package profile is the boundary between the generic runtime layers (cpu, crt, win32, d3d9) and a
// specific game binary.
//
// The shipped Feral/IndirectX executables are stripped, so the generic shims cannot find the
// statically linked MacDoze (Win32), IndirectX (D3D9/DirectInput/DirectSound) or third-party library
// functions by name. A Profile supplies the missing names: generic layers ask for a well-known
// function ("CreateFileA", "Direct3DCreate9", "vorbis:mdct_backward", ...) and the profile answers
// with an address, either from its own table or by scanning the executable for a known byte
// signature. Generic code must never contain a game address; game-specific code lives in
// pkg/games/<game>.
package profile

import (
	"fmt"
	"sort"

	"catalina32/pkg/cpu"
	"catalina32/pkg/macho32"
)

type Profile struct {
	Name string
	// Title is the game's name as players know it (the wizard and game apps show it).
	Title string
	// SHA256 of the Mach-O executable this profile was written for.
	SHA256 string

	// Symbols maps well-known function names to guest addresses in this binary.
	Symbols map[string]uint32
	// Globals maps well-known data locations used by generic shims (e.g. "MacDoze.LastError").
	Globals map[string]uint32

	// EntryPoint names the function the runtime calls after static initialisers (normally "WinMain").
	EntryPoint string
	// ModInitLimit, when non-zero, runs only __mod_init_func entries below this address.
	ModInitLimit uint32
	// DataDir is the game data folder's name, next to the .app (the default data root).
	DataDir string
	// SaveDir names the game's folder under the user's catalina32 settings directory, which
	// holds its SaveData (the profile ID when empty).
	SaveDir string
	// OriginalSaves is where the original game kept what the runtime maps to SaveData
	// ("~/" for the home folder); when set, game apps use it by default, so older saves load.
	OriginalSaves string
	// ModulePath is what GetModuleFileNameA reports (a Windows path).
	ModulePath string
	// UnmappedStubs lists engine->platform boundary functions that should return 1 and be logged
	// instead of being emulated (platform services the runtime does not model yet).
	UnmappedStubs []uint32

	text      []byte
	textAddr  uint32
	resolved  map[string]uint32
	missing   map[string]bool
	sigSource map[string]string
}

// Attach records the executable's code so signature lookups can scan it.
func (p *Profile) Attach(img *macho32.Image, mem []byte) {
	p.resolved = map[string]uint32{}
	p.missing = map[string]bool{}
	p.sigSource = map[string]string{}
	for _, sec := range img.Sections {
		if sec.SegName == "__TEXT" && sec.SectName == "__text" {
			p.textAddr = sec.Addr
			p.text = mem[sec.Addr : sec.Addr+sec.Size]
		}
	}
}

// Addr resolves a well-known function name: the profile's table first, then a byte signature
// scan of __TEXT,__text (see signatures.go).
func (p *Profile) Addr(name string) (uint32, bool) {
	if a, ok := p.Symbols[name]; ok {
		return a, true
	}
	if a, ok := p.resolved[name]; ok {
		return a, true
	}
	if sig, ok := Signatures[name]; ok && p.text != nil {
		if off, ok := sig.findUnique(p.text, p.textAddr); ok {
			a := p.textAddr + uint32(off)
			p.resolved[name] = a
			p.sigSource[name] = "signature"
			return a, true
		}
	}
	return 0, false
}

// Bind registers fn as the host implementation of the named function, if this binary has it.
func (p *Profile) Bind(c *cpu.CPU, name string, fn cpu.ThunkFunc) bool {
	addr, ok := p.Addr(name)
	if !ok {
		p.missing[name] = true
		return false
	}
	c.RegisterIntercept(addr, name, fn)
	return true
}

// Named reports whether addr is one of the profile's well-known functions (Symbols).
func (p *Profile) Named(addr uint32) bool {
	for _, a := range p.Symbols {
		if a == addr {
			return true
		}
	}
	return false
}

// Global returns the address of a well-known data location, or 0 if the profile does not define it.
func (p *Profile) Global(name string) uint32 {
	return p.Globals[name]
}

// Report summarises how names were resolved, so a new game's profile can be filled in.
func (p *Profile) Report() string {
	var missing, sigs []string
	for n := range p.missing {
		missing = append(missing, n)
	}
	for n := range p.sigSource {
		sigs = append(sigs, fmt.Sprintf("%s@0x%08x", n, p.resolved[n]))
	}
	sort.Strings(missing)
	sort.Strings(sigs)
	return fmt.Sprintf("profile %q: %d table symbols, found by signature: %v, unresolved: %v",
		p.Name, len(p.Symbols), sigs, missing)
}
