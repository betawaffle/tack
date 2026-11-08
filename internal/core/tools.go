package core

import (
	"bytes"
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"math"
	"reflect"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/betawaffle/tack/internal/openrouter"
	"github.com/betawaffle/tack/internal/store"
	"github.com/betawaffle/tack/internal/workspace"
)

// Tool is something a model can ask to run. Name and Definition are what the
// model sees, and Run is what actually happens, so one can change without the
// other. Definition is the API's whole tool object.
type Tool struct {
	Name       string
	Definition jsontext.Value
	Run        func(ctx context.Context, call Call) ([]byte, error)

	// Define overrides Definition for a tool whose definition depends on settings.
	Define func(store.Settings) jsontext.Value

	// Timeout overrides the default limit on how long a call can take.
	Timeout func(store.Settings) time.Duration

	// NoApproval runs the tool without asking. It's only for tools that reach nothing
	// outside the conversation's own record.
	NoApproval bool

	// AutoRun runs the tool without asking when it says so for a conversation's settings. It's
	// for tools that can't reach anything of a person's, and only ever because they opted in.
	AutoRun func(store.Settings) bool

	// TackWritten says a successful output is tack's own message, not anything read
	// from the system, so there's nothing for a person to vet before the model sees it.
	TackWritten bool

	// AutoSendBytes sends output up to this size to the model without asking, errors
	// included. It's only for tools that can't see anything a person would want to vet,
	// and a person has approved the call by then. Larger output still waits.
	AutoSendBytes int

	// Inspect runs when a call arrives, before any approval, on input the model
	// chose. An error means the call can't run. Flags draw a person's attention on
	// the approval card. It can touch metadata but mustn't open anything.
	Inspect func(call Call) (flags []string, err error)
}

// Call is what a tool gets. Arguments were parsed by parseArguments.
type Call struct {
	Args     map[string]jsontext.Value
	Settings store.Settings

	// Execution identifies this run of the call. It's empty during Inspect.
	Execution string

	// AddNote records a note. It's nil during Inspect.
	AddNote func(kind, key, status, text string) error

	// Keep stores a block beside the output. It's nil during Inspect, which mustn't
	// do anything but look.
	Keep func(name string, data []byte) error

	// Workspace is the files kept from the last call that kept any, and Files is the
	// conversation's own text for code to read, by file name. SaveWorkspace records what
	// to keep from this call. All are empty during Inspect.
	Workspace     []byte
	Files         map[string][]byte
	SaveWorkspace func(data []byte) error

	// Scratch does an operation on the conversation's scratch and returns what the model
	// should see. It's nil during Inspect.
	Scratch func(op, key, text string) (string, error)

	// Context returns the request that led to this call, exactly as it was sent, and the
	// reply that made the call, as JSON. It's nil during Inspect.
	Context func() ([]byte, error)

	// Serve answers what the running code writes to conn, until conn or ctx ends. See
	// serveChannel. It's nil during Inspect.
	Serve func(ctx context.Context, conn io.ReadWriter) error
}

func (t *Tool) define(settings store.Settings) jsontext.Value {
	if t.Define != nil {
		return t.Define(settings)
	}
	return t.Definition
}

// effectiveSettings returns what a conversation uses. One recorded before it kept its
// own takes the current settings, with the model and host of its last call.
func (conv *conversation) effectiveSettings(current store.Settings) store.Settings {
	if conv.settings != nil {
		return *conv.settings
	}
	if conv.lastModel != "" {
		current.Model, current.Host = conv.lastModel, conv.lastHost
	}
	return current
}

// pendingSettings are settings held until someone applies them.
type pendingSettings struct {
	id       string
	settings store.Settings
	// A proposal of tools has their definitions, which are empty for none at all.
	toolsProposed bool
	tools         store.Ref
	custom        []string
}

var (
	errBudgetStop     = errors.New("the budget stopped this call")
	errReplyCancelled = errors.New("You cancelled this reply.")
	errToolStopped    = errors.New("you stopped it")
)

// ToolInfo describes a tool for the page.
type ToolInfo struct {
	Name       string         `json:"name"`
	NoApproval bool           `json:"no_approval,omitzero"`
	Definition jsontext.Value `json:"definition"`
}

// ToolInfo lists the configured tools.
func (c *Core) ToolInfo() []ToolInfo {
	settings := c.currentSettings()
	out := make([]ToolInfo, 0, len(c.cfg.Tools))
	for i := range c.cfg.Tools {
		tool := &c.cfg.Tools[i]
		out = append(out, ToolInfo{Name: tool.Name, NoApproval: tool.NoApproval, Definition: tool.define(settings)})
	}
	return out
}

// FlagUnchecked is the flag on a call whose inspection didn't finish in time, as
// could happen with a path on a hung mount.
const FlagUnchecked = "Couldn't be checked in time"

// inspect runs a tool's Inspect with a time limit. A hung check leaves a goroutine
// behind, which is better than the whole reply hanging on what a model chose.
func inspect(tool *Tool, call Call) ([]string, error) {
	limit := time.Duration(call.Settings.WithDefaults().InspectTimeoutSeconds) * time.Second
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
	case <-time.After(limit):
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
	allowed        bool // approved to run, and not since told to stop
	started        bool
	finished       bool
	output         store.Ref
	failure        string
	workspace      store.Ref    // the files this call left, as an archive
	workspaceFiles store.Ref    // or as a manifest
	result         []store.Part // what the model sees; set once it's final
	released       bool         // the result is the tool's output, not a denial
	automatic      bool         // nobody was asked before it ran
	edited         bool         // someone changed what the model sees
	pending        *edit        // a change to that, held until it's applied or discarded
	channel        []*exchange  // what its code asked tack while it ran
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
	case store.BudgetAdded:
		conv.budgetAdded += picoFromUSD(ev.AmountUSD)
		conv.budgetStopped = false
	case store.AllowanceStarted:
		conv.allowanceSet, conv.allowanceBase, conv.allowance = true, picoFromUSD(ev.SpentUSD), picoFromUSD(ev.AmountUSD)
		conv.freshAllowance = true
		conv.budgetStopped = false
		conv.chainStarts = append(conv.chainStarts, len(conv.items))
	case store.BudgetStopped:
		conv.budgetStopped = true
		conv.stoppedEstimate = picoFromUSD(ev.EstimateUSD)
	case store.PauseRequested:
		conv.paused = true
	case store.PauseCleared:
		conv.paused = false
	case store.NoteAdded:
		if ev.Note != nil {
			conv.noteHistory = append(conv.noteHistory, noteAt{items: len(conv.items), note: ev.Note})
			conv.addNote(ev.Note)
		}
	case store.ScratchChanged:
		if ev.Scratch != nil {
			conv.scratchHistory = append(conv.scratchHistory, scratchAt{items: len(conv.items), change: ev.Scratch})
			conv.applyScratch(ev.Scratch)
		}
	case store.SettingsProposed:
		// A newer proposal replaces an older one. The record keeps both.
		conv.pendingSettings = &pendingSettings{id: ev.Edit, settings: *ev.Settings, toolsProposed: ev.ToolsProposed, tools: ev.Tools, custom: ev.CustomTools}
	case store.SettingsApplied:
		if p := conv.pendingSettings; p != nil && p.id == ev.Edit {
			settings := p.settings
			conv.settings = &settings
			switch {
			case p.toolsProposed:
				conv.tools, conv.custom = p.tools, p.custom
			case ev.Tools != "":
				conv.tools = ev.Tools
			}
			conv.pendingSettings = nil
		}
	case store.SettingsDiscarded:
		if p := conv.pendingSettings; p != nil && p.id == ev.Edit {
			conv.pendingSettings = nil
		}
	case store.DecisionRequested:
		conv.decision = &decision{id: ev.Decision, kind: ev.Kind, calls: ev.DecisionCalls}
	case store.DecisionMade:
		for _, choice := range ev.Choices {
			call := conv.tool(choice.ToolCall)
			if call == nil {
				continue
			}
			call.allowed = choice.Allow
			if choice.Parts == nil {
				call.automatic = ev.By == "tack"
			}
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
	case store.CodeRequest:
		if call := conv.tool(ev.ToolCall); call != nil {
			call.channel = append(call.channel, &exchange{id: ev.Channel, tool: ev.Kind, request: ev.Request})
		}
	case store.CodeResponse:
		if call := conv.tool(ev.ToolCall); call != nil {
			for _, ex := range call.channel {
				if ex.id == ev.Channel {
					ex.response, ex.by = ev.Response, ev.By
				}
			}
		}
	case store.ToolFinished:
		if call := conv.tool(ev.ToolCall); call != nil {
			call.finished, call.output, call.failure = true, ev.Output, ev.Error
			call.workspace, call.workspaceFiles = ev.Workspace, ev.WorkspaceFiles
		}
	}
}

// storeDefinitions writes the offered tools as a block. A conversation keeps
// sending these exact bytes, so changing a tool can't alter its cached prefix or
// what its earlier calls appear to have seen.
func (c *Core) storeDefinitions(settings store.Settings, opts Options) (store.Ref, error) {
	for _, name := range opts.LeaveOut {
		if c.findTool(name) == nil {
			return "", fmt.Errorf("%w: there's no tool called %q", ErrBadTools, name)
		}
	}
	for name := range opts.Definitions {
		if c.findTool(name) == nil {
			return "", fmt.Errorf("%w: there's no tool called %q", ErrBadTools, name)
		}
	}
	var definitions []jsontext.Value
	for i := range c.cfg.Tools {
		tool := &c.cfg.Tools[i]
		if slices.Contains(opts.LeaveOut, tool.Name) {
			continue
		}
		definition := tool.define(settings)
		if edited, ok := opts.Definitions[tool.Name]; ok {
			if !sameExceptDescriptions(definition, edited) {
				return "", fmt.Errorf("%w: only the descriptions and allowed values of %s can change", ErrBadTools, tool.Name)
			}
			definition = edited
		}
		definitions = append(definitions, definition)
	}
	return c.putDefinitions(definitions)
}

func (c *Core) putDefinitions(definitions []jsontext.Value) (store.Ref, error) {
	if len(definitions) == 0 {
		return "", nil
	}
	data, err := json.Marshal(definitions, json.Deterministic(true))
	if err != nil {
		return "", err
	}
	return c.cfg.Store.Put(data)
}

// sameExceptDescriptions says two definitions differ at most in the tool's description,
// its parameters' descriptions, and which values its string parameters list as allowed.
// The rest is what the tool's code relies on.
func sameExceptDescriptions(a, b jsontext.Value) bool {
	strip := func(data jsontext.Value) (map[string]any, bool) {
		var tool map[string]any
		if err := json.Unmarshal(data, &tool); err != nil {
			return nil, false
		}
		function, _ := tool["function"].(map[string]any)
		delete(function, "description")
		parameters, _ := function["parameters"].(map[string]any)
		properties, _ := parameters["properties"].(map[string]any)
		for _, property := range properties {
			if p, ok := property.(map[string]any); ok {
				delete(p, "description")
				// Which values a string can take is the person's to change: tack checks
				// what it gets, whatever the definition said.
				if p["type"] == "string" {
					delete(p, "enum")
				}
			}
		}
		return tool, true
	}
	x, ok := strip(a)
	y, ok2 := strip(b)
	return ok && ok2 && reflect.DeepEqual(x, y)
}

// refreshDefinitions updates a conversation's definitions for new settings. Only a
// definition that depends on settings can change, and only if it's still what those
// settings made: anything else, including a description someone edited, stays as it is.
func (c *Core) refreshDefinitions(block store.Ref, before, after store.Settings) (store.Ref, error) {
	data, err := c.cfg.Store.Get(block)
	if err != nil {
		return "", err
	}
	var definitions []jsontext.Value
	if err := json.Unmarshal(data, &definitions); err != nil {
		return "", err
	}
	names, err := definedNames(data)
	if err != nil {
		return "", err
	}
	for i, name := range names {
		tool := c.findTool(name)
		if tool != nil && tool.Define != nil && bytes.Equal(definitions[i], tool.define(before)) {
			definitions[i] = tool.define(after)
		}
	}
	return c.putDefinitions(definitions)
}

// Definitions returns the exact tool definitions a conversation sends.
func (c *Core) Definitions(convID string) ([]byte, error) {
	c.mu.Lock()
	conv := c.convs[convID]
	var block store.Ref
	if conv != nil {
		block = conv.tools
	}
	c.mu.Unlock()
	if conv == nil {
		return nil, ErrNotFound
	}
	if block == "" {
		return []byte("[]"), nil
	}
	return c.cfg.Store.Get(block)
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
// no automatic reply means no loop a model can spend money on. The exception is a tool
// it was offered that tack no longer has, which failRemovedTools answers.
func (c *Core) storeCalls(definitions []byte, custom []string, settings store.Settings, calls []openrouter.FunctionCallResult) ([]store.ToolCall, error) {
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
		if !slices.Contains(offered, call.Name) {
			stored.Invalid = fmt.Sprintf("there is no tool named %q", call.Name)
		} else if slices.Contains(custom, call.Name) {
			// A person reads it and writes the result, whatever it says.
		} else if tool == nil {
			// failRemovedTools answers it.
		} else if args, err := parseArguments([]byte(call.Arguments)); err != nil {
			stored.Invalid = "the arguments can't be used: " + err.Error()
		} else if flags, err := inspect(tool, Call{Args: args, Settings: settings}); errors.As(err, new(Unsupported)) {
			var unsupported Unsupported
			errors.As(err, &unsupported)
			stored.Unsupported = unsupported.Answer
		} else if err != nil {
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
	if err := c.failRemovedTools(conv); err != nil {
		return false, err
	}
	if err := c.releaseOwnMessages(conv); err != nil {
		return false, err
	}
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

// ToolUnavailable is what the model is told about a tool it was offered that tack no
// longer has. A conversation keeps sending the definitions it started with, so that its
// cache holds, which means a model can always call one.
const ToolUnavailable = "This tool is no longer available, so this call did not run."

// failRemovedTools answers calls to tools tack no longer has, which lets a tool be removed
// without leaving anything behind. If a decision was waiting on one, it opens again for
// whatever's left.
func (c *Core) failRemovedTools(conv *conversation) error {
	c.mu.Lock()
	var calls []string
	for _, call := range conv.openCalls() {
		if c.findTool(call.Name) == nil && call.Invalid == "" && !conv.isCustom(call.Name) {
			calls = append(calls, call.ID)
		}
	}
	c.mu.Unlock()
	if len(calls) == 0 {
		return nil
	}
	parts, err := c.parts(ToolUnavailable)
	if err != nil {
		return err
	}
	var choices []store.Choice
	for _, id := range calls {
		choices = append(choices, store.Choice{ToolCall: id, Parts: parts})
	}
	return c.record(&store.Event{Type: store.DecisionMade, Conversation: conv.id, Choices: choices, By: "tack"})
}

// releaseOwnMessages lets the model see a tool's success message when tack wrote it,
// since there's nothing in it for a person to vet. Errors still stop at the gate. It also
// sends small output from a tool that says that's fine.
func (c *Core) releaseOwnMessages(conv *conversation) error {
	c.mu.Lock()
	var calls []toolCall
	if conv.decision == nil {
		for _, call := range conv.openCalls() {
			tool := c.findTool(call.Name)
			if tool != nil && call.allowed && call.finished && (tool.TackWritten && call.failure == "" || tool.AutoSendBytes > 0) {
				calls = append(calls, *call)
			}
		}
	}
	c.mu.Unlock()
	if len(calls) == 0 {
		return nil
	}
	var choices []store.Choice
	for i := range calls {
		tool := c.findTool(calls[i].Name)
		if !(tool.TackWritten && calls[i].failure == "") {
			if output, err := c.cfg.Store.Get(calls[i].output); err != nil || len(output) > tool.AutoSendBytes {
				continue
			}
		}
		parts, err := c.releasedOutput(&calls[i])
		if err != nil {
			return err
		}
		choices = append(choices, store.Choice{ToolCall: calls[i].ID, Allow: true, Parts: parts})
	}
	if len(choices) == 0 {
		return nil
	}
	return c.record(&store.Event{Type: store.DecisionMade, Conversation: conv.id, Choices: choices, By: "tack"})
}

// advance does what's next: run what was approved, stop at any decision, and
// otherwise call the model.
func (c *Core) advance(conv *conversation, send func(Update)) error {
	justCalled := false
	for {
		if err := c.approveAutomatic(conv); err != nil {
			return err
		}
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
		c.mu.Lock()
		paused := conv.paused
		done := justCalled && c.turnEnded(conv)
		c.mu.Unlock()
		if paused || done {
			return nil
		}
		if err := c.call(conv, send); err != nil {
			if errors.Is(err, errBudgetStop) {
				return nil
			}
			return err
		}
		justCalled = true
	}
}

// turnEnded reports whether the reply that just arrived finishes the turn, which is when
// it made no calls. The caller holds mu.
func (c *Core) turnEnded(conv *conversation) bool {
	for _, it := range slices.Backward(conv.items) {
		if it.role == "" {
			return len(it.calls) == 0
		}
	}
	return true
}

// approveAutomatic allows the calls that don't need a person.
func (c *Core) approveAutomatic(conv *conversation) error {
	c.mu.Lock()
	var choices []store.Choice
	if conv.decision == nil {
		settings := conv.effectiveSettings(c.settings)
		for _, call := range conv.openCalls() {
			tool := c.findTool(call.Name)
			if tool == nil || call.Invalid != "" || call.Unsupported != "" || call.allowed {
				continue
			}
			if tool.NoApproval || (tool.AutoRun != nil && tool.AutoRun(settings)) {
				choices = append(choices, store.Choice{ToolCall: call.ID, Allow: true})
			}
		}
	}
	c.mu.Unlock()
	if len(choices) == 0 {
		return nil
	}
	return c.record(&store.Event{Type: store.DecisionMade, Conversation: conv.id, Choices: choices, By: "tack"})
}

func (c *Core) clearPause(conv *conversation) error {
	c.mu.Lock()
	paused := conv.paused
	c.mu.Unlock()
	if !paused {
		return nil
	}
	return c.record(&store.Event{Type: store.PauseCleared, Conversation: conv.id})
}

// Pause lets whatever is running or approved finish, but doesn't call the model again
// until the next message or Continue, so results go out with what you say next.
func (c *Core) Pause(convID string) error {
	if err := c.exists(convID); err != nil {
		return err
	}
	return c.record(&store.Event{Type: store.PauseRequested, Conversation: convID})
}

// CancelReply closes the connection to the model. Hosts that support it stop processing
// and billing. Others finish and charge in full, so this may save nothing.
func (c *Core) CancelReply(convID string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	conv := c.convs[convID]
	if conv == nil {
		return ErrNotFound
	}
	if conv.cancelReply == nil {
		return ErrNothingRunning
	}
	conv.cancelReply(errReplyCancelled)
	return nil
}

// StopTool asks one running call to stop. A command gets a signal first, then a little
// time to finish cleanly, before it's killed.
func (c *Core) StopTool(convID, callID string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	conv := c.convs[convID]
	if conv == nil {
		return ErrNotFound
	}
	stop := conv.stopTools[callID]
	if stop == nil {
		return ErrNothingRunning
	}
	stop(errToolStopped)
	return nil
}

// runTool runs one approved call. The intent is durable first, so a stop leaves a
// started call with no finish, which is never run again without asking.
func (c *Core) runTool(conv *conversation, call *toolCall, send func(Update)) error {
	execution := store.NewID()
	if err := c.record(&store.Event{Type: store.ToolStarted, Conversation: conv.id, ToolCall: call.ID, Execution: execution}); err != nil {
		return err
	}
	send(Update{Kind: "tool", Text: call.Name, ID: call.ID})
	defer send(Update{Kind: "tool_done", ID: call.ID})
	start := time.Now()
	c.mu.Lock()
	settings := conv.effectiveSettings(c.settings)
	c.mu.Unlock()
	var artifacts []store.Artifact
	ctx, stop := context.WithCancelCause(context.Background())
	defer stop(nil)
	c.mu.Lock()
	if conv.stopTools == nil {
		conv.stopTools = map[string]func(error){}
	}
	conv.stopTools[call.ID] = stop
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		delete(conv.stopTools, call.ID)
		c.mu.Unlock()
	}()
	addNote := func(kind, key, status, text string) error {
		block, err := c.cfg.Store.Put([]byte(text))
		if err != nil {
			return err
		}
		return c.record(&store.Event{Type: store.NoteAdded, Conversation: conv.id, Note: &store.Note{Kind: kind, Key: key, Status: status, Text: block}})
	}
	var workspaceFiles store.Ref
	saveWorkspace := func(data []byte) error {
		manifest, err := workspace.Split(data, math.MaxInt64, func(file []byte) (string, error) {
			ref, err := c.cfg.Store.Put(file)
			return string(ref), err
		})
		if err != nil {
			return err
		}
		workspaceFiles, err = c.cfg.Store.Put(manifest)
		return err
	}
	kept, files, err := c.codeState(conv)
	if err != nil {
		return err
	}
	scratch := func(op, key, text string) (string, error) { return c.scratchOperation(conv, op, key, text) }
	callContext := func() ([]byte, error) { return c.callContext(conv, call) }
	output, failure := c.execute(ctx, conv, call, Call{Settings: settings, Execution: execution, AddNote: addNote, Workspace: kept, Files: files, SaveWorkspace: saveWorkspace, Scratch: scratch, Context: callContext}, func(name string, data []byte) error {
		block, err := c.cfg.Store.Put(data)
		if err != nil {
			return err
		}
		artifacts = append(artifacts, store.Artifact{Name: name, Block: block})
		return nil
	}, send)
	ref, err := c.cfg.Store.Put(output)
	if err != nil {
		return err
	}
	finished := &store.Event{
		Type:           store.ToolFinished,
		Conversation:   conv.id,
		ToolCall:       call.ID,
		Execution:      execution,
		Output:         ref,
		Artifacts:      artifacts,
		WorkspaceFiles: workspaceFiles,
		DurationMillis: time.Since(start).Milliseconds(),
	}
	if failure != nil {
		finished.Error = failure.Error()
	}
	if err := c.record(finished); err != nil {
		return err
	}
	// Whatever's next shouldn't start into the same shortage by itself.
	if errors.Is(failure, ErrMachineShort) {
		return c.record(&store.Event{Type: store.PauseRequested, Conversation: conv.id})
	}
	return nil
}

// ErrMachineShort is a tool's failure when the whole machine ran short of memory, rather than
// the tool reaching its own limit. The conversation pauses, even if it runs code by itself.
var ErrMachineShort = errors.New("the machine ran short of memory, so the kernel stopped this run, and the conversation is paused until the user continues it")

func (c *Core) execute(parent context.Context, conv *conversation, call *toolCall, run Call, keep func(string, []byte) error, send func(Update)) ([]byte, error) {
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
	// Time passes between approving a call and running it, and an earlier call in
	// the batch may have changed things. If it no longer looks as it did on the card,
	// the approval doesn't cover it.
	run.Args = args
	flags, err := inspect(tool, Call{Args: args, Settings: run.Settings})
	if err != nil {
		return nil, fmt.Errorf("this changed since it was approved: %w", err)
	}
	if !slices.Equal(flags, call.Flags) {
		return nil, fmt.Errorf("this changed since it was approved: the card showed %q, and now it's %q", call.Flags, flags)
	}
	timeout := time.Duration(run.Settings.WithDefaults().ToolTimeoutSeconds) * time.Second
	if tool.Timeout != nil {
		timeout = tool.Timeout(run.Settings)
	}
	ctx, limit := pausableTimeout(parent, timeout, fmt.Errorf("it ran past its limit of %s", timeout))
	defer limit.stop()
	run.Keep = keep
	served := run
	run.Serve = func(ctx context.Context, conn io.ReadWriter) error {
		return c.serveChannel(ctx, conn, conv, call, served, limit.hold, send)
	}
	return tool.Run(ctx, run)
}

// Reconfigure changes what new conversations start with. Existing ones keep their own.
func (c *Core) Reconfigure(settings store.Settings) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.settings = settings
}

func (c *Core) currentSettings() store.Settings {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.settings
}

// RecordConfig notes the settings in force.
func (c *Core) RecordConfig(data []byte) error {
	block, err := c.cfg.Store.Put(data)
	if err != nil {
		return err
	}
	return c.record(&store.Event{Type: store.ConfigLoaded, Config: block})
}

// ProposeSettings holds new settings for a conversation. Nothing changes until they're
// applied.
func (c *Core) ProposeSettings(convID string, settings store.Settings) (string, error) {
	if err := c.exists(convID); err != nil {
		return "", err
	}
	settings = settings.WithDefaults()
	if err := settings.Validate(); err != nil {
		return "", fmt.Errorf("%w: %v", ErrBadSettings, err)
	}
	id := store.NewID()
	return id, c.record(&store.Event{Type: store.SettingsProposed, Conversation: convID, Edit: id, Settings: &settings})
}

func (c *Core) isCompact(name string) bool {
	tool := c.findTool(name)
	return tool != nil && tool.NoApproval
}

func nothingWaitingDiffers(a, b store.Settings) bool {
	a, b = a.WithDefaults(), b.WithDefaults()
	a.BudgetUSD, a.BudgetTopUpUSD = b.BudgetUSD, b.BudgetTopUpUSD
	a.AutoRunCode = b.AutoRunCode
	a.Host, a.ReasoningEffort = b.Host, b.ReasoningEffort
	return reflect.DeepEqual(a, b)
}

// ApplySettings makes held settings the conversation's own. Tool definitions come
// first in every request, so if they change, the next call misses the whole cache.
func (c *Core) ApplySettings(convID, id string) error {
	var pending, before store.Settings
	var current store.Ref
	var proposedTools bool
	conv, err := c.reserve(convID, func(conv *conversation) error {
		if conv.pendingSettings == nil || conv.pendingSettings.id != id {
			return ErrNoEdit
		}
		pending = conv.pendingSettings.settings
		proposedTools = conv.pendingSettings.toolsProposed
		before, current = conv.effectiveSettings(c.settings), conv.tools
		// A budget, or whether to ask next time, has nothing to do with whatever's waiting.
		waiting := conv.decision != nil || conv.unanswered()
		if waiting && (proposedTools || !nothingWaitingDiffers(before, pending)) {
			return ErrAwaiting
		}
		return nil
	})
	if err != nil {
		return err
	}
	defer c.release(conv)
	var tools store.Ref
	if current != "" && !proposedTools {
		if tools, err = c.refreshDefinitions(current, before, pending); err != nil {
			return err
		}
	}
	return c.record(&store.Event{Type: store.SettingsApplied, Conversation: convID, Edit: id, Tools: tools})
}

// DiscardSettings drops held settings.
func (c *Core) DiscardSettings(convID, id string) error {
	c.mu.Lock()
	conv := c.convs[convID]
	found := conv != nil && conv.pendingSettings != nil && conv.pendingSettings.id == id
	c.mu.Unlock()
	if conv == nil {
		return ErrNotFound
	}
	if !found {
		return ErrNoEdit
	}
	return c.record(&store.Event{Type: store.SettingsDiscarded, Conversation: convID, Edit: id})
}

// Decide answers the open decision. Calls not named in allow are denied, so an
// empty answer denies everything. The model sees text for anything denied, or a default if it's empty.
// What the decision releases then moves along as a reply does.
func (c *Core) Decide(convID, decisionID string, allow []string, text string) (<-chan Update, error) {
	return c.DecideWith(convID, decisionID, Decision{Allow: allow, Text: text})
}

// Decision is an answer to what a conversation is waiting on. Shorten sends only the start
// and end of those calls' output, for ones that are allowed. AutoRunCode also stops asking
// about code in this conversation from here on.
type Decision struct {
	Allow, Shorten []string
	Text           string
	AutoRunCode    bool
	// Results answer calls to custom tools, by call ID. The model is given exactly this
	// text. A call to one that's allowed has to have a result.
	Results map[string]string
}

// DecideWith answers a decision.
func (c *Core) DecideWith(convID, decisionID string, d Decision) (<-chan Update, error) {
	allow := d.Allow
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
		for _, id := range allow {
			if call := conv.tool(id); conv.byHand(call) {
				if _, ok := d.Results[id]; !ok {
					return ErrBadChoice
				}
			}
		}
		for id := range d.Results {
			call := conv.tool(id)
			if !slices.Contains(allow, id) || call == nil || !conv.byHand(call) || open.kind != store.DecideCalls {
				return ErrBadChoice
			}
		}
		for _, id := range d.Shorten {
			if !slices.Contains(allow, id) || open.kind != store.DecideOutput {
				return ErrBadChoice
			}
		}
		// It would replace a change someone's holding.
		if d.AutoRunCode && conv.pendingSettings != nil {
			return ErrBadChoice
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	choices, err := c.choices(conv, &open, d)
	if err == nil && d.AutoRunCode {
		err = c.stopAskingAboutCode(conv)
	}
	if err == nil {
		err = c.record(&store.Event{Type: store.DecisionMade, Conversation: convID, Decision: decisionID, Choices: choices, By: "user"})
	}
	if err != nil {
		c.release(conv)
		return nil, err
	}
	return c.start(conv), nil
}

func (c *Core) stopAskingAboutCode(conv *conversation) error {
	c.mu.Lock()
	settings := conv.effectiveSettings(c.settings).WithDefaults()
	c.mu.Unlock()
	settings.AutoRunCode = true
	id := store.NewID()
	if err := c.record(&store.Event{Type: store.SettingsProposed, Conversation: conv.id, Edit: id, Settings: &settings}); err != nil {
		return err
	}
	return c.record(&store.Event{Type: store.SettingsApplied, Conversation: conv.id, Edit: id})
}

func (c *Core) choices(conv *conversation, open *decision, d Decision) ([]store.Choice, error) {
	allow, text := d.Allow, d.Text
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
		case open.kind == store.DecideCalls && d.Results != nil && hasKey(d.Results, id):
			if choice.Parts, err = c.parts(d.Results[id]); err != nil {
				return nil, err
			}
		case open.kind == store.DecideOutput:
			if slices.Contains(d.Shorten, id) {
				choice.Parts, err = c.shortenedOutput(&call)
			} else {
				choice.Parts, err = c.releasedOutput(&call)
			}
			if err != nil {
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

// ShortenedBytes is how much of each end of a call's output "shorten" keeps.
const ShortenedBytes = 2048

// shortenedOutput is a call's output with its middle taken out, marked so the model knows.
func (c *Core) shortenedOutput(call *toolCall) ([]store.Part, error) {
	output, err := c.cfg.Store.Get(call.output)
	if err != nil {
		return nil, err
	}
	if len(output) <= 2*ShortenedBytes {
		return c.releasedOutput(call)
	}
	first := strings.ToValidUTF8(string(output[:ShortenedBytes]), "")
	last := strings.ToValidUTF8(string(output[len(output)-ShortenedBytes:]), "")
	text := fmt.Sprintf("%s\n\n[... the user left out %d bytes here ...]\n\n%s", first, len(output)-2*ShortenedBytes, last)
	if call.failure != "" {
		text += "\n\nThe tool reported an error: " + call.failure
	}
	return c.parts(text)
}

func (c *Core) toolCallView(conv *conversation, call *toolCall) ToolCallView {
	view := ToolCallView{
		ID:           call.ID,
		Name:         call.Name,
		Arguments:    c.text([]store.Part{{Block: call.Arguments}}),
		Invalid:      call.Invalid,
		Flags:        call.Flags,
		Error:        call.failure,
		Compact:      c.isCompact(call.Name),
		Custom:       conv.isCustom(call.Name),
		ByHand:       conv.byHand(call),
		Unsupported:  call.Unsupported,
		Channel:      c.channelViews(call),
		ResultEdited: call.edited,
		// Only the newest call with an ID can be the one a decision means.
		Decidable: conv.decision != nil && slices.Contains(conv.decision.calls, call.ID) &&
			conv.tool(call.ID) == call && !call.resolved() && call.Invalid == "",
	}
	if call.pending != nil {
		view.Pending = &PendingEdit{ID: call.pending.id, Text: c.text(call.pending.parts)}
	}
	if call.finished {
		view.HasOutput = true
		view.Output = c.text([]store.Part{{Block: call.output}})
	}
	if call.resolved() {
		view.Result = c.text(call.result)
	}
	if tool := c.findTool(call.Name); tool != nil && tool.AutoRun != nil && call.Unsupported == "" {
		view.RanAutomatically = call.automatic
		view.CanAutoRun = view.Decidable && conv.decision.kind == store.DecideCalls && conv.pendingSettings == nil
	}
	interrupted := conv.decision != nil && conv.decision.kind == store.DecideInterrupted && slices.Contains(conv.decision.calls, call.ID)
	switch {
	case call.released:
		view.State = "sent"
	case view.Result == ToolUnavailable:
		view.State = "unavailable"
	case call.resolved():
		view.State = "denied"
	case interrupted:
		view.State = "interrupted"
	case call.finished:
		view.State = "finished"
		view.OutputTokens = countOutput(conv.effectiveSettings(c.settings).Model, view.Output)
	case call.started:
		view.State = "running"
	case call.allowed:
		view.State = "approved"
	default:
		view.State = "waiting"
	}
	return view
}
