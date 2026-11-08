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

// rates are what a host charges per token, in picodollars: the least for input, uncached and
// cached, and for output, and the most for input and output.
type rates struct {
	uncached, cached, completion Pico
	maxPrompt, maxCompletion     Pico
}

func perToken(text string, off float64, round func(float64) float64) (Pico, bool) {
	price, err := strconv.ParseFloat(text, 64)
	if err != nil || price < 0 {
		return 0, false
	}
	return Pico(round(price * off * 1e12)), true
}

// priceRange is the cheapest and dearest of a host's prices, whatever the time of day. The
// cheapest has the discount taken off even if the listed prices already have, since a lower
// bound that's too low is still a lower bound, and the dearest doesn't.
func priceRange(e openrouter.Endpoint) (rates, bool) {
	r := rates{math.MaxInt64, math.MaxInt64, math.MaxInt64, 0, 0}
	ok := false
	off := 1 - min(max(e.Discount, 0), 1)
	for _, p := range append([]openrouter.Price{e.Pricing}, e.Overrides...) {
		prompt, okPrompt := perToken(p.Prompt, off, math.Floor)
		completion, okCompletion := perToken(p.Completion, off, math.Floor)
		if !okPrompt || !okCompletion {
			continue
		}
		cached := prompt
		if read, ok := perToken(p.InputCacheRead, off, math.Floor); ok && read < cached {
			cached = read
		}
		r.uncached, r.cached, r.completion = min(r.uncached, prompt), min(r.cached, cached), min(r.completion, completion)
		dearPrompt, _ := perToken(p.Prompt, 1, math.Ceil)
		dearCompletion, _ := perToken(p.Completion, 1, math.Ceil)
		r.maxPrompt, r.maxCompletion = max(r.maxPrompt, dearPrompt), max(r.maxCompletion, dearCompletion)
		ok = true
	}
	return r, ok
}

// hostsFor is the hosts a call to host could go to. With no host, any could take it.
func hostsFor(endpoints []openrouter.Endpoint, host string) []openrouter.Endpoint {
	var out []openrouter.Endpoint
	for _, e := range endpoints {
		if host == "" || e.Tag == host {
			out = append(out, e)
		}
	}
	return out
}

// ratesFor is the range of what a call to host could be charged per token.
func ratesFor(endpoints []openrouter.Endpoint, host string) (rates, bool) {
	best, found := rates{math.MaxInt64, math.MaxInt64, math.MaxInt64, 0, 0}, false
	for _, e := range hostsFor(endpoints, host) {
		if r, ok := priceRange(e); ok {
			best.uncached, best.cached, best.completion = min(best.uncached, r.uncached), min(best.cached, r.cached), min(best.completion, r.completion)
			best.maxPrompt, best.maxCompletion = max(best.maxPrompt, r.maxPrompt), max(best.maxCompletion, r.maxCompletion)
			found = true
		}
	}
	return best, found
}

// longestReply is the most tokens a reply to a prompt this long could have, from what the hosts
// say, or false if any of them doesn't say.
func longestReply(endpoints []openrouter.Endpoint, host string, prompt int) (int, bool) {
	longest, found := 0, false
	for _, e := range hostsFor(endpoints, host) {
		limit := e.MaxCompletionTokens
		if e.ContextLength > 0 && (limit == 0 || e.ContextLength-prompt < limit) {
			limit = max(e.ContextLength-prompt, 0)
		}
		if limit == 0 && e.ContextLength == 0 {
			return 0, false
		}
		longest, found = max(longest, limit), true
	}
	return longest, found
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
		c.processed, c.shortestReply = map[string]int{}, map[string]int{}
	}
	c.processed[model] = max(c.processed[model], usage.Prompt+usage.Completion)
	if shortest, ok := c.shortestReply[model]; !ok || usage.Completion < shortest {
		c.shortestReply[model] = usage.Completion
	}
}

// Estimate is three numbers for the next call: the least it could cost, the least it's likely
// to, with the shortest reply ever seen from the model, and the most it could.
type Estimate struct {
	Least, Likely, Most                Pico
	LeastKnown, LikelyKnown, MostKnown bool
}

// estimateFor works out the estimate of the request from prices. The caller holds mu.
func (c *Core) estimateFor(endpoints []openrouter.Endpoint, settings store.Settings, body []byte) Estimate {
	var out Estimate
	r, ok := ratesFor(endpoints, settings.Host)
	if !ok {
		return out
	}
	tokens := countRequest(settings.Model, body)
	if tokens == 0 {
		return out
	}
	out.Least, out.LeastKnown = minimumCost(r, tokens, c.processed[settings.Model]), true
	if shortest, ok := c.shortestReply[settings.Model]; ok {
		out.Likely, out.LikelyKnown = out.Least+Pico(shortest)*r.completion, true
	}
	if longest, ok := longestReply(endpoints, settings.Host, tokens); ok {
		out.Most, out.MostKnown = Pico(tokens)*r.maxPrompt+Pico(longest)*r.maxCompletion, true
	}
	return out
}

// minimumFor is the least the request could cost, given prices, or false if it can't be
// worked out. The caller holds mu.
func (c *Core) minimumFor(endpoints []openrouter.Endpoint, settings store.Settings, body []byte) (Pico, bool) {
	e := c.estimateFor(endpoints, settings, body)
	return e.Least, e.LeastKnown
}
