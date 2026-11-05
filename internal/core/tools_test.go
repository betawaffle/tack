package core

import (
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
	"strings"
	"testing"

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
		Run: func(_ context.Context, args map[string]jsontext.Value) ([]byte, error) {
			var text string
			json.Unmarshal(args["text"], &text)
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
