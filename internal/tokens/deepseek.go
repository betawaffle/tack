package tokens

import (
	"bytes"
	_ "embed"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"regexp"
	"slices"
	"strings"
	"sync"
)

// The tokenizer and encoding below come from deepseek-ai/DeepSeek-V4.1-Flash-1017 on Hugging
// Face, which is MIT licensed, Copyright (c) 2023 DeepSeek. The file is its tokenizer.json,
// sha256 c90dfa01249db1be4245780a052ede752e1361c612ac6d08e2bdada7d599476b, compressed.
//
//go:embed deepseek_v41_tokenizer.json.gz
var deepSeekV41File []byte

var loadDeepSeekV41 = sync.OnceValues(func() (*tokenizer, error) {
	return loadTokenizer(deepSeekV41File, splitDeepSeek)
})

var (
	deepSeekNumbers = regexp.MustCompile(`\p{N}{1,3}`)
	deepSeekCJK     = regexp.MustCompile(`[\x{4e00}-\x{9fa5}\x{3040}-\x{309f}\x{30a0}-\x{30ff}]+`)
	// The file's third pattern, up to the two whitespace alternatives. Those use a lookahead,
	// which Go doesn't have, so deepSeekWords does them itself. \s here is written out, because
	// it's wider in the library that runs the original.
	deepSeekWord = regexp.MustCompile("^(?:" +
		"[!\"#$%&'()*+,\\-./:;<=>?@\\[\\\\\\]^_`{|}~][A-Za-z]+" +
		`|[^\r\n\p{L}\p{P}\p{S}]?[\p{L}\p{M}]+` +
		`| ?[\p{P}\p{S}]+[\r\n]*` +
		`|[\t\n\x{0B}\f\r \x{85}\p{Z}]*[\r\n]+` +
		")")
)

func splitDeepSeek(text string) []string {
	pieces := isolate(isolate([]string{text}, deepSeekNumbers), deepSeekCJK)
	var out []string
	for _, piece := range pieces {
		out = deepSeekWords(out, piece)
	}
	return out
}

func deepSeekWords(out []string, piece string) []string {
	gap := 0
	flush := func(at int) {
		if at > gap {
			out = append(out, piece[gap:at])
		}
	}
	for at := 0; at < len(piece); {
		n := 0
		if match := deepSeekWord.FindString(piece[at:]); match != "" {
			n = len(match)
		} else {
			// \s+(?!\S), and then \s+. A run of whitespace leaves its last character for what
			// comes next, unless it reaches the end or is only one character long.
			last := 0
			count := 0
			for i, r := range piece[at:] {
				if !isSpace(r) {
					last = -1 - i
					break
				}
				last = i
				count++
			}
			switch {
			case count == 0:
			case last >= 0:
				n = len(piece) - at
			case count == 1:
				n = -1 - last
			default:
				// The start of its last whitespace character.
				end := -1 - last
				run := piece[at : at+end]
				_, size := lastRune(run)
				n = end - size
			}
		}
		if n == 0 {
			at += runeLen(piece[at:])
			continue
		}
		flush(at)
		out = append(out, piece[at:at+n])
		at += n
		gap = at
	}
	flush(len(piece))
	return out
}

func lastRune(s string) (rune, int) {
	for i := len(s) - 1; i >= 0; i-- {
		if s[i]&0xc0 != 0x80 {
			return rune(s[i]), len(s) - i
		}
	}
	return 0, 0
}

// The rest follows DeepSeek's encoding.py. Tokens and templates are copied exactly.
const (
	dsBOS       = "<｜begin▁of▁sentence｜>"
	dsEOS       = "<｜end▁of▁sentence｜>"
	dsThink     = "<think>"
	dsThinkEnd  = "</think>"
	dsDSML      = "｜DSML｜"
	dsUser      = "<｜User｜>"
	dsAssistant = "<｜Assistant｜>"
	dsSystem    = "<｜System｜>"
	dsCalls     = " calls"
	dsInvoke    = " invoke"
	dsParameter = " parameter"
)

var dsToolsTemplate = strings.NewReplacer(
	"{dsml_token}", dsDSML, "{tc_block_name}", dsCalls, "{tool_call_tag_name}", dsInvoke,
	"{tool_parameter_tag_name}", dsParameter, "{thinking_start_token}", dsThink, "{thinking_end_token}", dsThinkEnd,
).Replace(`## Tools

You have access to a set of tools to help answer the user's question. You can invoke tools by writing a "<{dsml_token}{tc_block_name}>" block like the following:

<{dsml_token}{tc_block_name}>
<{dsml_token}{tool_call_tag_name} name="$TOOL_NAME">
<{dsml_token}{tool_parameter_tag_name} name="$PARAMETER_NAME" string="true|false">$PARAMETER_VALUE</{dsml_token}{tool_parameter_tag_name}>
...
</{dsml_token}{tool_call_tag_name}>
<{dsml_token}{tool_call_tag_name} name="$TOOL_NAME2">
...
</{dsml_token}{tool_call_tag_name}>
</{dsml_token}{tc_block_name}>

String parameters should be specified as is and set ` + "`string=\"true\"`" + `. For all other types (numbers, booleans, arrays, objects), pass the value in JSON format and set ` + "`string=\"false\"`" + `.

If thinking_mode is enabled (triggered by {thinking_start_token}), you MUST output your complete reasoning inside {thinking_start_token}...{thinking_end_token} BEFORE any tool calls or final response.

Otherwise, output directly after {thinking_end_token} with tool calls or final response.

### Available Tool Schemas

{tool_schemas}

You MUST strictly follow the above defined tool name and parameter schemas to invoke tool calls.
`)

// pyJSON writes JSON the way Python's json.dumps does with ensure_ascii off, keeping the order
// of keys: a space after each comma and colon, and only the escapes it would use.
func pyJSON(value jsontext.Value) (string, error) {
	var out strings.Builder
	dec := jsontext.NewDecoder(bytes.NewReader(value))
	type level struct {
		object bool
		count  int
	}
	var stack []level
	for {
		token, err := dec.ReadToken()
		if errors.Is(err, io.EOF) {
			return out.String(), nil
		}
		if err != nil {
			return "", err
		}
		kind := token.Kind()
		closing := kind == '}' || kind == ']'
		if n := len(stack); n > 0 && !closing {
			top := &stack[n-1]
			switch {
			case top.object && top.count%2 == 1:
				out.WriteString(": ")
			case top.count > 0:
				out.WriteString(", ")
			}
			top.count++
		}
		switch kind {
		case '{', '[':
			out.WriteByte(byte(kind))
			stack = append(stack, level{object: kind == '{'})
		case '}', ']':
			out.WriteByte(byte(kind))
			stack = stack[:len(stack)-1]
		case '"':
			pyString(&out, token.String())
		default:
			out.WriteString(token.String())
		}
	}
}

func pyString(out *strings.Builder, s string) {
	out.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			out.WriteString(`\"`)
		case '\\':
			out.WriteString(`\\`)
		case '\n':
			out.WriteString(`\n`)
		case '\r':
			out.WriteString(`\r`)
		case '\t':
			out.WriteString(`\t`)
		case '\b':
			out.WriteString(`\b`)
		case '\f':
			out.WriteString(`\f`)
		default:
			if r < 0x20 {
				fmt.Fprintf(out, `\u%04x`, r)
			} else {
				out.WriteRune(r)
			}
		}
	}
	out.WriteByte('"')
}

type dsBlock struct {
	tool bool
	id   string
	text string
}

type dsCall struct {
	id, name  string
	arguments string
}

type dsMessage struct {
	role      string
	content   string
	blocks    []dsBlock
	reasoning string
	calls     []dsCall
}

type dsRequest struct {
	Messages []struct {
		Role             string         `json:"role"`
		Content          jsontext.Value `json:"content"`
		Reasoning        string         `json:"reasoning"`
		ReasoningContent string         `json:"reasoning_content"`
		ToolCallID       string         `json:"tool_call_id"`
		ToolCalls        []struct {
			ID       string `json:"id"`
			Function struct {
				Name      string `json:"name"`
				Arguments string `json:"arguments"`
			} `json:"function"`
		} `json:"tool_calls"`
	} `json:"messages"`
	Tools []struct {
		Function jsontext.Value `json:"function"`
	} `json:"tools"`
}

// dsText reads a message's content, which is a string or a list of parts. Only text parts are
// counted, since tack doesn't send anything else.
func dsText(content jsontext.Value) (string, error) {
	if len(content) == 0 || content.Kind() == 'n' {
		return "", nil
	}
	if content.Kind() == '"' {
		var text string
		err := json.Unmarshal(content, &text)
		return text, err
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(content, &parts); err != nil {
		return "", err
	}
	var texts []string
	for _, part := range parts {
		if part.Type != "text" {
			return "", fmt.Errorf("a %s part can't be counted", part.Type)
		}
		texts = append(texts, part.Text)
	}
	return strings.Join(texts, "\n\n"), nil
}

// dsMerge puts tool results into user messages, in the order they were called in, since this
// format has no role for them. Neighboring user messages become one.
func dsMerge(req *dsRequest) ([]dsMessage, error) {
	var out []dsMessage
	var order map[string]int
	sortResults := func(m *dsMessage) {
		if m == nil || order == nil {
			return
		}
		var tools []dsBlock
		for _, block := range m.blocks {
			if block.tool {
				tools = append(tools, block)
			}
		}
		slices.SortStableFunc(tools, func(a, b dsBlock) int { return order[a.id] - order[b.id] })
		for i, n := 0, 0; i < len(m.blocks); i++ {
			if m.blocks[i].tool {
				m.blocks[i] = tools[n]
				n++
			}
		}
	}
	for i := range req.Messages {
		m := &req.Messages[i]
		text, err := dsText(m.Content)
		if err != nil {
			return nil, err
		}
		switch m.Role {
		case "tool", "user":
			block := dsBlock{tool: m.Role == "tool", id: m.ToolCallID, text: text}
			if n := len(out); n > 0 && out[n-1].role == "user" {
				out[n-1].blocks = append(out[n-1].blocks, block)
			} else {
				out = append(out, dsMessage{role: "user", blocks: []dsBlock{block}})
			}
		default:
			message := dsMessage{role: m.Role, content: text, reasoning: m.ReasoningContent}
			if message.reasoning == "" {
				message.reasoning = m.Reasoning
			}
			out = append(out, message)
			if m.Role == "assistant" {
				if len(out) >= 2 {
					sortResults(&out[len(out)-2])
				}
				order = nil
				for j, call := range m.ToolCalls {
					if order == nil {
						order = map[string]int{}
					}
					order[call.ID] = j
					out[len(out)-1].calls = append(out[len(out)-1].calls, dsCall{call.ID, call.Function.Name, call.Function.Arguments})
				}
			}
		}
	}
	if len(out) > 0 {
		sortResults(&out[len(out)-1])
	}
	return out, nil
}

func dsLastUser(messages []dsMessage) int {
	for i := len(messages) - 1; i >= 0; i-- {
		m := &messages[i]
		if m.role == "user" || (m.role == "system" && i > 0) {
			if m.role == "user" && !slices.ContainsFunc(m.blocks, func(b dsBlock) bool { return !b.tool }) {
				continue
			}
			return i
		}
	}
	return -1
}

func dsArguments(arguments string) string {
	value := jsontext.Value(arguments)
	// Double-encoded arguments are tolerated, like the original does.
	if value.IsValid() && value.Kind() == '"' {
		var inner string
		if json.Unmarshal(value, &inner) == nil {
			value = jsontext.Value(inner)
		}
	}
	if !value.IsValid() || value.Kind() != '{' {
		return dsParameterTag("arguments", true, arguments)
	}
	var lines []string
	dec := jsontext.NewDecoder(bytes.NewReader(value))
	dec.ReadToken()
	for dec.PeekKind() != '}' {
		token, err := dec.ReadToken()
		if err != nil {
			break
		}
		key := token.String()
		raw, err := dec.ReadValue()
		if err != nil {
			break
		}
		if raw.Kind() == '"' {
			var text string
			json.Unmarshal(raw, &text)
			lines = append(lines, dsParameterTag(key, true, text))
		} else {
			text, err := pyJSON(raw)
			if err != nil {
				text = string(raw)
			}
			lines = append(lines, dsParameterTag(key, false, text))
		}
	}
	return strings.Join(lines, "\n")
}

func dsParameterTag(key string, isString bool, value string) string {
	kind := "false"
	if isString {
		kind = "true"
	}
	return "<" + dsDSML + dsParameter + ` name="` + key + `" string="` + kind + `">` + value + "</" + dsDSML + dsParameter + ">"
}

// dsPrompt renders the whole prompt. Effort is only shown when thinking.
func dsPrompt(messages []dsMessage, tools []string, thinking bool, effort int) string {
	var out strings.Builder
	out.WriteString(dsBOS)
	// With tools, reasoning is kept all the way back. Without them, only since the last user.
	drop := len(tools) == 0
	last := dsLastUser(messages)
	for i := range messages {
		m := &messages[i]
		if i == 0 {
			if thinking {
				out.WriteString(dsSystem)
				fmt.Fprintf(&out, "Reasoning Effort: %d (range 1-100, the higher the value, the more thorough the reasoning)\n\n", effort)
			} else if m.role == "system" {
				out.WriteString(dsSystem)
			}
		}
		switch m.role {
		case "system":
			if i > 0 {
				out.WriteString(dsSystem)
			}
			out.WriteString(m.content)
			if i == 0 && len(tools) > 0 {
				out.WriteString("\n\n")
				out.WriteString(strings.Replace(dsToolsTemplate, "{tool_schemas}", strings.Join(tools, "\n"), 1))
			}
		case "user":
			out.WriteString(dsUser)
			for j, block := range m.blocks {
				if j > 0 {
					out.WriteString("\n\n")
				}
				if block.tool {
					out.WriteString("<tool_result>" + block.text + "</tool_result>")
				} else {
					out.WriteString(block.text)
				}
			}
		case "assistant":
			if thinking && (!drop || i > last) {
				out.WriteString(m.reasoning)
				out.WriteString(dsThinkEnd)
			}
			out.WriteString(m.content)
			if len(m.calls) > 0 {
				var calls []string
				for _, call := range m.calls {
					calls = append(calls, "<"+dsDSML+dsInvoke+` name="`+call.name+`">`+"\n"+dsArguments(call.arguments)+"\n</"+dsDSML+dsInvoke+">")
				}
				out.WriteString("\n\n<" + dsDSML + dsCalls + ">\n" + strings.Join(calls, "\n") + "\n</" + dsDSML + dsCalls + ">")
			}
			out.WriteString(dsEOS)
		}
		if i+1 < len(messages) && messages[i+1].role != "assistant" {
			continue
		}
		if m.role == "user" || (m.role == "system" && i > 0) {
			out.WriteString(dsAssistant)
			if thinking && (!drop || i >= last) {
				out.WriteString(dsThink)
			} else {
				out.WriteString(dsThinkEnd)
			}
		}
	}
	return out.String()
}

func dsRender(body []byte, thinking bool, effort int) (string, error) {
	var req dsRequest
	if err := json.Unmarshal(body, &req, json.RejectUnknownMembers(false)); err != nil {
		return "", err
	}
	messages, err := dsMerge(&req)
	if err != nil {
		return "", err
	}
	var tools []string
	for _, tool := range req.Tools {
		text, err := pyJSON(tool.Function)
		if err != nil {
			return "", err
		}
		tools = append(tools, text)
	}
	if len(tools) > 0 && (len(messages) == 0 || messages[0].role != "system") {
		// The tools go in a system message, so one has to be there to carry them.
		messages = append([]dsMessage{{role: "system"}}, messages...)
	}
	return dsPrompt(messages, tools, thinking, effort), nil
}

// DeepSeek reasons by default, at a default effort of "high", which is 75.
const dsDefaultEffort = 75

func countDeepSeekV41(body []byte) (int, error) {
	t, err := loadDeepSeekV41()
	if err != nil {
		return 0, err
	}
	prompt, err := dsRender(body, true, dsDefaultEffort)
	if err != nil {
		return 0, err
	}
	// Compared with what's been reported, NUL bytes don't count, which suggests they don't
	// reach the model. Output read from /proc is where they turned up.
	return t.Count(strings.ReplaceAll(prompt, "\x00", "")), nil
}

func countDeepSeekV41Text(text string) (int, error) {
	t, err := loadDeepSeekV41()
	if err != nil {
		return 0, err
	}
	return t.Count(strings.ReplaceAll(text, "\x00", "")), nil
}
