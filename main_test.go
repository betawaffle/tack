package main

import (
	"io/fs"
	"os/exec"
	"slices"
	"strings"
	"testing"

	"github.com/betawaffle/tack/internal/docs"
	"github.com/betawaffle/tack/internal/store"
)

func TestTheSourceIsEveryTrackedFile(t *testing.T) {
	out, err := exec.Command("git", "ls-files").Output()
	if err != nil {
		t.Skip("not in a git checkout:", err)
	}
	tracked := strings.Fields(string(out))
	var embedded []string
	fs.WalkDir(source, ".", func(p string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			embedded = append(embedded, p)
		}
		return err
	})
	for _, p := range tracked {
		if !slices.Contains(embedded, p) {
			t.Errorf("%s is tracked but not in the source", p)
		}
	}
	for _, p := range embedded {
		if !slices.Contains(tracked, p) {
			t.Errorf("%s is in the source but not tracked", p)
		}
	}
}

func TestTacksDocsUseOnlyVariablesAndPagesThatExist(t *testing.T) {
	library := docs.Library{TackFS: modelDocs()}
	pages, err := library.Pages()
	if err != nil || len(pages) == 0 {
		t.Fatalf("pages = %v, %v", pages, err)
	}
	vars := variables(store.Settings{Model: "m", Host: "h"})
	for _, p := range pages {
		text := docs.ForModel(pages, p, vars)
		if strings.Contains(text, "{{") || strings.Contains(text, "[[") {
			t.Errorf("%s has something unfilled:\n%s", p.Path, text)
		}
	}
}
