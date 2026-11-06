package core

import (
	"encoding/json/v2"
	"errors"
	"io"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/betawaffle/tack/internal/store"
)

type sentMessage struct {
	Role       string `json:"role"`
	Content    string `json:"content"`
	Name       string `json:"name"`
	Reasoning  string `json:"reasoning"`
	ToolCallID string `json:"tool_call_id"`
	ToolCalls  []struct {
		ID       string `json:"id"`
		Function struct {
			Name      string `json:"name"`
			Arguments string `json:"arguments"`
		} `json:"function"`
	} `json:"tool_calls"`
}

func lastSent(t *testing.T, f *fake) []sentMessage {
	t.Helper()
	var body struct {
		Messages []sentMessage `json:"messages"`
	}
	if err := json.Unmarshal(f.bodies[len(f.bodies)-1], &body); err != nil {
		t.Fatal(err)
	}
	return body.Messages
}

func TestMessagesOfAnyRoleGoOutWithTheirFields(t *testing.T) {
	f := newFake(t)
	f.handler = func(w http.ResponseWriter) { io.WriteString(w, costStream("0.001")) }
	c := f.open()
	id, _ := c.NewConversation("")
	for _, m := range []Message{
		{Role: "system", Text: "Be brief.", Name: "boss"},
		{Role: "user", Text: "Hi", Name: "andy"},
		{Role: "assistant", Text: "Checking.", Reasoning: "Hmm.", ToolCalls: []MessageCall{{ID: "w1", Name: "echo", Arguments: `{"a":1}`}, {ID: "w2", Name: "echo", Arguments: `{}`}}},
		{Role: "tool", Text: "one", ToolCallID: "w1"},
		{Role: "tool", Text: "two", ToolCallID: "w2"},
		{Role: "developer", Text: "Custom."},
	} {
		if err := c.AddMessage(id, m); err != nil {
			t.Fatalf("%+v: %v", m, err)
		}
	}
	updates, err := c.Reply(id)
	mustFinish(t, updates, err)
	got := lastSent(t, f)
	if len(got) != 6 {
		t.Fatalf("messages = %+v", got)
	}
	if got[0].Name != "boss" || got[1].Name != "andy" {
		t.Errorf("names = %+v", got[:2])
	}
	if a := got[2]; a.Role != "assistant" || a.Reasoning != "Hmm." || len(a.ToolCalls) != 2 || a.ToolCalls[0].Function.Arguments != `{"a":1}` {
		t.Errorf("assistant = %+v", a)
	}
	if got[3].Role != "tool" || got[3].ToolCallID != "w1" || got[4].Content != "two" {
		t.Errorf("tools = %+v", got[3:5])
	}
	if got[5].Role != "developer" {
		t.Errorf("custom = %+v", got[5])
	}
	// tack doesn't run or ask about calls someone wrote.
	if conv, _ := c.Conversation(id); conv.Decision != nil {
		t.Errorf("decision = %+v", conv.Decision)
	}
	// They survive a restart as they were.
	c = f.open()
	conv, _ := c.Conversation(id)
	if a := conv.Items[2]; !a.Written || a.Role != "assistant" || a.Reasoning != "Hmm." || len(a.WrittenCalls) != 2 {
		t.Errorf("after a restart = %+v", a)
	}
}

func TestMessagesTheAPICouldNeverAcceptAreRefused(t *testing.T) {
	f := newFake(t)
	c := f.open()
	id, _ := c.NewConversation("")
	for name, m := range map[string]Message{
		"empty":             {Role: "user", Text: "  "},
		"empty assistant":   {Role: "assistant"},
		"tool, no id":       {Role: "tool", Text: "x"},
		"tool, nothing due": {Role: "tool", Text: "x", ToolCallID: "nope"},
		"tool, no text":     {Role: "tool", ToolCallID: "nope"},
		"call, no id":       {Role: "assistant", ToolCalls: []MessageCall{{Name: "a", Arguments: "{}"}}},
		"call, no name":     {Role: "assistant", ToolCalls: []MessageCall{{ID: "a", Arguments: "{}"}}},
		"call, bad JSON":    {Role: "assistant", ToolCalls: []MessageCall{{ID: "a", Name: "a", Arguments: "{"}}},
		"call, not object":  {Role: "assistant", ToolCalls: []MessageCall{{ID: "a", Name: "a", Arguments: "[]"}}},
		"two the same":      {Role: "assistant", ToolCalls: []MessageCall{{ID: "a", Name: "a", Arguments: "{}"}, {ID: "a", Name: "a", Arguments: "{}"}}},
		"user reasoning":    {Role: "user", Text: "x", Reasoning: "y"},
		"user answering":    {Role: "user", Text: "x", ToolCallID: "y"},
	} {
		if err := c.AddMessage(id, m); !errors.Is(err, ErrBadMessage) {
			t.Errorf("%s: %v", name, err)
		}
	}

	// Once a written call is open, only its answer can come next, and it has to come
	// before the model is called.
	calls := []MessageCall{{ID: "a", Name: "a", Arguments: "{}"}, {ID: "b", Name: "a", Arguments: "{}"}}
	if err := c.AddMessage(id, Message{Role: "assistant", ToolCalls: calls}); err != nil {
		t.Fatal(err)
	}
	if err := c.AddMessage(id, Message{Role: "user", Text: "x"}); !errors.Is(err, ErrBadMessage) {
		t.Errorf("user next: %v", err)
	}
	if conv, _ := c.Conversation(id); !slices.Equal(conv.WaitingToolCalls, []string{"a", "b"}) || conv.CanReply {
		t.Errorf("view = %+v", conv)
	}
	if _, err := c.Reply(id); !errors.Is(err, ErrBadMessage) {
		t.Errorf("reply: %v", err)
	}
	if _, err := c.SendMessage(id, Message{Role: "tool", Text: "x", ToolCallID: "a"}); !errors.Is(err, ErrBadMessage) {
		t.Errorf("sending with one left: %v", err)
	}
	if err := c.AddMessage(id, Message{Role: "tool", Text: "x", ToolCallID: "a"}); err != nil {
		t.Fatal(err)
	}
	updates, err := c.SendMessage(id, Message{Role: "tool", Text: "y", ToolCallID: "b"})
	mustFinish(t, updates, err)
	if n := len(f.bodies); n != 1 {
		t.Errorf("%d calls", n)
	}
}

func TestARepliesAndResultsCanBeChanged(t *testing.T) {
	f := newToolFake(t)
	f.noteTools()
	replies := 0
	f.handler = func(w http.ResponseWriter) {
		replies++
		if replies == 1 {
			io.WriteString(w, toolStream(noteCall))
			return
		}
		io.WriteString(w, replyStream)
	}
	c := f.open()
	id, _ := c.NewConversation("")
	updates, err := c.Send(id, "Hi")
	mustFinish(t, updates, err)
	conv, _ := c.Conversation(id)
	reply := conv.Items[len(conv.Items)-1]
	note := conv.Items[len(conv.Items)-2]
	if reply.Call == "" || note.ToolCalls[0].Result != "ok" {
		t.Fatalf("items = %+v", conv.Items)
	}

	// Held changes do nothing until they're applied.
	textEdit, err := c.ProposeMessageEdit(id, MessageEdit{Call: reply.Call, Text: "Changed.", Reasoning: "Because."})
	if err != nil {
		t.Fatal(err)
	}
	resultEdit, err := c.ProposeMessageEdit(id, MessageEdit{ToolCall: note.ToolCalls[0].ID, Text: "Different."})
	if err != nil {
		t.Fatal(err)
	}
	conv, _ = c.Conversation(id)
	if got := conv.Items[len(conv.Items)-1]; got.Text != "Hello there" || got.Pending == nil || got.Pending.Text != "Changed." {
		t.Errorf("held = %+v", got)
	}
	if err := c.ApplyMessageEdit(id, textEdit); err != nil {
		t.Fatal(err)
	}
	if err := c.ApplyMessageEdit(id, resultEdit); err != nil {
		t.Fatal(err)
	}

	f.replyWith(replyStream)
	updates, err = c.Send(id, "Next")
	mustFinish(t, updates, err)
	got := lastSent(t, f.fake)
	var assistant, tool *sentMessage
	for i := range got {
		if got[i].Content == "Changed." {
			assistant = &got[i]
		}
		if got[i].Role == "tool" {
			tool = &got[i]
		}
	}
	if assistant == nil || assistant.Reasoning != "Because." || tool == nil || tool.Content != "Different." {
		t.Errorf("messages = %+v", got)
	}

	// The record keeps both, and it survives a restart.
	c = f.open()
	conv, _ = c.Conversation(id)
	changed := conv.Items[2]
	if changed.Text != "Changed." || !changed.Edited || !conv.Items[1].ToolCalls[0].ResultEdited {
		t.Errorf("after a restart = %+v", conv.Items)
	}

	// A fork of it keeps the change, without what was only held.
	held, err := c.ProposeMessageEdit(id, MessageEdit{Call: reply.Call, Text: "Not this."})
	if err != nil {
		t.Fatal(err)
	}
	fork, err := c.Fork(id, 3)
	if err != nil {
		t.Fatal(err)
	}
	kept, _ := c.Conversation(fork)
	if kept.Items[2].Text != "Changed." || kept.Items[2].Pending != nil {
		t.Errorf("fork = %+v", kept.Items[2])
	}
	if err := c.DiscardMessageEdit(id, held); err != nil {
		t.Fatal(err)
	}

	for name, e := range map[string]MessageEdit{
		"no such reply": {Call: "nope", Text: "x"},
		"no such call":  {ToolCall: "nope", Text: "x"},
	} {
		if _, err := c.ProposeMessageEdit(id, e); !errors.Is(err, ErrNoMessage) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if _, err := c.ProposeMessageEdit(id, MessageEdit{ToolCall: note.ToolCalls[0].ID}); !errors.Is(err, ErrBadMessage) {
		t.Errorf("an empty result: %v", err)
	}
}

func TestAWrittenMessageCanBeChangedOrEmptied(t *testing.T) {
	f := newFake(t)
	c := f.open()
	id, _ := c.NewConversation("")
	if err := c.AddMessage(id, Message{Role: "assistant", Text: "Forged."}); err != nil {
		t.Fatal(err)
	}
	message := func() Item {
		conv, _ := c.Conversation(id)
		return conv.Items[0]
	}
	edit, err := c.ProposeMessageEdit(id, MessageEdit{Message: message().Message, Text: "Better.", Name: "x"})
	if err != nil {
		t.Fatal(err)
	}
	if err := c.ApplyMessageEdit(id, edit); err != nil {
		t.Fatal(err)
	}
	if m := message(); m.Text != "Better." || m.Name != "x" || !m.Edited {
		t.Errorf("message = %+v", m)
	}
	edit, err = c.ProposeMessageEdit(id, MessageEdit{Message: message().Message})
	if err != nil {
		t.Fatal(err)
	}
	if err := c.ApplyMessageEdit(id, edit); err != nil {
		t.Fatal(err)
	}
	if m := message(); !m.Removed {
		t.Errorf("message = %+v", m)
	}
	c.mu.Lock()
	entries := c.convs[id].context()
	c.mu.Unlock()
	if len(entries) != 0 {
		t.Errorf("still sent: %+v", entries)
	}
}

func TestAModelsRefusalsAreLearned(t *testing.T) {
	f := newFake(t)
	status := http.StatusBadRequest
	f.handler = func(w http.ResponseWriter) {
		if status != http.StatusOK {
			w.WriteHeader(status)
			io.WriteString(w, `{"error":{"message":"no such thing"}}`)
			return
		}
		io.WriteString(w, costStream("0.001"))
	}
	c := f.open()
	id, _ := c.NewConversation("")
	refused := func() []string {
		t.Helper()
		conv, err := c.Conversation(id)
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, r := range conv.Refused {
			out = append(out, r.Feature)
		}
		return out
	}

	// Nothing unusual, so a refusal says nothing.
	updates, err := c.Send(id, "Hi")
	finish(t, updates)
	if err != nil || len(refused()) != 0 {
		t.Fatalf("%v, %v", err, refused())
	}

	// One unusual thing, so it's that.
	updates, err = c.SendMessage(id, Message{Role: "user", Text: "Hi", Name: "andy"})
	if err != nil {
		t.Fatal(err)
	}
	finish(t, updates)
	if got := refused(); !slices.Equal(got, []string{featureName}) {
		t.Errorf("refused = %v", got)
	}
	// It's in the record, not just in memory.
	c = f.open()
	if got := refused(); !slices.Equal(got, []string{featureName}) {
		t.Errorf("after a restart = %v", got)
	}

	// Something already refused explains the next failure, whatever else is in it.
	if err := c.AddMessage(id, Message{Role: "developer", Text: "x"}); err != nil {
		t.Fatal(err)
	}
	updates, err = c.Reply(id)
	if err != nil {
		t.Fatal(err)
	}
	finish(t, updates)
	if got := refused(); !slices.Equal(got, []string{featureName}) {
		t.Errorf("explained = %v", got)
	}

	// It can be asked again. With two unknown things it could be either, so that says
	// nothing, and seeing it work settles it.
	if err := c.ForgetRefusal(id, featureName); err != nil {
		t.Fatal(err)
	}
	if got := refused(); len(got) != 0 {
		t.Errorf("forgotten = %v", got)
	}
	updates, err = c.Reply(id)
	if err != nil {
		t.Fatal(err)
	}
	finish(t, updates)
	if got := refused(); len(got) != 0 {
		t.Errorf("two unknown = %v", got)
	}
	status = http.StatusOK
	updates, err = c.Reply(id)
	mustFinish(t, updates, err)
	status = http.StatusBadRequest
	updates, err = c.Send(id, "Again")
	if err != nil {
		t.Fatal(err)
	}
	finish(t, updates)
	if got := refused(); len(got) != 0 {
		t.Errorf("after seeing it work = %v", got)
	}

	// A server error isn't a refusal.
	status = http.StatusBadGateway
	if err := c.ForgetRefusal(id, featureName); err != nil {
		t.Fatal(err)
	}
	if err := c.ForgetRefusal(id, "role:developer"); err != nil {
		t.Fatal(err)
	}
	updates, err = c.Send(id, "Again")
	if err != nil {
		t.Fatal(err)
	}
	finish(t, updates)
	if got := refused(); len(got) != 0 {
		t.Errorf("a server error = %v", got)
	}
}

func TestSomeRefusalsAreKnownAhead(t *testing.T) {
	f := newFake(t)
	f.settings = store.Settings{Model: "anthropic/claude"}
	c := f.open()
	id, _ := c.NewConversation("")
	conv, _ := c.Conversation(id)
	if len(conv.Refused) != 1 || conv.Refused[0].Feature != featureReasoning || conv.Refused[0].Learned {
		t.Errorf("refused = %+v", conv.Refused)
	}
}

func TestARemovedToolGetsAFailedResult(t *testing.T) {
	f := newToolFake(t)
	gone := Tool{Name: "gone", Definition: []byte(`{"type":"function","function":{"name":"gone","parameters":{"type":"object","properties":{}}}}`), Run: f.tools[0].Run}
	f.tools = append(f.tools, gone)
	c := f.open()
	id, _ := c.NewConversation("")
	waiting, _ := c.NewConversation("")
	replies := 0
	f.handler = func(w http.ResponseWriter) {
		replies++
		if replies == 1 {
			io.WriteString(w, toolStream(wireCall{"call-w", "gone", `{}`}))
			return
		}
		io.WriteString(w, replyStream)
	}
	// One's left waiting on a person when the tool goes away.
	updates, err := c.Send(waiting, "Use it")
	mustFinish(t, updates, err)
	if conv, _ := c.Conversation(waiting); conv.Decision == nil {
		t.Fatalf("expected a decision: %+v", conv)
	}

	f.tools = f.tools[:len(f.tools)-1]
	c = f.open()
	if err := c.Recover(); err != nil {
		t.Fatal(err)
	}
	conv, _ := c.Conversation(waiting)
	if conv.Decision != nil || !strings.Contains(conv.Items[len(conv.Items)-1].ToolCalls[0].Result, "no longer available") {
		t.Errorf("waiting = %+v", conv)
	}

	// One calls it afterwards. It's in the definitions it still sends.
	before := len(f.bodies)
	replies = 0
	updates, err = c.Send(id, "Use it")
	mustFinish(t, updates, err)
	if n := len(f.bodies) - before; n != 2 {
		t.Fatalf("%d calls", n)
	}
	conv, _ = c.Conversation(id)
	if conv.Decision != nil || conv.Items[len(conv.Items)-1].Text != "Hello there" {
		t.Errorf("view = %+v", conv.Items)
	}
	if got := conv.Items[len(conv.Items)-2].ToolCalls[0]; got.State != "unavailable" || !strings.Contains(got.Result, "no longer available") {
		t.Errorf("call = %+v", got)
	}
}
