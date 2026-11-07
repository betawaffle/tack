package prompt

import (
	"encoding/json/v2"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestAMissingFileGivesTheBuiltins(t *testing.T) {
	d, err := Load(filepath.Join(t.TempDir(), "nope.json"))
	if err != nil || len(d.Sections) != 2 || d.Sections[0].Name != "base" || d.Tools != nil {
		t.Errorf("defaults = %+v, %v", d, err)
	}
}

func TestSaveThenLoadKeepsEverythingInOrder(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", "prompt.json")
	want := Defaults{
		Sections: []Section{{Name: "b", Text: "B\n\nmore"}, {Name: "a", Text: "A", Off: true}},
		Tools:    []Tool{{Name: "x", Custom: true, Definition: []byte(`{"type":"function","function":{"name":"x"}}`)}, {Name: "run_code", Off: true}},
	}
	if err := Save(path, want); err != nil {
		t.Fatal(err)
	}
	got, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Sections) != 2 || got.Sections[0] != want.Sections[0] || got.Sections[1] != want.Sections[1] {
		t.Errorf("sections = %+v", got.Sections)
	}
	if len(got.Tools) != 2 || got.Tools[0].Name != "x" || !got.Tools[0].Custom || !got.Tools[1].Off || got.Tools[1].Definition != nil {
		t.Errorf("tools = %+v", got.Tools)
	}
	// An empty list stays empty rather than meaning the built-ins.
	if err := Save(path, Defaults{Tools: []Tool{}}); err != nil {
		t.Fatal(err)
	}
	if got, _ := Load(path); got.Tools == nil || len(got.Tools) != 0 || got.Sections == nil {
		t.Errorf("empty = %+v", got)
	}
}

func TestABrokenFileIsAnErrorWithTheBuiltins(t *testing.T) {
	path := filepath.Join(t.TempDir(), "prompt.json")
	if err := os.WriteFile(path, []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	if d, err := Load(path); err == nil || len(d.Sections) != 2 {
		t.Errorf("defaults = %+v, %v", d, err)
	}
}

func TestBlocksSkipWhatsOffAndFillTheDate(t *testing.T) {
	sections := []Section{{Name: "a", Text: "A"}, {Name: "off", Text: "X", Off: true}, {Name: "b", Text: "on {{date}} and {{date}}"}}
	got := Blocks(sections, Date(time.Date(2026, 11, 7, 23, 0, 0, 0, time.UTC)))
	if strings.Join(got, "") != "A\n\non 2026-11-07 and 2026-11-07" || len(got) != 3 {
		t.Errorf("blocks = %q", got)
	}
	if Blocks(nil, "d") != nil {
		t.Error("no sections should give no blocks")
	}
}

func TestNoToolsSavedStaysDistinctFromNone(t *testing.T) {
	data, err := json.Marshal(builtin())
	if err != nil || strings.Contains(string(data), `"tools":`) {
		t.Errorf("builtins = %s, %v", data, err)
	}
	data, _ = json.Marshal(Defaults{Tools: []Tool{}})
	if !strings.Contains(string(data), `"tools":[]`) {
		t.Errorf("none = %s", data)
	}
}
