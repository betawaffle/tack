package core

import (
	"encoding/json/v2"
	"fmt"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/betawaffle/tack/internal/store"
	"github.com/betawaffle/tack/internal/workspace"
)

// AgentView is an agent with one of its states open: every state it's had, which one is open,
// the files in that state's /work and what's in its scratch store. The first state is the one
// it was made with, which is empty.
type AgentView struct {
	Agent   Agent             `json:"agent"`
	States  []AgentState      `json:"states"`
	At      int               `json:"at"`
	Files   []workspace.Entry `json:"files"`
	Scratch map[string]string `json:"scratch"`
	Running bool              `json:"running,omitzero"`
}

// allStates is every state an agent has had, starting with the empty one it was made with. The
// caller holds mu.
func (a *agent) allStates() []AgentState {
	return append([]AgentState{{Time: a.created}}, a.states...)
}

// AgentState opens state at of an agent, or its current one if at is negative.
func (c *Core) AgentState(id string, at int) (AgentView, error) {
	c.mu.Lock()
	a := c.agents[id]
	if a == nil {
		c.mu.Unlock()
		return AgentView{}, ErrNoAgent
	}
	view := AgentView{Agent: a.shown(), States: a.allStates(), Running: a.running}
	c.mu.Unlock()
	if at < 0 || at >= len(view.States) {
		at = len(view.States) - 1
	}
	view.At = at
	state := view.States[at]
	var err error
	if view.Files, err = c.filesOf(state.Workspace); err != nil {
		return AgentView{}, err
	}
	if view.Scratch, err = c.scratchOf(state.Scratch); err != nil {
		return AgentView{}, err
	}
	return view, nil
}

func (c *Core) filesOf(manifest store.Ref) ([]workspace.Entry, error) {
	entries := []workspace.Entry{}
	if manifest == "" {
		return entries, nil
	}
	data, err := c.cfg.Store.Get(manifest)
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(data, &entries); err != nil {
		return nil, err
	}
	return entries, nil
}

// AgentFile is a file in state at of an agent's /work (its current one if at is negative).
// Text is its content if it's text; otherwise only its size says anything.
type AgentFile struct {
	Path   string `json:"path"`
	Size   int64  `json:"size"`
	Text   string `json:"text,omitzero"`
	Binary bool   `json:"binary,omitzero"`
}

func (c *Core) AgentFile(id string, at int, path string) (AgentFile, error) {
	view, err := c.AgentState(id, at)
	if err != nil {
		return AgentFile{}, err
	}
	i := slices.IndexFunc(view.Files, func(e workspace.Entry) bool { return e.Path == path && e.Type == "file" })
	if i < 0 {
		return AgentFile{}, fmt.Errorf("%w: there's no file %q there", ErrNotFound, path)
	}
	e := view.Files[i]
	data, err := c.cfg.Store.Get(store.Ref(e.Blob))
	if err != nil {
		return AgentFile{}, err
	}
	out := AgentFile{Path: e.Path, Size: int64(len(data))}
	if utf8.Valid(data) {
		out.Text = string(data)
	} else {
		out.Binary = true
	}
	return out, nil
}

// changeAgent sets an agent's state to what change makes of its current one, by hand, unless
// a tool's running as it.
func (c *Core) changeAgent(id string, change func(AgentState) (AgentState, error)) error {
	c.mu.Lock()
	a := c.agents[id]
	if a == nil {
		c.mu.Unlock()
		return ErrNoAgent
	}
	if a.running {
		c.mu.Unlock()
		return ErrAgentBusy
	}
	current := a.state()
	c.mu.Unlock()
	next, err := change(current)
	if err != nil {
		return err
	}
	if next.Workspace == current.Workspace && next.Scratch == current.Scratch {
		return nil
	}
	return c.record(&store.Event{Type: store.AgentStateSet, Agent: id, WorkspaceFiles: next.Workspace, ScratchFiles: next.Scratch, By: "user"})
}

// SetAgentState sets an agent back, or forward, to state at of the ones it's had. That's a new
// state of its own, so nothing it had is lost.
func (c *Core) SetAgentState(id string, at int) error {
	return c.changeAgent(id, func(AgentState) (AgentState, error) {
		c.mu.Lock()
		states := c.agents[id].allStates()
		c.mu.Unlock()
		if at < 0 || at >= len(states) {
			return AgentState{}, fmt.Errorf("%w: it has no state %d", ErrBadRequest, at)
		}
		return states[at], nil
	})
}

// SetAgentScratch writes text under key in an agent's scratch store, or deletes the key.
func (c *Core) SetAgentScratch(id, key, text string, remove bool) error {
	if key == "" {
		return fmt.Errorf("%w: a scratch key can't be empty", ErrBadRequest)
	}
	return c.changeAgent(id, func(state AgentState) (AgentState, error) {
		scratch, err := c.scratchOf(state.Scratch)
		if err != nil {
			return state, err
		}
		if remove {
			delete(scratch, key)
		} else {
			scratch[key] = text
		}
		state.Scratch, err = c.putScratch(scratch)
		return state, err
	})
}

// DeleteAgentFile takes a file, or a directory and everything in it, out of an agent's /work.
func (c *Core) DeleteAgentFile(id, path string) error {
	return c.changeAgent(id, func(state AgentState) (AgentState, error) {
		files, err := c.filesOf(state.Workspace)
		if err != nil {
			return state, err
		}
		kept := slices.DeleteFunc(files, func(e workspace.Entry) bool {
			return e.Path == path || strings.HasPrefix(e.Path, strings.TrimSuffix(path, "/")+"/")
		})
		if len(kept) == 0 {
			state.Workspace = ""
			return state, nil
		}
		data, err := json.Marshal(kept)
		if err != nil {
			return state, err
		}
		state.Workspace, err = c.cfg.Store.Put(data)
		return state, err
	})
}
