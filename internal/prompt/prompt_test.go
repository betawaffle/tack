package prompt

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func names(sections []Section) string {
	var out []string
	for _, s := range sections {
		out = append(out, s.Name)
	}
	return strings.Join(out, ",")
}

func TestBuiltinsHaveNothingPersonalAndAMissingDirIsFine(t *testing.T) {
	if got := names(Load(filepath.Join(t.TempDir(), "nope"))); got != "base,no-tools,tools,notes,workdir" {
		t.Errorf("sections = %s", got)
	}
}

func TestFilesReplaceRemoveAndAdd(t *testing.T) {
	dir := t.TempDir()
	for name, text := range map[string]string{
		"base.md":     " Mine. \n",
		"no-tools.md": "",
		"project.md":  "About the project.",
		"ignored.txt": "Not a section.",
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(text), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	got := Load(dir)
	if names(got) != "base,tools,notes,workdir,project" {
		t.Fatalf("sections = %s", names(got))
	}
	if got[0].Text != "Mine." || got[1].For != "tools" {
		t.Errorf("sections = %+v", got)
	}
}

func TestBlocksJoinInOrderAndReplaceTheWorkdir(t *testing.T) {
	sections := []Section{{Name: "a", Text: "A"}, {Name: "b", Text: "in {{workdir}} and {{workdir}}"}}
	got := Blocks(sections, []string{"a", "missing", "b"}, "/w")
	want := "A\n\nin /w and /w"
	if strings.Join(got, "") != want || len(got) != 3 {
		t.Errorf("blocks = %q", got)
	}
	if Blocks(sections, nil, "/w") != nil {
		t.Error("no names should give no blocks")
	}
}
