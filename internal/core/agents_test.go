package core

import (
	"errors"
	"testing"

	"github.com/betawaffle/tack/internal/store"
)

func TestAgentsAreMadeNamedForkedAndUsedByDrafts(t *testing.T) {
	f := newFake(t)
	f.settings = store.Settings{Model: "deepseek/deepseek-v4.1-flash-1017"}
	c := f.open()
	a, err := c.NewAgent("  first  ", "")
	if err != nil || a.Slug != "first" || a.ID == "" || a.States != 0 {
		t.Fatalf("new = %+v, %v", a, err)
	}
	if _, err := c.NewAgent("two\nlines", ""); !errors.Is(err, ErrBadSlug) {
		t.Errorf("a slug of two lines: %v", err)
	}
	if err := c.RenameAgent(a.ID, "renamed"); err != nil {
		t.Fatal(err)
	}
	// A fork starts with the state of the one it came from.
	if err := c.record(&store.Event{Type: store.AgentStateSet, Agent: a.ID, WorkspaceFiles: "sha256:aa", ScratchFiles: "sha256:bb"}); err != nil {
		t.Fatal(err)
	}
	fork, err := c.NewAgent("", a.ID)
	if err != nil || fork.ForkedFrom != a.ID || fork.State.Workspace != "sha256:aa" || fork.State.Scratch != "sha256:bb" {
		t.Fatalf("fork = %+v, %v", fork, err)
	}
	if _, err := c.NewAgent("", "nope"); !errors.Is(err, ErrNotFound) {
		t.Errorf("a fork of nothing: %v", err)
	}
	if list := c.Agents(); len(list) != 2 || list[0].ID != fork.ID || list[1].Slug != "renamed" {
		t.Errorf("agents = %+v", list)
	}

	// After a restart, they're all there, from the record.
	c = f.open()
	if got, err := c.Agent(a.ID); err != nil || got.Slug != "renamed" || got.States != 1 {
		t.Errorf("after a restart = %+v, %v", got, err)
	}

	// A draft runs as an agent, the call it becomes says so, and the draft after that one
	// runs as the same agent.
	id, _ := c.NewConversation("")
	updates, err := c.Send(id, "one")
	mustFinish(t, updates, err)
	conv, _ := c.Conversation(id)
	d, err := c.NewDraft(id + "/" + conv.Items[len(conv.Items)-1].Call)
	if err != nil || d.Agent != "" {
		t.Fatalf("draft = %+v, %v", d, err)
	}
	if err := c.SetDraftAgent(d.ID, "nope"); !errors.Is(err, ErrNotFound) {
		t.Errorf("an agent that isn't one: %v", err)
	}
	if err := c.SetDraftAgent(d.ID, a.ID); err != nil {
		t.Fatal(err)
	}
	call, updates, err := c.SendDraft(d.ID)
	mustFinish(t, updates, err)
	next, err := c.NewDraft("manual/" + call)
	if err != nil || next.Agent != a.ID {
		t.Errorf("next = %+v, %v", next, err)
	}
}
