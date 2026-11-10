package core

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/betawaffle/tack/internal/openrouter"
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
	d := &store.Draft{ID: store.NewID(), From: from, Model: before.Model, Request: request, Agent: c.agentOf(from)}
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

// agentOf is the agent a manual call ran as, so the draft after it runs as the same one.
func (c *Core) agentOf(ref string) string {
	id, ok := strings.CutPrefix(ref, "manual/")
	if !ok {
		return ""
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if m := c.manualCall(id); m != nil {
		return m.started.Agent
	}
	return ""
}

// SetDraftAgent sets the agent a draft's tools run as. Empty is none.
func (c *Core) SetDraftAgent(id, agent string) error {
	if agent != "" {
		if _, err := c.Agent(agent); err != nil {
			return err
		}
	}
	d, err := c.cfg.Store.LoadDraft(id)
	if err != nil {
		return err
	}
	if d.Sent != "" {
		return ErrDraftSent
	}
	d.Agent = agent
	return c.cfg.Store.SaveDraft(d)
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
	// Its model is the one the request names.
	var head struct {
		Model string `json:"model"`
	}
	if json.Unmarshal(request, &head) == nil && head.Model != "" {
		d.Model = head.Model
	}
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
	if d.Agent != "" {
		if _, err := c.Agent(d.Agent); err != nil {
			return "", nil, err
		}
	}
	call, updates, err := c.sendManual(d.Request, d.From, d.Agent)
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

// NewBlankDraft starts a draft that follows on from nothing: a system prompt, the tools it's
// offered and the settings it's given, and an empty user message to write. The note tool isn't
// offered: it belongs to conversations. A built-in tool whose saved definition no longer fits
// tack's own is offered as tack defines it.
func (c *Core) NewBlankDraft(system string, offers []Offer, given *store.Settings) (*store.Draft, error) {
	settings := c.StartSettings(given)
	if given != nil {
		if err := settings.Validate(); err != nil {
			return nil, fmt.Errorf("%w: %v", ErrBadSettings, err)
		}
	}
	var kept []Offer
	for _, o := range offers {
		if !o.Custom && o.Name == "note" {
			continue
		}
		if tool := c.findTool(o.Name); !o.Custom && tool != nil && o.Definition != nil && !sameExceptDescriptions(tool.define(settings), o.Definition) {
			o.Definition = nil
		}
		kept = append(kept, o)
	}
	tools, _, err := c.offered(settings, kept)
	if err != nil {
		return nil, err
	}
	definitions, err := c.definitions(tools)
	if err != nil {
		return nil, err
	}
	var messages []openrouter.Message
	if strings.TrimSpace(system) != "" {
		messages = append(messages, openrouter.Message{Role: "system", Content: system})
	}
	messages = append(messages, openrouter.Message{Role: "user"})
	id := store.NewID()
	// The session ID keeps the calls that follow on from this one with the host that has their
	// start cached, and each draft after it keeps it, being a copy of the request.
	body, err := openrouter.Render(openrouter.Request{
		Model: settings.Model, Routing: routingFor(settings.Host), Messages: messages, SessionID: id,
		Tools: definitions, Effort: settings.ReasoningEffort,
	})
	if err != nil {
		return nil, err
	}
	d := &store.Draft{ID: id, Model: settings.Model, Request: body}
	return d, c.cfg.Store.SaveDraft(d)
}

// DraftSummary is what's listed of a draft: the last user message in it that has text, or else
// the last message that has, cut short.
type DraftSummary struct {
	ID      string    `json:"id"`
	Updated time.Time `json:"updated"`
	From    string    `json:"from,omitzero"`
	Agent   string    `json:"agent,omitzero"`
	Model   string    `json:"model"`
	Last    string    `json:"last,omitzero"`
}

// Drafts lists the drafts that haven't been sent, the one changed last first.
func (c *Core) Drafts() ([]DraftSummary, error) {
	all, err := c.cfg.Store.Drafts()
	if err != nil {
		return nil, err
	}
	out := []DraftSummary{}
	for _, d := range all {
		if d.Sent != "" {
			continue
		}
		s := DraftSummary{ID: d.ID, Updated: d.Updated, From: d.From, Agent: d.Agent, Model: d.Model}
		var req struct {
			Messages []struct {
				Role    string `json:"role"`
				Content any    `json:"content"`
			} `json:"messages"`
		}
		if json.Unmarshal(d.Request, &req) == nil {
			// What the person wrote last, or else whatever has text.
			for _, role := range []string{"user", ""} {
				for _, m := range slices.Backward(req.Messages) {
					if text, ok := m.Content.(string); ok && strings.TrimSpace(text) != "" && (role == "" || m.Role == role) {
						s.Last = cut(strings.TrimSpace(text), 120)
						break
					}
				}
				if s.Last != "" {
					break
				}
			}
		}
		out = append(out, s)
	}
	return out, nil
}

// cut shortens text to at most n characters, marking that it was.
func cut(text string, n int) string {
	runes := []rune(text)
	if len(runes) <= n {
		return text
	}
	return string(runes[:n-1]) + "…"
}
