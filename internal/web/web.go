// Package web serves the browser page and its small JSON API.
package web

import (
	"embed"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/betawaffle/tack/internal/core"
	"github.com/betawaffle/tack/internal/prompt"
	"github.com/betawaffle/tack/internal/store"
)

//go:embed static/index.html static/style.css static/app.js static/markdown.js static/json.js
var static embed.FS

const maxMessageBytes = 8 << 20

// Defaults are what the page offers for a new conversation. Sections are read each
// time, so editing them doesn't need a restart.
type Defaults struct {
	Sections []prompt.Section `json:"sections"`
	Workdir  string           `json:"workdir"`
	Tools    []core.ToolInfo  `json:"tools"`
	Settings store.Settings   `json:"settings"`
}

// Reload is the result of reloading the config. NeedsRestart names settings that
// changed but only take effect at startup.
type Reload struct {
	NeedsRestart []string `json:"needs_restart"`
}

// Handler returns the whole site.
func Handler(c *core.Core, defaults func() Defaults, reload func() (Reload, error), hosts []string) http.Handler {
	s := &server{core: c, defaults: defaults, reload: reload}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", s.index)
	mux.Handle("GET /static/", http.FileServerFS(static))
	mux.HandleFunc("GET /api/conversations", s.list)
	mux.HandleFunc("POST /api/conversations", s.create)
	mux.HandleFunc("GET /api/conversations/{id}", s.get)
	mux.HandleFunc("GET /api/conversations/{id}/tools", s.definitions)
	mux.HandleFunc("POST /api/conversations/{id}/fork", s.fork)
	mux.HandleFunc("POST /api/conversations/{id}/messages", s.send)
	mux.HandleFunc("POST /api/conversations/{id}/held", s.hold)
	mux.HandleFunc("POST /api/conversations/{id}/reply", s.reply)
	mux.HandleFunc("POST /api/conversations/{id}/system", s.addSystem)
	mux.HandleFunc("POST /api/conversations/{id}/system/{message}/edits", s.proposeEdit)
	mux.HandleFunc("POST /api/conversations/{id}/edits/{edit}/apply", s.applyEdit)
	mux.HandleFunc("POST /api/conversations/{id}/edits/{edit}/discard", s.discardEdit)
	mux.HandleFunc("POST /api/conversations/{id}/decisions/{decision}", s.decide)
	mux.HandleFunc("POST /api/conversations/{id}/budget", s.addBudget)
	mux.HandleFunc("POST /api/conversations/{id}/pause", s.pause)
	mux.HandleFunc("POST /api/conversations/{id}/cancel", s.cancelReply)
	mux.HandleFunc("POST /api/conversations/{id}/tools/{call}/stop", s.stopTool)
	mux.HandleFunc("POST /api/conversations/{id}/settings", s.proposeSettings)
	mux.HandleFunc("POST /api/conversations/{id}/settings/{proposal}/apply", s.applySettings)
	mux.HandleFunc("POST /api/conversations/{id}/settings/{proposal}/discard", s.discardSettings)
	mux.HandleFunc("GET /api/defaults", s.newDefaults)
	mux.HandleFunc("GET /api/status", s.status)
	mux.HandleFunc("POST /api/reload", s.reloadConfig)
	// Other websites open in the same browser shouldn't be able to post here.
	return knownHost(hosts, security(http.NewCrossOriginProtection().Handler(mux)))
}

// knownHost refuses requests for hostnames it doesn't know. Without this, a website
// could point its own name at this machine, which makes the browser treat this site
// as that website's own: DNS rebinding. Tools can run commands from here, so that
// would be serious. IP addresses and localhost can't be rebound to.
func knownHost(hosts []string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host := r.Host
		if name, _, err := net.SplitHostPort(host); err == nil {
			host = name
		}
		host = strings.Trim(host, "[]")
		if net.ParseIP(host) == nil && !strings.EqualFold(host, "localhost") && !slices.ContainsFunc(hosts, func(known string) bool { return strings.EqualFold(known, host) }) {
			http.Error(w, "tack doesn't know this hostname. To allow it, add it to allowed_hosts in the config and restart.", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
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
	core     *core.Core
	defaults func() Defaults
	reload   func() (Reload, error)
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
	var body struct {
		System   string   `json:"system"`
		Sections []string `json:"sections"`
		Tools    bool     `json:"tools"`
		Workdir  string   `json:"workdir"`

		LeaveOut    []string                  `json:"leave_out"`
		Definitions map[string]jsontext.Value `json:"definitions"`
		Settings    *store.Settings           `json:"settings"`
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxMessageBytes)
	if err := json.UnmarshalRead(r.Body, &body); err != nil {
		http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
		return
	}
	defaults := s.defaults()
	// An empty working directory is fine: it leaves out the tools that need one.
	if body.Tools && body.Workdir != "" {
		info, err := os.Stat(body.Workdir)
		if err != nil || !info.IsDir() || !filepath.IsAbs(body.Workdir) {
			http.Error(w, "the working directory has to be an absolute path to a directory", http.StatusBadRequest)
			return
		}
	}
	// If the text is exactly what the sections make, it's stored section by section.
	// Otherwise it was edited, or a section file changed, and the text as sent wins.
	blocks := prompt.Blocks(defaults.Sections, body.Sections, body.Workdir)
	id, err := s.core.NewConversationWith(core.Options{
		System: body.System, Blocks: blocks, Tools: body.Tools, Workdir: body.Workdir,
		LeaveOut: body.LeaveOut, Definitions: body.Definitions, Settings: body.Settings,
	})
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

func (s *server) fork(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Items int `json:"items"`
	}
	if err := json.UnmarshalRead(r.Body, &body); err != nil {
		http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
		return
	}
	id, err := s.core.Fork(r.PathValue("id"), body.Items)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, map[string]string{"id": id})
}

func (s *server) definitions(w http.ResponseWriter, r *http.Request) {
	data, err := s.core.Definitions(r.PathValue("id"))
	if err != nil {
		fail(w, err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Write(data)
}

func (s *server) status(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, s.core.CurrentStatus())
}

func (s *server) newDefaults(w http.ResponseWriter, _ *http.Request) {
	defaults := s.defaults()
	writeJSON(w, defaults)
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

func (s *server) hold(w http.ResponseWriter, r *http.Request) {
	text, ok := readText(w, r, "text")
	if !ok {
		return
	}
	if strings.TrimSpace(text) == "" {
		http.Error(w, "empty message", http.StatusBadRequest)
		return
	}
	if err := s.core.HoldMessage(r.PathValue("id"), text); err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, map[string]bool{"ok": true})
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

func (s *server) addBudget(w http.ResponseWriter, r *http.Request) {
	var body struct {
		AmountUSD float64 `json:"amount_usd"`
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxMessageBytes)
	if err := json.UnmarshalRead(r.Body, &body); err != nil {
		http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
		return
	}
	if err := s.core.AddBudget(r.PathValue("id"), body.AmountUSD); err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, map[string]bool{"ok": true})
}

func (s *server) pause(w http.ResponseWriter, r *http.Request) {
	if err := s.core.Pause(r.PathValue("id")); err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, map[string]bool{"ok": true})
}

func (s *server) cancelReply(w http.ResponseWriter, r *http.Request) {
	if err := s.core.CancelReply(r.PathValue("id")); err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, map[string]bool{"ok": true})
}

func (s *server) stopTool(w http.ResponseWriter, r *http.Request) {
	if err := s.core.StopTool(r.PathValue("id"), r.PathValue("call")); err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, map[string]bool{"ok": true})
}

func (s *server) proposeSettings(w http.ResponseWriter, r *http.Request) {
	var settings store.Settings
	r.Body = http.MaxBytesReader(w, r.Body, maxMessageBytes)
	if err := json.UnmarshalRead(r.Body, &settings, json.RejectUnknownMembers(true)); err != nil {
		http.Error(w, "bad settings: "+err.Error(), http.StatusBadRequest)
		return
	}
	id, err := s.core.ProposeSettings(r.PathValue("id"), settings)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, map[string]string{"proposal": id})
}

func (s *server) applySettings(w http.ResponseWriter, r *http.Request) {
	if err := s.core.ApplySettings(r.PathValue("id"), r.PathValue("proposal")); err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, map[string]bool{"ok": true})
}

func (s *server) discardSettings(w http.ResponseWriter, r *http.Request) {
	if err := s.core.DiscardSettings(r.PathValue("id"), r.PathValue("proposal")); err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, map[string]bool{"ok": true})
}

// reloadConfig reports failure properly, which a signal can't do. A config that
// can't be read leaves the running one in place.
func (s *server) reloadConfig(w http.ResponseWriter, _ *http.Request) {
	result, err := s.reload()
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	writeJSON(w, result)
}

func (s *server) decide(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Allow      []string `json:"allow"`
		Network    []string `json:"network"`
		DenialText string   `json:"denial_text"`
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxMessageBytes)
	if err := json.UnmarshalRead(r.Body, &body); err != nil {
		http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
		return
	}
	updates, err := s.core.DecideWithNetwork(r.PathValue("id"), r.PathValue("decision"), body.Allow, body.Network, body.DenialText)
	if err != nil {
		fail(w, err)
		return
	}
	stream(w, r, updates)
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
	case errors.Is(err, core.ErrNotFound), errors.Is(err, core.ErrNoMessage), errors.Is(err, core.ErrNoEdit), errors.Is(err, core.ErrNoDecision):
		http.Error(w, err.Error(), http.StatusNotFound)
	case errors.Is(err, core.ErrBusy), errors.Is(err, core.ErrNothingToSend), errors.Is(err, core.ErrAwaiting), errors.Is(err, core.ErrNothingRunning):
		http.Error(w, err.Error(), http.StatusConflict)
	case errors.Is(err, core.ErrBadChoice), errors.Is(err, core.ErrBadSettings), errors.Is(err, core.ErrBadTools), errors.Is(err, core.ErrBadFork):
		http.Error(w, err.Error(), http.StatusBadRequest)
	case errors.Is(err, core.ErrRestarting):
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
	default:
		log.Printf("request failed: %v", err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}
