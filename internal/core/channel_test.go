package core

import (
	"bufio"
	"context"
	"encoding/json/jsontext"
	"errors"
	"net"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/betawaffle/tack/internal/store"
)

// codeTool stands in for run_code: its "code" writes each line of its text argument to
// tack and collects the replies, which become its output.
func codeTool(f *toolFake, timeout time.Duration) {
	f.tools = append(f.tools, Tool{
		Name:       "code",
		Definition: jsontext.Value(strings.ReplaceAll(definition, `"echo"`, `"code"`)),
		Timeout:    func(store.Settings) time.Duration { return timeout },
		Run: func(ctx context.Context, call Call) ([]byte, error) {
			var text string
			if err := jsonUnquote(call.Args["text"], &text); err != nil {
				return nil, err
			}
			code, tack := net.Pipe()
			served := make(chan error, 1)
			go func() {
				served <- call.Serve(ctx, tack)
				tack.Close()
			}()
			lines := slices.Collect(strings.Lines(text))
			go func() {
				for _, line := range lines {
					if _, err := code.Write([]byte(line)); err != nil {
						return
					}
				}
			}()
			replies := bufio.NewReader(code)
			var out strings.Builder
			for range lines {
				reply, err := replies.ReadString('\n')
				out.WriteString(reply)
				if err != nil {
					break
				}
			}
			code.Close()
			return []byte(out.String()), <-served
		},
	})
	f.tools = append(f.tools, NoteTools()...)
}

func jsonUnquote(value jsontext.Value, text *string) error {
	v, err := jsontext.AppendUnquote(nil, value)
	*text = string(v)
	return err
}

// startCode starts a conversation offering code, echo, note, scratch and a custom lookup,
// in which the model asks to run code with these lines.
func startCode(t *testing.T, f *toolFake, c *Core, lines ...string) string {
	t.Helper()
	id, err := c.NewConversationWith(Options{System: "x", Offer: []Offer{
		{Name: "code"}, {Name: "echo"}, {Name: "note"}, {Name: "scratch"},
		{Name: "lookup", Custom: true, Definition: jsontext.Value(lookup)},
	}})
	if err != nil {
		t.Fatal(err)
	}
	args, _ := jsontext.AppendQuote(nil, strings.Join(lines, "\n")+"\n")
	f.replyWith(toolStream(wireCall{"call-c", "code", `{"text":` + string(args) + `}`}))
	updates, err := c.Send(id, "go")
	mustFinish(t, updates, err)
	f.replyWith(replyStream)
	return id
}

func TestCodeCanCallToolsThatNeedNobody(t *testing.T) {
	f := newToolFake(t)
	codeTool(f, 5*time.Second)
	c := f.open()
	id := startCode(t, f, c,
		`{"tool":"scratch","arguments":{"op":"write","key":"k","text":"v"}}`,
		`{"tool":"scratch","arguments":{"op":"read","key":"k"}}`,
		`{"tool":"note","arguments":{"kind":"plan","key":"a","text":"from code"}}`,
		`{"tool":"echo","arguments":{"text":"a"}}`,
		`{"tool":"code","arguments":{}}`,
		`{"tool":"nope"}`,
		`{"tool":"scratch","arguments":{"op":"read"},"extra":1}`,
		`not json`,
	)
	f.decide(c, id, store.DecideCalls, "", "call-c")
	call := f.calls(c, id)[0]
	want := []string{`{"result":"ok"}`, `{"result":"v"}`, `{"result":"ok"}`, "can't be called from code", "can't be called from code", "no tool named", "a request is one line", "a request is one line"}
	lines := strings.Split(strings.TrimSpace(call.Output), "\n")
	if len(lines) != len(want) {
		t.Fatalf("output = %s", call.Output)
	}
	for i, w := range want {
		if !strings.Contains(lines[i], w) {
			t.Errorf("reply %d = %s, want %s", i, lines[i], w)
		}
	}
	if len(call.Channel) != 8 || call.Channel[0].Tool != "scratch" || call.Channel[0].By != "tack" || call.Channel[7].Request != "not json" {
		t.Errorf("channel = %+v", call.Channel)
	}
	conv, _ := c.Conversation(id)
	if len(conv.Scratch) != 1 || len(conv.Notes) != 1 || conv.Notes[0].Text != "from code" {
		t.Errorf("scratch = %+v, notes = %+v", conv.Scratch, conv.Notes)
	}
	// It's all in the record.
	c = f.open()
	if got := f.calls(c, id)[0]; len(got.Channel) != 8 || got.Channel[1].Response != `{"result":"v"}` {
		t.Errorf("after a restart = %+v", got.Channel)
	}
}

// answerWhenAsked waits until the code's call to a custom tool can be answered, then answers it.
func answerWhenAsked(t *testing.T, c *Core, id, text string, deny bool, after time.Duration) {
	t.Helper()
	for range 500 {
		conv, _ := c.Conversation(id)
		for _, it := range conv.Items {
			for _, call := range it.ToolCalls {
				for _, ex := range call.Channel {
					if ex.Answerable {
						time.Sleep(after)
						if err := c.AnswerCodeCall(id, ex.ID, text, deny); err != nil {
							t.Error(err)
						}
						return
					}
				}
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Error("nothing to answer")
}

func TestCodeCanCallACustomToolThatAPersonAnswers(t *testing.T) {
	f := newToolFake(t)
	// The person takes longer than the limit, which doesn't count while they're asked.
	codeTool(f, 300*time.Millisecond)
	c := f.open()
	id := startCode(t, f, c, `{"tool":"lookup","arguments":{"q":"tea"}}`, `{"tool":"lookup","arguments":{"q":"coffee"}}`)
	d := f.decision(c, id, store.DecideCalls)
	if err := c.AnswerCodeCall(id, "nope", "x", false); !errors.Is(err, ErrNoDecision) {
		t.Errorf("err = %v", err)
	}
	updates, err := c.DecideWith(id, d.ID, Decision{Allow: []string{"call-c"}})
	if err != nil {
		t.Fatal(err)
	}
	var asked []Update
	done := make(chan struct{})
	go func() {
		for u := range updates {
			if u.Kind == "code_call" {
				asked = append(asked, u)
			}
		}
		close(done)
	}()
	answerWhenAsked(t, c, id, "Tea is a drink.", false, 500*time.Millisecond)
	answerWhenAsked(t, c, id, "", true, 0)
	<-done
	call := f.calls(c, id)[0]
	if call.Error != "" || call.Output != `{"result":"Tea is a drink."}`+"\n"+`{"error":"`+DefaultCodeCallDenial+`"}`+"\n" {
		t.Errorf("output = %q, error = %q", call.Output, call.Error)
	}
	if len(asked) != 2 || asked[0].ID != "call-c" || !strings.Contains(asked[0].Text, `"tea"`) {
		t.Errorf("updates = %+v", asked)
	}
	if call.Channel[0].By != "user" || call.Channel[0].Answerable || call.Channel[0].Waiting {
		t.Errorf("channel = %+v", call.Channel)
	}
}

func TestTheChannelHasLimits(t *testing.T) {
	f := newToolFake(t)
	codeTool(f, 5*time.Second)
	c := f.open()
	id := startCode(t, f, c, `{"tool":"scratch","arguments":{"op":"list","pad":"`+strings.Repeat("x", ChannelLineBytes)+`"}}`, `{"tool":"scratch","arguments":{"op":"list"}}`)
	f.decide(c, id, store.DecideCalls, "", "call-c")
	call := f.calls(c, id)[0]
	if !strings.Contains(call.Output, "at most 65536 bytes") || len(call.Channel) != 0 {
		t.Errorf("output = %q, channel = %+v", call.Output, call.Channel)
	}
}

func TestATimeLimitCanBeHeld(t *testing.T) {
	ctx, limit := pausableTimeout(context.Background(), 100*time.Millisecond, errors.New("late"))
	defer limit.stop()
	release := limit.hold()
	time.Sleep(200 * time.Millisecond)
	if ctx.Err() != nil {
		t.Fatal("it ran out while held")
	}
	release()
	release() // twice is once
	select {
	case <-ctx.Done():
		if context.Cause(ctx).Error() != "late" {
			t.Errorf("cause = %v", context.Cause(ctx))
		}
	case <-time.After(time.Second):
		t.Error("it never ran out")
	}
}
