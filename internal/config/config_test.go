package config

import (
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
)

func write(t *testing.T, text string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestAMissingFileMeansTheDefaults(t *testing.T) {
	t.Setenv("HOME", "/home/someone")
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("XDG_DATA_HOME", "")
	cfg, err := Load(filepath.Join(t.TempDir(), "nope.json"), false)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Model != "deepseek/deepseek-v4.1-flash-1017" || cfg.Host != "morph/fp8" || cfg.Addr != "0.0.0.0:8484" {
		t.Errorf("config = %+v", cfg)
	}
	if cfg.Key != "/home/someone/.config/tack/openrouter-key" || cfg.Data != "/home/someone/.local/share/tack" {
		t.Errorf("paths = %+v", cfg)
	}
	if cfg.DefaultWorkdir != "" {
		t.Errorf("default workdir = %q: there must be no fallback", cfg.DefaultWorkdir)
	}
	if _, err := Load(filepath.Join(t.TempDir(), "nope.json"), true); err == nil {
		t.Error("a required file should have to exist")
	}
}

func TestTheFileOverridesOnlyWhatItNames(t *testing.T) {
	t.Setenv("HOME", "/home/someone")
	cfg, err := Load(write(t, `{"search_skip":[],"default_workdir":"/w","command_timeout_seconds":5}`), true)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.SearchSkip) != 0 || cfg.DefaultWorkdir != "/w" || cfg.CommandTimeoutSeconds != 5 || cfg.Host != "morph/fp8" {
		t.Errorf("config = %+v", cfg)
	}
}

func TestBadFilesAreRefused(t *testing.T) {
	t.Setenv("HOME", "/home/someone")
	for _, text := range []string{
		`{not json`,
		`{"serach_skip":[]}`,
		`{"data":"relative/path"}`,
		`{"key":"key"}`,
		`{"prompts":"p"}`,
		`{"default_workdir":"."}`,
		`{"model":""}`,
		`{"command_timeout_seconds":0}`,
		`{"search_skip":["a/b"]}`,
		`{"search_skip":[""]}`,
		`{"max_read_bytes":0}`,
		`{"sandbox_read_write":["relative"]}`,
		`{"sandbox_protected":["../outside"]}`,
		`{"sandbox_protected":["/absolute"]}`,
		`{"sandbox_extra_read_only":["relative"]}`,
	} {
		if _, err := Load(write(t, text), true); err == nil {
			t.Errorf("%s: no error", text)
		}
	}
}

func TestNoHomeIsAnErrorNotARelativePath(t *testing.T) {
	t.Setenv("HOME", "")
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("XDG_DATA_HOME", "")
	if _, err := Load(filepath.Join(t.TempDir(), "nope.json"), false); err == nil || !strings.Contains(err.Error(), "HOME") {
		t.Errorf("err = %v", err)
	}
	if _, err := DefaultPath(); err == nil {
		t.Error("expected an error")
	}
}

func TestStartupChanges(t *testing.T) {
	old := Config{Addr: "a", Data: "d", Key: "k", URL: "u"}
	changed := Config{Addr: "a2", Data: "d", Key: "k2", URL: "u2"}
	if got := StartupChanges(old, changed); !reflect.DeepEqual(got, []string{"addr", "key", "url"}) {
		t.Errorf("changes = %v", got)
	}
}

func TestSandboxExtrasAddToTheDefaults(t *testing.T) {
	t.Setenv("HOME", "/home/someone")
	cfg, err := Load(write(t, `{"sandbox_extra_read_only":["/opt/go"],"max_listed":5}`), true)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(cfg.SandboxReadOnly, "/usr") || !slices.Contains(cfg.SandboxReadOnly, "/opt/go") {
		t.Errorf("read only = %v", cfg.SandboxReadOnly)
	}
	if cfg.MaxListed != 5 || cfg.MaxReadBytes != 16<<20 {
		t.Errorf("limits = %d, %d", cfg.MaxListed, cfg.MaxReadBytes)
	}
}
