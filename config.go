package main

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const (
	configName    = "chronytop.conf"
	maxConfigSize = 4096
)

// Settings are saved on exit and restored on the next run, like btop's btop.conf.
type Settings struct {
	Shown    map[string]bool // Keyed by panel title.
	Interval time.Duration
}

func (s Settings) String() string {
	var shown []string
	for _, p := range panels {
		if s.Shown[p.title] {
			shown = append(shown, p.title)
		}
	}
	return fmt.Sprintf("# chronytop settings, saved on exit.\nshown_panels = %s\ninterval = %s\n",
		strings.Join(shown, " "), s.Interval)
}

func parseSettings(data string) Settings {
	var s Settings
	for _, line := range strings.Split(data, "\n") {
		k, v, ok := strings.Cut(line, "=")
		if !ok || strings.HasPrefix(strings.TrimSpace(k), "#") {
			continue
		}
		switch strings.TrimSpace(k) {
		case "shown_panels":
			s.Shown = map[string]bool{}
			for _, title := range strings.Fields(v) {
				s.Shown[title] = true
			}
		case "interval":
			if d, err := time.ParseDuration(strings.TrimSpace(v)); err == nil {
				s.Interval = min(maxInterval, max(minInterval, d))
			}
		}
	}
	return s
}

// ownedBy reports whether fi is owned by the current user. Lstat info means symlinks never match dirs.
func ownedBy(fi fs.FileInfo) bool {
	st, ok := fi.Sys().(*syscall.Stat_t)
	return ok && strconv.FormatUint(uint64(st.Uid), 10) == strconv.Itoa(os.Geteuid())
}

// safeDir checks path is a real directory owned by the current user, optionally creating it.
func safeDir(path string, create bool) bool {
	fi, err := os.Lstat(path)
	if errors.Is(err, fs.ErrNotExist) && create {
		if os.Mkdir(path, 0o700) != nil {
			return false
		}
		fi, err = os.Lstat(path)
	}
	return err == nil && fi.IsDir() && ownedBy(fi)
}

// userConfigDir is $XDG_CONFIG_HOME or ~/.config on every platform, unlike os.UserConfigDir,
// which uses ~/Library/Application Support on macOS.
func userConfigDir() (string, error) {
	if dir := os.Getenv("XDG_CONFIG_HOME"); filepath.IsAbs(dir) {
		return dir, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".config"), nil
}

// configDir returns the settings directory, or "" if it's missing or not safe to use. When run
// with sudo and a non-root HOME, the user's directories fail the ownership check, so nothing is
// read or written there as root.
func configDir(create bool) string {
	base, err := userConfigDir()
	if err != nil {
		return ""
	}
	dir := filepath.Join(base, "chronytop")
	if !safeDir(base, create) || !safeDir(dir, create) {
		return ""
	}
	return dir
}

func loadSettings() (Settings, bool) {
	dir := configDir(false)
	if dir == "" {
		return Settings{}, false
	}
	path := filepath.Join(dir, configName)
	fi, err := os.Lstat(path)
	if err != nil || !fi.Mode().IsRegular() || !ownedBy(fi) || fi.Size() > maxConfigSize {
		return Settings{}, false
	}
	data, err := os.ReadFile(path) // #nosec G304 -- checked above.
	if err != nil {
		return Settings{}, false
	}
	return parseSettings(string(data)), true
}

// saveSettings writes to a temporary file and renames it, so it never follows a symlink.
func saveSettings(s Settings) error {
	dir := configDir(true)
	if dir == "" {
		return errors.New("no safe settings directory")
	}
	f, err := os.CreateTemp(dir, ".chronytop-*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err := f.WriteString(s.String()); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(f.Name(), filepath.Join(dir, configName))
}
