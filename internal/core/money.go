package core

import "math"

// Pico is an amount of money in millionths of a millionth of a dollar. Adding up floats
// drifts, and the cheapest token costs a few thousand of these, so everything that
// counts money does it in whole picodollars. The record has dollars, as JSON numbers,
// which go through picoFromUSD on the way in and come back exactly.
type Pico int64

func picoFromUSD(usd float64) Pico {
	return Pico(math.Round(usd * 1e12))
}

// USD is the amount in dollars, for the record and for printing.
func (p Pico) USD() float64 {
	return float64(p) / 1e12
}
