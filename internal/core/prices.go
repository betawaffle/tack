package core

import (
	"context"
	"encoding/json/v2"
	"math"
	"strconv"
	"time"

	"github.com/betawaffle/tack/internal/openrouter"
	"github.com/betawaffle/tack/internal/store"
)

// The estimate of the next call is the least it could possibly cost, never a guess at
// what it will: every input token the host could have cached is counted at the cheapest
// rate it lists at any time of day, the rest at its cheapest uncached rate, and the reply
// as nothing. So a call can always cost more than its estimate, and never less, as long
// as the prices are current.

// pricesKept is how long a model's prices are used before they're asked for again.
const pricesKept = 10 * time.Minute

type modelPrices struct {
	endpoints []openrouter.Endpoint
	fetched   time.Time
}

// rates are the least a host charges per input token, in picodollars, uncached and cached.
type rates struct {
	uncached, cached Pico
}

// lowest is the cheapest of a host's prices. It takes the discount off even if the listed
// prices already have, since a lower bound that's too low is still a lower bound.
func lowest(e openrouter.Endpoint) (rates, bool) {
	r := rates{math.MaxInt64, math.MaxInt64}
	ok := false
	for _, p := range append([]openrouter.Price{e.Pricing}, e.Overrides...) {
		prompt, err := strconv.ParseFloat(p.Prompt, 64)
		if err != nil || prompt < 0 {
			continue
		}
		cached := prompt
		if read, err := strconv.ParseFloat(p.InputCacheRead, 64); err == nil && read >= 0 && read < cached {
			cached = read
		}
		off := 1 - min(max(e.Discount, 0), 1)
		r.uncached = min(r.uncached, Pico(math.Floor(prompt*off*1e12)))
		r.cached = min(r.cached, Pico(math.Floor(cached*off*1e12)))
		ok = true
	}
	return r, ok
}

// ratesFor is the cheapest a call to host could be charged per token. With no host, any
// host could take it.
func ratesFor(endpoints []openrouter.Endpoint, host string) (rates, bool) {
	best, found := rates{math.MaxInt64, math.MaxInt64}, false
	for _, e := range endpoints {
		if host != "" && e.Tag != host {
			continue
		}
		if r, ok := lowest(e); ok {
			best.uncached, best.cached = min(best.uncached, r.uncached), min(best.cached, r.cached)
			found = true
		}
	}
	return best, found
}

// minimumCost is the least a call with this many input tokens could cost, when at most
// cacheable of them can have been seen before.
func minimumCost(r rates, tokens, cacheable int) Pico {
	cached := min(tokens, max(cacheable, 0))
	return Pico(tokens-cached)*r.uncached + Pico(cached)*r.cached
}

// prices returns a model's prices, asking for them if they're old or missing. If they
// can't be had, an old answer will do, and with none at all it says so.
func (c *Core) prices(ctx context.Context, model string) ([]openrouter.Endpoint, bool) {
	c.mu.Lock()
	known, ok := c.priceCache[model]
	c.mu.Unlock()
	if ok && time.Since(known.fetched) < pricesKept || c.cfg.Prices == nil {
		return known.endpoints, ok
	}
	endpoints, err := c.cfg.Prices(ctx, model)
	if err != nil {
		return known.endpoints, ok
	}
	c.mu.Lock()
	if c.priceCache == nil {
		c.priceCache = map[string]modelPrices{}
	}
	c.priceCache[model] = modelPrices{endpoints, time.Now()}
	c.mu.Unlock()
	return endpoints, true
}

// cachedPrices is whatever prices are known now, for the page, which can't wait on a
// request. If they're old it starts asking for them again.
func (c *Core) cachedPrices(model string) ([]openrouter.Endpoint, bool) {
	known, ok := c.priceCache[model]
	if (!ok || time.Since(known.fetched) >= pricesKept) && c.cfg.Prices != nil && !c.fetchingPrices[model] {
		if c.fetchingPrices == nil {
			c.fetchingPrices = map[string]bool{}
		}
		c.fetchingPrices[model] = true
		go func() {
			c.prices(context.Background(), model)
			c.mu.Lock()
			delete(c.fetchingPrices, model)
			c.mu.Unlock()
		}()
	}
	return known.endpoints, ok
}

// noteProcessed remembers the most tokens a host has processed in one call to a model:
// prompt and reply, since some hosts cache what they generate too. A host can't have
// cached more of a prompt than that, so it bounds what can be cached. It counts calls
// from every conversation, since they can share a beginning. The caller holds mu.
func (c *Core) noteProcessed(model string, ev *store.Event) {
	var usage struct {
		Prompt     int `json:"prompt_tokens"`
		Completion int `json:"completion_tokens"`
	}
	if len(ev.Usage) == 0 || json.Unmarshal(ev.Usage, &usage) != nil {
		return
	}
	if c.processed == nil {
		c.processed = map[string]int{}
	}
	c.processed[model] = max(c.processed[model], usage.Prompt+usage.Completion)
}

// minimumFor is the least the request could cost, given prices, or false if it can't be
// worked out. The caller holds mu.
func (c *Core) minimumFor(endpoints []openrouter.Endpoint, settings store.Settings, body []byte) (Pico, bool) {
	r, ok := ratesFor(endpoints, settings.Host)
	if !ok {
		return 0, false
	}
	tokens := countRequest(settings.Model, body)
	if tokens == 0 {
		return 0, false
	}
	return minimumCost(r, tokens, c.processed[settings.Model]), true
}
