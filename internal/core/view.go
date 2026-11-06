package core

import (
	"encoding/json/v2"
	"reflect"
	"slices"
	"strings"
	"time"

	"github.com/betawaffle/tack/internal/store"
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
	Host             string  `json:"host,omitzero"`
	PromptTokens     int     `json:"prompt_tokens"`
	CachedTokens     int     `json:"cached_tokens"`
	CompletionTokens int     `json:"completion_tokens"`
	ReasoningTokens  int     `json:"reasoning_tokens"`
	CostUSD          float64 `json:"cost_usd"`
	TTFTMillis       int64   `json:"ttft_ms"`
	DurationMillis   int64   `json:"duration_ms"`
}

// PendingEdit is a system message edit that's being held. An empty Text
// proposes removing the message.
type PendingEdit struct {
	ID   string `json:"id"`
	Text string `json:"text"`
}

// Item is something to show in a conversation. Text holds the message, or for
// the "notice" role, what went wrong.
type Item struct {
	Role      string `json:"role"`
	Text      string `json:"text"`
	Reasoning string `json:"reasoning,omitzero"`
	Stats     *Stats `json:"stats,omitzero"`

	// User and system messages: no call has included this text yet.
	Queued bool `json:"queued,omitzero"`

	// System messages only.
	Message string       `json:"message,omitzero"`
	Edited  bool         `json:"edited,omitzero"`
	Removed bool         `json:"removed,omitzero"`
	Pending *PendingEdit `json:"pending,omitzero"`

	// Replies that made tool calls.
	ToolCalls []ToolCallView `json:"tool_calls,omitzero"`
}

// ToolCallView is a tool call as the UI shows it. Arguments are the exact text
// that was requested, which is also what runs. State is "waiting", "approved",
// "running", "interrupted", "finished", "sent" or "denied".
type ToolCallView struct {
	ID            string   `json:"id"`
	Name          string   `json:"name"`
	Arguments     string   `json:"arguments"`
	Invalid       string   `json:"invalid,omitzero"`
	Flags         []string `json:"flags,omitzero"`
	State         string   `json:"state"`
	Output        string   `json:"output,omitzero"`
	HasOutput     bool     `json:"has_output,omitzero"`
	Error         string   `json:"error,omitzero"`
	Result        string   `json:"result,omitzero"` // what the model was given
	Decidable     bool     `json:"decidable,omitzero"`
	NetworkOption bool     `json:"network_option,omitzero"`
	Network       bool     `json:"network,omitzero"`
}

// BudgetView is a conversation's spending. Stopped says a call wasn't started because
// of it. TopUp is what the add box starts with.
type BudgetView struct {
	Limit    float64 `json:"limit"`
	Spent    float64 `json:"spent"`
	Estimate float64 `json:"estimate"`
	TopUp    float64 `json:"top_up"`
	Stopped  bool    `json:"stopped,omitzero"`
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
	ID       string `json:"id"`
	Model    string `json:"model"`
	Host     string `json:"host,omitzero"` // the pinned host, if any
	Busy     bool   `json:"busy"`
	CanReply bool   `json:"can_reply"`
	Tools    bool   `json:"tools,omitzero"`
	Workdir  string `json:"workdir,omitzero"`
	// Settings are what it uses, and ConfigSettings what a new conversation would
	// start with. SettingsChanged says they differ, and PendingSettings are held.
	Settings        store.Settings   `json:"settings"`
	ConfigSettings  store.Settings   `json:"config_settings"`
	SettingsChanged bool             `json:"settings_changed,omitzero"`
	PendingSettings *PendingSettings `json:"pending_settings,omitzero"`
	Items           []Item           `json:"items"`

	Decision *DecisionView `json:"decision,omitzero"`

	Budget  BudgetView  `json:"budget"`
	Paused  bool        `json:"paused,omitzero"`
	Running RunningView `json:"running"`
	Notes   []NoteView  `json:"notes,omitzero"`
}

// Conversations lists conversations, newest first.
func (c *Core) Conversations() []Summary {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]Summary, 0, len(c.order))
	for _, id := range slices.Backward(c.order) {
		conv := c.convs[id]
		s := Summary{ID: id, Created: conv.created, Busy: conv.busy}
		for _, it := range conv.items {
			if it.role == "user" {
				s.Preview = c.preview(it.parts)
				break
			}
		}
		out = append(out, s)
	}
	return out
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
	out := Conversation{ID: id, Busy: conv.busy, CanReply: !conv.busy && conv.canReply(), Tools: conv.tools != "", Workdir: conv.workdir, Items: []Item{}}
	effective := conv.effectiveSettings(c.settings)
	out.Model, out.Host = effective.Model, effective.Host
	full := effective.WithDefaults()
	out.Budget = BudgetView{Limit: conv.budgetLimit(effective), Spent: conv.spent, Estimate: conv.maxRecentCost(), TopUp: full.BudgetTopUpUSD, Stopped: conv.budgetStopped}
	out.Paused = conv.paused
	out.Running.Reply = conv.cancelReply != nil
	for id := range conv.stopTools {
		out.Running.Tool = id
	}
	for _, note := range conv.notes {
		out.Notes = append(out.Notes, NoteView{Kind: note.kind, Key: note.key, Status: note.status, Text: c.text([]store.Part{{Block: note.text}})})
	}
	out.Settings, out.ConfigSettings = effective, c.settings
	out.SettingsChanged = !reflect.DeepEqual(effective.WithDefaults(), c.settings.WithDefaults())
	if p := conv.pendingSettings; p != nil {
		out.PendingSettings = &PendingSettings{ID: p.id, Settings: p.settings}
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
	for i, it := range conv.items {
		switch {
		case it.role == "user":
			out.Items = append(out.Items, Item{Role: "user", Text: c.text(it.parts), Queued: !it.sent})
		case it.role == "system":
			out.Items = append(out.Items, c.systemItem(it))
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
			for _, call := range it.calls {
				reply.ToolCalls = append(reply.ToolCalls, c.toolCallView(conv, call))
			}
			out.Items = append(out.Items, reply)
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
	}
	if it.pending != nil {
		out.Pending = &PendingEdit{ID: it.pending.id, Text: c.text(it.pending.parts)}
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
		CostUSD:          usage.Cost,
		TTFTMillis:       ev.TTFTMillis,
		DurationMillis:   ev.DurationMillis,
	}
}
