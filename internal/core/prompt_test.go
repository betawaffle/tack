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
	if _, err := c.Prompt(id, "nope"); err != ErrNotFound {
		t.Errorf("an unknown call: %v", err)
	}
}
