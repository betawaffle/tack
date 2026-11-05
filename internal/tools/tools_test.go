package tools

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/betawaffle/tack/internal/core"
	"github.com/betawaffle/tack/internal/store"
)

type fixture struct {
	t       *testing.T
	workdir string
	key     string
	tools   map[string]core.Tool
}

func newFixture(t *testing.T) *fixture {
	base := t.TempDir()
	f := &fixture{t: t, workdir: filepath.Join(base, "work"), key: filepath.Join(base, "secret-key"), tools: map[string]core.Tool{}}
	if err := os.Mkdir(f.workdir, 0o755); err != nil {
		t.Fatal(err)
	}
	f.write(f.key, "the key", 0o600)
	for _, tool := range All(Options{Protected: []string{f.key}}) {
		f.tools[tool.Name] = tool
	}
	return f
}

func (f *fixture) write(path, text string, mode os.FileMode) {
	f.t.Helper()
	if !filepath.IsAbs(path) {
		path = filepath.Join(f.workdir, path)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		f.t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(text), mode); err != nil {
		f.t.Fatal(err)
	}
	if err := os.Chmod(path, mode); err != nil {
		f.t.Fatal(err)
	}
}

func (f *fixture) call(args map[string]string) core.Call {
	f.t.Helper()
	out := map[string]jsontext.Value{}
	for name, value := range args {
		data, err := json.Marshal(value)
		if err != nil {
			f.t.Fatal(err)
		}
		out[name] = data
	}
	return core.Call{Args: out, Workdir: f.workdir, Settings: store.Settings{
		SearchSkip:            []string{".git", "node_modules"},
		CommandEnv:            map[string]string{"PATH": "/usr/bin:/bin", "ONLY_THIS": "yes"},
		CommandTimeoutSeconds: 5,
	}}
}

func (f *fixture) run(name string, args map[string]string) (string, error) {
	f.t.Helper()
	out, err := f.tools[name].Run(context.Background(), f.call(args))
	return string(out), err
}

func (f *fixture) inspect(name string, args map[string]string) []string {
	f.t.Helper()
	flags, err := f.tools[name].Inspect(f.call(args))
	if err != nil {
		f.t.Fatal(err)
	}
	return flags
}

func TestDefinitionsAreValidAndNamed(t *testing.T) {
	f := newFixture(t)
	for name, tool := range f.tools {
		var def struct {
			Function struct {
				Name string `json:"name"`
			} `json:"function"`
		}
		definition := tool.Definition
		if tool.Define != nil {
			definition = tool.Define(store.Settings{SearchSkip: []string{"x"}})
		}
		if err := json.Unmarshal(definition, &def); err != nil || def.Function.Name != name {
			t.Errorf("%s: %s, %v", name, definition, err)
		}
	}
}

func TestReadFile(t *testing.T) {
	f := newFixture(t)
	f.write("hello.txt", "hi\xff there", 0o644)
	if got, err := f.run("read_file", map[string]string{"path": "hello.txt"}); err != nil || got != "hi\xff there" {
		t.Errorf("got %q, %v", got, err)
	}
	if got, err := f.run("read_file", map[string]string{"path": filepath.Join(f.workdir, "hello.txt")}); err != nil || !strings.HasPrefix(got, "hi") {
		t.Errorf("absolute: %q, %v", got, err)
	}
	if _, err := f.run("read_file", map[string]string{"path": "missing.txt"}); err == nil {
		t.Error("missing file should fail")
	}
}

func TestTheKeyIsRefusedHoweverItsReached(t *testing.T) {
	f := newFixture(t)
	if err := os.Symlink(f.key, filepath.Join(f.workdir, "link")); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(f.key, filepath.Join(f.workdir, "hardlink")); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{f.key, "link", "hardlink", "../secret-key"} {
		got, err := f.run("read_file", map[string]string{"path": path})
		if err == nil || strings.Contains(got, "the key") || !strings.Contains(err.Error(), "protected") {
			t.Errorf("%s: got %q, %v", path, got, err)
		}
		// It isn't offered for approval, since approving it couldn't do anything.
		if _, err := f.tools["read_file"].Inspect(f.call(map[string]string{"path": path})); err == nil || !strings.Contains(err.Error(), "protected") {
			t.Errorf("%s: inspect = %v", path, err)
		}
	}
}

func TestOnlyRegularFilesAreRead(t *testing.T) {
	f := newFixture(t)
	fifo := filepath.Join(f.workdir, "fifo")
	if err := syscall.Mkfifo(fifo, 0o644); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := f.tools["read_file"].Run(context.Background(), f.call(map[string]string{"path": "fifo"}))
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Error("a FIFO should be refused")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("reading a FIFO hung")
	}
	if _, err := f.tools["read_file"].Inspect(f.call(map[string]string{"path": "fifo"})); err == nil {
		t.Error("a FIFO shouldn't be offered for approval")
	}
	if flags := f.inspect("list_files", map[string]string{"path": "."}); len(flags) != 0 {
		t.Errorf("a directory is fine to list: %v", flags)
	}
	if _, err := f.run("read_file", map[string]string{"path": "."}); err == nil {
		t.Error("a directory should be refused")
	}
	if _, err := f.run("read_file", map[string]string{"path": "/dev/zero"}); err == nil {
		t.Error("a device should be refused")
	}
}

func TestFlags(t *testing.T) {
	f := newFixture(t)
	f.write("open.txt", "x", 0o644)
	f.write("private.txt", "x", 0o600)
	f.write(filepath.Join(filepath.Dir(f.workdir), "outside.txt"), "x", 0o644)
	if err := os.Symlink(filepath.Join(filepath.Dir(f.workdir), "outside.txt"), filepath.Join(f.workdir, "sneaky")); err != nil {
		t.Fatal(err)
	}
	for path, want := range map[string][]string{
		"open.txt":       nil,
		"missing.txt":    nil,
		"private.txt":    {FlagPrivate},
		"../outside.txt": {FlagOutside},
		"sneaky":         {FlagOutside},
		"/etc/hostname":  {FlagOutside},
	} {
		got := f.inspect("read_file", map[string]string{"path": path})
		if !slices.Equal(got, want) {
			t.Errorf("%s: flags = %v, want %v", path, got, want)
		}
	}
}

func TestBadArgumentsAreCaughtBeforeApproval(t *testing.T) {
	f := newFixture(t)
	cases := []struct {
		tool string
		args string
	}{
		{"read_file", `{}`},
		{"read_file", `{"path":""}`},
		{"read_file", `{"path":1}`},
		{"read_file", `{"path":"a","extra":"b"}`},
		{"read_file", `{"path":"a\u0000b"}`},
		{"search_files", `{}`},
		{"search_files", `{"pattern":"("}`},
		{"list_files", `{"path":true}`},
	}
	for _, tc := range cases {
		var args map[string]jsontext.Value
		if err := json.Unmarshal([]byte(tc.args), &args); err != nil {
			t.Fatal(err)
		}
		if _, err := f.tools[tc.tool].Inspect(core.Call{Args: args, Workdir: f.workdir}); err == nil {
			t.Errorf("%s %s: no error", tc.tool, tc.args)
		}
	}
}

func TestRelativePathsNeedAWorkdir(t *testing.T) {
	f := newFixture(t)
	call := f.call(map[string]string{"path": "a.txt"})
	call.Workdir = ""
	if _, err := f.tools["read_file"].Inspect(call); err == nil {
		t.Error("expected an error")
	}
}

func TestListFiles(t *testing.T) {
	f := newFixture(t)
	f.write("b.txt", "x", 0o644)
	f.write("sub/inner.txt", "x", 0o644)
	if err := os.Symlink("b.txt", filepath.Join(f.workdir, "a-link")); err != nil {
		t.Fatal(err)
	}
	got, err := f.run("list_files", map[string]string{})
	if want := "a-link -> b.txt\nb.txt\nsub/\n"; err != nil || got != want {
		t.Errorf("got %q, %v", got, err)
	}
}

func TestSearchFiles(t *testing.T) {
	f := newFixture(t)
	f.write("a.txt", "one\nneedle here\nthree\n", 0o644)
	f.write("sub/b.txt", "a needle\n", 0o644)
	f.write("private.txt", "needle\n", 0o600)
	f.write("bin.dat", "needle\x00\n", 0o644)
	f.write(".git/config", "needle\n", 0o644)
	f.write("node_modules/x.js", "needle\n", 0o644)
	f.write("big.txt", "needle\n"+strings.Repeat("x", maxSearchFile), 0o644)
	if err := os.Symlink(f.key, filepath.Join(f.workdir, "link")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("a.txt", filepath.Join(f.workdir, "link2")); err != nil {
		t.Fatal(err)
	}
	got, err := f.run("search_files", map[string]string{"pattern": "need+le"})
	if err != nil {
		t.Fatal(err)
	}
	want := "a.txt:2:needle here\nsub/b.txt:1:a needle\nSkipped 3 files: 1 not world-readable, 1 large, 1 binary, 0 unreadable.\n"
	if got != want {
		t.Errorf("got:\n%s\nwant:\n%s", got, want)
	}
	if got, err := f.run("search_files", map[string]string{"pattern": "nothing like this"}); err != nil || !strings.HasPrefix(got, "No matches.") {
		t.Errorf("got %q, %v", got, err)
	}
}

func TestSearchNeverReturnsTheKey(t *testing.T) {
	f := newFixture(t)
	if err := os.Chmod(f.key, 0o644); err != nil { // even if its mode is wrong
		t.Fatal(err)
	}
	call := f.call(map[string]string{"pattern": "the key", "path": filepath.Dir(f.key)})
	out, err := f.tools["search_files"].Run(context.Background(), call)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(out), "the key") && !strings.HasPrefix(string(out), "No matches") {
		t.Errorf("got %s", out)
	}
}

func TestSearchStopsWhenCancelled(t *testing.T) {
	f := newFixture(t)
	f.write("a.txt", "x\n", 0o644)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := f.tools["search_files"].Run(ctx, f.call(map[string]string{"pattern": "x"})); err == nil {
		t.Error("expected an error")
	}
}

func TestSearchSkipComesFromTheSnapshot(t *testing.T) {
	f := newFixture(t)
	f.write(".git/config", "needle\n", 0o644)
	f.write("vendor/x.txt", "needle\n", 0o644)
	search := func(skip []string) string {
		call := f.call(map[string]string{"pattern": "needle"})
		call.Settings.SearchSkip = skip
		out, err := f.tools["search_files"].Run(context.Background(), call)
		if err != nil {
			t.Fatal(err)
		}
		return string(out)
	}
	if got := search([]string{"vendor"}); !strings.Contains(got, ".git/config") || strings.Contains(got, "vendor/") {
		t.Errorf("skipping vendor: %s", got)
	}
	if got := search(nil); !strings.Contains(got, ".git/config") || !strings.Contains(got, "vendor/x.txt") {
		t.Errorf("skipping nothing: %s", got)
	}
	def := string(f.tools["search_files"].Define(store.Settings{SearchSkip: []string{"vendor"}}))
	if !strings.Contains(def, "Skips vendor,") {
		t.Errorf("definition = %s", def)
	}
	if def := string(f.tools["search_files"].Define(store.Settings{})); strings.Contains(def, ".git") {
		t.Errorf("definition = %s", def)
	}
}

func (f *fixture) read(path string) string {
	f.t.Helper()
	data, err := os.ReadFile(filepath.Join(f.workdir, path))
	if err != nil {
		f.t.Fatal(err)
	}
	return string(data)
}

func TestEditReplacesExactlyOneMatch(t *testing.T) {
	f := newFixture(t)
	f.write("a.txt", "one two two\n", 0o640)
	edit := func(old, new string) (string, error) {
		return f.run("edit_file", map[string]string{"path": "a.txt", "old_string": old, "new_string": new})
	}
	if _, err := edit("missing", "x"); err == nil {
		t.Error("no match should fail")
	}
	if _, err := edit("two", "x"); err == nil || !strings.Contains(err.Error(), "2 times") {
		t.Errorf("two matches: %v", err)
	}
	if f.read("a.txt") != "one two two\n" {
		t.Error("a failed edit changed the file")
	}
	if _, err := edit("one", "uno"); err != nil {
		t.Fatal(err)
	}
	if got := f.read("a.txt"); got != "uno two two\n" {
		t.Errorf("file = %q", got)
	}
	if info, _ := os.Stat(filepath.Join(f.workdir, "a.txt")); info.Mode().Perm() != 0o640 {
		t.Errorf("mode = %o", info.Mode().Perm())
	}
	entries, _ := os.ReadDir(f.workdir)
	if len(entries) != 1 {
		t.Errorf("left behind: %v", entries)
	}
}

func TestEditKeepsWhatWasThere(t *testing.T) {
	f := newFixture(t)
	f.write("a.txt", "before\n", 0o644)
	call := f.call(map[string]string{"path": "a.txt", "old_string": "before", "new_string": "after"})
	kept := map[string]string{}
	call.Keep = func(name string, data []byte) error {
		kept[name] = string(data)
		return nil
	}
	if _, err := f.tools["edit_file"].Run(context.Background(), call); err != nil {
		t.Fatal(err)
	}
	if kept["before"] != "before\n" {
		t.Errorf("kept = %v", kept)
	}
}

func TestEditCreatesOnlyNewFiles(t *testing.T) {
	f := newFixture(t)
	if _, err := f.run("edit_file", map[string]string{"path": "new.txt", "old_string": "", "new_string": "hello"}); err != nil {
		t.Fatal(err)
	}
	if f.read("new.txt") != "hello" {
		t.Errorf("file = %q", f.read("new.txt"))
	}
	args := map[string]string{"path": "new.txt", "old_string": "", "new_string": "again"}
	if _, err := f.tools["edit_file"].Inspect(f.call(args)); err == nil {
		t.Error("an existing file shouldn't be offered as a create")
	}
	if _, err := f.run("edit_file", args); err == nil || f.read("new.txt") != "hello" {
		t.Errorf("create over a file: %v, %q", err, f.read("new.txt"))
	}
	if _, err := f.run("edit_file", map[string]string{"path": "no/such/dir/x", "old_string": "", "new_string": "x"}); err == nil {
		t.Error("a missing directory should fail")
	}
}

func TestEditThroughASymlinkKeepsTheLink(t *testing.T) {
	f := newFixture(t)
	f.write("real.txt", "text\n", 0o644)
	if err := os.Symlink("real.txt", filepath.Join(f.workdir, "link")); err != nil {
		t.Fatal(err)
	}
	if _, err := f.run("edit_file", map[string]string{"path": "link", "old_string": "text", "new_string": "changed"}); err != nil {
		t.Fatal(err)
	}
	if info, err := os.Lstat(filepath.Join(f.workdir, "link")); err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Errorf("link: %v, %v", info, err)
	}
	if f.read("real.txt") != "changed\n" {
		t.Errorf("file = %q", f.read("real.txt"))
	}
}

func TestEditRefusesTheKey(t *testing.T) {
	f := newFixture(t)
	if err := os.Symlink(f.key, filepath.Join(f.workdir, "link")); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{f.key, "link"} {
		args := map[string]string{"path": path, "old_string": "the key", "new_string": "x"}
		if _, err := f.tools["edit_file"].Inspect(f.call(args)); err == nil {
			t.Errorf("%s: offered for approval", path)
		}
		if _, err := f.run("edit_file", args); err == nil {
			t.Errorf("%s: edited", path)
		}
	}
	if data, _ := os.ReadFile(f.key); string(data) != "the key" {
		t.Errorf("key file = %q", data)
	}
}

func TestEditFlags(t *testing.T) {
	f := newFixture(t)
	outside := filepath.Dir(f.workdir)
	if err := os.Symlink(outside, filepath.Join(f.workdir, "up")); err != nil {
		t.Fatal(err)
	}
	// A new file under a symlinked directory is outside, though the path doesn't say so.
	flags := f.inspect("edit_file", map[string]string{"path": "up/new.txt", "old_string": "", "new_string": "x"})
	if !slices.Contains(flags, FlagOutside) {
		t.Errorf("flags = %v", flags)
	}
	for _, args := range []map[string]string{
		{"path": "a", "old_string": "x"},
		{"path": "a", "new_string": "x"},
		{"old_string": "x", "new_string": "y"},
		{"path": "a", "old_string": "x", "new_string": "x"},
	} {
		if _, err := f.tools["edit_file"].Inspect(f.call(args)); err == nil {
			t.Errorf("%v: no error", args)
		}
	}
}

func TestCommandRunsInTheWorkdirWithOnlyTheConfiguredEnvironment(t *testing.T) {
	f := newFixture(t)
	t.Setenv("TACK_SHOULD_NOT_LEAK", "secret-looking")
	got, err := f.run("run_command", map[string]string{"command": "pwd; env | sort; cat; echo to-stderr >&2"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, f.workdir+"\n") || !strings.Contains(got, "ONLY_THIS=yes") || !strings.Contains(got, "to-stderr") {
		t.Errorf("output = %s", got)
	}
	if strings.Contains(got, "TACK_SHOULD_NOT_LEAK") {
		t.Errorf("the environment leaked: %s", got)
	}
}

func TestCommandNeverUsesTacksOwnDirectory(t *testing.T) {
	f := newFixture(t)
	for _, workdir := range []string{"", "relative"} {
		call := f.call(map[string]string{"command": "pwd"})
		call.Workdir = workdir
		if out, err := f.tools["run_command"].Run(context.Background(), call); err == nil {
			t.Errorf("workdir %q ran: %s", workdir, out)
		}
		if _, err := f.tools["run_command"].Inspect(call); err == nil {
			t.Errorf("workdir %q was offered", workdir)
		}
	}
}

func TestCommandFlagIsPermanent(t *testing.T) {
	f := newFixture(t)
	if flags := f.inspect("run_command", map[string]string{"command": "true"}); !slices.Equal(flags, []string{FlagCommand}) {
		t.Errorf("flags = %v", flags)
	}
}

func TestCommandExitCodeIsReported(t *testing.T) {
	f := newFixture(t)
	got, err := f.run("run_command", map[string]string{"command": "echo out; exit 3"})
	if err == nil || !strings.Contains(err.Error(), "exit code 3") || got != "out\n" {
		t.Errorf("got %q, %v", got, err)
	}
}

func TestCommandIsKilledWithItsChildren(t *testing.T) {
	f := newFixture(t)
	marker := filepath.Join(f.workdir, "still-running")
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	call := f.call(map[string]string{"command": "(sleep 1; touch " + marker + ") & echo started; wait"})
	start := time.Now()
	out, err := f.tools["run_command"].Run(ctx, call)
	if err == nil || !strings.Contains(err.Error(), "stopped") || string(out) != "started\n" {
		t.Errorf("got %q, %v", out, err)
	}
	if took := time.Since(start); took > 4*time.Second {
		t.Errorf("took %s", took)
	}
	time.Sleep(1200 * time.Millisecond)
	if _, err := os.Stat(marker); err == nil {
		t.Error("a child survived")
	}
}

func TestBackgroundChildrenCantHangACommand(t *testing.T) {
	f := newFixture(t)
	marker := filepath.Join(f.workdir, "left-running")
	start := time.Now()
	got, _ := f.run("run_command", map[string]string{"command": "(sleep 8; touch " + marker + ") & echo done"})
	if !strings.Contains(got, "done") {
		t.Errorf("output = %q", got)
	}
	if took := time.Since(start); took > 7*time.Second {
		t.Errorf("took %s", took)
	}
}

func TestBadCommandsAreCaughtBeforeApproval(t *testing.T) {
	f := newFixture(t)
	for _, args := range []map[string]string{{"command": ""}, {"command": "  "}, {"command": "a\x00b"}, {"cmd": "ls"}} {
		if _, err := f.tools["run_command"].Inspect(f.call(args)); err == nil {
			t.Errorf("%v: no error", args)
		}
	}
}
