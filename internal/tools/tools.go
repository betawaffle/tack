// Package tools has the tools models can ask for. Everything here runs only after a
// person approves it, and its output then waits for a second approval. The checks
// below exist to draw attention and to catch mistakes. They aren't a sandbox.
//
// Name and Definition are what the model sees, and Run is what happens, so a
// different model can later get a different interface to the same code.
package tools

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/betawaffle/tack/internal/core"
	"github.com/betawaffle/tack/internal/store"
)

// Limits on what one call can produce or read. The full output goes in a blob and
// to a person, so these protect memory rather than the model's context.
const (
	maxRead       = 16 << 20
	maxListed     = 10000
	maxMatches    = 1000
	maxSearchFile = 2 << 20
	maxLineShown  = 500
)

// Flags shown on an approval card. They say "look here". The absence of a flag
// says nothing about safety.
const (
	FlagOutside = "Outside the working directory"
	FlagPrivate = "Not world-readable"
)

// FlagCommand is on every run_command. It never goes away, and such a call is never
// a candidate for approving automatically.
const FlagCommand = "Runs any command"

// Options configure the tools.
type Options struct {
	// Protected files are refused outright, wherever a path to them comes from.
	// They're compared by identity, which also catches links.
	Protected []string
}

// Read returns the read-only tools.
func Read(opts Options) []core.Tool {
	r := &reader{protected: opts.Protected}
	return []core.Tool{
		{
			Name:       "read_file",
			Definition: definition("read_file", "Read a text file.", `{"path":{"type":"string","description":"Path to the file."}}`, `["path"]`),
			Inspect:    r.inspectFile,
			Run:        r.readFile,
		},
		{
			Name:       "list_files",
			Definition: definition("list_files", "List a directory. Directories end in a slash.", `{"path":{"type":"string","description":"Path to the directory. Defaults to the working directory."}}`, `[]`),
			Inspect:    r.inspectDir,
			Run:        r.listFiles,
		},
		{
			Name: "search_files",
			Define: func(settings store.Settings) jsontext.Value {
				skipping := "Skips files that are binary, very large or not world-readable."
				if len(settings.SearchSkip) > 0 {
					skipping = "Skips " + strings.Join(settings.SearchSkip, ", ") + ", and files that are binary, very large or not world-readable."
				}
				return definition("search_files", "Search text files under a path with a regular expression, as path:line:text. "+skipping, `{"pattern":{"type":"string","description":"An RE2 regular expression."},"path":{"type":"string","description":"File or directory to search. Defaults to the working directory."}}`, `["pattern"]`)
			},
			Inspect: r.inspectSearch,
			Run:     r.searchFiles,
		},
	}
}

// All returns every tool.
func All(opts Options) []core.Tool {
	r := &reader{protected: opts.Protected}
	return append(Read(opts),
		core.Tool{
			Name:       "edit_file",
			Definition: definition("edit_file", "Change a file by replacing old_string with new_string. old_string has to appear exactly once, so include enough context to make it unique. An empty old_string creates a new file, and fails if the file exists.", `{"path":{"type":"string","description":"Path to the file."},"old_string":{"type":"string","description":"The exact text to replace."},"new_string":{"type":"string","description":"The text to put in its place."}}`, `["path","old_string","new_string"]`),
			Inspect:    r.inspectEdit,
			Run:        r.editFile,
		},
		core.Tool{
			Name:       "run_command",
			Definition: definition("run_command", "Run a command with bash in the working directory. It has a limited environment and no input.", `{"command":{"type":"string","description":"The command to run."}}`, `["command"]`),
			Inspect:    inspectCommand,
			Run:        runCommand,
			Timeout: func(settings store.Settings) time.Duration {
				return time.Duration(settings.CommandTimeoutSeconds) * time.Second
			},
		},
	)
}

func definition(name, description, properties, required string) jsontext.Value {
	return jsontext.Value(fmt.Sprintf(`{"type":"function","function":{"name":%q,"description":%q,"parameters":{"type":"object","properties":%s,"required":%s,"additionalProperties":false}}}`,
		name, description, properties, required))
}

type reader struct {
	protected []string
}

// arguments reads a call's strings, refusing anything it doesn't expect.
func arguments(call core.Call, allowed ...string) (map[string]string, error) {
	out := map[string]string{}
	for name, value := range call.Args {
		if !slices.Contains(allowed, name) {
			return nil, fmt.Errorf("unexpected argument %q", name)
		}
		var text string
		if err := json.Unmarshal(value, &text); err != nil {
			return nil, fmt.Errorf("argument %q has to be a string", name)
		}
		out[name] = text
	}
	return out, nil
}

// location makes a path absolute. An empty path means the working directory only
// when fallback is set.
func location(path, workdir string, fallback bool) (string, error) {
	if strings.ContainsRune(path, 0) {
		return "", errors.New("the path has a NUL in it")
	}
	if path == "" {
		if !fallback {
			return "", errors.New("the path is empty")
		}
		path = "."
	}
	if filepath.IsAbs(path) {
		return filepath.Clean(path), nil
	}
	// Never relative to tack's own working directory.
	if !filepath.IsAbs(workdir) {
		return "", errors.New("this conversation has no working directory, so the path has to be absolute")
	}
	return filepath.Join(workdir, path), nil
}

func (r *reader) isProtected(info fs.FileInfo) bool {
	for _, path := range r.protected {
		if other, err := os.Stat(path); err == nil && os.SameFile(info, other) {
			return true
		}
	}
	return false
}

// resolveExisting follows symlinks as far as the path exists, so that a new file
// under a symlinked directory still counts as being where it will really end up.
func resolveExisting(path string) string {
	var rest []string
	for at := path; ; {
		if real, err := filepath.EvalSymlinks(at); err == nil {
			return filepath.Join(append([]string{real}, rest...)...)
		}
		parent := filepath.Dir(at)
		if parent == at {
			return path
		}
		rest = append([]string{filepath.Base(at)}, rest...)
		at = parent
	}
}

// flags runs before anyone has approved anything, on a path the model chose. So it
// only ever looks at metadata and never opens the file. An error means the call
// would be refused anyway, so it isn't offered for approval. Results go to the
// person first: the model only hears about an error along with the decision.
func (r *reader) flags(path, workdir string, wantRegular bool) ([]string, error) {
	var out []string
	real := resolveExisting(path)
	if workdir != "" {
		root, err := filepath.EvalSymlinks(workdir)
		if err != nil {
			root = workdir
		}
		if rel, err := filepath.Rel(root, real); err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			out = append(out, FlagOutside)
		}
	}
	info, err := os.Stat(real)
	if err != nil {
		return out, nil // the tool reports this if it's approved
	}
	if r.isProtected(info) {
		return nil, fmt.Errorf("%s is protected, so tack won't read it", path)
	}
	if wantRegular && !info.Mode().IsRegular() {
		return nil, fmt.Errorf("%s isn't a regular file", path)
	}
	if info.Mode().Perm()&0o004 == 0 {
		out = append(out, FlagPrivate)
	}
	return out, nil
}

func (r *reader) inspectFile(call core.Call) ([]string, error) {
	args, err := arguments(call, "path")
	if err != nil {
		return nil, err
	}
	path, err := location(args["path"], call.Workdir, false)
	if err != nil {
		return nil, err
	}
	return r.flags(path, call.Workdir, true)
}

func (r *reader) inspectDir(call core.Call) ([]string, error) {
	args, err := arguments(call, "path")
	if err != nil {
		return nil, err
	}
	path, err := location(args["path"], call.Workdir, true)
	if err != nil {
		return nil, err
	}
	return r.flags(path, call.Workdir, false)
}

func (r *reader) inspectSearch(call core.Call) ([]string, error) {
	args, err := arguments(call, "pattern", "path")
	if err != nil {
		return nil, err
	}
	if args["pattern"] == "" {
		return nil, errors.New("the pattern is empty")
	}
	if _, err := regexp.Compile(args["pattern"]); err != nil {
		return nil, err
	}
	path, err := location(args["path"], call.Workdir, true)
	if err != nil {
		return nil, err
	}
	return r.flags(path, call.Workdir, false)
}

// openRegular opens a file only if it's a regular file, checking again on the open
// handle so that what's checked is what's read, even if the path changes in between.
// O_NONBLOCK keeps a FIFO swapped in at the last moment from hanging the open.
func (r *reader) openRegular(path string) (*os.File, fs.FileInfo, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, nil, fmt.Errorf("%s isn't a regular file", path)
	}
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, nil, err
	}
	info, err = f.Stat()
	if err == nil && !info.Mode().IsRegular() {
		err = fmt.Errorf("%s isn't a regular file", path)
	}
	if err == nil && r.isProtected(info) {
		err = fmt.Errorf("%s is protected, so tack won't read it", path)
	}
	if err != nil {
		f.Close()
		return nil, nil, err
	}
	return f, info, nil
}

func (r *reader) readFile(_ context.Context, call core.Call) ([]byte, error) {
	args, err := arguments(call, "path")
	if err != nil {
		return nil, err
	}
	path, err := location(args["path"], call.Workdir, false)
	if err != nil {
		return nil, err
	}
	f, _, err := r.openRegular(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, maxRead+1))
	if err != nil {
		return data, err
	}
	if len(data) > maxRead {
		return data[:maxRead], fmt.Errorf("only the first %d bytes were read", maxRead)
	}
	return data, nil
}

func (r *reader) listFiles(_ context.Context, call core.Call) ([]byte, error) {
	args, err := arguments(call, "path")
	if err != nil {
		return nil, err
	}
	path, err := location(args["path"], call.Workdir, true)
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(path)
	if err != nil {
		return nil, err
	}
	var b bytes.Buffer
	for i, e := range entries {
		if i == maxListed {
			fmt.Fprintf(&b, "... and %d more\n", len(entries)-maxListed)
			break
		}
		switch {
		case e.IsDir():
			fmt.Fprintf(&b, "%s/\n", e.Name())
		case e.Type()&fs.ModeSymlink != 0:
			target, err := os.Readlink(filepath.Join(path, e.Name()))
			if err != nil {
				target = "?"
			}
			fmt.Fprintf(&b, "%s -> %s\n", e.Name(), target)
		default:
			fmt.Fprintf(&b, "%s\n", e.Name())
		}
	}
	return b.Bytes(), nil
}

// searchFiles never follows symlinks and skips files that aren't world-readable.
// Nobody approves the individual files in a search, so it shouldn't reach any that
// would get a flag if they were asked for by name.
func (r *reader) searchFiles(ctx context.Context, call core.Call) ([]byte, error) {
	args, err := arguments(call, "pattern", "path")
	if err != nil {
		return nil, err
	}
	pattern, err := regexp.Compile(args["pattern"])
	if err != nil {
		return nil, err
	}
	root, err := location(args["path"], call.Workdir, true)
	if err != nil {
		return nil, err
	}
	var b bytes.Buffer
	matches, private, large, binary, unreadable := 0, 0, 0, 0, 0
	err = filepath.WalkDir(root, func(path string, d fs.DirEntry, walkErr error) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if walkErr != nil {
			unreadable++
			if d != nil && d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if d.IsDir() {
			if path != root && slices.Contains(call.Settings.SearchSkip, d.Name()) {
				return fs.SkipDir
			}
			return nil
		}
		if !d.Type().IsRegular() {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			unreadable++
			return nil
		}
		switch {
		case info.Mode().Perm()&0o004 == 0:
			private++
			return nil
		case info.Size() > maxSearchFile:
			large++
			return nil
		}
		f, _, err := r.openRegular(path)
		if err != nil {
			unreadable++
			return nil
		}
		defer f.Close()
		shown := path
		if rel, err := filepath.Rel(root, path); err == nil && rel != "." {
			shown = rel
		}
		scanner := bufio.NewScanner(f)
		scanner.Buffer(make([]byte, 64<<10), maxSearchFile+1)
		for line := 1; scanner.Scan(); line++ {
			text := scanner.Bytes()
			if bytes.IndexByte(text, 0) >= 0 {
				binary++
				return nil
			}
			if !pattern.Match(text) {
				continue
			}
			if matches++; matches > maxMatches {
				return fs.SkipAll
			}
			if len(text) > maxLineShown {
				fmt.Fprintf(&b, "%s:%d:%s...\n", shown, line, text[:maxLineShown])
			} else {
				fmt.Fprintf(&b, "%s:%d:%s\n", shown, line, text)
			}
		}
		return nil
	})
	if err != nil {
		return b.Bytes(), err
	}
	if matches > maxMatches {
		fmt.Fprintf(&b, "Stopped after %d matches.\n", maxMatches)
	}
	if matches == 0 {
		b.WriteString("No matches.\n")
	}
	if skipped := private + large + binary + unreadable; skipped > 0 {
		fmt.Fprintf(&b, "Skipped %d files: %d not world-readable, %d large, %d binary, %d unreadable.\n", skipped, private, large, binary, unreadable)
	}
	return b.Bytes(), nil
}
