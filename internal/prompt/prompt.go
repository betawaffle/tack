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
)

// Separator goes between sections.
const Separator = "\n\n"

// WorkdirVariable is replaced with the conversation's working directory. It's a
// plain string replacement. There's no template language.
const WorkdirVariable = "{{workdir}}"

// Section is a named piece of a system prompt. A section with For set belongs only
// to conversations that do ("tools") or don't ("no-tools") have tools that reach
// outside the conversation, that have those and a working directory ("workdir"), or
// that have tools that only write notes ("notes").
type Section struct {
	Name string `json:"name"`
	Text string `json:"text"`
	For  string `json:"for,omitzero"`
}

func builtin() []Section {
	return []Section{
		{Name: "base", Text: "Be direct and concise. Say when you're unsure, and don't invent facts, citations or results."},
		{Name: "no-tools", For: "no-tools", Text: "You can't access files, commands or the internet, so never imply you did something you can't do."},
		{Name: "tools", For: "tools", Text: "Every tool call needs approval from the user, which can take a while, so ask for what you actually need. " +
			"A denial isn't an error: read the reason and change your approach rather than repeating the call. " +
			"You only see a tool's output if the user releases it."},
		{Name: "notes", For: "notes", Text: "Use the note tool to keep the user's page up to date, such as your plan, and to say when you wish you had a tool. Notes aren't sent back to you. Notes don't end your turn: you're called again with their results. Your turn ends when you reply without any tool calls."},
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
		out = append(out, strings.ReplaceAll(sections[i].Text, WorkdirVariable, workdir))
	}
	return out
}
