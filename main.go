// Command tack is a coding agent harness. For now it's a browser chat with one
// model and tools that each need approval, and it records everything.
package main

import (
	"context"
	"encoding/json/v2"
	"errors"
	"flag"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"runtime/debug"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/betawaffle/tack/internal/cgroup"
	"github.com/betawaffle/tack/internal/config"
	"github.com/betawaffle/tack/internal/core"
	"github.com/betawaffle/tack/internal/docs"
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

// exitRestart asks systemd to start tack again, which runs whatever binary is
// installed by then. tack.service says so.
const exitRestart = 75

// restartNote is a file in the data directory holding what a requested restart
// changes, one per line, for the page to show. tack-deploy writes it.
const restartNote = "restart-note"

func main() {
	defaultConfig, err := config.DefaultPath()
	if err != nil {
		log.Print(err)
		os.Exit(exitUnreadableRecord)
	}
	configPath := flag.String("config", defaultConfig, "config file. See the README for its settings")
	check := flag.Bool("check", false, "check that this build can start with the config and record, without changing anything, then exit")
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
		// A key file with the wrong mode won't fix itself either.
		log.Print(err)
		os.Exit(exitUnreadableRecord)
	}
	if *check {
		if err := checkStart(cfg, key); err != nil {
			log.Print(err)
			os.Exit(exitUnreadableRecord)
		}
		return
	}
	groups, err := cgroup.Setup()
	if err != nil {
		// Commands still can't outlive their sandbox, but they don't get a cgroup.
		log.Printf("no delegated cgroup, so commands won't have their own: %v", err)
		groups = nil
	}
	st, err := store.Open(cfg.Data, secret.NewTripwire(key))
	if err != nil {
		log.Fatal(err)
	}
	client := openrouter.NewClient(cfg.URL, key)
	library := docs.Library{TackFS: modelDocs(), UserDir: filepath.Join(filepath.Dir(*configPath), "docs")}
	c, err := core.New(core.Config{
		Store:    st,
		Client:   client,
		Prices:   client.Endpoints,
		Tools:    append(tools.All(tools.Options{Protected: []string{cfg.Key}, Cgroups: groups, Docs: library, Variables: variables}), core.NoteTools()...),
		Settings: cfg.Settings,
	})
	if err != nil {
		// Restarting can't help, so this exit code tells systemd not to try.
		log.Print(err)
		os.Exit(exitUnreadableRecord)
	}
	// After reading the record, so a build that can't read it writes nothing, and
	// before recovering, because recovery's events belong to this build.
	if err := c.Started(version()); err != nil {
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
		c.Reconfigure(next.Settings)
		current = next
		return web.Reload{NeedsRestart: config.StartupChanges(cfg, next)}, nil
	}
	defaults := func() web.Defaults {
		mu.Lock()
		defer mu.Unlock()
		d := web.Defaults{Tools: c.ToolInfo(), Settings: current.Settings, Hosts: current.Hosts, Efforts: store.ReasoningEfforts, Date: prompt.Date(time.Now())}
		var err error
		if d.Prompt, err = prompt.Load(current.Prompt); err != nil {
			d.Problem = err.Error()
		}
		return d
	}
	save := func(d prompt.Defaults) error {
		mu.Lock()
		defer mu.Unlock()
		return prompt.Save(current.Prompt, d)
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

	// tack-deploy sends this, as do people who want a restart that disturbs nothing.
	// It's a signal rather than part of the API because the API has no authentication.
	restart := make(chan os.Signal, 1)
	signal.Notify(restart, syscall.SIGUSR1)
	go func() {
		for range restart {
			log.Print("restart requested, waiting for anything running to finish")
			if err := c.RequestRestart(takeRestartNote(cfg.Data)); err != nil {
				log.Printf("requesting a restart failed: %v", err)
			}
		}
	}()

	log.Printf("listening on %s, recording to %s, using %s on %s", cfg.Addr, cfg.Data, cfg.Model, cfg.Host)
	hosts := slices.Clone(cfg.AllowedHosts)
	if name, err := os.Hostname(); err == nil {
		hosts = append(hosts, name)
	}
	server := &http.Server{Addr: cfg.Addr, Handler: web.Handler(c, web.Options{
		Defaults: defaults, Save: save, Reload: reload, Hosts: hosts, Variables: variables,
		Source: source, Revision: version(), ModelDocsDir: modelDocsDir, Docs: library,
	})}
	go func() {
		<-c.Drained()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := server.Shutdown(ctx); err != nil {
			log.Printf("closing connections: %v", err)
		}
		log.Print("restarting")
		os.Exit(exitRestart)
	}()
	if err := server.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
		log.Fatal(err)
	}
	select {} // The goroutine above exits.
}

// checkStart does what starting would do, as far as it can without writing anything or
// getting in the way of a running tack.
func checkStart(cfg config.Config, key secret.Key) error {
	st, err := store.OpenReadOnly(cfg.Data)
	if errors.Is(err, os.ErrNotExist) {
		return nil // No record yet, so there's nothing to fail to read.
	}
	if err != nil {
		return err
	}
	_, err = core.New(core.Config{
		Store:    st,
		Client:   openrouter.NewClient(cfg.URL, key),
		Tools:    append(tools.All(tools.Options{Protected: []string{cfg.Key}}), core.NoteTools()...),
		Settings: cfg.Settings,
	})
	return err
}

// takeRestartNote reads and removes the note, so it can't be taken for a later restart's.
func takeRestartNote(data string) []string {
	path := filepath.Join(data, restartNote)
	text, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	os.Remove(path)
	var changes []string
	for line := range strings.Lines(string(text)) {
		if line = strings.TrimSpace(line); line != "" {
			changes = append(changes, line)
		}
	}
	return changes
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
