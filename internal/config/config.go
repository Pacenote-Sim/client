// Package config is what the client remembers between runs: the device token
// from pairing, and each plugin's own settings, kept for it. The server address is not
// here: it is stamped into the executable, and a client that let a file
// override it would be a client that could be pointed at another server by
// editing a file.
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// FileName is the file under the directory.
const FileName = "config.json"

// Config is the file's content.
type Config struct {
	// Token is the device token pairing gave, or empty when not paired.
	Token string `json:"token,omitempty"`
	// Plugins holds each companion's own settings, by plugin name then key.
	// The client keeps them and never reads them: a volume is the voice
	// plugin's business.
	Plugins map[string]map[string]string `json:"plugins,omitempty"`
}

// Default is a fresh client's configuration.
func Default() Config { return Config{} }

// DefaultDir is where the file lives: the user's configuration directory,
// under Pacenote — %AppData%\Pacenote on Windows.
func DefaultDir() (string, error) {
	base, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("config: no configuration directory: %w", err)
	}
	return filepath.Join(base, "Pacenote"), nil
}

// Load reads the file in dir, or returns [Default] when there is none yet.
// A file that cannot be read as JSON is an error, not a silent reset: a
// driver's pairing is in it.
func Load(dir string) (Config, error) {
	raw, err := os.ReadFile(filepath.Join(dir, FileName)) //nolint:gosec // G304: the path is this program's own configuration file.
	if errors.Is(err, os.ErrNotExist) {
		return Default(), nil
	}
	if err != nil {
		return Config{}, fmt.Errorf("config: reading %s: %w", FileName, err)
	}
	c := Default()
	if err := json.Unmarshal(raw, &c); err != nil {
		return Config{}, fmt.Errorf("config: %s is not the JSON it should be: %w", FileName, err)
	}
	return c, nil
}

// Save writes the file in dir, creating the directory, readable by this user
// only, through a temporary file and a rename so that a crash cannot leave
// half a file.
func Save(dir string, c Config) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("config: creating %s: %w", dir, err)
	}
	raw, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return fmt.Errorf("config: encoding: %w", err)
	}
	path := filepath.Join(dir, FileName)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(raw, '\n'), 0o600); err != nil {
		return fmt.Errorf("config: writing: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("config: writing: %w", err)
	}
	return nil
}

// Paired reports whether a token is held.
func (c Config) Paired() bool { return c.Token != "" }

// Setting is a plugin's kept value, or "".
func (c Config) Setting(plugin, key string) string { return c.Plugins[plugin][key] }

// WithSetting is c with a plugin's value set; an empty value forgets the key.
func (c Config) WithSetting(plugin, key, value string) Config {
	out := c
	out.Plugins = make(map[string]map[string]string, len(c.Plugins))
	for p, kv := range c.Plugins {
		out.Plugins[p] = make(map[string]string, len(kv))
		for k, v := range kv {
			out.Plugins[p][k] = v
		}
	}
	if value == "" {
		delete(out.Plugins[plugin], key)
		if len(out.Plugins[plugin]) == 0 {
			delete(out.Plugins, plugin)
		}
	} else {
		if out.Plugins[plugin] == nil {
			out.Plugins[plugin] = map[string]string{}
		}
		out.Plugins[plugin][key] = value
	}
	if len(out.Plugins) == 0 {
		out.Plugins = nil
	}
	return out
}
