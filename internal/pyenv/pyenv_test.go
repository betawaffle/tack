package pyenv

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func wheel(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var b bytes.Buffer
	w := zip.NewWriter(&b)
	for name, text := range files {
		f, err := w.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		f.Write([]byte(text))
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func lockFor(t *testing.T, server string, wheels map[string][]byte) Lock {
	t.Helper()
	var text strings.Builder
	text.WriteString("# test\npython 3.14\n")
	for name, data := range wheels {
		sum := sha256.Sum256(data)
		text.WriteString(name + " 1.0 " + server + "/" + name + ".whl sha256=" + hex.EncodeToString(sum[:]) + "\n")
	}
	lock, err := Parse([]byte(text.String()))
	if err != nil {
		t.Fatal(err)
	}
	return lock
}

func serve(t *testing.T, wheels map[string][]byte) *httptest.Server {
	s := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(wheels[strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/"), ".whl")])
	}))
	t.Cleanup(s.Close)
	return s
}

func TestABuildUnpacksCheckedWheelsReadOnly(t *testing.T) {
	wheels := map[string][]byte{
		"good": wheel(t, map[string]string{"good/__init__.py": "x = 1\n", "Good_Pkg-1.0.dist-info/METADATA": "m", "good-1.0.data/platlib/extra.py": "y", "good-1.0.data/scripts/run": "#!"}),
	}
	s := serve(t, wheels)
	lock := lockFor(t, s.URL, wheels)
	root := t.TempDir()
	t.Cleanup(func() {
		filepath.WalkDir(root, func(p string, _ os.DirEntry, _ error) error { os.Chmod(p, 0o755); return nil })
	})
	if err := Build(context.Background(), s.Client(), lock, root); err != nil {
		t.Fatal(err)
	}
	site := Site(lock.Dir(root))
	if data, err := os.ReadFile(filepath.Join(site, "good", "__init__.py")); err != nil || string(data) != "x = 1\n" {
		t.Errorf("unpacked = %q, %v", data, err)
	}
	if _, err := os.Stat(filepath.Join(site, "extra.py")); err != nil {
		t.Errorf("platlib data wasn't put in site: %v", err)
	}
	if _, err := os.Stat(filepath.Join(site, "run")); err == nil {
		t.Error("a script was unpacked")
	}
	if err := os.WriteFile(filepath.Join(site, "good", "__init__.py"), nil, 0o644); err == nil {
		t.Error("it's writable")
	}
	installed, err := Installed(site)
	if err != nil || installed["good-pkg"] != "1.0" {
		t.Errorf("installed = %v, %v", installed, err)
	}
	// Building again does nothing.
	if err := Build(context.Background(), s.Client(), lock, root); err != nil {
		t.Fatal(err)
	}
}

func TestABuildRefusesWhatIsntPinnedOrPlain(t *testing.T) {
	for name, files := range map[string]map[string]string{
		"escape":   {"../evil.py": "x"},
		"absolute": {"/etc/evil": "x"},
	} {
		wheels := map[string][]byte{"bad": wheel(t, files)}
		s := serve(t, wheels)
		lock := lockFor(t, s.URL, wheels)
		root := t.TempDir()
		if err := Build(context.Background(), s.Client(), lock, root); err == nil || lock.Built(root) {
			t.Errorf("%s: built, %v", name, err)
		}
	}
	// A wheel that isn't the one pinned.
	wheels := map[string][]byte{"good": wheel(t, map[string]string{"a.py": "1"})}
	s := serve(t, wheels)
	lock := lockFor(t, s.URL, wheels)
	wheels["good"] = wheel(t, map[string]string{"a.py": "2"})
	root := t.TempDir()
	if err := Build(context.Background(), s.Client(), lock, root); err == nil || !strings.Contains(err.Error(), "SHA-256") {
		t.Errorf("swapped wheel: %v", err)
	}
	if entries, _ := os.ReadDir(root); len(entries) != 0 {
		t.Errorf("left behind: %v", entries)
	}
}

func TestTacksLockParses(t *testing.T) {
	lock := Default()
	if lock.Python == "" || len(lock.Packages) == 0 {
		t.Errorf("lock = %+v", lock)
	}
	for _, bad := range []string{"python 3.14\nnumpy 1.0 http://x sha256=" + strings.Repeat("a", 64), "numpy 1.0", "python 3.14\nnumpy 1.0 https://x sha256=ab"} {
		if _, err := Parse([]byte(bad)); err == nil {
			t.Errorf("parsed %q", bad)
		}
	}
	if Normalize("Python_Dateutil") != "python-dateutil" || Normalize("zope.interface") != "zope-interface" {
		t.Error("normalizing")
	}
}
