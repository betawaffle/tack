// Package web serves the browser page and its small JSON API.
package web

import (
	_ "embed"
	"encoding/json/v2"
	"errors"
	"log"
	"net/http"
	"strings"

	"github.com/betawaffle/tack/internal/core"
)

//go:embed index.html
var page []byte

const maxMessageBytes = 8 << 20

// Handler returns the whole site.
func Handler(c *core.Core) http.Handler {
	s := &server{core: c}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", s.index)
	mux.HandleFunc("GET /api/conversations", s.list)
	mux.HandleFunc("POST /api/conversations", s.create)
	mux.HandleFunc("GET /api/conversations/{id}", s.get)
	mux.HandleFunc("POST /api/conversations/{id}/messages", s.send)
	// Other websites open in the same browser shouldn't be able to post here.
	return http.NewCrossOriginProtection().Handler(mux)
}

type server struct {
	core *core.Core
}

func (s *server) index(w http.ResponseWriter, _ *http.Request) {
	// Model output is only ever shown as text. This is the second layer: even
	// if that failed, the page couldn't load or send anything elsewhere.
	w.Header().Set("Content-Security-Policy",
		"default-src 'none'; script-src 'unsafe-inline'; style-src 'unsafe-inline'; connect-src 'self'; base-uri 'none'; form-action 'none'")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write(page)
}

func (s *server) list(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, s.core.Conversations())
}

func (s *server) create(w http.ResponseWriter, _ *http.Request) {
	id, err := s.core.NewConversation()
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

// send streams updates as one JSON object per line. If the browser goes away
// the call carries on: it belongs to the core, not to this request.
func (s *server) send(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Text string `json:"text"`
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxMessageBytes)
	if err := json.UnmarshalRead(r.Body, &body); err != nil {
		http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
		return
	}
	if strings.TrimSpace(body.Text) == "" {
		http.Error(w, "empty message", http.StatusBadRequest)
		return
	}
	updates, err := s.core.Send(r.PathValue("id"), body.Text)
	if err != nil {
		fail(w, err)
		return
	}

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
	case errors.Is(err, core.ErrNotFound):
		http.Error(w, err.Error(), http.StatusNotFound)
	case errors.Is(err, core.ErrBusy):
		http.Error(w, err.Error(), http.StatusConflict)
	default:
		log.Printf("request failed: %v", err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}
