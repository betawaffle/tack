package tokens

import (
	"strings"
	"testing"
)

// Results go in the order they were called in, within each run of them; a system message
// between them is a barrier, as in DeepSeek's encoder (sort_tool_results_by_call_order).
func TestResultsAreSortedWithinEachRun(t *testing.T) {
	calls := `{"role":"assistant","content":"","tool_calls":[` +
		`{"id":"a","type":"function","function":{"name":"f","arguments":"{}"}},` +
		`{"id":"b","type":"function","function":{"name":"f","arguments":"{}"}},` +
		`{"id":"c","type":"function","function":{"name":"f","arguments":"{}"}}]}`
	result := func(id string) string {
		return `{"role":"tool","tool_call_id":"` + id + `","content":"result ` + strings.ToUpper(id) + `"}`
	}
	system := `{"role":"system","content":"a note"}`
	cases := []struct {
		name     string
		messages []string
		want     []string
	}{
		{"together", []string{result("c"), result("a"), result("b")}, []string{"result A", "result B", "result C"}},
		{"before a system message", []string{result("b"), result("a"), system, result("c")}, []string{"result A", "result B", "a note", "result C"}},
		{"across a system message", []string{result("b"), system, result("a"), result("c")}, []string{"result B", "a note", "result A", "result C"}},
	}
	for _, c := range cases {
		body := `{"model":"deepseek/deepseek-v4.1-flash-1017","messages":[{"role":"user","content":"go"},` + calls + `,` + strings.Join(c.messages, ",") +
			`],"tools":[{"type":"function","function":{"name":"f","description":"F.","parameters":{"type":"object","properties":{}}}}]}`
		p, err := Render("deepseek/deepseek-v4.1-flash-1017", []byte(body), nil)
		if err != nil {
			t.Fatal(err)
		}
		text, at := p.Text(), -1
		for _, w := range c.want {
			i := strings.Index(text, w)
			if i < at {
				t.Errorf("%s: %q comes too soon in %q", c.name, w, text[strings.Index(text, "result"):])
			}
			at = i
		}
	}
}
