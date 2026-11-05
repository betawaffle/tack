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
	"net/http"
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
}

// Render produces the exact request body. The same Request always gives the
// same bytes, which is what keeps a provider's prefix cache working.
func Render(r Request) ([]byte, error) {
	return json.Marshal(struct {
		Model     string         `json:"model"`
		Messages  []Message      `json:"messages"`
		Provider  Routing        `json:"provider"`
		SessionID string         `json:"session_id,omitzero"`
		Stream    bool           `json:"stream"`
		Tools     jsontext.Value `json:"tools,omitzero"`
	}{r.Model, r.Messages, r.Routing, r.SessionID, true, r.Tools}, json.Deterministic(true))
}

// Delta is a piece of a reply as it arrives.
type Delta struct {
	Reasoning string
	Content   string
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
	Choices  []struct {
		Delta struct {
			Content          string `json:"content"`
			Reasoning        string `json:"reasoning"`
			ReasoningContent string `json:"reasoning_content"`
			ToolCalls        []struct {
				Index    int    `json:"index"`
				ID       string `json:"id"`
				Function struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				} `json:"function"`
			} `json:"tool_calls"`
		} `json:"delta"`
		FinishReason string         `json:"finish_reason"`
		Error        jsontext.Value `json:"error"`
	} `json:"choices"`
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
		for _, piece := range choice.Delta.ToolCalls {
			if piece.Index < 0 || piece.Index > maxToolCalls {
				res.ParseErrors++
				continue
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
		d := Delta{Content: choice.Delta.Content, Reasoning: choice.Delta.Reasoning}
		if d.Reasoning == "" {
			d.Reasoning = choice.Delta.ReasoningContent
		}
		if d == (Delta{}) {
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
