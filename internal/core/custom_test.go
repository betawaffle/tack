package core

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"strings"
	"testing"

	"github.com/betawaffle/tack/internal/openrouter"
	"github.com/betawaffle/tack/internal/store"
)

const lookup = `{"type":"function","function":{"name":"lookup","description":"Look something up.","parameters":{"type":"object","properties":{"q":{"type":"string"}},"required":["q"]},"strict":true}}`

var lookupCall = wireCall{"call-l", "lookup", `{"q":"tea"}`}

// startCustom starts a conversation offering lookup, then echo, and has the model call both.
func startCustom(t *testing.T, f *toolFake, c *Core) string {
	t.Helper()
	id, err := c.NewConversationWith(Options{System: "x", Offer: []Offer{
		{Name: "lookup", Custom: true, Definition: jsontext.Value(lookup)},
		{Name: "echo"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	f.replyWith(toolStream(lookupCall, callA))
	updates, err := c.Send(id, "go")
	mustFinish(t, updates, err)
	f.replyWith(replyStream)
	return id
}

func TestCustomToolsAreOfferedInOrderExactly(t *testing.T) {
	f := newToolFake(t)
	c := f.open()
	startCustom(t, f, c)
	var body struct {
		Tools []jsontext.Value `json:"tools"`
	}
	if err := json.Unmarshal(f.bodies[0], &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Tools) != 2 || string(body.Tools[0]) != lookup || !strings.Contains(string(body.Tools[1]), `"echo"`) {
		t.Errorf("tools = %s", body.Tools)
	}
}

func TestACustomToolsResultIsWhatThePersonWrites(t *testing.T) {
	f := newToolFake(t)
	c := f.open()
	id := startCustom(t, f, c)
	got := f.calls(c, id)
	if !got[0].Custom || got[0].Invalid != "" || !got[0].Decidable || got[1].Custom {
		t.Fatalf("calls = %+v", got)
	}
	d := f.decision(c, id, store.DecideCalls)
	// Allowing it without a result isn't an answer, and a result for a built-in tool isn't either.
	if _, err := c.DecideWith(id, d.ID, Decision{Allow: []string{"call-l", "call-a"}}); !errors.Is(err, ErrBadChoice) {
		t.Errorf("no result: %v", err)
	}
	if _, err := c.DecideWith(id, d.ID, Decision{Allow: []string{"call-l", "call-a"}, Results: map[string]string{"call-l": "x", "call-a": "y"}}); !errors.Is(err, ErrBadChoice) {
		t.Errorf("built-in result: %v", err)
	}
	updates, err := c.DecideWith(id, d.ID, Decision{Allow: []string{"call-l", "call-a"}, Results: map[string]string{"call-l": "Tea is a drink."}})
	mustFinish(t, updates, err)
	// Nothing ran for it, and the echo output still waits for its own decision.
	if len(f.runs) != 1 || f.runs[0] != "a" {
		t.Errorf("runs = %v", f.runs)
	}
	f.decide(c, id, store.DecideOutput, "", "call-a")
	f.wantMessages(1, sys("x"), user("go"), assistantCalls(lookupCall, callA), toolMsg("call-l", "Tea is a drink."), toolMsg("call-a", "echoed a"))
	// It's still a custom tool after a restart, and nothing failed it as removed.
	c = f.open()
	if got := f.calls(c, id); !got[0].Custom || got[0].State != "sent" || got[0].Result != "Tea is a drink." {
		t.Errorf("after a restart: %+v", got[0])
	}
}

func TestADeniedCustomCallGetsTheDenial(t *testing.T) {
	f := newToolFake(t)
	c := f.open()
	id := startCustom(t, f, c)
	d := f.decision(c, id, store.DecideCalls)
	updates, err := c.DecideWith(id, d.ID, Decision{Allow: []string{"call-a"}, Text: "Not that one."})
	mustFinish(t, updates, err)
	f.decide(c, id, store.DecideOutput, "", "call-a")
	f.wantMessages(1, sys("x"), user("go"), assistantCalls(lookupCall, callA), toolMsg("call-l", "Not that one."), toolMsg("call-a", "echoed a"))
}

func TestOffersAreChecked(t *testing.T) {
	f := newToolFake(t)
	c := f.open()
	for name, offer := range map[string][]Offer{
		"no name":        {{Custom: true, Definition: jsontext.Value(lookup)}},
		"twice":          {{Name: "echo"}, {Name: "echo"}},
		"taken":          {{Name: "echo", Custom: true, Definition: jsontext.Value(definition)}},
		"unknown":        {{Name: "nope"}},
		"mismatched":     {{Name: "other", Custom: true, Definition: jsontext.Value(lookup)}},
		"not json":       {{Name: "lookup", Custom: true, Definition: jsontext.Value(`{`)}},
		"changed schema": {{Name: "echo", Definition: jsontext.Value(strings.Replace(definition, `"string"`, `"number"`, 1))}},
	} {
		if _, err := c.NewConversationWith(Options{Offer: offer}); !errors.Is(err, ErrBadTools) {
			t.Errorf("%s: %v", name, err)
		}
	}
	// An empty list means no tools at all.
	id, err := c.NewConversationWith(Options{Tools: true, Offer: []Offer{}})
	if err != nil {
		t.Fatal(err)
	}
	if conv, _ := c.Conversation(id); conv.Tools {
		t.Error("an empty offer should mean no tools")
	}
}

func TestToolsCanChangeLikeSettings(t *testing.T) {
	f := newToolFake(t)
	c := f.open()
	id, _ := c.NewConversation("x")
	proposal, err := c.ProposeTools(id, []Offer{{Name: "lookup", Custom: true, Definition: jsontext.Value(lookup)}})
	if err != nil {
		t.Fatal(err)
	}
	conv, _ := c.Conversation(id)
	if p := conv.PendingSettings; p == nil || !p.Tools || !strings.Contains(string(p.Definitions), "lookup") || len(p.CustomTools) != 1 {
		t.Fatalf("pending = %+v", conv.PendingSettings)
	}
	if err := c.ApplySettings(id, proposal); err != nil {
		t.Fatal(err)
	}
	f.replyWith(toolStream(lookupCall))
	updates, err := c.Send(id, "go")
	mustFinish(t, updates, err)
	if body := string(f.bodies[0]); strings.Contains(body, `"echo"`) || !strings.Contains(body, `"lookup"`) {
		t.Errorf("request = %s", body)
	}
	if got := f.calls(c, id); !got[0].Custom {
		t.Errorf("call = %+v", got[0])
	}
	// Not while a call is waiting, since it was made with the tools it had.
	proposal, _ = c.ProposeTools(id, []Offer{})
	if err := c.ApplySettings(id, proposal); !errors.Is(err, ErrAwaiting) {
		t.Errorf("err = %v", err)
	}
	d := f.decision(c, id, store.DecideCalls)
	f.replyWith(replyStream)
	updates, err = c.DecideWith(id, d.ID, Decision{Allow: []string{"call-l"}, Results: map[string]string{"call-l": "r"}})
	mustFinish(t, updates, err)
	// Applying none at all leaves none, and that survives a restart.
	if err := c.ApplySettings(id, proposal); err != nil {
		t.Fatal(err)
	}
	c = f.open()
	if conv, _ := c.Conversation(id); conv.Tools || len(conv.CustomTools) != 0 {
		t.Errorf("conversation = %+v", conv)
	}
}

func TestAForkKeepsItsCustomTools(t *testing.T) {
	f := newToolFake(t)
	c := f.open()
	id := startCustom(t, f, c)
	d := f.decision(c, id, store.DecideCalls)
	updates, err := c.DecideWith(id, d.ID, Decision{Results: map[string]string{}})
	mustFinish(t, updates, err)
	fork, err := c.Fork(id, 1)
	if err != nil {
		t.Fatal(err)
	}
	if conv, _ := c.Conversation(fork); len(conv.CustomTools) != 1 || conv.CustomTools[0] != "lookup" {
		t.Errorf("fork = %+v", conv.CustomTools)
	}
}

func TestSomeSettingsChangeAtAnyPointAndKeepWhatsHeld(t *testing.T) {
	f := newToolFake(t)
	c := f.open()
	id := f.askForTools(c, callA)
	held, err := c.ProposeSettings(id, store.Settings{Model: "test/model", BudgetUSD: 5, BudgetTopUpUSD: 1})
	if err != nil {
		t.Fatal(err)
	}
	// A call is waiting, and these still change.
	if err := c.ChangeSettingsNow(id, func(s *store.Settings) {
		s.AutoRunCode, s.Host, s.ReasoningEffort = true, "relace/fp4", "low"
	}); err != nil {
		t.Fatal(err)
	}
	conv, _ := c.Conversation(id)
	if !conv.Settings.AutoRunCode || conv.Settings.Host != "relace/fp4" || conv.Settings.ReasoningEffort != "low" {
		t.Errorf("settings = %+v", conv.Settings)
	}
	p := conv.PendingSettings
	if p == nil || p.ID == held || p.Settings.BudgetUSD != 5 || p.Settings.Host != "relace/fp4" || !p.Settings.AutoRunCode {
		t.Errorf("held = %+v", p)
	}
	for name, change := range map[string]func(*store.Settings){
		"model":  func(s *store.Settings) { s.Model = "other" },
		"effort": func(s *store.Settings) { s.ReasoningEffort = "lots" },
	} {
		if err := c.ChangeSettingsNow(id, change); !errors.Is(err, ErrBadSettings) {
			t.Errorf("%s: %v", name, err)
		}
	}
	// The next call goes where it says, with the effort.
	f.decide(c, id, store.DecideCalls, "", "call-a")
	f.decide(c, id, store.DecideOutput, "", "call-a")
	last := string(f.bodies[len(f.bodies)-1])
	if !strings.Contains(last, `"order":["relace/fp4"]`) || !strings.Contains(last, `"reasoning":{"effort":"low"}`) {
		t.Errorf("request = %s", last)
	}
	if first := string(f.bodies[0]); strings.Contains(first, `"reasoning"`) {
		t.Errorf("no effort should send none: %s", first)
	}
}

func TestToolsGetTheRequestAndReplyThatMadeTheCall(t *testing.T) {
	f := newToolFake(t)
	var seen []byte
	f.tools[0].Run = func(_ context.Context, call Call) ([]byte, error) {
		var err error
		seen, err = call.Context()
		return nil, err
	}
	c := f.open()
	id := f.askForTools(c, callA)
	f.decide(c, id, store.DecideCalls, "", "call-a")
	var got struct {
		Request jsontext.Value     `json:"request"`
		Reply   openrouter.Message `json:"reply"`
	}
	if err := json.Unmarshal(seen, &got); err != nil {
		t.Fatalf("%s: %v", seen, err)
	}
	if string(got.Request) != string(f.bodies[0]) {
		t.Errorf("request = %s, want %s", got.Request, f.bodies[0])
	}
	if got.Reply.Role != "assistant" || got.Reply.Reasoning != "Thinking." || len(got.Reply.ToolCalls) != 1 || got.Reply.ToolCalls[0].Function.Arguments != callA.args {
		t.Errorf("reply = %+v", got.Reply)
	}
}
