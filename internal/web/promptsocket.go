package web

import (
	"context"
	"errors"
	"log"
	"net/http"
	"strconv"
	"sync"

	"encoding/json/jsontext"
	"encoding/json/v2"

	"github.com/betawaffle/tack/internal/core"
	"github.com/betawaffle/tack/internal/store"
	"github.com/betawaffle/tack/internal/tokens"
)

// The prompt page's socket. The page opens a draft over it, sends each change to the draft as a
// patch of its request, and gets back a patch of the prompt that's drawn: a long draft changes
// a little at a time, so very little goes either way. Both ends keep what they last sent and
// got for as long as the connection lasts. Opening another draft, like the one after a call
// that was just sent, sends it as a patch of the one that was open. A page that connects again
// starts from nothing.
//
// The page sends {"id": n, "op": ...} and gets {"re": n, ...} back:
//
//	open {"draft": id} or {"from": call}: the draft, as GET /api/drafts/{id} or /api/draft-for
//	    has it, but with "request" and "prompt" as patches of what was open
//	save {"patch": p, "off": [...], "off_tools": [...]}: the draft's request patched and saved,
//	    as PUT /api/drafts/{id} does, making the draft first if it isn't yet ("id" says what it
//	    is then), with "prompt" a patch of the last prompt sent
//	send {}: the draft sent, as POST /api/drafts/{id}/send does. First comes {"for": n,
//	    "call": id} naming the call, then {"for": n, "update": u} for each part of the reply as it
//	    comes, and the answer once it's done
//	run {"call": id, "flags": [...]}: one of its tool calls run, as POST
//	    /api/drafts/{id}/calls/{call}/run does, with "run" what it gave, and "request" and
//	    "prompt" as patches, since the result is in the draft now
//	stop {"which": n}: the send or run that request n started is stopped
//
// A failure is {"re": n, "error": text}, and changes nothing. Open and save answer in order, and
// after each the server sends {"estimate": ...} with what the draft would cost, which can take
// longer. A send or run goes on while other requests are answered, and its answer comes when
// it's done.

type promptSession struct {
	s *server
	c socket

	// mu guards what follows, and is held while what's sent about it is sent, so that the page
	// gets each patch in the order they were made.
	mu      sync.Mutex
	open    bool
	id      string
	from    string
	request jsontext.Value // the draft's request as the page has it, or empty for none yet
	prompt  jsontext.Value // the prompt last sent, as it's sent (wire.go), or empty for none yet
	// running stops each send and run that's going, by the request that started it.
	running map[int]func() error
}

type socketRequest struct {
	ID       int            `json:"id"`
	Op       string         `json:"op"`
	Draft    string         `json:"draft,omitzero"`
	From     *string        `json:"from,omitzero"`
	Patch    jsontext.Value `json:"patch,omitzero"`
	Off      []int          `json:"off,omitzero"`
	OffTools []int          `json:"off_tools,omitzero"`
	Call     string         `json:"call,omitzero"`
	Flags    []string       `json:"flags,omitzero"`
	Which    int            `json:"which,omitzero"`
}

type socketReply struct {
	Re      int            `json:"re"`
	Error   string         `json:"error,omitzero"`
	ID      string         `json:"id,omitzero"`
	Draft   *store.Draft   `json:"draft,omitzero"`
	Request jsontext.Value `json:"request,omitzero"`
	Prompt  jsontext.Value `json:"prompt,omitzero"`
	Problem string         `json:"problem,omitzero"`
	Call    string         `json:"call,omitzero"`
	Run     *core.DraftRun `json:"run,omitzero"`
}

func (s *server) promptSocket(w http.ResponseWriter, r *http.Request) {
	c, err := acceptSocket(w, r, maxPreviewBytes)
	if err != nil {
		return
	}
	if r.URL.Query().Get("page") != strconv.Itoa(pageRequired) {
		c.Close(closeOutOfDate, "This page is out of date. Reload it.")
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go c.keepAlive(ctx)
	(&promptSession{s: s, c: c}).serve(ctx)
}

// serve answers what the page sends until it goes. What's going when it goes carries on: a
// call that was sent is recorded either way.
func (p *promptSession) serve(ctx context.Context) {
	defer p.c.Close(closeNormal, "")
	for {
		_, data, err := p.c.Read(ctx)
		if err != nil {
			return
		}
		var req socketRequest
		if err := json.Unmarshal(data, &req, json.RejectUnknownMembers(true)); err != nil {
			p.send(socketReply{Re: req.ID, Error: "bad request: " + err.Error()})
			continue
		}
		switch req.Op {
		case "send":
			go p.sendDraft(req)
		case "run":
			go p.runCall(req)
		case "stop":
			p.mu.Lock()
			stop := p.running[req.Which]
			p.mu.Unlock()
			reply := socketReply{Re: req.ID}
			if stop == nil {
				reply.Error = core.ErrNothingRunning.Error()
			} else if err := stop(); err != nil {
				reply.Error = err.Error()
			}
			p.send(reply)
		default:
			p.mu.Lock()
			reply := p.handle(req)
			reply.Re = req.ID
			p.send(reply)
			request := p.request
			p.mu.Unlock()
			if reply.Error == "" {
				p.estimate(ctx, request)
			}
		}
	}
}

// send sends v to the page. If the page has gone, nothing's sent, and reading finds that out.
func (p *promptSession) send(v any) {
	out, err := json.Marshal(v)
	if err != nil {
		log.Printf("socket: %v", err)
		return
	}
	p.c.Write(context.Background(), messageText, out)
}

// handle opens and saves. The caller holds mu.
func (p *promptSession) handle(req socketRequest) socketReply {
	switch req.Op {
	case "open":
		var d *store.Draft
		var cp core.CallPrompt
		var err error
		switch {
		case req.Draft != "":
			d, cp, err = p.s.core.Draft(req.Draft)
		case req.From != nil:
			d, cp, err = p.s.core.DraftFor(*req.From)
		default:
			return socketReply{Error: "open needs a draft or where it's from"}
		}
		if d == nil {
			return socketReply{Error: errorText(err)}
		}
		prompt, problem, err := wirePrompt(cp, err)
		if err != nil {
			return socketReply{Error: errorText(err)}
		}
		reply, err := p.patches(d.Request, prompt)
		if err != nil {
			return socketReply{Error: errorText(err)}
		}
		// The request goes as a patch, rather than in the draft.
		shown := *d
		shown.Request = jsontext.Value("null")
		reply.Draft, reply.Problem = &shown, problem
		p.open, p.id, p.from = true, d.ID, d.From
		return reply
	case "save":
		if !p.open {
			return socketReply{Error: "no draft is open"}
		}
		request, err := applyJSON(p.request, req.Patch)
		if err != nil || !request.IsValid() {
			return socketReply{Error: "bad patch"}
		}
		if p.id == "" {
			var d *store.Draft
			if p.from != "" {
				d, err = p.s.core.NewDraft(p.from)
			} else {
				d, err = p.s.blankDraft()
			}
			if err != nil {
				return socketReply{Error: errorText(err)}
			}
			p.id = d.ID
		}
		cp, err := p.s.core.SaveDraft(p.id, request, req.Off, req.OffTools)
		prompt, problem, err := wirePrompt(cp, err)
		if err != nil {
			return socketReply{ID: p.id, Error: errorText(err)}
		}
		patch, err := diffUnordered(p.prompt, prompt)
		if err != nil {
			return socketReply{ID: p.id, Error: errorText(err)}
		}
		p.request, p.prompt = request, prompt
		return socketReply{ID: p.id, Prompt: patch, Problem: problem}
	}
	return socketReply{Error: "unknown op " + strconv.Quote(req.Op)}
}

// patches makes request and prompt what the page has, and says how they changed. The caller
// holds mu.
func (p *promptSession) patches(request, prompt jsontext.Value) (socketReply, error) {
	requestPatch, err := diffJSON(p.request, request)
	if err != nil {
		return socketReply{}, err
	}
	promptPatch, err := diffUnordered(p.prompt, prompt)
	if err != nil {
		return socketReply{}, err
	}
	p.request, p.prompt = request, prompt
	return socketReply{Request: requestPatch, Prompt: promptPatch}, nil
}

// going says how to stop what request n started, until it's done.
func (p *promptSession) going(n int, stop func() error) (done func()) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.running == nil {
		p.running = map[int]func() error{}
	}
	p.running[n] = stop
	return func() {
		p.mu.Lock()
		delete(p.running, n)
		p.mu.Unlock()
	}
}

// openID is the open draft's ID, once it's saved.
func (p *promptSession) openID() (string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.open || p.id == "" {
		return "", errors.New("the draft isn't saved yet")
	}
	return p.id, nil
}

func (p *promptSession) sendDraft(req socketRequest) {
	id, err := p.openID()
	if err != nil {
		p.send(socketReply{Re: req.ID, Error: err.Error()})
		return
	}
	call, updates, err := p.s.core.SendDraft(id)
	if err != nil {
		p.send(socketReply{Re: req.ID, Error: errorText(err)})
		return
	}
	done := p.going(req.ID, func() error { return p.s.core.CancelManual(call) })
	defer done()
	p.send(map[string]any{"for": req.ID, "call": call})
	for u := range updates {
		p.send(map[string]any{"for": req.ID, "update": u})
	}
	p.send(socketReply{Re: req.ID, Call: call})
}

func (p *promptSession) runCall(req socketRequest) {
	id, err := p.openID()
	if err != nil {
		p.send(socketReply{Re: req.ID, Error: err.Error()})
		return
	}
	done := p.going(req.ID, func() error { return p.s.core.StopDraftRun(id, req.Call) })
	run, err := p.s.core.RunDraftCall(id, req.Call, req.Flags)
	done()
	if err != nil {
		p.send(socketReply{Re: req.ID, Error: errorText(err)})
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	reply := socketReply{Re: req.ID, Run: &run}
	// The result is in the draft now, so what the page has of it changes, if it's still open.
	if p.open && p.id == id {
		d, cp, err := p.s.core.Draft(id)
		if d == nil {
			p.send(socketReply{Re: req.ID, Error: errorText(err)})
			return
		}
		prompt, problem, err := wirePrompt(cp, err)
		if err == nil {
			var patched socketReply
			if patched, err = p.patches(d.Request, prompt); err == nil {
				reply.Request, reply.Prompt, reply.Problem = patched.Request, patched.Prompt, problem
			}
		}
		if err != nil {
			reply.Error = errorText(err)
		}
	}
	p.send(reply)
}

// wirePrompt is a prompt as it's sent, or null with why when there's no renderer for it.
func wirePrompt(cp core.CallPrompt, err error) (jsontext.Value, string, error) {
	if errors.Is(err, tokens.ErrNoRenderer) {
		return jsontext.Value("null"), err.Error(), nil
	}
	if err != nil {
		return nil, "", err
	}
	data, err := json.Marshal(toWire(cp))
	return data, "", err
}

// estimate sends what request would cost, if it can say.
func (p *promptSession) estimate(ctx context.Context, request jsontext.Value) {
	if len(request) == 0 {
		return
	}
	e, err := p.s.core.EstimateRequest(ctx, request)
	if err != nil {
		return
	}
	p.send(map[string]any{"estimate": e})
}

// errorText is what an error says to the page, as fail would.
func errorText(err error) string {
	if err == nil {
		return "not found"
	}
	return err.Error()
}
