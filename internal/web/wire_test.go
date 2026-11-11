package web

import (
	"reflect"
	"testing"

	"encoding/json/jsontext"
	"encoding/json/v2"

	"github.com/betawaffle/tack/internal/core"
	"github.com/betawaffle/tack/internal/tokens"
)

const wireModel = "deepseek/deepseek-v4.1-flash-1017"

func wireRequest(extra string) string {
	return `{"model":"` + wireModel + `","messages":[
{"role":"system","content":"You are an agent.\n\nBe brief."},
{"role":"user","content":"List the files."},
{"role":"assistant","reasoning":"I'll look.","content":"Looking.","tool_calls":[
 {"id":"a","type":"function","function":{"name":"run_code","arguments":"{\"language\":\"python\",\"code\":\"print(1)\"}"}}]},
{"role":"tool","tool_call_id":"a","content":"1"},` + extra + `
{"role":"user","content":"Thanks."},
{"role":"assistant","content":"Welcome."},
{"role":"user","content":"Bye."}],
"tools":[{"type":"function","function":{"name":"run_code","description":"Run code.","parameters":{"type":"object","properties":{"language":{"type":"string"},"code":{"type":"string"}},"required":["language","code"]}}}]}`
}

func renderWire(t *testing.T, body string) (*tokens.Prompt, jsontext.Value) {
	t.Helper()
	p, err := tokens.Render(wireModel, []byte(body), nil)
	if err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(toWire(core.CallPrompt{Call: "x", Prompt: p}))
	if err != nil {
		t.Fatal(err)
	}
	return p, data
}

func TestWirePromptRoundTrips(t *testing.T) {
	p, data := renderWire(t, wireRequest(""))
	var got struct {
		Call   string          `json:"call"`
		Prompt *wirePromptBody `json:"prompt"`
	}
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	back := fromWire(got.Prompt)
	if got.Call != "x" || !reflect.DeepEqual(back.Parts, p.Parts) || !reflect.DeepEqual(back.Segments, p.Segments) || !reflect.DeepEqual(back.Tokens, p.Tokens) {
		t.Errorf("didn't come back the same: %s", data)
	}
}

// The wire forms have every field of what they stand for, so a field added to one isn't
// quietly left out of what the page gets.
func TestWireFormsHaveEveryField(t *testing.T) {
	for _, pair := range [][2]reflect.Type{
		{reflect.TypeFor[tokens.Prompt](), reflect.TypeFor[wirePromptBody]()},
		{reflect.TypeFor[tokens.Part](), reflect.TypeFor[wirePart]()},
		{reflect.TypeFor[tokens.Segment](), reflect.TypeFor[wireSegment]()},
	} {
		for i := range pair[0].NumField() {
			f := pair[0].Field(i)
			if f.IsExported() {
				if _, ok := pair[1].FieldByName(f.Name); !ok {
					t.Errorf("%s has no %s", pair[1], f.Name)
				}
			}
		}
	}
}

func TestWirePatchesStaySmall(t *testing.T) {
	_, before := renderWire(t, wireRequest(""))
	_, after := renderWire(t, wireRequest(`{"role":"system","content":"A note."},`))
	p, err := diffUnordered(before, after)
	if err != nil {
		t.Fatal(err)
	}
	if len(p) > len(after)/4 {
		t.Errorf("adding a message made a patch of %d bytes, of %d", len(p), len(after))
	}
	got, err := applyJSON(before, p)
	if err != nil || string(got) != string(after) {
		t.Errorf("the patch didn't make it: %v", err)
	}
}
