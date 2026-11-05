// Command tack is a coding agent harness. For now it's a browser chat with one
// model and read-only tools that each need approval, and it records everything.
package main

import (
	"flag"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"runtime/debug"
	"strings"

	"github.com/betawaffle/tack/internal/core"
	"github.com/betawaffle/tack/internal/openrouter"
	"github.com/betawaffle/tack/internal/prompt"
	"github.com/betawaffle/tack/internal/secret"
	"github.com/betawaffle/tack/internal/store"
	"github.com/betawaffle/tack/internal/tools"
	"github.com/betawaffle/tack/internal/web"
)

// exitUnreadableRecord is EX_CONFIG: tack can't read its record, such as one
// written by a newer build, and that needs a person. tack.service lists it in
// RestartPreventExitStatus.
const exitUnreadableRecord = 78

func main() {
	addr := flag.String("addr", "0.0.0.0:8484", "address to listen on")
	dataDir := flag.String("data", defaultPath("XDG_DATA_HOME", ".local/share", "tack"), "data directory")
	keyPath := flag.String("key", defaultPath("XDG_CONFIG_HOME", ".config", "tack/openrouter-key"), "OpenRouter key file")
	url := flag.String("url", openrouter.DefaultURL, "OpenRouter endpoint. The key is sent here")
	model := flag.String("model", "deepseek/deepseek-v4.1-flash-1017", "model")
	host := flag.String("host", "morph/fp8", "OpenRouter provider to pin")
	promptDir := flag.String("prompts", defaultPath("XDG_CONFIG_HOME", ".config", "tack/prompt"), "directory of system prompt sections, as <name>.md, overriding or adding to the built-in ones")
	searchSkip := flag.String("search-skip", strings.Join(tools.DefaultSkipDirs, ","), "comma-separated directory names search_files doesn't go into")
	workdir := flag.String("workdir", "", "default working directory for new conversations (default: the current directory)")
	flag.Parse()

	key, err := secret.Load(*keyPath)
	if err != nil {
		log.Fatal(err)
	}
	st, err := store.Open(*dataDir, secret.NewTripwire(key))
	if err != nil {
		log.Fatal(err)
	}
	c, err := core.New(core.Config{
		Store:   st,
		Client:  openrouter.NewClient(*url, key),
		Model:   *model,
		Routing: openrouter.Routing{Order: []string{*host}, AllowFallbacks: false},
		Tools:   tools.Read(tools.Options{Protected: []string{*keyPath}, SkipDirs: strings.FieldsFunc(*searchSkip, func(r rune) bool { return r == ',' })}),
	})
	if err != nil {
		// Restarting can't help, so this exit code tells systemd not to try.
		log.Print(err)
		os.Exit(exitUnreadableRecord)
	}
	// After reading the record, so a build that can't read it writes nothing, and
	// before recovering, because recovery's events belong to this build.
	err = st.Append(&store.Event{Type: store.TackStarted, Version: version(), PID: os.Getpid()})
	if err != nil {
		log.Fatal(err)
	}
	if err := c.Recover(); err != nil {
		log.Fatal(err)
	}

	log.Printf("listening on %s, recording to %s, using %s on %s", *addr, *dataDir, *model, *host)
	if *workdir == "" {
		if *workdir, err = os.Getwd(); err != nil {
			log.Fatal(err)
		}
	}
	defaults := func() web.Defaults {
		return web.Defaults{Sections: prompt.Load(*promptDir), Workdir: *workdir, Tools: true}
	}
	log.Fatal(http.ListenAndServe(*addr, web.Handler(c, defaults)))
}

func defaultPath(env, fallback, name string) string {
	if base := os.Getenv(env); base != "" {
		return filepath.Join(base, name)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		log.Fatal(err)
	}
	return filepath.Join(home, fallback, name)
}

// version identifies the code that's running, so the record can tell.
func version() string {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return "unknown"
	}
	revision, dirty := "unknown", ""
	for _, s := range info.Settings {
		switch s.Key {
		case "vcs.revision":
			revision = s.Value
		case "vcs.modified":
			if s.Value == "true" {
				dirty = "+dirty"
			}
		}
	}
	return revision + dirty
}
