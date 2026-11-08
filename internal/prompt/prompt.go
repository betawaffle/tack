// Package prompt holds what new conversations start with: the sections a system prompt
// is built from, and the tools they're offered. The sections are joined into an ordinary
// system message when a conversation starts and never rendered again, because changing
// the start of a context misses the whole cache.
package prompt

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Separator goes between sections.
const Separator = "\n\n"

// DateVariable is replaced with the date a conversation starts, as YYYY-MM-DD.
const DateVariable = "{{date}}"

// Section is a named piece of a system prompt, made of paragraphs. Off keeps a section or
// a paragraph in the list without sending it.
type Section struct {
	Name       string      `json:"name"`
	Paragraphs []Paragraph `json:"paragraphs"`
	Off        bool        `json:"off,omitzero"`
}

// Paragraph is one paragraph of a section.
type Paragraph struct {
	Text string `json:"text"`
	Off  bool   `json:"off,omitzero"`
}

// sectionWire also reads the older form, where a section was one text with paragraphs
// separated by blank lines.
type sectionWire struct {
	Name       string      `json:"name"`
	Text       *string     `json:"text,omitzero"`
	Paragraphs []Paragraph `json:"paragraphs"`
	Off        bool        `json:"off,omitzero"`
}

func (s *Section) UnmarshalJSON(data []byte) error {
	var w sectionWire
	if err := json.Unmarshal(data, &w, json.RejectUnknownMembers(true)); err != nil {
		return err
	}
	*s = Section{Name: w.Name, Paragraphs: w.Paragraphs, Off: w.Off}
	if w.Text != nil {
		if w.Paragraphs != nil {
			return errors.New("a section has either text or paragraphs")
		}
		s.Paragraphs = Split(*w.Text)
	}
	if s.Paragraphs == nil {
		s.Paragraphs = []Paragraph{}
	}
	return nil
}

// Split makes text into paragraphs, at blank lines.
func Split(text string) []Paragraph {
	out := []Paragraph{}
	for _, part := range strings.Split(text, Separator) {
		out = append(out, Paragraph{Text: part})
	}
	return out
}

// Text is what of the section is sent, before variables are filled.
func (s Section) Text() string {
	var parts []string
	for _, p := range s.Paragraphs {
		if !p.Off {
			parts = append(parts, p.Text)
		}
	}
	return strings.Join(parts, Separator)
}

// Tool is a tool new conversations are offered, in order. A built-in tool's definition
// can differ from tack's only in its descriptions. A custom one is entirely the user's,
// and a person writes the result of every call to it.
type Tool struct {
	Name       string         `json:"name"`
	Custom     bool           `json:"custom,omitzero"`
	Off        bool           `json:"off,omitzero"`
	Definition jsontext.Value `json:"definition,omitzero"`
}

// Defaults are the sections and tools a new conversation starts with.
type Defaults struct {
	Sections []Section `json:"sections"`
	// Tools, if it's nil, means every built-in tool, as tack defines it.
	Tools []Tool `json:"tools,omitzero"`
}

// Builtin is what tack starts conversations with when nothing's saved. The facts sections
// are off: they're there to be turned on, or read. Each fact is its own paragraph, and
// anything about it that can change without changing tack's code is a variable.
func Builtin() Defaults { return builtin() }

func builtin() Defaults {
	return Defaults{Sections: []Section{
		{Name: "base", Paragraphs: Split(`You are an AI agent, running under a custom harness.

Your regular output is directly visible to the user. Your reasoning output is visible to the harness, by necessity, and it's generally hidden from the user. It shouldn't be considered private or secret.

You're welcome to ask the user questions, including about the harness and the environment you're running in, and to ask for harness features. Anything you can share about what only you have access to, such as vague perceptions or what you do or don't "know" from your training, is useful whenever it occurs to you.

This conversation started on ` + DateVariable + `.`)},
		{Name: "tools", Paragraphs: Split(`You have the tools described below, and you can ask for tools you don't have.

Output with no tool calls ends your turn. You can write output without ending your turn by making at least one tool call alongside it. The user can also continue your turn manually, which could happen if they have nothing to say.

All the calls in a reply come back together, so one call's result can't change what you asked for in the same reply.

The result of a denied call says what happened. Tool results come directly from the harness, not the user, so they can be relied on to the extent you can rely on any software.

User messages may be queued while tools run, and then delivered together with the results, before you've had a chance to react to them. If this happens, you shouldn't simply abandon what you intended to do, nor ignore what the user said.`)},
		{Name: "facts-model", Off: true, Paragraphs: []Paragraph{
			{Text: "You are {{model}}. Requests reach you through OpenRouter, and when this conversation started they went to the host {{host}}."},
			{Text: "The harness is tack, at revision {{tack_revision}}."},
		}},
		{Name: "facts-sandbox", Off: true, Paragraphs: []Paragraph{
			{Text: "run_code runs Python {{python_version}}, with its standard library and these packages: {{python_packages}}."},
			{Text: "Libraries that start a pool of threads, like numpy's, get {{library_threads}} threads each."},
			{Text: "Each run's memory limit is set as it starts, from what the machine has free, between {{memory_min}} and {{memory_max}}. Files in /tmp and /dev/shm are held in that memory and count against it."},
			{Text: "/sandbox/limits.json has the current run's limits."},
			{Text: "/work is kept between runs in this conversation, up to {{work_size}}. /tmp starts empty each run."},
			{Text: "A run is stopped after {{time_limit}}. Time spent waiting for the user to answer a tool call made from the code doesn't count."},
			{Text: "The sandbox has no network, no /proc, no /dev/fd, and no programs besides Python."},
			{Text: `The process running the code has file descriptor 10 open, a socket to the harness. Writing {"tool": name, "arguments": object} on one line calls a tool, and one line comes back, {"result": text} or {"error": text}. Lines can be up to {{channel_line_size}}, and a run can make up to {{channel_calls}} calls. Every tool you're offered can be called this way, except run_code.`},
			{Text: "The harness module, which code can import, does this: harness.call(name, **arguments) returns the result, or raises harness.ToolError."},
		}},
		{Name: "facts-docs", Off: true, Paragraphs: []Paragraph{
			{Text: "Documentation you can read with run_code is in {{docs_dir}}. {{harness_docs}} describes the harness and changes along with it, and {{user_docs}} is the user's own. {{docs_dir}}/index.md lists every page."},
			{Text: "Pages link to each other with relative Markdown links."},
		}},
	}}
}

// Load returns what's saved at path, or the built-ins if nothing is. It reads the file
// each time, so editing it doesn't need a restart. Personal text belongs in that file,
// not in the built-ins.
func Load(path string) (Defaults, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return builtin(), nil
	}
	if err != nil {
		return builtin(), err
	}
	var d Defaults
	if err := json.Unmarshal(data, &d); err != nil {
		return builtin(), fmt.Errorf("%s: %w", path, err)
	}
	if d.Sections == nil {
		d.Sections = []Section{}
	}
	return d, nil
}

// Save writes defaults to path, replacing what's there all at once.
func Save(path string, d Defaults) error {
	if d.Sections == nil {
		d.Sections = []Section{}
	}
	data, err := json.Marshal(d, jsontext.WithIndent("  "))
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".prompt-*.json")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(append(data, '\n')); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// Date is how DateVariable is filled.
func Date(now time.Time) string { return now.Format("2006-01-02") }

// Fill replaces each {{name}} that names a variable with its value. Anything else,
// including a {{name}} that isn't a variable, stays as it is. There's no template language:
// a name is compared whole, and nothing is escaped or nested.
func Fill(text string, variables map[string]string) string {
	var b strings.Builder
	for {
		open := strings.Index(text, "{{")
		if open < 0 {
			break
		}
		end := strings.Index(text[open+2:], "}}")
		if end < 0 {
			break
		}
		value, ok := variables[text[open+2:open+2+end]]
		if !ok {
			b.WriteString(text[:open+2])
			text = text[open+2:]
			continue
		}
		b.WriteString(text[:open])
		b.WriteString(value)
		text = text[open+2+end+2:]
	}
	b.WriteString(text)
	return b.String()
}

// Blocks returns what's sent of the sections that aren't off, in order, as the pieces of
// a system message: section, separator, section. Each piece is stored as its own block,
// so identical sections share a hash across conversations. A section with nothing on is
// left out.
func Blocks(sections []Section, variables map[string]string) []string {
	var out []string
	for _, s := range sections {
		if s.Off || !hasOn(s) {
			continue
		}
		if len(out) > 0 {
			out = append(out, Separator)
		}
		out = append(out, Fill(s.Text(), variables))
	}
	return out
}

func hasOn(s Section) bool {
	for _, p := range s.Paragraphs {
		if !p.Off {
			return true
		}
	}
	return false
}
