// Command tack is a coding agent harness. For now it's a browser chat with one
// model and tools that each need approval, and it records everything.
package main

import (
	"encoding/json/v2"
	"flag"
	"log"
	"net/http"
	"os"
	"os/signal"
	"runtime/debug"
	"slices"
	"sync"
	"syscall"

	"github.com/betawaffle/tack/internal/config"
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
	defaultConfig, err := config.DefaultPath()
	if err != nil {
		log.Print(err)
		os.Exit(exitUnreadableRecord)
	}
	configPath := flag.String("config", defaultConfig, "config file. See the README for its settings")
	flag.Parse()
	explicit := false
	flag.Visit(func(f *flag.Flag) { explicit = explicit || f.Name == "config" })

	cfg, err := config.Load(*configPath, explicit)
	if err != nil {
		// Restarting can't help with this either.
		log.Print(err)
		os.Exit(exitUnreadableRecord)
	}

	key, err := secret.Load(cfg.Key)
	if err != nil {
		log.Fatal(err)
	}
	st, err := store.Open(cfg.Data, secret.NewTripwire(key))
	if err != nil {
		log.Fatal(err)
	}
	c, err := core.New(core.Config{
		Store:    st,
		Client:   openrouter.NewClient(cfg.URL, key),
		Model:    cfg.Model,
		Routing:  routing(cfg),
		Tools:    tools.All(tools.Options{Protected: []string{cfg.Key}}),
		Settings: settings(cfg),
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
	if err := record(c, cfg); err != nil {
		log.Fatal(err)
	}
	if err := c.Recover(); err != nil {
		log.Fatal(err)
	}

	var (
		mu      sync.Mutex
		current = cfg
	)
	reload := func() (web.Reload, error) {
		mu.Lock()
		defer mu.Unlock()
		next, err := config.Load(*configPath, explicit)
		if err != nil {
			return web.Reload{}, err
		}
		if err := record(c, next); err != nil {
			return web.Reload{}, err
		}
		c.Reconfigure(next.Model, routing(next), settings(next))
		current = next
		return web.Reload{NeedsRestart: config.StartupChanges(cfg, next)}, nil
	}
	defaults := func() web.Defaults {
		mu.Lock()
		defer mu.Unlock()
		return web.Defaults{Sections: prompt.Load(current.Prompts), Workdir: current.DefaultWorkdir, Tools: true}
	}

	hangup := make(chan os.Signal, 1)
	signal.Notify(hangup, syscall.SIGHUP)
	go func() {
		for range hangup {
			result, err := reload()
			switch {
			case err != nil:
				log.Printf("reloading the config failed, so it stays as it was: %v", err)
			case len(result.NeedsRestart) > 0:
				log.Printf("reloaded the config, but these need a restart: %v", result.NeedsRestart)
			default:
				log.Print("reloaded the config")
			}
		}
	}()

	log.Printf("listening on %s, recording to %s, using %s on %s", cfg.Addr, cfg.Data, cfg.Model, cfg.Host)
	hosts := slices.Clone(cfg.AllowedHosts)
	if name, err := os.Hostname(); err == nil {
		hosts = append(hosts, name)
	}
	log.Fatal(http.ListenAndServe(cfg.Addr, web.Handler(c, defaults, reload, hosts)))
}

func routing(cfg config.Config) openrouter.Routing {
	return openrouter.Routing{Order: []string{cfg.Host}, AllowFallbacks: false}
}

func settings(cfg config.Config) store.Settings {
	return store.Settings{SearchSkip: cfg.SearchSkip, CommandEnv: cfg.CommandEnv, CommandTimeoutSeconds: cfg.CommandTimeoutSeconds}
}

// record notes the settings in force, so the record shows what applied when.
func record(c *core.Core, cfg config.Config) error {
	data, err := json.Marshal(cfg, json.Deterministic(true))
	if err != nil {
		return err
	}
	return c.RecordConfig(data)
}

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
