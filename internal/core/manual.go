package core

import (
	"bytes"
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"slices"
	"time"

	"github.com/betawaffle/tack/internal/openrouter"
	"github.com/betawaffle/tack/internal/store"
)

// A manual call is a request someone wrote by hand and sent outside any conversation.
// It's recorded the way a conversation's calls are: the exact request before anything
// is sent, then everything that came back.
type manualCall struct {
	started  *store.Event
	finished *store.Event
	cancel   context.CancelCauseFunc // while it runs in this process
}

// ErrBadRequest says a hand-written request isn't something that can be sent.
var ErrBadRequest = errors.New("the request has to be a JSON object with a model")

var errManualCancelled = errors.New("cancelled from the request page")

// ManualSummary is one hand-written request, for the list on its page.
type ManualSummary struct {
	ID           string    `json:"id"`
	Time         time.Time `json:"time"`
	Model        string    `json:"model"`
	Host         string    `json:"host,omitzero"`
	RequestBytes int       `json:"request_bytes"`
	Running      bool      `json:"running,omitzero"`
	Interrupted  bool      `json:"interrupted,omitzero"`
	Status       int       `json:"status,omitzero"`
	FinishReason string    `json:"finish_reason,omitzero"`
	Error        string    `json:"error,omitzero"`
	CostUSD      float64   `json:"cost_usd,omitzero"`
	Tokens       int       `json:"tokens,omitzero"` // counted here, before sending
}

// ManualDetail is everything about one, as recorded.
type ManualDetail struct {
	ManualSummary
	Request         string              `json:"request"`
	RequestHeaders  map[string][]string `json:"request_headers,omitzero"`
	Response        string              `json:"response,omitzero"`
	ResponseHeaders map[string][]string `json:"response_headers,omitzero"`
	GenerationID    string              `json:"generation_id,omitzero"`
	Usage           jsontext.Value      `json:"usage,omitzero"`
	TTFTMillis      int64               `json:"ttft_ms,omitzero"`
	DurationMillis  int64               `json:"duration_ms,omitzero"`
	ParseErrors     int                 `json:"parse_errors,omitzero"`
	Text            string              `json:"text,omitzero"`
	Reasoning       string              `json:"reasoning,omitzero"`
	ToolCalls       []ManualToolCall    `json:"tool_calls,omitzero"`
}

// ManualToolCall is a tool call a hand-written request got back. Nothing runs it.
type ManualToolCall struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

// RecordedCall is a request already in the record, which a new one can start from.
type RecordedCall struct {
	Call         string    `json:"call"`
	Conversation string    `json:"conversation,omitzero"`
	Preview      string    `json:"preview,omitzero"`
	Time         time.Time `json:"time"`
	Model        string    `json:"model"`
	Host         string    `json:"host,omitzero"`
	RequestBytes int       `json:"request_bytes"`
}

// applyManual updates state from a manual call's event. The caller holds mu.
func (c *Core) applyManual(ev *store.Event) {
	switch ev.Type {
	case store.ManualCallStarted:
		c.manual = append(c.manual, &manualCall{started: ev})
	case store.ManualCallFinished:
		if m := c.manualCall(ev.Call); m != nil {
			m.finished = ev
		}
	}
}

func (c *Core) manualCall(id string) *manualCall {
	for _, m := range c.manual {
		if m.started.Call == id {
			return m
		}
	}
	return nil
}

// manualHost is the first provider a request asked for, if it named one.
func manualHost(routing jsontext.Value) string {
	var provider struct {
		Order []string `json:"order"`
	}
	if json.Unmarshal(routing, &provider, json.RejectUnknownMembers(false)) == nil && len(provider.Order) > 0 {
		return provider.Order[0]
	}
	return ""
}

func (c *Core) manualSummary(m *manualCall) ManualSummary {
	s := ManualSummary{
		ID: m.started.Call, Time: m.started.Time, Model: m.started.Model,
		Host: manualHost(m.started.Routing), RequestBytes: m.started.RequestBytes,
		Running: m.cancel != nil && m.finished == nil, Interrupted: m.cancel == nil && m.finished == nil,
	}
	if f := m.finished; f != nil {
		s.Status, s.FinishReason, s.Error = f.Status, f.FinishReason, f.Error
		if f.Host != "" {
			s.Host = f.Host
		}
		if cost, ok := usageCost(f); ok {
			s.CostUSD = cost.USD()
		}
	}
	return s
}

// ManualCalls lists the hand-written requests, newest first.
func (c *Core) ManualCalls() []ManualSummary {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]ManualSummary, 0, len(c.manual))
	for _, m := range slices.Backward(c.manual) {
		out = append(out, c.manualSummary(m))
	}
	return out
}

// ManualCall returns everything recorded about one.
func (c *Core) ManualCall(id string) (ManualDetail, error) {
	c.mu.Lock()
	m := c.manualCall(id)
	var d ManualDetail
	if m != nil {
		d.ManualSummary = c.manualSummary(m)
	}
	c.mu.Unlock()
	if m == nil {
		return d, ErrNotFound
	}
	request, err := c.cfg.Store.Get(m.started.Request)
	if err != nil {
		return d, err
	}
	d.Request, d.RequestHeaders = string(request), m.started.RequestHeaders
	d.Tokens = countRequest(m.started.Model, request)
	f := m.finished
	if f == nil {
		return d, nil
	}
	d.ResponseHeaders, d.GenerationID, d.Usage = f.ResponseHeaders, f.GenerationID, f.Usage
	d.TTFTMillis, d.DurationMillis, d.ParseErrors = f.TTFTMillis, f.DurationMillis, f.ParseErrors
	for _, part := range []struct {
		ref  store.Ref
		into *string
	}{{f.Response, &d.Response}, {f.Text, &d.Text}, {f.Reasoning, &d.Reasoning}} {
		if part.ref == "" {
			continue
		}
		data, err := c.cfg.Store.Get(part.ref)
		if err != nil {
			return d, err
		}
		*part.into = string(data)
	}
	for _, call := range f.ToolCalls {
		arguments, err := c.cfg.Store.Get(call.Arguments)
		if err != nil {
			return d, err
		}
		d.ToolCalls = append(d.ToolCalls, ManualToolCall{ID: call.ID, Name: call.Name, Arguments: string(arguments)})
	}
	return d, nil
}

// CountManual counts a request's tokens, for the model it names, before it's sent. Zero
// means it can't be counted here.
func CountManual(body []byte) int {
	var head struct {
		Model string `json:"model"`
	}
	if json.Unmarshal(body, &head, json.RejectUnknownMembers(false)) != nil {
		return 0
	}
	return countRequest(head.Model, body)
}

// SendManual sends a hand-written request exactly as given, to the configured
// endpoint with the usual headers and key. It's recorded before anything is sent.
// The updates are a preview, like a conversation's: the record has everything.
func (c *Core) SendManual(body []byte, from string) (string, <-chan Update, error) {
	var head struct {
		Model    string         `json:"model"`
		Provider jsontext.Value `json:"provider"`
	}
	trimmed := bytes.TrimSpace(body)
	if !jsontext.Value(trimmed).IsValid() || len(trimmed) == 0 || trimmed[0] != '{' {
		return "", nil, ErrBadRequest
	}
	if err := json.Unmarshal(body, &head, json.RejectUnknownMembers(false)); err != nil || head.Model == "" {
		return "", nil, ErrBadRequest
	}
	request, err := c.cfg.Store.Put(body)
	if err != nil {
		return "", nil, err
	}
	id := store.NewID()
	started := &store.Event{
		Type:           store.ManualCallStarted,
		Call:           id,
		Model:          head.Model,
		Routing:        head.Provider,
		URL:            c.cfg.Client.URL,
		Request:        request,
		RequestBytes:   len(body),
		RequestHeaders: openrouter.Headers(),
		From:           from,
	}
	ctx, cancel := context.WithCancelCause(context.Background())
	c.mu.Lock()
	if c.restarting {
		c.mu.Unlock()
		cancel(nil)
		return "", nil, ErrRestarting
	}
	if err := c.cfg.Store.Append(started); err != nil {
		c.mu.Unlock()
		cancel(nil)
		return "", nil, err
	}
	c.applyManual(started)
	m := c.manualCall(id)
	m.cancel = cancel
	c.mu.Unlock()

	updates := make(chan Update, updateBuffer)
	send := func(u Update) {
		select {
		case updates <- u:
		default:
		}
	}
	send(Update{Kind: "call", ID: id})
	go func() {
		defer close(updates)
		err := c.runManual(ctx, m, body, send)
		c.mu.Lock()
		m.cancel = nil
		c.checkDrained()
		c.mu.Unlock()
		cancel(nil)
		if err != nil {
			send(Update{Kind: "error", Text: err.Error()})
		}
		send(Update{Kind: "done", ID: id})
	}()
	return id, updates, nil
}

func (c *Core) runManual(ctx context.Context, m *manualCall, body []byte, send func(Update)) error {
	id := m.started.Call
	partial, err := c.cfg.Store.NewPartial("manual-" + id)
	if err != nil {
		return err
	}
	res, doErr := c.cfg.Client.Do(ctx, body, partial, func(d openrouter.Delta) {
		if d.Reasoning != "" {
			send(Update{Kind: "reasoning", Text: d.Reasoning})
		}
		if d.Content != "" {
			send(Update{Kind: "content", Text: d.Content})
		}
	})
	finished := &store.Event{
		Type:            store.ManualCallFinished,
		Call:            id,
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
	if errors.Is(context.Cause(ctx), errManualCancelled) {
		finished.Cancelled = true
		finished.Error = errManualCancelled.Error()
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
	for _, call := range res.ToolCalls {
		arguments, err := c.cfg.Store.Put([]byte(call.Arguments))
		if err != nil {
			return err
		}
		finished.ToolCalls = append(finished.ToolCalls, store.ToolCall{ID: call.ID, Name: call.Name, Arguments: arguments})
	}
	return c.record(finished)
}

// CancelManual stops a hand-written request that's still running.
func (c *Core) CancelManual(id string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	m := c.manualCall(id)
	if m == nil {
		return ErrNotFound
	}
	if m.cancel == nil {
		return ErrNothingRunning
	}
	m.cancel(errManualCancelled)
	return nil
}

// manualRunning says whether a hand-written request is still going. The caller holds mu.
func (c *Core) manualRunning() bool {
	return slices.ContainsFunc(c.manual, func(m *manualCall) bool { return m.cancel != nil })
}

// RecordedCalls lists the requests in the record, newest first, up to limit: every
// conversation's calls and the hand-written ones.
func (c *Core) RecordedCalls(limit int) []RecordedCall {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []RecordedCall
	for _, id := range c.order {
		conv := c.convs[id]
		preview := c.summary(conv).Preview
		for _, it := range conv.items {
			if it.callID != "" && it.request != "" {
				out = append(out, RecordedCall{Call: it.callID, Conversation: id, Preview: preview, Time: it.time, Model: it.model, Host: it.host, RequestBytes: it.requestBytes})
			}
		}
	}
	for _, m := range c.manual {
		out = append(out, RecordedCall{Call: m.started.Call, Time: m.started.Time, Model: m.started.Model, Host: manualHost(m.started.Routing), RequestBytes: m.started.RequestBytes})
	}
	slices.SortStableFunc(out, func(a, b RecordedCall) int { return b.Time.Compare(a.Time) })
	if len(out) > limit {
		out = out[:limit]
	}
	return out
}

// RecordedRequest returns the exact body of a request in the record.
func (c *Core) RecordedRequest(call string) ([]byte, error) {
	c.mu.Lock()
	var ref store.Ref
	if m := c.manualCall(call); m != nil {
		ref = m.started.Request
	}
	for _, id := range c.order {
		if ref != "" {
			break
		}
		for _, it := range c.convs[id].items {
			if it.callID == call {
				ref = it.request
				break
			}
		}
	}
	c.mu.Unlock()
	if ref == "" {
		return nil, ErrNotFound
	}
	return c.cfg.Store.Get(ref)
}
