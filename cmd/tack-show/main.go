// Command tack-show prints a conversation from the record, for reading from a terminal
// or by an agent. It only reads, so it's safe to run beside a live tack.
//
//	tack-show                 list conversations
//	tack-show last            the newest conversation
//	tack-show teapot          a conversation by part of its first message, or its ID
//	tack-show -full teapot    without shortening anything
//	tack-show -sent teapot    also what each model call actually sent
//	tack-show -events teapot  also its raw events
package main

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"slices"
	"strings"

	"github.com/betawaffle/tack/internal/config"
	"github.com/betawaffle/tack/internal/core"
	"github.com/betawaffle/tack/internal/store"
)

func main() {
	log.SetFlags(0)
	defaultConfig, err := config.DefaultPath()
	if err != nil {
		log.Fatal(err)
	}
	configPath := flag.String("config", defaultConfig, "config file")
	dataDir := flag.String("data", "", "data directory (default: from the config)")
	full := flag.Bool("full", false, "don't shorten long text")
	sent := flag.Bool("sent", false, "also show what each model call sent")
	events := flag.Bool("events", false, "also show the conversation's raw events")
	flag.Parse()

	dir := *dataDir
	if dir == "" {
		cfg, err := config.Load(*configPath, false)
		if err != nil {
			log.Fatal(err)
		}
		dir = cfg.Data
	}
	st, err := store.OpenReadOnly(dir)
	if err != nil {
		log.Fatal(err)
	}
	c, err := core.New(core.Config{Store: st})
	if err != nil {
		log.Fatal(err)
	}
	limit := 1500
	if *full {
		limit = 0
	}
	if err := run(os.Stdout, st, c, strings.Join(flag.Args(), " "), limit, *sent, *events); err != nil {
		log.Fatal(err)
	}
}

func run(w io.Writer, st *store.Store, c *core.Core, query string, limit int, sent, events bool) error {
	all := c.Conversations()
	if query == "" {
		list(w, all)
		return nil
	}
	matches, err := find(all, query)
	if err != nil {
		return err
	}
	if len(matches) > 1 {
		fmt.Fprintf(w, "%q matches %d conversations:\n", query, len(matches))
		list(w, matches)
		return nil
	}
	conv, err := c.Conversation(matches[0].ID)
	if err != nil {
		return err
	}
	show(w, conv, limit)
	if sent {
		if err := showSent(w, st, conv.ID, limit); err != nil {
			return err
		}
	}
	if events {
		return showEvents(w, st, conv.ID)
	}
	return nil
}

func list(w io.Writer, summaries []core.Summary) {
	for _, s := range summaries {
		fmt.Fprintf(w, "%s  %s  %s\n", s.ID, s.Created.Format("2006-01-02 15:04"), s.Preview)
	}
}

// find takes "last", an exact ID or the start of one, or part of the first message.
func find(all []core.Summary, query string) ([]core.Summary, error) {
	if len(all) == 0 {
		return nil, errors.New("there are no conversations")
	}
	if query == "last" {
		return all[:1], nil // newest first
	}
	var byID, byText []core.Summary
	for _, s := range all {
		if strings.HasPrefix(s.ID, query) {
			byID = append(byID, s)
		}
		if strings.Contains(strings.ToLower(s.Preview), strings.ToLower(query)) {
			byText = append(byText, s)
		}
	}
	if len(byID) > 0 {
		return byID, nil
	}
	if len(byText) > 0 {
		return byText, nil
	}
	return nil, fmt.Errorf("no conversation matches %q", query)
}

// shorten keeps the start and end, and says what's missing.
func shorten(text string, limit int) string {
	// Cutting a little isn't worth the marker.
	if limit <= 0 || len(text) <= limit+limit/4 {
		return text
	}
	return fmt.Sprintf("%s\n[... %d bytes cut, -full shows them ...]\n%s", strings.ToValidUTF8(text[:limit*2/3], ""), len(text)-limit, strings.ToValidUTF8(text[len(text)-limit/3:], ""))
}

func show(w io.Writer, conv core.Conversation, limit int) {
	fmt.Fprintf(w, "Conversation %s\n", conv.ID)
	fmt.Fprintf(w, "model %s on %s", conv.Model, conv.Host)
	if conv.Workdir != "" {
		fmt.Fprintf(w, ", workdir %s", conv.Workdir)
	}
	if conv.Tools {
		fmt.Fprint(w, ", tools")
	}
	fmt.Fprintf(w, ", spent $%.6f of $%.6f\n", conv.Budget.Spent, conv.Budget.Limit)
	var states []string
	if conv.Busy {
		states = append(states, "busy")
	}
	if conv.Paused {
		states = append(states, "paused")
	}
	if conv.Budget.Stopped {
		states = append(states, "stopped by budget")
	}
	if conv.CanReply {
		states = append(states, "can reply")
	}
	if conv.Decision != nil {
		states = append(states, "waiting for a decision: "+conv.Decision.Kind)
	}
	if len(states) > 0 {
		fmt.Fprintf(w, "state: %s\n", strings.Join(states, ", "))
	}
	for _, note := range conv.Notes {
		fmt.Fprintf(w, "note %s/%s", note.Kind, note.Key)
		if note.Status != "" {
			fmt.Fprintf(w, " [%s]", note.Status)
		}
		fmt.Fprintf(w, ": %s\n", shorten(note.Text, limit))
	}
	for i, item := range conv.Items {
		fmt.Fprintf(w, "\n--- %d. %s%s\n", i, item.Role, itemTags(item))
		if item.Reasoning != "" {
			fmt.Fprintf(w, "[reasoning] %s\n[/reasoning]\n", shorten(item.Reasoning, limit))
		}
		if item.Text != "" {
			fmt.Fprintln(w, shorten(item.Text, limit))
		}
		if item.Pending != nil {
			fmt.Fprintf(w, "[unused edit] %s\n", shorten(item.Pending.Text, limit))
		}
		for _, call := range item.ToolCalls {
			fmt.Fprintf(w, "[tool %s, %s] %s\n", call.Name, call.State, shorten(call.Arguments, limit))
			if len(call.Flags) > 0 {
				fmt.Fprintf(w, "  flags: %s\n", strings.Join(call.Flags, "; "))
			}
			if call.Invalid != "" {
				fmt.Fprintf(w, "  invalid: %s\n", call.Invalid)
			}
			if call.Error != "" {
				fmt.Fprintf(w, "  error: %s\n", call.Error)
			}
			if call.HasOutput {
				fmt.Fprintf(w, "  output: %s\n", shorten(call.Output, limit))
			}
			if call.Result != "" && call.Result != call.Output {
				fmt.Fprintf(w, "  the model was told: %s\n", shorten(call.Result, limit))
			}
		}
		if s := item.Stats; s != nil {
			fmt.Fprintf(w, "[%s, %d in, %d cached, %d out, $%.6f]\n", s.Host, s.PromptTokens, s.CachedTokens, s.CompletionTokens, s.CostUSD)
		}
	}
}

func itemTags(item core.Item) string {
	var tags []string
	if item.Queued {
		tags = append(tags, "not sent yet")
	}
	if item.Edited {
		tags = append(tags, "edited")
	}
	if item.Removed {
		tags = append(tags, "removed")
	}
	if len(tags) == 0 {
		return ""
	}
	return " (" + strings.Join(tags, ", ") + ")"
}

// showSent prints what went over the wire, which is the truth about roles and order.
func showSent(w io.Writer, st *store.Store, id string, limit int) error {
	n := 0
	lastDefinitions := ""
	return st.Replay(func(ev *store.Event) error {
		if ev.Conversation != id || ev.Type != store.CallStarted {
			return nil
		}
		data, err := st.Get(ev.Request)
		if err != nil {
			return err
		}
		var request struct {
			Model    string `json:"model"`
			Messages []struct {
				Role       string `json:"role"`
				Content    string `json:"content"`
				Reasoning  string `json:"reasoning"`
				ToolCallID string `json:"tool_call_id"`
				ToolCalls  []struct {
					Function struct {
						Name      string `json:"name"`
						Arguments string `json:"arguments"`
					} `json:"function"`
				} `json:"tool_calls"`
			} `json:"messages"`
			Tools []jsontext.Value `json:"tools"`
		}
		if err := json.Unmarshal(data, &request); err != nil {
			return err
		}
		var tools []string
		var definitions strings.Builder
		for _, tool := range request.Tools {
			var named struct {
				Function struct {
					Name string `json:"name"`
				} `json:"function"`
			}
			if err := json.Unmarshal(tool, &named); err != nil {
				return err
			}
			tools = append(tools, named.Function.Name)
			fmt.Fprintf(&definitions, "tool: %s\n", tool)
		}
		fmt.Fprintf(w, "\n=== call %d, %s, %d bytes, tools %v\n", n, request.Model, len(data), slices.Clip(tools))
		// They rarely change, so they only show when they do.
		if definitions.String() != lastDefinitions {
			lastDefinitions = definitions.String()
			fmt.Fprint(w, lastDefinitions)
		}
		for _, m := range request.Messages {
			fmt.Fprintf(w, "%s", m.Role)
			if m.ToolCallID != "" {
				fmt.Fprintf(w, " (%s)", m.ToolCallID)
			}
			fmt.Fprintf(w, ": %s\n", shorten(m.Content, limit/3))
			if m.Reasoning != "" {
				fmt.Fprintf(w, "  reasoning: %s\n", shorten(m.Reasoning, limit/3))
			}
			for _, call := range m.ToolCalls {
				fmt.Fprintf(w, "  calls %s %s\n", call.Function.Name, shorten(call.Function.Arguments, limit/3))
			}
		}
		n++
		return nil
	}, nil)
}

func showEvents(w io.Writer, st *store.Store, id string) error {
	fmt.Fprintln(w, "\n=== events")
	return st.Replay(func(ev *store.Event) error {
		if ev.Conversation != id {
			return nil
		}
		line, err := json.Marshal(ev, json.Deterministic(true))
		if err != nil {
			return err
		}
		fmt.Fprintln(w, string(line))
		return nil
	}, nil)
}
