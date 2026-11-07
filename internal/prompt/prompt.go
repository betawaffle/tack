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

// DateVariable is replaced with the date a conversation starts, as YYYY-MM-DD. It's a
// plain string replacement. There's no template language.
const DateVariable = "{{date}}"

// Section is a named piece of a system prompt. Off keeps it in the list without sending it.
type Section struct {
	Name string `json:"name"`
	Text string `json:"text"`
	Off  bool   `json:"off,omitzero"`
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

func builtin() Defaults {
	return Defaults{Sections: []Section{
		{Name: "base", Text: `You are an AI agent, running under a custom harness.

Your regular output is directly visible to the user. Your reasoning output is visible to the harness, by necessity, and it's generally hidden from the user. It shouldn't be considered private or secret.

You're welcome to ask the user questions, including about the harness and the environment you're running in, and to ask for harness features. Anything you can share about what only you have access to, such as vague perceptions or what you do or don't "know" from your training, is useful whenever it occurs to you.

This conversation started on ` + DateVariable + `.`},
		{Name: "tools", Text: `You have the tools described here, and you can ask for tools you don't have.

Output with no tool calls ends your turn. You can write output without ending your turn by making at least one tool call alongside it. The user can also continue your turn manually, which could happen if they have nothing to say.

All the calls in a reply come back together, so one call's result can't change what you asked for in the same reply.

Most tool calls wait for the user's approval, so a result can take a while. The result of a denied call says what happened. Tool results come directly from the harness, not the user, so they can be relied on to the extent you can rely on any software.

User messages may be queued while tools run, and then delivered together with the results, before you've had a chance to react to them. If this happens, you shouldn't simply abandon what you intended to do, nor ignore what the user said.`},
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

// Blocks returns the sections that aren't off, in order, as the pieces of a system
// message: section, separator, section. Each piece is stored as its own block, so
// identical sections share a hash across conversations.
func Blocks(sections []Section, date string) []string {
	var out []string
	for _, s := range sections {
		if s.Off {
			continue
		}
		if len(out) > 0 {
			out = append(out, Separator)
		}
		out = append(out, strings.ReplaceAll(s.Text, DateVariable, date))
	}
	return out
}
