// Package gamelib is the product logic around the engine: it identifies game apps, finds them
// on disk, makes game apps (wrappers) for them and decides where their saves go. The window
// app and the command line both use it, so it holds no UI.
package gamelib

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"catalina32/pkg/games"
)

// Status says whether catalina32 can run a game.
type Status string

const (
	Supported   Status = "supported"   // a known game: catalina32 runs it
	Unsupported Status = "unsupported" // 32-bit, but no profile matches (yet)
	Native      Status = "native"      // has 64-bit code: macOS runs it without catalina32
	Unreadable  Status = "unreadable"  // not a game app we can read
)

// Info is what catalina32 knows about one game app.
type Info struct {
	App        string `json:"app"`                  // the .app bundle
	Executable string `json:"executable,omitempty"` // relative to App
	Name       string `json:"name,omitempty"`       // the bundle's name
	BundleID   string `json:"bundleId,omitempty"`
	Icon       string `json:"icon,omitempty"` // the bundle's .icns, if any
	Arch       string `json:"arch,omitempty"` // i386, x86_64, arm64, or a list for universal binaries
	SHA256     string `json:"sha256,omitempty"`
	Status     Status `json:"status"`
	Detail     string `json:"detail"` // one sentence for players
	Profile    string `json:"profile,omitempty"`
	Title      string `json:"title,omitempty"` // the game's name as players know it
	Data       string `json:"data,omitempty"`  // the game's data folder (supported games)
	Saves      string `json:"saves,omitempty"` // where its saves go by default (supported games)
}

// DisplayName is the best name to show for the game.
func (i Info) DisplayName() string {
	switch {
	case i.Title != "":
		return i.Title
	case i.Name != "":
		return i.Name
	}
	return strings.TrimSuffix(filepath.Base(i.App), ".app")
}

// Identify reads a game app: its Info.plist, its executable's architectures and, for 32-bit
// executables, its checksum, which picks the profile.
func Identify(app string) Info {
	app = filepath.Clean(app)
	info := Info{App: app}
	plist, _ := readPlist(filepath.Join(app, "Contents", "Info.plist"))
	info.Name = plist["CFBundleName"]
	info.BundleID = plist["CFBundleIdentifier"]
	if icon := plist["CFBundleIconFile"]; icon != "" {
		if filepath.Ext(icon) == "" {
			icon += ".icns"
		}
		if p := filepath.Join(app, "Contents", "Resources", icon); fileExists(p) {
			info.Icon = p
		}
	}
	exe := plist["CFBundleExecutable"]
	if exe == "" || !fileExists(filepath.Join(app, "Contents", "MacOS", exe)) {
		exe = onlyExecutable(filepath.Join(app, "Contents", "MacOS"))
	}
	if exe == "" {
		info.Status, info.Detail = Unreadable, "No executable found in this app."
		return info
	}
	info.Executable = filepath.Join("Contents", "MacOS", exe)
	exePath := filepath.Join(app, info.Executable)
	head, err := readHead(exePath, 4096)
	if err != nil {
		info.Status, info.Detail = Unreadable, "Its executable can't be read: "+err.Error()
		return info
	}
	archs := machArchs(head)
	info.Arch = strings.Join(archs, ",")
	switch {
	case len(archs) == 0:
		info.Status, info.Detail = Unreadable, "Its executable isn't a Mac program catalina32 recognises."
		return info
	case contains(archs, "x86_64") || contains(archs, "arm64"):
		info.Status, info.Detail = Native, "This game has 64-bit code: macOS runs it without catalina32."
		return info
	case len(archs) > 1 || archs[0] != "i386":
		info.Status, info.Detail = Unsupported, "Executables with "+info.Arch+" code aren't supported yet."
		return info
	}
	data, err := os.ReadFile(exePath) // only 32-bit executables are read whole
	if err != nil {
		info.Status, info.Detail = Unreadable, "Its executable can't be read: "+err.Error()
		return info
	}
	sum := sha256.Sum256(data)
	info.SHA256 = hex.EncodeToString(sum[:])
	game, err := games.Detect(data, "")
	if err != nil {
		info.Status, info.Detail = Unsupported, "This version of the game isn't supported yet."
		return info
	}
	prof := game.Profile
	info.Status, info.Profile, info.Title = Supported, game.ID, prof.Title
	info.Detail = "Supported."
	if prof.DataDir != "" {
		info.Data = filepath.Join(filepath.Dir(app), prof.DataDir)
		if !dirExists(info.Data) {
			info.Detail = fmt.Sprintf("Supported, but its data folder %q isn't next to the app.", prof.DataDir)
		}
	}
	info.Saves = DefaultSaves(game.ID, prof.SaveDir, prof.OriginalSaves)
	return info
}

// Mach-O constants for the headers machArchs reads.
const (
	mhMagic32   = 0xfeedface
	mhMagic64   = 0xfeedfacf
	fatMagic    = 0xcafebabe // big-endian
	cpuTypeI386 = 7
	cpuTypeX64  = 0x01000007
	cpuTypeARM  = 12
	cpuTypeA64  = 0x0100000c
	cpuTypePPC  = 18
	cpuTypePPC6 = 0x01000012
)

// machArchs lists the architectures of a Mach-O executable (thin or universal).
func machArchs(b []byte) []string {
	if len(b) < 8 {
		return nil
	}
	name := func(t uint32) string {
		switch t {
		case cpuTypeI386:
			return "i386"
		case cpuTypeX64:
			return "x86_64"
		case cpuTypeARM:
			return "arm"
		case cpuTypeA64:
			return "arm64"
		case cpuTypePPC:
			return "ppc"
		case cpuTypePPC6:
			return "ppc64"
		}
		return fmt.Sprintf("cpu%#x", t)
	}
	switch binary.LittleEndian.Uint32(b) {
	case mhMagic32, mhMagic64:
		return []string{name(binary.LittleEndian.Uint32(b[4:]))}
	}
	if binary.BigEndian.Uint32(b) != fatMagic {
		return nil
	}
	n := binary.BigEndian.Uint32(b[4:])
	var archs []string
	for i := uint32(0); i < n && i < 16 && 8+20*(i+1) <= uint32(len(b)); i++ {
		archs = append(archs, name(binary.BigEndian.Uint32(b[8+20*i:])))
	}
	return archs
}

// readPlist returns the top-level string values of an XML property list (binary property
// lists aren't read: callers fall back to looking at the bundle's files).
func readPlist(path string) (map[string]string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if bytes.HasPrefix(b, []byte("bplist")) {
		return nil, errors.New("binary property list")
	}
	out := map[string]string{}
	d := xml.NewDecoder(bytes.NewReader(b))
	d.Strict = false
	depth, key, inKey, inString := 0, "", false, false
	var text strings.Builder
	for {
		tok, err := d.Token()
		if err == io.EOF {
			return out, nil
		}
		if err != nil {
			return out, err
		}
		switch t := tok.(type) {
		case xml.StartElement:
			depth++
			// plist > dict > key/string: the top-level dict's entries are at depth 3.
			inKey, inString = depth == 3 && t.Name.Local == "key", depth == 3 && t.Name.Local == "string"
			text.Reset()
		case xml.CharData:
			if inKey || inString {
				text.Write(t)
			}
		case xml.EndElement:
			switch {
			case inKey:
				key = text.String()
			case inString && key != "":
				out[key] = text.String()
				key = ""
			case depth == 3:
				key = "" // a value of another type
			}
			inKey, inString = false, false
			depth--
		}
	}
}

// readHead reads up to n bytes from the start of a file.
func readHead(path string, n int) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	b := make([]byte, n)
	k, err := io.ReadFull(f, b)
	if err == io.ErrUnexpectedEOF || err == io.EOF {
		err = nil
	}
	return b[:k], err
}

// onlyExecutable returns the one regular file in dir, if there is exactly one.
func onlyExecutable(dir string) string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return ""
	}
	found := ""
	for _, e := range entries {
		if e.Type().IsRegular() {
			if found != "" {
				return ""
			}
			found = e.Name()
		}
	}
	return found
}

func fileExists(p string) bool {
	st, err := os.Stat(p)
	return err == nil && st.Mode().IsRegular()
}

func dirExists(p string) bool {
	st, err := os.Stat(p)
	return err == nil && st.IsDir()
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}
