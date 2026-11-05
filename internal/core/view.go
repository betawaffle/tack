package core

import (
	"encoding/json/v2"
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

	// System messages only.
	Message string       `json:"message,omitzero"`
	Queued  bool         `json:"queued,omitzero"` // no call has included this text yet
	Edited  bool         `json:"edited,omitzero"`
	Removed bool         `json:"removed,omitzero"`
	Pending *PendingEdit `json:"pending,omitzero"`
}

// Conversation is a conversation as the UI shows it.
type Conversation struct {
	ID       string `json:"id"`
	Busy     bool   `json:"busy"`
	CanReply bool   `json:"can_reply"`
	Items    []Item `json:"items"`
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
	out := Conversation{ID: id, Busy: conv.busy, CanReply: !conv.busy && conv.canReply(), Items: []Item{}}
	lastCall := -1
	for i, it := range conv.items {
		if it.role == "" {
			lastCall = i
		}
	}
	for i, it := range conv.items {
		switch {
		case it.role == "user":
			out.Items = append(out.Items, Item{Role: "user", Text: c.text(it.parts)})
		case it.role == "system":
			out.Items = append(out.Items, c.systemItem(it))
		case it.finished == nil:
			// Only the newest call of a busy conversation can still be running.
			if !conv.busy || i != lastCall {
				out.Items = append(out.Items, Item{Role: "notice", Text: "This call was interrupted before it finished."})
			}
		case it.finished.Error != "":
			out.Items = append(out.Items, Item{Role: "notice", Text: it.finished.Error})
		case it.finished.Text == "":
			out.Items = append(out.Items, Item{Role: "notice", Text: "The model returned no text."})
		default:
			out.Items = append(out.Items, c.reply(it.finished))
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
	item := Item{
		Role:  "assistant",
		Text:  c.text([]store.Part{{Block: ev.Text}}),
		Stats: statsFrom(ev),
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
