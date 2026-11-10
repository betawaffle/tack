package web

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"io"
	"io/fs"
	"net/http"
	"path"
	"slices"
	"strconv"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/betawaffle/tack/internal/core"
	"github.com/betawaffle/tack/internal/docs"
	"github.com/betawaffle/tack/internal/prompt"
	"github.com/betawaffle/tack/internal/store"
	"github.com/betawaffle/tack/internal/tokens"
)

// The codebase and documentation pages show tack's source as this binary was built from it,
// and take comments on it, which are read later as a batch. The documentation page also
// edits the user's own documentation.

// SourceFile is a file in the source, for the page's list.
type SourceFile struct {
	Path string `json:"path"`
	Size int    `json:"size"`
}

func (s *server) sourceFiles() ([]SourceFile, error) {
	files := []SourceFile{}
	if s.source == nil {
		return files, nil
	}
	err := fs.WalkDir(s.source, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		files = append(files, SourceFile{Path: p, Size: int(info.Size())})
		return nil
	})
	return files, err
}

func (s *server) codePage(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write(codePage())
}

func (s *server) docsPage(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write(docsPage())
}

var codePage = sync.OnceValue(func() []byte { return page("static/code.html") })
var docsPage = sync.OnceValue(func() []byte { return page("static/docs.html") })
var promptPage = sync.OnceValue(func() []byte { return page("static/prompt.html") })

func page(name string) []byte {
	data, err := static.ReadFile(name)
	if err != nil {
		panic(err)
	}
	return []byte(strings.NewReplacer("{{page_version}}", pageVersion(), "{{page_required}}", strconv.Itoa(pageRequired)).Replace(string(data)))
}

// source lists the source. The model's documentation is shown on its own page, and only
// linked to from here.
func (s *server) sourceList(w http.ResponseWriter, _ *http.Request) {
	files, err := s.sourceFiles()
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, map[string]any{"revision": s.revision, "files": files, "docs_dir": s.modelDocsDir})
}

// sourceFile is one file's text. A file that isn't text says so instead.
func (s *server) sourceFile(w http.ResponseWriter, r *http.Request) {
	p := r.URL.Query().Get("path")
	if s.source == nil || !fs.ValidPath(p) {
		http.Error(w, "no such file", http.StatusNotFound)
		return
	}
	data, err := fs.ReadFile(s.source, p)
	if err != nil {
		http.Error(w, "no such file", http.StatusNotFound)
		return
	}
	binary := !utf8.Valid(data) || slices.Contains(data, 0)
	out := map[string]any{"revision": s.revision, "path": p, "size": len(data), "binary": binary}
	if !binary {
		out["text"] = string(data)
	}
	writeJSON(w, out)
}

func (s *server) feedbackList(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, map[string]any{"revision": s.revision, "feedback": s.core.Feedback()})
}

func (s *server) addFeedback(w http.ResponseWriter, r *http.Request) {
	var body store.Feedback
	r.Body = http.MaxBytesReader(w, r.Body, 4*core.MaxFeedbackBytes)
	if err := json.UnmarshalRead(r.Body, &body, json.RejectUnknownMembers(true)); err != nil {
		http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
		return
	}
	if body.ID != "" {
		http.Error(w, "bad request: tack gives a comment its ID", http.StatusBadRequest)
		return
	}
	if body.Revision == "" {
		body.Revision = s.revision
	}
	id, err := s.core.AddFeedback(body)
	if err != nil {
		failFeedback(w, err)
		return
	}
	writeJSON(w, map[string]string{"id": id})
}

func (s *server) removeFeedback(w http.ResponseWriter, r *http.Request) {
	if err := s.core.RemoveFeedback(r.PathValue("id")); err != nil {
		failFeedback(w, err)
		return
	}
	writeJSON(w, map[string]bool{"ok": true})
}

// readFeedback marks comments read. It's for whoever reads them, not the page.
func (s *server) readFeedback(w http.ResponseWriter, r *http.Request) {
	var body struct {
		IDs []string `json:"ids"`
		By  string   `json:"by"`
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxMessageBytes)
	if err := json.UnmarshalRead(r.Body, &body, json.RejectUnknownMembers(true)); err != nil || body.By == "" {
		http.Error(w, `bad request: it takes {"ids": [...], "by": who}`, http.StatusBadRequest)
		return
	}
	if err := s.core.MarkFeedbackRead(body.IDs, body.By); err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, map[string]bool{"ok": true})
}

func failFeedback(w http.ResponseWriter, err error) {
	if errors.Is(err, core.ErrBadFeedback) {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	fail(w, err)
}

// DocPage is one page in the list.
type DocPage struct {
	Tree string `json:"tree"`
	Path string `json:"path"`
}

func (s *server) docsList(w http.ResponseWriter, _ *http.Request) {
	pages, err := s.docs.Pages()
	if err != nil {
		fail(w, err)
		return
	}
	out := []DocPage{}
	for _, p := range pages {
		out = append(out, DocPage{Tree: p.Tree, Path: p.Path})
	}
	writeJSON(w, map[string]any{"revision": s.revision, "pages": out, "source_dir": s.modelDocsDir})
}

// docPage is a page as it's written, with where each of its links goes, and as the model
// reads it.
func (s *server) docPage(w http.ResponseWriter, r *http.Request) {
	tree, p := r.URL.Query().Get("tree"), r.URL.Query().Get("path")
	pages, err := s.docs.Pages()
	if err != nil {
		fail(w, err)
		return
	}
	i := slices.IndexFunc(pages, func(x docs.Page) bool { return x.Tree == tree && x.Path == p })
	if i < 0 {
		http.Error(w, "no such page", http.StatusNotFound)
		return
	}
	page := pages[i]
	variables := s.variables(s.core.StartSettings(nil))
	type link struct {
		Target string   `json:"target"`
		To     *DocPage `json:"to,omitzero"`
	}
	links := []link{}
	for _, l := range docs.Links(page.Text) {
		out := link{Target: l.Target}
		if to, ok := docs.Resolve(pages, page, l.Target); ok {
			out.To = &DocPage{Tree: to.Tree, Path: to.Path}
		}
		links = append(links, out)
	}
	out := map[string]any{
		"revision": s.revision, "tree": page.Tree, "path": page.Path, "text": page.Text, "links": links,
		"for_model": docs.ForModel(pages, page, variables),
		// For reading: the variables filled, and the links as they're written.
		"filled": prompt.Fill(page.Text, variables),
	}
	if page.Tree == docs.Harness {
		out["source"] = path.Join(s.modelDocsDir, page.Path)
	}
	writeJSON(w, out)
}

// saveDoc writes one of the user's pages. With From, it's a rename: From is removed once
// the page is saved under its new path.
func (s *server) saveDoc(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Path string `json:"path"`
		Text string `json:"text"`
		From string `json:"from"`
	}
	r.Body = http.MaxBytesReader(w, r.Body, 2*docs.MaxPageBytes)
	if err := json.UnmarshalRead(r.Body, &body, json.RejectUnknownMembers(true)); err != nil {
		http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
		return
	}
	if err := s.docs.Save(body.Path, body.Text); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if body.From != "" && body.From != body.Path {
		if err := s.docs.Remove(body.From); err != nil {
			http.Error(w, "it's saved as "+body.Path+", but "+body.From+" couldn't be removed: "+err.Error(), http.StatusInternalServerError)
			return
		}
	}
	writeJSON(w, map[string]bool{"ok": true})
}

func (s *server) removeDoc(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Path string `json:"path"`
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxMessageBytes)
	if err := json.UnmarshalRead(r.Body, &body, json.RejectUnknownMembers(true)); err != nil {
		http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
		return
	}
	if err := s.docs.Remove(body.Path); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	writeJSON(w, map[string]bool{"ok": true})
}

func (s *server) promptPage(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write(promptPage())
}

// callPrompt is a call as the model saw it. Only the body is shown, never the headers.
func (s *server) callPrompt(w http.ResponseWriter, r *http.Request) {
	p, err := s.core.Prompt(r.PathValue("id"), r.PathValue("call"))
	if errors.Is(err, tokens.ErrNoRenderer) {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, p)
}

// maxPreviewBytes is the most of a request body that can be previewed: a context window's
// worth, with room to spare.
const maxPreviewBytes = 32 << 20

// previewPrompt shows any request body as the model would read it, without sending it, and
// with before, compared with that request and its reply. It's for drafting the next call, and
// for seeing how the page copes with a big prompt.
func (s *server) previewPrompt(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Model   string            `json:"model"`
		Request jsontext.Value    `json:"request"`
		Before  *core.CallRequest `json:"before"`
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxPreviewBytes)
	if err := json.UnmarshalRead(r.Body, &body, json.RejectUnknownMembers(true)); err != nil {
		http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
		return
	}
	p, err := core.Preview(body.Model, body.Request, body.Before)
	if errors.Is(err, tokens.ErrNoRenderer) {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	if err != nil {
		http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
		return
	}
	writeJSON(w, p)
}

func (s *server) manualPrompt(w http.ResponseWriter, r *http.Request) {
	p, err := s.core.ManualPrompt(r.PathValue("id"))
	if errors.Is(err, tokens.ErrNoRenderer) {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, p)
}

// estimate is what a request body would cost to send.
func (s *server) estimate(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxPreviewBytes))
	if err != nil {
		http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
		return
	}
	e, err := s.core.EstimateRequest(r.Context(), body)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, e)
}

// draftResponse is a draft and its prompt as the model would read it. A draft for a model whose
// format tack doesn't know has no prompt, and Problem says so, so the model can be changed.
type draftResponse struct {
	Draft   *store.Draft     `json:"draft,omitzero"`
	Prompt  *core.CallPrompt `json:"prompt"`
	Problem string           `json:"problem,omitzero"`
}

// writeDraft writes a draft and its prompt, or why there's no prompt.
func writeDraft(w http.ResponseWriter, d *store.Draft, p core.CallPrompt, err error) {
	if errors.Is(err, tokens.ErrNoRenderer) {
		writeJSON(w, draftResponse{Draft: d, Problem: err.Error()})
		return
	}
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, draftResponse{Draft: d, Prompt: &p})
}

// newDraft starts a draft of the call after another, or opens the one that's already started.
func (s *server) newDraft(w http.ResponseWriter, r *http.Request) {
	var body struct {
		From string `json:"from"`
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxMessageBytes)
	if err := json.UnmarshalRead(r.Body, &body, json.RejectUnknownMembers(true)); err != nil {
		http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
		return
	}
	var d *store.Draft
	var err error
	if body.From != "" {
		d, err = s.core.NewDraft(body.From)
	} else {
		d, err = s.blankDraft()
	}
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, d)
}

// blankDraft starts a draft from what new conversations start with: the saved prompt, with its
// variables filled, the saved tools, and the default settings.
func (s *server) blankDraft() (*store.Draft, error) {
	defaults := s.defaults()
	settings := s.core.StartSettings(nil)
	blocks := prompt.Blocks(defaults.Prompt.Sections, s.variables(settings))
	return s.core.NewBlankDraft(strings.Join(blocks, ""), offersFrom(defaults.Prompt.Tools, defaults.Tools), nil)
}

// offersFrom is the tools saved as defaults, as offers: every built-in one if none were saved,
// and otherwise the saved ones that are on, leaving out any tack doesn't have any more.
func offersFrom(saved []prompt.Tool, builtins []core.ToolInfo) []core.Offer {
	offers := []core.Offer{}
	if saved == nil {
		for _, b := range builtins {
			offers = append(offers, core.Offer{Name: b.Name})
		}
		return offers
	}
	for _, t := range saved {
		if t.Off {
			continue
		}
		if !t.Custom && !slices.ContainsFunc(builtins, func(b core.ToolInfo) bool { return b.Name == t.Name }) {
			continue
		}
		offers = append(offers, core.Offer{Name: t.Name, Custom: t.Custom, Definition: t.Definition})
	}
	return offers
}

// drafts lists the drafts that haven't been sent.
func (s *server) drafts(w http.ResponseWriter, _ *http.Request) {
	list, err := s.core.Drafts()
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, list)
}

func (s *server) getDraft(w http.ResponseWriter, r *http.Request) {
	d, p, err := s.core.Draft(r.PathValue("id"))
	if d == nil {
		fail(w, err)
		return
	}
	writeDraft(w, d, p, err)
}

// saveDraft replaces a draft's request, and answers with its prompt, as getDraft does.
func (s *server) saveDraft(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Request jsontext.Value `json:"request"`
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxPreviewBytes)
	if err := json.UnmarshalRead(r.Body, &body, json.RejectUnknownMembers(true)); err != nil {
		http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
		return
	}
	p, err := s.core.SaveDraft(r.PathValue("id"), body.Request)
	writeDraft(w, nil, p, err)
}

// sendDraft sends a draft as a manual call. The first update names the call.
func (s *server) sendDraft(w http.ResponseWriter, r *http.Request) {
	_, updates, err := s.core.SendDraft(r.PathValue("id"))
	if err != nil {
		fail(w, err)
		return
	}
	stream(w, r, updates)
}

// setDraftAgent sets the agent a draft's tools run as.
func (s *server) setDraftAgent(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Agent string `json:"agent"`
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxMessageBytes)
	if err := json.UnmarshalRead(r.Body, &body, json.RejectUnknownMembers(true)); err != nil {
		http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
		return
	}
	if err := s.core.SetDraftAgent(r.PathValue("id"), body.Agent); err != nil {
		fail(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *server) agents(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, s.core.Agents())
}

// newAgent makes an agent, empty or as a copy of another's state.
func (s *server) newAgent(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Slug string `json:"slug"`
		From string `json:"from"`
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxMessageBytes)
	if err := json.UnmarshalRead(r.Body, &body, json.RejectUnknownMembers(true)); err != nil {
		http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
		return
	}
	a, err := s.core.NewAgent(body.Slug, body.From)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, a)
}

func (s *server) renameAgent(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Slug string `json:"slug"`
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxMessageBytes)
	if err := json.UnmarshalRead(r.Body, &body, json.RejectUnknownMembers(true)); err != nil {
		http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
		return
	}
	if err := s.core.RenameAgent(r.PathValue("id"), body.Slug); err != nil {
		fail(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
