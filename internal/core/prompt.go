package core

import (
	"encoding/json/jsontext"
	"fmt"
	"strings"

	"github.com/betawaffle/tack/internal/store"
	"github.com/betawaffle/tack/internal/tokens"
)

// CallPrompt is one call as the model saw it: the prompt the request became, and what the
// model wrote after it, set against what it saw and wrote in the call before.
type CallPrompt struct {
	Call   string         `json:"call"`
	Model  string         `json:"model"`
	Host   string         `json:"host,omitzero"`
	Prompt *tokens.Prompt `json:"prompt"`
	// The call before, in the same conversation, and how its prompt and reply became this
	// call's prompt, token by token. This call's own reply isn't in the comparison.
	Previous       string `json:"previous,omitzero"`
	PreviousText   string `json:"previous_text,omitzero"`
	PreviousTokens []int  `json:"previous_tokens,omitzero"`
	// PreviousPrompt is how many of the previous tokens are its prompt, before its reply.
	PreviousPrompt int `json:"previous_prompt,omitzero"`
	// Shown is, for a draft, where each message of the prompt is in the draft's request, which
	// also has the messages that are turned off.
	Shown   []int         `json:"shown,omitzero"`
	Changes []tokens.Edit `json:"changes,omitzero"`
	// Calls lists every call in the conversation, in order, to move between them.
	Calls []string `json:"calls"`
	// What the host said the prompt and the reply came to, in tokens, to check the counting
	// against. Zero when it didn't say.
	ReportedPrompt int `json:"reported_prompt,omitzero"`
	// CacheBlock is how many tokens the host caches at a time.
	CacheBlock     int `json:"cache_block"`
	ReportedOutput int `json:"reported_output,omitzero"`
}

// a call's request and what it wrote, read under mu.
type callSource struct {
	id, model, host string
	request         store.Ref
	out             *tokens.Output
	stats           *Stats
}

func (c *Core) callSource(it *item) callSource {
	src := callSource{id: it.callID, model: it.model, host: it.host, request: it.request}
	if it.finished != nil && len(it.finished.Usage) > 0 {
		src.stats = statsFrom(it.finished)
	}
	// What the model wrote, before anyone changed it.
	if ev := it.finished; ev != nil && ev.Error == "" && usable(ev) {
		out := &tokens.Output{}
		if ev.Text != "" {
			out.Content = c.text([]store.Part{{Block: ev.Text}})
		}
		if ev.Reasoning != "" {
			out.Reasoning = c.text([]store.Part{{Block: ev.Reasoning}})
		}
		for _, call := range it.calls {
			args, _ := c.cfg.Store.Get(call.Arguments)
			out.Calls = append(out.Calls, tokens.OutputCall{Name: call.Name, Arguments: string(args)})
		}
		src.out = out
	}
	return src
}

// Prompt shows a call as the model saw it.
func (c *Core) Prompt(convID, callID string) (CallPrompt, error) {
	c.mu.Lock()
	conv := c.convs[convID]
	if conv == nil {
		c.mu.Unlock()
		return CallPrompt{}, ErrNotFound
	}
	var this, before *callSource
	var calls []string
	for _, it := range conv.items {
		if it.callID == "" || it.request == "" {
			continue
		}
		calls = append(calls, it.callID)
		if this != nil {
			continue
		}
		src := c.callSource(it)
		if it.callID == callID {
			this = &src
		} else {
			before = &src
		}
	}
	c.mu.Unlock()
	if this == nil {
		return CallPrompt{}, ErrNotFound
	}
	out := CallPrompt{Call: this.id, Model: this.model, Host: this.host, Calls: calls, CacheBlock: CacheBlockTokens}
	if this.stats != nil {
		out.ReportedPrompt, out.ReportedOutput = this.stats.PromptTokens, this.stats.CompletionTokens
	}
	body, err := c.cfg.Store.Get(this.request)
	if err != nil {
		return out, err
	}
	if out.Prompt, err = tokens.Render(this.model, body, this.out); err != nil {
		return out, err
	}
	if before == nil {
		return out, nil
	}
	prev, err := c.cfg.Store.Get(before.request)
	if err != nil {
		return out, err
	}
	was, err := tokens.Render(before.model, prev, before.out)
	if err != nil {
		// A call to a model whose format isn't known can't be compared with.
		return out, nil
	}
	out.Previous = before.id
	return out, compare(&out, this.model, was)
}

// compare sets what changed from the prompt and reply before to this prompt, token by token,
// and how much of the prompt before there was, which is what the host could have cached.
func compare(out *CallPrompt, model string, was *tokens.Prompt) error {
	end := out.Prompt.Generated
	if end < 0 {
		end = len(out.Prompt.Text())
	}
	now, err := tokens.Tokenize(model, out.Prompt.Text()[:end])
	if err != nil {
		return fmt.Errorf("splitting the prompt: %w", err)
	}
	out.PreviousText, out.PreviousTokens = was.Text(), was.Tokens
	out.PreviousPrompt = len(was.Tokens)
	if was.Generated >= 0 {
		out.PreviousPrompt = len(tokens.SplitAt(was.Tokens, was.Generated))
	}
	out.Changes = tokens.Diff(pieces(was.Text(), was.Tokens), pieces(out.Prompt.Text()[:end], now))
	return nil
}

// CallRequest is what a call sent and what the model wrote back, to start a draft from.
type CallRequest struct {
	Model   string         `json:"model"`
	Request jsontext.Value `json:"request"`
	Output  *tokens.Output `json:"output,omitzero"`
}

// Request is a call's request and reply.
func (c *Core) Request(convID, callID string) (CallRequest, error) {
	return c.requestOf(convID + "/" + callID)
}

// Preview shows a request as the model would read it, without sending it. With before, it's
// compared with that request and the reply to it, the way a call is compared with the call
// before it.
func Preview(model string, request jsontext.Value, before *CallRequest) (CallPrompt, error) {
	out := CallPrompt{Model: model, Calls: []string{}, CacheBlock: CacheBlockTokens}
	p, err := tokens.Render(model, request, nil)
	if err != nil {
		return out, err
	}
	out.Prompt = p
	if before == nil {
		return out, nil
	}
	was, err := tokens.Render(before.Model, before.Request, before.Output)
	if err != nil {
		return out, err
	}
	return out, compare(&out, model, was)
}

// pieces cuts text into its tokens' text.
func pieces(text string, lengths []int) []string {
	out := make([]string, 0, len(lengths))
	at := 0
	for _, n := range lengths {
		out = append(out, text[at:at+n])
		at += n
	}
	return out
}

// CacheBlockTokens is how many tokens the host caches at a time. Morph's, found from the record by
// tack-show -cache: everything it reported as cached was a whole number of these.
const CacheBlockTokens = 512

// manualSource is a manual call's request and what it wrote, read under mu.
func (c *Core) manualSource(m *manualCall) callSource {
	src := callSource{id: "manual/" + m.started.Call, model: m.started.Model, request: m.started.Request}
	ev := m.finished
	if ev == nil {
		return src
	}
	src.host = ev.Host
	if len(ev.Usage) > 0 {
		src.stats = statsFrom(ev)
	}
	if ev.Error == "" && usable(ev) {
		out := &tokens.Output{}
		if ev.Text != "" {
			out.Content = c.text([]store.Part{{Block: ev.Text}})
		}
		if ev.Reasoning != "" {
			out.Reasoning = c.text([]store.Part{{Block: ev.Reasoning}})
		}
		for _, call := range ev.ToolCalls {
			args, _ := c.cfg.Store.Get(call.Arguments)
			out.Calls = append(out.Calls, tokens.OutputCall{Name: call.Name, Arguments: string(args)})
		}
		src.out = out
	}
	return src
}

// sourceOf finds a call by conversation/call or manual/call. The caller holds mu.
func (c *Core) sourceOf(ref string) (*callSource, string) {
	where, id, ok := strings.Cut(ref, "/")
	if !ok {
		return nil, ""
	}
	if where == "manual" {
		m := c.manualCall(id)
		if m == nil {
			return nil, ""
		}
		src := c.manualSource(m)
		return &src, m.started.From
	}
	conv := c.convs[where]
	if conv == nil {
		return nil, ""
	}
	for _, it := range conv.items {
		if it.callID == id && it.request != "" {
			src := c.callSource(it)
			return &src, ""
		}
	}
	return nil, ""
}

// ManualRequest is a manual call's request and reply, to start a draft from.
func (c *Core) ManualRequest(id string) (CallRequest, error) {
	return c.requestOf("manual/" + id)
}

// ManualPrompt shows a manual call as the model saw it, set against the call its draft was
// made from. Its calls are the chain of drafts it came from, oldest first, as
// conversation/call or manual/call.
func (c *Core) ManualPrompt(id string) (CallPrompt, error) {
	c.mu.Lock()
	this, from := c.sourceOf("manual/" + id)
	if this == nil {
		c.mu.Unlock()
		return CallPrompt{}, ErrNotFound
	}
	chain := []string{this.id}
	var before *callSource
	for ref, n := from, 0; ref != "" && n < 1000; n++ {
		src, next := c.sourceOf(ref)
		if src == nil {
			break
		}
		if before == nil {
			before = src
		}
		chain = append([]string{ref}, chain...)
		ref = next
	}
	c.mu.Unlock()
	out := CallPrompt{Call: this.id, Model: this.model, Host: this.host, Calls: chain, CacheBlock: CacheBlockTokens}
	if this.stats != nil {
		out.ReportedPrompt, out.ReportedOutput = this.stats.PromptTokens, this.stats.CompletionTokens
	}
	body, err := c.cfg.Store.Get(this.request)
	if err != nil {
		return out, err
	}
	if out.Prompt, err = tokens.Render(this.model, body, this.out); err != nil {
		return out, err
	}
	if before == nil {
		return out, nil
	}
	prev, err := c.cfg.Store.Get(before.request)
	if err != nil {
		return out, err
	}
	was, err := tokens.Render(before.model, prev, before.out)
	if err != nil {
		return out, nil
	}
	out.Previous = from
	return out, compare(&out, this.model, was)
}
