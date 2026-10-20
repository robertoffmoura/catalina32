package gamelib

import (
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// ownFolder is catalina32's folder for its own data (separate saves, backups, logs).
func ownFolder() string {
	dir, err := os.UserConfigDir() // ~/Library/Application Support on macOS
	if err != nil {
		dir = os.TempDir()
	}
	return filepath.Join(dir, "catalina32")
}

// DefaultSaves is where a game's saves go: the original game's folder when the profile knows
// it, so saves made before catalina32 load as they are; otherwise catalina32's own folder.
func DefaultSaves(profileID, saveDir, original string) string {
	if p := expandHome(original); p != "" {
		return p
	}
	if saveDir == "" {
		saveDir = profileID
	}
	return filepath.Join(ownFolder(), saveDir, "SaveData")
}

// expandHome turns a leading "~/" into the home folder ("" stays "").
func expandHome(p string) string {
	if !strings.HasPrefix(p, "~/") {
		return p
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, p[2:])
}

const (
	backupEvery = 7 * 24 * time.Hour // a new backup when the newest is older than this
	backupsKept = 5
)

// BackupSaves copies a game's save folder to catalina32's backups
// (Backups/<profile>/<date-time>) when there is something to back up and no backup is newer
// than backupEvery, keeping the newest backupsKept. It returns the new backup's folder ("" if
// none was made).
func BackupSaves(profileID, saves string, now time.Time) (string, error) {
	if empty, err := isEmptyDir(saves); err != nil || empty {
		return "", nil // nothing yet (or no folder): nothing to protect
	}
	root := filepath.Join(ownFolder(), "Backups", profileID)
	existing, _ := backupsIn(root)
	if n := len(existing); n > 0 {
		if t, err := time.ParseInLocation(backupLayout, existing[n-1], time.Local); err == nil && now.Sub(t) < backupEvery {
			return "", nil
		}
	}
	dst := filepath.Join(root, now.Format(backupLayout))
	if err := copyTree(saves, dst); err != nil {
		return "", err
	}
	existing = append(existing, filepath.Base(dst))
	for len(existing) > backupsKept {
		// Only folders named like a backup, inside the backups folder, are ever removed.
		_ = os.RemoveAll(filepath.Join(root, existing[0]))
		existing = existing[1:]
	}
	return dst, nil
}

const backupLayout = "2006-01-02_150405"

// backupsIn lists the backups in root, oldest first.
func backupsIn(root string) ([]string, error) {
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil, err
	}
	var names []string
	for _, e := range entries {
		if _, err := time.Parse(backupLayout, e.Name()); err == nil && e.IsDir() {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	return names, nil
}

func isEmptyDir(p string) (bool, error) {
	entries, err := os.ReadDir(p)
	if err != nil {
		return true, err
	}
	return len(entries) == 0, nil
}

// copyTree copies the regular files and folders under src to dst (which must not exist).
func copyTree(src, dst string) error {
	if _, err := os.Stat(dst); err == nil {
		return os.ErrExist
	}
	return filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, p)
		target := filepath.Join(dst, rel)
		switch {
		case d.IsDir():
			return os.MkdirAll(target, 0o755)
		case d.Type().IsRegular():
			return copyFile(p, target)
		}
		return nil // links and other files aren't saves
	})
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	st, err := in.Stat()
	if err != nil {
		return err
	}
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, st.Mode().Perm())
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}
