package tools

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"debug/elf"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/betawaffle/tack/internal/cgroup"
	"github.com/betawaffle/tack/internal/core"
)

// python is the interpreter for run_code. It's an absolute path, so it can't depend on
// any PATH.
const python = "/usr/bin/python3"

// None of these are settings, so nothing about a conversation can widen the sandbox.
const (
	codeTmpBytes  = 64 << 20 // /tmp, which is gone after each call
	codeWorkBytes = 16 << 20 // /work, which is kept
	codeShmBytes  = 16 << 20 // /dev/shm, which multiprocessing needs for its locks

	// What it can use, /tmp and /work included, since they're held in memory.
	codeMemoryBytes = 2 << 30
	codePids        = 256

	// codeSavedBytes is the most tack keeps of what's in /work. Compressed files in a full
	// /work come to less, but many small files take more room in an archive than on disk.
	codeSavedBytes = 32 << 20
	// codeUnpackedBytes stops tack reading something that unpacks to far more than it's stored as.
	codeUnpackedBytes = 256 << 20

	// codeAutoSendBytes is the most output that goes to the model without asking. It's a
	// few thousand tokens, and more than that is usually a mistake worth a look.
	codeAutoSendBytes = 8 << 10
)

const codeDescription = "Run code in a sandbox with no network, no input, no other programs and no access to the user's files. It starts in /work, whose files are kept for the next call in this conversation, up to 16 MB. /tmp is empty each time, and holds up to 64 MB. /conversation/notes.json and /conversation/scratch.json hold your notes and scratch as of the start of the call, for reading. It has Python's standard library and no other packages. It's limited to 2 GB of memory and 256 processes. The result is its output, and says if the exit code wasn't zero, the output was cut off, or the code was stopped. The user may send long output shortened.\n\nIt is completely safe to experiment with this on your own."

// File descriptors in the sandbox. bwrap consumes the first four itself.
const (
	fdFilter = 3 + iota
	fdNotes
	fdScratch
	fdCode
	fdKeptIn
	fdKeptOut
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
child = subprocess.Popen([sys.executable, "-I", "-u", "-B", "-X", "utf8", "/code/main.py"], stdin=subprocess.DEVNULL, cwd="/work")
child.wait()
def add(archive, path):
    info = archive.gettarinfo(path, arcname=os.path.relpath(path, "/work"))
    if info is None or not (info.isreg() or info.isdir() or info.issym()):
        return
    info.uid = info.gid = 0
    info.uname = info.gname = ""
    info.mtime = int(info.mtime)
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
func (r *reader) codeArgs(real string, dirs []string, files map[string]string) []string {
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
	// Date math needs time zones, and they're nothing private.
	args = append(args, "--ro-bind-try", "/usr/share/zoneinfo", "/usr/share/zoneinfo")
	for _, device := range []string{"/dev/null", "/dev/zero", "/dev/urandom"} {
		args = append(args, "--dev-bind", device, device)
	}
	args = append(args,
		"--size", strconv.Itoa(codeTmpBytes), "--tmpfs", "/tmp",
		"--size", strconv.Itoa(codeWorkBytes), "--tmpfs", "/work",
		"--size", strconv.Itoa(codeShmBytes), "--tmpfs", "/dev/shm",
		// A file rather than stdin, because multiprocessing has to be able to load it again.
		"--ro-bind-data", strconv.Itoa(fdCode), "/code/main.py",
		"--ro-bind-data", strconv.Itoa(fdNotes), "/conversation/notes.json",
		"--ro-bind-data", strconv.Itoa(fdScratch), "/conversation/scratch.json",
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

func inspectCode(call core.Call) ([]string, error) {
	args, err := arguments(call, "language", "code")
	if err != nil {
		return nil, err
	}
	if args["language"] != "python" {
		return nil, errors.New("the only language is python")
	}
	if strings.TrimSpace(args["code"]) == "" {
		return nil, errors.New("the code is empty")
	}
	if strings.ContainsRune(args["code"], 0) {
		return nil, errors.New("the code has a NUL in it")
	}
	for _, needed := range []string{bwrap, python} {
		if _, err := os.Stat(needed); err != nil {
			return nil, fmt.Errorf("running code needs %s, and it isn't there", needed)
		}
	}
	if _, err := codeFilter(); err != nil {
		return nil, err
	}
	return []string{FlagCode}, nil
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
	args, err := arguments(call, "language", "code")
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
	sandbox := r.codeArgs(real, dirs, files)
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
	for _, data := range [][]byte{filter, notes, scratch, []byte(args["code"]), call.Workspace} {
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
	kept := make(chan []byte, 1)
	go func() {
		data, _ := io.ReadAll(io.LimitReader(keptRead, codeSavedBytes+1))
		kept <- data
	}()

	cmd := exec.CommandContext(ctx, bwrap, append(sandbox, real, "-I", "-S", "-c", codeStart)...)
	// Somewhere that's always there. It's tack's side of things, which code never sees.
	cmd.Dir = "/"
	cmd.Env = codeEnv
	cmd.ExtraFiles = extra
	output, failure := r.runSandboxed(ctx, cmd, call, s, "code", &cgroup.Limits{Memory: codeMemoryBytes, Pids: codePids})

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
