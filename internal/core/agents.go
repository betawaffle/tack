package core

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/betawaffle/tack/internal/store"
)

// An agent is its persistent state: its /work and its scratch store. It has one state at a
// time, and each change to it is in the record, so it can be set back to an earlier one. A
// draft names the agent its tools run as.
type agent struct {
	id, slug, forkedFrom string
	created              time.Time
	// states is every state it's had, oldest first; the last is the one it has.
	states []AgentState
	// running says a tool is running as it now. It isn't in the record: nothing survives a
	// restart to be running.
	running bool
}

// AgentState is one state an agent has had.
type AgentState struct {
	Time time.Time `json:"time"`
	// Workspace and Scratch are manifests of the blocks of its /work and its scratch store.
	// Empty is nothing.
	Workspace store.Ref `json:"workspace,omitzero"`
	Scratch   store.Ref `json:"scratch,omitzero"`
	// Execution is the tool run that left it, if one did; By is "user" for one someone set by
	// hand.
	Execution string `json:"execution,omitzero"`
	By        string `json:"by,omitzero"`
}

// Agent is what's shown of an agent.
type Agent struct {
	ID         string     `json:"id"`
	Slug       string     `json:"slug,omitzero"`
	ForkedFrom string     `json:"forked_from,omitzero"`
	Created    time.Time  `json:"created"`
	State      AgentState `json:"state"`
	States     int        `json:"states"`
}

// ErrNoAgent is an agent that doesn't exist.
var ErrNoAgent = fmt.Errorf("%w: there's no such agent", ErrNotFound)

// ErrBadSlug is a slug that can't be used.
var ErrBadSlug = errors.New("a slug is one line of at most 100 characters")

// applyAgent updates agents from an event. The caller holds mu.
func (c *Core) applyAgent(ev *store.Event) {
	if c.agents == nil {
		c.agents = map[string]*agent{}
	}
	if ev.Type == store.AgentCreated {
		c.agents[ev.Agent] = &agent{id: ev.Agent, slug: ev.Slug, forkedFrom: ev.ForkedFrom, created: ev.Time}
		c.agentOrder = append(c.agentOrder, ev.Agent)
		return
	}
	a := c.agents[ev.Agent]
	if a == nil {
		return
	}
	switch ev.Type {
	case store.AgentRenamed:
		a.slug = ev.Slug
	case store.AgentStateSet:
		a.states = append(a.states, AgentState{Time: ev.Time, Workspace: ev.WorkspaceFiles, Scratch: ev.ScratchFiles, Execution: ev.Execution, By: ev.By})
	}
}

func (a *agent) state() AgentState {
	if len(a.states) == 0 {
		return AgentState{Time: a.created}
	}
	return a.states[len(a.states)-1]
}

func (a *agent) shown() Agent {
	return Agent{ID: a.id, Slug: a.slug, ForkedFrom: a.forkedFrom, Created: a.created, State: a.state(), States: len(a.states)}
}

func checkSlug(slug string) error {
	if len(slug) > 100 || strings.ContainsAny(slug, "\r\n") {
		return ErrBadSlug
	}
	return nil
}

// Agents lists the agents, the newest first.
func (c *Core) Agents() []Agent {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]Agent, 0, len(c.agentOrder))
	for i := len(c.agentOrder) - 1; i >= 0; i-- {
		out = append(out, c.agents[c.agentOrder[i]].shown())
	}
	return out
}

// Agent is one agent.
func (c *Core) Agent(id string) (Agent, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	a := c.agents[id]
	if a == nil {
		return Agent{}, ErrNoAgent
	}
	return a.shown(), nil
}

// NewAgent makes an agent with nothing in its /work or scratch store, or, with from, one that
// starts with a copy of that agent's state.
func (c *Core) NewAgent(slug, from string) (Agent, error) {
	slug = strings.TrimSpace(slug)
	if err := checkSlug(slug); err != nil {
		return Agent{}, err
	}
	var state AgentState
	if from != "" {
		parent, err := c.Agent(from)
		if err != nil {
			return Agent{}, err
		}
		state = parent.State
	}
	id := store.NewID()
	if err := c.record(&store.Event{Type: store.AgentCreated, Agent: id, Slug: slug, ForkedFrom: from}); err != nil {
		return Agent{}, err
	}
	if state.Workspace != "" || state.Scratch != "" {
		if err := c.record(&store.Event{Type: store.AgentStateSet, Agent: id, WorkspaceFiles: state.Workspace, ScratchFiles: state.Scratch}); err != nil {
			return Agent{}, err
		}
	}
	return c.Agent(id)
}

// RenameAgent changes an agent's slug. An empty one takes it away.
func (c *Core) RenameAgent(id, slug string) error {
	slug = strings.TrimSpace(slug)
	if err := checkSlug(slug); err != nil {
		return err
	}
	if _, err := c.Agent(id); err != nil {
		return err
	}
	return c.record(&store.Event{Type: store.AgentRenamed, Agent: id, Slug: slug})
}
