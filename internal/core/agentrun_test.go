package core

import (
	"context"
	"encoding/json/jsontext"
	"errors"
	"strings"
	"testing"

	"github.com/betawaffle/tack/internal/store"
)

func TestADraftsCallRunsAsItsAgent(t *testing.T) {
	f := newFake(t)
	f.settings = store.Settings{Model: "deepseek/deepseek-v4.1-flash-1017"}
	runs := 0
	f.tools = []Tool{{
		Name:       "remember",
		Definition: jsontext.Value(`{"type":"function","function":{"name":"remember","description":"Keep a name.","parameters":{"type":"object","properties":{"name":{"type":"string"}}}}}`),
		Inspect: func(call Call) ([]string, error) {
			if strings.Contains(string(call.Args["name"]), "net") {
				return []string{"network"}, nil
			}
			return nil, nil
		},
		Run: func(_ context.Context, call Call) ([]byte, error) {
			runs++
			before, _ := call.Scratch("read", "name", "")
			if _, err := call.Scratch("write", "name", string(call.Args["name"])); err != nil {
				return nil, err
			}
			if strings.Contains(string(call.Args["name"]), "fail") {
				return []byte("half done"), errors.New("it broke")
			}
			return []byte("was: " + before), nil
		},
	}}
	c := f.open()
	d, err := c.NewBlankDraft("", []Offer{{Name: "remember"}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	// A draft with a call the model made, and nothing in its result yet.
	request := strings.Replace(string(d.Request), `{"role":"user","content":""}`,
		`{"role":"user","content":"Remember Wren."},{"role":"assistant","content":"","tool_calls":[{"id":"c1","type":"function","function":{"name":"remember","arguments":"{\"name\":\"Wren\"}"}},{"id":"c2","type":"function","function":{"name":"remember","arguments":"{\"name\":\"net fail\"}"}}]},{"role":"tool","tool_call_id":"c1","content":""},{"role":"tool","tool_call_id":"c2","content":""}`, 1)
	if _, err := c.SaveDraft(d.ID, jsontext.Value(request), nil, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := c.RunDraftCall(d.ID, "c1", nil); !errors.Is(err, ErrNoAgentForDraft) {
		t.Errorf("without an agent: %v", err)
	}
	a, _ := c.NewAgent("wren", "")
	if err := c.SetDraftAgent(d.ID, a.ID); err != nil {
		t.Fatal(err)
	}
	if flags, err := c.DraftCallFlags(d.ID, "c2"); err != nil || len(flags) != 1 || flags[0] != "network" {
		t.Errorf("flags = %v, %v", flags, err)
	}

	run, err := c.RunDraftCall(d.ID, "c1", nil)
	if err != nil || run.Result != "was: There's nothing under that key." || run.Failure != "" {
		t.Fatalf("run = %+v, %v", run, err)
	}
	// The agent keeps what it changed, and the result is in the draft.
	if got, _ := c.Agent(a.ID); got.State.Scratch == "" || got.States != 1 {
		t.Errorf("agent = %+v", got)
	}
	kept, _, _ := c.Draft(d.ID)
	if !strings.Contains(string(kept.Request), `"content":"was: There's nothing under that key."`) {
		t.Errorf("draft = %s", kept.Request)
	}

	// Run with flags other than the ones it has, it doesn't run.
	before := runs
	if _, err := c.RunDraftCall(d.ID, "c2", nil); !errors.Is(err, ErrCallChanged) || runs != before {
		t.Errorf("unapproved = %v", err)
	}
	// A run that fails says so in what the model sees, after what it wrote.
	run, err = c.RunDraftCall(d.ID, "c2", []string{"network"})
	if err != nil || run.Result != "half done\n\nThe tool reported an error: it broke" {
		t.Errorf("failed run = %+v, %v", run, err)
	}
	// It ran with what the first run left.
	if again, err := c.RunDraftCall(d.ID, "c1", nil); err != nil || again.Result != `was: "net fail"` {
		t.Errorf("again = %+v, %v", again, err)
	}
	if _, err := c.RunDraftCall(d.ID, "nope", nil); !errors.Is(err, ErrNotFound) {
		t.Errorf("a call that isn't there: %v", err)
	}

	// Turning off one call's result turns off the model's message that made it, and every
	// result of that message: what's left is the system prompt, if any, and the user's message.
	kept, _, _ = c.Draft(d.ID)
	p, err := c.SaveDraft(d.ID, kept.Request, []int{2}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if again, _, _ := c.Draft(d.ID); len(again.Off) != 3 || again.Off[0] != 1 || again.Off[2] != 3 {
		t.Errorf("off = %v", again.Off)
	}
	if len(p.Shown) != 1 || p.Shown[0] != 0 || strings.Contains(p.Prompt.Text(), `invoke name="remember"`) {
		t.Errorf("shown = %v, prompt = %q", p.Shown, p.Prompt.Text())
	}
	// What's turned off isn't sent.
	f.mu.Lock()
	sentBefore := len(f.bodies)
	f.mu.Unlock()
	_, updates, err := c.SendDraft(d.ID)
	mustFinish(t, updates, err)
	f.mu.Lock()
	body := string(f.bodies[sentBefore])
	f.mu.Unlock()
	if strings.Contains(body, "tool_call") || !strings.Contains(body, "Remember Wren.") {
		t.Errorf("sent = %s", body)
	}

	// After a restart, the agent's state is still what the runs left.
	c = f.open()
	if got, _ := c.Agent(a.ID); got.States != 3 {
		t.Errorf("after a restart = %+v", got)
	}
}
