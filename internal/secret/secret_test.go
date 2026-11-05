package secret

import (
	"encoding/json/v2"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const value = "sk-or-test-not-a-real-key"

func TestKeyDoesNotPrint(t *testing.T) {
	k := NewKey(value)
	type wrapper struct{ K Key }
	for _, format := range []string{"%v", "%+v", "%#v", "%s", "%q", "%x", "%d"} {
		for _, v := range []any{k, wrapper{k}, &wrapper{k}} {
			if got := fmt.Sprintf(format, v); strings.Contains(got, value) || strings.Contains(got, fmt.Sprintf("%x", value)) {
				t.Errorf("Sprintf(%q) = %q", format, got)
			}
		}
	}
	if data, err := json.Marshal(wrapper{k}); err == nil {
		if strings.Contains(string(data), value) {
			t.Errorf("Marshal = %s", data)
		}
	}
}

func TestAuthorize(t *testing.T) {
	r, _ := http.NewRequest(http.MethodGet, "http://example.invalid", nil)
	NewKey(value).Authorize(r)
	if got := r.Header.Get("Authorization"); got != "Bearer "+value {
		t.Errorf("header wrong")
	}
}

func TestLoad(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "key")
	if err := os.WriteFile(path, []byte(value+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	k, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	fired := false
	NewTestTripwire(func(string) { fired = true }, k).Check("test", []byte("x"+value+"y"))
	if !fired {
		t.Error("loaded key isn't what the file held (trailing newline kept?)")
	}

	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil {
		t.Error("loaded a world-readable key")
	}
	if err := os.WriteFile(filepath.Join(dir, "empty"), []byte("\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(filepath.Join(dir, "empty")); err == nil {
		t.Error("loaded an empty key")
	}
}
