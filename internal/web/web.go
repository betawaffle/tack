// Package web serves the browser page and its small JSON API.
package web

import (
	"bytes"
	"context"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"encoding/json/v2"
	"errors"
	"io"
	"io/fs"
	"log"
	"net"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/betawaffle/tack/internal/core"
	"github.com/betawaffle/tack/internal/docs"
	"github.com/betawaffle/tack/internal/prompt"
	"github.com/betawaffle/tack/internal/store"
)

//go:embed static/index.html static/requests.html static/requests.js static/requests.css static/style.css static/app.js static/start.js static/markdown.js static/json.js static/money.js static/highlight.js static/code.html static/docs.html static/browse.js static/browse.css
var static embed.FS

const maxMessageBytes = 8 << 20

// pageRequired changes when a page that's already open would do the wrong thing with
// this server, so that it has to be reloaded. Any other change to the page's files only
// offers a reload. Pages send it with every request, and it's checked there too, since
// they might not have heard yet.
const pageRequired = 4

const pageRequiredHeader = "X-Tack-Page-Required"

// pageVersion identifies the page's files, so an open page can tell if it's out of date.
var pageVersion = sync.OnceValue(func() string {
	hash := sha256.New()
	entries, err := static.ReadDir("static")
	if err != nil {
		panic(err)
	}
	for _, entry := range entries {
		data, err := static.ReadFile("static/" + entry.Name())
		if err != nil {
			panic(err)
		}
		hash.Write(data)
	}
	return hex.EncodeToString(hash.Sum(nil))[:16]
})

// indexPage is the page, told what it is.
var indexPage = sync.OnceValue(func() []byte {
	data, err := static.ReadFile("static/index.html")
	if err != nil {
		panic(err)
	}
	return []byte(strings.NewReplacer("{{page_version}}", pageVersion(), "{{page_required}}", strconv.Itoa(pageRequired)).Replace(string(data)))
})

// requestsPage is the page for sending hand-written requests.
var requestsPage = sync.OnceValue(func() []byte {
	data, err := static.ReadFile("static/requests.html")
	if err != nil {
		panic(err)
	}
	return []byte(strings.NewReplacer("{{page_version}}", pageVersion(), "{{page_required}}", strconv.Itoa(pageRequired)).Replace(string(data)))
})

// siteStatus is core.Status, plus what the page needs to tell if it's out of date.
type siteStatus struct {
	core.Status
	PageVersion  string `json:"page_version"`
	PageRequired int    `json:"page_required"`
}

// Defaults are what the page offers for a new conversation. The prompt is read each
// time, so editing it doesn't need a restart. Tools are tack's own, and Prompt says which
// of those to offer, in what order, with which descriptions, and any custom ones.
type Defaults struct {
	Prompt   prompt.Defaults `json:"prompt"`
	Problem  string          `json:"problem,omitzero"` // why the saved prompt couldn't be read
	Tools    []core.ToolInfo `json:"tools"`
	Settings store.Settings  `json:"settings"`
	Hosts    []string        `json:"hosts"`
	Efforts  []string        `json:"efforts"`
	Date     string          `json:"date"` // what {{date}} would be filled with now
	// Builtin is what tack would start with if nothing were saved.
	Builtin prompt.Defaults `json:"builtin"`
	// Variables are what each {{name}} would be filled with now, with these settings.
	Variables map[string]string `json:"variables"`
}

// Reload is the result of reloading the config. NeedsRestart names settings that
// changed but only take effect at startup.
type Reload struct {
	NeedsRestart []string `json:"needs_restart"`
}

// Handler returns the whole site.
// Options are what the web side needs from the rest of tack.
type Options struct {
	Defaults func() Defaults
	Save     func(prompt.Defaults) error
	Reload   func() (Reload, error)
	// Hosts are the names the page can be reached by, besides localhost.
	Hosts []string
	// Variables are what {{name}} fills with, for a conversation with these settings.
	Variables func(store.Settings) map[string]string
	// Source is tack's source as this binary was built from it, at Revision, with the
	// model's documentation under ModelDocsDir. Docs is all the documentation.
	Source       fs.FS
	Revision     string
	ModelDocsDir string
	Docs         docs.Library
}

func Handler(c *core.Core, opts Options) http.Handler {
	hosts := opts.Hosts
	s := &server{core: c, defaults: opts.Defaults, save: opts.Save, reload: opts.Reload, variables: opts.Variables,
		source: opts.Source, revision: opts.Revision, modelDocsDir: opts.ModelDocsDir, docs: opts.Docs}
	if s.variables == nil {
		s.variables = func(store.Settings) map[string]string { return map[string]string{"date": prompt.Date(time.Now())} }
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", s.index)
	mux.Handle("GET /static/", http.FileServerFS(static))
	mux.HandleFunc("GET /api/conversations", s.list)
	mux.HandleFunc("POST /api/conversations", s.create)
	mux.HandleFunc("GET /api/conversations/{id}", s.get)
	mux.HandleFunc("GET /api/conversations/{id}/tools", s.definitions)
	mux.HandleFunc("POST /api/conversations/{id}/fork", s.current(s.fork))
	mux.HandleFunc("POST /api/conversations/{id}/messages", s.current(s.send))
	mux.HandleFunc("POST /api/conversations/{id}/held", s.current(s.hold))
	mux.HandleFunc("POST /api/conversations/{id}/reply", s.current(s.reply))
	mux.HandleFunc("POST /api/conversations/{id}/system/{message}/edits", s.current(s.proposeEdit))
	mux.HandleFunc("POST /api/conversations/{id}/edits/{edit}/apply", s.current(s.applyEdit))
	mux.HandleFunc("POST /api/conversations/{id}/edits/{edit}/discard", s.current(s.discardEdit))
	mux.HandleFunc("POST /api/conversations/{id}/message-edits", s.current(s.proposeMessageEdit))
	mux.HandleFunc("POST /api/conversations/{id}/message-edits/{edit}/apply", s.current(s.applyMessageEdit))
	mux.HandleFunc("POST /api/conversations/{id}/message-edits/{edit}/discard", s.current(s.discardMessageEdit))
	mux.HandleFunc("POST /api/conversations/{id}/refusals/forget", s.current(s.forgetRefusal))
	mux.HandleFunc("POST /api/conversations/{id}/decisions/{decision}", s.current(s.decide))
	mux.HandleFunc("POST /api/conversations/{id}/budget", s.current(s.addBudget))
	mux.HandleFunc("POST /api/conversations/{id}/pause", s.pause)
	mux.HandleFunc("POST /api/conversations/{id}/cancel", s.cancelReply)
	mux.HandleFunc("POST /api/conversations/{id}/tools/{call}/stop", s.stopTool)
	mux.HandleFunc("POST /api/conversations/{id}/settings", s.current(s.proposeSettings))
	mux.HandleFunc("POST /api/conversations/{id}/settings/{proposal}/apply", s.current(s.applySettings))
	mux.HandleFunc("POST /api/conversations/{id}/settings/{proposal}/discard", s.current(s.discardSettings))
	mux.HandleFunc("GET /requests", s.requests)
	mux.HandleFunc("GET /api/manual-calls", s.manualCalls)
	mux.HandleFunc("POST /api/manual-calls", s.current(s.sendManual))
	mux.HandleFunc("POST /api/manual-calls/count", s.countManual)
	mux.HandleFunc("GET /api/manual-calls/{id}", s.manualCall)
	mux.HandleFunc("POST /api/manual-calls/{id}/cancel", s.cancelManual)
	mux.HandleFunc("GET /api/recorded-calls", s.recordedCalls)
	mux.HandleFunc("GET /api/recorded-calls/{call}", s.recordedRequest)
	mux.HandleFunc("GET /api/defaults", s.newDefaults)
	mux.HandleFunc("POST /api/defaults", s.saveDefaults)
	mux.HandleFunc("POST /api/conversations/{id}/tools", s.current(s.proposeTools))
	mux.HandleFunc("POST /api/conversations/{id}/settings/now", s.changeSettingsNow)
	mux.HandleFunc("POST /api/conversations/{id}/code-calls/{call}", s.answerCodeCall)
	mux.HandleFunc("GET /code", s.codePage)
	mux.HandleFunc("GET /docs", s.docsPage)
	mux.HandleFunc("GET /api/source", s.sourceList)
	mux.HandleFunc("GET /api/source/file", s.sourceFile)
	mux.HandleFunc("GET /api/feedback", s.feedbackList)
	mux.HandleFunc("POST /api/feedback", s.addFeedback)
	mux.HandleFunc("POST /api/feedback/{id}/remove", s.removeFeedback)
	mux.HandleFunc("POST /api/feedback/read", s.readFeedback)
	mux.HandleFunc("GET /api/docs", s.docsList)
	mux.HandleFunc("GET /api/docs/page", s.docPage)
	mux.HandleFunc("POST /api/docs/save", s.saveDoc)
	mux.HandleFunc("POST /api/docs/remove", s.removeDoc)
	mux.HandleFunc("GET /api/status", s.status)
	mux.HandleFunc("POST /api/reload", s.reloadConfig)
	// Other websites open in the same browser shouldn't be able to post here.
	return knownHost(hosts, security(http.NewCrossOriginProtection().Handler(currentPage(mux))))
}

// currentPage refuses changes from a page that would get them wrong. Anything that
// doesn't say which page it is, like curl, isn't a page.
func currentPage(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		required := r.Header.Get(pageRequiredHeader)
		if r.Method == http.MethodPost && required != "" && required != strconv.Itoa(pageRequired) {
			http.Error(w, "This page is out of date, so that wasn't done. Reload it, and what you've typed will still be there.", http.StatusConflict)
			return
		}
		next.ServeHTTP(w, r)
	})
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

// current refuses an action that says which revision of the conversation it was based
// on, if that isn't the current one. Stopping what's running doesn't go through this: it
// has to work while the revision moves, and it names what it stops anyway.
//
// Actions take turns, so nothing can change between the check and the action. The body is
// read first, so a slow client can't hold everyone else up, and a call that streams lets
// go once it's started.
func (s *server) current(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxMessageBytes))
		if err != nil {
			http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
			return
		}
		r.Body = io.NopCloser(bytes.NewReader(body))
		s.actions.Lock()
		release := sync.OnceFunc(s.actions.Unlock)
		defer release()
		if text := r.Header.Get("X-Tack-Revision"); text != "" {
			revision, err := strconv.Atoi(text)
			if err != nil {
				http.Error(w, "bad revision", http.StatusBadRequest)
				return
			}
			if err := s.core.CheckRevision(r.PathValue("id"), revision); err != nil {
				fail(w, err)
				return
			}
		}
		next(w, r.WithContext(context.WithValue(r.Context(), actionKey{}, release)))
	}
}

// actionKey finds the function that ends an action's turn.
type actionKey struct{}

type server struct {
	actions  sync.Mutex
	core     *core.Core
	defaults func() Defaults
	save     func(prompt.Defaults) error
	reload   func() (Reload, error)
	// variables are what {{name}} fills with.
	variables func(store.Settings) map[string]string

	source       fs.FS
	revision     string
	modelDocsDir string
	docs         docs.Library
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

func (s *server) requests(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write(requestsPage())
}

func (s *server) manualCalls(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, s.core.ManualCalls())
}

func (s *server) manualCall(w http.ResponseWriter, r *http.Request) {
	d, err := s.core.ManualCall(r.PathValue("id"))
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, d)
}

// sendManual sends the body exactly as it arrived. The first update names the call.
func (s *server) sendManual(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
		return
	}
	_, updates, err := s.core.SendManual(body)
	if err != nil {
		fail(w, err)
		return
	}
	stream(w, r, updates)
}

func (s *server) countManual(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxMessageBytes))
	if err != nil {
		http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
		return
	}
	writeJSON(w, map[string]int{"tokens": core.CountManual(body)})
}

func (s *server) cancelManual(w http.ResponseWriter, r *http.Request) {
	if err := s.core.CancelManual(r.PathValue("id")); err != nil {
		fail(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *server) recordedCalls(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, s.core.RecordedCalls(500))
}

func (s *server) recordedRequest(w http.ResponseWriter, r *http.Request) {
	body, err := s.core.RecordedRequest(r.PathValue("call"))
	if err != nil {
		fail(w, err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.Write(body)
}

func (s *server) index(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write(indexPage())
}

func (s *server) list(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, s.core.Conversations())
}

// create starts a conversation from sections, which are stored one block each with the
// date filled in, and the tools to offer, in order. No tools means none at all.
func (s *server) create(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Sections []prompt.Section `json:"sections"`
		Tools    []core.Offer     `json:"tools"`
		Settings *store.Settings  `json:"settings"`
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxMessageBytes)
	if err := json.UnmarshalRead(r.Body, &body, json.RejectUnknownMembers(true)); err != nil {
		http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
		return
	}
	blocks := prompt.Blocks(body.Sections, s.variables(s.core.StartSettings(body.Settings)))
	offer := body.Tools
	if offer == nil {
		offer = []core.Offer{}
	}
	id, err := s.core.NewConversationWith(core.Options{
		System: strings.Join(blocks, ""), Blocks: blocks, Offer: offer, Settings: body.Settings,
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
	writeJSON(w, siteStatus{Status: s.core.CurrentStatus(), PageVersion: pageVersion(), PageRequired: pageRequired})
}

func (s *server) newDefaults(w http.ResponseWriter, _ *http.Request) {
	defaults := s.defaults()
	defaults.Builtin = prompt.Builtin()
	defaults.Variables = s.variables(s.core.StartSettings(nil))
	writeJSON(w, defaults)
}

// saveDefaults replaces what new conversations start with. The tools are only checked
// when a conversation starts with them, since tack's own can change in between.
func (s *server) saveDefaults(w http.ResponseWriter, r *http.Request) {
	var body prompt.Defaults
	r.Body = http.MaxBytesReader(w, r.Body, maxMessageBytes)
	if err := json.UnmarshalRead(r.Body, &body, json.RejectUnknownMembers(true)); err != nil {
		http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
		return
	}
	if body.Tools == nil {
		body.Tools = []prompt.Tool{}
	}
	if err := s.save(body); err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, map[string]bool{"ok": true})
}

func (s *server) proposeTools(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Tools []core.Offer `json:"tools"`
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxMessageBytes)
	if err := json.UnmarshalRead(r.Body, &body, json.RejectUnknownMembers(true)); err != nil {
		http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
		return
	}
	id, err := s.core.ProposeTools(r.PathValue("id"), body.Tools)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, map[string]string{"proposal": id})
}

// answerCodeCall answers running code's call to a custom tool. It isn't tied to a revision,
// since the conversation is busy running that code.
func (s *server) answerCodeCall(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Text string `json:"text"`
		Deny bool   `json:"deny"`
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxMessageBytes)
	if err := json.UnmarshalRead(r.Body, &body, json.RejectUnknownMembers(true)); err != nil {
		http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
		return
	}
	if err := s.core.AnswerCodeCall(r.PathValue("id"), r.PathValue("call"), body.Text, body.Deny); err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, map[string]bool{"ok": true})
}

// changeSettingsNow changes the settings that can change at any point, even while a
// reply is running, so it isn't tied to a revision.
func (s *server) changeSettingsNow(w http.ResponseWriter, r *http.Request) {
	var body struct {
		AutoRunCode     *bool   `json:"auto_run_code"`
		Host            *string `json:"host"`
		ReasoningEffort *string `json:"reasoning_effort"`
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxMessageBytes)
	if err := json.UnmarshalRead(r.Body, &body, json.RejectUnknownMembers(true)); err != nil {
		http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
		return
	}
	err := s.core.ChangeSettingsNow(r.PathValue("id"), func(settings *store.Settings) {
		if body.AutoRunCode != nil {
			settings.AutoRunCode = *body.AutoRunCode
		}
		if body.Host != nil {
			settings.Host = *body.Host
		}
		if body.ReasoningEffort != nil {
			settings.ReasoningEffort = *body.ReasoningEffort
		}
	})
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, map[string]bool{"ok": true})
}

func readMessage(w http.ResponseWriter, r *http.Request) (core.Message, bool) {
	var message core.Message
	r.Body = http.MaxBytesReader(w, r.Body, maxMessageBytes)
	if err := json.UnmarshalRead(r.Body, &message); err != nil {
		http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
		return message, false
	}
	return message, true
}

func (s *server) send(w http.ResponseWriter, r *http.Request) {
	message, ok := readMessage(w, r)
	if !ok {
		return
	}
	updates, err := s.core.SendMessage(r.PathValue("id"), message)
	if err != nil {
		fail(w, err)
		return
	}
	stream(w, r, updates)
}

func (s *server) hold(w http.ResponseWriter, r *http.Request) {
	message, ok := readMessage(w, r)
	if !ok {
		return
	}
	if err := s.core.AddMessage(r.PathValue("id"), message); err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, map[string]bool{"ok": true})
}

func (s *server) proposeMessageEdit(w http.ResponseWriter, r *http.Request) {
	var body core.MessageEdit
	r.Body = http.MaxBytesReader(w, r.Body, maxMessageBytes)
	if err := json.UnmarshalRead(r.Body, &body); err != nil {
		http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
		return
	}
	id, err := s.core.ProposeMessageEdit(r.PathValue("id"), body)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, map[string]string{"edit": id})
}

func (s *server) applyMessageEdit(w http.ResponseWriter, r *http.Request) {
	if err := s.core.ApplyMessageEdit(r.PathValue("id"), r.PathValue("edit")); err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, map[string]bool{"ok": true})
}

func (s *server) discardMessageEdit(w http.ResponseWriter, r *http.Request) {
	if err := s.core.DiscardMessageEdit(r.PathValue("id"), r.PathValue("edit")); err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, map[string]bool{"ok": true})
}

func (s *server) forgetRefusal(w http.ResponseWriter, r *http.Request) {
	feature, ok := readText(w, r, "feature")
	if !ok {
		return
	}
	if err := s.core.ForgetRefusal(r.PathValue("id"), feature); err != nil {
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
		Amount core.Pico `json:"amount_pico"`
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxMessageBytes)
	if err := json.UnmarshalRead(r.Body, &body); err != nil {
		http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
		return
	}
	if err := s.core.AllowBudget(r.PathValue("id"), body.Amount); err != nil {
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
		Allow      []string          `json:"allow"`
		Shorten    []string          `json:"shorten"`
		AutoRun    bool              `json:"auto_run_code"`
		DenialText string            `json:"denial_text"`
		Results    map[string]string `json:"results"`
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxMessageBytes)
	if err := json.UnmarshalRead(r.Body, &body); err != nil {
		http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
		return
	}
	updates, err := s.core.DecideWith(r.PathValue("id"), r.PathValue("decision"), core.Decision{Allow: body.Allow, Shorten: body.Shorten, Text: body.DenialText, AutoRunCode: body.AutoRun, Results: body.Results})
	if err != nil {
		fail(w, err)
		return
	}
	stream(w, r, updates)
}

// stream sends updates as one JSON object per line. If the browser goes away
// the call carries on: it belongs to the core, not to this request.
func stream(w http.ResponseWriter, r *http.Request, updates <-chan core.Update) {
	if release, ok := r.Context().Value(actionKey{}).(func()); ok {
		release()
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
	case errors.Is(err, core.ErrNotFound), errors.Is(err, core.ErrNoMessage), errors.Is(err, core.ErrNoEdit), errors.Is(err, core.ErrNoDecision):
		http.Error(w, err.Error(), http.StatusNotFound)
	case errors.Is(err, core.ErrBusy), errors.Is(err, core.ErrNothingToSend), errors.Is(err, core.ErrAwaiting), errors.Is(err, core.ErrNothingRunning):
		http.Error(w, err.Error(), http.StatusConflict)
	case errors.Is(err, core.ErrBadChoice), errors.Is(err, core.ErrBadSettings), errors.Is(err, core.ErrBadTools), errors.Is(err, core.ErrBadFork), errors.Is(err, core.ErrBadMessage),
		errors.Is(err, core.ErrBadRequest):
		http.Error(w, err.Error(), http.StatusBadRequest)
	case errors.Is(err, core.ErrStale):
		w.Header().Set("X-Tack-Stale", "1")
		http.Error(w, err.Error(), http.StatusConflict)
	case errors.Is(err, core.ErrRestarting):
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
	default:
		log.Printf("request failed: %v", err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}
