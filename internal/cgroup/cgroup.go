// Package cgroup gives each execution its own cgroup under one that systemd delegated
// to tack (Delegate=yes). That makes stopping a command reliable, whatever its children
// do, and leaves room for resource limits.
package cgroup

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const root = "/sys/fs/cgroup"

// Manager makes cgroups under the one tack was given.
type Manager struct {
	base string
}

// Setup takes over the cgroup tack is running in. A cgroup can't both hold processes
// and have children with controllers enabled, so tack moves itself into a child. It
// fails if the cgroup isn't writable, which means it wasn't delegated.
func Setup() (*Manager, error) {
	data, err := os.ReadFile("/proc/self/cgroup")
	if err != nil {
		return nil, err
	}
	rel, ok := strings.CutPrefix(strings.TrimSpace(string(data)), "0::")
	if !ok {
		return nil, errors.New("not on cgroup v2")
	}
	return setupAt(filepath.Join(root, rel))
}

func setupAt(base string) (*Manager, error) {
	main := filepath.Join(base, "tack")
	if err := os.Mkdir(main, 0o755); err != nil && !errors.Is(err, os.ErrExist) {
		return nil, err
	}
	procs := []byte(strconv.Itoa(os.Getpid()))
	if err := os.WriteFile(filepath.Join(main, "cgroup.procs"), procs, 0o644); err != nil {
		return nil, err
	}
	// Limits come later. Failing to enable a controller shouldn't stop anything now.
	os.WriteFile(filepath.Join(base, "cgroup.subtree_control"), []byte("+memory +pids"), 0o644)
	return &Manager{base: base}, nil
}

// Group is one execution's cgroup.
type Group struct {
	path string
	dir  *os.File
}

// New makes a cgroup for an execution.
func (m *Manager) New(execution string) (*Group, error) {
	if execution == "" || strings.ContainsAny(execution, "/.") {
		return nil, fmt.Errorf("bad execution ID %q", execution)
	}
	path := filepath.Join(m.base, "exec-"+execution)
	if err := os.Mkdir(path, 0o755); err != nil {
		return nil, err
	}
	dir, err := os.Open(path)
	if err != nil {
		os.Remove(path)
		return nil, err
	}
	return &Group{path: path, dir: dir}, nil
}

// FD is for starting a process directly inside the cgroup, so that there's no moment
// when it's outside.
func (g *Group) FD() int { return int(g.dir.Fd()) }

// Kill ends every process in the cgroup, including any that started their own session.
func (g *Group) Kill() error {
	return os.WriteFile(filepath.Join(g.path, "cgroup.kill"), []byte("1"), 0o644)
}

// Terminate asks every process to stop, so they can finish cleanly. skip names programs
// to leave alone, like the sandbox's own processes, which end when what's in them does.
func (g *Group) Terminate(skip ...string) error {
	data, err := os.ReadFile(filepath.Join(g.path, "cgroup.procs"))
	if err != nil {
		return err
	}
	for _, field := range strings.Fields(string(data)) {
		pid, err := strconv.Atoi(field)
		if err != nil {
			continue
		}
		comm, err := os.ReadFile("/proc/" + field + "/comm")
		if err != nil || slices.Contains(skip, strings.TrimSpace(string(comm))) {
			continue
		}
		syscall.Kill(pid, syscall.SIGTERM)
	}
	return nil
}

// Close kills whatever is left and removes the cgroup. The kernel takes a moment to
// finish killing, and a cgroup with processes can't be removed.
func (g *Group) Close() error {
	defer g.dir.Close()
	killed := g.Kill()
	deadline := time.Now().Add(2 * time.Second)
	for {
		err := os.Remove(g.path)
		if err == nil {
			return killed
		}
		if time.Now().After(deadline) {
			return errors.Join(killed, err)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
