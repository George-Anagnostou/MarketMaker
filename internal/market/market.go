package market

import "math/rand/v2"

// Order represents a single market order from the "flow" (aggressive traders).
// In Phase 1, these are pure market orders that always execute against the
// market maker's posted bid or ask. There is no price on the order itself;
// the execution price is determined by the MM's quote.
type Order struct {
	IsBuy bool    // true = market buy (hits MM's ask, MM sells)
	Qty   float64 // positive quantity
}

// GenerateFlow creates a slice of market orders for one turn.
// Per the original spec: "random number of market orders (1-5 per turn)".
// Count is uniform in [1, maxOrders] when maxOrders >= 1.
// Each order direction is 50/50 buy/sell.
// Each size is uniform in [1, maxSize].
func GenerateFlow(r *rand.Rand, maxOrders int, maxSize float64) []Order {
	if maxOrders <= 0 || maxSize <= 0 {
		return nil
	}
	// 1 to maxOrders inclusive
	num := 1 + r.IntN(maxOrders)
	orders := make([]Order, num)
	for i := range orders {
		orders[i].IsBuy = r.IntN(2) == 0
		// uniform [1, maxSize]
		orders[i].Qty = 1 + r.Float64()*(maxSize-1)
	}
	return orders
}

// NextPrice performs a simple percentage random walk.
// delta ~ uniform[minPct, maxPct]
// returns current * (1 + delta)
func NextPrice(r *rand.Rand, current, minPct, maxPct float64) float64 {
	if minPct > maxPct {
		minPct, maxPct = maxPct, minPct
	}
	delta := minPct + r.Float64()*(maxPct-minPct)
	return current * (1 + delta)
}
