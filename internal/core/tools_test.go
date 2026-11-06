package core

import (
	"bytes"
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/betawaffle/tack/internal/openrouter"
	"github.com/betawaffle/tack/internal/store"
)

const definition = `{"type":"function","function":{"name":"echo","description":"Echo text.","parameters":{"type":"object","properties":{"text":{"type":"string"}}}}}`

// toolFake has an "echo" tool that exists only in tests, and counts its runs.
type toolFake struct {
	*fake
	runs   []string
	output func(args string) ([]byte, error)
}

func newToolFake(t *testing.T) *toolFake {
	f := &toolFake{fake: newFake(t)}
	f.output = func(args string) ([]byte, error) { return []byte("echoed " + args), nil }
	f.tools = []Tool{{
		Name:       "echo",
		Definition: jsontext.Value(definition),
		Run: func(_ context.Context, call Call) ([]byte, error) {
			var text string
			json.Unmarshal(call.Args["text"], &text)
			f.runs = append(f.runs, text)
			return f.output(text)
		},
	}}
	return f
}

type wireCall struct{ id, name, args string }

// toolStream replies with tool calls, with the arguments arriving in pieces as
// they do from a real host.
func toolStream(calls ...wireCall) string {
	var b strings.Builder
	b.WriteString(`data: {"id":"gen-1","provider":"Morph","choices":[{"delta":{"reasoning":"Think"}}]}` + "\n\n")
	b.WriteString(`data: {"id":"gen-1","provider":"Morph","choices":[{"delta":{"reasoning":"ing."}}]}` + "\n\n")
	for i, call := range calls {
		fmt.Fprintf(&b, `data: {"id":"gen-1","choices":[{"delta":{"tool_calls":[{"index":%d,"id":%q,"type":"function","function":{"name":%q,"arguments":""}}]}}]}`+"\n\n", i, call.id, call.name)
		half := len(call.args) / 2
		for _, piece := range []string{call.args[:half], call.args[half:]} {
			quoted, _ := json.Marshal(piece)
			fmt.Fprintf(&b, `data: {"id":"gen-1","choices":[{"delta":{"tool_calls":[{"index":%d,"function":{"arguments":%s}}]}}]}`+"\n\n", i, quoted)
		}
	}
	b.WriteString(`data: {"id":"gen-1","choices":[{"delta":{"content":""},"finish_reason":"tool_calls"}]}` + "\n\n")
	b.WriteString("data: [DONE]\n\n")
	return b.String()
}

func (f *toolFake) replyWith(stream string) {
	f.handler = func(w http.ResponseWriter) { io.WriteString(w, stream) }
}

// askForTools starts a conversation in which the model has asked for calls.
func (f *toolFake) askForTools(c *Core, calls ...wireCall) string {
	f.t.Helper()
	id, err := c.NewConversation("Be brief.")
	if err != nil {
		f.t.Fatal(err)
	}
	f.replyWith(toolStream(calls...))
	updates, err := c.Send(id, "Use your tools.")
	mustFinish(f.t, updates, err)
	f.replyWith(replyStream)
	return id
}

func (f *toolFake) decision(c *Core, id, kind string) *DecisionView {
	f.t.Helper()
	conv, err := c.Conversation(id)
	if err != nil {
		f.t.Fatal(err)
	}
	if conv.Decision == nil || conv.Decision.Kind != kind {
		f.t.Fatalf("decision = %+v, want kind %s", conv.Decision, kind)
	}
	return conv.Decision
}

func (f *toolFake) decide(c *Core, id, kind, text string, allow ...string) {
	f.t.Helper()
	d := f.decision(c, id, kind)
	updates, err := c.Decide(id, d.ID, allow, text)
	mustFinish(f.t, updates, err)
}

func (f *toolFake) calls(c *Core, id string) []ToolCallView {
	f.t.Helper()
	conv, err := c.Conversation(id)
	if err != nil {
		f.t.Fatal(err)
	}
	var out []ToolCallView
	for _, it := range conv.Items {
		out = append(out, it.ToolCalls...)
	}
	return out
}

func toolMsg(id, text string) openrouter.Message {
	return openrouter.Message{Role: "tool", ToolCallID: id, Content: text}
}

func assistantCalls(calls ...wireCall) openrouter.Message {
	m := openrouter.Message{Role: "assistant", Reasoning: "Thinking."}
	for _, call := range calls {
		m.ToolCalls = append(m.ToolCalls, openrouter.ToolCall{ID: call.id, Type: "function", Function: openrouter.FunctionCall{Name: call.name, Arguments: call.args}})
	}
	return m
}

var (
	callA = wireCall{"call-a", "echo", `{"text":"a"}`}
	callB = wireCall{"call-b", "echo", `{"text":"b"}`}
)

func TestToolCallsWaitForADecision(t *testing.T) {
	f := newToolFake(t)
	c := f.open()
	id := f.askForTools(c, callA, callB)

	if len(f.runs) != 0 {
		t.Fatalf("ran without approval: %v", f.runs)
	}
	if d := f.decision(c, id, store.DecideCalls); !reflect.DeepEqual(d.Calls, []string{"call-a", "call-b"}) {
		t.Errorf("calls = %v", d.Calls)
	}
	if _, err := c.Send(id, "hello?"); !errors.Is(err, ErrAwaiting) {
		t.Errorf("Send = %v", err)
	}
	if _, err := c.Reply(id); !errors.Is(err, ErrAwaiting) {
		t.Errorf("Reply = %v", err)
	}
	conv, _ := c.Conversation(id)
	if conv.CanReply {
		t.Error("CanReply while waiting")
	}
	got := f.calls(c, id)
	if len(got) != 2 || got[0].Arguments != callA.args || got[0].State != "waiting" {
		t.Errorf("calls = %+v", got)
	}
	if n := len(f.bodies); n != 1 {
		t.Errorf("%d model calls", n)
	}

	// The wait survives a restart.
	before := f.decision(c, id, store.DecideCalls).ID
	c = f.open()
	if after := f.decision(c, id, store.DecideCalls).ID; after != before {
		t.Errorf("decision changed across a restart: %s, %s", before, after)
	}
}

func TestDenyingEverythingIsTheDefault(t *testing.T) {
	f := newToolFake(t)
	c := f.open()
	id := f.askForTools(c, callA, callB)
	f.decide(c, id, store.DecideCalls, "")

	if len(f.runs) != 0 {
		t.Errorf("ran: %v", f.runs)
	}
	f.wantMessages(1, sys("Be brief."), user("Use your tools."), assistantCalls(callA, callB),
		toolMsg("call-a", DefaultCallDenial), toolMsg("call-b", DefaultCallDenial))
	for _, call := range f.calls(c, id) {
		if call.State != "denied" || call.Result != DefaultCallDenial {
			t.Errorf("call = %+v", call)
		}
	}
	conv, _ := c.Conversation(id)
	if conv.Decision != nil || conv.Items[len(conv.Items)-1].Text != "Hello there" {
		t.Errorf("after deciding: %+v", conv)
	}
}

func TestDenialTextIsWhatTheModelSees(t *testing.T) {
	f := newToolFake(t)
	c := f.open()
	id := f.askForTools(c, callA)
	f.decide(c, id, store.DecideCalls, "Not that one, try the docs.")
	f.wantMessages(1, sys("Be brief."), user("Use your tools."), assistantCalls(callA),
		toolMsg("call-a", "Not that one, try the docs."))
}

func TestApprovedCallsRunButOutputHasItsOwnGate(t *testing.T) {
	f := newToolFake(t)
	c := f.open()
	id := f.askForTools(c, callA, callB)
	f.decide(c, id, store.DecideCalls, "", "call-b")

	if !reflect.DeepEqual(f.runs, []string{"b"}) {
		t.Fatalf("runs = %v", f.runs)
	}
	if n := len(f.bodies); n != 1 {
		t.Fatalf("the model was called before the output was released: %d calls", n)
	}
	got := f.calls(c, id)
	if got[0].State != "denied" || got[1].State != "finished" || got[1].Output != "echoed b" {
		t.Errorf("calls = %+v", got)
	}
	if d := f.decision(c, id, store.DecideOutput); !reflect.DeepEqual(d.Calls, []string{"call-b"}) {
		t.Errorf("decision = %+v", d)
	}

	// The gate survives a restart, and nothing runs twice.
	c = f.open()
	f.decide(c, id, store.DecideOutput, "", "call-b")
	if len(f.runs) != 1 {
		t.Errorf("runs = %v", f.runs)
	}
	f.wantMessages(1, sys("Be brief."), user("Use your tools."), assistantCalls(callA, callB),
		toolMsg("call-a", DefaultCallDenial), toolMsg("call-b", "echoed b"))
	if got := f.calls(c, id); got[1].State != "sent" {
		t.Errorf("state = %s", got[1].State)
	}
}

func TestOutputCanBeWithheld(t *testing.T) {
	f := newToolFake(t)
	c := f.open()
	id := f.askForTools(c, callA)
	f.decide(c, id, store.DecideCalls, "", "call-a")
	f.decide(c, id, store.DecideOutput, "")
	f.wantMessages(1, sys("Be brief."), user("Use your tools."), assistantCalls(callA), toolMsg("call-a", DefaultOutputDenial))
	if strings.Contains(string(f.bodies[1]), "echoed") {
		t.Error("withheld output reached the model")
	}
	if got := f.calls(c, id); got[0].Output != "echoed a" || got[0].State != "denied" {
		t.Errorf("the output should stay in the record: %+v", got[0])
	}
}

func TestReasoningAndArgumentsAreReplayedExactly(t *testing.T) {
	f := newToolFake(t)
	c := f.open()
	spaced := wireCall{"call-s", "echo", "{ \"text\" :  \"a\\u00e9\" }"}
	id := f.askForTools(c, spaced)
	f.decide(c, id, store.DecideCalls, "")
	var request struct {
		Messages []struct {
			Reasoning string `json:"reasoning"`
			ToolCalls []struct {
				Function struct {
					Arguments string `json:"arguments"`
				} `json:"function"`
			} `json:"tool_calls"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(f.bodies[1], &request); err != nil {
		t.Fatal(err)
	}
	m := request.Messages[2]
	if m.Reasoning != "Thinking." || m.ToolCalls[0].Function.Arguments != spaced.args {
		t.Errorf("message = %+v", m)
	}
}

func TestPlainRepliesStillDontSendReasoning(t *testing.T) {
	f := newToolFake(t)
	c := f.open()
	id, _ := c.NewConversation("")
	updates, err := c.Send(id, "one")
	mustFinish(t, updates, err)
	updates, err = c.Send(id, "two")
	mustFinish(t, updates, err)
	if strings.Contains(string(f.bodies[1]), `"reasoning"`) {
		t.Errorf("request = %s", f.bodies[1])
	}
}

func TestArgumentsAndOutputStayOutOfEventLines(t *testing.T) {
	f := newToolFake(t)
	c := f.open()
	id := f.askForTools(c, wireCall{"call-x", "echo", `{"text":"sensitive-argument"}`})
	f.output = func(string) ([]byte, error) { return []byte("sensitive-output"), nil }
	f.decide(c, id, store.DecideCalls, "", "call-x")
	f.decide(c, id, store.DecideOutput, "", "call-x")
	matches, _ := filepath.Glob(filepath.Join(f.dataDir, "events", "*.jsonl"))
	data, err := os.ReadFile(matches[0])
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"sensitive-argument", "sensitive-output"} {
		if strings.Contains(string(data), secret) {
			t.Errorf("%s is inline in an event line", secret)
		}
	}
}

func TestDefinitionsComeFromTheRecord(t *testing.T) {
	f := newToolFake(t)
	c := f.open()
	id, err := c.NewConversation("")
	if err != nil {
		t.Fatal(err)
	}
	updates, err := c.Send(id, "one")
	mustFinish(t, updates, err)

	// A newer build changes the tool's description.
	f.tools[0].Definition = jsontext.Value(strings.Replace(definition, "Echo text.", "Echo some text.", 1))
	c = f.open()
	updates, err = c.Send(id, "two")
	mustFinish(t, updates, err)
	for i, body := range f.bodies {
		if !strings.Contains(string(body), `"tools":[`+definition+`]`) {
			t.Errorf("request %d = %s", i, body)
		}
	}
	// A new conversation gets the new definitions.
	id, _ = c.NewConversation("")
	updates, err = c.Send(id, "three")
	mustFinish(t, updates, err)
	if !strings.Contains(string(f.bodies[2]), "Echo some text.") {
		t.Errorf("request = %s", f.bodies[2])
	}
}

func TestInvalidCallsCantBeApproved(t *testing.T) {
	cases := []struct {
		name string
		call wireCall
	}{
		{"truncated", wireCall{"call-x", "echo", `{"text":"a`}},
		{"duplicate keys", wireCall{"call-x", "echo", `{"text":"a","text":"b"}`}},
		{"not an object", wireCall{"call-x", "echo", `[1]`}},
		{"null", wireCall{"call-x", "echo", `null`}},
		{"empty", wireCall{"call-x", "echo", ``}},
		{"unknown tool", wireCall{"call-x", "rm", `{}`}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newToolFake(t)
			c := f.open()
			id := f.askForTools(c, tc.call)

			// It still goes to a person, so a model can't loop unattended.
			d := f.decision(c, id, store.DecideCalls)
			if n := len(f.bodies); n != 1 {
				t.Fatalf("%d model calls", n)
			}
			if _, err := c.Decide(id, d.ID, []string{"call-x"}, ""); !errors.Is(err, ErrBadChoice) {
				t.Fatalf("Decide = %v", err)
			}
			got := f.calls(c, id)[0]
			if got.Invalid == "" || got.Arguments != tc.call.args {
				t.Errorf("call = %+v", got)
			}
			f.decide(c, id, store.DecideCalls, "ignored for invalid calls")
			if len(f.runs) != 0 {
				t.Errorf("runs = %v", f.runs)
			}
			m := f.messagesIn(1)
			if last := m[len(m)-1]; last.Role != "tool" || !strings.Contains(last.Content, got.Invalid) {
				t.Errorf("result = %+v", last)
			}
		})
	}
}

func TestDecisionsAreAnsweredOnce(t *testing.T) {
	f := newToolFake(t)
	c := f.open()
	id := f.askForTools(c, callA)
	d := f.decision(c, id, store.DecideCalls)
	if _, err := c.Decide(id, "nope", nil, ""); !errors.Is(err, ErrNoDecision) {
		t.Errorf("wrong id = %v", err)
	}
	if _, err := c.Decide(id, d.ID, []string{"call-z"}, ""); !errors.Is(err, ErrBadChoice) {
		t.Errorf("unknown call = %v", err)
	}
	updates, err := c.Decide(id, d.ID, nil, "")
	mustFinish(t, updates, err)
	if _, err := c.Decide(id, d.ID, nil, ""); !errors.Is(err, ErrNoDecision) {
		t.Errorf("second answer = %v", err)
	}
}

// cutRecord leaves the record as a stop would, just before the first event of
// the given type.
func (f *toolFake) cutRecord(eventType string) {
	f.t.Helper()
	matches, _ := filepath.Glob(filepath.Join(f.dataDir, "events", "*.jsonl"))
	data, err := os.ReadFile(matches[0])
	if err != nil {
		f.t.Fatal(err)
	}
	var kept []string
	for _, line := range strings.SplitAfter(string(data), "\n") {
		if strings.Contains(line, `"type":"`+eventType+`"`) {
			break
		}
		kept = append(kept, line)
	}
	if err := os.WriteFile(matches[0], []byte(strings.Join(kept, "")), 0o600); err != nil {
		f.t.Fatal(err)
	}
}

func TestToolInterruptedWhileRunningIsNeverRerunOnItsOwn(t *testing.T) {
	f := newToolFake(t)
	c := f.open()
	id := f.askForTools(c, callA)
	f.decide(c, id, store.DecideCalls, "", "call-a")
	f.store.Close()
	f.store = nil
	f.cutRecord(store.ToolFinished)
	f.runs = nil

	c = f.open()
	d := f.decision(c, id, store.DecideInterrupted)
	if !reflect.DeepEqual(d.Calls, []string{"call-a"}) {
		t.Errorf("calls = %v", d.Calls)
	}
	if got := f.calls(c, id)[0]; got.State != "interrupted" {
		t.Errorf("state = %s", got.State)
	}
	if len(f.runs) != 0 {
		t.Fatalf("ran again on its own: %v", f.runs)
	}

	// Choosing to report it tells the model it may have partly run.
	f.decide(c, id, store.DecideInterrupted, "")
	if len(f.runs) != 0 {
		t.Errorf("runs = %v", f.runs)
	}
	m := f.messagesIn(1)
	if last := m[len(m)-1]; last.Content != DefaultInterrupted {
		t.Errorf("result = %+v", last)
	}
}

func TestInterruptedToolCanBeRunAgainByHand(t *testing.T) {
	f := newToolFake(t)
	c := f.open()
	id := f.askForTools(c, callA)
	f.decide(c, id, store.DecideCalls, "", "call-a")
	f.store.Close()
	f.store = nil
	f.cutRecord(store.ToolFinished)
	f.runs = nil

	c = f.open()
	f.decide(c, id, store.DecideInterrupted, "", "call-a")
	if !reflect.DeepEqual(f.runs, []string{"a"}) {
		t.Fatalf("runs = %v", f.runs)
	}
	f.decide(c, id, store.DecideOutput, "", "call-a")
	m := f.messagesIn(1)
	if last := m[len(m)-1]; last.Content != "echoed a" {
		t.Errorf("result = %+v", last)
	}
}

func TestApprovedButUnstartedCallsAskAgainAfterAStop(t *testing.T) {
	f := newToolFake(t)
	c := f.open()
	id := f.askForTools(c, callA)
	f.decide(c, id, store.DecideCalls, "", "call-a")
	f.store.Close()
	f.store = nil
	f.cutRecord(store.ToolStarted)
	f.runs = nil

	c = f.open()
	f.decision(c, id, store.DecideInterrupted)
	if len(f.runs) != 0 {
		t.Errorf("runs = %v", f.runs)
	}
	if got := f.calls(c, id)[0]; got.Arguments != callA.args {
		t.Errorf("call = %+v", got)
	}
}

func TestStopBeforeTheDecisionWasRecorded(t *testing.T) {
	f := newToolFake(t)
	c := f.open()
	id := f.askForTools(c, callA)
	f.store.Close()
	f.store = nil
	f.cutRecord(store.DecisionRequested)

	c = f.open()
	f.decision(c, id, store.DecideCalls)
	if len(f.bodies) != 1 || len(f.runs) != 0 {
		t.Errorf("bodies %d, runs %v", len(f.bodies), f.runs)
	}
}

func TestStopAfterOutputOpensTheOutputGate(t *testing.T) {
	f := newToolFake(t)
	c := f.open()
	id := f.askForTools(c, callA)
	f.decide(c, id, store.DecideCalls, "", "call-a")
	f.store.Close()
	f.store = nil
	matches, _ := filepath.Glob(filepath.Join(f.dataDir, "events", "*.jsonl"))
	data, _ := os.ReadFile(matches[0])
	lines := strings.SplitAfter(string(data), "\n")
	var kept []string
	for _, line := range lines {
		if strings.Contains(line, `"kind":"tool_output"`) {
			break
		}
		kept = append(kept, line)
	}
	os.WriteFile(matches[0], []byte(strings.Join(kept, "")), 0o600)
	f.runs = nil

	c = f.open()
	f.decision(c, id, store.DecideOutput)
	if len(f.runs) != 0 || len(f.bodies) != 1 {
		t.Errorf("runs %v, bodies %d", f.runs, len(f.bodies))
	}
}

func TestToolFailureIsShownAndCanBeSent(t *testing.T) {
	f := newToolFake(t)
	c := f.open()
	id := f.askForTools(c, callA)
	f.output = func(string) ([]byte, error) { return []byte("partial"), errors.New("it broke") }
	f.decide(c, id, store.DecideCalls, "", "call-a")
	if got := f.calls(c, id)[0]; got.Error != "it broke" || got.Output != "partial" {
		t.Errorf("call = %+v", got)
	}
	f.decide(c, id, store.DecideOutput, "", "call-a")
	m := f.messagesIn(1)
	if last := m[len(m)-1]; last.Content != "partial\n\nThe tool reported an error: it broke" {
		t.Errorf("result = %+v", last)
	}
}

func TestInvalidUTF8OutputIsConvertedOnlyForTheModel(t *testing.T) {
	f := newToolFake(t)
	c := f.open()
	id := f.askForTools(c, callA)
	raw := []byte("ok\xffbad")
	f.output = func(string) ([]byte, error) { return raw, nil }
	f.decide(c, id, store.DecideCalls, "", "call-a")
	f.decide(c, id, store.DecideOutput, "", "call-a")
	m := f.messagesIn(1)
	if last := m[len(m)-1]; last.Content != "ok�bad" {
		t.Errorf("result = %q", last.Content)
	}
	for _, ev := range f.events() {
		if ev.Type == store.ToolFinished {
			got, err := f.store.Get(ev.Output)
			if err != nil || string(got) != string(raw) {
				t.Errorf("recorded output = %q, %v", got, err)
			}
		}
	}
}

func TestBrokenToolCallsAreAFailedCall(t *testing.T) {
	for name, stream := range map[string]string{
		"no id":      toolStream(wireCall{"", "echo", `{}`}),
		"no name":    toolStream(wireCall{"call-a", "", `{}`}),
		"same id":    toolStream(callA, callA),
		"lost piece": strings.Replace(toolStream(callA), `data: {"id":"gen-1","choices":[{"delta":{"content":""}`, "data: {garbage\n\n"+`data: {"id":"gen-1","choices":[{"delta":{"content":""}`, 1),
		"cut short":  strings.Replace(toolStream(callA), `"finish_reason":"tool_calls"`, `"finish_reason":""`, 1),
	} {
		t.Run(name, func(t *testing.T) {
			f := newToolFake(t)
			c := f.open()
			id, _ := c.NewConversation("")
			f.replyWith(stream)
			updates, err := c.Send(id, "go")
			if err != nil {
				t.Fatal(err)
			}
			if got := finish(t, updates); got.Kind != "error" {
				t.Errorf("update = %+v", got)
			}
			conv, _ := c.Conversation(id)
			if conv.Decision != nil || !conv.CanReply {
				t.Errorf("conversation = %+v", conv)
			}
		})
	}
}

func TestConversationsWithoutToolsDontSendThem(t *testing.T) {
	f := newFake(t)
	c := f.open()
	id, _ := c.NewConversation("")
	updates, err := c.Send(id, "hi")
	mustFinish(t, updates, err)
	if strings.Contains(string(f.bodies[0]), `"tools"`) {
		t.Errorf("request = %s", f.bodies[0])
	}
}

func TestNewerToolEventsDontBreakTheDerivedContext(t *testing.T) {
	f := newToolFake(t)
	c := f.open()
	id := f.askForTools(c, callA)
	f.decide(c, id, store.DecideCalls, "")
	updates, err := c.Send(id, "next")
	mustFinish(t, updates, err)

	// The prefix of each request is the whole of the one before.
	first, second := f.messagesIn(1), f.messagesIn(2)
	if !reflect.DeepEqual(first, second[:len(first)]) {
		t.Errorf("prefix changed:\n%+v\n%+v", first, second)
	}
	c = f.open()
	again, _ := c.Conversation(id)
	if again.Decision != nil || len(again.Items) == 0 {
		t.Errorf("after restart: %+v", again)
	}
}

func TestHostsCanReuseCallIDsBetweenTurns(t *testing.T) {
	f := newToolFake(t)
	c := f.open()
	id := f.askForTools(c, callA)
	f.decide(c, id, store.DecideCalls, "First denial.")

	f.replyWith(toolStream(callA))
	updates, err := c.Send(id, "Again.")
	mustFinish(t, updates, err)
	f.replyWith(replyStream)
	f.decide(c, id, store.DecideCalls, "Second denial.")

	f.wantMessages(3, sys("Be brief."), user("Use your tools."), assistantCalls(callA), toolMsg("call-a", "First denial."),
		bot("Hello there"), user("Again."), assistantCalls(callA), toolMsg("call-a", "Second denial."))

	c = f.open()
	got := f.calls(c, id)
	if len(got) != 2 || got[0].Result != "First denial." || got[1].Result != "Second denial." {
		t.Errorf("after a restart: %+v", got)
	}
}

func TestOnlyToolsThisConversationWasOfferedCanRun(t *testing.T) {
	f := newToolFake(t)
	tools := f.tools
	f.tools = nil
	c := f.open()
	id, _ := c.NewConversation("")
	updates, err := c.Send(id, "no tools here")
	mustFinish(t, updates, err)

	// A later build offers tools, and a host returns a call anyway.
	f.tools = tools
	c = f.open()
	f.replyWith(toolStream(callA))
	updates, err = c.Send(id, "go")
	mustFinish(t, updates, err)
	got := f.calls(c, id)
	if len(got) != 1 || got[0].Invalid == "" || got[0].Decidable {
		t.Fatalf("calls = %+v", got)
	}
	d := f.decision(c, id, store.DecideCalls)
	if _, err := c.Decide(id, d.ID, []string{"call-a"}, ""); !errors.Is(err, ErrBadChoice) {
		t.Errorf("Decide = %v", err)
	}
}

func TestOnlyTheNewestCallWithAnIDIsDecidable(t *testing.T) {
	f := newToolFake(t)
	c := f.open()
	id := f.askForTools(c, callA)
	f.decide(c, id, store.DecideCalls, "")
	f.replyWith(toolStream(callA))
	updates, err := c.Send(id, "Again.")
	mustFinish(t, updates, err)
	got := f.calls(c, id)
	if got[0].Decidable || !got[1].Decidable {
		t.Errorf("calls = %+v", got)
	}
}

func TestInspectResultsAreRecordedBeforeApproval(t *testing.T) {
	f := newToolFake(t)
	f.tools[0].Inspect = func(call Call) ([]string, error) {
		var text string
		json.Unmarshal(call.Args["text"], &text)
		if text == "bad" {
			return nil, errors.New("not allowed")
		}
		return []string{"Look at " + text + " in " + call.Workdir}, nil
	}
	c := f.open()
	id, err := c.NewConversationWith(Options{System: "x", Tools: true, Workdir: "/somewhere"})
	if err != nil {
		t.Fatal(err)
	}
	f.replyWith(toolStream(callA, wireCall{"call-b", "echo", `{"text":"bad"}`}))
	updates, err := c.Send(id, "go")
	mustFinish(t, updates, err)
	got := f.calls(c, id)
	if !reflect.DeepEqual(got[0].Flags, []string{"Look at a in /somewhere"}) || !got[0].Decidable {
		t.Errorf("call = %+v", got[0])
	}
	if !strings.Contains(got[1].Invalid, "not allowed") || got[1].Decidable {
		t.Errorf("call = %+v", got[1])
	}
	c = f.open()
	if got := f.calls(c, id); len(got[0].Flags) != 1 {
		t.Errorf("flags after a restart: %+v", got[0])
	}
	if conv, _ := c.Conversation(id); conv.Workdir != "/somewhere" || !conv.Tools {
		t.Errorf("conversation = %+v", conv)
	}
}

func TestToolsRunInTheConversationsWorkdir(t *testing.T) {
	f := newToolFake(t)
	var seen string
	f.tools[0].Run = func(_ context.Context, call Call) ([]byte, error) {
		seen = call.Workdir
		return nil, nil
	}
	c := f.open()
	id, _ := c.NewConversationWith(Options{Tools: true, Workdir: "/there"})
	f.replyWith(toolStream(callA))
	updates, err := c.Send(id, "go")
	mustFinish(t, updates, err)
	f.replyWith(replyStream)
	f.decide(c, id, store.DecideCalls, "", "call-a")
	if seen != "/there" {
		t.Errorf("workdir = %q", seen)
	}
}

func TestAConversationCanStartWithoutTools(t *testing.T) {
	f := newToolFake(t)
	c := f.open()
	id, err := c.NewConversationWith(Options{Tools: false, Workdir: "/ignored"})
	if err != nil {
		t.Fatal(err)
	}
	updates, err := c.Send(id, "hi")
	mustFinish(t, updates, err)
	if strings.Contains(string(f.bodies[0]), `"tools"`) {
		t.Errorf("request = %s", f.bodies[0])
	}
	if conv, _ := c.Conversation(id); conv.Tools || conv.Workdir != "" {
		t.Errorf("conversation = %+v", conv)
	}
}

func TestSectionsAreSeparateBlocks(t *testing.T) {
	f := newFake(t)
	c := f.open()
	id, err := c.NewConversationWith(Options{System: "A\n\nB", Blocks: []string{"A", "\n\n", "B"}})
	if err != nil {
		t.Fatal(err)
	}
	other, err := c.NewConversationWith(Options{System: "edited by hand", Blocks: []string{"A"}})
	if err != nil {
		t.Fatal(err)
	}
	updates, err := c.Send(id, "hi")
	mustFinish(t, updates, err)
	f.wantMessages(0, sys("A\n\nB"), user("hi"))
	counts := map[string]int{}
	for _, ev := range f.events() {
		if ev.Type == store.SystemMessage {
			counts[ev.Conversation] = len(ev.Parts)
		}
	}
	if counts[id] != 3 || counts[other] != 1 {
		t.Errorf("parts = %v", counts)
	}
}

func TestToolNameHasToMatchItsDefinition(t *testing.T) {
	f := newFake(t)
	st := f.open()
	_ = st
	_, err := New(Config{Store: f.store, Tools: []Tool{{Name: "other", Definition: jsontext.Value(definition)}}})
	if err == nil {
		t.Error("expected an error")
	}
}

func TestAChangeSinceApprovalStopsTheRun(t *testing.T) {
	f := newToolFake(t)
	flags := []string{"Fine"}
	f.tools[0].Inspect = func(Call) ([]string, error) { return flags, nil }
	c := f.open()
	id := f.askForTools(c, callA)
	flags = []string{"Fine", "Now outside"}
	f.decide(c, id, store.DecideCalls, "", "call-a")
	if len(f.runs) != 0 {
		t.Fatalf("ran: %v", f.runs)
	}
	got := f.calls(c, id)[0]
	if !strings.Contains(got.Error, "changed since it was approved") {
		t.Errorf("call = %+v", got)
	}
}

func TestAnErrorSinceApprovalStopsTheRun(t *testing.T) {
	f := newToolFake(t)
	var problem error
	f.tools[0].Inspect = func(Call) ([]string, error) { return nil, problem }
	c := f.open()
	id := f.askForTools(c, callA)
	problem = errors.New("it became the key file")
	f.decide(c, id, store.DecideCalls, "", "call-a")
	if len(f.runs) != 0 {
		t.Fatalf("ran: %v", f.runs)
	}
	if got := f.calls(c, id)[0]; !strings.Contains(got.Error, "it became the key file") {
		t.Errorf("call = %+v", got)
	}
}

func TestArtifactsAreKeptBesideTheOutput(t *testing.T) {
	f := newToolFake(t)
	f.tools[0].Run = func(_ context.Context, call Call) ([]byte, error) {
		return []byte("done"), call.Keep("before", []byte("old contents"))
	}
	c := f.open()
	id := f.askForTools(c, callA)
	f.decide(c, id, store.DecideCalls, "", "call-a")
	for _, ev := range f.events() {
		if ev.Type == store.ToolFinished {
			if len(ev.Artifacts) != 1 || ev.Artifacts[0].Name != "before" {
				t.Fatalf("artifacts = %+v", ev.Artifacts)
			}
			if got, _ := f.store.Get(ev.Artifacts[0].Block); string(got) != "old contents" {
				t.Errorf("artifact = %q", got)
			}
		}
	}
}

func settingsTool(f *toolFake, seen *[]string) {
	f.tools[0].Define = func(s store.Settings) jsontext.Value {
		return jsontext.Value(strings.Replace(definition, "Echo text.", "Skips "+strings.Join(s.SearchSkip, ",")+".", 1))
	}
	f.tools[0].Run = func(_ context.Context, call Call) ([]byte, error) {
		*seen = call.Settings.SearchSkip
		return nil, nil
	}
}

func TestConversationsKeepTheirOwnSettings(t *testing.T) {
	f := newToolFake(t)
	var seen []string
	settingsTool(f, &seen)
	f.fake.settings = store.Settings{SearchSkip: []string{"old"}}
	c := f.open()
	id := f.askForTools(c, callA)
	if conv, _ := c.Conversation(id); conv.SettingsChanged {
		t.Error("nothing has changed yet")
	}

	// The config changes, including the model. This conversation doesn't.
	next := store.Settings{Model: "new/model", Host: "new/host", SearchSkip: []string{"new"}}
	c.Reconfigure(next)
	f.decide(c, id, store.DecideCalls, "", "call-a")
	if !reflect.DeepEqual(seen, []string{"old"}) {
		t.Errorf("tool saw %v", seen)
	}
	f.decide(c, id, store.DecideOutput, "", "call-a")
	body := string(f.bodies[1])
	if !strings.Contains(body, "Skips old.") || !strings.Contains(body, `"model":"test/model"`) || !strings.Contains(body, `"morph/fp8"`) {
		t.Errorf("request = %s", body)
	}
	conv, _ := c.Conversation(id)
	if !conv.SettingsChanged || conv.Model != "test/model" || conv.ConfigSettings.Model != "new/model" {
		t.Errorf("view = %+v", conv)
	}

	// A new conversation starts from the new settings.
	other, _ := c.NewConversation("")
	if conv, _ := c.Conversation(other); conv.SettingsChanged || conv.Model != "new/model" {
		t.Errorf("new conversation = %+v", conv)
	}

	// A change is held until it's applied, and that survives a restart.
	proposal, err := c.ProposeSettings(id, next)
	if err != nil {
		t.Fatal(err)
	}
	updates, err := c.Send(id, "still old")
	mustFinish(t, updates, err)
	if last := string(f.bodies[len(f.bodies)-1]); !strings.Contains(last, "Skips old.") || !strings.Contains(last, `"test/model"`) {
		t.Errorf("a held change was used: %s", last)
	}
	f.fake.settings = next
	f.fake.t = t
	c = f.open()
	conv, _ = c.Conversation(id)
	if conv.PendingSettings == nil || conv.PendingSettings.ID != proposal {
		t.Fatalf("pending = %+v", conv.PendingSettings)
	}
	if err := c.ApplySettings(id, proposal); err != nil {
		t.Fatal(err)
	}
	updates, err = c.Send(id, "now new")
	mustFinish(t, updates, err)
	last := string(f.bodies[len(f.bodies)-1])
	if !strings.Contains(last, "Skips new.") || !strings.Contains(last, `"new/model"`) || !strings.Contains(last, `"new/host"`) {
		t.Errorf("request = %s", last)
	}
	c = f.open()
	if conv, _ := c.Conversation(id); conv.PendingSettings != nil || conv.Model != "new/model" || conv.SettingsChanged {
		t.Errorf("after a restart: %+v", conv)
	}
}

func TestHeldSettingsCanBeDiscardedOrReplaced(t *testing.T) {
	f := newFake(t)
	c := f.open()
	id, _ := c.NewConversation("")
	first, err := c.ProposeSettings(id, store.Settings{Model: "a"})
	if err != nil {
		t.Fatal(err)
	}
	second, err := c.ProposeSettings(id, store.Settings{Model: "b"})
	if err != nil {
		t.Fatal(err)
	}
	if err := c.ApplySettings(id, first); !errors.Is(err, ErrNoEdit) {
		t.Errorf("replaced proposal = %v", err)
	}
	if err := c.DiscardSettings(id, second); err != nil {
		t.Fatal(err)
	}
	if conv, _ := c.Conversation(id); conv.PendingSettings != nil || conv.Model != "test/model" {
		t.Errorf("view = %+v", conv)
	}
	if _, err := c.ProposeSettings(id, store.Settings{}); !errors.Is(err, ErrBadSettings) {
		t.Errorf("no model = %v", err)
	}
}

func TestSettingsCantChangeInTheMiddleOfAToolTurn(t *testing.T) {
	f := newToolFake(t)
	c := f.open()
	id := f.askForTools(c, callA)
	proposal, err := c.ProposeSettings(id, store.Settings{Model: "other"})
	if err != nil {
		t.Fatal(err)
	}
	if err := c.ApplySettings(id, proposal); !errors.Is(err, ErrAwaiting) {
		t.Errorf("err = %v", err)
	}
}

func TestOlderConversationsUseTheirLastModel(t *testing.T) {
	f := newFake(t)
	c := f.openOldRecord()
	c.Reconfigure(store.Settings{Model: "new/model", Host: "new/host"})
	updates, err := c.Send(oldRecordConversation, "next")
	mustFinish(t, updates, err)
	if body := string(f.bodies[0]); !strings.Contains(body, `"model":"test/model"`) || !strings.Contains(body, `"morph/fp8"`) {
		t.Errorf("request = %s", body)
	}
}

func TestToolsNeedingAWorkdirAreFilteredOut(t *testing.T) {
	f := newToolFake(t)
	f.tools = append(f.tools, Tool{Name: "needy", NeedsWorkdir: true, Definition: jsontext.Value(strings.ReplaceAll(definition, `"echo"`, `"needy"`))})
	c := f.open()
	id, _ := c.NewConversationWith(Options{Tools: true})
	updates, err := c.Send(id, "hi")
	mustFinish(t, updates, err)
	body := string(f.bodies[0])
	if !strings.Contains(body, `"echo"`) || strings.Contains(body, `"needy"`) {
		t.Errorf("request = %s", body)
	}
	id, _ = c.NewConversationWith(Options{Tools: true, Workdir: "/w"})
	updates, err = c.Send(id, "hi")
	mustFinish(t, updates, err)
	if body := string(f.bodies[1]); !strings.Contains(body, `"needy"`) {
		t.Errorf("request = %s", body)
	}
	// If none are left, there are none at all.
	f.tools = f.tools[1:]
	c = f.open()
	id, _ = c.NewConversationWith(Options{Tools: true})
	if conv, _ := c.Conversation(id); conv.Tools {
		t.Errorf("view = %+v", conv)
	}
}

func TestTacksOwnMessagesSkipTheOutputGate(t *testing.T) {
	f := newToolFake(t)
	f.tools[0].TackWritten = true
	c := f.open()
	id := f.askForTools(c, callA)
	f.decide(c, id, store.DecideCalls, "", "call-a")
	if conv, _ := c.Conversation(id); conv.Decision != nil {
		t.Fatalf("decision = %+v", conv.Decision)
	}
	f.wantMessages(1, sys("Be brief."), user("Use your tools."), assistantCalls(callA), toolMsg("call-a", "echoed a"))
	var by string
	for _, ev := range f.events() {
		if ev.Type == store.DecisionMade {
			by = ev.By
		}
	}
	if by != "tack" {
		t.Errorf("by = %q", by)
	}
}

func TestErrorsFromTacksOwnToolsStillStopAtTheGate(t *testing.T) {
	f := newToolFake(t)
	f.tools[0].TackWritten = true
	f.output = func(string) ([]byte, error) { return nil, errors.New("old_string wasn't found") }
	c := f.open()
	id := f.askForTools(c, callA)
	f.decide(c, id, store.DecideCalls, "", "call-a")
	f.decision(c, id, store.DecideOutput)
}

func TestEachRunGetsItsOwnExecutionID(t *testing.T) {
	f := newToolFake(t)
	c := f.open()
	id := f.askForTools(c, callA)
	f.decide(c, id, store.DecideCalls, "", "call-a")
	f.store.Close()
	f.store = nil
	f.cutRecord(store.ToolFinished)
	c = f.open()
	f.decide(c, id, store.DecideInterrupted, "", "call-a")
	var started, finished []string
	for _, ev := range f.events() {
		switch ev.Type {
		case store.ToolStarted:
			started = append(started, ev.Execution)
		case store.ToolFinished:
			finished = append(finished, ev.Execution)
		}
	}
	if len(started) != 2 || started[0] == "" || started[0] == started[1] || len(finished) != 1 || finished[0] != started[1] {
		t.Errorf("started %v, finished %v", started, finished)
	}
}

func TestNetworkIsChosenPerCall(t *testing.T) {
	f := newToolFake(t)
	f.tools[0].NetworkOption = true
	var seen []bool
	f.tools[0].Run = func(_ context.Context, call Call) ([]byte, error) {
		seen = append(seen, call.Network)
		return nil, nil
	}
	c := f.open()
	id := f.askForTools(c, callA, callB)
	d := f.decision(c, id, store.DecideCalls)
	if got := f.calls(c, id); !got[0].NetworkOption {
		t.Errorf("call = %+v", got[0])
	}
	if _, err := c.DecideWithNetwork(id, d.ID, []string{"call-a"}, []string{"call-b"}, ""); !errors.Is(err, ErrBadChoice) {
		t.Errorf("network without allowing = %v", err)
	}
	updates, err := c.DecideWithNetwork(id, d.ID, []string{"call-a", "call-b"}, []string{"call-b"}, "")
	mustFinish(t, updates, err)
	if !reflect.DeepEqual(seen, []bool{false, true}) {
		t.Errorf("seen = %v", seen)
	}
	got := f.calls(c, id)
	if got[0].Network || !got[1].Network || got[0].NetworkOption {
		t.Errorf("calls = %+v", got)
	}
	found := 0
	for _, ev := range f.events() {
		if ev.Type == store.ToolStarted && ev.Network {
			found++
		}
	}
	if found != 1 {
		t.Errorf("recorded %d calls with network", found)
	}
}

func TestToolsWithoutTheOptionCantGetNetwork(t *testing.T) {
	f := newToolFake(t)
	c := f.open()
	id := f.askForTools(c, callA)
	d := f.decision(c, id, store.DecideCalls)
	if _, err := c.DecideWithNetwork(id, d.ID, []string{"call-a"}, []string{"call-a"}, ""); !errors.Is(err, ErrBadChoice) {
		t.Errorf("err = %v", err)
	}
}

func TestRecordedConfigCanBeReplayed(t *testing.T) {
	f := newFake(t)
	c := f.open()
	if err := c.RecordConfig([]byte(`{"model":"m"}`)); err != nil {
		t.Fatal(err)
	}
	f.open()
	found := false
	for _, ev := range f.events() {
		if ev.Type == store.ConfigLoaded {
			got, err := f.store.Get(ev.Config)
			found = err == nil && string(got) == `{"model":"m"}`
		}
	}
	if !found {
		t.Error("config not recorded")
	}
}

func costStream(cost string) string {
	return `data: {"id":"g","provider":"Morph","choices":[{"delta":{"content":"Hi"},"finish_reason":"stop"}]}` + "\n\n" +
		`data: {"id":"g","choices":[],"usage":{"prompt_tokens":10,"completion_tokens":1,"cost":` + cost + `}}` + "\n\n"
}

// chainStream replies with only a note that has a cost, so the model carries on.
func chainStream(n *int, cost string) string {
	*n++
	stream := toolStream(wireCall{fmt.Sprintf("call-%d", *n), "note", fmt.Sprintf(`{"kind":"plan","key":"k%d","text":"More"}`, *n)})
	usage := `data: {"id":"g","choices":[],"usage":{"prompt_tokens":10,"completion_tokens":1,"cost":` + cost + `}}` + "\n\n"
	return strings.Replace(stream, "data: [DONE]", usage+"data: [DONE]", 1)
}

func TestABudgetStopsCallsTheModelStartsByItself(t *testing.T) {
	f := newToolFake(t)
	f.noteTools()
	f.settings = store.Settings{BudgetUSD: 0.0025}
	n := 0
	f.handler = func(w http.ResponseWriter) { io.WriteString(w, chainStream(&n, "0.001")) }
	c := f.open()
	id, _ := c.NewConversation("")
	updates, err := c.Send(id, "one")
	if got := finish(t, updates); err != nil || got.Kind != "done" {
		t.Fatalf("%+v, %v", got, err)
	}
	// 0.002 spent, and another 0.001 is expected, which would go over.
	if n := len(f.bodies); n != 2 {
		t.Fatalf("%d calls", n)
	}
	conv, _ := c.Conversation(id)
	if !conv.Budget.Stopped || conv.Budget.Spent != picoFromUSD(0.002) || !conv.CanReply || conv.Budget.Estimate <= picoFromUSD(0.001) {
		t.Errorf("budget = %+v", conv.Budget)
	}

	// It survives a restart, and allowing more starts from what's been spent.
	c = f.open()
	if conv, _ := c.Conversation(id); !conv.Budget.Stopped || conv.Budget.Spent != picoFromUSD(0.002) {
		t.Errorf("after a restart: %+v", conv.Budget)
	}
	if err := c.AllowBudget(id, 0); !errors.Is(err, ErrBadSettings) {
		t.Errorf("zero = %v", err)
	}
	if err := c.AllowBudget(id, picoFromUSD(0.0001)); err != nil {
		t.Fatal(err)
	}
	conv, _ = c.Conversation(id)
	if conv.Budget.Stopped || conv.Budget.Limit != picoFromUSD(0.0021) {
		t.Errorf("allowed = %+v", conv.Budget)
	}

	// Even that little lets the next call go, since someone asked, and it's what applies.
	updates, err = c.Reply(id)
	mustFinish(t, updates, err)
	if n := len(f.bodies); n != 3 {
		t.Errorf("%d calls", n)
	}
	conv, _ = c.Conversation(id)
	if !conv.Budget.Stopped || conv.Budget.Limit != picoFromUSD(0.0021) {
		t.Errorf("budget = %+v", conv.Budget)
	}

	// A new message starts the usual allowance from there, and the model stops by itself.
	f.handler = func(w http.ResponseWriter) { io.WriteString(w, costStream("0.001")) }
	updates, err = c.Send(id, "two")
	mustFinish(t, updates, err)
	conv, _ = c.Conversation(id)
	if conv.Budget.Stopped || conv.Budget.Limit != picoFromUSD(0.0055) {
		t.Errorf("after a message: %+v", conv.Budget)
	}
}

func TestAnExpensiveCallYouAskForStillGoes(t *testing.T) {
	f := newFake(t)
	f.settings = store.Settings{BudgetUSD: 0.0001}
	f.handler = func(w http.ResponseWriter) { io.WriteString(w, costStream("0.01")) }
	c := f.open()
	id, _ := c.NewConversation("")
	for _, text := range []string{"one", "two", "three"} {
		updates, err := c.Send(id, text)
		mustFinish(t, updates, err)
	}
	if n := len(f.bodies); n != 3 {
		t.Errorf("%d calls", n)
	}
}

func TestChainsAddUpWhatRanWithoutYou(t *testing.T) {
	f := newToolFake(t)
	f.noteTools()
	f.settings = store.Settings{BudgetUSD: 1}
	replies := 0
	n := 0
	f.handler = func(w http.ResponseWriter) {
		replies++
		if replies%2 == 1 {
			io.WriteString(w, chainStream(&n, "0.25"))
			return
		}
		io.WriteString(w, costStream("0.125"))
	}
	c := f.open()
	id, _ := c.NewConversation("")
	for _, text := range []string{"one", "two"} {
		updates, err := c.Send(id, text)
		mustFinish(t, updates, err)
	}
	check := func(c *Core) {
		t.Helper()
		conv, _ := c.Conversation(id)
		var totals []Pico
		var calls []int
		for _, item := range conv.Items {
			if item.ChainPico != 0 {
				totals = append(totals, item.ChainPico)
				calls = append(calls, item.ChainCalls)
			}
		}
		if !slices.Equal(totals, []Pico{picoFromUSD(0.375), picoFromUSD(0.375)}) || !slices.Equal(calls, []int{2, 2}) {
			t.Errorf("totals = %v, calls = %v, in %+v", totals, calls, conv.Items)
		}
	}
	check(c)
	check(f.open())
}

func TestAFirstCallIsNeverEstimatedOverBudget(t *testing.T) {
	f := newFake(t)
	f.settings = store.Settings{BudgetUSD: 0.0000001}
	f.handler = func(w http.ResponseWriter) { io.WriteString(w, costStream("0.5")) }
	c := f.open()
	id, _ := c.NewConversation("")
	updates, err := c.Send(id, "one")
	mustFinish(t, updates, err)
	if n := len(f.bodies); n != 1 {
		t.Errorf("%d calls", n)
	}
}

func TestBudgetEstimatesGrowWithTheRequest(t *testing.T) {
	conv := &conversation{recentCosts: []Pico{picoFromUSD(0.001), picoFromUSD(0.002)}, lastRequestBytes: 100}
	if got := conv.estimate(100); got != picoFromUSD(0.002) {
		t.Errorf("same size = %v", got)
	}
	if got := conv.estimate(200); got != picoFromUSD(0.004) {
		t.Errorf("twice the size = %v", got)
	}
}

func TestAFailedRequestCostsNothingButACutOffOneCounts(t *testing.T) {
	conv := &conversation{recentCosts: []Pico{picoFromUSD(0.003)}}
	conv.addCost(&store.Event{Status: 502})
	if conv.spent != 0 {
		t.Errorf("refused: %v", conv.spent)
	}
	conv.addCost(&store.Event{Status: 200})
	if conv.spent != picoFromUSD(0.003) {
		t.Errorf("cut off: %v", conv.spent)
	}
}

func TestPauseFinishesWhatsApprovedButDoesntCallTheModel(t *testing.T) {
	f := newToolFake(t)
	f.tools[0].TackWritten = true
	c := f.open()
	id := f.askForTools(c, callA, callB)
	if err := c.Pause(id); err != nil {
		t.Fatal(err)
	}
	f.decide(c, id, store.DecideCalls, "", "call-a", "call-b")
	if !reflect.DeepEqual(f.runs, []string{"a", "b"}) {
		t.Errorf("runs = %v", f.runs)
	}
	if n := len(f.bodies); n != 1 {
		t.Fatalf("called the model while paused: %d", n)
	}
	conv, _ := c.Conversation(id)
	if !conv.Paused || !conv.CanReply {
		t.Errorf("view = %+v", conv)
	}
	// The pause survives a restart, and the results go out with the next message.
	c = f.open()
	if conv, _ := c.Conversation(id); !conv.Paused {
		t.Error("the pause was lost")
	}
	updates, err := c.Send(id, "I wanted to say something.")
	mustFinish(t, updates, err)
	f.wantMessages(1, sys("Be brief."), user("Use your tools."), assistantCalls(callA, callB),
		toolMsg("call-a", "echoed a"), toolMsg("call-b", "echoed b"), user("I wanted to say something."))
	if conv, _ := c.Conversation(id); conv.Paused {
		t.Error("still paused")
	}
}

func TestContinueClearsAPause(t *testing.T) {
	f := newFake(t)
	c := f.open()
	id, _ := c.NewConversation("")
	if _, err := c.AddSystemMessage(id, "Speak up."); err != nil {
		t.Fatal(err)
	}
	if err := c.Pause(id); err != nil {
		t.Fatal(err)
	}
	updates, err := c.Reply(id)
	mustFinish(t, updates, err)
	if conv, _ := c.Conversation(id); conv.Paused || len(f.bodies) != 1 {
		t.Errorf("paused %v, %d calls", conv.Paused, len(f.bodies))
	}
}

func TestCancelEndsTheCallAndKeepsWhatArrived(t *testing.T) {
	f := newFake(t)
	wrote := make(chan struct{})
	release := make(chan struct{})
	defer close(release)
	f.handler = func(w http.ResponseWriter) {
		io.WriteString(w, `data: {"id":"g","provider":"Morph","choices":[{"delta":{"content":"Partly"}}]}`+"\n\n")
		w.(http.Flusher).Flush()
		close(wrote)
		<-release
	}
	c := f.open()
	id, _ := c.NewConversation("")
	if err := c.CancelReply(id); !errors.Is(err, ErrNothingRunning) {
		t.Errorf("nothing running = %v", err)
	}
	updates, err := c.Send(id, "go")
	if err != nil {
		t.Fatal(err)
	}
	<-wrote
	// Wait until the text has actually arrived, so there's something to keep.
	for u := range updates {
		if u.Kind == "content" {
			break
		}
	}
	if err := c.CancelReply(id); err != nil {
		t.Fatal(err)
	}
	if got := finish(t, updates); got.Kind != "error" || got.Text != errReplyCancelled.Error() {
		t.Errorf("update = %+v", got)
	}
	var finished *store.Event
	for _, ev := range f.events() {
		if ev.Type == store.CallFinished {
			finished = ev
		}
	}
	if finished == nil || !finished.Cancelled {
		t.Fatalf("finished = %+v", finished)
	}
	if got, err := f.store.Get(finished.Response); err != nil || !strings.Contains(string(got), "Partly") {
		t.Errorf("response = %q, %v", got, err)
	}
	conv, _ := c.Conversation(id)
	if !conv.CanReply || conv.Running.Reply {
		t.Errorf("view = %+v", conv)
	}
}

func TestStopToolEndsOneCallAndKeepsItsOutput(t *testing.T) {
	f := newToolFake(t)
	started := make(chan struct{})
	f.tools[0].Run = func(ctx context.Context, _ Call) ([]byte, error) {
		close(started)
		<-ctx.Done()
		return []byte("partial"), context.Cause(ctx)
	}
	c := f.open()
	id := f.askForTools(c, callA)
	d := f.decision(c, id, store.DecideCalls)
	if err := c.StopTool(id, "call-a"); !errors.Is(err, ErrNothingRunning) {
		t.Errorf("nothing running = %v", err)
	}
	updates, err := c.Decide(id, d.ID, []string{"call-a"}, "")
	if err != nil {
		t.Fatal(err)
	}
	<-started
	if conv, _ := c.Conversation(id); conv.Running.Tool != "call-a" {
		t.Errorf("running = %+v", conv.Running)
	}
	if err := c.StopTool(id, "call-a"); err != nil {
		t.Fatal(err)
	}
	finish(t, updates)
	got := f.calls(c, id)[0]
	if got.Output != "partial" || !strings.Contains(got.Error, "you stopped it") {
		t.Errorf("call = %+v", got)
	}
	f.decision(c, id, store.DecideOutput)
}

func (f *toolFake) noteTools() {
	f.tools = append(f.tools, NoteTools()...)
}

// noteWithText replies with a note and some text, which ends the turn.
func noteWithText() string {
	return strings.Replace(toolStream(noteCall), `data: {"id":"gen-1","choices":[{"delta":{"content":""},"finish_reason":"tool_calls"}]}`,
		`data: {"id":"gen-1","choices":[{"delta":{"content":"Noted."},"finish_reason":"tool_calls"}]}`, 1)
}

var (
	noteCall = wireCall{"call-n", "note", `{"kind":"plan","key":"first","text":"Do it","status":"pending"}`}
	goCall   = wireCall{"call-g", "keep_going", `{}`}
)

func TestNotesDontNeedApprovalAndCarryOn(t *testing.T) {
	f := newToolFake(t)
	f.noteTools()
	c := f.open()
	id, _ := c.NewConversation("Be brief.")
	replies := 0
	f.handler = func(w http.ResponseWriter) {
		replies++
		if replies == 1 {
			io.WriteString(w, toolStream(noteCall))
			return
		}
		io.WriteString(w, replyStream)
	}
	updates, err := c.Send(id, "Use your tools.")
	mustFinish(t, updates, err)
	conv, _ := c.Conversation(id)
	if conv.Decision != nil {
		t.Fatalf("decision = %+v", conv.Decision)
	}
	if n := len(f.bodies); n != 2 {
		t.Fatalf("%d calls", n)
	}
	// The note's result went out straight away.
	m := f.messagesIn(1)
	if len(m) < 1 || m[len(m)-1].Role != "tool" || m[len(m)-1].Content != "ok" {
		t.Errorf("messages = %+v", m)
	}
	if len(conv.Notes) != 1 || conv.Notes[0].Text != "Do it" || conv.Notes[0].Status != "pending" {
		t.Errorf("notes = %+v", conv.Notes)
	}
	if conv.Items[len(conv.Items)-1].Text != "Hello there" {
		t.Errorf("items = %+v", conv.Items)
	}
	if !conv.Items[len(conv.Items)-2].ToolCalls[0].Compact {
		t.Errorf("a note should be compact: %+v", conv.Items[len(conv.Items)-2])
	}

	// Notes survive a restart, and the same key updates the item.
	replies = 0
	f.handler = func(w http.ResponseWriter) {
		replies++
		if replies == 1 {
			io.WriteString(w, toolStream(wireCall{"call-n2", "note", `{"kind":"plan","key":"first","text":"Did it","status":"done"}`}))
			return
		}
		io.WriteString(w, replyStream)
	}
	updates, err = c.Send(id, "Next.")
	mustFinish(t, updates, err)
	c = f.open()
	conv, _ = c.Conversation(id)
	if len(conv.Notes) != 1 || conv.Notes[0].Text != "Did it" || conv.Notes[0].Status != "done" {
		t.Errorf("notes = %+v", conv.Notes)
	}
}

func TestNotesWithTextCarryOnToo(t *testing.T) {
	f := newToolFake(t)
	f.noteTools()
	c := f.open()
	id, _ := c.NewConversation("Be brief.")
	replies := 0
	f.handler = func(w http.ResponseWriter) {
		replies++
		if replies == 1 {
			io.WriteString(w, noteWithText())
			return
		}
		io.WriteString(w, replyStream)
	}
	updates, err := c.Send(id, "Use your tools.")
	mustFinish(t, updates, err)
	if n := len(f.bodies); n != 2 {
		t.Fatalf("%d calls", n)
	}
}

func TestPauseStopsNotesFromCarryingOn(t *testing.T) {
	f := newToolFake(t)
	f.noteTools()
	c := f.open()
	id, _ := c.NewConversation("Be brief.")
	started := make(chan struct{})
	release := make(chan struct{})
	f.handler = func(w http.ResponseWriter) {
		close(started)
		<-release
		io.WriteString(w, toolStream(noteCall))
	}
	updates, err := c.Send(id, "Use your tools.")
	if err != nil {
		t.Fatal(err)
	}
	<-started
	if err := c.Pause(id); err != nil {
		t.Fatal(err)
	}
	close(release)
	mustFinish(t, updates, nil)
	if n := len(f.bodies); n != 1 {
		t.Fatalf("called the model while paused: %d", n)
	}
	if conv, _ := c.Conversation(id); !conv.Paused || !conv.CanReply {
		t.Errorf("view = %+v", conv)
	}
}

func TestRevisionsMoveWithTheRecord(t *testing.T) {
	f := newFake(t)
	c := f.open()
	id, _ := c.NewConversation("")
	revision := func() int {
		t.Helper()
		conv, err := c.Conversation(id)
		if err != nil {
			t.Fatal(err)
		}
		return conv.Revision
	}
	start := revision()
	if err := c.CheckRevision(id, start); err != nil {
		t.Errorf("current: %v", err)
	}
	if err := c.HoldMessage(id, "hi"); err != nil {
		t.Fatal(err)
	}
	if revision() == start {
		t.Error("holding didn't move it")
	}
	if err := c.CheckRevision(id, start); !errors.Is(err, ErrStale) {
		t.Errorf("stale: %v", err)
	}
	if err := c.CheckRevision("nope", 0); !errors.Is(err, ErrNotFound) {
		t.Errorf("missing: %v", err)
	}
	// A rebuild gets the same revision.
	before := revision()
	c = f.open()
	if revision() != before {
		t.Errorf("after a restart %d, was %d", revision(), before)
	}
}

func TestBudgetCanChangeWhileADecisionWaits(t *testing.T) {
	f := newToolFake(t)
	c := f.open()
	id := f.askForTools(c, callA)
	conv, _ := c.Conversation(id)
	settings := conv.Settings
	settings.BudgetUSD = 0.5
	proposal, err := c.ProposeSettings(id, settings)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.ApplySettings(id, proposal); err != nil {
		t.Errorf("budget: %v", err)
	}
	settings.Model = "something/else"
	proposal, err = c.ProposeSettings(id, settings)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.ApplySettings(id, proposal); !errors.Is(err, ErrAwaiting) {
		t.Errorf("model: %v", err)
	}
}

func TestMoneyAddsUpExactly(t *testing.T) {
	conv := &conversation{}
	for range 10 {
		conv.addCost(&store.Event{Usage: []byte(`{"cost":0.1}`)})
	}
	if conv.spent != picoFromUSD(1) {
		t.Errorf("spent = %d", conv.spent)
	}
	for _, usd := range []float64{0.000292608, 0.000000006, 0.0021, 1.01} {
		if got := picoFromUSD(picoFromUSD(usd).USD()); got != picoFromUSD(usd) {
			t.Errorf("%v came back as %d", usd, got)
		}
	}
}

func TestToolDefinitionsCanBeEditedAndLeftOut(t *testing.T) {
	f := newToolFake(t)
	f.noteTools()
	c := f.open()
	edited := jsontext.Value(`{"type":"function","function":{"name":"echo","description":"Changed.","parameters":{"type":"object","properties":{"text":{"type":"string"}}}}}`)
	id, err := c.NewConversationWith(Options{Tools: true, Definitions: map[string]jsontext.Value{"echo": edited}, LeaveOut: []string{"note"}})
	if err != nil {
		t.Fatal(err)
	}
	data, err := c.Definitions(id)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), string(edited)) || strings.Contains(string(data), `"name":"note"`) {
		t.Errorf("definitions = %s", data)
	}

	// Editing the settings later keeps the edit.
	proposal, err := c.ProposeSettings(id, store.Settings{Model: "other/model"})
	if err != nil {
		t.Fatal(err)
	}
	if err := c.ApplySettings(id, proposal); err != nil {
		t.Fatal(err)
	}
	if data, _ := c.Definitions(id); !strings.Contains(string(data), `"Changed."`) {
		t.Errorf("after settings = %s", data)
	}

	for name, opts := range map[string]Options{
		"schema": {Tools: true, Definitions: map[string]jsontext.Value{"echo": jsontext.Value(`{"type":"function","function":{"name":"echo","parameters":{"type":"object","properties":{"x":{"type":"string"}}}}}`)}},
		"rename": {Tools: true, Definitions: map[string]jsontext.Value{"echo": jsontext.Value(`{"type":"function","function":{"name":"other","parameters":{"type":"object","properties":{"text":{"type":"string"}}}}}`)}},
		"tool":   {Tools: true, Definitions: map[string]jsontext.Value{"nope": edited}},
		"out":    {Tools: true, LeaveOut: []string{"nope"}},
	} {
		if _, err := c.NewConversationWith(opts); !errors.Is(err, ErrBadTools) {
			t.Errorf("%s: err = %v", name, err)
		}
	}
}

func TestForkKeepsTheStartAndGoesItsOwnWay(t *testing.T) {
	f := newToolFake(t)
	f.noteTools()
	c := f.open()
	id, err := c.NewConversation("Be brief.")
	if err != nil {
		t.Fatal(err)
	}
	replies := 0
	f.handler = func(w http.ResponseWriter) {
		replies++
		if replies == 1 {
			io.WriteString(w, toolStream(noteCall))
			return
		}
		io.WriteString(w, replyStream)
	}
	updates, err := c.Send(id, "First")
	mustFinish(t, updates, err)
	updates, err = c.Send(id, "Second")
	mustFinish(t, updates, err)
	second := f.bodies[2]

	// Keep the system message, the first message, the note and the reply after it.
	fork, err := c.Fork(id, 4)
	if err != nil {
		t.Fatal(err)
	}
	updates, err = c.Send(fork, "Second")
	mustFinish(t, updates, err)
	if !bytes.Equal(f.bodies[3], second) {
		t.Errorf("the same point should send the same bytes:\n got %s\nwant %s", f.bodies[3], second)
	}

	// An edited first message: nothing after the system message comes along.
	early, err := c.Fork(id, 1)
	if err != nil {
		t.Fatal(err)
	}
	updates, err = c.Send(early, "Different")
	mustFinish(t, updates, err)
	body := string(f.bodies[4])
	if !strings.Contains(body, "Be brief.") || !strings.Contains(body, "Different") || strings.Contains(body, "First") {
		t.Errorf("request = %s", body)
	}

	whole, err := c.Fork(id, 6)
	if err != nil {
		t.Fatal(err)
	}

	check := func(c *Core) {
		t.Helper()
		original, err := c.Conversation(id)
		if err != nil {
			t.Fatal(err)
		}
		kept, err := c.Conversation(fork)
		if err != nil {
			t.Fatal(err)
		}
		other, err := c.Conversation(early)
		if err != nil {
			t.Fatal(err)
		}
		if n := len(original.Items); n != 6 {
			t.Errorf("original has %d items", n)
		}
		if kept.ForkedFrom != id || kept.ForkItems != 4 || len(kept.Items) != 6 {
			t.Errorf("fork = %+v", kept)
		}
		if kept.Items[5].Index != 5 {
			t.Errorf("index = %d", kept.Items[5].Index)
		}
		if len(kept.Notes) != 1 || len(other.Notes) != 0 {
			t.Errorf("notes = %+v and %+v", kept.Notes, other.Notes)
		}
		// Its budget only counts what it spent itself, but it knows what calls cost.
		copied, err := c.Conversation(whole)
		if err != nil {
			t.Fatal(err)
		}
		if copied.Budget.Spent != 0 || copied.Budget.Estimate != original.Budget.Estimate || original.Budget.Estimate == 0 {
			t.Errorf("budget = %+v, original %+v", copied.Budget, original.Budget)
		}
	}
	check(c)
	again, err := New(Config{Store: f.store, Tools: f.tools})
	if err != nil {
		t.Fatal(err)
	}
	check(again)
}

func TestForkRefusesWhatItCantKeep(t *testing.T) {
	f := newToolFake(t)
	c := f.open()
	id := f.askForTools(c, callA)
	conv, err := c.Conversation(id)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Fork(id, len(conv.Items)); !errors.Is(err, ErrAwaiting) {
		t.Errorf("waiting call: %v", err)
	}
	if _, err := c.Fork(id, len(conv.Items)-1); err != nil {
		t.Errorf("before the waiting call: %v", err)
	}
	if _, err := c.Fork(id, len(conv.Items)+1); !errors.Is(err, ErrBadFork) {
		t.Errorf("too far: %v", err)
	}
	if _, err := c.Fork("nope", 0); !errors.Is(err, ErrNotFound) {
		t.Errorf("missing: %v", err)
	}
}

func TestRealToolsStillContinueOnTheirOwn(t *testing.T) {
	f := newToolFake(t)
	f.noteTools()
	f.tools[0].TackWritten = true
	c := f.open()
	id := f.askForTools(c, callA, noteCall)
	f.decide(c, id, store.DecideCalls, "", "call-a")
	if n := len(f.bodies); n != 2 {
		t.Errorf("%d calls", n)
	}
}

func TestBadNotesStillGoThroughAPerson(t *testing.T) {
	f := newToolFake(t)
	f.noteTools()
	c := f.open()
	id := f.askForTools(c, wireCall{"call-n", "note", `{"kind":"plan"}`})
	f.decision(c, id, store.DecideCalls)
	if got := f.calls(c, id)[0]; got.Invalid == "" || got.Decidable {
		t.Errorf("call = %+v", got)
	}
}

func TestNotesWithNoWorkdirAreStillOffered(t *testing.T) {
	f := newFake(t)
	f.tools = append(f.tools, Tool{Name: "needy", NeedsWorkdir: true, Definition: jsontext.Value(strings.ReplaceAll(definition, `"echo"`, `"needy"`))})
	f.tools = append(f.tools, NoteTools()...)
	c := f.open()
	id, _ := c.NewConversationWith(Options{Tools: true})
	updates, err := c.Send(id, "hi")
	mustFinish(t, updates, err)
	body := string(f.bodies[0])
	if !strings.Contains(body, `"note"`) || strings.Contains(body, `"needy"`) {
		t.Errorf("request = %s", body)
	}
}

func TestHeldMessagesGoWithTheNextCallInOrder(t *testing.T) {
	f := newFake(t)
	c := f.open()
	id, _ := c.NewConversation("Be brief.")
	if err := c.HoldMessage("nope", "x"); !errors.Is(err, ErrNotFound) {
		t.Errorf("err = %v", err)
	}
	if err := c.HoldMessage(id, "Held."); err != nil {
		t.Fatal(err)
	}
	if _, err := c.AddSystemMessage(id, "A rule after it."); err != nil {
		t.Fatal(err)
	}
	if n := len(f.bodies); n != 0 {
		t.Fatalf("called the model: %d", n)
	}
	conv, _ := c.Conversation(id)
	if !conv.CanReply || !conv.Items[1].Queued || conv.Items[1].Role != "user" {
		t.Errorf("view = %+v", conv.Items)
	}
	// It survives a restart, and a reply sends it where it is.
	c = f.open()
	updates, err := c.Reply(id)
	mustFinish(t, updates, err)
	f.wantMessages(0, sys("Be brief."), user("Held."), sys("A rule after it."))
	conv, _ = c.Conversation(id)
	if conv.Items[1].Queued {
		t.Error("still shown as waiting")
	}
}

func TestSendingAfterAHeldMessageKeepsTheOrder(t *testing.T) {
	f := newFake(t)
	c := f.open()
	id, _ := c.NewConversation("")
	if err := c.HoldMessage(id, "First."); err != nil {
		t.Fatal(err)
	}
	if _, err := c.AddSystemMessage(id, "Middle."); err != nil {
		t.Fatal(err)
	}
	updates, err := c.Send(id, "Last.")
	mustFinish(t, updates, err)
	f.wantMessages(0, user("First."), sys("Middle."), user("Last."))
}

func TestAMessageCanBeHeldWhileADecisionWaits(t *testing.T) {
	f := newToolFake(t)
	c := f.open()
	id := f.askForTools(c, callA)
	if err := c.HoldMessage(id, "Before you go on."); err != nil {
		t.Fatal(err)
	}
	f.decide(c, id, store.DecideCalls, "")
	f.wantMessages(1, sys("Be brief."), user("Use your tools."), assistantCalls(callA), toolMsg("call-a", DefaultCallDenial), user("Before you go on."))
}

func TestSpendingIsCountedByBillingPeriod(t *testing.T) {
	f := newFake(t)
	f.settings = store.Settings{BudgetUSD: 1}
	f.handler = func(w http.ResponseWriter) { io.WriteString(w, costStream("0.25")) }
	c := f.open()
	one, _ := c.NewConversation("")
	two, _ := c.NewConversation("")
	for _, id := range []string{one, one, two} {
		updates, err := c.Send(id, "hi")
		mustFinish(t, updates, err)
	}
	conv, _ := c.Conversation(one)
	if conv.Budget.PeriodHere != picoFromUSD(0.5) || conv.Budget.PeriodAll != picoFromUSD(0.75) {
		t.Errorf("budget = %+v", conv.Budget)
	}

	// A fork counts what it spends itself, and its original keeps what it spent.
	fork, err := c.Fork(one, 2)
	if err != nil {
		t.Fatal(err)
	}
	conv, _ = c.Conversation(fork)
	if conv.Budget.PeriodHere != 0 || conv.Budget.PeriodAll != picoFromUSD(0.75) {
		t.Errorf("fork budget = %+v", conv.Budget)
	}

	// Last month's doesn't count.
	c.mu.Lock()
	last := billingPeriod(time.Now().AddDate(0, -1, -1))
	c.convs[one].spentByPeriod = map[string]Pico{last: picoFromUSD(0.5)}
	c.mu.Unlock()
	conv, _ = c.Conversation(one)
	if conv.Budget.PeriodHere != 0 || conv.Budget.PeriodAll != picoFromUSD(0.25) || conv.Budget.Spent != picoFromUSD(0.5) {
		t.Errorf("next month = %+v", conv.Budget)
	}

	// All of it survives a restart.
	c = f.open()
	conv, _ = c.Conversation(two)
	if conv.Budget.PeriodHere != picoFromUSD(0.25) || conv.Budget.PeriodAll != picoFromUSD(0.75) {
		t.Errorf("after a restart = %+v", conv.Budget)
	}
}

func TestARestartWaitsForTurnsAndStartsNothingNew(t *testing.T) {
	f := newFake(t)
	f.settings = store.Settings{BudgetUSD: 1}
	gate := make(chan struct{})
	f.handler = func(w http.ResponseWriter) {
		<-gate
		io.WriteString(w, costStream("0.001"))
	}
	c := f.open()
	if err := c.Started("first"); err != nil {
		t.Fatal(err)
	}
	busy, _ := c.NewConversation("")
	other, _ := c.NewConversation("")
	updates, err := c.Send(busy, "hi")
	if err != nil {
		t.Fatal(err)
	}

	if err := c.RequestRestart([]string{"a change"}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-c.Drained():
		t.Fatal("drained while a turn was running")
	default:
	}
	status := c.CurrentStatus()
	if !status.Restarting || len(status.Waiting) != 1 || status.Waiting[0].ID != busy || status.Waiting[0].Preview != "hi" {
		t.Errorf("status = %+v", status)
	}
	if _, err := c.Send(other, "no"); !errors.Is(err, ErrRestarting) {
		t.Errorf("send = %v", err)
	}
	if _, err := c.Reply(other); !errors.Is(err, ErrRestarting) {
		t.Errorf("reply = %v", err)
	}
	// Nothing was recorded for the refused send, and holding still works.
	if conv, _ := c.Conversation(other); len(conv.Items) != 0 {
		t.Errorf("items = %+v", conv.Items)
	}
	if err := c.HoldMessage(other, "for later"); err != nil {
		t.Error(err)
	}

	close(gate)
	mustFinish(t, updates, nil)
	select {
	case <-c.Drained():
	case <-time.After(5 * time.Second):
		t.Fatal("never drained")
	}

	// The next run knows what it replaced, and that clears once it's been seen.
	c = f.open()
	if err := c.Started("second"); err != nil {
		t.Fatal(err)
	}
	status = c.CurrentStatus()
	if status.Restarting || status.Version != "second" || status.Previous != "first" || !slices.Equal(status.Changes, []string{"a change"}) {
		t.Errorf("after = %+v", status)
	}
	c = f.open()
	if err := c.Started("third"); err != nil {
		t.Fatal(err)
	}
	if status := c.CurrentStatus(); status.Previous != "second" || len(status.Changes) != 0 {
		t.Errorf("a plain restart = %+v", status)
	}
	if conv, _ := c.Conversation(other); len(conv.Items) != 1 || conv.Items[0].Text != "for later" {
		t.Errorf("held message = %+v", conv.Items)
	}
}

func TestARestartWithNothingRunningIsDrainedRightAway(t *testing.T) {
	c := newFake(t).open()
	id, _ := c.NewConversation("")
	if err := c.RequestRestart(nil); err != nil {
		t.Fatal(err)
	}
	select {
	case <-c.Drained():
	default:
		t.Error("not drained")
	}
	if status := c.CurrentStatus(); !status.Restarting || len(status.Waiting) != 0 {
		t.Errorf("status = %+v", status)
	}
	if _, err := c.Send(id, "no"); !errors.Is(err, ErrRestarting) {
		t.Errorf("send = %v", err)
	}
}

func scratchReply(n int, op, key, text string) string {
	args, _ := json.Marshal(map[string]string{"op": op, "key": key, "text": text})
	if op == "read" || op == "list" || op == "delete" {
		args, _ = json.Marshal(map[string]string{"op": op, "key": key})
	}
	return toolStream(wireCall{fmt.Sprintf("call-s%d", n), "scratch", string(args)})
}

func TestScratchIsTheModelsAndForksWithIt(t *testing.T) {
	f := newToolFake(t)
	f.noteTools()
	c := f.open()
	id, _ := c.NewConversation("Be brief.")
	steps := []string{
		scratchReply(1, "write", "a", "one"),
		scratchReply(2, "append", "a", " two"),
		scratchReply(3, "read", "a", ""),
		scratchReply(4, "read", "nope", ""),
		scratchReply(5, "write", "b", "other"),
		scratchReply(6, "list", "", ""),
		scratchReply(7, "delete", "b", ""),
		replyStream,
	}
	next := 0
	f.handler = func(w http.ResponseWriter) {
		io.WriteString(w, steps[next])
		next++
	}
	updates, err := c.Send(id, "Use scratch.")
	mustFinish(t, updates, err)
	conv, _ := c.Conversation(id)
	if conv.Decision != nil {
		t.Fatalf("it waited: %+v", conv.Decision)
	}
	last := func(call int) string {
		m := f.messagesIn(call)
		return fmt.Sprint(m[len(m)-1].Content)
	}
	if got := last(3); got != "one two" {
		t.Errorf("read = %q", got)
	}
	if got := last(4); !strings.Contains(got, "nothing") {
		t.Errorf("read of nothing = %q", got)
	}
	if got := last(6); got != "a: 7 bytes\nb: 5 bytes\n" {
		t.Errorf("list = %q", got)
	}
	if len(conv.Scratch) != 1 || conv.Scratch[0].Key != "a" || conv.Scratch[0].Text != "one two" {
		t.Errorf("scratch = %+v", conv.Scratch)
	}
	// A fork from before the delete has what was there then. Restarting finds the same.
	var at int
	for i, item := range conv.Items {
		if len(item.ToolCalls) > 0 && strings.Contains(item.ToolCalls[0].Arguments, `"list"`) {
			at = i + 1
		}
	}
	forked, err := c.Fork(id, at)
	if err != nil {
		t.Fatal(err)
	}
	view, _ := c.Conversation(forked)
	if len(view.Scratch) != 2 {
		t.Errorf("forked scratch = %+v", view.Scratch)
	}
	c = f.open()
	view, _ = c.Conversation(id)
	if len(view.Scratch) != 1 || view.Scratch[0].Text != "one two" {
		t.Errorf("after restart = %+v", view.Scratch)
	}
}

// keptTool is a tool like run_code, but in memory: it adds a letter to what's kept.
func keptTool(saw *[]string, output string, failure error) Tool {
	return Tool{
		Name:          "kept",
		Definition:    jsontext.Value(`{"type":"function","function":{"name":"kept","parameters":{"type":"object","properties":{}}}}`),
		AutoSendBytes: 100,
		Run: func(_ context.Context, call Call) ([]byte, error) {
			*saw = append(*saw, string(call.Workspace)+"|"+string(call.Files["notes.json"])+"|"+string(call.Files["scratch.json"]))
			if err := call.SaveWorkspace(append(call.Workspace, 'x')); err != nil {
				return nil, err
			}
			return []byte(output), failure
		},
	}
}

func TestSmallOutputGoesStraightOnAndLargeWaits(t *testing.T) {
	f := newToolFake(t)
	var saw []string
	output := "short"
	f.tools = []Tool{keptTool(&saw, output, errors.New("exit code 3"))}
	c := f.open()
	id := f.askForTools(c, wireCall{"call-k", "kept", `{}`})
	f.replyWith(replyStream)
	f.decide(c, id, store.DecideCalls, "", "call-k")
	conv, _ := c.Conversation(id)
	if conv.Decision != nil {
		t.Fatalf("small output waited: %+v", conv.Decision)
	}
	m := f.messagesIn(1)
	if got := fmt.Sprint(m[len(m)-1].Content); !strings.Contains(got, "short") || !strings.Contains(got, "exit code 3") {
		t.Errorf("sent = %q", got)
	}

	// Large output waits, and can be sent with its middle taken out.
	big := strings.Repeat("a", 3000) + strings.Repeat("b", 3000) + strings.Repeat("c", 3000)
	f.tools = []Tool{keptTool(&saw, big, nil)}
	c = f.open()
	f.replyWith(toolStream(wireCall{"call-l", "kept", `{}`}))
	updates, err := c.Send(id, "Again.")
	mustFinish(t, updates, err)
	f.decide(c, id, store.DecideCalls, "", "call-l")
	decision := f.decision(c, id, store.DecideOutput)
	f.replyWith(replyStream)
	updates, err = c.DecideWith(id, decision.ID, Decision{Allow: []string{"call-l"}, Shorten: []string{"call-l"}})
	mustFinish(t, updates, err)
	sent := f.messagesIn(len(f.bodies) - 1)
	got := fmt.Sprint(sent[len(sent)-1].Content)
	if !strings.HasPrefix(got, strings.Repeat("a", 2048)) || !strings.HasSuffix(got, strings.Repeat("c", 2048)) || !strings.Contains(got, "left out 4904 bytes") || strings.Contains(got, "b") && len(got) > 4300 {
		t.Errorf("shortened = %d bytes: %.60q", len(got), got)
	}
	// What's kept carried over from the first call.
	if len(saw) != 2 || !strings.HasPrefix(saw[1], "x|") {
		t.Errorf("saw = %q", saw)
	}
}

func TestKeptFilesFollowTheConversationAndItsForks(t *testing.T) {
	f := newToolFake(t)
	var saw []string
	f.tools = append([]Tool{keptTool(&saw, "ok", nil)}, NoteTools()...)
	c := f.open()
	steps := []string{toolStream(wireCall{"c1", "kept", `{}`}), toolStream(wireCall{"c2", "kept", `{}`}), replyStream, toolStream(wireCall{"c3", "kept", `{}`}), replyStream}
	next := 0
	f.handler = func(w http.ResponseWriter) {
		io.WriteString(w, steps[next])
		next++
	}
	id, _ := c.NewConversation("Be brief.")
	updates, err := c.Send(id, "Go.")
	mustFinish(t, updates, err)
	f.decide(c, id, store.DecideCalls, "", "c1")
	f.decide(c, id, store.DecideCalls, "", "c2")
	conv, _ := c.Conversation(id)
	if len(saw) != 2 || saw[0] != "|[]|{}" || !strings.HasPrefix(saw[1], "x|") {
		t.Fatalf("saw = %q", saw)
	}
	// A fork from before the second call goes on from the first call's files.
	var at int
	for i, item := range conv.Items {
		if len(item.ToolCalls) > 0 && item.ToolCalls[0].ID == "c1" {
			at = i + 1
		}
	}
	forked, err := c.Fork(id, at)
	if err != nil {
		t.Fatal(err)
	}
	updates, err = c.Send(forked, "More.")
	mustFinish(t, updates, err)
	f.decide(c, forked, store.DecideCalls, "", "c3")
	if len(saw) != 3 || !strings.HasPrefix(saw[2], "x|") {
		t.Errorf("forked saw = %q", saw)
	}
}

func TestTokensAreCountedForAModelTackKnows(t *testing.T) {
	f := newToolFake(t)
	f.output = func(string) ([]byte, error) { return []byte("Some output with a few words."), nil }
	c := f.open()
	id := f.askForTools(c, callA)
	settings, _ := c.Conversation(id)
	if settings.Budget.NextTokens != 0 {
		t.Errorf("counted for a model it doesn't know: %d", settings.Budget.NextTokens)
	}
	if countRequest("deepseek/deepseek-v4.1-flash-1017", []byte(`{"messages":[{"role":"user","content":"Hi"}]}`)) == 0 {
		t.Error("it can't count for the model it should")
	}
	if countOutput("deepseek/deepseek-v4.1-flash-1017", "Hi there") != 2 {
		t.Errorf("output = %d", countOutput("deepseek/deepseek-v4.1-flash-1017", "Hi there"))
	}
	if countOutput("some/other-model", "Hi there") != 0 {
		t.Error("it counted for an unknown model")
	}
}
