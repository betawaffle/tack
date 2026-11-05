package core

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/betawaffle/tack/internal/openrouter"
	"github.com/betawaffle/tack/internal/store"
)

// Tool is something a model can ask to run. Name and Definition are what the
// model sees, and Run is what actually happens, so one can change without the
// other. Definition is the API's whole tool object.
type Tool struct {
	Name       string
	Definition jsontext.Value
	Run        func(ctx context.Context, call Call) ([]byte, error)

	// Inspect runs when a call arrives, before any approval, on input the model
	// chose. An error means the call can't run. Flags draw a person's attention on
	// the approval card. It can touch metadata but mustn't open anything.
	Inspect func(call Call) (flags []string, err error)
}

// Call is what a tool gets. Arguments were parsed by parseArguments.
type Call struct {
	Args    map[string]jsontext.Value
	Workdir string
}

const (
	inspectTimeout = 2 * time.Second
	toolTimeout    = time.Minute
)

// FlagUnchecked is the flag on a call whose inspection didn't finish in time, as
// could happen with a path on a hung mount.
const FlagUnchecked = "Couldn't be checked in time"

// inspect runs a tool's Inspect with a time limit. A hung check leaves a goroutine
// behind, which is better than the whole reply hanging on what a model chose.
func inspect(tool *Tool, call Call) ([]string, error) {
	if tool.Inspect == nil {
		return nil, nil
	}
	type result struct {
		flags []string
		err   error
	}
	done := make(chan result, 1)
	go func() {
		flags, err := tool.Inspect(call)
		done <- result{flags, err}
	}()
	select {
	case r := <-done:
		return r.flags, r.err
	case <-time.After(inspectTimeout):
		return []string{FlagUnchecked}, nil
	}
}

// What the model is told about anything denied, unless the person says otherwise.
const (
	DefaultCallDenial   = "The user denied this tool call. It did not run."
	DefaultOutputDenial = "The tool ran, but the user withheld its output."
	DefaultInterrupted  = "This tool call was interrupted. It may have partly run, or not run at all."
)

func denialText(kind string) string {
	switch kind {
	case store.DecideOutput:
		return DefaultOutputDenial
	case store.DecideInterrupted:
		return DefaultInterrupted
	}
	return DefaultCallDenial
}

// toolCall is the state of one call, derived from events.
type toolCall struct {
	store.ToolCall
	allowed  bool // approved to run, and not since told to stop
	started  bool
	finished bool
	output   store.Ref
	failure  string
	result   []store.Part // what the model sees; set once it's final
	released bool         // the result is the tool's output, not a denial
}

func (t *toolCall) resolved() bool { return t.result != nil }

// decision is something waiting on a person. It survives restarts because it's
// derived from events like everything else.
type decision struct {
	id    string
	kind  string
	calls []string
}

// tool finds a call by ID, newest first. Some hosts reuse IDs from turn to turn,
// and only the newest batch can still be open, so an old call can't be disturbed.
func (conv *conversation) tool(id string) *toolCall {
	for _, it := range slices.Backward(conv.items) {
		for _, call := range it.calls {
			if call.ID == id {
				return call
			}
		}
	}
	return nil
}

func (conv *conversation) openCalls() []*toolCall {
	var out []*toolCall
	for _, it := range conv.items {
		for _, call := range it.calls {
			if !call.resolved() {
				out = append(out, call)
			}
		}
	}
	return out
}

func (conv *conversation) unanswered() bool { return len(conv.openCalls()) > 0 }

func (conv *conversation) applyTool(ev *store.Event) {
	switch ev.Type {
	case store.DecisionRequested:
		conv.decision = &decision{id: ev.Decision, kind: ev.Kind, calls: ev.DecisionCalls}
	case store.DecisionMade:
		for _, choice := range ev.Choices {
			call := conv.tool(choice.ToolCall)
			if call == nil {
				continue
			}
			call.allowed = choice.Allow
			if choice.Parts != nil {
				call.result = choice.Parts
				call.released = choice.Allow
			} else {
				// Allowed to run, or to run again.
				call.started, call.finished = false, false
			}
		}
		conv.decision = nil
	case store.ToolStarted:
		if call := conv.tool(ev.ToolCall); call != nil {
			call.started, call.finished = true, false
		}
	case store.ToolFinished:
		if call := conv.tool(ev.ToolCall); call != nil {
			call.finished, call.output, call.failure = true, ev.Output, ev.Error
		}
	}
}

// storeDefinitions writes the offered tools as a block. A conversation keeps
// sending these exact bytes, so changing a tool can't alter its cached prefix or
// what its earlier calls appear to have seen.
func (c *Core) storeDefinitions() (store.Ref, error) {
	if len(c.cfg.Tools) == 0 {
		return "", nil
	}
	definitions := make([]jsontext.Value, len(c.cfg.Tools))
	for i, tool := range c.cfg.Tools {
		definitions[i] = tool.Definition
	}
	data, err := json.Marshal(definitions, json.Deterministic(true))
	if err != nil {
		return "", err
	}
	return c.cfg.Store.Put(data)
}

func (c *Core) findTool(name string) *Tool {
	for i := range c.cfg.Tools {
		if c.cfg.Tools[i].Name == name {
			return &c.cfg.Tools[i]
		}
	}
	return nil
}

// parseArguments is the one parser, used both to judge a call when it arrives and
// to run it, so what gets approved is what runs. It refuses duplicate keys and
// invalid UTF-8, which is what keeps the page's view and the tool's view the same.
func parseArguments(text []byte) (map[string]jsontext.Value, error) {
	var args map[string]jsontext.Value
	if err := json.Unmarshal(text, &args); err != nil {
		return nil, err
	}
	if args == nil {
		return nil, errors.New("arguments aren't a JSON object")
	}
	return args, nil
}

// malformedCalls reports a reply whose calls couldn't be answered or trusted, which
// makes it a failed call that can be retried. A lost piece of a stream counts: the
// arguments could still parse, and then say something the model never asked for.
func malformedCalls(res openrouter.Result) string {
	if res.ParseErrors > 0 && len(res.ToolCalls) > 0 {
		return "part of a reply with tool calls couldn't be read"
	}
	seen := map[string]bool{}
	for _, call := range res.ToolCalls {
		switch {
		case call.ID == "":
			return "the model sent a tool call with no ID"
		case call.Name == "":
			return "the model sent a tool call with no name"
		case seen[call.ID]:
			return "the model sent two tool calls with the same ID"
		}
		seen[call.ID] = true
	}
	return ""
}

// definedNames lists the tools in a conversation's recorded definitions.
func definedNames(definitions []byte) ([]string, error) {
	if len(definitions) == 0 {
		return nil, nil
	}
	var tools []struct {
		Function struct {
			Name string `json:"name"`
		} `json:"function"`
	}
	if err := json.Unmarshal(definitions, &tools); err != nil {
		return nil, err
	}
	names := make([]string, len(tools))
	for i, tool := range tools {
		names[i] = tool.Function.Name
	}
	return names, nil
}

// storeCalls records calls, judging each one once. A call has to name a tool this
// conversation was actually offered. A call that can't run still goes to a person:
// no automatic reply means no loop a model can spend money on.
func (c *Core) storeCalls(definitions []byte, workdir string, calls []openrouter.FunctionCallResult) ([]store.ToolCall, error) {
	offered, err := definedNames(definitions)
	if err != nil {
		return nil, err
	}
	var out []store.ToolCall
	for _, call := range calls {
		arguments, err := c.cfg.Store.Put([]byte(call.Arguments))
		if err != nil {
			return nil, err
		}
		stored := store.ToolCall{ID: call.ID, Name: call.Name, Arguments: arguments}
		tool := c.findTool(call.Name)
		if !slices.Contains(offered, call.Name) || tool == nil {
			stored.Invalid = fmt.Sprintf("there is no tool named %q", call.Name)
		} else if args, err := parseArguments([]byte(call.Arguments)); err != nil {
			stored.Invalid = "the arguments can't be used: " + err.Error()
		} else if flags, err := inspect(tool, Call{Args: args, Workdir: workdir}); err != nil {
			stored.Invalid = "the arguments can't be used: " + err.Error()
		} else {
			stored.Flags = flags
		}
		out = append(out, stored)
	}
	return out, nil
}

// ensureDecision opens whichever decision the conversation's tool calls need. It
// reports whether one is open, in which case nothing else can happen until a
// person answers. The caller must not be running any tools.
func (c *Core) ensureDecision(conv *conversation) (bool, error) {
	c.mu.Lock()
	if conv.decision != nil {
		c.mu.Unlock()
		return true, nil
	}
	var stopped, undecided, finished []string
	for _, call := range conv.openCalls() {
		switch {
		case call.allowed && call.finished:
			finished = append(finished, call.ID)
		case call.allowed:
			stopped = append(stopped, call.ID)
		default:
			undecided = append(undecided, call.ID)
		}
	}
	c.mu.Unlock()

	kind, calls := "", []string(nil)
	switch {
	case len(stopped) > 0:
		kind, calls = store.DecideInterrupted, stopped
	case len(undecided) > 0:
		kind, calls = store.DecideCalls, undecided
	case len(finished) > 0:
		kind, calls = store.DecideOutput, finished
	default:
		return false, nil
	}
	err := c.record(&store.Event{Type: store.DecisionRequested, Conversation: conv.id, Decision: store.NewID(), Kind: kind, DecisionCalls: calls})
	return err == nil, err
}

// advance does what's next: run what was approved, stop at any decision, and
// otherwise call the model.
func (c *Core) advance(conv *conversation, send func(Update)) error {
	for {
		c.mu.Lock()
		var next *toolCall
		for _, call := range conv.openCalls() {
			if call.allowed && !call.started {
				next = call
				break
			}
		}
		c.mu.Unlock()
		if next == nil {
			break
		}
		if err := c.runTool(conv, next, send); err != nil {
			return err
		}
	}
	if waiting, err := c.ensureDecision(conv); waiting || err != nil {
		return err
	}
	if err := c.call(conv, send); err != nil {
		return err
	}
	_, err := c.ensureDecision(conv)
	return err
}

// runTool runs one approved call. The intent is durable first, so a stop leaves a
// started call with no finish, which is never run again without asking.
func (c *Core) runTool(conv *conversation, call *toolCall, send func(Update)) error {
	if err := c.record(&store.Event{Type: store.ToolStarted, Conversation: conv.id, ToolCall: call.ID}); err != nil {
		return err
	}
	send(Update{Kind: "tool", Text: call.Name})
	start := time.Now()
	c.mu.Lock()
	workdir := conv.workdir
	c.mu.Unlock()
	output, failure := c.execute(call, workdir)
	ref, err := c.cfg.Store.Put(output)
	if err != nil {
		return err
	}
	finished := &store.Event{
		Type:           store.ToolFinished,
		Conversation:   conv.id,
		ToolCall:       call.ID,
		Output:         ref,
		DurationMillis: time.Since(start).Milliseconds(),
	}
	if failure != nil {
		finished.Error = failure.Error()
	}
	return c.record(finished)
}

func (c *Core) execute(call *toolCall, workdir string) ([]byte, error) {
	tool := c.findTool(call.Name)
	if tool == nil {
		return nil, fmt.Errorf("there is no tool named %q", call.Name)
	}
	text, err := c.cfg.Store.Get(call.Arguments)
	if err != nil {
		return nil, err
	}
	args, err := parseArguments(text)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), toolTimeout)
	defer cancel()
	return tool.Run(ctx, Call{Args: args, Workdir: workdir})
}

// Decide answers the open decision. Calls not named in allow are denied, so an
// empty answer denies everything. The model sees text for anything denied, or a default if it's empty.
// What the decision releases then moves along as a reply does.
func (c *Core) Decide(convID, decisionID string, allow []string, text string) (<-chan Update, error) {
	var open decision
	conv, err := c.reserve(convID, func(conv *conversation) error {
		if conv.decision == nil || conv.decision.id != decisionID {
			return ErrNoDecision
		}
		open = *conv.decision
		for _, id := range allow {
			call := conv.tool(id)
			if !slices.Contains(open.calls, id) || call == nil || call.Invalid != "" {
				return ErrBadChoice
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	choices, err := c.choices(conv, &open, allow, text)
	if err == nil {
		err = c.record(&store.Event{Type: store.DecisionMade, Conversation: convID, Decision: decisionID, Choices: choices, By: "user"})
	}
	if err != nil {
		c.release(conv)
		return nil, err
	}
	return c.start(conv), nil
}

func (c *Core) choices(conv *conversation, open *decision, allow []string, text string) ([]store.Choice, error) {
	if strings.TrimSpace(text) == "" {
		text = denialText(open.kind)
	}
	denial, err := c.parts(text)
	if err != nil {
		return nil, err
	}
	var out []store.Choice
	for _, id := range open.calls {
		c.mu.Lock()
		call := *conv.tool(id)
		c.mu.Unlock()
		choice := store.Choice{ToolCall: id, Allow: slices.Contains(allow, id)}
		switch {
		case call.Invalid != "":
			if choice.Parts, err = c.parts("This tool call can't run: " + call.Invalid); err != nil {
				return nil, err
			}
		case !choice.Allow:
			choice.Parts = denial
		case open.kind == store.DecideOutput:
			if choice.Parts, err = c.releasedOutput(&call); err != nil {
				return nil, err
			}
		}
		out = append(out, choice)
	}
	return out, nil
}

// releasedOutput is what the model sees of a call's output. The API's JSON can't
// carry invalid UTF-8, so that's converted on purpose, and the result then points
// to a different block from the output, which records that it happened.
func (c *Core) releasedOutput(call *toolCall) ([]store.Part, error) {
	output, err := c.cfg.Store.Get(call.output)
	if err != nil {
		return nil, err
	}
	parts := []store.Part{{Block: call.output}}
	if !utf8.Valid(output) {
		if parts, err = c.parts(strings.ToValidUTF8(string(output), "�")); err != nil {
			return nil, err
		}
	}
	if call.failure != "" {
		failure, err := c.parts("\n\nThe tool reported an error: " + call.failure)
		if err != nil {
			return nil, err
		}
		parts = append(parts, failure...)
	}
	return parts, nil
}

func (c *Core) toolCallView(conv *conversation, call *toolCall) ToolCallView {
	view := ToolCallView{
		ID:        call.ID,
		Name:      call.Name,
		Arguments: c.text([]store.Part{{Block: call.Arguments}}),
		Invalid:   call.Invalid,
		Flags:     call.Flags,
		Error:     call.failure,
		// Only the newest call with an ID can be the one a decision means.
		Decidable: conv.decision != nil && slices.Contains(conv.decision.calls, call.ID) &&
			conv.tool(call.ID) == call && !call.resolved() && call.Invalid == "",
	}
	if call.finished {
		view.HasOutput = true
		view.Output = c.text([]store.Part{{Block: call.output}})
	}
	if call.resolved() {
		view.Result = c.text(call.result)
	}
	interrupted := conv.decision != nil && conv.decision.kind == store.DecideInterrupted && slices.Contains(conv.decision.calls, call.ID)
	switch {
	case call.released:
		view.State = "sent"
	case call.resolved():
		view.State = "denied"
	case interrupted:
		view.State = "interrupted"
	case call.finished:
		view.State = "finished"
	case call.started:
		view.State = "running"
	case call.allowed:
		view.State = "approved"
	default:
		view.State = "waiting"
	}
	return view
}
