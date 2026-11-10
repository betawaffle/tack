package core

import (
	"encoding/json/v2"
	"errors"
	"testing"

	"github.com/betawaffle/tack/internal/store"
	"github.com/betawaffle/tack/internal/workspace"
)

func TestAnAgentsStateCanBeSeenAndChangedByHand(t *testing.T) {
	f := newFake(t)
	c := f.open()
	a, _ := c.NewAgent("wren", "")
	put := func(text string) string {
		ref, err := c.cfg.Store.Put([]byte(text))
		if err != nil {
			t.Fatal(err)
		}
		return string(ref)
	}
	manifest, _ := json.Marshal([]workspace.Entry{
		{Path: "a.txt", Type: "file", Size: 5, Blob: put("hello")},
		{Path: "d", Type: "dir"},
		{Path: "d/b.bin", Type: "file", Size: 2, Blob: put("\xff\xfe")},
	})
	ws, _ := c.cfg.Store.Put(manifest)
	if err := c.record(&store.Event{Type: store.AgentStateSet, Agent: a.ID, WorkspaceFiles: ws, Execution: "run1"}); err != nil {
		t.Fatal(err)
	}

	view, err := c.AgentState(a.ID, -1)
	if err != nil || len(view.States) != 2 || view.At != 1 || len(view.Files) != 3 || len(view.Scratch) != 0 || view.States[1].Execution != "run1" {
		t.Fatalf("view = %+v, %v", view, err)
	}
	if f, err := c.AgentFile(a.ID, -1, "a.txt"); err != nil || f.Text != "hello" {
		t.Errorf("a.txt = %+v, %v", f, err)
	}
	if f, err := c.AgentFile(a.ID, -1, "d/b.bin"); err != nil || !f.Binary || f.Text != "" || f.Size != 2 {
		t.Errorf("b.bin = %+v, %v", f, err)
	}
	// The state it was made with is empty.
	if first, _ := c.AgentState(a.ID, 0); len(first.Files) != 0 {
		t.Errorf("first = %+v", first)
	}

	// Changes by hand are states of their own.
	if err := c.SetAgentScratch(a.ID, "name", "Wren", false); err != nil {
		t.Fatal(err)
	}
	if err := c.DeleteAgentFile(a.ID, "d"); err != nil {
		t.Fatal(err)
	}
	view, _ = c.AgentState(a.ID, -1)
	if len(view.States) != 4 || view.Scratch["name"] != "Wren" || len(view.Files) != 1 || view.Files[0].Path != "a.txt" || view.States[3].By != "user" {
		t.Errorf("after = %+v", view)
	}
	// Setting it back to the start empties it, and setting it forward again brings it back.
	if err := c.SetAgentState(a.ID, 0); err != nil {
		t.Fatal(err)
	}
	if view, _ = c.AgentState(a.ID, -1); len(view.Files) != 0 || len(view.Scratch) != 0 {
		t.Errorf("set back = %+v", view)
	}
	if err := c.SetAgentState(a.ID, 3); err != nil {
		t.Fatal(err)
	}
	if view, _ = c.AgentState(a.ID, -1); view.Scratch["name"] != "Wren" || len(view.States) != 6 {
		t.Errorf("set forward = %+v", view)
	}
	if err := c.SetAgentScratch(a.ID, "", "x", false); !errors.Is(err, ErrBadRequest) {
		t.Errorf("an empty key: %v", err)
	}
	// Nothing changes by hand while a tool runs as it.
	c.mu.Lock()
	c.agents[a.ID].running = true
	c.mu.Unlock()
	if err := c.SetAgentScratch(a.ID, "k", "v", false); !errors.Is(err, ErrAgentBusy) {
		t.Errorf("while running: %v", err)
	}
}
