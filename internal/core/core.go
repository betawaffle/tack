// Package core holds tack's state. All of it is derived from recorded events:
// every change is written to the store first, then applied through the same
// function that rebuilds the state at startup.
package core

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/betawaffle/tack/internal/openrouter"
	"github.com/betawaffle/tack/internal/store"
)

// Errors a caller can act on.
var (
	ErrNotFound = errors.New("no such conversation")
	ErrBusy     = errors.New("a reply is already in progress")
)

// Config is what a Core needs.
type Config struct {
	Store   *store.Store
	Client  *openrouter.Client
	Model   string
	Routing openrouter.Routing
}

// Core runs conversations.
type Core struct {
	cfg Config

	mu    sync.Mutex
	convs map[string]*conversation
	order []string
}

type conversation struct {
	id      string
	created time.Time
	items   []*item
	busy    bool
}

// item is a user message or a model call, in the order they started.
type item struct {
	userParts []store.Part
	time      time.Time
	callID    string
	finished  *store.Event
}

// New rebuilds state from the record.
func New(cfg Config) (*Core, error) {
	c := &Core{cfg: cfg, convs: map[string]*conversation{}}
	err := cfg.Store.Replay(func(ev *store.Event) error {
		c.apply(ev)
		return nil
	}, func(file string, line int) {
		log.Printf("skipping torn event at %s line %d", file, line)
	})
	if err != nil {
		return nil, err
	}
	return c, nil
}

func (c *Core) record(ev *store.Event) error {
	if err := c.cfg.Store.Append(ev); err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.apply(ev)
	return nil
}

// apply updates state from an event. The caller holds mu, or is New. Event
// types it doesn't know are ignored, so old binaries can read newer records.
func (c *Core) apply(ev *store.Event) {
	switch ev.Type {
	case store.ConversationAdded:
		c.convs[ev.Conversation] = &conversation{id: ev.Conversation, created: ev.Time}
		c.order = append(c.order, ev.Conversation)
	case store.UserMessage:
		if conv := c.convs[ev.Conversation]; conv != nil {
			conv.items = append(conv.items, &item{userParts: ev.Parts, time: ev.Time})
		}
	case store.CallStarted:
		if conv := c.convs[ev.Conversation]; conv != nil {
			conv.items = append(conv.items, &item{callID: ev.Call, time: ev.Time})
		}
	case store.CallFinished:
		if conv := c.convs[ev.Conversation]; conv != nil {
			for _, it := range conv.items {
				if it.callID == ev.Call {
					it.finished = ev
				}
			}
		}
	}
}

// usable reports whether a finished call's reply belongs in the next context.
func usable(ev *store.Event) bool {
	return ev != nil && ev.Error == "" && ev.Text != ""
}

// context returns what the next call sends. The caller holds mu.
func (conv *conversation) context() []store.Entry {
	var out []store.Entry
	for _, it := range conv.items {
		switch {
		case it.userParts != nil:
			out = append(out, store.Entry{Role: "user", Parts: it.userParts})
		case usable(it.finished):
			out = append(out, store.Entry{Role: "assistant", Parts: []store.Part{{Block: it.finished.Text}}})
		}
	}
	return out
}

// NewConversation records a new, empty conversation.
func (c *Core) NewConversation() (string, error) {
	id := store.NewID()
	return id, c.record(&store.Event{Type: store.ConversationAdded, Conversation: id})
}

// Update is a step in a reply. The kinds are "reasoning" and "content" while it
// streams, then one "done" or "error" at the end.
type Update struct {
	Kind string `json:"kind"`
	Text string `json:"text,omitzero"`
}

// updateBuffer is how far a slow reader can fall behind before it misses deltas.
// Deltas are only a preview: the record has the whole reply either way.
const updateBuffer = 4096

// Send adds a user message and starts a model call. The call runs to completion
// even if nobody reads the updates, so closing the browser loses nothing. It
// ends the channel when the call has been recorded.
func (c *Core) Send(convID, text string) (<-chan Update, error) {
	c.mu.Lock()
	conv := c.convs[convID]
	if conv == nil {
		c.mu.Unlock()
		return nil, ErrNotFound
	}
	if conv.busy {
		c.mu.Unlock()
		return nil, ErrBusy
	}
	conv.busy = true
	c.mu.Unlock()

	clearBusy := func() {
		c.mu.Lock()
		conv.busy = false
		c.mu.Unlock()
	}

	block, err := c.cfg.Store.Put([]byte(text))
	if err != nil {
		clearBusy()
		return nil, err
	}
	err = c.record(&store.Event{
		Type:         store.UserMessage,
		Conversation: convID,
		Parts:        []store.Part{{Block: block}},
	})
	if err != nil {
		clearBusy()
		return nil, err
	}

	updates := make(chan Update, updateBuffer)
	send := func(u Update) {
		select {
		case updates <- u:
		default:
		}
	}
	go func() {
		defer close(updates)
		defer clearBusy()
		if err := c.call(conv, send); err != nil {
			log.Printf("call in conversation %s: %v", convID, err)
			// Make room so the final update can't be the one that gets dropped.
			select {
			case <-updates:
			default:
			}
			send(Update{Kind: "error", Text: err.Error()})
			return
		}
		select {
		case <-updates:
		default:
		}
		send(Update{Kind: "done"})
	}()
	return updates, nil
}

// call makes one model call from the conversation's current context. The order
// matters: the request and the intent are durable before anything is sent, so
// a crash leaves a started call with no finish, plus whatever had arrived.
func (c *Core) call(conv *conversation, send func(Update)) error {
	c.mu.Lock()
	entries := conv.context()
	c.mu.Unlock()

	messages, err := c.renderMessages(entries)
	if err != nil {
		return err
	}
	body, err := openrouter.Render(openrouter.Request{
		Model:     c.cfg.Model,
		Routing:   c.cfg.Routing,
		Messages:  messages,
		SessionID: conv.id,
	})
	if err != nil {
		return err
	}
	request, err := c.cfg.Store.Put(body)
	if err != nil {
		return err
	}
	routing, err := json.Marshal(c.cfg.Routing, json.Deterministic(true))
	if err != nil {
		return err
	}
	callID := store.NewID()
	err = c.record(&store.Event{
		Type:           store.CallStarted,
		Conversation:   conv.id,
		Call:           callID,
		Context:        entries,
		Model:          c.cfg.Model,
		Routing:        routing,
		URL:            c.cfg.Client.URL,
		Request:        request,
		RequestHeaders: openrouter.Headers(),
	})
	if err != nil {
		return err
	}

	partial, err := c.cfg.Store.NewPartial("call-" + callID)
	if err != nil {
		return err
	}
	res, doErr := c.cfg.Client.Do(context.Background(), body, partial, func(d openrouter.Delta) {
		if d.Reasoning != "" {
			send(Update{Kind: "reasoning", Text: d.Reasoning})
		}
		if d.Content != "" {
			send(Update{Kind: "content", Text: d.Content})
		}
	})

	finished := &store.Event{
		Type:            store.CallFinished,
		Conversation:    conv.id,
		Call:            callID,
		Status:          res.Status,
		ResponseHeaders: res.ResponseHeaders,
		Host:            res.Host,
		GenerationID:    res.GenerationID,
		FinishReason:    res.FinishReason,
		Usage:           res.Usage,
		TTFTMillis:      res.TTFT.Milliseconds(),
		DurationMillis:  res.Duration.Milliseconds(),
		ParseErrors:     res.ParseErrors,
		Error:           res.Error,
	}
	if finished.Error == "" && doErr != nil {
		finished.Error = doErr.Error()
	}
	if partial.Size() == 0 {
		partial.Abort()
	} else if finished.Response, err = partial.Commit(); err != nil {
		return err
	}
	if res.Text != "" {
		if finished.Text, err = c.cfg.Store.Put([]byte(res.Text)); err != nil {
			return err
		}
	}
	if res.Reasoning != "" {
		if finished.Reasoning, err = c.cfg.Store.Put([]byte(res.Reasoning)); err != nil {
			return err
		}
	}
	if err := c.record(finished); err != nil {
		return err
	}
	if finished.Error != "" {
		return errors.New(finished.Error)
	}
	if finished.Text == "" {
		return fmt.Errorf("the model returned no text (finish reason %q)", finished.FinishReason)
	}
	return nil
}

func (c *Core) renderMessages(entries []store.Entry) ([]openrouter.Message, error) {
	out := make([]openrouter.Message, 0, len(entries))
	for _, e := range entries {
		var content strings.Builder
		for _, p := range e.Parts {
			data, err := c.cfg.Store.Get(p.Block)
			if err != nil {
				return nil, err
			}
			content.Write(data)
		}
		out = append(out, openrouter.Message{Role: e.Role, Content: content.String()})
	}
	return out, nil
}
