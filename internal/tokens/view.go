package tokens

import (
	"encoding/json/v2"
	"errors"
	"strings"
	"unicode/utf8"
)

// A prompt, as a model reads it, is the format's own text around the fields of the request.
// The view of one call shows which is which, where each part of the request is, how the text
// splits into tokens, and how it differs from the call before.

// The kinds of part a prompt is made of.
const (
	PartStart      = "start"       // what the format starts every prompt with
	PartEffort     = "effort"      // the reasoning effort, which the format writes from a setting
	PartSystem     = "system"      // a system message
	PartTools      = "tools"       // the tools offered, which the format writes into the first system message
	PartUser       = "user"        // a user message
	PartToolResult = "tool_result" // a tool's result
	PartReasoning  = "reasoning"   // an assistant's reasoning
	PartContent    = "content"     // an assistant's reply
	PartToolCall   = "tool_call"   // one of an assistant's tool calls
	PartNext       = "next"        // what the format puts before the model's turn
	PartAssistant  = "assistant"   // an assistant's whole message, around its reasoning, reply and calls
	PartCalls      = "calls"       // the block an assistant's tool calls are in
	PartParameter  = "parameter"   // one argument of a tool call
	PartValue      = "value"       // an argument's value
)

// Part is a piece of the request.
type Part struct {
	Kind string `json:"kind"`
	// Message is the request message it comes from, or -1 for one the format adds.
	Message int `json:"message"`
	// Call is which of an assistant's tool calls it is.
	Call int `json:"call,omitzero"`
	// Generated is set on the parts the model wrote in this call.
	Generated bool `json:"generated,omitzero"`
	// Parent is the part this one is inside, or -1.
	Parent int `json:"parent"`
}

// Segment is a run of text from one place: the format's (Field false) or a request field's.
type Segment struct {
	Text  string `json:"text"`
	Field bool   `json:"field,omitzero"`
	Part  int    `json:"part"`
}

type dsWriter struct {
	parts    []Part
	open     []int // the parts being written, outermost first
	segments []Segment
	text     strings.Builder
}

// part starts a part at the top level, ending any that are open.
func (w *dsWriter) part(p Part) {
	w.open = w.open[:0]
	w.enter(p)
}

// enter starts a part inside the one being written, until leave.
func (w *dsWriter) enter(p Part) {
	p.Parent = -1
	if n := len(w.open); n > 0 {
		p.Parent = w.open[n-1]
	}
	w.parts = append(w.parts, p)
	w.open = append(w.open, len(w.parts)-1)
}

func (w *dsWriter) leave() { w.open = w.open[:len(w.open)-1] }

func (w *dsWriter) add(text string, field bool) {
	if text == "" {
		return
	}
	w.text.WriteString(text)
	part := w.open[len(w.open)-1]
	if n := len(w.segments); n > 0 && w.segments[n-1].Field == field && w.segments[n-1].Part == part {
		w.segments[n-1].Text += text
		return
	}
	w.segments = append(w.segments, Segment{Text: text, Field: field, Part: part})
}

func (w *dsWriter) template(text string) { w.add(text, false) }
func (w *dsWriter) field(text string)    { w.add(text, true) }
func (w *dsWriter) String() string       { return w.text.String() }

// Tokens splits text into its tokens, as byte lengths that add up to the text's.
func (t *tokenizer) Tokens(text string) []int {
	var out []int
	plain := func(s string) {
		for _, piece := range t.split(s) {
			if piece == "" {
				continue
			}
			for _, symbol := range t.merge(piece) {
				// Each rune of a symbol stands for one byte of the text.
				out = append(out, utf8.RuneCountInString(symbol))
			}
		}
	}
	at := 0
	for i := 0; i < len(text); i++ {
		if !t.starts[text[i]] {
			continue
		}
		for end := min(len(text), i+t.longest); end > i; end-- {
			if _, ok := t.added[text[i:end]]; ok {
				plain(text[at:i])
				out = append(out, end-i)
				at, i = end, end-1
				break
			}
		}
	}
	plain(text[at:])
	return out
}

// Edit is one step from the call before's tokens to this call's: Same and Deleted take tokens
// from the call before, Same and Inserted from this one.
type Edit struct {
	Op string `json:"op"` // "same", "deleted" or "inserted"
	N  int    `json:"n"`
}

// Diff finds the fewest tokens to delete and insert to make a into b, deletions first where
// one replaces the other. What's the same at either end is left out of the search, since
// consecutive calls are nearly always the one before with more on the end.
func Diff(a, b []string) []Edit {
	var out []Edit
	add := func(op string, n int) {
		if n == 0 {
			return
		}
		if k := len(out); k > 0 && out[k-1].Op == op {
			out[k-1].N += n
			return
		}
		out = append(out, Edit{op, n})
	}
	start := 0
	for start < len(a) && start < len(b) && a[start] == b[start] {
		start++
	}
	end := 0
	for end < len(a)-start && end < len(b)-start && a[len(a)-1-end] == b[len(b)-1-end] {
		end++
	}
	add("same", start)
	for _, e := range myers(a[start:len(a)-end], b[start:len(b)-end]) {
		add(e.Op, e.N)
	}
	add("same", end)
	return replacementsDeleteFirst(out)
}

// myers is the classic O(ND) search, which is quick when the two differ by little.
func myers(a, b []string) []Edit {
	n, m := len(a), len(b)
	if n == 0 && m == 0 {
		return nil
	}
	max := n + m
	v := make([]int, 2*max+2)
	var trace [][]int
	// Its memory grows with the number of differences times the length, so past a point the
	// rest is shown as replaced whole, which is still right, just not the fewest.
	for d := 0; d <= max && d <= maxDiffSteps; d++ {
		trace = append(trace, append([]int(nil), v...))
		for k := -d; k <= d; k += 2 {
			var x int
			if k == -d || (k != d && v[max+k-1] < v[max+k+1]) {
				x = v[max+k+1]
			} else {
				x = v[max+k-1] + 1
			}
			y := x - k
			for x < n && y < m && a[x] == b[y] {
				x++
				y++
			}
			v[max+k] = x
			if x >= n && y >= m {
				return backtrack(trace, a, b, d, max)
			}
		}
	}
	return []Edit{{"deleted", n}, {"inserted", m}}
}

// maxDiffSteps is how many differences the search looks for before giving up on the fewest.
const maxDiffSteps = 2000

func backtrack(trace [][]int, a, b []string, d, max int) []Edit {
	var steps []string
	x, y := len(a), len(b)
	for ; d > 0; d-- {
		k := x - y
		prev := trace[d]
		var pk int
		if k == -d || (k != d && prev[max+k-1] < prev[max+k+1]) {
			pk = k + 1
		} else {
			pk = k - 1
		}
		px := prev[max+pk]
		py := px - pk
		for x > px && y > py {
			steps = append(steps, "same")
			x--
			y--
		}
		if x == px {
			steps = append(steps, "inserted")
		} else {
			steps = append(steps, "deleted")
		}
		x, y = px, py
	}
	for x > 0 && y > 0 {
		steps = append(steps, "same")
		x--
		y--
	}
	var out []Edit
	for i := len(steps) - 1; i >= 0; i-- {
		if k := len(out); k > 0 && out[k-1].Op == steps[i] {
			out[k-1].N++
		} else {
			out = append(out, Edit{steps[i], 1})
		}
	}
	return out
}

// replacementsDeleteFirst puts each run of deletions before the insertions next to it.
func replacementsDeleteFirst(edits []Edit) []Edit {
	var out []Edit
	for i := 0; i < len(edits); {
		if edits[i].Op == "same" {
			out = append(out, edits[i])
			i++
			continue
		}
		del, ins := 0, 0
		for ; i < len(edits) && edits[i].Op != "same"; i++ {
			if edits[i].Op == "deleted" {
				del += edits[i].N
			} else {
				ins += edits[i].N
			}
		}
		if del > 0 {
			out = append(out, Edit{"deleted", del})
		}
		if ins > 0 {
			out = append(out, Edit{"inserted", ins})
		}
	}
	return out
}

// Prompt is one call's prompt, and what the model wrote after it, as the model read and wrote
// them.
type Prompt struct {
	Parts    []Part    `json:"parts"`
	Segments []Segment `json:"segments"`
	// Tokens are each token's length in bytes.
	Tokens []int `json:"tokens"`
	// Generated is where the model's own writing starts, in bytes, if it's here.
	Generated int `json:"generated"`
	// Effort is the reasoning effort written in, and EffortKnown whether the request said one
	// this format knows. Without it, it's the format's default, which may not be what the host
	// used.
	Effort      int  `json:"effort"`
	Thinking    bool `json:"thinking"`
	EffortKnown bool `json:"effort_known"`
	text        string
}

// Text is the whole prompt.
func (p *Prompt) Text() string { return p.text }

// Output is what the model wrote in a call.
type Output struct {
	Reasoning string
	Content   string
	Calls     []OutputCall
}

// OutputCall is a tool call the model made.
type OutputCall struct {
	Name      string
	Arguments string
}

// ErrNoRenderer is for a model whose prompt format tack doesn't know.
var ErrNoRenderer = errors.New("tack doesn't know how this model's prompts are written")

// Render shows a request as the model reads it, followed by what it wrote, if out isn't nil.
func Render(model string, body []byte, out *Output) (*Prompt, error) {
	counter, ok := counters[model]
	if !ok || counter.render == nil {
		return nil, ErrNoRenderer
	}
	return counter.render(body, out)
}

// Tokenize splits a prompt's text into tokens, as byte lengths.
func Tokenize(model, text string) ([]int, error) {
	counter, ok := counters[model]
	if !ok || counter.tokens == nil {
		return nil, ErrNoRenderer
	}
	return counter.tokens(text)
}

// dsEfforts are the named efforts and what DeepSeek's encoder writes for them.
var dsEfforts = map[string]int{"low": 50, "high": 75, "max": 100}

func renderDeepSeekV41(body []byte, out *Output) (*Prompt, error) {
	var settings struct {
		Reasoning struct {
			Effort string `json:"effort"`
		} `json:"reasoning"`
	}
	if err := json.Unmarshal(body, &settings, json.RejectUnknownMembers(false)); err != nil {
		return nil, err
	}
	p := &Prompt{Thinking: true, Effort: dsDefaultEffort, EffortKnown: settings.Reasoning.Effort == ""}
	if n, ok := dsEfforts[settings.Reasoning.Effort]; ok {
		p.Effort, p.EffortKnown = n, true
	} else if settings.Reasoning.Effort == "none" {
		p.Thinking, p.EffortKnown = false, true
	}
	var req dsRequest
	if err := json.Unmarshal(body, &req, json.RejectUnknownMembers(false)); err != nil {
		return nil, err
	}
	messages, err := dsMerge(&req)
	if err != nil {
		return nil, err
	}
	var tools []string
	for _, tool := range req.Tools {
		text, err := pyJSON(tool.Function)
		if err != nil {
			return nil, err
		}
		tools = append(tools, text)
	}
	if len(tools) > 0 && (len(messages) == 0 || messages[0].role != "system") {
		messages = append([]dsMessage{{role: "system", src: -1}}, messages...)
	}
	var w dsWriter
	dsWrite(&w, messages, tools, p.Thinking, p.Effort)
	prompt := w.String()
	p.Generated = -1
	if out != nil {
		reply := dsMessage{role: "assistant", content: out.Content, reasoning: out.Reasoning, src: len(req.Messages)}
		for _, call := range out.Calls {
			reply.calls = append(reply.calls, dsCall{name: call.Name, arguments: call.Arguments})
		}
		var whole dsWriter
		dsWrite(&whole, append(messages, reply), tools, p.Thinking, p.Effort)
		// What the model wrote follows the prompt exactly, or it isn't shown.
		if strings.HasPrefix(whole.String(), prompt) {
			for i := range whole.parts {
				if whole.parts[i].Message == reply.src {
					whole.parts[i].Generated = true
				}
			}
			w, p.Generated = whole, len(prompt)
		}
	}
	p.Parts, p.Segments, p.text = w.parts, w.segments, w.String()
	t, err := loadDeepSeekV41()
	if err != nil {
		return nil, err
	}
	p.Tokens = t.Tokens(p.text)
	return p, nil
}

func tokensDeepSeekV41(text string) ([]int, error) {
	t, err := loadDeepSeekV41()
	if err != nil {
		return nil, err
	}
	return t.Tokens(text), nil
}
