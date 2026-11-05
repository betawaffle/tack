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
	ErrNotFound      = errors.New("no such conversation")
	ErrNoMessage     = errors.New("no such system message")
	ErrNoEdit        = errors.New("no such unused edit")
	ErrBusy          = errors.New("a reply is already in progress")
	ErrNothingToSend = errors.New("the model has already replied to everything in this conversation")
	ErrAwaiting      = errors.New("tool calls in this conversation are waiting for a decision")
	ErrNoDecision    = errors.New("no such open decision")
	ErrBadChoice     = errors.New("that choice isn't possible for this decision")
)

// Config is what a Core needs.
type Config struct {
	Store   *store.Store
	Client  *openrouter.Client
	Model   string
	Routing openrouter.Routing
	Tools   []Tool // offered to new conversations
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

	tools    store.Ref // the exact definitions it sends, if it has any
	workdir  string    // where its tools' relative paths start
	decision *decision // what's waiting on a person, if anything
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
	calls    []*toolCall // what a usable reply asked for
}

type edit struct {
	id    string
	parts []store.Part
}

// New rebuilds state from the record.
func New(cfg Config) (*Core, error) {
	for _, tool := range cfg.Tools {
		names, err := definedNames(append(append([]byte{'['}, tool.Definition...), ']'))
		if err != nil {
			return nil, fmt.Errorf("tool %s: %w", tool.Name, err)
		}
		if names[0] != tool.Name {
			return nil, fmt.Errorf("tool %s is defined with the name %q", tool.Name, names[0])
		}
	}
	c := &Core{cfg: cfg, convs: map[string]*conversation{}}
	err := cfg.Store.Replay(func(ev *store.Event) error {
		if !store.Known(ev.Type) {
			return fmt.Errorf("unknown event type %q: a newer build wrote this record, so run that build or a later one", ev.Type)
		}
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

// Recover deals with whatever a stop left behind. It can fall anywhere in a tool
// turn, and anything unfinished has to go through a person. It's separate from New
// so that reading the record writes nothing, and a build that can't read it leaves
// no trace.
func (c *Core) Recover() error {
	for _, id := range c.order {
		if _, err := c.ensureDecision(c.convs[id]); err != nil {
			return err
		}
	}
	return nil
}

// record writes an event and applies it. The lock covers both, so live state
// sees events in the order the record has them, and a rebuild can't differ.
func (c *Core) record(ev *store.Event) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.cfg.Store.Append(ev); err != nil {
		return err
	}
	c.apply(ev)
	return nil
}

// apply updates state from an event. The caller holds mu, or is New.
func (c *Core) apply(ev *store.Event) {
	if ev.Type == store.ConversationAdded {
		c.convs[ev.Conversation] = &conversation{id: ev.Conversation, created: ev.Time, tools: ev.Tools, workdir: ev.Workdir}
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
				if usable(ev) {
					for _, call := range ev.ToolCalls {
						it.calls = append(it.calls, &toolCall{ToolCall: call})
					}
				}
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
	default:
		conv.applyTool(ev)
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
	return ev != nil && ev.Error == "" && (ev.Text != "" || len(ev.ToolCalls) > 0)
}

// context returns what the next call sends. The caller holds mu. Unused edits
// aren't part of it, and neither are failed calls. A tool call with no result yet
// is left unanswered here, and call refuses to send that.
func (conv *conversation) context() []store.Entry {
	var out []store.Entry
	for _, it := range conv.items {
		switch {
		case it.role != "":
			if len(it.parts) > 0 {
				out = append(out, store.Entry{Role: it.role, Parts: it.parts})
			}
		case usable(it.finished):
			entry := store.Entry{Role: "assistant"}
			if it.finished.Text != "" {
				entry.Parts = []store.Part{{Block: it.finished.Text}}
			}
			if len(it.calls) > 0 {
				entry.ToolCalls = it.finished.ToolCalls
				entry.Reasoning = it.finished.Reasoning
			}
			out = append(out, entry)
			for _, call := range it.calls {
				if call.resolved() {
					out = append(out, store.Entry{Role: "tool", ToolCall: call.ID, Parts: call.result})
				}
			}
		}
	}
	return out
}

// canReply reports whether there's anything for the model to respond to. The
// caller holds mu.
func (conv *conversation) canReply() bool {
	if conv.decision != nil {
		return false
	}
	if conv.unanswered() {
		return true // the rest of an interrupted tool turn
	}
	entries := conv.context()
	return len(entries) > 0 && entries[len(entries)-1].Role != "assistant"
}

// Options describe a new conversation.
type Options struct {
	// System is the system prompt. Empty means the conversation starts without one.
	System string
	// Blocks, if they join to exactly System, are stored as separate blocks.
	Blocks []string
	// Tools says whether to offer the configured tools.
	Tools   bool
	Workdir string
}

// NewConversation records a new conversation offering every configured tool.
func (c *Core) NewConversation(systemPrompt string) (string, error) {
	return c.NewConversationWith(Options{System: systemPrompt, Tools: true})
}

// NewConversationWith records a new conversation.
func (c *Core) NewConversationWith(opts Options) (string, error) {
	var tools store.Ref
	if opts.Tools {
		var err error
		if tools, err = c.storeDefinitions(); err != nil {
			return "", err
		}
	}
	id := store.NewID()
	event := &store.Event{Type: store.ConversationAdded, Conversation: id, Tools: tools}
	if tools != "" {
		event.Workdir = opts.Workdir
	}
	if err := c.record(event); err != nil {
		return "", err
	}
	if strings.TrimSpace(opts.System) == "" {
		return id, nil
	}
	var parts []store.Part
	if strings.Join(opts.Blocks, "") == opts.System {
		for _, block := range opts.Blocks {
			piece, err := c.parts(block)
			if err != nil {
				return "", err
			}
			parts = append(parts, piece...)
		}
	} else {
		var err error
		if parts, err = c.parts(opts.System); err != nil {
			return "", err
		}
	}
	return id, c.record(&store.Event{Type: store.SystemMessage, Conversation: id, Message: store.NewID(), Parts: parts})
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

// Update is a step in a reply. The kinds are "tool", "reasoning" and "content"
// while it streams, then one "done" or "error" at the end.
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
	conv, err := c.reserve(convID, func(conv *conversation) error {
		if conv.decision != nil || conv.unanswered() {
			return ErrAwaiting
		}
		return nil
	})
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
		if conv.decision != nil {
			return ErrAwaiting
		}
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

// start moves a reserved conversation along. The channel closes once everything
// has been recorded.
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
		if err := c.advance(conv, send); err != nil {
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
	unanswered := conv.unanswered()
	tools := conv.tools
	workdir := conv.workdir
	c.mu.Unlock()
	if unanswered {
		// Hosts may accept this silently, but the model would be told nothing.
		return errors.New("refusing to send a tool call with no result")
	}

	messages, err := c.renderMessages(entries)
	if err != nil {
		return err
	}
	var definitions []byte
	if tools != "" {
		if definitions, err = c.cfg.Store.Get(tools); err != nil {
			return err
		}
	}
	body, err := openrouter.Render(openrouter.Request{
		Model:     c.cfg.Model,
		Routing:   c.cfg.Routing,
		Messages:  messages,
		SessionID: conv.id,
		Tools:     definitions,
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
	if finished.Error == "" {
		finished.Error = malformedCalls(res)
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
	if finished.Error == "" {
		if finished.ToolCalls, err = c.storeCalls(definitions, workdir, res.ToolCalls); err != nil {
			return err
		}
	}
	if err := c.record(finished); err != nil {
		return err
	}
	if finished.Error != "" {
		return errors.New(finished.Error)
	}
	if finished.Text == "" && len(finished.ToolCalls) == 0 {
		return fmt.Errorf("the model returned no text (finish reason %q)", finished.FinishReason)
	}
	return nil
}

func (c *Core) renderMessages(entries []store.Entry) ([]openrouter.Message, error) {
	out := make([]openrouter.Message, 0, len(entries))
	for _, e := range entries {
		content, err := c.joined(e.Parts)
		if err != nil {
			return nil, err
		}
		message := openrouter.Message{Role: e.Role, Content: content, ToolCallID: e.ToolCall}
		if e.Reasoning != "" {
			reasoning, err := c.cfg.Store.Get(e.Reasoning)
			if err != nil {
				return nil, err
			}
			message.Reasoning = string(reasoning)
		}
		for _, call := range e.ToolCalls {
			arguments, err := c.cfg.Store.Get(call.Arguments)
			if err != nil {
				return nil, err
			}
			message.ToolCalls = append(message.ToolCalls, openrouter.ToolCall{
				ID:       call.ID,
				Type:     "function",
				Function: openrouter.FunctionCall{Name: call.Name, Arguments: string(arguments)},
			})
		}
		out = append(out, message)
	}
	return out, nil
}

func (c *Core) joined(parts []store.Part) (string, error) {
	var content strings.Builder
	for _, p := range parts {
		data, err := c.cfg.Store.Get(p.Block)
		if err != nil {
			return "", err
		}
		content.Write(data)
	}
	return content.String(), nil
}
