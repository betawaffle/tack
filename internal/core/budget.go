package core

import (
	"encoding/json/v2"
	"fmt"
	"math"
	"slices"
	"time"

	"github.com/betawaffle/tack/internal/store"
)

// A budget says how much the model can spend without a person getting involved. Whenever
// someone does something that calls the model, an allowance starts from there. That first
// call always goes, since they just asked for it. After that the budget is soft: tack
// doesn't start a call that it expects to go over, but it never interrupts one for it.
// Spending comes from what OpenRouter reports for each call.

// recentCostsKept is how many calls the estimate looks at.
const recentCostsKept = 3

func usageCost(ev *store.Event) (Pico, bool) {
	if len(ev.Usage) == 0 {
		return 0, false
	}
	var usage struct {
		Cost *float64 `json:"cost"`
	}
	if json.Unmarshal(ev.Usage, &usage) != nil || usage.Cost == nil {
		return 0, false
	}
	return picoFromUSD(*usage.Cost), true
}

// addCost counts a finished call. A call that was cancelled or cut off doesn't report
// its cost, and the host may charge for it anyway, so it counts as the most expensive
// recent call. A refused request costs nothing.
func (conv *conversation) addCost(ev *store.Event) Pico {
	if cost, ok := usageCost(ev); ok {
		conv.count(ev, cost)
		conv.recentCosts = append(conv.recentCosts, cost)
		if len(conv.recentCosts) > recentCostsKept {
			conv.recentCosts = conv.recentCosts[1:]
		}
		return cost
	}
	if ev.Status == 200 || ev.Cancelled {
		cost := conv.maxRecentCost()
		conv.count(ev, cost)
		return cost
	}
	return 0
}

func (conv *conversation) count(ev *store.Event, cost Pico) {
	conv.spent += cost
	if conv.spentByPeriod == nil {
		conv.spentByPeriod = map[string]Pico{}
	}
	conv.spentByPeriod[billingPeriod(ev.Time)] += cost
}

// billingPeriod names the period a time falls in. For now that's the calendar month
// where tack is running.
func billingPeriod(t time.Time) string {
	return t.Local().Format("2006-01")
}

// spentInPeriod is what all of tack has spent in a period. The caller holds mu.
func (c *Core) spentInPeriod(period string) Pico {
	var total Pico
	for _, conv := range c.convs {
		total += conv.spentByPeriod[period]
	}
	return total
}

func (conv *conversation) maxRecentCost() Pico {
	if len(conv.recentCosts) == 0 {
		return 0
	}
	return slices.Max(conv.recentCosts)
}

// estimate guesses the next call's cost from recent ones, scaled by how much the
// request has grown. A cache miss or a long reply can cost more, which is part of why
// the budget is soft. The first call has nothing to go on.
func (conv *conversation) estimate(requestBytes int) Pico {
	estimate := conv.maxRecentCost()
	if conv.lastRequestBytes > 0 && requestBytes > conv.lastRequestBytes {
		// It's only a guess, so a float is fine here, and multiplying first could overflow.
		estimate = Pico(math.Round(float64(estimate) * float64(requestBytes) / float64(conv.lastRequestBytes)))
	}
	return estimate
}

// budgetLimit is the total spending that the next call shouldn't go past.
func (conv *conversation) budgetLimit(settings store.Settings) Pico {
	if conv.allowanceSet {
		return conv.allowanceBase + conv.allowance
	}
	return picoFromUSD(settings.WithDefaults().BudgetUSD) + conv.budgetAdded
}

// startAllowance marks that a person has set things going. If the allowance they chose
// hasn't been used yet, that's the one that applies.
func (c *Core) startAllowance(conv *conversation) error {
	c.mu.Lock()
	if conv.freshAllowance {
		c.mu.Unlock()
		return nil
	}
	amount, spent := conv.effectiveSettings(c.settings).WithDefaults().BudgetUSD, conv.spent
	c.mu.Unlock()
	return c.record(&store.Event{Type: store.AllowanceStarted, Conversation: conv.id, AmountUSD: amount, SpentUSD: spent.USD(), By: "user"})
}

// AllowBudget lets the model spend this much more from here, whatever was left before.
// It's allowed at any time.
func (c *Core) AllowBudget(convID string, amount Pico) error {
	c.mu.Lock()
	conv := c.convs[convID]
	if conv == nil {
		c.mu.Unlock()
		return ErrNotFound
	}
	spent := conv.spent
	c.mu.Unlock()
	if amount <= 0 {
		return fmt.Errorf("%w: the amount has to be positive", ErrBadSettings)
	}
	return c.record(&store.Event{Type: store.AllowanceStarted, Conversation: convID, AmountUSD: amount.USD(), SpentUSD: spent.USD(), By: "user"})
}

// chainTotal is what the calls since someone last set things going cost.
type chainTotal struct {
	cost  Pico
	calls int
}

// chainTotals finds each finished chain's total, by the index of its last call. The
// caller holds mu.
func (conv *conversation) chainTotals() map[int]chainTotal {
	out := map[int]chainTotal{}
	for i, start := range conv.chainStarts {
		end := len(conv.items)
		if i+1 < len(conv.chainStarts) {
			end = conv.chainStarts[i+1]
		} else if conv.busy {
			continue
		}
		var total chainTotal
		last := -1
		for j := start; j < end; j++ {
			if it := conv.items[j]; it.role == "" {
				total.cost += it.cost
				total.calls++
				last = j
			}
		}
		if last >= 0 && total.cost > 0 {
			out[last] = total
		}
	}
	return out
}
