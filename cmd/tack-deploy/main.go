// Command tack-deploy installs the committed code and has the running tack restart into
// it at a clean stopping point. Run it from the repository with go run ./cmd/tack-deploy.
package main

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/betawaffle/tack/internal/config"
	"github.com/betawaffle/tack/internal/pyenv"
)

// A restart with nothing to wait for takes a couple of seconds. This is how long to
// watch for one before calling it stuck.
const startTimeout = 30 * time.Second

type status struct {
	Version string     `json:"version"`
	Waiting []struct{} `json:"waiting"`
}

func main() {
	if err := deploy(); err != nil {
		fmt.Fprintln(os.Stderr, "tack-deploy:", err)
		os.Exit(1)
	}
}

func deploy() error {
	root, err := output("git", "rev-parse", "--show-toplevel")
	if err != nil {
		return err
	}
	if err := os.Chdir(root); err != nil {
		return err
	}
	dirty, err := output("git", "status", "--porcelain")
	if err != nil {
		return err
	}
	if dirty != "" {
		return errors.New("there are uncommitted changes, which wouldn't be in a revision anyone can find later")
	}
	head, err := output("git", "rev-parse", "HEAD")
	if err != nil {
		return err
	}
	path, err := config.DefaultPath()
	if err != nil {
		return err
	}
	cfg, err := config.Load(path, false)
	if err != nil {
		return err
	}
	_, port, err := net.SplitHostPort(cfg.Addr)
	if err != nil {
		return err
	}
	base := "http://localhost:" + port
	running, err := fetchStatus(base)
	if err != nil {
		return fmt.Errorf("asking the running tack: %w", err)
	}
	if running.Version == head {
		fmt.Println("already running", head[:7])
		return nil
	}

	if err := run("go", "test", "./..."); err != nil {
		return err
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	bin := filepath.Join(home, ".local", "bin")
	// In the same directory, so that moving them into place is atomic.
	newTack, newShow := filepath.Join(bin, ".tack.new"), filepath.Join(bin, ".tack-show.new")
	defer os.Remove(newTack)
	defer os.Remove(newShow)
	if err := run("go", "build", "-o", newTack, "."); err != nil {
		return err
	}
	if err := run("go", "build", "-o", newShow, "./cmd/tack-show"); err != nil {
		return err
	}
	// The packages code can import, built from the lock this build has. They're downloaded,
	// checked against their hashes and unpacked; nothing in them runs here.
	lock := pyenv.Default()
	if err := pyenv.Build(context.Background(), &http.Client{Timeout: 15 * time.Minute}, lock, filepath.Join(cfg.Data, "pyenv")); err != nil {
		return fmt.Errorf("building the packages for code, so nothing changed: %w", err)
	}
	if err := run(newTack, "-check"); err != nil {
		return fmt.Errorf("the new build can't start, so nothing changed: %w", err)
	}

	tack := filepath.Join(bin, "tack")
	previous := tack + ".previous"
	os.Remove(previous)
	if err := os.Link(tack, previous); err != nil {
		return err
	}
	if err := os.Rename(newTack, tack); err != nil {
		return err
	}
	if err := os.Rename(newShow, filepath.Join(bin, "tack-show")); err != nil {
		return err
	}

	if err := installUnit(); err != nil {
		return err
	}

	// The range fails if the running version isn't a commit here, or was built dirty.
	// That only costs the list on the page.
	changes, _ := output("git", "log", "--reverse", "--format=%s", strings.TrimSuffix(running.Version, "+dirty")+"..HEAD")
	note := filepath.Join(cfg.Data, "restart-note")
	if err := os.WriteFile(note, []byte(changes+"\n"), 0o600); err != nil {
		return err
	}
	// Only the main process: commands tack is running are in the same cgroup.
	if err := run("systemctl", "--user", "kill", "--kill-whom=main", "--signal=SIGUSR1", "tack"); err != nil {
		return err
	}
	return watch(base, head)
}

// watch reports how it went, as far as that's known soon. If something's running, tack
// will restart when it's done, however long that takes, so there's no point waiting.
func watch(base, head string) error {
	deadline := time.Now().Add(startTimeout)
	for time.Now().Before(deadline) {
		time.Sleep(500 * time.Millisecond)
		s, err := fetchStatus(base)
		switch {
		case err != nil:
			continue // restarting
		case s.Version == head:
			fmt.Println("running", head[:7])
			return nil
		case len(s.Waiting) > 0:
			fmt.Printf("installed %s, and tack will restart when %d running conversations finish\n", head[:7], len(s.Waiting))
			return nil
		}
	}
	return fmt.Errorf("tack isn't running %s after %s: see journalctl --user -u tack", head[:7], startTimeout)
}

// installUnit keeps the installed service the same as the one in the repository.
func installUnit() error {
	wanted, err := os.ReadFile("tack.service")
	if err != nil {
		return err
	}
	dir, err := os.UserConfigDir()
	if err != nil {
		return err
	}
	path := filepath.Join(dir, "systemd", "user", "tack.service")
	if have, err := os.ReadFile(path); err == nil && bytes.Equal(have, wanted) {
		return nil
	}
	if err := os.WriteFile(path, wanted, 0o644); err != nil {
		return err
	}
	return run("systemctl", "--user", "daemon-reload")
}

func fetchStatus(base string) (status, error) {
	var s status
	client := http.Client{Timeout: 2 * time.Second}
	resp, err := client.Get(base + "/api/status")
	if err != nil {
		return s, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return s, fmt.Errorf("status %s", resp.Status)
	}
	return s, json.UnmarshalRead(resp.Body, &s)
}

func output(name string, args ...string) (string, error) {
	cmd := exec.Command(name, args...)
	cmd.Stderr = os.Stderr
	out, err := cmd.Output()
	return strings.TrimSpace(string(out)), err
}

func run(name string, args ...string) error {
	cmd := exec.Command(name, args...)
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	return cmd.Run()
}
