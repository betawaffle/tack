package tools

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"github.com/betawaffle/tack/internal/core"
	"github.com/betawaffle/tack/internal/store"
)

// Where documentation is in the sandbox.
const (
	DocsDir        = "/docs"
	HarnessDocsDir = DocsDir + "/harness"
	UserDocsDir    = DocsDir + "/user"
)

// PythonVersion asks the interpreter code runs with what version it is. It isn't cached,
// since a system update can change it while tack runs.
func PythonVersion(ctx context.Context) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, python, "-I", "-S", "-c", "import platform; print(platform.python_version())")
	cmd.Dir, cmd.Env = "/", []string{}
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("asking python its version: %w", err)
	}
	return strings.TrimSpace(string(out)), nil
}

// Variables are facts about running code, for prompts and documentation, as they'd be
// for a conversation with these settings.
func Variables(ctx context.Context, settings store.Settings, site string) map[string]string {
	s := settings.WithDefaults()
	out := map[string]string{
		"memory_min":        fmt.Sprintf("%d GiB", codeMemoryMin>>30),
		"memory_max":        fmt.Sprintf("%d GiB", codeMemoryMax>>30),
		"work_size":         fmt.Sprintf("%d MiB", codeWorkBytes>>20),
		"time_limit":        seconds(s.CommandTimeoutSeconds),
		"channel_line_size": fmt.Sprintf("%d KiB", core.ChannelLineBytes>>10),
		"channel_calls":     fmt.Sprint(core.ChannelCalls),
		"auto_send_size":    fmt.Sprintf("%d KiB", codeAutoSendBytes>>10),
		"docs_dir":          DocsDir,
		"harness_docs":      HarnessDocsDir,
		"user_docs":         UserDocsDir,
	}
	if version, err := PythonVersion(ctx); err == nil {
		out["python_version"] = version
	}
	out["python_packages"] = "none"
	if list := PackageList(site); list != "" {
		out["python_packages"] = list
	}
	out["library_threads"] = codeThreads
	return out
}

func seconds(n int) string {
	if n == 1 {
		return "1 second"
	}
	return fmt.Sprintf("%d seconds", n)
}
