package web

import (
	"strings"
	"testing"
	"unicode/utf16"

	"encoding/json/jsontext"
	"encoding/json/v2"
)

// putBack is what the page does: each piece goes back in the string at its pointer, at its
// offset, in order.
func putBack(t *testing.T, data []byte, known map[string]string) []byte {
	t.Helper()
	var v any
	if err := json.Unmarshal(data, &v); err != nil {
		t.Fatal(err)
	}
	root := v.(map[string]any)
	list, _ := root["streamed"].([]any)
	delete(root, "streamed")
	// Each text's length is there, for the page to check it has just that.
	lengths, _ := root["lengths"].(map[string]any)
	for kind, n := range lengths {
		if int(n.(float64)) != len(utf16.Encode([]rune(known[kind]))) {
			t.Errorf("%s's length is %v", kind, n)
		}
	}
	delete(root, "lengths")
	for _, item := range list {
		a := item.(map[string]any)
		parts := strings.Split(a["at"].(string), "/")[1:]
		var parent any = root
		for _, p := range parts[:len(parts)-1] {
			p = strings.NewReplacer("~1", "/", "~0", "~").Replace(p)
			switch c := parent.(type) {
			case map[string]any:
				parent = c[p]
			case []any:
				var n int
				json.Unmarshal([]byte(p), &n)
				parent = c[n]
			}
		}
		last := strings.NewReplacer("~1", "/", "~0", "~").Replace(parts[len(parts)-1])
		get := func() string {
			switch c := parent.(type) {
			case map[string]any:
				return c[last].(string)
			case []any:
				var n int
				json.Unmarshal([]byte(last), &n)
				return c[n].(string)
			}
			return ""
		}
		units := utf16.Encode([]rune(get()))
		off := int(a["offset"].(float64))
		s := string(utf16.Decode(units[:off])) + known[a["is"].(string)] + string(utf16.Decode(units[off:]))
		switch c := parent.(type) {
		case map[string]any:
			c[last] = s
		case []any:
			var n int
			json.Unmarshal([]byte(last), &n)
			c[n] = s
		}
	}
	out, _ := json.Marshal(v, json.Deterministic(true))
	return out
}

func TestStreamedTextGoesOnce(t *testing.T) {
	reasoning := strings.Repeat("I'll think about this, é and 𝄞. ", 4)
	content := strings.Repeat("Here's the answer, at some length. ", 3)
	known := map[string]string{"reasoning": reasoning, "content": content}
	reply := map[string]any{
		"re": 1,
		"request": map[string]any{"o": map[string]any{"messages": map[string]any{"s": []any{3, 0, []any{
			map[string]any{"role": "assistant", "reasoning": reasoning, "content": content},
		}}}}},
		"prompt": map[string]any{"o": map[string]any{
			"previous_text": map[string]any{"t": []any{10, 0, "<think>" + reasoning + "</think>" + content + "<end>"}},
			"a/b~c":         []any{"x " + content, content + content},
		}},
		"short": "short text isn't touched",
	}
	data, _ := json.Marshal(reply, json.Deterministic(true))
	out, err := withStreamed(data, known)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(out), "think about this") || strings.Contains(string(out), "the answer") {
		t.Errorf("what streamed is still in it: %s", out)
	}
	if !jsontext.Value(out).IsValid() {
		t.Fatalf("not JSON: %s", out)
	}
	if back := putBack(t, out, known); string(back) != string(data) {
		t.Errorf("put back, it's\n%s\nnot\n%s", back, data)
	}
	// Nothing streamed, or too little to bother with: it's as it was.
	if same, _ := withStreamed(data, map[string]string{"content": "", "reasoning": "short"}); string(same) != string(data) {
		t.Errorf("changed with nothing to take out: %s", same)
	}
}
