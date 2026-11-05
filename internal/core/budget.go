package core

import (
	"encoding/json/v2"
	"fmt"
	"slices"

	"github.com/betawaffle/tack/internal/store"
)

// A budget is soft: tack doesn't start a call that it expects to go over, but it never
// interrupts one for it. Spending comes from what OpenRouter reports for each call.

// recentCostsKept is how many calls the estimate looks at.
const recentCostsKept = 3

func usageCost(ev *store.Event) (float64, bool) {
	if len(ev.Usage) == 0 {
		return 0, false
	}
	var usage struct {
		Cost *float64 `json:"cost"`
	}
	if json.Unmarshal(ev.Usage, &usage) != nil || usage.Cost == nil {
		return 0, false
	}
	return *usage.Cost, true
}

// addCost counts a finished call. A call that was cancelled or cut off doesn't report
// its cost, and the host may charge for it anyway, so it counts as the most expensive
// recent call. A refused request costs nothing.
func (conv *conversation) addCost(ev *store.Event) {
	if cost, ok := usageCost(ev); ok {
		conv.spent += cost
		conv.recentCosts = append(conv.recentCosts, cost)
		if len(conv.recentCosts) > recentCostsKept {
			conv.recentCosts = conv.recentCosts[1:]
		}
		return
	}
	if ev.Status == 200 || ev.Cancelled {
		conv.spent += conv.maxRecentCost()
	}
}

func (conv *conversation) maxRecentCost() float64 {
	if len(conv.recentCosts) == 0 {
		return 0
	}
	return slices.Max(conv.recentCosts)
}

// estimate guesses the next call's cost from recent ones, scaled by how much the
// request has grown. A cache miss or a long reply can cost more, which is part of why
// the budget is soft. The first call has nothing to go on.
func (conv *conversation) estimate(requestBytes int) float64 {
	estimate := conv.maxRecentCost()
	if conv.lastRequestBytes > 0 && requestBytes > conv.lastRequestBytes {
		estimate *= float64(requestBytes) / float64(conv.lastRequestBytes)
	}
	return estimate
}

func (conv *conversation) budgetLimit(settings store.Settings) float64 {
	return settings.WithDefaults().BudgetUSD + conv.budgetAdded
}

// AddBudget adds to a conversation's budget. It's allowed at any time.
func (c *Core) AddBudget(convID string, amount float64) error {
	if err := c.exists(convID); err != nil {
		return err
	}
	if amount <= 0 {
		return fmt.Errorf("%w: the amount has to be positive", ErrBadSettings)
	}
	return c.record(&store.Event{Type: store.BudgetAdded, Conversation: convID, AmountUSD: amount, By: "user"})
}
