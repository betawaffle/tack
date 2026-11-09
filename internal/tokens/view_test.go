package tokens

import (
	"fmt"
	"os"
	"slices"
	"strings"
	"testing"
)

func TestARenderedPromptIsTheCountedOne(t *testing.T) {
	model := "deepseek/deepseek-v4.1-flash-1017"
	for i := 1; i <= 2; i++ {
		input, err := os.ReadFile(fmt.Sprintf("testdata/test_input_%d.json", i))
		if err != nil {
			t.Fatal(err)
		}
		body := input
		if input[0] == '[' {
			body = []byte(`{"messages":` + string(input) + `,"tools":[{"type":"function","function":{"name":"f","description":"d","parameters":{}}}]}`)
		}
		p, err := Render(model, body, &Output{Reasoning: "Hmm.", Content: "Done.", Calls: []OutputCall{{"f", `{"a":"x","b":2}`}}})
		if err != nil {
			t.Fatal(err)
		}
		var joined strings.Builder
		for _, s := range p.Segments {
			joined.WriteString(s.Text)
			if s.Part < 0 || s.Part >= len(p.Parts) {
				t.Fatalf("case %d: segment %q has no part", i, s.Text)
			}
		}
		if joined.String() != p.Text() {
			t.Errorf("case %d: the segments aren't the text", i)
		}
		alone, _ := Render(model, body, nil)
		if p.Generated != len(alone.Text()) || !strings.HasPrefix(p.Text(), alone.Text()) {
			t.Errorf("case %d: the reply should start where the prompt ends: %d", i, p.Generated)
		}
		if !strings.Contains(p.Text()[p.Generated:], "Done.") {
			t.Errorf("case %d: the reply isn't there", i)
		}
		total := 0
		for _, n := range p.Tokens {
			total += n
		}
		tk, _ := loadDeepSeekV41()
		if total != len(p.Text()) || len(p.Tokens) != tk.Count(p.Text()) {
			t.Errorf("case %d: %d tokens over %d bytes, for %d bytes and %d counted", i, len(p.Tokens), total, len(p.Text()), tk.Count(p.Text()))
		}
		generated := slices.ContainsFunc(p.Parts, func(x Part) bool { return x.Generated && x.Kind == PartToolCall })
		fieldValue := slices.ContainsFunc(p.Segments, func(s Segment) bool { return s.Field && s.Text == "x" })
		if !generated || !fieldValue {
			t.Errorf("case %d: the call should be the model's, with its value a field", i)
		}
	}
}

func TestEffortComesFromTheRequest(t *testing.T) {
	model := "deepseek/deepseek-v4.1-flash-1017"
	for effort, want := range map[string]string{"low": "Effort: 50", "max": "Effort: 100", "": "Effort: 75", "minimal": "Effort: 75"} {
		body := `{"messages":[{"role":"user","content":"hi"}],"reasoning":{"effort":"` + effort + `"}}`
		p, err := Render(model, []byte(body), nil)
		if err != nil || !strings.Contains(p.Text(), want) || p.EffortKnown != (effort != "minimal") {
			t.Errorf("%q: %v %v", effort, p.EffortKnown, err)
		}
	}
	p, _ := Render(model, []byte(`{"messages":[{"role":"user","content":"hi"}],"reasoning":{"effort":"none"}}`), nil)
	if strings.Contains(p.Text(), "Effort") || !strings.HasSuffix(p.Text(), dsThinkEnd) {
		t.Errorf("no thinking: %q", p.Text())
	}
	if _, err := Render("other/model", []byte(`{}`), nil); err != ErrNoRenderer {
		t.Errorf("an unknown model: %v", err)
	}
}

func TestDiffsAreSmallAndDeleteFirst(t *testing.T) {
	split := func(s string) []string { return strings.Split(s, " ") }
	apply := func(a, b []string, edits []Edit) []string {
		var out []string
		i, j := 0, 0
		for _, e := range edits {
			switch e.Op {
			case "same":
				out = append(out, b[j:j+e.N]...)
				i, j = i+e.N, j+e.N
			case "deleted":
				i += e.N
			case "inserted":
				out = append(out, b[j:j+e.N]...)
				j += e.N
			}
		}
		if i != len(a) || j != len(b) {
			t.Errorf("edits cover %d of %d and %d of %d", i, len(a), j, len(b))
		}
		return out
	}
	for _, c := range []struct {
		a, b    string
		changed int
		want    string
	}{
		{"a b c", "a b c d e", 2, "same 3, inserted 2"},
		{"a b c d", "a x c d", 2, "same 1, deleted 1, inserted 1, same 2"},
		{"a b c d e f", "a c d f", 2, "same 1, deleted 1, same 2, deleted 1, same 1"},
		{"x y", "p q r", 5, "deleted 2, inserted 3"},
		{"a b a b", "b a b a", 2, ""},
	} {
		a, b := split(c.a), split(c.b)
		edits := Diff(a, b)
		if got := apply(a, b, edits); !slices.Equal(got, b) {
			t.Errorf("%q to %q: made %q", c.a, c.b, got)
		}
		changed := 0
		var desc []string
		for k, e := range edits {
			desc = append(desc, fmt.Sprintf("%s %d", e.Op, e.N))
			if e.Op != "same" {
				changed += e.N
			}
			if e.Op == "inserted" && k+1 < len(edits) && edits[k+1].Op == "deleted" {
				t.Errorf("%q to %q: an insertion comes before a deletion", c.a, c.b)
			}
		}
		if changed != c.changed || (c.want != "" && strings.Join(desc, ", ") != c.want) {
			t.Errorf("%q to %q: %s", c.a, c.b, strings.Join(desc, ", "))
		}
	}
}
