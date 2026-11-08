package core

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"reflect"
	"slices"
	"strings"
	"time"

	"github.com/betawaffle/tack/internal/store"
	"github.com/betawaffle/tack/internal/tokens"
)

// Summary describes a conversation in a list.
type Summary struct {
	ID      string    `json:"id"`
	Created time.Time `json:"created"`
	Preview string    `json:"preview"`
	Busy    bool      `json:"busy"`
}

// Stats are the facts about a call worth showing next to its reply.
type Stats struct {
	Host             string `json:"host,omitzero"`
	PromptTokens     int    `json:"prompt_tokens"`
	CachedTokens     int    `json:"cached_tokens"`
	CompletionTokens int    `json:"completion_tokens"`
	ReasoningTokens  int    `json:"reasoning_tokens"`
	CostPico         Pico   `json:"cost_pico"`
	TTFTMillis       int64  `json:"ttft_ms"`
	DurationMillis   int64  `json:"duration_ms"`
}

// PendingEdit is a system message edit that's being held. An empty Text
// proposes removing the message.
type PendingEdit struct {
	ID   string `json:"id"`
	Text string `json:"text"`

	Name      string        `json:"name,omitzero"`
	Reasoning string        `json:"reasoning,omitzero"`
	ToolCalls []MessageCall `json:"tool_calls,omitzero"`
}

// Item is something to show in a conversation. Text holds the message, or for
// the "notice" role, what went wrong.
type Item struct {
	// Index is its place among the conversation's items, which is what Fork counts.
	Index     int    `json:"index"`
	Role      string `json:"role"`
	Text      string `json:"text"`
	Reasoning string `json:"reasoning,omitzero"`
	Stats     *Stats `json:"stats,omitzero"`

	// A message's name field.
	Name string `json:"name,omitzero"`
	// Written says someone wrote this as a message of its role. ToolCallID says which
	// call a written tool message answers, and WrittenCalls are ones it asks for.
	Written      bool          `json:"written,omitzero"`
	ToolCallID   string        `json:"tool_call_id,omitzero"`
	WrittenCalls []MessageCall `json:"written_calls,omitzero"`
	// Call identifies a reply from the model, for changing it.
	Call string `json:"call,omitzero"`

	// User and system messages: no call has included this text yet.
	Queued bool `json:"queued,omitzero"`

	// System and written messages, and Edited and Pending for replies too.
	Message string       `json:"message,omitzero"`
	Edited  bool         `json:"edited,omitzero"`
	Removed bool         `json:"removed,omitzero"`
	Pending *PendingEdit `json:"pending,omitzero"`

	// Replies that made tool calls.
	ToolCalls []ToolCallView `json:"tool_calls,omitzero"`

	// The last call since someone set things going: what those calls cost in all.
	ChainPico  Pico `json:"chain_pico,omitzero"`
	ChainCalls int  `json:"chain_calls,omitzero"`
}

// ToolCallView is a tool call as the UI shows it. Arguments are the exact text
// that was requested, which is also what runs. State is "waiting", "approved",
// "running", "interrupted", "finished", "sent" or "denied".
type ToolCallView struct {
	ID        string   `json:"id"`
	Name      string   `json:"name"`
	Arguments string   `json:"arguments"`
	Invalid   string   `json:"invalid,omitzero"`
	Flags     []string `json:"flags,omitzero"`
	State     string   `json:"state"`
	Output    string   `json:"output,omitzero"`
	HasOutput bool     `json:"has_output,omitzero"`
	Error     string   `json:"error,omitzero"`
	Result    string   `json:"result,omitzero"` // what the model was given
	Decidable bool     `json:"decidable,omitzero"`
	// Compact says it's only bookkeeping, like a note, which doesn't need a full card.
	Compact bool `json:"compact,omitzero"`
	// Custom says it's a call to a tool the person defined, so they write its result.
	Custom bool `json:"custom,omitzero"`
	// Channel is what its code asked tack while it ran, in order.
	Channel []ChannelView `json:"channel,omitzero"`

	// OutputTokens is what the output would add to the next call, while it's waiting on someone
	// deciding whether to send it. Zero means it can't be counted.
	OutputTokens int `json:"output_tokens,omitzero"`

	// RanAutomatically says nobody was asked, because the conversation opted in. CanAutoRun says
	// someone answering this call could opt in now.
	RanAutomatically bool `json:"ran_automatically,omitzero"`
	CanAutoRun       bool `json:"can_auto_run,omitzero"`
	// Someone changed what the model was given, or has a change held.
	ResultEdited bool         `json:"result_edited,omitzero"`
	Pending      *PendingEdit `json:"pending,omitzero"`
}

// BudgetView is a conversation's spending. Stopped says a call wasn't started because
// of it, and then Estimate is what that call looked like it would cost. TopUp is what
// the add box starts with. The period totals are for the current billing period.
type BudgetView struct {
	Limit      Pico `json:"limit_pico"`
	Spent      Pico `json:"spent_pico"`
	Estimate   Pico `json:"estimate_pico"`
	TopUp      Pico `json:"top_up_pico"`
	Stopped    bool `json:"stopped,omitzero"`
	PeriodHere Pico `json:"period_here_pico"`
	PeriodAll  Pico `json:"period_all_pico"`
	// EstimateKnown says Estimate is the least the next call could cost, if nothing's
	// added. Without prices or a way to count tokens there's no estimate. Allowance is
	// what someone setting things going next would allow.
	EstimateKnown bool `json:"estimate_known,omitzero"`
	NextBytes     int  `json:"next_bytes"`
	// NextTokens is the input tokens of the next call if nothing's added, for a model tack
	// can count for. Zero means it can't.
	NextTokens int  `json:"next_tokens,omitzero"`
	Allowance  Pico `json:"allowance_pico"`
}

// NoteView is an item on the page, written by the model.
type NoteView struct {
	Kind   string `json:"kind"`
	Key    string `json:"key"`
	Status string `json:"status,omitzero"`
	Text   string `json:"text"`
}

// RunningView says what can be stopped right now.
type RunningView struct {
	Reply bool   `json:"reply,omitzero"`
	Tool  string `json:"tool,omitzero"` // the call that's running
}

// PendingSettings are settings being held until someone applies them.
type PendingSettings struct {
	ID       string         `json:"id"`
	Settings store.Settings `json:"settings"`
	// Tools says it changes the tools, to Definitions, which are exact.
	Tools       bool           `json:"tools,omitzero"`
	Definitions jsontext.Value `json:"definitions,omitzero"`
	CustomTools []string       `json:"custom_tools,omitzero"`
}

// DecisionView is what's waiting on a person. DenialText is the default for what
// the model sees about anything denied.
type DecisionView struct {
	ID         string   `json:"id"`
	Kind       string   `json:"kind"`
	Calls      []string `json:"calls"`
	DenialText string   `json:"denial_text"`
}

// Conversation is a conversation as the UI shows it.
type Conversation struct {
	ID string `json:"id"`
	// Revision changes with everything recorded in it. An action can name the one it
	// was based on, and it's refused if that's no longer the current one.
	Revision int    `json:"revision"`
	Model    string `json:"model"`
	Host     string `json:"host,omitzero"` // the pinned host, if any
	Busy     bool   `json:"busy"`
	CanReply bool   `json:"can_reply"`
	Tools    bool   `json:"tools,omitzero"`
	Workdir  string `json:"workdir,omitzero"`
	// CustomTools are the tools it offers that a person answers.
	CustomTools []string `json:"custom_tools,omitzero"`
	// Settings are what it uses, and ConfigSettings what a new conversation would
	// start with. SettingsChanged says they differ, and PendingSettings are held.
	Settings        store.Settings   `json:"settings"`
	ConfigSettings  store.Settings   `json:"config_settings"`
	SettingsChanged bool             `json:"settings_changed,omitzero"`
	PendingSettings *PendingSettings `json:"pending_settings,omitzero"`
	Items           []Item           `json:"items"`

	Decision *DecisionView `json:"decision,omitzero"`

	// WaitingToolCalls are written tool calls that only a tool message can answer.
	WaitingToolCalls []string `json:"waiting_tool_calls,omitzero"`
	// Refused is what the current model and host won't accept, as far as is known.
	Refused []RefusedFeature `json:"refused,omitzero"`

	// Where it was forked from, and how many of its items came from there.
	ForkedFrom string `json:"forked_from,omitzero"`
	ForkItems  int    `json:"fork_items,omitzero"`

	Budget  BudgetView  `json:"budget"`
	Paused  bool        `json:"paused,omitzero"`
	Running RunningView `json:"running"`
	Notes   []NoteView  `json:"notes,omitzero"`
	Scratch []NoteView  `json:"scratch,omitzero"`
}

// Conversations lists conversations, newest first.
func (c *Core) Conversations() []Summary {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]Summary, 0, len(c.order))
	for _, id := range slices.Backward(c.order) {
		out = append(out, c.summary(c.convs[id]))
	}
	return out
}

// summary describes a conversation. The caller holds mu.
func (c *Core) summary(conv *conversation) Summary {
	s := Summary{ID: conv.id, Created: conv.created, Busy: conv.busy}
	for _, it := range conv.items {
		if it.role == "user" {
			s.Preview = c.preview(it.parts)
			break
		}
	}
	return s
}

func (c *Core) preview(parts []store.Part) string {
	const limit = 60
	text := strings.Join(strings.Fields(c.text(parts)), " ")
	if runes := []rune(text); len(runes) > limit {
		return string(runes[:limit]) + "…"
	}
	return text
}

// text reads parts for display. Display isn't the record, so a blob that can't
// be read shows as a note rather than failing the whole view.
func (c *Core) text(parts []store.Part) string {
	var b strings.Builder
	for _, p := range parts {
		data, err := c.cfg.Store.Get(p.Block)
		if err != nil {
			b.WriteString("[unreadable block: " + err.Error() + "]")
			continue
		}
		b.Write(data)
	}
	return strings.ToValidUTF8(b.String(), "�")
}

// Conversation returns one conversation for display.
func (c *Core) Conversation(id string) (Conversation, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	conv := c.convs[id]
	if conv == nil {
		return Conversation{}, ErrNotFound
	}
	out := Conversation{ID: id, Revision: conv.revision, Busy: conv.busy, CanReply: !conv.busy && conv.canReply(), Tools: conv.tools != "", Workdir: conv.workdir, Items: []Item{}, ForkedFrom: conv.forkedFrom, ForkItems: conv.forkItems}
	effective := conv.effectiveSettings(c.settings)
	out.Model, out.Host = effective.Model, effective.Host
	full := effective.WithDefaults()
	period := billingPeriod(time.Now())
	out.Budget = BudgetView{
		Limit: conv.budgetLimit(effective), Spent: conv.spent, TopUp: picoFromUSD(full.BudgetTopUpUSD), Stopped: conv.budgetStopped,
		PeriodHere: conv.spentByPeriod[period], PeriodAll: c.spentInPeriod(period),
		Allowance: picoFromUSD(full.BudgetUSD),
	}
	// Only for an estimate, so one that can't be worked out just goes without.
	if definitions, err := c.definitions(conv.tools); err == nil {
		if body, err := c.renderRequest(conv, conv.context(), definitions, effective); err == nil {
			out.Budget.NextBytes = len(body)
			out.Budget.NextTokens = countRequest(effective.Model, body)
			if endpoints, ok := c.cachedPrices(effective.Model); ok {
				out.Budget.Estimate, out.Budget.EstimateKnown = c.minimumFor(endpoints, effective, body)
			}
		}
	}
	if conv.budgetStopped {
		out.Budget.Estimate, out.Budget.EstimateKnown = conv.stoppedEstimate, true
	}
	out.Paused = conv.paused
	out.WaitingToolCalls = conv.openWritten()
	out.Refused = c.refused(effective.Model, effective.Host)
	out.Running.Reply = conv.cancelReply != nil
	for id := range conv.stopTools {
		out.Running.Tool = id
	}
	for _, note := range conv.notes {
		out.Notes = append(out.Notes, NoteView{Kind: note.kind, Key: note.key, Status: note.status, Text: c.text([]store.Part{{Block: note.text}})})
	}
	for _, item := range conv.scratch {
		out.Scratch = append(out.Scratch, NoteView{Key: item.key, Text: c.text([]store.Part{{Block: item.text}})})
	}
	out.Settings, out.ConfigSettings = effective, c.settings
	out.CustomTools = conv.custom
	out.SettingsChanged = !reflect.DeepEqual(effective.WithDefaults(), c.settings.WithDefaults())
	if p := conv.pendingSettings; p != nil {
		out.PendingSettings = &PendingSettings{ID: p.id, Settings: p.settings, Tools: p.toolsProposed, CustomTools: p.custom}
		if p.toolsProposed {
			out.PendingSettings.Definitions = jsontext.Value("[]")
			if p.tools != "" {
				if data, err := c.cfg.Store.Get(p.tools); err == nil {
					out.PendingSettings.Definitions = data
				}
			}
		}
	}
	if d := conv.decision; d != nil {
		out.Decision = &DecisionView{ID: d.id, Kind: d.kind, Calls: d.calls, DenialText: denialText(d.kind)}
	}
	lastCall := -1
	for i, it := range conv.items {
		if it.role == "" {
			lastCall = i
		}
	}
	chains := conv.chainTotals()
	for i, it := range conv.items {
		shown := len(out.Items)
		switch {
		case it.role == "user":
			out.Items = append(out.Items, Item{Role: "user", Text: c.text(it.parts), Queued: !it.sent, Name: it.name})
		case it.role == "system":
			out.Items = append(out.Items, c.systemItem(it))
		case it.written():
			out.Items = append(out.Items, c.writtenItem(it))
		case it.finished == nil:
			// Only the newest call of a busy conversation can still be running.
			if !conv.busy || i != lastCall {
				out.Items = append(out.Items, Item{Role: "notice", Text: "This call was interrupted before it finished."})
			}
		case it.finished.Error != "":
			out.Items = append(out.Items, Item{Role: "notice", Text: it.finished.Error})
		case !usable(it.finished):
			out.Items = append(out.Items, Item{Role: "notice", Text: "The model returned no text."})
		default:
			reply := c.reply(it.finished)
			reply.Call, reply.Edited = it.callID, it.edited
			if change := it.override; change != nil {
				reply.Text, reply.Name, reply.Reasoning = c.text(change.parts), change.name, ""
				if change.reasoning != "" {
					reply.Reasoning = c.text([]store.Part{{Block: change.reasoning}})
				}
				reply.Removed = len(change.parts) == 0 && len(it.calls) == 0
			}
			reply.Pending = c.pendingView(it.pending)
			for _, call := range it.calls {
				reply.ToolCalls = append(reply.ToolCalls, c.toolCallView(conv, call))
			}
			out.Items = append(out.Items, reply)
		}
		if len(out.Items) > shown {
			out.Items[shown].Index = i
			out.Items[shown].ChainPico, out.Items[shown].ChainCalls = chains[i].cost, chains[i].calls
		}
	}
	return out, nil
}

func (c *Core) systemItem(it *item) Item {
	out := Item{
		Role:    "system",
		Text:    c.text(it.parts),
		Message: it.messageID,
		Queued:  !it.sent && len(it.parts) > 0,
		Edited:  it.edited,
		Removed: len(it.parts) == 0,
		Name:    it.name,
	}
	if it.pending != nil {
		out.Pending = &PendingEdit{ID: it.pending.id, Text: c.text(it.pending.parts)}
	}
	return out
}

func (c *Core) writtenItem(it *item) Item {
	out := Item{
		Role: it.role, Written: true, Text: c.text(it.parts), Message: it.messageID, Name: it.name, ToolCallID: it.toolCallID,
		WrittenCalls: c.writtenCalls(it.writtenCalls), Queued: !it.sent, Edited: it.edited,
		Removed: len(it.parts) == 0 && len(it.writtenCalls) == 0, Pending: c.pendingView(it.pending),
	}
	if it.reasoning != "" {
		out.Reasoning = c.text([]store.Part{{Block: it.reasoning}})
	}
	return out
}

func (c *Core) writtenCalls(calls []store.ToolCall) []MessageCall {
	var out []MessageCall
	for _, call := range calls {
		out = append(out, MessageCall{ID: call.ID, Name: call.Name, Arguments: c.text([]store.Part{{Block: call.Arguments}})})
	}
	return out
}

func (c *Core) pendingView(pending *edit) *PendingEdit {
	if pending == nil {
		return nil
	}
	out := &PendingEdit{ID: pending.id, Text: c.text(pending.parts), Name: pending.name, ToolCalls: c.writtenCalls(pending.calls)}
	if pending.reasoning != "" {
		out.Reasoning = c.text([]store.Part{{Block: pending.reasoning}})
	}
	return out
}

func (c *Core) reply(ev *store.Event) Item {
	item := Item{Role: "assistant", Stats: statsFrom(ev)}
	if ev.Text != "" {
		item.Text = c.text([]store.Part{{Block: ev.Text}})
	}
	if ev.Reasoning != "" {
		item.Reasoning = c.text([]store.Part{{Block: ev.Reasoning}})
	}
	return item
}

func statsFrom(ev *store.Event) *Stats {
	var usage struct {
		PromptTokens        int     `json:"prompt_tokens"`
		CompletionTokens    int     `json:"completion_tokens"`
		Cost                float64 `json:"cost"`
		PromptTokensDetails struct {
			CachedTokens int `json:"cached_tokens"`
		} `json:"prompt_tokens_details"`
		CompletionTokensDetails struct {
			ReasoningTokens int `json:"reasoning_tokens"`
		} `json:"completion_tokens_details"`
	}
	if len(ev.Usage) > 0 {
		// Stats are a convenience. The raw usage stays in the record whatever
		// shape it has, so a surprising shape just shows as zeros.
		_ = json.Unmarshal(ev.Usage, &usage)
	}
	return &Stats{
		Host:             ev.Host,
		PromptTokens:     usage.PromptTokens,
		CachedTokens:     usage.PromptTokensDetails.CachedTokens,
		CompletionTokens: usage.CompletionTokens,
		ReasoningTokens:  usage.CompletionTokensDetails.ReasoningTokens,
		CostPico:         picoFromUSD(usage.Cost),
		TTFTMillis:       ev.TTFTMillis,
		DurationMillis:   ev.DurationMillis,
	}
}

// These are the most that gets counted to show on the page. Counting is quick, but redrawing
// a page shouldn't wait on an enormous request or output.
const (
	countRequestBytes = 1 << 20
	countOutputBytes  = 4 << 20
)

func countRequest(model string, body []byte) int {
	counter, ok := tokens.For(model)
	if !ok || len(body) > countRequestBytes {
		return 0
	}
	n, err := counter.Request(body)
	if err != nil {
		return 0
	}
	return n
}

func countOutput(model, text string) int {
	counter, ok := tokens.For(model)
	if !ok || len(text) > countOutputBytes {
		return 0
	}
	n, err := counter.Text(text)
	if err != nil {
		return 0
	}
	return n
}
