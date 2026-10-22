package gamelib

import (
	"errors"
	"fmt"
	"html"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

// WrapOptions says where a game app goes and what it is made of.
type WrapOptions struct {
	OutDir  string // folder for the game app (default ~/Applications)
	Window  string // the window app executable (Catalina32.app/Contents/MacOS/Catalina32)
	Engine  string // the engine (Catalina32.app/Contents/Helpers/catalina32)
	Version string // catalina32's version, recorded in the game app
}

// Wrap makes a game app for a supported game: the window app and the engine (copies, so the
// game app keeps working if Catalina32.app moves), the game's icon and a manifest pointing at
// the installed game, which is never copied or changed. An existing game app of the same name
// is replaced; any other app of that name is left alone. It returns the game app's path.
func Wrap(info Info, opt WrapOptions) (string, error) {
	if info.Status != Supported {
		return "", fmt.Errorf("%s: %s", info.DisplayName(), info.Detail)
	}
	if opt.OutDir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		opt.OutDir = filepath.Join(home, "Applications")
	}
	for _, p := range []string{opt.Window, opt.Engine} {
		if !fileExists(p) {
			return "", fmt.Errorf("catalina32 file missing: %s", p)
		}
	}
	name := strings.TrimSuffix(filepath.Base(info.App), ".app")
	app := filepath.Join(opt.OutDir, name+".app")
	if same, _ := sameFile(app, info.App); same || filepath.Clean(app) == filepath.Clean(info.App) {
		return "", fmt.Errorf("the game app can't go in the game's own folder (%s): choose another folder", opt.OutDir)
	}
	if dirExists(app) && !fileExists(filepath.Join(app, "Contents", "Resources", ManifestName)) {
		return "", fmt.Errorf("%s already exists and isn't a catalina32 game app: choose another folder", app)
	}
	if err := os.MkdirAll(opt.OutDir, 0o755); err != nil {
		return "", err
	}

	// Build next to the destination, then move it into place.
	tmp, err := os.MkdirTemp(opt.OutDir, "."+name+".app-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(tmp) // the temporary folder only (gone after a successful rename)
	exe := filepath.Base(info.Executable)
	if info.Kind == KindSource {
		exe = name // not "hl2_osx": the name players see in Activity Monitor
	}
	contents := filepath.Join(tmp, "Contents")
	for _, d := range []string{"MacOS", "Helpers", "Resources"} {
		if err := os.MkdirAll(filepath.Join(contents, d), 0o755); err != nil {
			return "", err
		}
	}
	steps := []struct{ src, dst string }{
		{opt.Window, filepath.Join(contents, "MacOS", exe)},
		{opt.Engine, filepath.Join(contents, "Helpers", "catalina32")},
	}
	if info.Icon != "" {
		steps = append(steps, struct{ src, dst string }{info.Icon, filepath.Join(contents, "Resources", "AppIcon.icns")})
	}
	for _, s := range steps {
		if err := copyFile(s.src, s.dst); err != nil {
			return "", err
		}
	}
	for _, p := range []string{filepath.Join(contents, "MacOS", exe), filepath.Join(contents, "Helpers", "catalina32")} {
		if err := os.Chmod(p, 0o755); err != nil {
			return "", err
		}
	}
	if err := WriteManifest(filepath.Join(contents, "Resources", ManifestName), ManifestFor(info)); err != nil {
		return "", err
	}
	plist := gameAppPlist(info, exe, opt.Version, info.Icon != "")
	if err := os.WriteFile(filepath.Join(contents, "Info.plist"), []byte(plist), 0o644); err != nil {
		return "", err
	}
	if err := signAdHoc(tmp); err != nil {
		return "", err
	}

	if dirExists(app) { // an earlier game app (checked above): replace it
		old := app + ".old"
		_ = os.RemoveAll(old)
		if err := os.Rename(app, old); err != nil {
			return "", err
		}
		defer os.RemoveAll(old)
	}
	if err := os.Rename(tmp, app); err != nil {
		return "", err
	}
	return app, nil
}

// gameAppPlist is the game app's Info.plist.
func gameAppPlist(info Info, exe, version string, icon bool) string {
	if version == "" {
		version = "0"
	}
	esc := html.EscapeString
	var iconKey string
	if icon {
		iconKey = "\t<key>CFBundleIconFile</key>\n\t<string>AppIcon</string>\n"
	}
	return `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>CFBundleExecutable</key>
	<string>` + esc(exe) + `</string>
	<key>CFBundleIdentifier</key>
	<string>com.ffmoura.catalina32.` + esc(info.Profile) + `</string>
	<key>CFBundleName</key>
	<string>` + esc(strings.TrimSuffix(filepath.Base(info.App), ".app")) + `</string>
	<key>CFBundleDisplayName</key>
	<string>` + esc(info.DisplayName()) + `</string>
	<key>CFBundlePackageType</key>
	<string>APPL</string>
	<key>CFBundleShortVersionString</key>
	<string>` + esc(version) + `</string>
` + iconKey + `	<key>LSMinimumSystemVersion</key>
	<string>10.15</string>
	<key>NSHighResolutionCapable</key>
	<true/>
	<key>LSApplicationCategoryType</key>
	<string>public.app-category.games</string>
</dict>
</plist>
`
}

// signAdHoc signs the bundle ad hoc, which macOS requires for code on Apple Silicon. Off macOS
// (tests) there is nothing to do.
func signAdHoc(app string) error {
	if runtime.GOOS != "darwin" {
		return nil
	}
	out, err := exec.Command("codesign", "--force", "--deep", "--sign", "-", app).CombinedOutput()
	if err != nil {
		return fmt.Errorf("codesign: %v: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

func sameFile(a, b string) (bool, error) {
	sa, err := os.Stat(a)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		return false, err
	}
	sb, err := os.Stat(b)
	if err != nil {
		return false, err
	}
	return os.SameFile(sa, sb), nil
}
