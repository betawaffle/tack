package docs

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"
)

func library(t *testing.T) Library {
	t.Helper()
	dir := t.TempDir()
	return Library{
		TackFS: fstest.MapFS{
			"sandbox.md":       {Data: []byte("Python {{python_version}}. See [[limits/memory]] and [[notes|my notes]], not [[nowhere]].")},
			"limits/memory.md": {Data: []byte("Back to [[sandbox]]. {{nope}}")},
			"README":           {Data: []byte("not a page")},
		},
		UserDir: dir,
	}
}

func TestPagesComeFromBothTreesAndLinksBecomeRelative(t *testing.T) {
	l := library(t)
	if err := l.Save("notes.md", "Mine. See [[sandbox]] and [[limits/memory]]."); err != nil {
		t.Fatal(err)
	}
	if err := l.Save("deep/er/page.md", "[[notes]]"); err != nil {
		t.Fatal(err)
	}
	pages, err := l.Pages()
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, p := range pages {
		names = append(names, p.Tree+":"+p.Name())
	}
	if strings.Join(names, ",") != "harness:limits/memory,harness:sandbox,user:deep/er/page,user:notes" {
		t.Fatalf("pages = %v", names)
	}
	vars := map[string]string{"python_version": "3.14.1"}
	if got := ForModel(pages, pages[1], vars); got != "Python 3.14.1. See [limits/memory](limits/memory.md) and [my notes](../user/notes.md), not [[nowhere]]." {
		t.Errorf("sandbox = %q", got)
	}
	if got := ForModel(pages, pages[0], vars); got != "Back to [sandbox](../sandbox.md). {{nope}}" {
		t.Errorf("memory = %q", got)
	}
	if got := ForModel(pages, pages[2], vars); got != "[notes](../../notes.md)" {
		t.Errorf("deep = %q", got)
	}
	if got := ForModel(pages, pages[3], vars); got != "Mine. See [sandbox](../harness/sandbox.md) and [limits/memory](../harness/limits/memory.md)." {
		t.Errorf("notes = %q", got)
	}

	dir := t.TempDir()
	if err := l.WriteFor(dir, vars); err != nil {
		t.Fatal(err)
	}
	index, _ := os.ReadFile(filepath.Join(dir, "index.md"))
	if !strings.Contains(string(index), "- [limits/memory](harness/limits/memory.md)") || !strings.Contains(string(index), "- [deep/er/page](user/deep/er/page.md)") {
		t.Errorf("index = %s", index)
	}
	if data, err := os.ReadFile(filepath.Join(dir, "user", "deep", "er", "page.md")); err != nil || string(data) != "[notes](../../notes.md)" {
		t.Errorf("written = %q, %v", data, err)
	}
}

func TestOnlyPagesCanBeSavedOrRemoved(t *testing.T) {
	l := library(t)
	for _, bad := range []string{"", ".md", "x.txt", "../x.md", "/x.md", "a/../x.md", ".hidden.md", "a/.b/x.md", "a//x.md", "a\\x.md"} {
		if err := l.Save(bad, "x"); err == nil {
			t.Errorf("saved %q", bad)
		}
	}
	if err := l.Save("a/b/c.md", "x"); err != nil {
		t.Fatal(err)
	}
	if err := l.Remove("a/b/c.md"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(l.UserDir, "a")); !os.IsNotExist(err) {
		t.Errorf("an emptied directory stayed: %v", err)
	}
	if _, err := os.Stat(l.UserDir); err != nil {
		t.Errorf("the tree itself went: %v", err)
	}
}

func TestOnlyRegularFilesAreRead(t *testing.T) {
	l := library(t)
	secret := filepath.Join(t.TempDir(), "secret")
	os.WriteFile(secret, []byte("key"), 0o600)
	if err := os.Symlink(secret, filepath.Join(l.UserDir, "link.md")); err != nil {
		t.Fatal(err)
	}
	pages, err := l.Pages()
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range pages {
		if p.Tree == User {
			t.Errorf("read %s", p.Path)
		}
	}
	if err := l.Remove("link.md"); err == nil {
		t.Error("removed a link")
	}
	// A missing tree is just empty.
	l.UserDir = filepath.Join(l.UserDir, "nope")
	if _, err := l.Pages(); err != nil {
		t.Error(err)
	}
}

func TestLinksAreFoundWhole(t *testing.T) {
	got := Links("[[a]] [[b|c d]] [[]] [[x\ny]] [[ [[e]]")
	if len(got) != 3 || got[0].Target != "a" || got[1].Label != "c d" || got[2].Target != "e" {
		t.Errorf("links = %+v", got)
	}
}
