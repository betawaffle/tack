package prompt

import (
	"encoding/json/v2"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestAMissingFileGivesTheBuiltins(t *testing.T) {
	d, err := Load(filepath.Join(t.TempDir(), "nope.json"))
	if err != nil || len(d.Sections) != 5 || d.Sections[0].Name != "base" || d.Tools != nil {
		t.Errorf("defaults = %+v, %v", d, err)
	}
}

func TestSaveThenLoadKeepsEverythingInOrder(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", "prompt.json")
	want := Defaults{
		Sections: []Section{{Name: "b", Paragraphs: []Paragraph{{Text: "B"}, {Text: "more", Off: true}}}, {Name: "a", Paragraphs: Split("A"), Off: true}},
		Tools:    []Tool{{Name: "x", Custom: true, Definition: []byte(`{"type":"function","function":{"name":"x"}}`)}, {Name: "run_code", Off: true}},
	}
	if err := Save(path, want); err != nil {
		t.Fatal(err)
	}
	got, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got.Sections, want.Sections) {
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
	if d, err := Load(path); err == nil || len(d.Sections) != 5 {
		t.Errorf("defaults = %+v, %v", d, err)
	}
}

func TestBlocksSkipWhatsOffAndFillVariables(t *testing.T) {
	sections := []Section{
		{Name: "a", Paragraphs: []Paragraph{{Text: "A"}, {Text: "hidden", Off: true}, {Text: "B"}}},
		{Name: "off", Paragraphs: Split("X"), Off: true},
		{Name: "none on", Paragraphs: []Paragraph{{Text: "Y", Off: true}}},
		{Name: "b", Paragraphs: Split("on {{date}} and {{date}}, {{nope}} {{ date }} {{")},
	}
	got := Blocks(sections, map[string]string{"date": Date(time.Date(2026, 11, 7, 23, 0, 0, 0, time.UTC))})
	if strings.Join(got, "") != "A\n\nB\n\non 2026-11-07 and 2026-11-07, {{nope}} {{ date }} {{" || len(got) != 3 {
		t.Errorf("blocks = %q", got)
	}
	if Blocks(nil, nil) != nil {
		t.Error("no sections should give no blocks")
	}
	if got := Fill("{{a}}{{b}}}}{{a}", map[string]string{"a": "{{b}}", "b": "x"}); got != "{{b}}x}}{{a}" {
		t.Errorf("fill = %q", got)
	}
}

func TestTheOlderFormStillLoads(t *testing.T) {
	path := filepath.Join(t.TempDir(), "prompt.json")
	if err := os.WriteFile(path, []byte(`{"sections":[{"name":"a","text":"one\n\ntwo","off":true}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	d, err := Load(path)
	if err != nil || len(d.Sections) != 1 || !d.Sections[0].Off || !reflect.DeepEqual(d.Sections[0].Paragraphs, []Paragraph{{Text: "one"}, {Text: "two"}}) {
		t.Errorf("loaded = %+v, %v", d, err)
	}
}

func TestEveryBuiltinVariableIsOneTackFills(t *testing.T) {
	known := []string{"date", "model", "host", "tack_revision", "python_version", "python_packages", "library_threads", "memory_min", "memory_max", "work_size", "time_limit", "channel_line_size", "channel_calls", "docs_dir", "tack_docs", "user_docs"}
	for _, s := range builtin().Sections {
		text := s.Text()
		for {
			i := strings.Index(text, "{{")
			if i < 0 {
				break
			}
			j := strings.Index(text[i:], "}}")
			name := text[i+2 : i+j]
			if !slices.Contains(known, name) {
				t.Errorf("%s uses {{%s}}", s.Name, name)
			}
			text = text[i+j:]
		}
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
