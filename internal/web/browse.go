package web

import (
	"encoding/json/v2"
	"errors"
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
