package core

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"strings"
	"testing"

	"github.com/betawaffle/tack/internal/store"
)

func TestADraftIsKeptChangedAndSent(t *testing.T) {
	f := newFake(t)
	f.settings = store.Settings{Model: "deepseek/deepseek-v4.1-flash-1017"}
	c := f.open()
	id, _ := c.NewConversation("")
	updates, err := c.Send(id, "one")
	mustFinish(t, updates, err)
	conv, _ := c.Conversation(id)
	from := id + "/" + conv.Items[len(conv.Items)-1].Call

	// It starts as the call's request, its reply, and an empty user message.
	d, err := c.NewDraft(from)
	if err != nil {
		t.Fatal(err)
	}
	var req struct {
		Messages []map[string]jsontext.Value `json:"messages"`
	}
	if err := json.Unmarshal(d.Request, &req); err != nil {
		t.Fatal(err)
	}
	n := len(req.Messages)
	if n < 3 || string(req.Messages[n-2]["role"]) != `"assistant"` || string(req.Messages[n-1]["role"]) != `"user"` || string(req.Messages[n-1]["content"]) != `""` {
		t.Fatalf("draft = %s", d.Request)
	}
	// Edit on the same call opens it again.
	if again, err := c.NewDraft(from); err != nil || again.ID != d.ID {
		t.Errorf("again = %+v, %v", again, err)
	}

	// Changing it saves it, and it's shown against the call it came from.
	changed := strings.Replace(string(d.Request), `"content":""}]`, `"content":"two"}]`, 1)
	if changed == string(d.Request) {
		t.Fatalf("no empty message to fill: %s", d.Request)
	}
	p, err := c.SaveDraft(d.ID, jsontext.Value(changed), nil, nil)
	if err != nil || !strings.Contains(p.Prompt.Text(), "two") || len(p.Changes) == 0 || p.Changes[0].Op != "same" {
		t.Fatalf("saved = %+v, %v", p.Changes, err)
	}
	if kept, shown, err := c.Draft(d.ID); err != nil || string(kept.Request) != changed || !strings.Contains(shown.Prompt.Text(), "two") {
		t.Errorf("kept = %+v, %v", kept, err)
	}
	if _, err := c.SaveDraft(d.ID, jsontext.Value(`[1]`), nil, nil); !errors.Is(err, ErrBadRequest) {
		t.Errorf("a request that isn't an object: %v", err)
	}

	// Sending it makes a manual call from the same call, and then it can't change.
	call, updates, err := c.SendDraft(d.ID)
	mustFinish(t, updates, err)
	shown, err := c.ManualPrompt(call)
	if err != nil || shown.Previous != from || !strings.Contains(shown.Prompt.Text(), "two") {
		t.Errorf("sent = %+v, %v", shown, err)
	}
	if _, err := c.SaveDraft(d.ID, jsontext.Value(changed), nil, nil); !errors.Is(err, ErrDraftSent) {
		t.Errorf("changing a sent draft: %v", err)
	}
	if _, _, err := c.SendDraft(d.ID); !errors.Is(err, ErrDraftSent) {
		t.Errorf("sending it again: %v", err)
	}
	// Edit on the call now starts a new one.
	if next, err := c.NewDraft(from); err != nil || next.ID == d.ID {
		t.Errorf("next = %+v, %v", next, err)
	}
	if _, _, err := c.Draft("../nope"); !errors.Is(err, store.ErrNoDraft) {
		t.Errorf("a draft that isn't one: %v", err)
	}
}

func TestABlankDraftStartsFromWhatItsGiven(t *testing.T) {
	f := newFake(t)
	f.settings = store.Settings{Model: "deepseek/deepseek-v4.1-flash-1017"}
	f.tools = []Tool{{Name: "run_code", Definition: jsontext.Value(`{"type":"function","function":{"name":"run_code","description":"Run code.","parameters":{"type":"object"}}}`)}}
	c := f.open()
	d, err := c.NewBlankDraft("Be brief.", []Offer{{Name: "run_code"}, {Name: "note"}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	var req struct {
		Model     string `json:"model"`
		SessionID string `json:"session_id"`
		Messages  []struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"messages"`
		Tools []struct {
			Function struct {
				Name string `json:"name"`
			} `json:"function"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(d.Request, &req); err != nil {
		t.Fatal(err)
	}
	if req.Model != f.settings.Model || d.Model != req.Model || req.SessionID != d.ID || d.From != "" {
		t.Errorf("draft = %+v, request = %+v", d, req)
	}
	if len(req.Messages) != 2 || req.Messages[0].Role != "system" || req.Messages[0].Content != "Be brief." || req.Messages[1].Role != "user" || req.Messages[1].Content != "" {
		t.Errorf("messages = %+v", req.Messages)
	}
	// The note tool isn't offered to a draft: it belongs to conversations.
	if len(req.Tools) != 1 || req.Tools[0].Function.Name != "run_code" {
		t.Errorf("tools = %+v", req.Tools)
	}
	if _, p, err := c.Draft(d.ID); err != nil || !strings.Contains(p.Prompt.Text(), "Be brief.") {
		t.Errorf("shown = %v", err)
	}
	// Its model is the one its request names.
	changed := strings.Replace(string(d.Request), `"model":"deepseek/deepseek-v4.1-flash-1017"`, `"model":"deepseek/deepseek-v4.1-flash"`, 1)
	if _, err := c.SaveDraft(d.ID, jsontext.Value(changed), nil, nil); err != nil {
		t.Fatal(err)
	}
	if kept, _, _ := c.Draft(d.ID); kept.Model != "deepseek/deepseek-v4.1-flash" {
		t.Errorf("model = %q", kept.Model)
	}
	// A tool that's turned off isn't offered, and with none left, there's no tools section.
	kept, _, _ := c.Draft(d.ID)
	p, err := c.SaveDraft(d.ID, kept.Request, nil, []int{0})
	if err != nil || len(p.ShownTools) != 0 || strings.Contains(p.Prompt.Text(), "Run code.") || strings.Contains(p.Prompt.Text(), "## Tools") {
		t.Errorf("without its tool = %v, %v", p.ShownTools, err)
	}
	if again, _, _ := c.Draft(d.ID); len(again.OffTools) != 1 || !strings.Contains(string(again.Request), "Run code.") {
		t.Errorf("kept = %+v", again)
	}
	if list, err := c.Drafts(); err != nil || len(list) != 1 || list[0].ID != d.ID {
		t.Errorf("drafts = %+v, %v", list, err)
	}
}

func TestLeavingOutAToolChangesOnlyThatTool(t *testing.T) {
	tool := func(name, description string) string {
		return `{"type":"function","function":{"name":"` + name + `","description":"` + description + `","parameters":{"type":"object","properties":{}}}}`
	}
	request := func(tools ...string) jsontext.Value {
		return jsontext.Value(`{"model":"deepseek/deepseek-v4.1-flash-1017","messages":[{"role":"system","content":"Be brief."},{"role":"user","content":"Hi."}],"tools":[` + strings.Join(tools, ",") + `]}`)
	}
	before := CallRequest{Model: "deepseek/deepseek-v4.1-flash-1017", Request: request(tool("first", "The first tool."), tool("second", "The second tool."))}
	p, err := Preview(before.Model, request(tool("second", "The second tool.")), &before)
	if err != nil {
		t.Fatal(err)
	}
	// The second tool's schema is all still there, so nothing's inserted: the first's is
	// deleted, and what's left of it doesn't match the start of the second.
	for _, e := range p.Changes {
		if e.Op == "inserted" {
			t.Errorf("changes = %+v", p.Changes)
		}
	}
}

func TestRepeatedCallIDsPairWithTheNearestCall(t *testing.T) {
	// An older draft named every reply's calls draft-0, draft-1, and so on.
	request := jsontext.Value(`{"messages":[
{"role":"user","content":"a"},
{"role":"assistant","content":"","tool_calls":[{"id":"draft-0","type":"function","function":{"name":"scratch","arguments":"{}"}}]},
{"role":"tool","tool_call_id":"draft-0","content":"one"},
{"role":"assistant","content":"","tool_calls":[{"id":"draft-0","type":"function","function":{"name":"run_code","arguments":"{}"}}]},
{"role":"tool","tool_call_id":"draft-0","content":""}]}`)
	if off := pairedOff(request, []int{4}); len(off) != 2 || off[0] != 3 || off[1] != 4 {
		t.Errorf("off = %v", off)
	}
	dc, err := findDraftCall(&store.Draft{Request: request}, "draft-0")
	if err != nil || dc.name != "run_code" || dc.result != 4 {
		t.Errorf("call = %+v, %v", dc, err)
	}
}
