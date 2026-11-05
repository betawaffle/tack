// Package web serves the browser page and its small JSON API.
package web

import (
	"embed"
	"encoding/json/v2"
	"errors"
	"log"
	"net/http"
	"strings"

	"github.com/betawaffle/tack/internal/core"
)

//go:embed static/index.html static/style.css static/app.js static/markdown.js
var static embed.FS

const maxMessageBytes = 8 << 20

// Handler returns the whole site.
func Handler(c *core.Core, defaultPrompt func() string) http.Handler {
	s := &server{core: c, defaultPrompt: defaultPrompt}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", s.index)
	mux.Handle("GET /static/", http.FileServerFS(static))
	mux.HandleFunc("GET /api/conversations", s.list)
	mux.HandleFunc("POST /api/conversations", s.create)
	mux.HandleFunc("GET /api/conversations/{id}", s.get)
	mux.HandleFunc("POST /api/conversations/{id}/messages", s.send)
	mux.HandleFunc("POST /api/conversations/{id}/reply", s.reply)
	mux.HandleFunc("POST /api/conversations/{id}/system", s.addSystem)
	mux.HandleFunc("POST /api/conversations/{id}/system/{message}/edits", s.proposeEdit)
	mux.HandleFunc("POST /api/conversations/{id}/edits/{edit}/apply", s.applyEdit)
	mux.HandleFunc("POST /api/conversations/{id}/edits/{edit}/discard", s.discardEdit)
	mux.HandleFunc("GET /api/system-default", s.systemDefault)
	// Other websites open in the same browser shouldn't be able to post here.
	return security(http.NewCrossOriginProtection().Handler(mux))
}

// security sets the headers every response gets. Model output is only ever shown
// as text or built into DOM nodes. This policy is the second layer: even if that
// failed, the page couldn't run injected code, or load or send anything elsewhere.
// That includes images, so a reply can't leak anything through an image's URL.
func security(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Security-Policy",
			"default-src 'none'; script-src 'self'; style-src 'self'; connect-src 'self'; base-uri 'none'; form-action 'none'; frame-ancestors 'none'")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		if strings.HasPrefix(r.URL.Path, "/static/") {
			// The files are embedded in the binary, so a restart can change them.
			w.Header().Set("Cache-Control", "no-cache")
		}
		next.ServeHTTP(w, r)
	})
}

type server struct {
	core          *core.Core
	defaultPrompt func() string
}

// readText reads the JSON body every write endpoint takes.
func readText(w http.ResponseWriter, r *http.Request, field string) (string, bool) {
	var body map[string]string
	r.Body = http.MaxBytesReader(w, r.Body, maxMessageBytes)
	if err := json.UnmarshalRead(r.Body, &body); err != nil {
		http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
		return "", false
	}
	return body[field], true
}

func (s *server) index(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-cache")
	http.ServeFileFS(w, r, static, "static/index.html")
}

func (s *server) list(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, s.core.Conversations())
}

func (s *server) create(w http.ResponseWriter, r *http.Request) {
	system, ok := readText(w, r, "system")
	if !ok {
		return
	}
	id, err := s.core.NewConversation(system)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, map[string]string{"id": id})
}

func (s *server) get(w http.ResponseWriter, r *http.Request) {
	conv, err := s.core.Conversation(r.PathValue("id"))
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, conv)
}

func (s *server) systemDefault(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, map[string]string{"text": s.defaultPrompt()})
}

func (s *server) send(w http.ResponseWriter, r *http.Request) {
	text, ok := readText(w, r, "text")
	if !ok {
		return
	}
	if strings.TrimSpace(text) == "" {
		http.Error(w, "empty message", http.StatusBadRequest)
		return
	}
	updates, err := s.core.Send(r.PathValue("id"), text)
	if err != nil {
		fail(w, err)
		return
	}
	stream(w, r, updates)
}

func (s *server) reply(w http.ResponseWriter, r *http.Request) {
	updates, err := s.core.Reply(r.PathValue("id"))
	if err != nil {
		fail(w, err)
		return
	}
	stream(w, r, updates)
}

func (s *server) addSystem(w http.ResponseWriter, r *http.Request) {
	text, ok := readText(w, r, "text")
	if !ok {
		return
	}
	if strings.TrimSpace(text) == "" {
		http.Error(w, "empty message", http.StatusBadRequest)
		return
	}
	id, err := s.core.AddSystemMessage(r.PathValue("id"), text)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, map[string]string{"message": id})
}

func (s *server) proposeEdit(w http.ResponseWriter, r *http.Request) {
	text, ok := readText(w, r, "text")
	if !ok {
		return
	}
	id, err := s.core.ProposeEdit(r.PathValue("id"), r.PathValue("message"), text)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, map[string]string{"edit": id})
}

func (s *server) applyEdit(w http.ResponseWriter, r *http.Request) {
	if err := s.core.ApplyEdit(r.PathValue("id"), r.PathValue("edit")); err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, map[string]bool{"ok": true})
}

func (s *server) discardEdit(w http.ResponseWriter, r *http.Request) {
	if err := s.core.DiscardEdit(r.PathValue("id"), r.PathValue("edit")); err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, map[string]bool{"ok": true})
}

// stream sends updates as one JSON object per line. If the browser goes away
// the call carries on: it belongs to the core, not to this request.
func stream(w http.ResponseWriter, r *http.Request, updates <-chan core.Update) {
	w.Header().Set("Content-Type", "application/x-ndjson")
	w.Header().Set("Cache-Control", "no-store")
	rc := http.NewResponseController(w)
	rc.Flush()
	for {
		select {
		case u, ok := <-updates:
			if !ok {
				return
			}
			line, err := json.Marshal(u)
			if err != nil {
				return
			}
			if _, err := w.Write(append(line, '\n')); err != nil {
				return
			}
			rc.Flush()
		case <-r.Context().Done():
			return
		}
	}
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	if err := json.MarshalWrite(w, v); err != nil {
		log.Printf("writing response: %v", err)
	}
}

func fail(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, core.ErrNotFound), errors.Is(err, core.ErrNoMessage), errors.Is(err, core.ErrNoEdit):
		http.Error(w, err.Error(), http.StatusNotFound)
	case errors.Is(err, core.ErrBusy), errors.Is(err, core.ErrNothingToSend):
		http.Error(w, err.Error(), http.StatusConflict)
	default:
		log.Printf("request failed: %v", err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}
