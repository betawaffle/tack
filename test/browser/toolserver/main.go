// Command toolserver runs tack with tools that exist only for the browser tests,
// since the real binary doesn't offer any yet.
package main

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"flag"
	"log"
	"net/http"
	"sync"
	"time"

	"github.com/betawaffle/tack/internal/core"
	"github.com/betawaffle/tack/internal/openrouter"
	"github.com/betawaffle/tack/internal/prompt"
	"github.com/betawaffle/tack/internal/secret"
	"github.com/betawaffle/tack/internal/store"
	"github.com/betawaffle/tack/internal/web"
)

func definition(name string) jsontext.Value {
	return jsontext.Value(`{"type":"function","function":{"name":"` + name + `","description":"A test tool.","parameters":{"type":"object","properties":{"text":{"type":"string"}}}}}`)
}

func main() {
	addr := flag.String("addr", "127.0.0.1:18602", "address to serve on")
	data := flag.String("data", "", "data directory")
	url := flag.String("url", "", "provider URL, normally the fake one")
	keyPath := flag.String("key", "", "key file for a real provider; empty means a fake key")
	model := flag.String("model", "test/model", "model")
	flag.Parse()

	key := secret.NewKey("sk-or-fake-key-for-browser-tests")
	if *keyPath != "" {
		var err error
		if key, err = secret.Load(*keyPath); err != nil {
			log.Fatal(err)
		}
	}
	st, err := store.Open(*data, secret.NewTripwire(key))
	if err != nil {
		log.Fatal(err)
	}
	c, err := core.New(core.Config{
		Store:    st,
		Client:   openrouter.NewClient(*url, key),
		Model:    *model,
		Settings: store.Settings{BudgetUSD: 1},
		Routing:  openrouter.Routing{Order: []string{"morph/fp8"}},
		Tools: []core.Tool{
			{Name: "echo", Definition: definition("echo"), Run: func(_ context.Context, call core.Call) ([]byte, error) {
				var text string
				err := json.Unmarshal(call.Args["text"], &text)
				return []byte("echoed " + text), err
			}},
			{Name: "json", Definition: definition("json"), Run: func(context.Context, core.Call) ([]byte, error) {
				return []byte(`{"big":12345678901234567890,"list":[1,true,null],"empty":{},"text":"a\u202eb"}`), nil
			}},
			{Name: "boom", Definition: definition("boom"), Run: func(context.Context, core.Call) ([]byte, error) {
				return []byte("some output"), errors.New("it broke")
			}},
		},
	})
	if err != nil {
		log.Fatal(err)
	}
	if err := c.Recover(); err != nil {
		log.Fatal(err)
	}
	var (
		mu    sync.Mutex
		saved = prompt.Defaults{Sections: []prompt.Section{{Name: "test", Text: "Use your tools carefully."}}}
	)
	defaults := func() web.Defaults {
		mu.Lock()
		defer mu.Unlock()
		return web.Defaults{Prompt: saved, Tools: c.ToolInfo(), Settings: store.Settings{BudgetUSD: 1}.WithDefaults(), Hosts: []string{"morph/fp8", "relace/fp4"}, Efforts: store.ReasoningEfforts, Date: prompt.Date(time.Now())}
	}
	save := func(d prompt.Defaults) error {
		mu.Lock()
		defer mu.Unlock()
		saved = d
		return nil
	}
	log.Fatal(http.ListenAndServe(*addr, web.Handler(c, defaults, save, func() (web.Reload, error) { return web.Reload{}, nil }, nil)))
}
