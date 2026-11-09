package core

import (
	"strings"
	"testing"

	"github.com/betawaffle/tack/internal/store"
)

func TestACallsPromptIsShownWithItsReplyAndChanges(t *testing.T) {
	f := newFake(t)
	f.settings = store.Settings{Model: "deepseek/deepseek-v4.1-flash-1017"}
	c := f.open()
	id, _ := c.NewConversation("")
	updates, err := c.Send(id, "one")
	mustFinish(t, updates, err)
	updates, err = c.Send(id, "two")
	mustFinish(t, updates, err)
	conv, _ := c.Conversation(id)
	var calls []string
	for _, it := range conv.Items {
		if it.Call != "" {
			calls = append(calls, it.Call)
		}
	}
	if len(calls) != 2 {
		t.Fatalf("calls = %v", calls)
	}
	first, err := c.Prompt(id, calls[0])
	if err != nil || first.Previous != "" || len(first.Calls) != 2 {
		t.Fatalf("%+v, %v", first, err)
	}
	second, err := c.Prompt(id, calls[1])
	if err != nil {
		t.Fatal(err)
	}
	text := second.Prompt.Text()
	if second.Previous != calls[0] || second.Prompt.Generated <= 0 || !strings.Contains(text[:second.Prompt.Generated], "one") || !strings.Contains(text[:second.Prompt.Generated], "two") {
		t.Errorf("second = %+v\n%q", second, text)
	}
	if reply := conv.Items[len(conv.Items)-1].Text; reply == "" || !strings.Contains(text[second.Prompt.Generated:], reply) {
		t.Errorf("the reply %q isn't after the prompt: %q", reply, text[second.Prompt.Generated:])
	}
	// The call before ended with its reply; this one adds the new message after it.
	inserted := false
	for _, e := range second.Changes {
		if e.Op == "inserted" {
			inserted = true
		}
	}
	if !inserted || second.Changes[0].Op != "same" {
		t.Errorf("changes = %+v", second.Changes)
	}
	if second.CacheBlock != CacheBlockTokens {
		t.Errorf("cache block = %d", second.CacheBlock)
	}
	// A draft of the next call: this call's request and reply, with a message added.
	req, err := c.Request(id, calls[1])
	if err != nil || req.Output == nil || req.Output.Content == "" || !strings.Contains(string(req.Request), "two") {
		t.Fatalf("request = %+v, %v", req, err)
	}
	draft := strings.Replace(string(req.Request), `"two"}]`, `"two"},{"role":"assistant","content":"`+req.Output.Content+`"},{"role":"user","content":"three"}]`, 1)
	preview, err := Preview(req.Model, []byte(draft), &req)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(preview.Prompt.Text(), "three") || len(preview.Changes) == 0 || preview.Changes[0].Op != "same" || preview.PreviousPrompt == 0 || preview.PreviousPrompt >= len(preview.PreviousTokens) {
		t.Errorf("preview = %+v", preview.Changes)
	}
	// Sending the draft: it's a manual call that remembers where it came from, shown against
	// that call, and a draft can be made from it in turn.
	sent, updates, err := c.SendManual([]byte(draft), id+"/"+calls[1])
	mustFinish(t, updates, err)
	shown, err := c.ManualPrompt(sent)
	if err != nil {
		t.Fatal(err)
	}
	if shown.Previous != id+"/"+calls[1] || len(shown.Calls) != 2 || shown.Calls[1] != "manual/"+sent || shown.Prompt.Generated <= 0 || len(shown.Changes) == 0 {
		t.Errorf("sent = %+v", shown)
	}
	if again, err := c.ManualRequest(sent); err != nil || again.Output == nil || !strings.Contains(string(again.Request), "three") {
		t.Errorf("draft from the sent call = %+v, %v", again, err)
	}
	if _, err := c.Prompt(id, "nope"); err != ErrNotFound {
		t.Errorf("an unknown call: %v", err)
	}
}
