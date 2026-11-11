// Package openrouter talks to OpenRouter's Chat Completions API.
package openrouter

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/betawaffle/tack/internal/secret"
)

// DefaultURL is the Chat Completions endpoint.
const DefaultURL = "https://openrouter.ai/api/v1/chat/completions"

// DefaultIdleTimeout ends a call when nothing at all arrives for this long.
// OpenRouter's keep-alive comments count as something arriving.
const DefaultIdleTimeout = 5 * time.Minute

// Routing is the provider section of a request.
type Routing struct {
	Order          []string `json:"order"`
	AllowFallbacks bool     `json:"allow_fallbacks"`
}

// Message is a rendered message.
type Message struct {
	Role       string     `json:"role"`
	Content    string     `json:"content"`
	Name       string     `json:"name,omitzero"`
	Reasoning  string     `json:"reasoning,omitzero"`
	ToolCalls  []ToolCall `json:"tool_calls,omitzero"`
	ToolCallID string     `json:"tool_call_id,omitzero"`
}

// ToolCall is a tool call as the API writes it.
type ToolCall struct {
	ID       string       `json:"id"`
	Type     string       `json:"type"`
	Function FunctionCall `json:"function"`
}

// FunctionCall names a function and holds its arguments as exact text.
type FunctionCall struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

// Request describes a call to render.
type Request struct {
	Model     string
	Routing   Routing
	Messages  []Message
	SessionID string
	Tools     jsontext.Value // exact definitions, if there are any
	// Effort is the reasoning effort to ask for. Empty leaves it out.
	Effort string
}

// Reasoning is the reasoning section of a request.
type Reasoning struct {
	Effort string `json:"effort"`
}

// Render produces the exact request body. The same Request always gives the
// same bytes, which is what keeps a provider's prefix cache working.
func Render(r Request) ([]byte, error) {
	var reasoning *Reasoning
	if r.Effort != "" {
		reasoning = &Reasoning{Effort: r.Effort}
	}
	return json.Marshal(struct {
		Model     string         `json:"model"`
		Messages  []Message      `json:"messages"`
		Provider  Routing        `json:"provider"`
		Reasoning *Reasoning     `json:"reasoning,omitzero"`
		SessionID string         `json:"session_id,omitzero"`
		Stream    bool           `json:"stream"`
		Tools     jsontext.Value `json:"tools,omitzero"`
	}{r.Model, r.Messages, r.Routing, reasoning, r.SessionID, true, r.Tools}, json.Deterministic(true))
}

// Delta is a piece of a reply as it arrives.
type Delta struct {
	Reasoning string
	Content   string
	// Calls are pieces of tool calls: each call's name comes first, then its arguments, a piece
	// at a time.
	Calls []CallDelta
}

// CallDelta is a piece of tool call Index: its name, once, and some of its arguments.
type CallDelta struct {
	Index     int
	Name      string
	Arguments string
}

// Result is what a finished call produced.
type Result struct {
	RequestHeaders  map[string][]string
	Status          int
	ResponseHeaders map[string][]string
	Host            string
	GenerationID    string
	FinishReason    string
	Usage           jsontext.Value
	TTFT            time.Duration
	Duration        time.Duration
	Text            string
	Reasoning       string
	ToolCalls       []FunctionCallResult
	ParseErrors     int
	Error           string
}

// FunctionCallResult is a tool call put together from a stream.
type FunctionCallResult struct {
	ID        string
	Name      string
	Arguments string
}

type chunk struct {
	ID       string         `json:"id"`
	Provider string         `json:"provider"`
	Usage    jsontext.Value `json:"usage"`
	Error    jsontext.Value `json:"error"`
	Choices  []chunkChoice  `json:"choices"`
}

type chunkChoice struct {
	Delta struct {
		Content          string      `json:"content"`
		Reasoning        string      `json:"reasoning"`
		ReasoningContent string      `json:"reasoning_content"`
		ToolCalls        []chunkCall `json:"tool_calls"`
	} `json:"delta"`
	FinishReason string         `json:"finish_reason"`
	Error        jsontext.Value `json:"error"`
}

type chunkCall struct {
	Index    int    `json:"index"`
	ID       string `json:"id"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

// wholeReply is a reply that wasn't streamed. Its choices have a message where a
// stream's chunks have a delta.
type wholeReply struct {
	ID       string         `json:"id"`
	Provider string         `json:"provider"`
	Usage    jsontext.Value `json:"usage"`
	Error    jsontext.Value `json:"error"`
	Choices  []struct {
		Message struct {
			Content          string `json:"content"`
			Reasoning        string `json:"reasoning"`
			ReasoningContent string `json:"reasoning_content"`
			ToolCalls        []struct {
				ID       string `json:"id"`
				Function struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				} `json:"function"`
			} `json:"tool_calls"`
		} `json:"message"`
		FinishReason string         `json:"finish_reason"`
		Error        jsontext.Value `json:"error"`
	} `json:"choices"`
}

// chunk turns it into one chunk holding everything, so it's read the same way.
func (w *wholeReply) chunk() *chunk {
	ch := &chunk{ID: w.ID, Provider: w.Provider, Usage: w.Usage, Error: w.Error}
	ch.Choices = make([]chunkChoice, len(w.Choices))
	for i, choice := range w.Choices {
		out := &ch.Choices[i]
		out.FinishReason, out.Error = choice.FinishReason, choice.Error
		m := choice.Message
		out.Delta.Content, out.Delta.Reasoning, out.Delta.ReasoningContent = m.Content, m.Reasoning, m.ReasoningContent
		for j, call := range m.ToolCalls {
			out.Delta.ToolCalls = append(out.Delta.ToolCalls, chunkCall{Index: j, ID: call.ID, Function: call.Function})
		}
	}
	return ch
}

// maxToolCalls bounds the index a stream can name, since the index sizes a slice.
const maxToolCalls = 1024

// Client sends requests.
type Client struct {
	HTTP        *http.Client
	URL         string
	Key         secret.Key
	IdleTimeout time.Duration
}

// NewClient returns a Client that refuses redirects, so the key only ever goes
// to the configured URL.
func NewClient(url string, key secret.Key) *Client {
	return &Client{
		URL:         url,
		Key:         key,
		IdleTimeout: DefaultIdleTimeout,
		HTTP: &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		}},
	}
}

// Headers returns the non-secret request headers, which are also what gets
// recorded. The key is added only at the moment of sending.
func Headers() http.Header {
	return http.Header{
		"Accept":       {"text/event-stream"},
		"Content-Type": {"application/json"},
		"User-Agent":   {"tack"},
		"X-Title":      {"tack"},
	}
}

// Do sends body and streams the response. Every response byte goes to raw,
// whatever it turns out to be. Deltas go to onDelta as they arrive. The error
// is only for failures to complete the exchange; a provider's reported error
// is in Result.Error.
func (c *Client) Do(ctx context.Context, body []byte, raw io.Writer, onDelta func(Delta)) (res Result, err error) {
	start := time.Now()
	defer func() { res.Duration = time.Since(start) }()

	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	idle := time.AfterFunc(c.IdleTimeout, func() {
		cancel(fmt.Errorf("nothing received for %s", c.IdleTimeout))
	})
	defer idle.Stop()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.URL, bytes.NewReader(body))
	if err != nil {
		return res, err
	}
	req.Header = Headers()
	res.RequestHeaders = req.Header.Clone()
	c.Key.Authorize(req)

	resp, err := c.HTTP.Do(req)
	if err != nil {
		res.Error = errorText(ctx, err)
		return res, err
	}
	defer resp.Body.Close()
	res.Status = resp.StatusCode
	res.ResponseHeaders = recordableHeaders(resp.Header)
	idle.Reset(c.IdleTimeout)

	if resp.StatusCode != http.StatusOK {
		data, err := io.ReadAll(io.TeeReader(resp.Body, raw))
		if err != nil {
			res.Error = errorText(ctx, err)
			return res, err
		}
		res.Error = fmt.Sprintf("HTTP %d: %s", resp.StatusCode, data)
		return res, nil
	}

	var text, reasoning strings.Builder
	defer func() { res.Text, res.Reasoning = text.String(), reasoning.String() }()

	if mediaType, _, _ := mime.ParseMediaType(resp.Header.Get("Content-Type")); mediaType == "application/json" {
		// A request that didn't ask to stream gets the whole reply in one object.
		data, err := io.ReadAll(io.TeeReader(resp.Body, raw))
		if err != nil {
			res.Error = errorText(ctx, err)
			return res, err
		}
		var whole wholeReply
		if err := json.Unmarshal(data, &whole, json.RejectUnknownMembers(false)); err != nil {
			res.ParseErrors++
			res.Error = "the reply wasn't the JSON it said it was"
			return res, nil
		}
		c.apply(&res, whole.chunk(), &text, &reasoning, start, onDelta)
		if res.Error == "" && res.FinishReason == "" {
			res.Error = "the reply had no finish reason"
		}
		return res, nil
	}

	r := bufio.NewReader(io.TeeReader(resp.Body, raw))
	for {
		line, readErr := r.ReadBytes('\n')
		idle.Reset(c.IdleTimeout)
		if payload, ok := dataLine(line); ok {
			var ch chunk
			if err := json.Unmarshal(payload, &ch); err != nil {
				res.ParseErrors++
			} else {
				c.apply(&res, &ch, &text, &reasoning, start, onDelta)
			}
		}
		if readErr == nil {
			continue
		}
		if !errors.Is(readErr, io.EOF) {
			res.Error = errorText(ctx, readErr)
			return res, readErr
		}
		if res.Error == "" && res.FinishReason == "" {
			res.Error = "stream ended without a finish reason"
		}
		return res, nil
	}
}

func (c *Client) apply(res *Result, ch *chunk, text, reasoning *strings.Builder, start time.Time, onDelta func(Delta)) {
	if res.GenerationID == "" {
		res.GenerationID = ch.ID
	}
	if res.Host == "" {
		res.Host = ch.Provider
	}
	if present(ch.Usage) {
		res.Usage = ch.Usage.Clone()
	}
	if present(ch.Error) {
		res.Error = string(ch.Error)
	}
	for _, choice := range ch.Choices {
		if present(choice.Error) {
			res.Error = string(choice.Error)
		}
		if choice.FinishReason != "" {
			res.FinishReason = choice.FinishReason
		}
		var calls []CallDelta
		for _, piece := range choice.Delta.ToolCalls {
			if piece.Index < 0 || piece.Index > maxToolCalls {
				res.ParseErrors++
				continue
			}
			if piece.Function.Name != "" || piece.Function.Arguments != "" {
				calls = append(calls, CallDelta{Index: piece.Index, Name: piece.Function.Name, Arguments: piece.Function.Arguments})
			}
			for len(res.ToolCalls) <= piece.Index {
				res.ToolCalls = append(res.ToolCalls, FunctionCallResult{})
			}
			call := &res.ToolCalls[piece.Index]
			if call.ID == "" {
				call.ID = piece.ID
			}
			if call.Name == "" {
				call.Name = piece.Function.Name
			}
			call.Arguments += piece.Function.Arguments
			if res.TTFT == 0 {
				res.TTFT = time.Since(start)
			}
		}
		d := Delta{Content: choice.Delta.Content, Reasoning: choice.Delta.Reasoning, Calls: calls}
		if d.Reasoning == "" {
			d.Reasoning = choice.Delta.ReasoningContent
		}
		if d.Content == "" && d.Reasoning == "" && len(d.Calls) == 0 {
			continue
		}
		if res.TTFT == 0 {
			res.TTFT = time.Since(start)
		}
		text.WriteString(d.Content)
		reasoning.WriteString(d.Reasoning)
		if onDelta != nil {
			onDelta(d)
		}
	}
}

// dataLine returns the payload of an SSE data line. Comments, such as
// OpenRouter's keep-alives, and the [DONE] marker have none.
func dataLine(line []byte) ([]byte, bool) {
	payload, ok := bytes.CutPrefix(bytes.TrimRight(line, "\r\n"), []byte("data:"))
	if !ok {
		return nil, false
	}
	payload = bytes.TrimLeft(payload, " ")
	if len(payload) == 0 || bytes.Equal(payload, []byte("[DONE]")) {
		return nil, false
	}
	return payload, true
}

func present(v jsontext.Value) bool {
	return len(v) > 0 && !bytes.Equal(v, []byte("null"))
}

// errorText prefers the reason a context was cancelled, like the idle timeout.
func errorText(ctx context.Context, err error) string {
	if cause := context.Cause(ctx); cause != nil {
		return cause.Error()
	}
	return err.Error()
}

// recordableHeaders drops cookies, which aren't useful and may be sensitive.
func recordableHeaders(h http.Header) map[string][]string {
	out := h.Clone()
	out.Del("Set-Cookie")
	return out
}

// Price is what a host charges, in dollars per token, as OpenRouter lists it.
type Price struct {
	Prompt         string `json:"prompt"`
	Completion     string `json:"completion"`
	InputCacheRead string `json:"input_cache_read,omitzero"`
}

// Endpoint is one host's offer of a model. Overrides are other prices that apply at
// certain times, and Discount is a fraction taken off.
type Endpoint struct {
	Tag       string  `json:"tag"`
	Pricing   Price   `json:"pricing"`
	Overrides []Price `json:"-"`
	Discount  float64 `json:"-"`
	// The most tokens a call's context and reply can have, if the host says.
	ContextLength       int `json:"-"`
	MaxCompletionTokens int `json:"-"`
}

type endpointWire struct {
	Tag                 string `json:"tag"`
	ContextLength       *int   `json:"context_length"`
	MaxCompletionTokens *int   `json:"max_completion_tokens"`
	Pricing             struct {
		Price
		Discount  float64 `json:"discount"`
		Overrides []Price `json:"overrides"`
	} `json:"pricing"`
}

// Endpoints asks OpenRouter which hosts offer a model and what they charge. It's public,
// so it goes without the key, to the models API next to the configured URL.
func (c *Client) Endpoints(ctx context.Context, model string) ([]Endpoint, error) {
	base, ok := strings.CutSuffix(c.URL, "/chat/completions")
	if !ok {
		return nil, fmt.Errorf("can't find the models API next to %s", c.URL)
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/models/"+model+"/endpoints", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "tack")
	res, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("listing %s's hosts: %s", model, res.Status)
	}
	var body struct {
		Data struct {
			Endpoints []endpointWire `json:"endpoints"`
		} `json:"data"`
	}
	if err := json.UnmarshalRead(io.LimitReader(res.Body, 16<<20), &body); err != nil {
		return nil, fmt.Errorf("listing %s's hosts: %w", model, err)
	}
	var out []Endpoint
	for _, e := range body.Data.Endpoints {
		ep := Endpoint{Tag: e.Tag, Pricing: e.Pricing.Price, Overrides: e.Pricing.Overrides, Discount: e.Pricing.Discount}
		if e.ContextLength != nil {
			ep.ContextLength = *e.ContextLength
		}
		if e.MaxCompletionTokens != nil {
			ep.MaxCompletionTokens = *e.MaxCompletionTokens
		}
		out = append(out, ep)
	}
	return out, nil
}

// Generation asks OpenRouter what a call cost, by its generation ID. It's for calls whose
// stream ended before the usage came, like a cancelled one. ok is false while OpenRouter
// doesn't know yet, which can take a little while after a call. raw is its whole answer.
func (c *Client) Generation(ctx context.Context, id string) (cost float64, ok bool, raw []byte, err error) {
	base, found := strings.CutSuffix(c.URL, "/chat/completions")
	if !found {
		return 0, false, nil, fmt.Errorf("can't find the generation API next to %s", c.URL)
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/generation?id="+url.QueryEscape(id), nil)
	if err != nil {
		return 0, false, nil, err
	}
	req.Header.Set("User-Agent", "tack")
	c.Key.Authorize(req)
	res, err := c.HTTP.Do(req)
	if err != nil {
		return 0, false, nil, err
	}
	defer res.Body.Close()
	raw, err = io.ReadAll(io.LimitReader(res.Body, 1<<20))
	if err != nil {
		return 0, false, nil, err
	}
	if res.StatusCode == http.StatusNotFound {
		return 0, false, raw, nil
	}
	if res.StatusCode != http.StatusOK {
		return 0, false, raw, fmt.Errorf("asking what %s cost: %s", id, res.Status)
	}
	var body struct {
		Data struct {
			TotalCost *float64 `json:"total_cost"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		return 0, false, raw, err
	}
	if body.Data.TotalCost == nil {
		return 0, false, raw, nil
	}
	return *body.Data.TotalCost, true, raw, nil
}
