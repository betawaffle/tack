package web

import (
	"context"
	"errors"
	"log"
	"net/http"
	"strconv"

	"encoding/json/jsontext"
	"encoding/json/v2"

	"github.com/betawaffle/tack/internal/core"
	"github.com/betawaffle/tack/internal/store"
	"github.com/betawaffle/tack/internal/tokens"
)

// The prompt page's socket. The page opens a draft over it, then sends each change to the draft
// as a patch of its request, and gets back a patch of the prompt that's drawn: a long draft
// changes a little at a time, so very little goes either way. Both ends keep what they last
// sent and got, for as long as the connection lasts; a page that connects again opens the
// draft again, and starts from the whole of it.
//
// The page sends {"id": n, "op": ...} and gets {"re": n, ...} back, in order:
//
//	open {"draft": id} or {"from": call}: the draft, as GET /api/drafts/{id} or /api/draft-for
//	    has it, with "prompt" a patch from nothing
//	save {"patch": p, "off": [...], "off_tools": [...]}: the draft's request patched and saved,
//	    as PUT /api/drafts/{id} does, making the draft first if it isn't yet ("id" says what it
//	    is then), with "prompt" a patch from the last prompt sent
//
// A failure is {"re": n, "error": text}, and changes nothing. After each, the server sends
// {"estimate": ...} with what the draft would cost, which can take longer.

type promptSession struct {
	s       *server
	open    bool
	id      string
	from    string
	request jsontext.Value // the draft's request as the page has it
	prompt  jsontext.Value // the prompt last sent, as it's sent (wire.go), or empty for none yet
}

type socketRequest struct {
	ID       int            `json:"id"`
	Op       string         `json:"op"`
	Draft    string         `json:"draft,omitzero"`
	From     *string        `json:"from,omitzero"`
	Patch    jsontext.Value `json:"patch,omitzero"`
	Off      []int          `json:"off,omitzero"`
	OffTools []int          `json:"off_tools,omitzero"`
}

type socketReply struct {
	Re      int            `json:"re"`
	Error   string         `json:"error,omitzero"`
	ID      string         `json:"id,omitzero"`
	Draft   *store.Draft   `json:"draft,omitzero"`
	Prompt  jsontext.Value `json:"prompt,omitzero"`
	Problem string         `json:"problem,omitzero"`
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
	runSocket(ctx, c, &promptSession{s: s})
}

// runSocket answers what the page sends, one at a time, until it goes.
func runSocket(ctx context.Context, c socket, p *promptSession) {
	defer c.Close(closeNormal, "")
	for {
		_, data, err := c.Read(ctx)
		if err != nil {
			return
		}
		var req socketRequest
		var reply socketReply
		if err := json.Unmarshal(data, &req, json.RejectUnknownMembers(true)); err != nil {
			reply.Error = "bad request: " + err.Error()
		} else {
			reply = p.handle(req)
		}
		reply.Re = req.ID
		out, err := json.Marshal(reply)
		if err != nil {
			log.Printf("socket reply: %v", err)
			return
		}
		if c.Write(ctx, messageText, out) != nil {
			return
		}
		if reply.Error == "" && p.open {
			if out := p.estimate(ctx); out != nil && c.Write(ctx, messageText, out) != nil {
				return
			}
		}
	}
}

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
		reply := socketReply{Draft: d}
		prompt, problem, err := wirePrompt(cp, err)
		if err != nil {
			return socketReply{Error: errorText(err)}
		}
		if reply.Prompt, err = diffJSON(nil, prompt); err != nil {
			return socketReply{Error: errorText(err)}
		}
		reply.Problem = problem
		*p = promptSession{s: p.s, open: true, id: d.ID, from: d.From, request: d.Request, prompt: prompt}
		return reply
	case "save":
		if !p.open {
			return socketReply{Error: "no draft is open"}
		}
		request, err := applyJSON(p.request, req.Patch)
		if err != nil || !request.IsValid() {
			return socketReply{Error: "bad patch"}
		}
		reply := socketReply{}
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
		reply.ID = p.id
		cp, err := p.s.core.SaveDraft(p.id, request, req.Off, req.OffTools)
		prompt, problem, err := wirePrompt(cp, err)
		if err != nil {
			return socketReply{ID: p.id, Error: errorText(err)}
		}
		if reply.Prompt, err = diffJSON(p.prompt, prompt); err != nil {
			return socketReply{ID: p.id, Error: errorText(err)}
		}
		reply.Problem = problem
		p.request, p.prompt = request, prompt
		return reply
	}
	return socketReply{Error: "unknown op " + strconv.Quote(req.Op)}
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

// estimate is what the open draft would cost, as a message to send, or nil if it can't say.
func (p *promptSession) estimate(ctx context.Context) []byte {
	e, err := p.s.core.EstimateRequest(ctx, p.request)
	if err != nil {
		return nil
	}
	out, err := json.Marshal(map[string]any{"estimate": e})
	if err != nil {
		return nil
	}
	return out
}

// errorText is what an error says to the page, as fail would.
func errorText(err error) string {
	if err == nil {
		return "not found"
	}
	return err.Error()
}
