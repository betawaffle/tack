package core

import (
	"bufio"
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"slices"
	"sync"
	"time"

	"github.com/betawaffle/tack/internal/store"
)

// Running code can call tools through a socket, one JSON object per line each way. The
// code is as hostile as the model that wrote it, so the channel gives it nothing the model
// doesn't already have: it can call the tools the conversation offers that run without
// asking, like note and scratch, and the custom ones, which wait for a person. Everything
// else, including run_code itself, is refused. Every request and response is recorded.
const (
	ChannelLineBytes = 64 << 10 // the longest line, either way
	ChannelCalls     = 200      // the most requests in one run
	ChannelBytes     = 1 << 20  // the most it can write in one run
)

// DefaultCodeCallDenial is what code is told about a call a person denied without saying why.
const DefaultCodeCallDenial = "The user denied this call."

// exchange is one request from running code and what it got back.
type exchange struct {
	id       string
	tool     string
	request  store.Ref
	response store.Ref
	by       string
}

// ChannelView is an exchange for the page. Answerable says a person can answer it now.
type ChannelView struct {
	ID         string `json:"id"`
	Tool       string `json:"tool,omitzero"`
	Request    string `json:"request"`
	Response   string `json:"response,omitzero"`
	By         string `json:"by,omitzero"`
	Waiting    bool   `json:"waiting,omitzero"`
	Answerable bool   `json:"answerable,omitzero"`
}

type codeAnswer struct {
	text string
	deny bool
}

type channelRequest struct {
	Tool      string         `json:"tool"`
	Arguments jsontext.Value `json:"arguments"`
}

type channelReply struct {
	Result *string `json:"result,omitzero"`
	Error  string  `json:"error,omitzero"`
}

// serveChannel answers requests from running code until conn or ctx ends, or the code goes
// past a limit, which ends the channel but not the code. While a person is being waited on,
// the run's time limit is held.
func (c *Core) serveChannel(ctx context.Context, conn io.ReadWriter, conv *conversation, call *toolCall, run Call, hold func() func(), send func(Update)) error {
	reader := bufio.NewReaderSize(conn, ChannelLineBytes+1)
	total := 0
	for n := 0; ; n++ {
		line, err := reader.ReadSlice('\n')
		if errors.Is(err, bufio.ErrBufferFull) {
			return writeReply(conn, channelReply{Error: fmt.Sprintf("a line can be at most %d bytes, so tack has stopped reading", ChannelLineBytes)})
		}
		if err != nil {
			return nil // the code closed it, or the run ended
		}
		if total += len(line); total > ChannelBytes || n >= ChannelCalls {
			return writeReply(conn, channelReply{Error: fmt.Sprintf("a run can make at most %d calls and write at most %d bytes, so tack has stopped reading", ChannelCalls, ChannelBytes)})
		}
		id := store.NewID()
		block, err := c.cfg.Store.Put(line[:len(line)-1])
		if err != nil {
			return err
		}
		var req channelRequest
		parseErr := json.Unmarshal(line, &req, json.RejectUnknownMembers(true))
		if err := c.record(&store.Event{Type: store.CodeRequest, Conversation: conv.id, ToolCall: call.ID, Execution: run.Execution,
			Channel: id, Request: block, Kind: req.Tool}); err != nil {
			return err
		}
		reply, by := c.channelCall(ctx, conv, call, run, id, req, parseErr, hold, send)
		data, err := json.Marshal(reply)
		if err != nil {
			return err
		}
		response, err := c.cfg.Store.Put(data)
		if err != nil {
			return err
		}
		if err := c.record(&store.Event{Type: store.CodeResponse, Conversation: conv.id, ToolCall: call.ID, Channel: id, Response: response, By: by}); err != nil {
			return err
		}
		if _, err := conn.Write(append(data, '\n')); err != nil {
			return nil
		}
	}
}

func writeReply(conn io.Writer, reply channelReply) error {
	data, err := json.Marshal(reply)
	if err != nil {
		return err
	}
	conn.Write(append(data, '\n'))
	return nil
}

func failed(format string, args ...any) channelReply {
	return channelReply{Error: fmt.Sprintf(format, args...)}
}

// channelCall does one request and says who answered it.
func (c *Core) channelCall(ctx context.Context, conv *conversation, call *toolCall, run Call, id string, req channelRequest, parseErr error, hold func() func(), send func(Update)) (channelReply, string) {
	if parseErr != nil {
		return failed(`a request is one line of {"tool": name, "arguments": object}: %v`, parseErr), "tack"
	}
	definitions, err := c.Definitions(conv.id)
	if err != nil {
		return failed("%v", err), "tack"
	}
	offered, _ := definedNames(definitions)
	c.mu.Lock()
	custom := conv.isCustom(req.Tool)
	c.mu.Unlock()
	tool := c.findTool(req.Tool)
	switch {
	case !slices.Contains(offered, req.Tool):
		return failed("there's no tool named %q in this conversation", req.Tool), "tack"
	case custom:
		return c.waitForPerson(ctx, conv, call, id, req, hold, send)
	case tool == nil || !tool.NoApproval:
		return failed("%s can't be called from code", req.Tool), "tack"
	}
	if len(req.Arguments) == 0 {
		req.Arguments = jsontext.Value("{}")
	}
	args, err := parseArguments(req.Arguments)
	if err != nil {
		return failed("the arguments can't be used: %v", err), "tack"
	}
	if _, err := inspect(tool, Call{Args: args, Settings: run.Settings}); err != nil {
		return failed("the arguments can't be used: %v", err), "tack"
	}
	output, err := tool.Run(ctx, Call{Args: args, Settings: run.Settings, Execution: run.Execution, AddNote: run.AddNote, Scratch: run.Scratch, Context: run.Context})
	if err != nil {
		return failed("%v", err), "tack"
	}
	result := string(output)
	return channelReply{Result: &result}, "tack"
}

// waitForPerson waits for someone to answer a call to a custom tool, which they do on the
// running call's card. The run's time limit is held meanwhile.
func (c *Core) waitForPerson(ctx context.Context, conv *conversation, call *toolCall, id string, req channelRequest, hold func() func(), send func(Update)) (channelReply, string) {
	answer := make(chan codeAnswer, 1)
	c.mu.Lock()
	if c.codeCalls == nil {
		c.codeCalls = map[string]*waitingCodeCall{}
	}
	c.codeCalls[id] = &waitingCodeCall{conv: conv.id, answer: answer}
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		delete(c.codeCalls, id)
		c.mu.Unlock()
	}()
	release := hold()
	defer release()
	data, _ := json.Marshal(map[string]any{"id": id, "tool": req.Tool, "arguments": req.Arguments})
	send(Update{Kind: "code_call", ID: call.ID, Text: string(data)})
	select {
	case a := <-answer:
		if a.deny {
			if a.text == "" {
				a.text = DefaultCodeCallDenial
			}
			return channelReply{Error: a.text}, "user"
		}
		return channelReply{Result: &a.text}, "user"
	case <-ctx.Done():
		return failed("the run ended before anyone answered"), "tack"
	}
}

type waitingCodeCall struct {
	conv   string
	answer chan codeAnswer
}

// AnswerCodeCall answers a call running code made to a custom tool. Denying it sends text,
// or a default, as the error. It works while the conversation is busy, since that's the
// only time there's anything to answer.
func (c *Core) AnswerCodeCall(convID, id, text string, deny bool) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	waiting := c.codeCalls[id]
	if waiting == nil || waiting.conv != convID {
		return ErrNoDecision
	}
	delete(c.codeCalls, id)
	waiting.answer <- codeAnswer{text: text, deny: deny}
	return nil
}

// channelViews is what a call's code asked. The caller holds mu.
func (c *Core) channelViews(call *toolCall) []ChannelView {
	var out []ChannelView
	for _, ex := range call.channel {
		view := ChannelView{ID: ex.id, Tool: ex.tool, Request: c.text([]store.Part{{Block: ex.request}}), By: ex.by}
		if ex.response != "" {
			view.Response = c.text([]store.Part{{Block: ex.response}})
		} else {
			view.Waiting = true
			view.Answerable = c.codeCalls[ex.id] != nil
		}
		out = append(out, view)
	}
	return out
}

// timeLimit is a run's time limit, which can be held while it waits on a person.
type timeLimit struct {
	mu      sync.Mutex
	timer   *time.Timer
	left    time.Duration
	started time.Time
	holds   int
	cancel  context.CancelFunc
}

func pausableTimeout(parent context.Context, timeout time.Duration, cause error) (context.Context, *timeLimit) {
	ctx, cancel := context.WithCancelCause(parent)
	l := &timeLimit{left: timeout, started: time.Now(), cancel: func() { cancel(nil) }}
	l.timer = time.AfterFunc(timeout, func() { cancel(cause) })
	return ctx, l
}

// hold stops the clock until the function it returns is called.
func (l *timeLimit) hold() func() {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.holds++; l.holds == 1 && l.timer.Stop() {
		l.left -= time.Since(l.started)
	}
	var once sync.Once
	return func() {
		once.Do(func() {
			l.mu.Lock()
			defer l.mu.Unlock()
			if l.holds--; l.holds == 0 {
				l.started = time.Now()
				l.timer.Reset(max(l.left, 0))
			}
		})
	}
}

func (l *timeLimit) stop() {
	l.mu.Lock()
	l.timer.Stop()
	l.mu.Unlock()
	l.cancel()
}
