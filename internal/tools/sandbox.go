package tools

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/betawaffle/tack/internal/cgroup"
	"github.com/betawaffle/tack/internal/core"
	"github.com/betawaffle/tack/internal/store"
)

// outputBytes is the most of a run's output that's kept. The rest is counted.
const outputBytes = 16 << 20

// capped keeps the start of what's written and counts the rest. The buffer is a field
// rather than embedded, because a bytes.Buffer's ReadFrom would let io.Copy go straight
// past Write, and past the limit.
type capped struct {
	buffer  bytes.Buffer
	limit   int
	dropped int64
}

func (c *capped) Write(p []byte) (int, error) {
	room := max(c.limit-c.buffer.Len(), 0)
	if room >= len(p) {
		return c.buffer.Write(p)
	}
	c.buffer.Write(p[:room])
	c.dropped += int64(len(p) - room)
	return len(p), nil
}

func (c *capped) Bytes() []byte { return c.buffer.Bytes() }

// bwrap is the sandbox. It's an absolute path, so that it can't depend on any PATH.
const bwrap = "/usr/bin/bwrap"

// within reports whether path is dir or something under it.
func within(path, dir string) bool {
	rel, err := filepath.Rel(dir, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// runSandboxed runs a prepared bwrap command, keeping its output, stopping it if the
// context ends, and reporting whatever went wrong alongside what it printed.
func (r *reader) runSandboxed(ctx context.Context, cmd *exec.Cmd, call core.Call, s store.Settings, what string, limits *cgroup.Limits) ([]byte, error) {
	output := capped{limit: outputBytes}
	cmd.Stdout, cmd.Stderr = &output, &output
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	// Stopping prefers to be clean: a signal first, then a kill if that's ignored.
	terminate := func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM) }
	kill := func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	var group *cgroup.Group
	if r.cgroups != nil {
		id := call.Execution
		if id == "" {
			return nil, fmt.Errorf("a %s needs an execution ID", what)
		}
		var err error
		if group, err = r.cgroups.New(id); err != nil {
			return nil, fmt.Errorf("making a cgroup for this %s: %w", what, err)
		}
		defer group.Close()
		if limits != nil {
			if err := group.Limit(*limits); err != nil {
				return nil, fmt.Errorf("limiting this %s: %w", what, err)
			}
		}
		cmd.SysProcAttr.UseCgroupFD = true
		cmd.SysProcAttr.CgroupFD = group.FD()
		terminate = func() error { return group.Terminate("bwrap") }
		kill = group.Kill
	}
	grace := time.Duration(s.StopGraceSeconds) * time.Second
	var killer *time.Timer
	cmd.Cancel = func() error {
		killer = time.AfterFunc(grace, func() { kill() })
		return terminate()
	}
	cmd.WaitDelay = grace + time.Duration(s.OutputWaitSeconds)*time.Second
	err := cmd.Run()
	if killer != nil {
		killer.Stop()
	}
	var problems []string
	if ctx.Err() != nil {
		problems = append(problems, "the "+what+" was stopped: "+context.Cause(ctx).Error())
	} else if err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) && exit.ExitCode() >= 0 {
			problems = append(problems, fmt.Sprintf("exit code %d", exit.ExitCode()))
		} else {
			problems = append(problems, err.Error())
		}
	}
	if group != nil && limits != nil && group.OOMKills() > 0 {
		problems = append(problems, fmt.Sprintf("it ran out of its %d MB memory limit", limits.Memory>>20))
	}
	if output.dropped > 0 {
		problems = append(problems, fmt.Sprintf("%d bytes of output past the first %d were dropped", output.dropped, outputBytes))
	}
	if len(problems) > 0 {
		return output.Bytes(), errors.New(strings.Join(problems, "; "))
	}
	return output.Bytes(), nil
}
