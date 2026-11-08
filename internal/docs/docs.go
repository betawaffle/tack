// Package docs is documentation a model can read when it needs to, rather than having it
// all in its prompt. There are two trees of Markdown pages: tack's, which is part of its
// code and changes along with it, and the user's, in the config directory, which they edit
// from the page. Pages can use {{name}} variables, like prompts, and [[Page]] links, which
// become relative Markdown links in what the model reads.
package docs

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/betawaffle/tack/internal/prompt"
)

// The trees, by the name of their directory in what the model reads.
const (
	Tack = "tack"
	User = "user"
)

// Limits on the user's tree, which is read for every run of code.
const (
	MaxPageBytes  = 1 << 20
	MaxTotalBytes = 16 << 20
	MaxPages      = 1000
)

// Library is both trees. Tack's is fixed for a build. The user's is read each time.
type Library struct {
	TackFS  fs.FS
	UserDir string
}

// Page is one page, by its tree and its path in it, which ends in .md.
type Page struct {
	Tree string `json:"tree"`
	Path string `json:"path"`
	Text string `json:"text"`
}

// Name is how [[Page]] links name a page: its path without .md.
func (p Page) Name() string { return strings.TrimSuffix(p.Path, ".md") }

// CheckPath says whether path can be a page: relative, with no empty, dot or hidden parts,
// ending in .md.
func CheckPath(p string) error {
	if !strings.HasSuffix(p, ".md") || len(p) <= len(".md") {
		return errors.New("a page's path ends in .md")
	}
	if !fs.ValidPath(p) || strings.Contains(p, "\\") {
		return errors.New("a page's path is relative, with / between its parts")
	}
	for _, part := range strings.Split(p, "/") {
		if strings.HasPrefix(part, ".") {
			return errors.New("no part of a page's path can start with a dot")
		}
	}
	return nil
}

// Pages returns every page in both trees, tack's first, each in path order. Only regular
// files are read from the user's tree, so a link there can't bring in anything else.
func (l Library) Pages() ([]Page, error) {
	var out []Page
	if l.TackFS != nil {
		err := fs.WalkDir(l.TackFS, ".", func(p string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() || CheckPath(p) != nil {
				return err
			}
			data, err := fs.ReadFile(l.TackFS, p)
			if err != nil {
				return err
			}
			out = append(out, Page{Tree: Tack, Path: p, Text: string(data)})
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	user, err := l.userPages()
	if err != nil {
		return nil, err
	}
	return append(out, user...), nil
}

func (l Library) userPages() ([]Page, error) {
	if l.UserDir == "" {
		return nil, nil
	}
	var out []Page
	total := 0
	err := filepath.WalkDir(l.UserDir, func(full string, d fs.DirEntry, err error) error {
		if errors.Is(err, fs.ErrNotExist) && full == l.UserDir {
			return fs.SkipAll
		}
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(l.UserDir, full)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if d.IsDir() {
			if rel != "." && strings.HasPrefix(d.Name(), ".") {
				return fs.SkipDir
			}
			return nil
		}
		if !d.Type().IsRegular() || CheckPath(rel) != nil {
			return nil
		}
		data, err := os.ReadFile(full)
		if err != nil {
			return err
		}
		if len(data) > MaxPageBytes {
			return fmt.Errorf("%s is over %d bytes", rel, MaxPageBytes)
		}
		if total += len(data); total > MaxTotalBytes || len(out) >= MaxPages {
			return fmt.Errorf("the user's documentation is over %d bytes or %d pages", MaxTotalBytes, MaxPages)
		}
		out = append(out, Page{Tree: User, Path: rel, Text: string(data)})
		return nil
	})
	return out, err
}

// Save writes one of the user's pages, replacing it all at once.
func (l Library) Save(p string, text string) error {
	if err := CheckPath(p); err != nil {
		return err
	}
	if len(text) > MaxPageBytes {
		return fmt.Errorf("a page can be at most %d bytes", MaxPageBytes)
	}
	full := filepath.Join(l.UserDir, filepath.FromSlash(p))
	if err := os.MkdirAll(filepath.Dir(full), 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(full), ".page-*.md")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.WriteString(text); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), full)
}

// Remove deletes one of the user's pages, and any directories that leaves empty.
func (l Library) Remove(p string) error {
	if err := CheckPath(p); err != nil {
		return err
	}
	full := filepath.Join(l.UserDir, filepath.FromSlash(p))
	info, err := os.Lstat(full)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return errors.New("that isn't a page")
	}
	if err := os.Remove(full); err != nil {
		return err
	}
	for dir := filepath.Dir(full); dir != l.UserDir && within(dir, l.UserDir); dir = filepath.Dir(dir) {
		if os.Remove(dir) != nil {
			break
		}
	}
	return nil
}

func within(p, dir string) bool {
	rel, err := filepath.Rel(dir, p)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, "../")
}

// Link is a [[Page]] or [[Page|label]] link where it was found.
type Link struct {
	Start, End int // in the page's text
	Target     string
	Label      string
}

// Links finds the [[...]] links in text. A link is everything up to the next ]], and can't
// span lines.
func Links(text string) []Link {
	var out []Link
	at := 0
	for {
		open := strings.Index(text[at:], "[[")
		if open < 0 {
			return out
		}
		open += at
		end := strings.Index(text[open+2:], "]]")
		if end < 0 {
			return out
		}
		end += open + 2
		inside := text[open+2 : end]
		if inside == "" || strings.ContainsAny(inside, "\n[]") {
			at = open + 2
			continue
		}
		target, label, _ := strings.Cut(inside, "|")
		out = append(out, Link{Start: open, End: end + 2, Target: target, Label: label})
		at = end + 2
	}
}

// Resolve finds the page a link names, by its name compared whole: in the same tree
// first, then the other. It returns false if there's none.
func Resolve(pages []Page, from Page, target string) (Page, bool) {
	for _, sameTree := range []bool{true, false} {
		for _, p := range pages {
			if (p.Tree == from.Tree) == sameTree && p.Name() == target {
				return p, true
			}
		}
	}
	return Page{}, false
}

// relative is the path from one page to another, as both are laid out for the model.
func relative(from, to Page) string {
	rel, err := filepath.Rel(path.Dir(path.Join(from.Tree, from.Path)), path.Join(to.Tree, to.Path))
	if err != nil {
		return path.Join("/", to.Tree, to.Path)
	}
	return filepath.ToSlash(rel)
}

// ForModel is a page as the model reads it: variables filled, then links made relative.
// A link to no page is left as it is. Labels and names go in as written; a name with
// characters Markdown treats specially gets its destination in angle brackets.
func ForModel(pages []Page, p Page, variables map[string]string) string {
	text := prompt.Fill(p.Text, variables)
	var b strings.Builder
	at := 0
	for _, link := range Links(text) {
		to, ok := Resolve(pages, p, link.Target)
		if !ok {
			continue
		}
		label := link.Label
		if label == "" {
			label = link.Target
		}
		dest := relative(p, to)
		if strings.ContainsAny(dest, " ()<>") {
			dest = "<" + dest + ">"
		}
		b.WriteString(text[at:link.Start])
		fmt.Fprintf(&b, "[%s](%s)", label, dest)
		at = link.End
	}
	b.WriteString(text[at:])
	return b.String()
}

// Index is the generated index.md, listing every page by tree.
func Index(pages []Page) string {
	var b strings.Builder
	b.WriteString("# Documentation\n")
	for _, tree := range []struct{ name, about string }{
		{Tack, "tack's, which describes the harness and changes along with it"},
		{User, "the user's own"},
	} {
		fmt.Fprintf(&b, "\n## %s\n\nThese pages are %s.\n\n", tree.name, tree.about)
		found := false
		for _, p := range pages {
			if p.Tree == tree.name {
				fmt.Fprintf(&b, "- [%s](%s/%s)\n", p.Name(), p.Tree, p.Path)
				found = true
			}
		}
		if !found {
			b.WriteString("There are none yet.\n")
		}
	}
	return b.String()
}

// WriteFor lays out every page as the model reads it under dir, which must be empty, with
// the index at the top.
func (l Library) WriteFor(dir string, variables map[string]string) error {
	pages, err := l.Pages()
	if err != nil {
		return err
	}
	for _, tree := range []string{Tack, User} {
		if err := os.MkdirAll(filepath.Join(dir, tree), 0o755); err != nil {
			return err
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "index.md"), []byte(Index(pages)), 0o644); err != nil {
		return err
	}
	for _, p := range pages {
		full := filepath.Join(dir, p.Tree, filepath.FromSlash(p.Path))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(full, []byte(ForModel(pages, p, variables)), 0o644); err != nil {
			return err
		}
	}
	return nil
}
