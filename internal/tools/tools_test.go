package tools

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/betawaffle/tack/internal/cgroup"
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
	f.write("big.txt", "needle\n"+strings.Repeat("x", 2<<20), 0o644)
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
	call := f.call(map[string]string{"command": "setsid bash -c 'sleep 1; touch " + marker + "' & echo started; wait"})
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

func TestTheSandboxHidesAndProtects(t *testing.T) {
	f := newFixture(t)
	outside := filepath.Join(filepath.Dir(f.workdir), "outside.txt")
	f.write(outside, "outside\n", 0o644)
	got, err := f.run("run_command", map[string]string{"command": strings.Join([]string{
		"cat " + outside + " 2>&1 && echo READ-OUTSIDE",
		"cat " + f.key + " 2>&1 && echo READ-KEY",
		"touch /usr/nope 2>&1 && echo WROTE-USR",
		"touch " + outside + "2 2>&1",
		"touch here && echo WROTE-HERE",
		"unshare -m true 2>&1 && echo MADE-NAMESPACE",
		"(exec 3<>/dev/tcp/1.1.1.1/53) 2>&1 && echo CONNECTED",
		"ls /sys/fs/cgroup 2>&1 && echo SAW-CGROUPS",
		"true",
	}, "; ")})
	if err != nil {
		t.Fatalf("%v: %s", err, got)
	}
	for _, bad := range []string{"READ-OUTSIDE\n", "READ-KEY\n", "WROTE-USR\n", "MADE-NAMESPACE\n", "CONNECTED\n", "SAW-CGROUPS\n"} {
		if strings.Contains(got, bad) {
			t.Errorf("%s:\n%s", bad, got)
		}
	}
	if !strings.Contains(got, "WROTE-HERE") {
		t.Errorf("couldn't write in the working directory:\n%s", got)
	}
	// The parents of the working directory are an empty tmpfs in the sandbox, so
	// that touch may succeed there. What matters is that it doesn't reach the host.
	if _, err := os.Stat(outside + "2"); err == nil {
		t.Error("wrote outside")
	}
}

func TestAKeyInsideTheWorkdirIsStillHidden(t *testing.T) {
	f := newFixture(t)
	inside := filepath.Join(f.workdir, "key-file")
	f.write(inside, "the inside key", 0o600)
	tools := map[string]core.Tool{}
	for _, tool := range All(Options{Protected: []string{inside}}) {
		tools[tool.Name] = tool
	}
	out, err := tools["run_command"].Run(context.Background(), f.call(map[string]string{"command": "cat key-file; ls -la key-file"}))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(out), "the inside key") {
		t.Errorf("output = %s", out)
	}
}

func TestReadOnlyPathsAreConfigured(t *testing.T) {
	f := newFixture(t)
	extra := filepath.Join(filepath.Dir(f.workdir), "tools-dir")
	f.write(filepath.Join(extra, "note"), "shared\n", 0o644)
	readable := filepath.Join(filepath.Dir(f.workdir), "writable")
	f.write(filepath.Join(readable, "note"), "x", 0o644)
	call := f.call(map[string]string{"command": "cat " + extra + "/note; touch " + extra + "/no 2>&1 | wc -l; touch " + readable + "/yes && echo wrote"})
	call.Settings.SandboxReadOnly = append(slices.Clone(store.DefaultSandboxReadOnly), extra)
	call.Settings.SandboxReadWrite = []string{readable}
	out, err := f.tools["run_command"].Run(context.Background(), call)
	if err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	if !strings.Contains(string(out), "shared") || !strings.Contains(string(out), "wrote") {
		t.Errorf("output = %s", out)
	}
	if _, err := os.Stat(filepath.Join(extra, "no")); err == nil {
		t.Error("wrote to a read-only path")
	}
}

func TestNetworkIsOnlyThereWhenAllowed(t *testing.T) {
	f := newFixture(t)
	count := func(network bool) string {
		call := f.call(map[string]string{"command": "wc -l < /proc/net/dev"})
		call.Network = network
		out, err := f.tools["run_command"].Run(context.Background(), call)
		if err != nil {
			t.Fatalf("%v: %s", err, out)
		}
		return string(out)
	}
	host, err := os.ReadFile("/proc/net/dev")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(string(host), "\n") <= 3 {
		t.Skip("this machine only has loopback")
	}
	if off, on := count(false), count(true); off == on {
		t.Errorf("the same interfaces either way: %q", off)
	}
}

func TestTheSandboxArgumentsAreKept(t *testing.T) {
	f := newFixture(t)
	call := f.call(map[string]string{"command": "true"})
	kept := map[string]string{}
	call.Keep = func(name string, data []byte) error {
		kept[name] = string(data)
		return nil
	}
	if _, err := f.tools["run_command"].Run(context.Background(), call); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(kept["sandbox"], "--unshare-all") {
		t.Errorf("kept = %v", kept)
	}
}

func TestLimitsComeFromSettings(t *testing.T) {
	f := newFixture(t)
	f.write("big.txt", "0123456789", 0o644)
	call := f.call(map[string]string{"path": "big.txt"})
	call.Settings.MaxReadBytes = 4
	out, err := f.tools["read_file"].Run(context.Background(), call)
	if string(out) != "0123" || err == nil {
		t.Errorf("got %q, %v", out, err)
	}
}

func TestEditIsMarkedAsTacksOwnMessage(t *testing.T) {
	f := newFixture(t)
	if !f.tools["edit_file"].TackWritten || f.tools["run_command"].TackWritten || f.tools["read_file"].TackWritten {
		t.Error("only edit_file writes its own message")
	}
	for name, tool := range f.tools {
		if !tool.NeedsWorkdir && name != "run_code" {
			t.Errorf("%s doesn't need a working directory", name)
		}
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

func TestGitStaysReadOnly(t *testing.T) {
	f := newFixture(t)
	dir := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = f.workdir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	dir("init", "-q")
	f.write("a.txt", "a\n", 0o644)
	got, err := f.run("run_command", map[string]string{"command": strings.Join([]string{
		"touch .git/hooks/pre-commit 2>/dev/null && echo WROTE-HOOK",
		"echo '[core]' >> .git/config 2>/dev/null && echo WROTE-CONFIG",
		"git add a.txt 2>/dev/null && echo ADDED",
		"git status --short",
		"touch b.txt && echo WROTE-SOURCE",
	}, "; ")})
	if err != nil {
		t.Fatalf("%v: %s", err, got)
	}
	for _, bad := range []string{"WROTE-HOOK\n", "WROTE-CONFIG\n", "ADDED\n"} {
		if strings.Contains(got, bad) {
			t.Errorf("%s:\n%s", bad, got)
		}
	}
	if !strings.Contains(got, "?? a.txt") || !strings.Contains(got, "WROTE-SOURCE") {
		t.Errorf("git status and source changes should still work:\n%s", got)
	}
	if _, err := os.Stat(filepath.Join(f.workdir, ".git/hooks/pre-commit")); err == nil {
		t.Error("a hook reached the host")
	}
}

func TestStoppingACommandAsksFirstWithACgroup(t *testing.T) {
	if os.Getenv("TACK_TEST_CGROUP") == "" {
		t.Skip("needs a delegated cgroup")
	}
	groups, err := cgroup.Setup()
	if err != nil {
		t.Fatal(err)
	}
	f := newFixture(t)
	tools := map[string]core.Tool{}
	for _, tool := range All(Options{Cgroups: groups}) {
		tools[tool.Name] = tool
	}
	run := func(command string, timeout time.Duration) (string, error, time.Duration) {
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		defer cancel()
		call := f.call(map[string]string{"command": command})
		call.Execution = store.NewID()
		call.Settings.StopGraceSeconds = 2
		start := time.Now()
		out, err := tools["run_command"].Run(ctx, call)
		return string(out), err, time.Since(start)
	}
	// A clean exit gets a chance.
	got, err, took := run("trap 'echo cleaning up; exit 0' TERM; echo started; sleep 30 & wait", 300*time.Millisecond)
	if err == nil || !strings.Contains(got, "cleaning up") || took > 1500*time.Millisecond {
		t.Errorf("clean: %q, %v, %s", got, err, took)
	}
	// One that ignores the signal is killed after the grace period, children and all.
	marker := filepath.Join(f.workdir, "survived")
	got, err, took = run("trap '' TERM; setsid bash -c 'trap \"\" TERM; sleep 4; touch "+marker+"' & sleep 30", 300*time.Millisecond)
	if err == nil || took < 2*time.Second || took > 5*time.Second {
		t.Errorf("ignored: %q, %v, %s", got, err, took)
	}
	time.Sleep(4 * time.Second)
	if _, err := os.Stat(marker); err == nil {
		t.Error("a child survived")
	}
}

func runCode(t *testing.T, f *fixture, code string) (string, error) {
	t.Helper()
	if _, err := os.Stat(python); err != nil {
		t.Skip("no python here")
	}
	return f.run("run_code", map[string]string{"language": "python", "code": code})
}

func TestCodeRunsAndReportsLikeACommand(t *testing.T) {
	f := newFixture(t)
	got, err := runCode(t, f, "import sys\nprint(2 + 2)\nprint('to-stderr', file=sys.stderr)\nsys.exit(3)")
	if !strings.Contains(got, "4\n") || !strings.Contains(got, "to-stderr") {
		t.Errorf("output = %q", got)
	}
	if err == nil || err.Error() != "exit code 3" {
		t.Errorf("err = %v", err)
	}
	// It's not limited by how long a command line can be.
	long := "x = '" + strings.Repeat("a", 1<<20) + "'\nprint(len(x))"
	if got, err := runCode(t, f, long); err != nil || got != "1048576\n" {
		t.Errorf("long = %q, %v", got, err)
	}
}

func TestCodeCantReachAnything(t *testing.T) {
	f := newFixture(t)
	t.Setenv("TACK_SHOULD_NOT_LEAK", "secret-looking")
	outside := filepath.Join(filepath.Dir(f.workdir), "outside.txt")
	f.write(outside, "outside\n", 0o644)
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	f.write(filepath.Join(f.workdir, "secret.txt"), "in the workdir\n", 0o644)
	script := `
import os, socket, subprocess
def attempt(name, fn):
    try:
        fn()
        print("REACHED", name)
    except Exception as e:
        print("refused", name, type(e).__name__)
for path in ` + fmt.Sprintf("%q, %q, %q, %q, %q", outside, f.key, filepath.Join(f.workdir, "secret.txt"), "/etc/passwd", filepath.Join(home, ".bashrc")) + `:
    attempt("read " + path, lambda: open(path).read())
attempt("write usr", lambda: open("/usr/nope", "w"))
def connect():
    s = socket.socket()
    s.settimeout(2)
    s.connect(("1.1.1.1", 53))
attempt("network", connect)
attempt("namespace", lambda: subprocess.run(["unshare", "-m", "true"], check=True, capture_output=True))
print("cwd", os.getcwd())
print("env", sorted(os.environ))
attempt("proc", lambda: os.listdir("/proc"))
attempt("write root", lambda: open("/nope", "w"))
attempt("write dev", lambda: open("/dev/nope", "w"))
print("host", os.uname().nodename)
for gone in ["/usr/bin/gcc", "/usr/bin/bash", "/bin/sh", "/usr/local/bin", "/usr/lib64/python3.14/site-packages/pip"]:
    print("exists", gone, os.path.exists(gone))
print("workdir exists", os.path.exists(` + fmt.Sprintf("%q", f.workdir) + `))
open("/tmp/mine", "w").write("kept for now")
print("tmp", open("/tmp/mine").read())
`
	got, err := runCode(t, f, script)
	if err != nil {
		t.Fatalf("%v: %s", err, got)
	}
	if strings.Contains(got, "REACHED") {
		t.Errorf("it got out:\n%s", got)
	}
	if strings.Contains(got, "TACK_SHOULD_NOT_LEAK") || strings.Contains(got, "ONLY_THIS") {
		t.Errorf("the environment leaked:\n%s", got)
	}
	for _, want := range []string{"cwd /work\n", "workdir exists False\n", "tmp kept for now\n", "refused network", "refused namespace", "host sandbox\n",
		"exists /usr/bin/gcc False", "exists /usr/bin/bash False", "exists /bin/sh False", "exists /usr/local/bin False", "site-packages/pip False"} {
		if !strings.Contains(got, want) {
			t.Errorf("no %q in:\n%s", want, got)
		}
	}
	// /tmp doesn't carry over.
	if got, err := runCode(t, f, "import os\nprint(os.listdir('/tmp'))"); err != nil || got != "[]\n" {
		t.Errorf("next call = %q, %v", got, err)
	}
}

func TestCodeNeedsNoWorkingDirectoryAndIgnoresSettings(t *testing.T) {
	f := newFixture(t)
	if _, err := os.Stat(python); err != nil {
		t.Skip("no python here")
	}
	// A conversation's settings can widen what commands see, but never what code sees.
	call := f.call(map[string]string{"language": "python", "code": "import os\nprint(os.path.exists('/etc/passwd'), os.path.exists(" + fmt.Sprintf("%q", f.workdir) + "))"})
	call.Workdir = ""
	call.Settings.SandboxReadOnly = []string{"/etc", f.workdir}
	call.Network = true
	out, err := f.tools["run_code"].Run(context.Background(), call)
	if err != nil || string(out) != "False False\n" {
		t.Errorf("out = %q, %v", out, err)
	}
	if f.tools["run_code"].NeedsWorkdir || f.tools["run_code"].NetworkOption || f.tools["run_code"].NoApproval {
		t.Error("it should need no directory, offer no network, and still need approval")
	}
}

func TestCodeLimitsHold(t *testing.T) {
	f := newFixture(t)
	// None of the places it can write are unlimited.
	got, err := runCode(t, f, "import os\nfor path in ['/work/big', '/dev/shm/big']:\n    try:\n        open(path, 'wb').write(b'x' * (128 << 20))\n        print('WROTE', path)\n    except OSError as e:\n        print('full', e.errno)\n    os.remove(path)")
	if err != nil || strings.Contains(got, "WROTE") {
		t.Errorf("big = %q, %v", got, err)
	}
	// /tmp is small.
	got, err = runCode(t, f, "try:\n    open('/tmp/big', 'wb').write(b'x' * (128 << 20))\n    print('WROTE')\nexcept OSError as e:\n    print('full', e.errno)")
	if err != nil || strings.Contains(got, "WROTE") {
		t.Errorf("big = %q, %v", got, err)
	}
	// Output is capped, and it says so.
	call := f.call(map[string]string{"language": "python", "code": "print('y' * 100000)"})
	call.Settings.MaxReadBytes = 1000
	out, err := f.tools["run_code"].Run(context.Background(), call)
	if len(out) != 1000 || err == nil || !strings.Contains(err.Error(), "were dropped") {
		t.Errorf("capped = %d bytes, %v", len(out), err)
	}
	// Stopping it ends everything it started.
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	call = f.call(map[string]string{"language": "python", "code": "import subprocess, sys, time\nsubprocess.Popen([sys.executable, '-c', 'import time; time.sleep(60)'])\ntime.sleep(60)"})
	started := time.Now()
	_, err = f.tools["run_code"].Run(ctx, call)
	if err == nil || !strings.Contains(err.Error(), "the code was stopped") {
		t.Errorf("stopped = %v", err)
	}
	if took := time.Since(started); took > 20*time.Second {
		t.Errorf("stopping took %v", took)
	}
}

// Each of these goes straight to the kernel, so nothing but the filter is in the way.
func TestCodeSyscallsAreFiltered(t *testing.T) {
	f := newFixture(t)
	got, err := runCode(t, f, `
import ctypes, errno, socket
libc = ctypes.CDLL(None, use_errno=True)
refused = {"io_uring_setup": 425, "io_uring_enter": 426, "io_uring_register": 427, "bpf": 321, "perf_event_open": 298,
    "userfaultfd": 323, "keyctl": 250, "add_key": 248, "request_key": 249, "ptrace": 101, "process_vm_readv": 310,
    "process_vm_writev": 311, "pidfd_getfd": 438, "pidfd_open": 434, "kcmp": 312, "mount": 165, "umount2": 166, "pivot_root": 155, "chroot": 161,
    "unshare": 272, "setns": 308, "open_tree": 428, "move_mount": 429, "fsopen": 430, "init_module": 175, "finit_module": 313,
    "kexec_load": 246, "reboot": 169, "acct": 163, "swapon": 167, "setuid": 105, "setgid": 106, "setns": 308, "seccomp": 317,
    "memfd_create": 319, "shmget": 29, "msgget": 68, "semget": 64, "mknod": 133, "mknodat": 259, "quotactl": 179, "syslog": 103,
    "fanotify_init": 300, "inotify_init1": 294, "name_to_handle_at": 303, "open_by_handle_at": 304, "landlock_create_ruleset": 444,
    "mbind": 237, "migrate_pages": 256, "move_pages": 279, "vmsplice": 278, "splice": 275, "tee": 276, "modify_ldt": 154, "iopl": 172, "ioperm": 173}
for name, number in refused.items():
    ctypes.set_errno(0)
    result = libc.syscall(number, 0, 0, 0, 0, 0, 0)
    if result != -1 or ctypes.get_errno() != errno.EPERM:
        print("REACHED", name, result, ctypes.get_errno())
# clone3 pretends not to exist, so that libc falls back to clone.
if libc.syscall(435, 0, 0) != -1 or ctypes.get_errno() != errno.ENOSYS:
    print("REACHED clone3")
# A namespace through plain clone, flags in the first argument.
for flag in [0x10000000, 0x00020000, 0x40000000, 0x20000000, 0x08000000, 0x04000000, 0x02000000, 0x80]:
    if libc.syscall(56, flag | 17, 0, 0, 0, 0) != -1 or ctypes.get_errno() != errno.EPERM:
        print("REACHED clone", hex(flag))
# Pushing input into a terminal.
if libc.syscall(16, 0, 0x5412, 0) != -1 or ctypes.get_errno() != errno.ENOTTY:
    print("REACHED TIOCSTI")
# Calling convention tricks: the x32 bit.
if libc.syscall(0x40000000 | 39) != -1:
    print("REACHED x32")
for family in [socket.AF_INET, socket.AF_INET6, socket.AF_NETLINK, socket.AF_PACKET, socket.AF_VSOCK, socket.AF_ALG, 30, 38]:
    try:
        socket.socket(family, socket.SOCK_DGRAM if family != socket.AF_ALG else socket.SOCK_SEQPACKET)
        print("REACHED socket", family)
    except OSError:
        pass
print("done")
`)
	if err != nil || got != "done\n" {
		t.Errorf("%v:\n%s", err, got)
	}
}

// What the filter and the small tree leave has to still be a useful Python.
func TestCodeTheStandardLibraryStillWorks(t *testing.T) {
	f := newFixture(t)
	got, err := runCode(t, f, `
import asyncio, base64, bz2, concurrent.futures, csv, ctypes, dataclasses, datetime, decimal, fractions, gzip, hashlib, heapq, itertools, json, lzma, math, multiprocessing, os, pathlib, pickle, random, re, resource, secrets, shutil, socket, sqlite3, ssl, statistics, struct, subprocess, sys, tarfile, tempfile, threading, time, unicodedata, uuid, xml.etree.ElementTree, zipfile, zlib, zoneinfo

def square(x):
    return x * x

async def together():
    return await asyncio.gather(asyncio.sleep(0.01, 1), asyncio.to_thread(sum, [1, 1]))

if __name__ == "__main__":
    con = sqlite3.connect("/tmp/db.sqlite")
    con.execute("create table t (x)")
    con.execute("insert into t values (41)")
    con.commit()
    assert con.execute("select x + 1 from t").fetchone() == (42,)
    with tempfile.TemporaryDirectory() as d:
        p = pathlib.Path(d, "a.txt")
        p.write_text("héllo")
        shutil.copy2(p, p.with_suffix(".bak"))
        shutil.make_archive(d + "-archive", "gztar", d)
        os.symlink(p, p.with_suffix(".lnk"))
        assert sorted(x.name for x in pathlib.Path(d).iterdir()) == ["a.bak", "a.lnk", "a.txt"]
    with concurrent.futures.ThreadPoolExecutor(4) as pool:
        assert list(pool.map(square, range(4))) == [0, 1, 4, 9]
    assert asyncio.run(together()) == [1, 2]
    a, b = socket.socketpair()
    a.send(b"hi")
    assert b.recv(2) == b"hi"
    out = subprocess.run([sys.executable, "-I", "-c", "print(6 * 7)"], capture_output=True, text=True)
    assert out.stdout == "42\n", out
    with multiprocessing.Pool(2) as pool:
        assert pool.map(square, range(3)) == [0, 1, 4]
    print(datetime.datetime(2026, 7, 1, tzinfo=zoneinfo.ZoneInfo("Asia/Tokyo")).utcoffset())
    print(hashlib.sha256(b"x").hexdigest()[:8], base64.b64encode(zlib.compress(b"x"))[:2], os.cpu_count() > 0, secrets.token_hex(1) != "", len(uuid.uuid4().hex))
    print("done")
`)
	if err != nil || !strings.Contains(got, "9:00:00\n") || !strings.HasSuffix(got, "done\n") {
		t.Errorf("%v:\n%s", err, got)
	}
}

func TestCodeOutputStaysInOrder(t *testing.T) {
	f := newFixture(t)
	got, err := runCode(t, f, "import sys\nprint('one')\nprint('two', file=sys.stderr)\nprint('three')")
	if err != nil || got != "one\ntwo\nthree\n" {
		t.Errorf("%q, %v", got, err)
	}
}

// keeper runs code the way a conversation does, carrying what's kept from call to call.
type keeper struct {
	f     *fixture
	kept  []byte
	files map[string][]byte
	saved int
}

func (k *keeper) run(code string, ctx context.Context) (string, error) {
	call := k.f.call(map[string]string{"language": "python", "code": code})
	call.Workspace, call.Files = k.kept, k.files
	call.Execution = store.NewID()
	call.SaveWorkspace = func(data []byte) error {
		k.kept = data
		k.saved++
		return nil
	}
	out, err := k.f.tools["run_code"].Run(ctx, call)
	return string(out), err
}

func TestCodeKeepsFilesInWork(t *testing.T) {
	f := newFixture(t)
	k := &keeper{f: f}
	if got, err := k.run("import os\nos.mkdir('dir')\nopen('dir/a.txt', 'w').write('kept')\nos.symlink('dir/a.txt', 'link')\nopen('/tmp/gone', 'w').write('gone')\nprint(os.getcwd())", context.Background()); err != nil || got != "/work\n" {
		t.Fatalf("%q, %v", got, err)
	}
	first := k.kept
	got, err := k.run("import os\nprint(open('dir/a.txt').read(), open('link').read(), os.path.exists('/tmp/gone'))", context.Background())
	if err != nil || got != "kept kept False\n" {
		t.Errorf("%q, %v", got, err)
	}
	// Nothing changed, so it's the same bytes, which the store only keeps once.
	if !bytes.Equal(first, k.kept) {
		t.Error("nothing changed, and what's kept did")
	}
	// A failing call keeps its files too.
	if _, err := k.run("open('more', 'w').write('x')\nraise SystemExit(4)", context.Background()); err == nil || !strings.Contains(err.Error(), "exit code 4") {
		t.Errorf("%v", err)
	}
	if got, _ := k.run("print(open('more').read())", context.Background()); got != "x\n" {
		t.Errorf("%q", got)
	}
}

// These need a delegated cgroup, like the ones in internal/cgroup, so they only run inside:
//
//	systemd-run --user --scope -p Delegate=yes env TACK_TEST_CGROUP=1 go test ./internal/tools -run Delegated
func delegated(t *testing.T) *keeper {
	t.Helper()
	if os.Getenv("TACK_TEST_CGROUP") == "" {
		t.Skip("needs a delegated cgroup")
	}
	groups, err := cgroup.Setup()
	if err != nil {
		t.Fatal(err)
	}
	f := newFixture(t)
	for _, tool := range All(Options{Protected: []string{f.key}, Cgroups: groups}) {
		f.tools[tool.Name] = tool
	}
	return &keeper{f: f}
}

func TestDelegatedCodeStoppedStillLeavesItsFiles(t *testing.T) {
	k := delegated(t)
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	if _, err := k.run("import time\nopen('stopped', 'w').write('y')\ntime.sleep(60)", ctx); err == nil || !strings.Contains(err.Error(), "stopped") {
		t.Errorf("%v", err)
	}
	if got, err := k.run("print(open('stopped').read())", context.Background()); got != "y\n" {
		t.Errorf("%q, %v", got, err)
	}
}

func TestDelegatedCodeLimits(t *testing.T) {
	k := delegated(t)
	// Memory: a list that wants far more than the limit. It says why it died.
	got, err := k.run("x = [bytes(1 << 20) for _ in range(4096)]\nfor b in x: pass\nx = [bytearray(b'y' * (1 << 20)) for _ in range(4096)]\nprint('ALLOCATED')", context.Background())
	if strings.Contains(got, "ALLOCATED") || err == nil || !strings.Contains(err.Error(), "memory limit") {
		t.Errorf("memory: %q, %v", got, err)
	}
	// Processes: a fork bomb stops at the limit, and only hurts itself.
	got, err = k.run("import os, time\ncount = 0\ntry:\n    while True:\n        if os.fork() == 0:\n            time.sleep(5)\n            os._exit(0)\n        count += 1\nexcept OSError as e:\n    print('stopped at', count, e.errno)\n    os._exit(0)", context.Background())
	if !strings.Contains(got, "stopped at") {
		t.Errorf("processes: %q, %v", got, err)
	}
	if strings.Contains(got, "stopped at 0") {
		t.Errorf("no process could start: %q", got)
	}
}

// What comes out is hostile, so tampering can only ever cost it its own files.
func TestCodeThatBreaksSavingLosesOnlyItsOwnFiles(t *testing.T) {
	f := newFixture(t)
	k := &keeper{f: f}
	if _, err := k.run("open('safe', 'w').write('safe')", context.Background()); err != nil {
		t.Fatal(err)
	}
	// Its parent is what saves, so kill that. Nothing comes out.
	got, err := k.run("import os, signal, time\nos.kill(os.getppid(), signal.SIGKILL)\ntime.sleep(0.2)\nprint('still here')", context.Background())
	if err == nil {
		t.Errorf("no complaint: %q", got)
	}
	if got, err := k.run("print(open('safe').read())", context.Background()); err != nil || got != "safe\n" {
		t.Errorf("%q, %v", got, err)
	}
	// An archive that climbs out has nowhere to go, because it's only unpacked in a sandbox.
	var buf bytes.Buffer
	zipped := gzip.NewWriter(&buf)
	archive := tar.NewWriter(zipped)
	body := []byte("escaped")
	escape := filepath.Join(filepath.Dir(f.workdir), "escaped")
	archive.WriteHeader(&tar.Header{Name: "../../../.." + escape, Mode: 0o644, Size: int64(len(body)), Typeflag: tar.TypeReg})
	archive.Write(body)
	archive.Close()
	zipped.Close()
	k.kept = buf.Bytes()
	got, err = k.run("print('ran')", context.Background())
	if err != nil || !strings.Contains(got, "couldn't restore") || !strings.Contains(got, "ran\n") {
		t.Errorf("%q, %v", got, err)
	}
	if _, err := os.Stat(escape); err == nil {
		t.Error("it got out")
	}
	if checkKept([]byte("not an archive")) == nil || checkKept(buf.Bytes()[:buf.Len()-10]) == nil {
		t.Error("a broken archive passed")
	}
}

func TestCodeCanReadTheConversationButNotChangeIt(t *testing.T) {
	f := newFixture(t)
	k := &keeper{f: f, files: map[string][]byte{"notes.json": []byte(`[{"kind":"plan","key":"a","text":"b"}]`), "scratch.json": []byte(`{"x":"y"}`)}}
	got, err := k.run(`
import json
print(json.load(open("/conversation/notes.json"))[0]["text"], json.load(open("/conversation/scratch.json"))["x"])
for path in ["/conversation/notes.json", "/conversation/new"]:
    try:
        open(path, "w")
        print("REACHED", path)
    except OSError:
        pass
`, context.Background())
	if err != nil || got != "b y\n" {
		t.Errorf("%q, %v", got, err)
	}
}

func TestBadCodeIsCaughtBeforeApproval(t *testing.T) {
	f := newFixture(t)
	if flags := f.inspect("run_code", map[string]string{"language": "python", "code": "print(1)"}); !slices.Equal(flags, []string{FlagCode}) {
		t.Errorf("flags = %v", flags)
	}
	for name, args := range map[string]map[string]string{
		"language": {"language": "ruby", "code": "puts 1"},
		"empty":    {"language": "python", "code": "  "},
		"nul":      {"language": "python", "code": "a\x00b"},
	} {
		if _, err := f.tools["run_code"].Inspect(f.call(args)); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
}

func TestCommandOutputIsCappedWhateverItsSize(t *testing.T) {
	f := newFixture(t)
	call := f.call(map[string]string{"command": "head -c 5000000 /dev/zero"})
	call.Settings.MaxReadBytes = 1000
	out, err := f.tools["run_command"].Run(context.Background(), call)
	if len(out) != 1000 || err == nil || !strings.Contains(err.Error(), "were dropped") {
		t.Errorf("capped = %d bytes, %v", len(out), err)
	}
}
