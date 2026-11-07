package core

import (
	"errors"
	"os"
	"time"

	"github.com/betawaffle/tack/internal/store"
)

// ErrRestarting says tack is finishing up to restart, so nothing new can start.
var ErrRestarting = errors.New("tack is restarting for an update, so this wasn't started. Try again in a moment")

// ErrStale says an action was based on an out-of-date view of the conversation.
var ErrStale = errors.New("this conversation has changed since this page last showed it, so that wasn't done")

// A restart waits for a clean stopping point: every running turn finishes, so none
// needs someone to press Continue afterwards. What's waiting on a person isn't running
// and the record keeps it. Nothing new that would call the model or run a tool starts
// meanwhile, but messages can still be held and everything else recorded.

// Status is what the page needs to know about tack itself.
type Status struct {
	Version string    `json:"version"`
	Started time.Time `json:"started"`
	// What this run replaced and what's new in it, if a requested restart said.
	Previous string   `json:"previous,omitzero"`
	Changes  []string `json:"changes,omitzero"`
	// Restarting says a restart is waiting for the conversations in Waiting.
	Restarting bool      `json:"restarting,omitzero"`
	Waiting    []Summary `json:"waiting,omitzero"`
}

// CheckRevision refuses if a conversation isn't at the revision an action was based on.
// What's on someone's page is what they're acting on, so if that's out of date, the action
// isn't what they meant.
func (c *Core) CheckRevision(convID string, revision int) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	conv := c.convs[convID]
	if conv == nil {
		return ErrNotFound
	}
	if conv.revision != revision {
		return ErrStale
	}
	return nil
}

// Started records that this process is the one running. It's separate from New so that
// reading the record writes nothing.
func (c *Core) Started(version string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.status = Status{Version: version, Previous: c.lastVersion, Changes: c.nextChanges}
	ev := &store.Event{Type: store.TackStarted, Version: version, PID: os.Getpid()}
	if err := c.cfg.Store.Append(ev); err != nil {
		return err
	}
	c.apply(ev)
	c.status.Started = c.lastStarted
	return nil
}

// RequestRestart stops new work from starting. Drained closes once everything running
// has finished, possibly straight away. Asking again replaces the changes.
func (c *Core) RequestRestart(changes []string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	ev := &store.Event{Type: store.RestartRequested, Changes: changes}
	if err := c.cfg.Store.Append(ev); err != nil {
		return err
	}
	c.apply(ev)
	c.restarting = true
	c.checkDrained()
	return nil
}

// Drained closes when a requested restart has nothing left to wait for.
func (c *Core) Drained() <-chan struct{} {
	return c.drained
}

// checkDrained is called whenever that could have become true. The caller holds mu.
func (c *Core) checkDrained() {
	if !c.restarting || c.closedDrained {
		return
	}
	for _, conv := range c.convs {
		if conv.busy {
			return
		}
	}
	if c.manualRunning() {
		return
	}
	c.closedDrained = true
	close(c.drained)
}

// CurrentStatus says how tack itself is doing.
func (c *Core) CurrentStatus() Status {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := c.status
	out.Restarting = c.restarting
	if c.restarting {
		for _, id := range c.order {
			if conv := c.convs[id]; conv.busy {
				out.Waiting = append(out.Waiting, c.summary(conv))
			}
		}
	}
	return out
}
