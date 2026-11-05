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

// DefaultSystemPrompt starts new conversations unless it's overridden.
const DefaultSystemPrompt = "You have no tools and can't access files, commands or the internet, so never imply you did something you can't do. " +
	"Be direct and concise. Say when you're unsure, and don't invent facts, citations or results."

// Errors a caller can act on.
var (
	ErrNotFound      = errors.New("no such conversation")
	ErrNoMessage     = errors.New("no such system message")
	ErrNoEdit        = errors.New("no such unused edit")
	ErrBusy          = errors.New("a reply is already in progress")
	ErrNothingToSend = errors.New("the model has already replied to everything in this conversation")
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

// item is a message or a model call, in the order they were recorded. Its role
// is "user" or "system" for a message and empty for a call.
type item struct {
	role string
	time time.Time

	// Messages. Parts are empty if an edit removed the message.
	parts []store.Part

	// System messages.
	messageID string
	sent      bool  // a call has included the current parts
	edited    bool  // an applied edit changed the parts
	pending   *edit // held until it's applied or discarded

	// Calls.
	callID   string
	finished *store.Event
}

type edit struct {
	id    string
	parts []store.Part
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
	if ev.Type == store.ConversationAdded {
		c.convs[ev.Conversation] = &conversation{id: ev.Conversation, created: ev.Time}
		c.order = append(c.order, ev.Conversation)
		return
	}
	conv := c.convs[ev.Conversation]
	if conv == nil {
		return
	}
	switch ev.Type {
	case store.UserMessage:
		conv.items = append(conv.items, &item{role: "user", parts: ev.Parts, time: ev.Time})
	case store.SystemMessage:
		conv.items = append(conv.items, &item{role: "system", parts: ev.Parts, messageID: ev.Message, time: ev.Time})
	case store.CallStarted:
		for _, it := range conv.items {
			it.sent = true
		}
		conv.items = append(conv.items, &item{callID: ev.Call, time: ev.Time})
	case store.CallFinished:
		for _, it := range conv.items {
			if it.callID == ev.Call {
				it.finished = ev
			}
		}
	case store.EditProposed:
		// A newer edit replaces an older unused one. The record keeps both.
		if it := conv.message(ev.Message); it != nil {
			it.pending = &edit{id: ev.Edit, parts: ev.Parts}
		}
	case store.EditApplied:
		if it := conv.editing(ev.Edit); it != nil {
			it.parts = it.pending.parts
			it.pending = nil
			it.edited = true
			it.sent = false
		}
	case store.EditDiscarded:
		if it := conv.editing(ev.Edit); it != nil {
			it.pending = nil
		}
	}
}

func (conv *conversation) message(id string) *item {
	for _, it := range conv.items {
		if it.role == "system" && it.messageID == id {
			return it
		}
	}
	return nil
}

func (conv *conversation) editing(editID string) *item {
	for _, it := range conv.items {
		if it.pending != nil && it.pending.id == editID {
			return it
		}
	}
	return nil
}

// usable reports whether a finished call's reply belongs in the next context.
func usable(ev *store.Event) bool {
	return ev != nil && ev.Error == "" && ev.Text != ""
}

// context returns what the next call sends. The caller holds mu. Unused edits
// aren't part of it, and neither are failed calls.
func (conv *conversation) context() []store.Entry {
	var out []store.Entry
	for _, it := range conv.items {
		switch {
		case it.role != "":
			if len(it.parts) > 0 {
				out = append(out, store.Entry{Role: it.role, Parts: it.parts})
			}
		case usable(it.finished):
			out = append(out, store.Entry{Role: "assistant", Parts: []store.Part{{Block: it.finished.Text}}})
		}
	}
	return out
}

// canReply reports whether there's anything for the model to respond to. The
// caller holds mu.
func (conv *conversation) canReply() bool {
	entries := conv.context()
	return len(entries) > 0 && entries[len(entries)-1].Role != "assistant"
}

// NewConversation records a new conversation. An empty system prompt means the
// conversation starts without one.
func (c *Core) NewConversation(systemPrompt string) (string, error) {
	id := store.NewID()
	if err := c.record(&store.Event{Type: store.ConversationAdded, Conversation: id}); err != nil {
		return "", err
	}
	if strings.TrimSpace(systemPrompt) == "" {
		return id, nil
	}
	_, err := c.AddSystemMessage(id, systemPrompt)
	return id, err
}

func (c *Core) parts(text string) ([]store.Part, error) {
	block, err := c.cfg.Store.Put([]byte(text))
	if err != nil {
		return nil, err
	}
	return []store.Part{{Block: block}}, nil
}

func (c *Core) exists(convID string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.convs[convID] == nil {
		return ErrNotFound
	}
	return nil
}

// AddSystemMessage adds a system message at the end of the conversation. It
// goes to the model with the next call, whichever call that is.
func (c *Core) AddSystemMessage(convID, text string) (string, error) {
	if err := c.exists(convID); err != nil {
		return "", err
	}
	parts, err := c.parts(text)
	if err != nil {
		return "", err
	}
	id := store.NewID()
	return id, c.record(&store.Event{Type: store.SystemMessage, Conversation: convID, Message: id, Parts: parts})
}

// ProposeEdit holds new text for a system message. Nothing changes until the
// edit is applied. Empty text proposes removing the message.
func (c *Core) ProposeEdit(convID, messageID, text string) (string, error) {
	c.mu.Lock()
	conv := c.convs[convID]
	if conv == nil {
		c.mu.Unlock()
		return "", ErrNotFound
	}
	found := conv.message(messageID) != nil
	c.mu.Unlock()
	if !found {
		return "", ErrNoMessage
	}
	var parts []store.Part
	if strings.TrimSpace(text) != "" {
		var err error
		if parts, err = c.parts(text); err != nil {
			return "", err
		}
	}
	id := store.NewID()
	return id, c.record(&store.Event{Type: store.EditProposed, Conversation: convID, Message: messageID, Edit: id, Parts: parts})
}

// ApplyEdit makes a held edit part of the context for later calls.
func (c *Core) ApplyEdit(convID, editID string) error {
	return c.resolveEdit(convID, editID, store.EditApplied)
}

// DiscardEdit drops a held edit.
func (c *Core) DiscardEdit(convID, editID string) error {
	return c.resolveEdit(convID, editID, store.EditDiscarded)
}

func (c *Core) resolveEdit(convID, editID, eventType string) error {
	c.mu.Lock()
	conv := c.convs[convID]
	if conv == nil {
		c.mu.Unlock()
		return ErrNotFound
	}
	found := conv.editing(editID) != nil
	c.mu.Unlock()
	if !found {
		return ErrNoEdit
	}
	return c.record(&store.Event{Type: eventType, Conversation: convID, Edit: editID})
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

// reserve claims a conversation for one call at a time. If check is given, it
// runs with mu held and can refuse.
func (c *Core) reserve(convID string, check func(*conversation) error) (*conversation, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	conv := c.convs[convID]
	if conv == nil {
		return nil, ErrNotFound
	}
	if conv.busy {
		return nil, ErrBusy
	}
	if check != nil {
		if err := check(conv); err != nil {
			return nil, err
		}
	}
	conv.busy = true
	return conv, nil
}

func (c *Core) release(conv *conversation) {
	c.mu.Lock()
	conv.busy = false
	c.mu.Unlock()
}

// Send adds a user message and asks the model to reply. Any system messages
// waiting in the conversation go with it. The call runs to completion even if
// nobody reads the updates, so closing the browser loses nothing.
func (c *Core) Send(convID, text string) (<-chan Update, error) {
	conv, err := c.reserve(convID, nil)
	if err != nil {
		return nil, err
	}
	parts, err := c.parts(text)
	if err != nil {
		c.release(conv)
		return nil, err
	}
	err = c.record(&store.Event{Type: store.UserMessage, Conversation: convID, Parts: parts})
	if err != nil {
		c.release(conv)
		return nil, err
	}
	return c.start(conv), nil
}

// Reply asks the model to respond to the conversation as it stands, without a
// new user message. That retries a call that failed, and it lets the model speak
// out of turn after a system message.
func (c *Core) Reply(convID string) (<-chan Update, error) {
	conv, err := c.reserve(convID, func(conv *conversation) error {
		if !conv.canReply() {
			return ErrNothingToSend
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return c.start(conv), nil
}

// start runs a call for a reserved conversation. The channel closes once the
// call has been recorded.
func (c *Core) start(conv *conversation) <-chan Update {
	updates := make(chan Update, updateBuffer)
	send := func(u Update) {
		select {
		case updates <- u:
		default:
		}
	}
	final := func(u Update) {
		// Make room so the last update can't be the one that gets dropped.
		select {
		case <-updates:
		default:
		}
		send(u)
	}
	go func() {
		defer close(updates)
		defer c.release(conv)
		if err := c.call(conv, send); err != nil {
			log.Printf("call in conversation %s: %v", conv.id, err)
			final(Update{Kind: "error", Text: err.Error()})
			return
		}
		final(Update{Kind: "done"})
	}()
	return updates
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
