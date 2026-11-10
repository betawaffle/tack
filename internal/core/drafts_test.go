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
	p, err := c.SaveDraft(d.ID, jsontext.Value(changed))
	if err != nil || !strings.Contains(p.Prompt.Text(), "two") || len(p.Changes) == 0 || p.Changes[0].Op != "same" {
		t.Fatalf("saved = %+v, %v", p.Changes, err)
	}
	if kept, shown, err := c.Draft(d.ID); err != nil || string(kept.Request) != changed || !strings.Contains(shown.Prompt.Text(), "two") {
		t.Errorf("kept = %+v, %v", kept, err)
	}
	if _, err := c.SaveDraft(d.ID, jsontext.Value(`[1]`)); !errors.Is(err, ErrBadRequest) {
		t.Errorf("a request that isn't an object: %v", err)
	}

	// Sending it makes a manual call from the same call, and then it can't change.
	call, updates, err := c.SendDraft(d.ID)
	mustFinish(t, updates, err)
	shown, err := c.ManualPrompt(call)
	if err != nil || shown.Previous != from || !strings.Contains(shown.Prompt.Text(), "two") {
		t.Errorf("sent = %+v, %v", shown, err)
	}
	if _, err := c.SaveDraft(d.ID, jsontext.Value(changed)); !errors.Is(err, ErrDraftSent) {
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
