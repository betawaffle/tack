package tokens

import (
	"bytes"
	"encoding/json/v2"
	"flag"
	"os"
	"testing"
)

var update = flag.Bool("update", false, "write testdata/view_fixture.json again")

// fixtureBody has every kind of part the page draws: a system prompt, tools with nested and
// empty JSON, a user message, an assistant message with reasoning, text and a batch of two
// calls (one with a value over several lines, one with a value that isn't a string), their
// results and another message in one user turn, a system message part way through, and
// another user message.
const fixtureBody = `{"model":"deepseek/deepseek-v4.1-flash-1017","reasoning":{"effort":"high"},"messages":[
{"role":"system","content":"You are an agent.\n\nBe brief."},
{"role":"user","content":"List the files."},
{"role":"assistant","reasoning":"I'll look.","content":"Looking.","tool_calls":[
 {"id":"a","type":"function","function":{"name":"run_code","arguments":"{\"language\":\"python\",\"code\":\"import os\\nfor f in os.listdir('.'):\\n    print(f)\"}"}},
 {"id":"b","type":"function","function":{"name":"scratch","arguments":"{\"op\":\"list\",\"limit\":5}"}}]},
{"role":"tool","tool_call_id":"a","content":"a.txt\nb.txt"},
{"role":"tool","tool_call_id":"b","content":""},
{"role":"user","content":"Also, what's in b.txt?"},
{"role":"system","content":"Now be briefer."},
{"role":"user","content":"Thanks."}],
"tools":[
 {"type":"function","function":{"name":"run_code","description":"Run code.","parameters":{"type":"object","properties":{"language":{"type":"string","enum":["python"]},"code":{"type":"string","description":"A long description that goes on and on so that it wraps when the column is narrow, which is what the hanging indent is for."}},"required":["language","code"]}}},
 {"type":"function","function":{"name":"scratch","description":"Keep notes.","parameters":{"type":"object","properties":{}}}}]}`

// fixtureEarlier is the call before: everything up to the first reply, with a system prompt
// that's since changed and a tool that's since been left out, so the comparison has deletions,
// in a part and of one, as well as insertions.
const fixtureEarlier = `{"model":"deepseek/deepseek-v4.1-flash-1017","reasoning":{"effort":"high"},"messages":[
{"role":"system","content":"You are an agent.\n\nBe short."},
{"role":"user","content":"List the files."}],
"tools":[
 {"type":"function","function":{"name":"old_tool","description":"Since left out.","parameters":{"type":"object","properties":{}}}},
 {"type":"function","function":{"name":"run_code","description":"Run code.","parameters":{"type":"object","properties":{"language":{"type":"string","enum":["python"]},"code":{"type":"string","description":"A long description that goes on and on so that it wraps when the column is narrow, which is what the hanging indent is for."}},"required":["language","code"]}}},
 {"type":"function","function":{"name":"scratch","description":"Keep notes.","parameters":{"type":"object","properties":{}}}}]}`

// TestTheViewFixtureIsCurrent keeps the page's tests (internal/web/prompt.test.js) on what
// the renderer really makes. Run with -update after changing the renderer.
func TestTheViewFixtureIsCurrent(t *testing.T) {
	model := "deepseek/deepseek-v4.1-flash-1017"
	before, err := Render(model, []byte(fixtureEarlier), &Output{
		Reasoning: "I'll look.", Content: "Looking.",
		Calls: []OutputCall{{"run_code", "{\"language\":\"python\",\"code\":\"import os\\nfor f in os.listdir('.'):\\n    print(f)\"}"}, {"scratch", `{"op":"list","limit":5}`}},
	})
	if err != nil {
		t.Fatal(err)
	}
	now, err := Render(model, []byte(fixtureBody), &Output{Reasoning: "Done.", Content: "You're welcome."})
	if err != nil {
		t.Fatal(err)
	}
	cut := func(text string, lengths []int, end int) []string {
		var out []string
		at := 0
		for _, n := range lengths {
			if at >= end {
				break
			}
			out = append(out, text[at:at+n])
			at += n
		}
		return out
	}
	fixture := map[string]any{
		"call": "now", "calls": []string{"before", "now"}, "model": model,
		"prompt": now, "text": now.Text(),
		"previous": "before", "previous_text": before.Text(), "previous_tokens": before.Tokens,
		"changes": Compare(before, cut(before.Text(), before.Tokens, len(before.Text())), now, cut(now.Text(), now.Tokens, now.Generated)),
	}
	data, err := json.Marshal(fixture, json.Deterministic(true))
	if err != nil {
		t.Fatal(err)
	}
	data = append(data, '\n')
	const path = "testdata/view_fixture.json"
	if *update {
		if err := os.WriteFile(path, data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	have, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(have, data) {
		t.Errorf("%s is out of date; run go test ./internal/tokens -run Fixture -update", path)
	}
}
