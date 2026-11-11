package core

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"math"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/betawaffle/tack/internal/store"
	"github.com/betawaffle/tack/internal/tokens"
	"github.com/betawaffle/tack/internal/workspace"
)

// ErrNoAgentForDraft is a run asked of a draft that doesn't name an agent to run as.
var ErrNoAgentForDraft = fmt.Errorf("%w: pick an agent for the draft first; its tools run as that agent", ErrBadRequest)

// ErrCallChanged is a run asked for with flags the call doesn't have now.
var ErrCallChanged = errors.New("this call isn't what it was when it was looked at")

// ErrAgentBusy is a run asked of an agent that's already running one.
var ErrAgentBusy = fmt.Errorf("%w: the agent is already running a tool", ErrBusy)

// draftCall is one of a draft's tool calls: the model's message that made it, and the call.
type draftCall struct {
	draft     *store.Draft
	request   map[string]jsontext.Value
	messages  []map[string]jsontext.Value
	reply     jsontext.Value // the message the call is in
	result    int            // the index of the message that answers it
	name      string
	arguments string
}

// findDraftCall finds call id in a draft, and the message that answers it.
func findDraftCall(d *store.Draft, id string) (*draftCall, error) {
	out := &draftCall{draft: d, result: -1}
	if err := json.Unmarshal(d.Request, &out.request); err != nil {
		return nil, fmt.Errorf("%w: reading the request: %v", ErrBadRequest, err)
	}
	if err := json.Unmarshal(out.request["messages"], &out.messages); err != nil {
		return nil, fmt.Errorf("%w: reading the messages: %v", ErrBadRequest, err)
	}
	type call struct {
		ID       string `json:"id"`
		Function struct {
			Name      string `json:"name"`
			Arguments string `json:"arguments"`
		} `json:"function"`
	}
	// An older draft's IDs can repeat, so the call is the last one with that ID that something
	// answers, and its result the first answer after it.
	for i, m := range out.messages {
		var calls []call
		if raw, ok := m["tool_calls"]; ok && json.Unmarshal(raw, &calls) == nil {
			for _, c := range calls {
				if c.ID == id {
					out.reply, _ = json.Marshal(m, json.Deterministic(true))
					out.name, out.arguments = c.Function.Name, c.Function.Arguments
					out.result = -1
				}
			}
		}
		var answers string
		if raw, ok := m["tool_call_id"]; ok && json.Unmarshal(raw, &answers) == nil && answers == id && out.name != "" && out.result < 0 {
			out.result = i
		}
	}
	if out.name == "" {
		return nil, fmt.Errorf("%w: the draft has no tool call %q", ErrNotFound, id)
	}
	if out.result < 0 {
		return nil, fmt.Errorf("%w: nothing in the draft answers tool call %q", ErrBadRequest, id)
	}
	return out, nil
}

// setResult puts text in the message that answers the call, and gives back the request.
func (dc *draftCall) setResult(text string) (jsontext.Value, error) {
	content, err := json.Marshal(text)
	if err != nil {
		return nil, err
	}
	dc.messages[dc.result]["content"] = content
	raw, err := json.Marshal(dc.messages, json.Deterministic(true))
	if err != nil {
		return nil, err
	}
	dc.request["messages"] = raw
	return json.Marshal(dc.request, json.Deterministic(true))
}

// DraftCallFlags are what the approval card would point out about one of a draft's tool
// calls, for the person to see before they run it.
func (c *Core) DraftCallFlags(draftID, callID string) ([]string, error) {
	d, err := c.cfg.Store.LoadDraft(draftID)
	if err != nil {
		return nil, err
	}
	dc, err := findDraftCall(d, callID)
	if err != nil {
		return nil, err
	}
	return c.ToolFlags(dc.name, dc.arguments)
}

// ToolFlags are what the approval card would point out about a call to a tool, by its name and
// arguments.
func (c *Core) ToolFlags(name, arguments string) ([]string, error) {
	tool := c.findTool(name)
	if tool == nil {
		return nil, fmt.Errorf("%w: tack has no tool called %q to run", ErrBadRequest, name)
	}
	args, err := parseArguments([]byte(arguments))
	if err != nil {
		return nil, fmt.Errorf("%w: the arguments can't be used: %v", ErrBadRequest, err)
	}
	flags, err := inspect(tool, Call{Args: args, Settings: c.StartSettings(nil).WithDefaults()})
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrBadRequest, err)
	}
	return flags, nil
}

// DraftRun is what running one of a draft's calls did.
type DraftRun struct {
	// Result is what the model will see, which is now in the draft.
	Result  string `json:"result"`
	Failure string `json:"failure,omitzero"`
}

// RunDraftCall runs one of a draft's tool calls as the draft's agent, with its /work and
// scratch store, which it leaves changed. flags are what the person saw before asking for
// it, which is the approval: if the call doesn't look like that now, it doesn't run. What the
// model will see goes in the message that answers the call, replacing what was there.
func (c *Core) RunDraftCall(draftID, callID string, flags []string) (DraftRun, error) {
	d, err := c.cfg.Store.LoadDraft(draftID)
	if err != nil {
		return DraftRun{}, err
	}
	if d.Sent != "" {
		return DraftRun{}, ErrDraftSent
	}
	if d.Agent == "" {
		return DraftRun{}, ErrNoAgentForDraft
	}
	dc, err := findDraftCall(d, callID)
	if err != nil {
		return DraftRun{}, err
	}
	if c.findTool(dc.name) == nil {
		return DraftRun{}, fmt.Errorf("%w: tack has no tool called %q to run", ErrBadRequest, dc.name)
	}
	var offered struct {
		Tools jsontext.Value `json:"tools"`
	}
	json.Unmarshal(d.Request, &offered)
	if names, _ := definedNames(offered.Tools); !slices.Contains(names, dc.name) {
		return DraftRun{}, fmt.Errorf("%w: the draft doesn't offer %q", ErrBadRequest, dc.name)
	}
	// What the person saw has to be what it is now, or they didn't approve this.
	now, err := c.DraftCallFlags(draftID, callID)
	if err != nil {
		return DraftRun{}, err
	}
	if !slices.Equal(now, flags) {
		return DraftRun{}, fmt.Errorf("%w: it now has %q pointed out, not %q, so look again before running it", ErrCallChanged, now, flags)
	}

	// One run at a time for each agent, so its /work never has two writers.
	c.mu.Lock()
	a := c.agents[d.Agent]
	switch {
	case a == nil:
		c.mu.Unlock()
		return DraftRun{}, ErrNoAgent
	case a.running:
		c.mu.Unlock()
		return DraftRun{}, ErrAgentBusy
	}
	a.running = true
	state := a.state()
	ctx, stop := context.WithCancelCause(context.Background())
	defer stop(nil)
	if c.draftRuns == nil {
		c.draftRuns = map[string]func(error){}
	}
	runKey := draftID + "/" + callID
	c.draftRuns[runKey] = stop
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		a.running = false
		delete(c.draftRuns, runKey)
		c.mu.Unlock()
	}()

	arguments, err := c.cfg.Store.Put([]byte(dc.arguments))
	if err != nil {
		return DraftRun{}, err
	}
	call := &toolCall{ToolCall: store.ToolCall{ID: callID, Name: dc.name, Arguments: arguments, Flags: flags}}
	scope := runScope{agent: d.Agent, draft: d.ID, definitions: offered.Tools}
	output, failure, err := c.runAsAgent(ctx, scope, call, state, dc)
	if err != nil {
		return DraftRun{}, err
	}
	run := DraftRun{Result: resultText(output, failure)}
	if failure != nil {
		run.Failure = failure.Error()
	}
	request, err := dc.setResult(run.Result)
	if err != nil {
		return DraftRun{}, err
	}
	// The draft may have changed while it ran, so only the result is put in what's there now.
	if now, err := c.cfg.Store.LoadDraft(draftID); err == nil && now.Sent == "" {
		if again, err := findDraftCall(now, callID); err == nil {
			if request, err = again.setResult(run.Result); err != nil {
				return DraftRun{}, err
			}
		}
	}
	// A draft for a model tack can't draw is saved all the same.
	if _, err := c.saveDraft(draftID, request, nil, nil); err != nil && !errors.Is(err, tokens.ErrNoRenderer) {
		return run, err
	}
	return run, nil
}

// StopDraftRun stops a draft's tool call that's running. It finishes as stopped, with what it
// had written so far as its result.
func (c *Core) StopDraftRun(draftID, callID string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	stop := c.draftRuns[draftID+"/"+callID]
	if stop == nil {
		return ErrNothingRunning
	}
	stop(errToolStopped)
	return nil
}

// runAsAgent runs call with the agent's state, records the run, and sets the agent's state to
// what it left.
func (c *Core) runAsAgent(ctx context.Context, scope runScope, call *toolCall, state AgentState, dc *draftCall) ([]byte, error, error) {
	execution := store.NewID()
	if err := c.record(scope.stamp(&store.Event{Type: store.ToolStarted, ToolCall: call.ID, Execution: execution})); err != nil {
		return nil, nil, err
	}
	start := time.Now()
	settings := c.StartSettings(nil).WithDefaults()

	// Its /work, and its scratch store, which is changed in place as the run goes.
	var kept []byte
	if state.Workspace != "" {
		manifest, err := c.cfg.Store.Get(state.Workspace)
		if err != nil {
			return nil, nil, err
		}
		if kept, err = workspace.Join(manifest, func(blob string) ([]byte, error) { return c.cfg.Store.Get(store.Ref(blob)) }); err != nil {
			return nil, nil, err
		}
	}
	scratch, err := c.scratchOf(state.Scratch)
	if err != nil {
		return nil, nil, err
	}
	files := map[string][]byte{}
	if files["scratch.json"], err = json.Marshal(scratch); err != nil {
		return nil, nil, err
	}
	current := state
	setState := func(next AgentState) error {
		if next.Workspace == current.Workspace && next.Scratch == current.Scratch {
			return nil
		}
		current = next
		return c.record(&store.Event{Type: store.AgentStateSet, Agent: scope.agent, WorkspaceFiles: next.Workspace, ScratchFiles: next.Scratch, Execution: execution})
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
	scratchOp := func(op, key, text string) (string, error) {
		answer, changed, err := scratchChange(scratch, op, key, text)
		if err != nil || !changed {
			return answer, err
		}
		ref, err := c.putScratch(scratch)
		if err != nil {
			return "", err
		}
		next := current
		next.Scratch = ref
		return answer, setState(next)
	}
	callContext := func() ([]byte, error) { return c.draftCallContext(dc) }

	var artifacts []store.Artifact
	output, failure := c.execute(ctx, scope, call, Call{Settings: settings, Execution: execution, Workspace: kept, Files: files, SaveWorkspace: saveWorkspace, Scratch: scratchOp, Context: callContext},
		func(name string, data []byte) error {
			block, err := c.cfg.Store.Put(data)
			if err != nil {
				return err
			}
			artifacts = append(artifacts, store.Artifact{Name: name, Block: block})
			return nil
		}, func(Update) {})
	ref, err := c.cfg.Store.Put(output)
	if err != nil {
		return nil, nil, err
	}
	finished := scope.stamp(&store.Event{Type: store.ToolFinished, ToolCall: call.ID, Execution: execution, Output: ref,
		Artifacts: artifacts, WorkspaceFiles: workspaceFiles, DurationMillis: time.Since(start).Milliseconds()})
	if failure != nil {
		finished.Error = failure.Error()
	}
	if err := c.record(finished); err != nil {
		return nil, nil, err
	}
	if workspaceFiles != "" {
		next := current
		next.Workspace = workspaceFiles
		if err := setState(next); err != nil {
			return nil, nil, err
		}
	}
	return output, failure, nil
}

// resultText is what the model sees of a run's output, as releasedOutput makes it.
func resultText(output []byte, failure error) string {
	text := string(output)
	if !utf8.Valid(output) {
		text = strings.ToValidUTF8(text, "�")
	}
	if failure != nil {
		text += "\n\nThe tool reported an error: " + failure.Error()
	}
	return text
}

// An agent's scratch store is kept as a manifest: a JSON object of each key's block.
func (c *Core) scratchOf(manifest store.Ref) (map[string]string, error) {
	out := map[string]string{}
	if manifest == "" {
		return out, nil
	}
	data, err := c.cfg.Store.Get(manifest)
	if err != nil {
		return nil, err
	}
	var refs map[string]store.Ref
	if err := json.Unmarshal(data, &refs); err != nil {
		return nil, err
	}
	for key, ref := range refs {
		text, err := c.cfg.Store.Get(ref)
		if err != nil {
			return nil, err
		}
		out[key] = string(text)
	}
	return out, nil
}

func (c *Core) putScratch(scratch map[string]string) (store.Ref, error) {
	if len(scratch) == 0 {
		return "", nil
	}
	refs := map[string]store.Ref{}
	for key, text := range scratch {
		ref, err := c.cfg.Store.Put([]byte(text))
		if err != nil {
			return "", err
		}
		refs[key] = ref
	}
	data, err := json.Marshal(refs, json.Deterministic(true))
	if err != nil {
		return "", err
	}
	return c.cfg.Store.Put(data)
}

// scratchChange does an operation on a scratch store held in memory, as scratchOperation does
// on a conversation's, and says whether it changed it.
func scratchChange(scratch map[string]string, op, key, text string) (string, bool, error) {
	current, ok := scratch[key]
	switch op {
	case "read":
		if !ok {
			return "There's nothing under that key.", false, nil
		}
		return current, false, nil
	case "list":
		var out strings.Builder
		keys := make([]string, 0, len(scratch))
		for k := range scratch {
			keys = append(keys, k)
		}
		slices.Sort(keys)
		for _, k := range keys {
			fmt.Fprintf(&out, "%s: %d bytes\n", k, len(scratch[k]))
		}
		if out.Len() == 0 {
			return "There's nothing here yet.", false, nil
		}
		return out.String(), false, nil
	case "delete":
		if !ok {
			return "There's nothing under that key.", false, nil
		}
		delete(scratch, key)
		return "ok", true, nil
	case "append":
		text = current + text
	}
	scratch[key] = text
	return "ok", true, nil
}

// draftCallContext is a draft's call's context, as callContext gives a conversation's: the
// request of the call the draft follows on from, if it does, and the message that made it.
func (c *Core) draftCallContext(dc *draftCall) ([]byte, error) {
	body := []byte("null")
	if dc.draft.From != "" {
		if before, err := c.requestOf(dc.draft.From); err == nil {
			body = before.Request
		}
	}
	out := append([]byte(`{"request":`), body...)
	out = append(out, `,"reply":`...)
	out = append(out, dc.reply...)
	return append(out, "}"...), nil
}
