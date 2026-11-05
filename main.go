// Command tack is a coding agent harness. For now it's a browser chat with one
// model that records everything.
package main

import (
	"errors"
	"flag"
	"io/fs"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"runtime/debug"

	"github.com/betawaffle/tack/internal/core"
	"github.com/betawaffle/tack/internal/openrouter"
	"github.com/betawaffle/tack/internal/secret"
	"github.com/betawaffle/tack/internal/store"
	"github.com/betawaffle/tack/internal/web"
)

func main() {
	addr := flag.String("addr", "0.0.0.0:8484", "address to listen on")
	dataDir := flag.String("data", defaultPath("XDG_DATA_HOME", ".local/share", "tack"), "data directory")
	keyPath := flag.String("key", defaultPath("XDG_CONFIG_HOME", ".config", "tack/openrouter-key"), "OpenRouter key file")
	url := flag.String("url", openrouter.DefaultURL, "OpenRouter endpoint. The key is sent here")
	model := flag.String("model", "deepseek/deepseek-v4.1-flash-1017", "model")
	host := flag.String("host", "morph/fp8", "OpenRouter provider to pin")
	promptPath := flag.String("system-prompt", defaultPath("XDG_CONFIG_HOME", ".config", "tack/system-prompt"), "file overriding the default system prompt for new conversations")
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
	})
	if err != nil {
		log.Fatal(err)
	}
	err = st.Append(&store.Event{Type: store.TackStarted, Version: version(), PID: os.Getpid()})
	if err != nil {
		log.Fatal(err)
	}

	log.Printf("listening on %s, recording to %s, using %s on %s", *addr, *dataDir, *model, *host)
	log.Fatal(http.ListenAndServe(*addr, web.Handler(c, func() string { return defaultPrompt(*promptPath) })))
}

// defaultPrompt reads the override each time, so editing the file doesn't need a
// restart.
func defaultPrompt(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			log.Printf("using the built-in system prompt: %v", err)
		}
		return core.DefaultSystemPrompt
	}
	return string(data)
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
