package web

import (
	"github.com/betawaffle/tack/internal/core"
	"github.com/betawaffle/tack/internal/tokens"
)

// A prompt as the socket sends it: each part's message and parent, and each segment's part,
// are counted from the one before, so that adding or taking away a message only changes the
// few entries around it, and a patch of the prompt stays small. The page counts them back
// (prompt-patch.js, absolute).

// wirePart is a tokens.Part with Message as how far on from the last part's that had one, left
// out for one the format adds, and Parent as how many parts back it is, or 0 for none.
type wirePart struct {
	Kind      string `json:"kind"`
	Message   *int   `json:"m,omitempty"`
	Call      int    `json:"call,omitzero"`
	Generated bool   `json:"generated,omitzero"`
	Parent    int    `json:"p,omitzero"`
	Name      string `json:"name,omitzero"`
	Raw       bool   `json:"raw,omitzero"`
}

// wireSegment is a tokens.Segment with Part as how far on from the segment before's.
type wireSegment struct {
	Text  string `json:"text"`
	Field bool   `json:"field,omitzero"`
	Part  int    `json:"p,omitzero"`
}

// wirePromptBody is tokens.Prompt, with its parts and segments as they're sent.
type wirePromptBody struct {
	Parts       []wirePart    `json:"parts"`
	Segments    []wireSegment `json:"segments"`
	Tokens      []int         `json:"tokens"`
	Generated   int           `json:"generated"`
	Effort      int           `json:"effort"`
	Thinking    bool          `json:"thinking"`
	EffortKnown bool          `json:"effort_known"`
}

// wireCallPrompt is a core.CallPrompt with its prompt as it's sent.
type wireCallPrompt struct {
	core.CallPrompt
	Prompt *wirePromptBody `json:"prompt"`
}

func toWire(cp core.CallPrompt) wireCallPrompt {
	out := wireCallPrompt{CallPrompt: cp}
	p := cp.Prompt
	if p == nil {
		return out
	}
	body := &wirePromptBody{
		Parts: make([]wirePart, len(p.Parts)), Segments: make([]wireSegment, len(p.Segments)), Tokens: p.Tokens,
		Generated: p.Generated, Effort: p.Effort, Thinking: p.Thinking, EffortKnown: p.EffortKnown,
	}
	last := 0
	for k, part := range p.Parts {
		w := wirePart{Kind: part.Kind, Call: part.Call, Generated: part.Generated, Name: part.Name, Raw: part.Raw}
		if part.Message >= 0 {
			m := part.Message - last
			w.Message = &m
			last = part.Message
		}
		if part.Parent >= 0 {
			w.Parent = k - part.Parent
		}
		body.Parts[k] = w
	}
	prev := 0
	for i, s := range p.Segments {
		body.Segments[i] = wireSegment{Text: s.Text, Field: s.Field, Part: s.Part - prev}
		prev = s.Part
	}
	out.Prompt = body
	return out
}

// fromWire undoes toWire, for testing it.
func fromWire(w *wirePromptBody) *tokens.Prompt {
	p := &tokens.Prompt{
		Parts: make([]tokens.Part, len(w.Parts)), Segments: make([]tokens.Segment, len(w.Segments)), Tokens: w.Tokens,
		Generated: w.Generated, Effort: w.Effort, Thinking: w.Thinking, EffortKnown: w.EffortKnown,
	}
	last := 0
	for k, part := range w.Parts {
		out := tokens.Part{Kind: part.Kind, Message: -1, Call: part.Call, Generated: part.Generated, Parent: -1, Name: part.Name, Raw: part.Raw}
		if part.Message != nil {
			last += *part.Message
			out.Message = last
		}
		if part.Parent > 0 {
			out.Parent = k - part.Parent
		}
		p.Parts[k] = out
	}
	prev := 0
	for i, s := range w.Segments {
		prev += s.Part
		p.Segments[i] = tokens.Segment{Text: s.Text, Field: s.Field, Part: prev}
	}
	return p
}
