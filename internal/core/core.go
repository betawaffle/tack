// Package core holds tack's state. All of it is derived from recorded events:
// every change is written to the store first, then applied through the same
// function that rebuilds the state at startup.
package core

import (
	"context"
	"encoding/json/jsontext"
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
	ErrNotFound       = errors.New("no such conversation")
	ErrNoMessage      = errors.New("no such system message")
	ErrNoEdit         = errors.New("no such unused edit")
	ErrBusy           = errors.New("a reply is already in progress")
	ErrNothingToSend  = errors.New("the model has already replied to everything in this conversation")
	ErrAwaiting       = errors.New("tool calls in this conversation are waiting for a decision")
	ErrNoDecision     = errors.New("no such open decision")
	ErrBadChoice      = errors.New("that choice isn't possible for this decision")
	ErrBadSettings    = errors.New("those settings can't work")
	ErrBadTools       = errors.New("those tools can't work")
	ErrBadFork        = errors.New("that isn't a place to fork from")
	ErrNothingRunning = errors.New("that isn't running")
)

// Config is what a Core needs.
type Config struct {
	Store   *store.Store
	Client  *openrouter.Client
	Model   string
	Routing openrouter.Routing
	Tools   []Tool // offered to new conversations
	// Settings are what new conversations start with. Model and Routing fill in for
	// a Settings without them.
	Settings store.Settings
}

// Core runs conversations.
type Core struct {
	cfg Config

	mu       sync.Mutex
	settings store.Settings
	convs    map[string]*conversation
	order    []string

	// The last start and any restart asked for since, derived from the record.
	lastVersion string
	lastStarted time.Time
	nextChanges []string
	// What models have shown they accept or don't, also from the record.
	capabilities map[capabilityKey]capability
	// This run, which isn't.
	status        Status
	restarting    bool
	closedDrained bool
	drained       chan struct{}
}

type conversation struct {
	id      string
	created time.Time
	items   []*item
	busy    bool
	// revision counts the events that have changed it, so a page can say which state of
	// it an action was based on.
	revision int

	tools    store.Ref       // the exact definitions it sends, if it has any
	workdir  string          // where its tools' relative paths start
	settings *store.Settings // what it uses. Older conversations have none
	// lastModel and lastHost stand in for older conversations, from their last call.
	lastModel, lastHost string
	pendingSettings     *pendingSettings

	// Spending, derived from the record.
	spent, budgetAdded Pico
	spentByPeriod      map[string]Pico
	recentCosts        []Pico
	lastRequestBytes   int
	budgetStopped      bool
	stoppedEstimate    Pico
	// The allowance from the last time a person set things going, which applies from
	// allowanceBase on. Older conversations have none until then, and use budgetAdded.
	allowanceSet             bool
	allowanceBase, allowance Pico
	// freshAllowance says no call has used it yet. That call goes whatever it's expected
	// to cost, because someone just asked for it.
	freshAllowance bool
	// chainStarts is how many items there were when each allowance started.
	chainStarts []int

	paused bool // don't call the model until a message or Continue
	notes  []*noteItem
	// noteHistory is every note in order, with how many items there were at the time,
	// so that a fork can keep only those from before its point.
	noteHistory []noteAt

	forkedFrom string
	forkItems  int
	session    string // the session ID it shares with what it forked from, if it did

	// What's running now, so it can be stopped. These aren't derived from the record,
	// because nothing survives a restart to be stopped.
	cancelReply func(error)
	stopTools   map[string]func(error)
	decision    *decision // what's waiting on a person, if anything
}

// item is a message or a model call, in the order they were recorded. Its role
// is "user" or "system" for a message and empty for a call.
type item struct {
	role string
	time time.Time

	// Messages. Parts are empty if an edit removed the message.
	parts []store.Part
	name  string

	// Written messages of other roles.
	reasoning    store.Ref
	writtenCalls []store.ToolCall
	toolCallID   string

	// System messages.
	messageID string
	sent      bool  // a call has included the current parts
	edited    bool  // an applied edit changed the parts
	pending   *edit // held until it's applied or discarded

	// Calls.
	callID       string
	model, host  string
	features     []string // the unusual things in its request
	override     *edit    // what someone changed its reply to
	requestBytes int
	cost         Pico // what it counted as
	finished     *store.Event
	calls        []*toolCall // what a usable reply asked for
}

type edit struct {
	id    string
	parts []store.Part

	name      string
	reasoning store.Ref
	calls     []store.ToolCall
}

// New rebuilds state from the record.
func New(cfg Config) (*Core, error) {
	settings := cfg.Settings
	if settings.Model == "" {
		settings.Model = cfg.Model
	}
	if settings.Host == "" && len(cfg.Routing.Order) > 0 {
		settings.Host = cfg.Routing.Order[0]
	}
	for _, tool := range cfg.Tools {
		names, err := definedNames(append(append([]byte{'['}, tool.define(settings)...), ']'))
		if err != nil {
			return nil, fmt.Errorf("tool %s: %w", tool.Name, err)
		}
		if names[0] != tool.Name {
			return nil, fmt.Errorf("tool %s is defined with the name %q", tool.Name, names[0])
		}
	}
	c := &Core{cfg: cfg, convs: map[string]*conversation{}, settings: settings, drained: make(chan struct{}), capabilities: map[capabilityKey]capability{}}
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
	switch ev.Type {
	case store.TackStarted:
		c.lastVersion, c.lastStarted, c.nextChanges = ev.Version, ev.Time, nil
		return
	case store.RestartRequested:
		c.nextChanges = ev.Changes
		return
	case store.CapabilityForgotten:
		delete(c.capabilities, capabilityKey{ev.Model, ev.Host, ev.Feature})
		return
	}
	if ev.Type == store.ConversationAdded {
		c.convs[ev.Conversation] = &conversation{id: ev.Conversation, created: ev.Time, tools: ev.Tools, workdir: ev.Workdir, settings: ev.Settings}
		c.order = append(c.order, ev.Conversation)
		return
	}
	if ev.Type == store.ConversationForked {
		// Events are in order, so the parent is exactly as it was when this happened.
		if parent := c.convs[ev.ForkedFrom]; parent != nil {
			c.convs[ev.Conversation] = parent.fork(ev)
			c.order = append(c.order, ev.Conversation)
		}
		return
	}
	conv := c.convs[ev.Conversation]
	if conv == nil {
		return
	}
	conv.revision++
	switch ev.Type {
	case store.UserMessage:
		conv.paused = false
		conv.items = append(conv.items, &item{role: "user", parts: ev.Parts, time: ev.Time, name: ev.Name})
	case store.SystemMessage:
		conv.items = append(conv.items, &item{role: "system", parts: ev.Parts, messageID: ev.Message, time: ev.Time, name: ev.Name})
	case store.MessageAdded, store.MessageEditProposed, store.MessageEditApplied, store.MessageEditDiscarded:
		conv.applyMessage(ev)
	case store.CallStarted:
		for _, it := range conv.items {
			it.sent = true
		}
		conv.lastModel = ev.Model
		conv.lastRequestBytes = ev.RequestBytes
		conv.budgetStopped = false
		conv.freshAllowance = false
		host := ""
		var routing openrouter.Routing
		if json.Unmarshal(ev.Routing, &routing) == nil && len(routing.Order) > 0 {
			conv.lastHost = routing.Order[0]
			host = routing.Order[0]
		}
		conv.items = append(conv.items, &item{callID: ev.Call, requestBytes: ev.RequestBytes, time: ev.Time, model: ev.Model, host: host, features: ev.Features})
	case store.CallFinished:
		for _, it := range conv.items {
			if it.callID == ev.Call {
				it.finished = ev
				it.cost = conv.addCost(ev)
				c.learn(it, ev)
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
		if it.role == "system" && it.pending != nil && it.pending.id == editID {
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
			if len(it.parts) > 0 || len(it.writtenCalls) > 0 {
				entry := store.Entry{Role: it.role, Parts: it.parts, Name: it.name, ToolCalls: it.writtenCalls, Reasoning: it.reasoning, ToolCall: it.toolCallID}
				out = append(out, entry)
			}
		case usable(it.finished):
			entry := store.Entry{Role: "assistant"}
			if change := it.override; change != nil {
				// Emptied with nothing else in it, it stops being sent.
				if len(change.parts) == 0 && len(it.calls) == 0 {
					continue
				}
				entry.Parts, entry.Name, entry.Reasoning = change.parts, change.name, change.reasoning
			} else if it.finished.Text != "" {
				entry.Parts = []store.Part{{Block: it.finished.Text}}
			}
			if len(it.calls) > 0 {
				entry.ToolCalls = it.finished.ToolCalls
				if it.override == nil {
					entry.Reasoning = it.finished.Reasoning
				}
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
	if len(conv.openWritten()) > 0 {
		return false
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
	// LeaveOut names tools not to offer.
	LeaveOut []string
	// Definitions replace those of the named tools. Only descriptions can differ.
	Definitions map[string]jsontext.Value
	// Settings replace what the config says a conversation starts with.
	Settings *store.Settings
}

// NewConversation records a new conversation offering every configured tool.
func (c *Core) NewConversation(systemPrompt string) (string, error) {
	return c.NewConversationWith(Options{System: systemPrompt, Tools: true})
}

// NewConversationWith records a new conversation.
func (c *Core) NewConversationWith(opts Options) (string, error) {
	settings := c.currentSettings()
	if opts.Settings != nil {
		settings = opts.Settings.WithDefaults()
		if err := settings.Validate(); err != nil {
			return "", fmt.Errorf("%w: %v", ErrBadSettings, err)
		}
	}
	var tools store.Ref
	if opts.Tools {
		var err error
		if tools, err = c.storeDefinitions(settings, opts); err != nil {
			return "", err
		}
	}
	id := store.NewID()
	event := &store.Event{Type: store.ConversationAdded, Conversation: id, Tools: tools, Settings: &settings}
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

// Fork starts a new conversation with the first items of this one, the same tools
// and its current settings, which then goes its own way. The items it keeps have to
// be settled: a tool call still waiting can't be answered in two places. Nothing is
// undone, so files that later tool calls changed stay changed.
func (c *Core) Fork(convID string, items int) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	conv := c.convs[convID]
	if conv == nil {
		return "", ErrNotFound
	}
	if items < 0 || items > len(conv.items) {
		return "", fmt.Errorf("%w: it has %d items", ErrBadFork, len(conv.items))
	}
	for _, it := range conv.items[:items] {
		if it.role == "" && it.finished == nil && conv.busy {
			return "", ErrBusy
		}
		for _, call := range it.calls {
			if !call.resolved() {
				return "", ErrAwaiting
			}
		}
	}
	settings := conv.effectiveSettings(c.settings)
	ev := &store.Event{Type: store.ConversationForked, Conversation: store.NewID(), ForkedFrom: convID, ForkItems: items, Settings: &settings}
	if err := c.cfg.Store.Append(ev); err != nil {
		return "", err
	}
	c.apply(ev)
	return ev.Conversation, nil
}

// sessionID is what calls tell the host. Forks keep the original's, so that a host
// that routes by session can send them where the cache is.
func (conv *conversation) sessionID() string {
	if conv.session != "" {
		return conv.session
	}
	return conv.id
}

// fork copies what a forked conversation keeps. Its budget starts again, because it
// hasn't spent anything itself, but what those calls cost still informs its estimates.
func (conv *conversation) fork(ev *store.Event) *conversation {
	out := &conversation{
		id: ev.Conversation, created: ev.Time, tools: conv.tools, workdir: conv.workdir, settings: ev.Settings,
		lastModel: conv.lastModel, lastHost: conv.lastHost, forkedFrom: ev.ForkedFrom, forkItems: ev.ForkItems, session: conv.sessionID(),
	}
	for _, it := range conv.items[:ev.ForkItems] {
		kept := *it
		kept.pending = nil
		kept.calls = nil
		for _, call := range it.calls {
			copied := *call
			copied.pending = nil
			kept.calls = append(kept.calls, &copied)
		}
		if it.finished != nil {
			out.addCost(it.finished)
		}
		if it.requestBytes > 0 {
			out.lastRequestBytes = it.requestBytes
		}
		out.items = append(out.items, &kept)
	}
	out.spent, out.spentByPeriod = 0, nil
	for _, start := range conv.chainStarts {
		if start < ev.ForkItems {
			out.chainStarts = append(out.chainStarts, start)
		}
	}
	for _, entry := range conv.noteHistory {
		if entry.items <= ev.ForkItems {
			out.noteHistory = append(out.noteHistory, entry)
			out.addNote(entry.note)
		}
	}
	return out
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

// Update is a step in a reply. The kinds are "call", "tool", "tool_done", "reasoning"
// and "content" while it streams, then one "done" or "error" at the end.
type Update struct {
	Kind string `json:"kind"`
	Text string `json:"text,omitzero"`
	ID   string `json:"id,omitzero"`
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
	if c.restarting {
		return nil, ErrRestarting
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
	c.checkDrained()
	c.mu.Unlock()
}

// Reply asks the model to respond to the conversation as it stands, without a
// new user message. That retries a call that failed, and it lets the model speak
// out of turn after a system message.
func (c *Core) Reply(convID string) (<-chan Update, error) {
	conv, err := c.reserve(convID, func(conv *conversation) error {
		if conv.decision != nil {
			return ErrAwaiting
		}
		if err := conv.checkComplete(nil); err != nil {
			return err
		}
		if !conv.canReply() {
			return ErrNothingToSend
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if err := c.clearPause(conv); err != nil {
		c.release(conv)
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
		err := c.startAllowance(conv)
		if err == nil {
			err = c.advance(conv, send)
		}
		if err != nil {
			log.Printf("call in conversation %s: %v", conv.id, err)
			final(Update{Kind: "error", Text: err.Error()})
			return
		}
		final(Update{Kind: "done"})
	}()
	return updates
}

func routingFor(host string) openrouter.Routing {
	if host == "" {
		return openrouter.Routing{}
	}
	return openrouter.Routing{Order: []string{host}}
}

// call makes one model call from the conversation's current context. The order
// matters: the request and the intent are durable before anything is sent, so
// a crash leaves a started call with no finish, plus whatever had arrived.
func (c *Core) call(conv *conversation, send func(Update)) error {
	c.mu.Lock()
	entries := conv.context()
	features := conv.features(entries)
	unanswered := conv.unanswered()
	tools := conv.tools
	workdir := conv.workdir
	settings := conv.effectiveSettings(c.settings)
	c.mu.Unlock()
	model := settings.Model
	routingChoice := routingFor(settings.Host)
	if unanswered {
		// Hosts may accept this silently, but the model would be told nothing.
		return errors.New("refusing to send a tool call with no result")
	}

	definitions, err := c.definitions(tools)
	if err != nil {
		return err
	}
	body, err := c.renderRequest(conv, entries, definitions, settings)
	if err != nil {
		return err
	}
	// The budget is soft: this only decides whether to start the call.
	c.mu.Lock()
	limit := conv.budgetLimit(settings)
	spent, estimate := conv.spent, conv.estimate(len(body))
	asked := conv.freshAllowance
	c.mu.Unlock()
	if !asked && (spent >= limit || spent+estimate > limit) {
		err := c.record(&store.Event{Type: store.BudgetStopped, Conversation: conv.id, SpentUSD: spent.USD(), EstimateUSD: estimate.USD(), LimitUSD: limit.USD()})
		if err != nil {
			return err
		}
		return errBudgetStop
	}
	send(Update{Kind: "call"})
	request, err := c.cfg.Store.Put(body)
	if err != nil {
		return err
	}
	routing, err := json.Marshal(routingChoice, json.Deterministic(true))
	if err != nil {
		return err
	}
	callID := store.NewID()
	err = c.record(&store.Event{
		Type:           store.CallStarted,
		Conversation:   conv.id,
		Call:           callID,
		Context:        entries,
		Features:       features,
		Model:          model,
		Routing:        routing,
		URL:            c.cfg.Client.URL,
		Request:        request,
		RequestBytes:   len(body),
		RequestHeaders: openrouter.Headers(),
	})
	if err != nil {
		return err
	}

	partial, err := c.cfg.Store.NewPartial("call-" + callID)
	if err != nil {
		return err
	}
	// Closing the connection is how OpenRouter says to cancel. For hosts that support
	// it, that stops processing and billing. Others finish and charge in full.
	ctx, cancel := context.WithCancelCause(context.Background())
	defer cancel(nil)
	c.mu.Lock()
	conv.cancelReply = cancel
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		conv.cancelReply = nil
		c.mu.Unlock()
	}()
	res, doErr := c.cfg.Client.Do(ctx, body, partial, func(d openrouter.Delta) {
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
	if errors.Is(context.Cause(ctx), errReplyCancelled) {
		finished.Cancelled = true
		finished.Error = errReplyCancelled.Error()
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
		if finished.ToolCalls, err = c.storeCalls(definitions, workdir, settings, res.ToolCalls); err != nil {
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

// definitions reads a conversation's tool definitions, if it has any.
func (c *Core) definitions(tools store.Ref) ([]byte, error) {
	if tools == "" {
		return nil, nil
	}
	return c.cfg.Store.Get(tools)
}

// renderRequest makes the bytes a call would send.
func (c *Core) renderRequest(conv *conversation, entries []store.Entry, definitions []byte, settings store.Settings) ([]byte, error) {
	messages, err := c.renderMessages(entries)
	if err != nil {
		return nil, err
	}
	return openrouter.Render(openrouter.Request{
		Model:     settings.Model,
		Routing:   routingFor(settings.Host),
		Messages:  messages,
		SessionID: conv.sessionID(),
		Tools:     definitions,
	})
}

func (c *Core) renderMessages(entries []store.Entry) ([]openrouter.Message, error) {
	out := make([]openrouter.Message, 0, len(entries))
	for _, e := range entries {
		content, err := c.joined(e.Parts)
		if err != nil {
			return nil, err
		}
		message := openrouter.Message{Role: e.Role, Content: content, Name: e.Name, ToolCallID: e.ToolCall}
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
