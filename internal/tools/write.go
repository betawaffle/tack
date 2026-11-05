package tools

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/betawaffle/tack/internal/cgroup"
	"github.com/betawaffle/tack/internal/core"
	"github.com/betawaffle/tack/internal/store"
)

func required(call core.Call, names ...string) error {
	for _, name := range names {
		if _, ok := call.Args[name]; !ok {
			return fmt.Errorf("missing argument %q", name)
		}
	}
	return nil
}

func (r *reader) inspectEdit(call core.Call) ([]string, error) {
	args, err := arguments(call, "path", "old_string", "new_string")
	if err != nil {
		return nil, err
	}
	if err := required(call, "path", "old_string", "new_string"); err != nil {
		return nil, err
	}
	if args["old_string"] == args["new_string"] {
		return nil, errors.New("old_string and new_string are the same")
	}
	path, err := location(args["path"], call.Workdir, false)
	if err != nil {
		return nil, err
	}
	if args["old_string"] == "" {
		if _, err := os.Lstat(path); err == nil {
			return nil, fmt.Errorf("%s already exists, so old_string can't be empty", path)
		}
	}
	return r.flags(path, call.Workdir, true)
}

// editFile replaces one exact string, or creates a new file. The match is itself a
// check that the file is as the model believes. What was there is kept in the record
// so the edit can be undone, and the write is atomic, so a stop can't leave half a file.
func (r *reader) editFile(_ context.Context, call core.Call) ([]byte, error) {
	args, err := arguments(call, "path", "old_string", "new_string")
	if err != nil {
		return nil, err
	}
	path, err := location(args["path"], call.Workdir, false)
	if err != nil {
		return nil, err
	}
	if args["old_string"] == "" {
		return createFile(path, args["new_string"])
	}

	f, info, err := r.openRegular(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	before, err := readLimited(f, info.Size(), call.Settings.WithDefaults().MaxReadBytes)
	if err != nil {
		return nil, err
	}
	switch n := bytes.Count(before, []byte(args["old_string"])); n {
	case 0:
		return nil, errors.New("old_string doesn't appear in the file")
	case 1:
	default:
		return nil, fmt.Errorf("old_string appears %d times, so include more context to make it unique", n)
	}
	after := bytes.Replace(before, []byte(args["old_string"]), []byte(args["new_string"]), 1)

	// Writing through a symlink means replacing what it points at, not the link.
	target, err := filepath.EvalSymlinks(path)
	if err != nil {
		return nil, err
	}
	if now, err := os.Stat(target); err != nil || !os.SameFile(info, now) {
		return nil, errors.New("the file changed while it was being edited")
	}
	if call.Keep != nil {
		if err := call.Keep("before", before); err != nil {
			return nil, err
		}
	}
	if err := replaceFile(target, after, info.Mode().Perm()); err != nil {
		return nil, err
	}
	return fmt.Appendf(nil, "Edited %s: %d bytes before, %d after.\n", path, len(before), len(after)), nil
}

func readLimited(f *os.File, size int64, maxRead int) ([]byte, error) {
	if size > int64(maxRead) {
		return nil, fmt.Errorf("the file is larger than %d bytes", maxRead)
	}
	data := make([]byte, 0, size)
	buf := bytes.NewBuffer(data)
	if _, err := buf.ReadFrom(f); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func createFile(path, text string) ([]byte, error) {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, fmt.Errorf("the directory for %s doesn't exist", path)
		}
		return nil, err
	}
	defer f.Close()
	if _, err := f.WriteString(text); err != nil {
		return nil, err
	}
	if err := f.Sync(); err != nil {
		return nil, err
	}
	return fmt.Appendf(nil, "Created %s: %d bytes.\n", path, len(text)), nil
}

func replaceFile(path string, data []byte, perm fs.FileMode) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".tack-edit-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	defer tmp.Close()
	if _, err := tmp.Write(data); err != nil {
		return err
	}
	if err := tmp.Chmod(perm); err != nil {
		return err
	}
	if err := tmp.Sync(); err != nil {
		return err
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return err
	}
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}

func inspectCommand(call core.Call) ([]string, error) {
	args, err := arguments(call, "command")
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(args["command"]) == "" {
		return nil, errors.New("the command is empty")
	}
	if strings.ContainsRune(args["command"], 0) {
		return nil, errors.New("the command has a NUL in it")
	}
	if err := checkWorkdir(call.Workdir); err != nil {
		return nil, err
	}
	if _, err := os.Stat(bwrap); err != nil {
		return nil, fmt.Errorf("commands need bubblewrap, and %s isn't there", bwrap)
	}
	return []string{FlagCommand}, nil
}

// checkWorkdir matters more than it looks: a command with no directory set would
// run in tack's own working directory, and that must never happen.
func checkWorkdir(workdir string) error {
	if !filepath.IsAbs(workdir) {
		return errors.New("this conversation has no working directory, so it can't run commands")
	}
	info, err := os.Stat(workdir)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return fmt.Errorf("%s isn't a directory", workdir)
	}
	return nil
}

// capped keeps the start of what's written and counts the rest.
type capped struct {
	bytes.Buffer
	limit   int
	dropped int64
}

func (c *capped) Write(p []byte) (int, error) {
	room := max(c.limit-c.Len(), 0)
	if room >= len(p) {
		return c.Buffer.Write(p)
	}
	c.Buffer.Write(p[:room])
	c.dropped += int64(len(p) - room)
	return len(p), nil
}

// bwrap is the sandbox. It's an absolute path, so that it can't depend on any PATH.
const bwrap = "/usr/bin/bwrap"

// within reports whether path is dir or something under it.
func within(path, dir string) bool {
	rel, err := filepath.Rel(dir, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// sandboxArgs builds the bwrap command line. The command starts with nothing visible
// and gets only what's listed: read-only system paths, a writable working directory,
// and an empty /tmp and home. It has its own process tree, so nothing outlives it, and
// it can't make namespaces of its own. It shares the host's kernel, so this limits what
// it can reach, but a kernel exploit could get through.
func (r *reader) sandboxArgs(call core.Call, s store.Settings) []string {
	args := []string{"--unshare-all", "--unshare-user", "--disable-userns", "--die-with-parent", "--new-session"}
	if call.Network {
		args = append(args, "--share-net")
	}
	args = append(args, "--proc", "/proc", "--dev", "/dev", "--tmpfs", "/tmp")
	home := s.CommandEnv["HOME"]
	if filepath.IsAbs(home) {
		args = append(args, "--tmpfs", home)
	}
	for _, path := range s.SandboxReadOnly {
		args = append(args, "--ro-bind-try", path, path)
	}
	for _, path := range s.SandboxReadWrite {
		args = append(args, "--bind-try", path, path)
	}
	args = append(args, "--bind", call.Workdir, call.Workdir)
	for _, path := range s.SandboxProtected {
		inside := filepath.Join(call.Workdir, path)
		args = append(args, "--ro-bind-try", inside, inside)
	}
	// Listing a directory shouldn't be a way to expose the key inside it.
	for _, path := range r.protected {
		visible := within(path, call.Workdir) ||
			slices.ContainsFunc(s.SandboxReadOnly, func(dir string) bool { return within(path, dir) }) ||
			slices.ContainsFunc(s.SandboxReadWrite, func(dir string) bool { return within(path, dir) })
		if visible {
			args = append(args, "--ro-bind", "/dev/null", path)
		}
	}
	return append(args, "--chdir", call.Workdir)
}

// runCommand runs a shell string in the sandbox, in the conversation's working
// directory, with only the configured environment and no input.
func (r *reader) runCommand(ctx context.Context, call core.Call) ([]byte, error) {
	args, err := arguments(call, "command")
	if err != nil {
		return nil, err
	}
	if err := checkWorkdir(call.Workdir); err != nil {
		return nil, err
	}
	s := call.Settings.WithDefaults()
	sandbox := r.sandboxArgs(call, s)
	if call.Keep != nil {
		if err := call.Keep("sandbox", []byte(strings.Join(sandbox, "\n"))); err != nil {
			return nil, err
		}
	}
	output := capped{limit: s.MaxReadBytes}
	cmd := exec.CommandContext(ctx, bwrap, append(sandbox, "/bin/bash", "-c", args["command"])...)
	cmd.Dir = call.Workdir
	cmd.Env = []string{}
	for _, name := range slices.Sorted(maps.Keys(s.CommandEnv)) {
		cmd.Env = append(cmd.Env, name+"="+s.CommandEnv[name])
	}
	cmd.Stdout, cmd.Stderr = &output, &output
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	stop := func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	var group *cgroup.Group
	if r.cgroups != nil {
		id := call.Execution
		if id == "" {
			return nil, errors.New("a command needs an execution ID")
		}
		if group, err = r.cgroups.New(id); err != nil {
			return nil, fmt.Errorf("making a cgroup for this command: %w", err)
		}
		defer group.Close()
		cmd.SysProcAttr.UseCgroupFD = true
		cmd.SysProcAttr.CgroupFD = group.FD()
		stop = group.Kill
	}
	cmd.Cancel = stop
	cmd.WaitDelay = time.Duration(s.OutputWaitSeconds) * time.Second
	err = cmd.Run()
	var problems []string
	if ctx.Err() != nil {
		problems = append(problems, "the command was stopped: "+context.Cause(ctx).Error())
	} else if err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) && exit.ExitCode() >= 0 {
			problems = append(problems, fmt.Sprintf("exit code %d", exit.ExitCode()))
		} else {
			problems = append(problems, err.Error())
		}
	}
	if output.dropped > 0 {
		problems = append(problems, fmt.Sprintf("%d bytes of output past the first %d were dropped", output.dropped, s.MaxReadBytes))
	}
	if len(problems) > 0 {
		return output.Bytes(), errors.New(strings.Join(problems, "; "))
	}
	return output.Bytes(), nil
}
