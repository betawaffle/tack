// Package config reads tack's config file. It's JSON, and an unknown field is an
// error, so a typo can't silently leave a setting at its default.
package config

import (
	"encoding/json/v2"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// Config is every setting, with defaults filled in. The first group is read only at
// startup: in particular, a reload can never change where the key is sent. The rest
// take effect on a reload.
type Config struct {
	Addr string `json:"addr"`
	Data string `json:"data"`
	Key  string `json:"key"`
	URL  string `json:"url"`
	// AllowedHosts are extra hostnames the page can be reached by.
	AllowedHosts []string `json:"allowed_hosts"`

	Model   string `json:"model"`
	Host    string `json:"host"`
	Prompts string `json:"prompts"`

	// DefaultWorkdir fills in the working directory for a new conversation. tack's
	// own working directory is never used for anything, so it has no fallback.
	DefaultWorkdir string `json:"default_workdir"`

	// SearchSkip names directories search_files doesn't go into.
	SearchSkip []string `json:"search_skip"`

	// CommandEnv is the whole environment run_command gets. Nothing is inherited,
	// so it can't pick up anything tack happens to have.
	CommandEnv map[string]string `json:"command_env"`

	// CommandTimeoutSeconds limits how long one run_command can take.
	CommandTimeoutSeconds int `json:"command_timeout_seconds"`
}

func defaults() (Config, error) {
	home := os.Getenv("HOME")
	configHome, dataHome := os.Getenv("XDG_CONFIG_HOME"), os.Getenv("XDG_DATA_HOME")
	if home == "" && (configHome == "" || dataHome == "") {
		return Config{}, errors.New("HOME isn't set, so there's nowhere to find tack's files")
	}
	if configHome == "" {
		configHome = filepath.Join(home, ".config")
	}
	if dataHome == "" {
		dataHome = filepath.Join(home, ".local/share")
	}
	return Config{
		Addr:                  "0.0.0.0:8484",
		Data:                  filepath.Join(dataHome, "tack"),
		Key:                   filepath.Join(configHome, "tack/openrouter-key"),
		URL:                   "https://openrouter.ai/api/v1/chat/completions",
		Model:                 "deepseek/deepseek-v4.1-flash-1017",
		Host:                  "morph/fp8",
		Prompts:               filepath.Join(configHome, "tack/prompt"),
		SearchSkip:            []string{".git", "node_modules"},
		CommandEnv:            map[string]string{"PATH": "/usr/local/bin:/usr/bin:/bin", "HOME": home, "LANG": "C.UTF-8"},
		CommandTimeoutSeconds: 120,
	}, nil
}

// DefaultPath is where the config file lives unless -config says otherwise.
func DefaultPath() (string, error) {
	base := os.Getenv("XDG_CONFIG_HOME")
	if base == "" {
		home := os.Getenv("HOME")
		if home == "" {
			return "", errors.New("HOME isn't set, so there's nowhere to find tack's files")
		}
		base = filepath.Join(home, ".config")
	}
	return filepath.Join(base, "tack/config.json"), nil
}

// Load reads a config file over the defaults. With required unset, a missing file
// just means the defaults.
func Load(path string, required bool) (Config, error) {
	cfg, err := defaults()
	if err != nil {
		return Config{}, err
	}
	data, err := os.ReadFile(path)
	switch {
	case errors.Is(err, fs.ErrNotExist) && !required:
		return cfg, nil
	case err != nil:
		return Config{}, err
	}
	if err := json.Unmarshal(data, &cfg, json.RejectUnknownMembers(true)); err != nil {
		return Config{}, fmt.Errorf("%s: %w", path, err)
	}
	if err := cfg.validate(); err != nil {
		return Config{}, fmt.Errorf("%s: %w", path, err)
	}
	return cfg, nil
}

// validate refuses relative paths, because they'd depend on tack's own working
// directory, which is never meant to matter.
func (c Config) validate() error {
	for name, path := range map[string]string{"data": c.Data, "key": c.Key, "prompts": c.Prompts} {
		if !filepath.IsAbs(path) {
			return fmt.Errorf("%s has to be an absolute path", name)
		}
	}
	if c.DefaultWorkdir != "" && !filepath.IsAbs(c.DefaultWorkdir) {
		return errors.New("default_workdir has to be an absolute path")
	}
	if c.Model == "" {
		return errors.New("model can't be empty")
	}
	if c.CommandTimeoutSeconds <= 0 {
		return errors.New("command_timeout_seconds has to be positive")
	}
	if slices.ContainsFunc(c.SearchSkip, func(name string) bool { return name == "" || strings.ContainsRune(name, filepath.Separator) }) {
		return errors.New("search_skip has directory names, not paths")
	}
	return nil
}

// StartupChanges names startup-only settings that differ, which a reload can't apply.
func StartupChanges(old, new Config) []string {
	var out []string
	if old.Addr != new.Addr {
		out = append(out, "addr")
	}
	if old.Data != new.Data {
		out = append(out, "data")
	}
	if old.Key != new.Key {
		out = append(out, "key")
	}
	if old.URL != new.URL {
		out = append(out, "url")
	}
	if !slices.Equal(old.AllowedHosts, new.AllowedHosts) {
		out = append(out, "allowed_hosts")
	}
	return out
}
