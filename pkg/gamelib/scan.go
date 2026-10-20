package gamelib

import (
	"bufio"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// SearchFolders are the folders Scan looks in: every Steam library's steamapps/common, then
// /Applications and ~/Applications.
func SearchFolders() []string {
	home, _ := os.UserHomeDir()
	steam := filepath.Join(home, "Library", "Application Support", "Steam")
	var dirs []string
	for _, lib := range steamLibraries(steam) {
		dirs = append(dirs, filepath.Join(lib, "steamapps", "common"))
	}
	return append(dirs, "/Applications", filepath.Join(home, "Applications"))
}

var vdfPath = regexp.MustCompile(`^\s*"path"\s+"(.*)"\s*$`)

// steamLibraries lists the Steam library folders: the one inside Steam's own folder, and every
// "path" in steamapps/libraryfolders.vdf (libraries on other drives).
func steamLibraries(steam string) []string {
	libs := []string{steam}
	f, err := os.Open(filepath.Join(steam, "steamapps", "libraryfolders.vdf"))
	if err != nil {
		return libs
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if m := vdfPath.FindStringSubmatch(sc.Text()); m != nil {
			p := strings.ReplaceAll(m[1], `\\`, `\`)
			if p != steam {
				libs = append(libs, p)
			}
		}
	}
	return libs
}

// Scan identifies the apps in folders (and one level of subfolders, where Steam keeps games),
// keeping those that need catalina32: supported and unsupported 32-bit games. all keeps every
// app it looked at.
func Scan(folders []string, all bool) []Info {
	seen := map[string]bool{}
	var out []Info
	for _, dir := range folders {
		for _, app := range appsIn(dir) {
			real, err := filepath.EvalSymlinks(app)
			if err != nil || seen[real] {
				continue
			}
			seen[real] = true
			info := Identify(app)
			if all || info.Status == Supported || info.Status == Unsupported {
				out = append(out, info)
			}
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Status != out[j].Status {
			return out[i].Status == Supported // supported games first
		}
		return strings.ToLower(out[i].DisplayName()) < strings.ToLower(out[j].DisplayName())
	})
	return out
}

// appsIn lists the .app bundles in dir and in its immediate subfolders.
func appsIn(dir string) []string {
	var apps []string
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	for _, e := range entries {
		p := filepath.Join(dir, e.Name())
		if !isDirOrLink(p) {
			continue
		}
		if strings.HasSuffix(e.Name(), ".app") {
			apps = append(apps, p)
			continue
		}
		sub, err := os.ReadDir(p)
		if err != nil {
			continue
		}
		for _, s := range sub {
			if q := filepath.Join(p, s.Name()); strings.HasSuffix(s.Name(), ".app") && isDirOrLink(q) {
				apps = append(apps, q)
			}
		}
	}
	return apps
}

func isDirOrLink(p string) bool {
	st, err := os.Stat(p) // follows links
	return err == nil && st.IsDir()
}
