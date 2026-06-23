// Package config reads and writes the machine-global devbox configuration:
// the device's joined hub and its list of mounts (daemon.toml).
package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"

	"github.com/BurntSushi/toml"
)

// Dir returns the machine-global devbox config directory:
// $XDG_CONFIG_HOME/devbox, else ~/.config/devbox on unix/mac, else
// %AppData%\devbox on Windows.
func Dir() (string, error) {
	if x := os.Getenv("XDG_CONFIG_HOME"); x != "" {
		return filepath.Join(x, "devbox"), nil
	}
	if runtime.GOOS == "windows" {
		base, err := os.UserConfigDir()
		if err != nil {
			return "", err
		}
		return filepath.Join(base, "devbox"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".config", "devbox"), nil
}

// Mount binds a hub share (or sub-path) to a local directory.
type Mount struct {
	Share    string `toml:"share"`
	Subpath  string `toml:"subpath,omitempty"`
	Local    string `toml:"local"`
	Hub      string `toml:"hub"`
	ReadOnly bool   `toml:"readonly,omitempty"`
}

// Daemon is daemon.toml: which hub this device joined (with its bearer token and
// device id) and what it mounts.
type Daemon struct {
	Hub      string  `toml:"hub,omitempty"`
	DeviceID string  `toml:"device_id,omitempty"`
	Bearer   string  `toml:"bearer,omitempty"`
	Mounts   []Mount `toml:"mount"`
}

func daemonPath(dir string) string { return filepath.Join(dir, "daemon.toml") }

// LoadDaemon reads daemon.toml from dir; a missing file yields a zero Daemon.
func LoadDaemon(dir string) (Daemon, error) {
	var d Daemon
	b, err := os.ReadFile(daemonPath(dir))
	if os.IsNotExist(err) {
		return d, nil
	}
	if err != nil {
		return d, err
	}
	return d, toml.Unmarshal(b, &d)
}

// SaveDaemon writes daemon.toml under dir atomically (temp file + rename), so a
// crash or encode error never leaves a truncated config that forgets the device's
// hub and mounts. os.Rename replaces the target atomically on POSIX and Windows.
func SaveDaemon(dir string, d Daemon) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, ".daemon-*.tmp")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp) // no-op once renamed
	if err := toml.NewEncoder(f).Encode(d); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, daemonPath(dir))
}

// Settings is config.toml: per-machine tunables (separate from daemon.toml).
type Settings struct {
	Transfer struct {
		MaxKbps int `toml:"max_kbps"` // blob transfer cap; 0 = unlimited
	} `toml:"transfer"`
	Secrets struct {
		ExtraPatterns []string `toml:"extra_patterns"`
	} `toml:"secrets"`
}

// LoadSettings reads config.toml from dir (zero Settings if absent).
func LoadSettings(dir string) (Settings, error) {
	var s Settings
	b, err := os.ReadFile(filepath.Join(dir, "config.toml"))
	if os.IsNotExist(err) {
		return s, nil
	}
	if err != nil {
		return s, err
	}
	return s, toml.Unmarshal(b, &s)
}

func statePath(dir string) string { return filepath.Join(dir, "state.json") }

// LoadState reads the per-mount last-applied snapshot map (mountKey -> snapshot).
func LoadState(dir string) (map[string]string, error) {
	b, err := os.ReadFile(statePath(dir))
	if os.IsNotExist(err) {
		return map[string]string{}, nil
	}
	if err != nil {
		return nil, err
	}
	m := map[string]string{}
	return m, json.Unmarshal(b, &m)
}

// SaveState writes the per-mount snapshot map atomically.
func SaveState(dir string, m map[string]string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	b, err := json.Marshal(m)
	if err != nil {
		return err
	}
	tmp := statePath(dir) + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, statePath(dir))
}
