package core

import (
	"fmt"

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
	Previous       string        `json:"previous,omitzero"`
	PreviousText   string        `json:"previous_text,omitzero"`
	PreviousTokens []int         `json:"previous_tokens,omitzero"`
	Changes        []tokens.Edit `json:"changes,omitzero"`
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
	end := out.Prompt.Generated
	if end < 0 {
		end = len(out.Prompt.Text())
	}
	now, err := tokens.Tokenize(this.model, out.Prompt.Text()[:end])
	if err != nil {
		return out, fmt.Errorf("splitting the prompt: %w", err)
	}
	out.Previous, out.PreviousText, out.PreviousTokens = before.id, was.Text(), was.Tokens
	out.Changes = tokens.Diff(pieces(was.Text(), was.Tokens), pieces(out.Prompt.Text()[:end], now))
	return out, nil
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
