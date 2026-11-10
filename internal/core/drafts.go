package core

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"

	"github.com/betawaffle/tack/internal/store"
)

// ErrDraftSent is a change to a draft that's already been sent.
var ErrDraftSent = errors.New("this draft has been sent, so it can't change")

// requestOf is a call's request and reply, by conversation/call or manual/call.
func (c *Core) requestOf(ref string) (CallRequest, error) {
	c.mu.Lock()
	src, _ := c.sourceOf(ref)
	c.mu.Unlock()
	if src == nil {
		return CallRequest{}, ErrNotFound
	}
	body, err := c.cfg.Store.Get(src.request)
	if err != nil {
		return CallRequest{}, err
	}
	return CallRequest{Model: src.model, Request: body, Output: src.out}, nil
}

// NewDraft starts a draft of the call after from: its request, the reply as the model's
// message, and an empty result for each tool call in that, or else an empty user message. An
// unsent draft already made from the same call is opened again instead.
func (c *Core) NewDraft(from string) (*store.Draft, error) {
	drafts, err := c.cfg.Store.Drafts()
	if err != nil {
		return nil, err
	}
	for _, d := range drafts {
		if d.From == from && d.Sent == "" {
			return d, nil
		}
	}
	before, err := c.requestOf(from)
	if err != nil {
		return nil, err
	}
	request, err := nextRequest(before)
	if err != nil {
		return nil, err
	}
	d := &store.Draft{ID: store.NewID(), From: from, Model: before.Model, Request: request}
	return d, c.cfg.Store.SaveDraft(d)
}

// nextRequest is the request after before: it with the reply and what answers it added.
func nextRequest(before CallRequest) (jsontext.Value, error) {
	var request map[string]jsontext.Value
	if err := json.Unmarshal(before.Request, &request); err != nil {
		return nil, fmt.Errorf("reading the request: %w", err)
	}
	var messages []jsontext.Value
	if raw, ok := request["messages"]; ok {
		if err := json.Unmarshal(raw, &messages); err != nil {
			return nil, fmt.Errorf("reading the messages: %w", err)
		}
	}
	type function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	}
	type toolCall struct {
		ID       string   `json:"id"`
		Type     string   `json:"type"`
		Function function `json:"function"`
	}
	type message struct {
		Role       string     `json:"role"`
		Content    string     `json:"content"`
		Reasoning  string     `json:"reasoning,omitzero"`
		ToolCalls  []toolCall `json:"tool_calls,omitzero"`
		ToolCallID string     `json:"tool_call_id,omitzero"`
	}
	reply := message{Role: "assistant"}
	if out := before.Output; out != nil {
		reply.Content, reply.Reasoning = out.Content, out.Reasoning
		for i, call := range out.Calls {
			reply.ToolCalls = append(reply.ToolCalls, toolCall{ID: fmt.Sprintf("draft-%d", i), Type: "function", Function: function{Name: call.Name, Arguments: call.Arguments}})
		}
	}
	added := []message{reply}
	// The next call answers the tool calls, or else carries a new user message.
	for _, call := range reply.ToolCalls {
		added = append(added, message{Role: "tool", ToolCallID: call.ID})
	}
	if len(reply.ToolCalls) == 0 {
		added = append(added, message{Role: "user"})
	}
	for _, m := range added {
		raw, err := json.Marshal(m)
		if err != nil {
			return nil, err
		}
		messages = append(messages, raw)
	}
	raw, err := json.Marshal(messages)
	if err != nil {
		return nil, err
	}
	request["messages"] = raw
	return json.Marshal(request, json.Deterministic(true))
}

// Draft is a draft and its prompt as the model would read it, set against the call it follows
// on from.
func (c *Core) Draft(id string) (*store.Draft, CallPrompt, error) {
	d, err := c.cfg.Store.LoadDraft(id)
	if err != nil {
		return nil, CallPrompt{}, err
	}
	p, err := c.draftPrompt(d)
	return d, p, err
}

func (c *Core) draftPrompt(d *store.Draft) (CallPrompt, error) {
	var before *CallRequest
	if d.From != "" {
		b, err := c.requestOf(d.From)
		if err != nil {
			return CallPrompt{}, err
		}
		before = &b
	}
	return Preview(d.Model, d.Request, before)
}

// SaveDraft replaces a draft's request, and shows it as Draft does.
func (c *Core) SaveDraft(id string, request jsontext.Value) (CallPrompt, error) {
	var check map[string]jsontext.Value
	if err := json.Unmarshal(request, &check); err != nil {
		return CallPrompt{}, fmt.Errorf("%w: the request isn't a JSON object: %v", ErrBadRequest, err)
	}
	d, err := c.cfg.Store.LoadDraft(id)
	if err != nil {
		return CallPrompt{}, err
	}
	if d.Sent != "" {
		return CallPrompt{}, ErrDraftSent
	}
	d.Request = request
	if err := c.cfg.Store.SaveDraft(d); err != nil {
		return CallPrompt{}, err
	}
	return c.draftPrompt(d)
}

// SendDraft sends a draft as a manual call, as SendManual does, and marks it sent.
func (c *Core) SendDraft(id string) (string, <-chan Update, error) {
	d, err := c.cfg.Store.LoadDraft(id)
	if err != nil {
		return "", nil, err
	}
	if d.Sent != "" {
		return "", nil, ErrDraftSent
	}
	call, updates, err := c.SendManual(d.Request, d.From)
	if err != nil {
		return "", nil, err
	}
	d.Sent = call
	if err := c.cfg.Store.SaveDraft(d); err != nil {
		// The call is going either way; the draft only stays open to send again.
		return call, updates, nil
	}
	return call, updates, nil
}
