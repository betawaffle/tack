package cgroup

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// These need a delegated cgroup, so they only run inside something like:
//
//	systemd-run --user --scope -p Delegate=yes env TACK_TEST_CGROUP=1 go test ./internal/cgroup
func manager(t *testing.T) *Manager {
	t.Helper()
	if os.Getenv("TACK_TEST_CGROUP") == "" {
		t.Skip("needs a delegated cgroup")
	}
	m, err := Setup()
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func TestBadExecutionIDs(t *testing.T) {
	m := &Manager{base: t.TempDir()}
	for _, id := range []string{"", "../x", "a/b", ".."} {
		if _, err := m.New(id); err == nil {
			t.Errorf("%q: no error", id)
		}
	}
}

func TestKillReachesProcessesThatLeftTheSession(t *testing.T) {
	m := manager(t)
	g, err := m.New("test-kill")
	if err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(t.TempDir(), "survived")
	cmd := exec.Command("/bin/bash", "-c", "setsid bash -c 'sleep 2; touch "+marker+"' & sleep 30")
	cmd.SysProcAttr = &syscall.SysProcAttr{UseCgroupFD: true, CgroupFD: g.FD()}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile("/proc/" + strconv.Itoa(cmd.Process.Pid) + "/cgroup")
	if err != nil || !strings.Contains(string(data), "exec-test-kill") {
		t.Errorf("started in %q, %v", data, err)
	}
	time.Sleep(200 * time.Millisecond)
	if err := g.Close(); err != nil {
		t.Fatal(err)
	}
	cmd.Wait()
	time.Sleep(2500 * time.Millisecond)
	if _, err := os.Stat(marker); err == nil {
		t.Error("a child survived")
	}
}
