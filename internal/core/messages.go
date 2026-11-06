package core

import (
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/betawaffle/tack/internal/store"
)

// ErrBadMessage says a message is one the API could never accept.
var ErrBadMessage = errors.New("that message can't be sent")

// Message is a message someone wrote, of any role. Each field is a field of the API's
// message. Nothing here is ever worked out from the text.
type Message struct {
	Role       string        `json:"role"`
	Text       string        `json:"text"`
	Name       string        `json:"name"`
	Reasoning  string        `json:"reasoning"`
	ToolCalls  []MessageCall `json:"tool_calls"`
	ToolCallID string        `json:"tool_call_id"`
}

// MessageCall is a tool call someone wrote into an assistant message. tack never runs
// these. They're history for the model to read, and someone answers them by hand.
type MessageCall struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

// written says a message is one that only exists because someone wrote it this way, as
// opposed to the user and system messages tack has always had.
func (it *item) written() bool {
	return it.role != "" && it.role != "user" && it.role != "system"
}

// check refuses what the API could never accept. Whether a particular model will is
// something only it can say.
func (m *Message) check() error {
	bad := func(format string, args ...any) error {
		return fmt.Errorf("%w: %s", ErrBadMessage, fmt.Sprintf(format, args...))
	}
	if m.Role == "" {
		m.Role = "user"
	}
	blank := strings.TrimSpace(m.Text) == ""
	switch m.Role {
	case "assistant":
		if blank && len(m.ToolCalls) == 0 {
			return bad("an assistant message needs text or a tool call")
		}
		seen := map[string]bool{}
		for _, call := range m.ToolCalls {
			switch {
			case call.ID == "":
				return bad("a tool call needs an ID")
			case seen[call.ID]:
				return bad("two tool calls have the ID %q", call.ID)
			case call.Name == "":
				return bad("tool call %q needs a tool name", call.ID)
			}
			seen[call.ID] = true
			if _, err := parseArguments([]byte(call.Arguments)); err != nil {
				return bad("the arguments of tool call %q have to be a JSON object: %v", call.ID, err)
			}
		}
	case "tool":
		if m.ToolCallID == "" {
			return bad("a tool message has to say which tool call it answers")
		}
		if blank {
			return bad("a tool message needs text")
		}
	default:
		if blank {
			return bad("empty message")
		}
	}
	if m.Role != "assistant" && (m.Reasoning != "" || len(m.ToolCalls) > 0) {
		return bad("only an assistant message can have reasoning or tool calls")
	}
	if m.Role != "tool" && m.ToolCallID != "" {
		return bad("only a tool message can answer a tool call")
	}
	return nil
}

// openWritten lists the tool calls at the end of what's sent that nothing has answered
// yet, when those are written ones. Only a tool message can come next.
func (conv *conversation) openWritten() []string {
	var open []string
	for _, it := range conv.items {
		switch {
		case it.role == "tool":
			if i := slices.Index(open, it.toolCallID); i >= 0 {
				open = slices.Delete(open, i, i+1)
			}
		case it.role == "assistant" && len(it.writtenCalls) > 0:
			open = nil
			for _, call := range it.writtenCalls {
				open = append(open, call.ID)
			}
		case it.role == "" && it.finished == nil:
			// A call that's still going, or was cut off, doesn't change anything.
		case len(it.parts) > 0 || usable(it.finished):
			open = nil
		}
	}
	return open
}

// checkFits refuses a message that can't come next. The caller holds mu.
func (c *Core) checkFits(conv *conversation, m Message) error {
	open := conv.openWritten()
	switch {
	case m.Role == "tool":
		if !slices.Contains(open, m.ToolCallID) {
			return fmt.Errorf("%w: no tool call %q is waiting for an answer", ErrBadMessage, m.ToolCallID)
		}
	case len(open) > 0:
		return fmt.Errorf("%w: tool calls %s need answering first", ErrBadMessage, strings.Join(open, ", "))
	}
	// A real call that's waiting has to go through its decision. Only a message for the
	// person's own next turn can wait alongside it.
	if conv.unanswered() && m.Role != "user" && m.Role != "system" {
		return ErrAwaiting
	}
	return nil
}

// checkComplete refuses to call the model with tool calls unanswered. The caller holds mu.
func (conv *conversation) checkComplete(adding *Message) error {
	open := conv.openWritten()
	if adding != nil && adding.Role == "tool" {
		open = slices.DeleteFunc(slices.Clone(open), func(id string) bool { return id == adding.ToolCallID })
	}
	if adding != nil && adding.Role == "assistant" {
		open = nil
		for _, call := range adding.ToolCalls {
			open = append(open, call.ID)
		}
	}
	if len(open) > 0 {
		return fmt.Errorf("%w: tool calls %s need answering first. Hold this to answer the rest", ErrBadMessage, strings.Join(open, ", "))
	}
	return nil
}

// messageEvent stores a message's content and makes the event that adds it.
func (c *Core) messageEvent(convID string, m Message) (*store.Event, error) {
	ev := &store.Event{Conversation: convID, Name: m.Name}
	var err error
	if strings.TrimSpace(m.Text) != "" {
		if ev.Parts, err = c.parts(m.Text); err != nil {
			return nil, err
		}
	}
	switch m.Role {
	case "user":
		ev.Type = store.UserMessage
		return ev, nil
	case "system":
		ev.Type = store.SystemMessage
		ev.Message = store.NewID()
		return ev, nil
	}
	ev.Type, ev.Role, ev.Message, ev.ToolCall = store.MessageAdded, m.Role, store.NewID(), m.ToolCallID
	if ev.Reasoning, ev.ToolCalls, err = c.storeWritten(m.Reasoning, m.ToolCalls); err != nil {
		return nil, err
	}
	return ev, nil
}

func (c *Core) storeWritten(reasoning string, calls []MessageCall) (store.Ref, []store.ToolCall, error) {
	var ref store.Ref
	var err error
	if reasoning != "" {
		if ref, err = c.cfg.Store.Put([]byte(reasoning)); err != nil {
			return "", nil, err
		}
	}
	var out []store.ToolCall
	for _, call := range calls {
		arguments, err := c.cfg.Store.Put([]byte(call.Arguments))
		if err != nil {
			return "", nil, err
		}
		out = append(out, store.ToolCall{ID: call.ID, Name: call.Name, Arguments: arguments})
	}
	return ref, out, nil
}

// AddMessage adds a message without calling the model. It goes with the next call,
// whichever call that is, in the order it was added.
func (c *Core) AddMessage(convID string, m Message) error {
	if err := m.check(); err != nil {
		return err
	}
	c.mu.Lock()
	conv := c.convs[convID]
	if conv == nil {
		c.mu.Unlock()
		return ErrNotFound
	}
	err := c.checkFits(conv, m)
	c.mu.Unlock()
	if err != nil {
		return err
	}
	ev, err := c.messageEvent(convID, m)
	if err != nil {
		return err
	}
	return c.record(ev)
}

// SendMessage adds a message and asks the model to reply. Any messages waiting in the
// conversation go with it. The call runs to completion even if nobody reads the updates,
// so closing the browser loses nothing.
func (c *Core) SendMessage(convID string, m Message) (<-chan Update, error) {
	if err := m.check(); err != nil {
		return nil, err
	}
	conv, err := c.reserve(convID, func(conv *conversation) error {
		if conv.decision != nil || conv.unanswered() {
			return ErrAwaiting
		}
		if err := c.checkFits(conv, m); err != nil {
			return err
		}
		return conv.checkComplete(&m)
	})
	if err != nil {
		return nil, err
	}
	ev, err := c.messageEvent(convID, m)
	if err == nil {
		err = c.record(ev)
	}
	if err != nil {
		c.release(conv)
		return nil, err
	}
	return c.start(conv), nil
}

// Send sends a plain user message.
func (c *Core) Send(convID, text string) (<-chan Update, error) {
	return c.SendMessage(convID, Message{Role: "user", Text: text})
}

// HoldMessage holds a plain user message.
func (c *Core) HoldMessage(convID, text string) error {
	return c.AddMessage(convID, Message{Role: "user", Text: text})
}

// MessageEdit is a change to a message that's already there. It names one of Message,
// the ID of a written message; Call, a reply from the model; or ToolCall, the result the
// model was given. Each field is what it should become.
type MessageEdit struct {
	Message   string        `json:"message"`
	Call      string        `json:"call"`
	ToolCall  string        `json:"tool_call"`
	Text      string        `json:"text"`
	Name      string        `json:"name"`
	Reasoning string        `json:"reasoning"`
	ToolCalls []MessageCall `json:"tool_calls"`
}

// ProposeMessageEdit holds a change to a message. Nothing changes until it's applied, and
// the record keeps what the message originally was.
func (c *Core) ProposeMessageEdit(convID string, e MessageEdit) (string, error) {
	c.mu.Lock()
	conv := c.convs[convID]
	if conv == nil {
		c.mu.Unlock()
		return "", ErrNotFound
	}
	err := conv.checkEdit(e)
	c.mu.Unlock()
	if err != nil {
		return "", err
	}
	ev := &store.Event{
		Type: store.MessageEditProposed, Conversation: convID, Edit: store.NewID(),
		Message: e.Message, Call: e.Call, ToolCall: e.ToolCall, Name: e.Name,
	}
	if strings.TrimSpace(e.Text) != "" {
		if ev.Parts, err = c.parts(e.Text); err != nil {
			return "", err
		}
	}
	if ev.Reasoning, ev.ToolCalls, err = c.storeWritten(e.Reasoning, e.ToolCalls); err != nil {
		return "", err
	}
	return ev.Edit, c.record(ev)
}

func (conv *conversation) checkEdit(e MessageEdit) error {
	blank := strings.TrimSpace(e.Text) == ""
	switch {
	case e.ToolCall != "":
		call := conv.tool(e.ToolCall)
		if call == nil {
			return ErrNoMessage
		}
		if !call.resolved() {
			return fmt.Errorf("%w: this call hasn't given the model a result yet", ErrBadMessage)
		}
		if blank {
			return fmt.Errorf("%w: a tool message needs text", ErrBadMessage)
		}
	case e.Call != "":
		it := conv.callItem(e.Call)
		if it == nil || !usable(it.finished) {
			return ErrNoMessage
		}
		if len(e.ToolCalls) > 0 {
			return fmt.Errorf("%w: the tool calls of a reply from the model can't be changed", ErrBadMessage)
		}
	default:
		it := conv.writtenMessage(e.Message)
		if it == nil {
			return ErrNoMessage
		}
		m := Message{Role: it.role, Text: e.Text, Name: e.Name, Reasoning: e.Reasoning, ToolCalls: e.ToolCalls, ToolCallID: it.toolCallID}
		// An assistant or custom message can be emptied, which stops it being sent.
		if it.role == "assistant" && blank && len(e.ToolCalls) == 0 {
			return nil
		}
		if it.role != "assistant" && it.role != "tool" && blank {
			return nil
		}
		return m.check()
	}
	return nil
}

// ApplyMessageEdit makes a held change part of the context for later calls.
func (c *Core) ApplyMessageEdit(convID, editID string) error {
	return c.resolveMessageEdit(convID, editID, store.MessageEditApplied)
}

// DiscardMessageEdit drops a held change.
func (c *Core) DiscardMessageEdit(convID, editID string) error {
	return c.resolveMessageEdit(convID, editID, store.MessageEditDiscarded)
}

func (c *Core) resolveMessageEdit(convID, editID, eventType string) error {
	conv, err := c.reserve(convID, func(conv *conversation) error {
		if conv.messageEditing(editID) == nil {
			return ErrNoEdit
		}
		return nil
	})
	if err != nil {
		return err
	}
	defer c.release(conv)
	return c.record(&store.Event{Type: eventType, Conversation: convID, Edit: editID})
}

func (conv *conversation) writtenMessage(id string) *item {
	for _, it := range conv.items {
		if it.written() && it.messageID == id {
			return it
		}
	}
	return nil
}

func (conv *conversation) callItem(id string) *item {
	for _, it := range conv.items {
		if it.role == "" && it.callID == id {
			return it
		}
	}
	return nil
}

// messageEditing finds where a held change is. It's an item or a tool call.
func (conv *conversation) messageEditing(id string) *edit {
	for _, it := range conv.items {
		if it.pending != nil && it.pending.id == id && it.role != "system" {
			return it.pending
		}
		for _, call := range it.calls {
			if call.pending != nil && call.pending.id == id {
				return call.pending
			}
		}
	}
	return nil
}

func (conv *conversation) applyMessage(ev *store.Event) {
	switch ev.Type {
	case store.MessageAdded:
		conv.items = append(conv.items, &item{
			role: ev.Role, parts: ev.Parts, messageID: ev.Message, time: ev.Time, name: ev.Name,
			reasoning: ev.Reasoning, writtenCalls: ev.ToolCalls, toolCallID: ev.ToolCall,
		})
	case store.MessageEditProposed:
		pending := &edit{id: ev.Edit, parts: ev.Parts, name: ev.Name, reasoning: ev.Reasoning, calls: ev.ToolCalls}
		switch {
		case ev.ToolCall != "":
			if call := conv.tool(ev.ToolCall); call != nil {
				call.pending = pending
			}
		case ev.Call != "":
			if it := conv.callItem(ev.Call); it != nil {
				it.pending = pending
			}
		default:
			if it := conv.writtenMessage(ev.Message); it != nil {
				it.pending = pending
			}
		}
	case store.MessageEditApplied:
		for _, it := range conv.items {
			if it.pending != nil && it.pending.id == ev.Edit && it.role != "system" {
				change := it.pending
				it.pending = nil
				it.edited = true
				if it.role == "" {
					it.override = change
					continue
				}
				it.parts, it.name, it.reasoning, it.writtenCalls = change.parts, change.name, change.reasoning, change.calls
				it.sent = false
			}
			for _, call := range it.calls {
				if call.pending != nil && call.pending.id == ev.Edit {
					call.result, call.edited, call.pending = call.pending.parts, true, nil
				}
			}
		}
	case store.MessageEditDiscarded:
		for _, it := range conv.items {
			if it.pending != nil && it.pending.id == ev.Edit && it.role != "system" {
				it.pending = nil
			}
			for _, call := range it.calls {
				if call.pending != nil && call.pending.id == ev.Edit {
					call.pending = nil
				}
			}
		}
	}
}
