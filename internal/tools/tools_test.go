package tools

import (
	"archive/tar"
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/betawaffle/tack/internal/cgroup"
	"github.com/betawaffle/tack/internal/core"
	"github.com/betawaffle/tack/internal/docs"
	"github.com/betawaffle/tack/internal/store"
)

type fixture struct {
	t       *testing.T
	workdir string
	key     string
	docs    string
	tools   map[string]core.Tool
}

func newFixture(t *testing.T) *fixture {
	base := t.TempDir()
	f := &fixture{t: t, workdir: filepath.Join(base, "work"), key: filepath.Join(base, "secret-key"), tools: map[string]core.Tool{}}
	if err := os.Mkdir(f.workdir, 0o755); err != nil {
		t.Fatal(err)
	}
	f.write(f.key, "the key", 0o600)
	f.docs = filepath.Join(base, "docs")
	f.write(filepath.Join(f.docs, "mine.md"), "Mine, see [[theirs]] on {{model}}.", 0o644)
	library := docs.Library{TackFS: fstest.MapFS{"theirs.md": {Data: []byte("Tack's.")}}, UserDir: f.docs}
	variables := func(s store.Settings) map[string]string { return map[string]string{"model": "the model"} }
	for _, tool := range All(Options{Protected: []string{f.key}, Docs: library, Variables: variables}) {
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
	return core.Call{Args: out, Settings: store.Settings{CommandTimeoutSeconds: 5}}
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
			definition = tool.Define(store.Settings{})
		}
		if err := json.Unmarshal(definition, &def); err != nil || def.Function.Name != name {
			t.Errorf("%s: %s, %v", name, definition, err)
		}
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

func TestCodeStillNeedsApprovalAndIsCapped(t *testing.T) {
	f := newFixture(t)
	if f.tools["run_code"].NoApproval {
		t.Error("it should still need approval")
	}
	got, err := runCode(t, f, "import sys\nsys.stdout.write('x' * (17 << 20))")
	if len(got) != outputBytes || err == nil || !strings.Contains(err.Error(), "were dropped") {
		t.Errorf("capped = %d bytes, %v", len(got), err)
	}
}

func TestMemoryIsWhatsSpareWithinBounds(t *testing.T) {
	var b budgets
	const gib = 1 << 30
	// A tenth of 125 GB is left, and the rest is spare.
	first, releaseFirst := b.memoryLimit(125*gib, 78*gib)
	if want := int64(78*gib - 125*gib/10); first != want {
		t.Errorf("first = %d, want %d", first, want)
	}
	// Another running at the same time gets as much: they can overcommit.
	second, releaseSecond := b.memoryLimit(125*gib, 78*gib)
	if second != first {
		t.Errorf("second = %d", second)
	}
	releaseFirst()
	releaseSecond()
	// The margin is at least 8 GB, and there's a most.
	if got, release := b.memoryLimit(16*gib, 12*gib); got != 4*gib {
		t.Errorf("small machine = %d", got)
	} else {
		release()
	}
	if got, release := b.memoryLimit(1024*gib, 1000*gib); got != codeMemoryMax {
		t.Errorf("big machine = %d", got)
	} else {
		release()
	}
}

func TestCodeCanReadTheDocsButNotChangeThem(t *testing.T) {
	f := newFixture(t)
	got, err := runCode(t, f, `
print(open("/docs/user/mine.md").read())
print(open("/docs/harness/theirs.md").read())
print("index" if "- [mine](user/mine.md)" in open("/docs/index.md").read() else "no index")
try:
    open("/docs/user/mine.md", "w")
except OSError as e:
    print("refused", e.errno)
`)
	if err != nil || got != "Mine, see [theirs](../harness/theirs.md) on the model.\nTack's.\nindex\nrefused 30\n" {
		t.Errorf("docs = %q, %v", got, err)
	}
	// Each run gets its own copy, and it's gone afterwards.
	if left, _ := filepath.Glob(filepath.Join(os.TempDir(), "tack-docs-*")); len(left) != 0 {
		t.Errorf("left behind: %v", left)
	}
}

func TestCodeLimitsHold(t *testing.T) {
	f := newFixture(t)
	// /work is small.
	got, err := runCode(t, f, "import os\ntry:\n    open('/work/big', 'wb').write(b'x' * (128 << 20))\n    print('WROTE')\nexcept OSError as e:\n    print('full', e.errno)\nos.remove('/work/big')")
	if err != nil || strings.Contains(got, "WROTE") {
		t.Errorf("big = %q, %v", got, err)
	}
	// /tmp and /dev/shm are as big as the memory limit they share, which it can read.
	got, err = runCode(t, f, "import json, os\nlimits = json.load(open('/sandbox/limits.json'))\nfor path in ['/tmp', '/dev/shm']:\n    s = os.statvfs(path)\n    print(path, s.f_blocks * s.f_frsize == limits['memory_bytes'])\nprint(limits['memory_bytes'] >= 2 << 30, limits['processes_and_threads'], limits['time_limit_seconds'])")
	if err != nil || got != "/tmp True\n/dev/shm True\nTrue 4096 5\n" {
		t.Errorf("limits = %q, %v", got, err)
	}
	// Stopping it ends everything it started.
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	call := f.call(map[string]string{"language": "python", "code": "import subprocess, sys, time\nsubprocess.Popen([sys.executable, '-c', 'import time; time.sleep(60)'])\ntime.sleep(60)"})
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
	f       *fixture
	kept    []byte
	files   map[string][]byte
	context []byte
	saved   int
}

func (k *keeper) run(code string, ctx context.Context) (string, error) {
	call := k.f.call(map[string]string{"language": "python", "code": code})
	call.Workspace, call.Files = k.kept, k.files
	if k.context != nil {
		call.Context = func() ([]byte, error) { return k.context, nil }
	}
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
	k := &keeper{f: f, files: map[string][]byte{"notes.json": []byte(`[{"kind":"plan","key":"a","text":"b"}]`), "scratch.json": []byte(`{"x":"y"}`)},
		context: []byte(`{"request":{"model":"m"},"reply":{"role":"assistant"}}`)}
	got, err := k.run(`
import json
context = json.load(open("/conversation/context.json"))
print(json.load(open("/conversation/notes.json"))[0]["text"], json.load(open("/conversation/scratch.json"))["x"], context["request"]["model"], context["reply"]["role"])
for path in ["/conversation/notes.json", "/conversation/context.json", "/conversation/new"]:
    try:
        open(path, "w")
        print("REACHED", path)
    except OSError:
        pass
`, context.Background())
	if err != nil || got != "b y m assistant\n" {
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

func TestCodeCanTalkToTackThroughItsSocket(t *testing.T) {
	f := newFixture(t)
	call := f.call(map[string]string{"language": "python", "code": `
import json, socket
s = socket.socket(fileno=10)
f = s.makefile("rwb")
for tool in ["scratch", "other"]:
    f.write(json.dumps({"tool": tool, "arguments": {"op": "list"}}).encode() + b"\n")
    f.flush()
    print(json.loads(f.readline()))
`})
	call.Execution = store.NewID()
	var seen []string
	call.Serve = func(_ context.Context, conn io.ReadWriter) error {
		r := bufio.NewReader(conn)
		for {
			line, err := r.ReadString('\n')
			if err != nil {
				return nil
			}
			seen = append(seen, strings.TrimSpace(line))
			fmt.Fprintf(conn, `{"result":"answer %d"}`+"\n", len(seen))
		}
	}
	out, err := f.tools["run_code"].Run(context.Background(), call)
	if err != nil || string(out) != "{'result': 'answer 1'}\n{'result': 'answer 2'}\n" {
		t.Errorf("out = %q, %v", out, err)
	}
	if len(seen) != 2 || seen[0] != `{"tool": "scratch", "arguments": {"op": "list"}}` {
		t.Errorf("seen = %q", seen)
	}
	// Without anything serving it, writing fails rather than hanging.
	call.Serve = nil
	call.Args["code"] = []byte(`"import os\ntry:\n    os.write(10, b'x\\n')\n    print(os.read(10, 10))\nexcept OSError as e:\n    print('closed', e.errno)"`)
	if out, err := f.tools["run_code"].Run(context.Background(), call); err != nil || !strings.Contains(string(out), "closed") && string(out) != "b''\n" {
		t.Errorf("unserved = %q, %v", out, err)
	}
}

func TestWhatCodeAsksForThatIsntThereGoesToAPerson(t *testing.T) {
	f := newFixture(t)
	have, err := PythonVersion(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	inspect := func(args map[string]any) error {
		t.Helper()
		raw := map[string]jsontext.Value{}
		for k, v := range args {
			data, _ := json.Marshal(v)
			raw[k] = data
		}
		_, err := f.tools["run_code"].Inspect(core.Call{Args: raw})
		return err
	}
	unsupported := func(err error) string {
		var u core.Unsupported
		if !errors.As(err, &u) {
			return ""
		}
		return u.Answer
	}
	if err := inspect(map[string]any{"language": "python", "code": "1", "version": have, "packages": map[string]string{}}); err != nil {
		t.Errorf("what's there: %v", err)
	}
	if got := unsupported(inspect(map[string]any{"language": "rust", "code": "fn main() {}"})); got != "This didn't run, because run_code doesn't run rust yet. It runs python." {
		t.Errorf("rust = %q", got)
	}
	if got := unsupported(inspect(map[string]any{"language": "python", "code": "1", "version": "<3"})); !strings.Contains(got, `no Python matching "<3". There's only Python `+have) {
		t.Errorf("old python = %q", got)
	}
	if got := unsupported(inspect(map[string]any{"language": "python", "code": "1", "packages": map[string]string{"numpy": ">=2", "attrs": "*"}})); got != "This didn't run, because these packages aren't available: attrs, numpy >=2. There's only Python's standard library." {
		t.Errorf("packages = %q", got)
	}
	// A constraint that can't be read is a mistake, not something missing.
	for _, args := range []map[string]any{
		{"language": "python", "code": "1", "version": "newest"},
		{"language": "python", "code": "1", "packages": map[string]string{"numpy": "lots"}},
		{"language": "python", "code": "1", "packages": []string{"numpy"}},
		{"language": "python"},
	} {
		if err := inspect(args); err == nil || unsupported(err) != "" {
			t.Errorf("%v = %v", args, err)
		}
	}
}

// TACK_TEST_PYTHON_SITE names a built environment, from pyenv, for the packages in the lock.
func TestCodeCanUseThePackages(t *testing.T) {
	site := os.Getenv("TACK_TEST_PYTHON_SITE")
	if site == "" {
		t.Skip("TACK_TEST_PYTHON_SITE isn't set")
	}
	f := newFixture(t)
	for _, tool := range All(Options{Protected: []string{f.key}, PythonSite: site, PythonFor: "3.14"}) {
		f.tools[tool.Name] = tool
	}
	got, err := runCode(t, f, `
import numpy as np, pandas as pd, threading
a = np.random.default_rng(1).random((300, 300))
print("inverse ok", np.allclose(a @ np.linalg.inv(a), np.eye(300)))
df = pd.DataFrame({"x": [1, 2, 3], "when": pd.to_datetime(["2026-01-01", "2026-06-01", "2026-11-08"]).tz_localize("Europe/London")})
print("sum", df.x.sum(), df.when.dt.month.tolist())
print("versions", np.__version__, pd.__version__)
print("threads", threading.active_count())
`)
	if err != nil || !strings.Contains(got, "inverse ok True") || !strings.Contains(got, "sum 6 [1, 6, 11]") || !strings.Contains(got, "versions 2.3.5 2.3.3") {
		t.Errorf("packages = %q, %v", got, err)
	}
	if !strings.Contains(f.tools["run_code"].Definition.String(), "numpy 2.3.5") {
		t.Error("the description doesn't list the packages")
	}
	// They can be asked for by version, and what isn't there goes to a person.
	inspect := func(packages map[string]string) error {
		data, _ := json.Marshal(packages)
		_, err := f.tools["run_code"].Inspect(core.Call{Args: map[string]jsontext.Value{"language": jsontext.Value(`"python"`), "code": jsontext.Value(`"1"`), "packages": data}})
		return err
	}
	if err := inspect(map[string]string{"numpy": ">=2.3,<3", "Pandas": "2.3.*", "python_dateutil": "*"}); err != nil {
		t.Errorf("what's there: %v", err)
	}
	if err := inspect(map[string]string{"pandas": ">=3"}); !errors.As(err, new(core.Unsupported)) || !strings.Contains(err.Error(), "pandas 2.3.3") {
		t.Errorf("pandas 3 = %v", err)
	}
}
