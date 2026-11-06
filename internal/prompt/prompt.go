// Package prompt holds the sections that system prompts are built from. They're
// joined into an ordinary system message when a conversation starts and never
// rendered again, because changing the start of a context misses the whole cache.
package prompt

import (
	"errors"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

// Separator goes between sections.
const Separator = "\n\n"

// WorkdirVariable is replaced with the conversation's working directory. It's a
// plain string replacement. There's no template language.
const WorkdirVariable = "{{workdir}}"

// DateVariable is replaced with the date, when a conversation starts.
const DateVariable = "{{date}}"

// Section is a named piece of a system prompt. A section with For set belongs only
// to conversations that do ("tools") or don't ("no-tools") have tools that reach
// outside the conversation, or that have those and a working directory ("workdir").
type Section struct {
	Name string `json:"name"`
	Text string `json:"text"`
	For  string `json:"for,omitzero"`
}

func builtin() []Section {
	return []Section{
		{Name: "base", Text: `You are an AI agent, running under a custom harness.

Your regular output is directly visible to the user. Your reasoning output is visible to the harness, by necessity, and it's generally hidden from the user. It shouldn't be considered private or secret.

You're welcome to ask the user questions, including about the harness and the environment you're running in, and to ask for harness features. Anything you can share about what only you have access to, such as vague perceptions or what you do or don't "know" from your training, is useful whenever it occurs to you.

This conversation started on ` + DateVariable + `.`},
		{Name: "no-tools", For: "no-tools", Text: "You don't have access to any tools, but you can ask for tools you wish you had."},
		{Name: "tools", For: "tools", Text: `You have the tools described here, and you can ask for tools you don't have.

Output with no tool calls ends your turn. You can write output without ending your turn by making at least one tool call alongside it. The user can also continue your turn manually, which could happen if they have nothing to say.

All the calls in a reply come back together, so one call's result can't change what you asked for in the same reply.

Most tool calls wait for the user's approval, so a result can take a while. The result of a denied call says what happened. Tool results come directly from the harness, not the user, so they can be relied on to the extent you can rely on any software.

User messages may be queued while tools run, and then delivered together with the results, before you've had a chance to react to them. If this happens, you shouldn't simply abandon what you intended to do, nor ignore what the user said.`},
		{Name: "workdir", For: "workdir", Text: "Relative paths and commands start from the working directory, " + WorkdirVariable + "."},
	}
}

// Load returns the built-in sections with dir's files applied. <name>.md replaces
// the section of that name, an empty file removes it, and any other name adds a
// section at the end. It reads the files each time, so editing them doesn't need a
// restart. Personal text belongs in those files, not in the built-ins.
func Load(dir string) []Section {
	sections := builtin()
	entries, err := os.ReadDir(dir)
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			log.Printf("reading prompt sections: %v", err)
		}
		return sections
	}
	var names []string
	for _, e := range entries {
		if name, ok := strings.CutSuffix(e.Name(), ".md"); ok && !e.IsDir() {
			names = append(names, name)
		}
	}
	slices.Sort(names)
	for _, name := range names {
		data, err := os.ReadFile(filepath.Join(dir, name+".md"))
		if err != nil {
			log.Printf("reading prompt section: %v", err)
			continue
		}
		text := strings.TrimSpace(string(data))
		i := slices.IndexFunc(sections, func(s Section) bool { return s.Name == name })
		switch {
		case i >= 0 && text == "":
			sections = slices.Delete(sections, i, i+1)
		case i >= 0:
			sections[i].Text = text
		case text != "":
			sections = append(sections, Section{Name: name, Text: text})
		}
	}
	return sections
}

// Blocks returns the named sections, in the order given, as the pieces of a system
// message: section, separator, section. Each piece is stored as its own block, so
// identical sections share a hash across conversations. Unknown names are skipped.
func Blocks(sections []Section, names []string, workdir string) []string {
	var out []string
	for _, name := range names {
		i := slices.IndexFunc(sections, func(s Section) bool { return s.Name == name })
		if i < 0 {
			continue
		}
		if len(out) > 0 {
			out = append(out, Separator)
		}
		text := strings.ReplaceAll(sections[i].Text, WorkdirVariable, workdir)
		out = append(out, strings.ReplaceAll(text, DateVariable, time.Now().Format("January 2, 2006")))
	}
	return out
}
