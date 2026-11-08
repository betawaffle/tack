package tools

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"debug/elf"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/betawaffle/tack/internal/cgroup"
	"github.com/betawaffle/tack/internal/core"
	"github.com/betawaffle/tack/internal/store"
	"github.com/betawaffle/tack/internal/version"
)

// python is the interpreter for run_code. It's an absolute path, so it can't depend on
// any PATH.
const python = "/usr/bin/python3"

// None of these are settings, so nothing about a conversation can widen the sandbox. Its
// memory is worked out as it starts, in memory.go.
const (
	codeWorkBytes = 16 << 20 // /work, which is kept

	// Threads count as well as processes, and libraries start a thread for each CPU.
	codePids = 4096

	// codeSavedBytes is the most tack keeps of what's in /work. Compressed files in a full
	// /work come to less, but many small files take more room in an archive than on disk.
	codeSavedBytes = 32 << 20
	// codeUnpackedBytes stops tack reading something that unpacks to far more than it's stored as.
	codeUnpackedBytes = 256 << 20

	// codeAutoSendBytes is the most output that goes to the model without asking. It's a
	// few thousand tokens, and more than that is usually a mistake worth a look.
	codeAutoSendBytes = 8 << 10
)

var codeDescription = fmt.Sprintf("Run code in a sandbox with no network, no input, no other programs and no access to the user's files. It starts in /work, whose files are kept for the next call in this conversation, up to %d MB. /tmp is empty each time. /conversation/notes.json and /conversation/scratch.json hold your notes and scratch as of the start of the call, and /conversation/context.json holds the request that led to this call, exactly as it was sent, under \"request\", and the reply with this call in it under \"reply\", all for reading. The process running your code has file descriptor 10 open, a socket to the harness; there's no /proc or /dev/fd, so use it by number, as socket.socket(fileno=10). Write {\"tool\": name, \"arguments\": object} on one line to call any tool you've been offered except run_code, and read one line back, {\"result\": text} or {\"error\": text}. Some tools wait for the user to answer, and that time doesn't count against the time limit. Lines can be up to %d KB, and a run can make up to %d calls. It has Python's standard library and no other packages. Its memory limit is set as it starts, from what the machine has free, between %d and %d GB, and files in /tmp and /dev/shm count against it. /sandbox/limits.json has this run's limits. The result is its output, and says if the exit code wasn't zero, the output was cut off, or the code was stopped. The user may send long output shortened.\n\nIt is completely safe to experiment with this on your own.", codeWorkBytes>>20, core.ChannelLineBytes>>10, core.ChannelCalls, codeMemoryMin>>30, codeMemoryMax>>30)

// File descriptors in the sandbox. bwrap consumes the first four itself.
const (
	fdFilter = 3 + iota
	fdNotes
	fdScratch
	fdCode
	fdKeptIn
	fdKeptOut
	fdContext
	fdChannel // what the model's code talks to tack through
	fdLimits
)

// codeStart runs in the sandbox around the model's code, which gets its own process and can't
// reach these descriptors. -u keeps stdout and stderr in order. The code can still kill this
// process, and then nothing's saved. What comes out
// is treated as hostile: tack only checks it's whole and stores it, and it's only ever unpacked
// in here again. It ignores a request to stop so that the files can be saved once the code has.
const codeStart = `
import gzip, io, os, signal, stat, subprocess, sys, tarfile
signal.signal(signal.SIGTERM, lambda *args: None)
try:
    with os.fdopen(7, "rb") as f:
        kept = f.read()
    if kept:
        with tarfile.open(fileobj=io.BytesIO(kept), mode="r:gz") as archive:
            archive.extractall("/work", filter="data")
except Exception as e:
    print("tack couldn't restore the files in /work:", e, file=sys.stderr, flush=True)
try:
    channel = (os.dup2(10, 10),)
except OSError:
    channel = ()
child = subprocess.Popen([sys.executable, "-I", "-u", "-B", "-X", "utf8", "/code/main.py"], stdin=subprocess.DEVNULL, cwd="/work", pass_fds=channel)
for fd in channel:
    os.close(fd)
child.wait()
def add(archive, path):
    info = archive.gettarinfo(path, arcname=os.path.relpath(path, "/work"))
    if info is None or not (info.isreg() or info.isdir() or info.issym()):
        return
    info.uid = info.gid = 0
    info.uname = info.gname = ""
    # Unpacking can't set a link's time, so keeping one would change the archive for nothing.
    info.mtime = 0 if info.issym() else int(info.mtime)
    if info.isreg():
        with open(path, "rb") as f:
            archive.addfile(info, f)
    else:
        archive.addfile(info)
try:
    with os.fdopen(8, "wb") as out, gzip.GzipFile(filename="", fileobj=out, mode="wb", mtime=0) as packed:
        with tarfile.open(fileobj=packed, mode="w|", format=tarfile.GNU_FORMAT) as archive:
            for top, dirs, files in os.walk("/work"):
                dirs.sort()
                for name in dirs + sorted(files):
                    add(archive, os.path.join(top, name))
except Exception:
    pass
code = child.returncode
sys.exit(128 - code if code < 0 else code)
`

// libraryDirs is where libraries are looked for, in the dynamic linker's usual order. The
// sandbox has no ld.so.cache, so each one is put where the linker looks by default.
var libraryDirs = []string{"/lib64", "/usr/lib64", "/lib/x86_64-linux-gnu", "/usr/lib/x86_64-linux-gnu", "/lib", "/usr/lib"}

// pythonParts finds the least of the machine that Python's standard library runs with: the
// interpreter, its directories, and the libraries those load. Nothing is cached, because a
// system update can change any of it while tack is running.
func pythonParts(ctx context.Context) (real string, dirs []string, files map[string]string, err error) {
	if real, err = filepath.EvalSymlinks(python); err != nil {
		return "", nil, nil, err
	}
	// -I and -S leave just the standard library's own directories. It's a fixed program,
	// run by tack on tack's side, with nothing from a model in it.
	cmd := exec.CommandContext(ctx, real, "-I", "-S", "-c", `import sys; print("\n".join(sys.path))`)
	cmd.Dir, cmd.Env = "/", []string{}
	out, err := cmd.Output()
	if err != nil {
		return "", nil, nil, fmt.Errorf("asking python where its library is: %w", err)
	}
	var todo []string
	todo = append(todo, real)
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if info, err := os.Stat(line); err == nil && info.IsDir() {
			dirs = append(dirs, line)
			found, _ := filepath.Glob(filepath.Join(line, "*.so"))
			todo = append(todo, found...)
		}
	}
	if len(dirs) == 0 {
		return "", nil, nil, errors.New("python didn't name any library directories")
	}
	files = map[string]string{}
	// glibc loads this itself when a thread ends, so no file names it.
	need := func(name string) error {
		for _, dir := range libraryDirs {
			path := filepath.Join(dir, name)
			source, err := filepath.EvalSymlinks(path)
			if err != nil {
				continue
			}
			if _, seen := files[path]; !seen {
				files[path] = source
				todo = append(todo, source)
			}
			return nil
		}
		return fmt.Errorf("couldn't find %s", name)
	}
	if err := need("libgcc_s.so.1"); err != nil {
		return "", nil, nil, err
	}
	seen := map[string]bool{}
	for len(todo) > 0 {
		path := todo[0]
		todo = todo[1:]
		if seen[path] {
			continue
		}
		seen[path] = true
		f, err := elf.Open(path)
		if err != nil {
			return "", nil, nil, fmt.Errorf("reading %s: %w", path, err)
		}
		libs, err := f.ImportedLibraries()
		var interp string
		for _, prog := range f.Progs {
			if prog.Type == elf.PT_INTERP {
				data, _ := io.ReadAll(prog.Open())
				interp = strings.TrimRight(string(data), "\x00")
			}
		}
		f.Close()
		if err != nil {
			return "", nil, nil, fmt.Errorf("reading %s: %w", path, err)
		}
		if interp != "" {
			source, err := filepath.EvalSymlinks(interp)
			if err != nil {
				return "", nil, nil, err
			}
			files[interp] = source
		}
		for _, lib := range libs {
			if err := need(lib); err != nil {
				return "", nil, nil, fmt.Errorf("%s: %w", path, err)
			}
		}
	}
	return real, dirs, files, nil
}

// codeArgs is the sandbox for run_code: Python and what it loads, read-only, with a few small
// places in memory to write to. The rest of the tree is read-only and empty. There's no /proc,
// which also hides the arguments of the sandbox's own processes, and no real /dev. It shares
// the host's kernel, which is why there's a syscall filter as well.
func (r *reader) codeArgs(real string, dirs []string, files map[string]string, memory int64, docsDir string) []string {
	args := []string{"--unshare-all", "--unshare-user", "--disable-userns", "--die-with-parent", "--new-session",
		"--hostname", "sandbox", "--ro-bind", real, real}
	for _, path := range slices.Sorted(mapKeys(files)) {
		args = append(args, "--ro-bind", files[path], path)
	}
	for _, dir := range dirs {
		args = append(args, "--ro-bind", dir, dir)
		// Whatever's been installed on this machine isn't part of the standard library.
		packages := filepath.Join(dir, "site-packages")
		if info, err := os.Stat(packages); err == nil && info.IsDir() {
			args = append(args, "--size", "4096", "--tmpfs", packages, "--remount-ro", packages)
		}
		for _, path := range r.protected {
			if within(path, dir) {
				args = append(args, "--ro-bind", "/dev/null", path)
			}
		}
	}
	if docsDir != "" {
		args = append(args, "--ro-bind", docsDir, DocsDir)
	}
	// Date math needs time zones, and they're nothing private.
	args = append(args, "--ro-bind-try", "/usr/share/zoneinfo", "/usr/share/zoneinfo")
	for _, device := range []string{"/dev/null", "/dev/zero", "/dev/urandom"} {
		args = append(args, "--dev-bind", device, device)
	}
	args = append(args,
		// These two share the memory limit with everything else, so each can be as big.
		"--size", strconv.FormatInt(memory, 10), "--tmpfs", "/tmp",
		"--size", strconv.FormatInt(memory, 10), "--tmpfs", "/dev/shm",
		"--size", strconv.Itoa(codeWorkBytes), "--tmpfs", "/work",
		// A file rather than stdin, because multiprocessing has to be able to load it again.
		"--ro-bind-data", strconv.Itoa(fdCode), "/code/main.py",
		"--ro-bind-data", strconv.Itoa(fdNotes), "/conversation/notes.json",
		"--ro-bind-data", strconv.Itoa(fdScratch), "/conversation/scratch.json",
		"--ro-bind-data", strconv.Itoa(fdContext), "/conversation/context.json",
		"--ro-bind-data", strconv.Itoa(fdLimits), "/sandbox/limits.json",
		"--seccomp", strconv.Itoa(fdFilter),
		// Last, because the root is an unlimited place to write until it's read-only.
		"--remount-ro", "/",
		"--chdir", "/work")
	return args
}

func mapKeys(m map[string]string) func(func(string) bool) {
	return func(yield func(string) bool) {
		for key := range m {
			if !yield(key) {
				return
			}
		}
	}
}

// codeEnv is the whole environment for code. It's fixed, because a config's environment for
// commands can have anything in it. Python's -I ignores its own variables, so the settings it
// needs are flags on the command line instead.
var codeEnv = []string{"PATH=/usr/bin", "HOME=/work", "LANG=C.UTF-8"}

// codeProperties are run_code's parameters. Which languages it lists is up to the person
// offering it, who can add ones tack doesn't run, or take the list away, to see what a model
// asks for. Whatever it asks for that tack doesn't have goes to them to answer.
const codeProperties = `{` +
	`"language":{"type":"string","enum":["python"],"description":"The language it's written in."},` +
	`"code":{"type":"string","description":"The code to run."},` +
	`"version":{"type":"string","description":"Which versions of the language will do, as a constraint like \"3.14\", \">=3.12,<3.15\" or \"^3.12\". Any will do without it."},` +
	`"packages":{"type":"object","additionalProperties":{"type":"string"},"description":"Packages the code needs beyond the language's standard library, each name with a version constraint, or \"*\" for any version."}` +
	`}`

// codeRequest is what a call to run_code asks for.
type codeRequest struct {
	language, code, version string
	packages                map[string]string
}

func codeArguments(call core.Call) (codeRequest, error) {
	var req codeRequest
	for name, value := range call.Args {
		var err error
		switch name {
		case "language":
			err = json.Unmarshal(value, &req.language)
		case "code":
			err = json.Unmarshal(value, &req.code)
		case "version":
			err = json.Unmarshal(value, &req.version)
		case "packages":
			err = json.Unmarshal(value, &req.packages)
		default:
			return req, fmt.Errorf("unexpected argument %q", name)
		}
		if err != nil {
			return req, fmt.Errorf("%s: %w", name, err)
		}
	}
	if _, ok := call.Args["language"]; !ok {
		return req, errors.New("there's no language")
	}
	if _, ok := call.Args["code"]; !ok {
		return req, errors.New("there's no code")
	}
	return req, nil
}

// inspectCode checks a call, and says when it asks for something tack doesn't have, which a
// person then answers.
func inspectCode(call core.Call) ([]string, error) {
	req, err := codeArguments(call)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(req.code) == "" {
		return nil, errors.New("the code is empty")
	}
	if strings.ContainsRune(req.code, 0) {
		return nil, errors.New("the code has a NUL in it")
	}
	constraint, err := version.ParseConstraint(req.version)
	if err != nil {
		return nil, fmt.Errorf("version: %w", err)
	}
	names := slices.Sorted(maps.Keys(req.packages))
	for _, name := range names {
		if name == "" {
			return nil, errors.New("a package has no name")
		}
		if _, err := version.ParseConstraint(req.packages[name]); err != nil {
			return nil, fmt.Errorf("packages: %s: %w", name, err)
		}
	}
	if req.language != "python" {
		return nil, core.Unsupported{Answer: fmt.Sprintf("This didn't run, because run_code doesn't run %s yet. It runs python.", req.language)}
	}
	if req.version != "" {
		have, err := PythonVersion(context.Background())
		if err != nil {
			return nil, err
		}
		v, err := version.Parse(have)
		if err != nil {
			return nil, err
		}
		if !constraint.Allows(v) {
			return nil, core.Unsupported{Answer: fmt.Sprintf("This didn't run, because there's no Python matching %q. There's only Python %s.", req.version, have)}
		}
	}
	if len(names) > 0 {
		var asked []string
		for _, name := range names {
			if c := req.packages[name]; c != "" && c != "*" {
				asked = append(asked, fmt.Sprintf("%s %s", name, c))
			} else {
				asked = append(asked, name)
			}
		}
		return nil, core.Unsupported{Answer: fmt.Sprintf("This didn't run, because these packages aren't available: %s. There's only Python's standard library.", strings.Join(asked, ", "))}
	}
	for _, needed := range []string{bwrap, choom, python} {
		if _, err := os.Stat(needed); err != nil {
			return nil, fmt.Errorf("running code needs %s, and it isn't there", needed)
		}
	}
	if _, err := codeFilter(); err != nil {
		return nil, err
	}
	return []string{FlagCode}, nil
}

// writeDocs lays out the documentation as the model reads it, in a new directory on tack's
// side that's mounted read-only. It's written fresh for each run, so it's always current.
func (r *reader) writeDocs(settings store.Settings) (string, error) {
	if r.docs.TackFS == nil && r.docs.UserDir == "" {
		return "", nil
	}
	dir, err := os.MkdirTemp("", "tack-docs-")
	if err != nil {
		return "", err
	}
	variables := map[string]string{}
	if r.variables != nil {
		variables = r.variables(settings)
	}
	if err := r.docs.WriteFor(dir, variables); err != nil {
		os.RemoveAll(dir)
		return "", err
	}
	return dir, nil
}

// codeLimits is /sandbox/limits.json, what this run was given. Memory covers /tmp and
// /dev/shm too, which are held in it.
type codeLimits struct {
	Memory       int64 `json:"memory_bytes"`
	Work         int   `json:"work_bytes"`
	Saved        int   `json:"work_saved_bytes"`
	Pids         int   `json:"processes_and_threads"`
	Seconds      int   `json:"time_limit_seconds"`
	ChannelLine  int   `json:"channel_line_bytes"`
	ChannelCalls int   `json:"channel_calls"`
	ChannelBytes int   `json:"channel_bytes"`
}

// feed starts writing data to a pipe, for the sandbox to read, and returns the end to give it.
func feed(data []byte) (*os.File, error) {
	read, write, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	go func() {
		// It fails if nothing reads it, and then there's nothing more to do.
		write.Write(data)
		write.Close()
	}()
	return read, nil
}

// checkKept says whether what code left is a whole archive. It reads it, and that's all: it's
// hostile, so nothing in it ever touches this machine's files.
func checkKept(data []byte) error {
	packed, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		return err
	}
	archive := tar.NewReader(packed)
	total := int64(0)
	for {
		if _, err := archive.Next(); errors.Is(err, io.EOF) {
			break
		} else if err != nil {
			return err
		}
		n, err := io.Copy(io.Discard, io.LimitReader(archive, codeUnpackedBytes-total+1))
		if err != nil {
			return err
		}
		if total += n; total > codeUnpackedBytes {
			return errors.New("it's too big")
		}
	}
	// Reading to the end checks the checksum.
	_, err = io.Copy(io.Discard, io.LimitReader(packed, 1<<20))
	return err
}

// runCode runs code in its own sandbox. The code goes in as a file, so it can be longer than a
// command line, and it has no input of its own.
func (r *reader) runCode(ctx context.Context, call core.Call) ([]byte, error) {
	if _, err := inspectCode(call); err != nil {
		return nil, err
	}
	req, err := codeArguments(call)
	if err != nil {
		return nil, err
	}
	s := call.Settings.WithDefaults()
	filter, err := codeFilter()
	if err != nil {
		return nil, err
	}
	real, dirs, files, err := pythonParts(ctx)
	if err != nil {
		return nil, err
	}
	total, available, err := meminfo()
	if err != nil {
		return nil, err
	}
	memory, release := r.memory.memoryLimit(total, available)
	defer release()
	docsDir, err := r.writeDocs(call.Settings)
	if err != nil {
		return nil, fmt.Errorf("laying out the documentation: %w", err)
	}
	if docsDir != "" {
		defer os.RemoveAll(docsDir)
	}
	sandbox := r.codeArgs(real, dirs, files, memory, docsDir)
	if call.Keep != nil {
		if err := call.Keep("sandbox", []byte(strings.Join(sandbox, "\n"))); err != nil {
			return nil, err
		}
	}
	notes, scratch := call.Files["notes.json"], call.Files["scratch.json"]
	if notes == nil {
		notes = []byte("[]")
	}
	if scratch == nil {
		scratch = []byte("{}")
	}
	var extra []*os.File
	defer func() {
		for _, f := range extra {
			f.Close()
		}
	}()
	for _, data := range [][]byte{filter, notes, scratch, []byte(req.code), call.Workspace} {
		in, err := feed(data)
		if err != nil {
			return nil, err
		}
		extra = append(extra, in)
	}
	keptRead, keptWrite, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	defer keptRead.Close()
	extra = append(extra, keptWrite)
	conversation := []byte(`{"request":null,"reply":null}`)
	if call.Context != nil {
		if conversation, err = call.Context(); err != nil {
			return nil, err
		}
	}
	in, err := feed(conversation)
	if err != nil {
		return nil, err
	}
	extra = append(extra, in)
	// A pair of connected sockets: tack's end can be closed under a reader, and the code's
	// end stays blocking, the way code expects a socket to be.
	pair, err := syscall.Socketpair(syscall.AF_UNIX, syscall.SOCK_STREAM|syscall.SOCK_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	if err := syscall.SetNonblock(pair[0], true); err != nil {
		syscall.Close(pair[0])
		syscall.Close(pair[1])
		return nil, err
	}
	tackEnd, codeEnd := os.NewFile(uintptr(pair[0]), "channel"), os.NewFile(uintptr(pair[1]), "channel for code")
	defer tackEnd.Close()
	extra = append(extra, codeEnd)
	limits, err := json.Marshal(codeLimits{Memory: memory, Work: codeWorkBytes, Saved: codeSavedBytes, Pids: codePids,
		Seconds: s.CommandTimeoutSeconds, ChannelLine: core.ChannelLineBytes, ChannelCalls: core.ChannelCalls, ChannelBytes: core.ChannelBytes}, json.Deterministic(true))
	if err != nil {
		return nil, err
	}
	if in, err = feed(limits); err != nil {
		return nil, err
	}
	extra = append(extra, in)
	serveCtx, stopServing := context.WithCancel(ctx)
	served := make(chan error, 1)
	go func() {
		if call.Serve == nil {
			// Nothing answers, so the code finds it closed rather than waiting.
			tackEnd.Close()
			served <- nil
			return
		}
		err := call.Serve(serveCtx, tackEnd)
		// Once tack stops answering, as after a limit, the code finds it closed.
		tackEnd.Close()
		served <- err
	}()
	kept := make(chan []byte, 1)
	go func() {
		data, _ := io.ReadAll(io.LimitReader(keptRead, codeSavedBytes+1))
		kept <- data
	}()

	// choom makes it the first thing the kernel kills if the whole machine runs short,
	// and then runs bwrap in its place.
	cmd := exec.CommandContext(ctx, choom, append([]string{"-n", "1000", "--", bwrap}, append(sandbox, real, "-I", "-S", "-c", codeStart)...)...)
	// Somewhere that's always there. It's tack's side of things, which code never sees.
	cmd.Dir = "/"
	cmd.Env = codeEnv
	cmd.ExtraFiles = extra
	output, failure := r.runSandboxed(ctx, cmd, call, s, "code", &cgroup.Limits{Memory: memory, Pids: codePids})
	stopServing()
	tackEnd.Close()
	if err := <-served; err != nil {
		failure = errors.Join(failure, fmt.Errorf("tack's side of the channel failed: %w", err))
	}

	keptWrite.Close()
	// With the sandbox gone nothing else can be holding the other end, but this is what's
	// going to wait on it, so it gets a limit anyway.
	keptRead.SetReadDeadline(time.Now().Add(5 * time.Second))
	data := <-kept
	if err := r.saveKept(call, data); err != nil {
		failure = errors.Join(failure, fmt.Errorf("the files in /work weren't kept, so they're as they were before this call: %w", err))
	}
	if failure != nil {
		return output, errors.New(strings.ReplaceAll(failure.Error(), "\n", "; "))
	}
	return output, nil
}

func (r *reader) saveKept(call core.Call, data []byte) error {
	if len(data) > codeSavedBytes {
		return fmt.Errorf("they'd take more than %d MB to keep", codeSavedBytes>>20)
	}
	if err := checkKept(data); err != nil {
		return err
	}
	if call.SaveWorkspace == nil {
		return nil
	}
	return call.SaveWorkspace(data)
}
