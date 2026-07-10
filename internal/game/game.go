package game

import (
	"errors"
	"fmt"
	"math"
	"math/rand/v2"

	"market-maker/internal/market"
	"market-maker/internal/types"
)

// Game is the core single-player market making engine.
// It is deterministic when a non-zero Seed is provided in config.
// All randomness (order flow generation and price walk) goes through the
// internal RNG so that the same sequence of player actions + same seed
// produces identical results.
//
// Phase 1 design:
// - Player posts bid and ask prices each turn (size is implicit / "at size").
// - Pseudo-random market orders (aggressive flow) are generated.
// - Market buys hit the ask (player sells to flow).
// - Market sells hit the bid (player buys from flow).
// - Price performs a uniform random walk.
// - Storage cost is charged on absolute inventory.
// - Equity = Cash + Inventory * LastPrice (mark-to-market to current reference price).
// - Game ends on bankruptcy (cash <= 0) or after configured number of turns.
// - NumTurns == 0 means unlimited mode (ends only on bankruptcy or explicit quit at CLI level).
//
// MTM note: LastPrice is the exogenous simulated market reference price
// (driven by the random walk). This is the standard approach in trading
// simulations for marking inventory risk to "the market", not to your own
// executed fill prices. Using your own last fill price for MTM would mask
// the inventory risk lesson.
type Game struct {
	cfg   types.GameConfig
	rng   *rand.Rand
	state types.GameState

	// Lifetime stats for end-of-game reporting and future web API.
	maxAbsInventory float64
	totalUnits      float64
	totalNetFill    float64 // sum of NetFillCash across all turns
	totalStorage    float64 // sum of storage costs paid
}

// NewGame creates a new game from config.
// If cfg.Seed == 0, a non-deterministic seed is used (fine for play, not for tests).
func NewGame(cfg types.GameConfig) *Game {
	seed := cfg.Seed
	if seed == 0 {
		seed = int64(rand.Uint64())
	}
	r := rand.New(rand.NewPCG(uint64(seed), uint64(seed>>32)))

	g := &Game{
		cfg: cfg,
		rng: r,
		state: types.GameState{
			Turn:      0,
			Cash:      cfg.StartingCash,
			Inventory: cfg.StartingInventory,
			LastPrice: cfg.StartingPrice,
			IsOver:    false,
			Reason:    "",
		},
	}
	return g
}

// State returns a copy of the current observable state.
func (g *Game) State() types.GameState {
	return g.state
}

// Config returns a copy of the game config.
func (g *Game) Config() types.GameConfig {
	return g.cfg
}

// SubmitTurn executes one turn with the player's bid/ask.
// It generates flow, executes fills, applies storage and price walk,
// updates state, and returns the full TurnResult.
//
// Returns error for invalid inputs (bid >= ask, non-positive prices, or game already over).
func (g *Game) SubmitTurn(bid, ask float64) (types.TurnResult, error) {
	if g.state.IsOver {
		return types.TurnResult{State: g.state}, errors.New("game is already over")
	}
	if bid <= 0 || ask <= 0 {
		return types.TurnResult{State: g.state}, errors.New("bid and ask must be positive")
	}
	if bid >= ask {
		return types.TurnResult{State: g.state}, errors.New("bid must be strictly less than ask")
	}

	events := []types.Event{}
	beforeCash := g.state.Cash
	beforeInv := g.state.Inventory
	beforeEquity := beforeCash + beforeInv*g.state.LastPrice

	// 1. Generate flow (market orders)
	flow := market.GenerateFlow(g.rng, g.cfg.MaxOrdersPerTurn, g.cfg.MaxOrderSize)
	ordersReceived := len(flow)

	// 2. Execute fills against player's quotes
	var unitsTraded float64
	var realizedCash float64 // net cash from fills (positive = received from sells)
	var buyVol, sellVol float64

	for _, ord := range flow {
		if ord.IsBuy {
			// Market buy hits ask: player sells to aggressive buyers
			notional := ask * ord.Qty
			g.state.Cash += notional
			g.state.Inventory -= ord.Qty
			unitsTraded += ord.Qty
			realizedCash += notional
			buyVol += ord.Qty
			events = append(events, types.Event{
				Type:    "fill",
				Message: fmt.Sprintf("MARKET BUY %.2f @ your ASK $%.2f → you SOLD (aggressive buyers hit you)", ord.Qty, ask),
			})
		} else {
			// Market sell hits bid: player buys from aggressive sellers
			notional := bid * ord.Qty
			g.state.Cash -= notional
			g.state.Inventory += ord.Qty
			unitsTraded += ord.Qty
			realizedCash -= notional
			sellVol += ord.Qty
			events = append(events, types.Event{
				Type:    "fill",
				Message: fmt.Sprintf("MARKET SELL %.2f @ your BID $%.2f → you BOUGHT (aggressive sellers hit you)", ord.Qty, bid),
			})
		}
	}

	// Update lifetime stats after fills (even if 0 orders this turn, inventory may be held from before)
	absInv := math.Abs(g.state.Inventory)
	if absInv > g.maxAbsInventory {
		g.maxAbsInventory = absInv
	}
	g.totalUnits += unitsTraded
	g.totalNetFill += realizedCash

	if ordersReceived > 0 {
		totalUnits := 0.0
		for _, o := range flow {
			totalUnits += o.Qty
		}
		events = append([]types.Event{{
			Type:    "flow",
			Message: fmt.Sprintf("%d market orders received (%.2f units total)", ordersReceived, totalUnits),
		}}, events...)
	} else {
		events = append([]types.Event{{
			Type:    "flow",
			Message: "No market orders this turn",
		}}, events...)
	}

	// 3. Storage cost on post-fill inventory
	storage := g.cfg.StorageCostPerUnit * math.Abs(g.state.Inventory)
	g.state.Cash -= storage
	g.totalStorage += storage
	if storage > 0 {
		events = append(events, types.Event{
			Type:    "storage",
			Message: fmt.Sprintf("Storage costs: $%.2f (%.2f units held)", storage, math.Abs(g.state.Inventory)),
		})
	}

	// 4. Price random walk
	oldPrice := g.state.LastPrice
	g.state.LastPrice = market.NextPrice(g.rng, oldPrice, g.cfg.MinPriceMovePct, g.cfg.MaxPriceMovePct)
	pct := (g.state.LastPrice - oldPrice) / oldPrice * 100
	events = append(events, types.Event{
		Type:    "price_update",
		Message: fmt.Sprintf("Price moved from $%.2f to $%.2f (%+.2f%%)", oldPrice, g.state.LastPrice, pct),
	})

	// 5. Turn accounting
	afterEquity := g.state.Cash + g.state.Inventory*g.state.LastPrice
	turnPnL := afterEquity - beforeEquity

	// 6. Advance turn
	g.state.Turn++

	// 7. Check terminal conditions
	if g.state.Cash <= 0 {
		g.state.IsOver = true
		g.state.Reason = "bankrupt"
		events = append(events, types.Event{
			Type:    "game_over",
			Message: "BANKRUPTCY! Cash <= 0. Game over.",
		})
	} else if g.cfg.NumTurns > 0 && g.state.Turn >= g.cfg.NumTurns {
		g.state.IsOver = true
		g.state.Reason = "turns_complete"
		events = append(events, types.Event{
			Type:    "game_over",
			Message: fmt.Sprintf("Completed %d turns.", g.cfg.NumTurns),
		})
	}

	summary := types.TurnSummary{
		OrdersReceived: ordersReceived,
		UnitsTraded:    unitsTraded,
		NetFillCash:    realizedCash,
		StorageCost:    storage,
		TurnPnL:        turnPnL,
		BuyVolume:      buyVol,
		SellVolume:     sellVol,
	}

	result := types.TurnResult{
		State:   g.state,
		Events:  events,
		Summary: summary,
	}
	return result, nil
}

// IsOver reports whether the game has ended.
func (g *Game) IsOver() bool {
	return g.state.IsOver
}

// Reason returns the terminal reason if over, otherwise "".
func (g *Game) Reason() string {
	return g.state.Reason
}

// GameStats holds lifetime aggregates for the session.
// Useful for end-of-game reporting and future analytics / web API.
type GameStats struct {
	MaxAbsInventory  float64 // maximum |inventory| reached at any point (risk metric)
	TotalUnitsTraded float64 // sum of absolute units filled across all turns
	TotalNetFillCash float64 // sum of net cash from fills (before storage) across turns
	TotalStoragePaid float64 // sum of all storage costs deducted
}

// Stats returns the lifetime stats accumulated during this game.
func (g *Game) Stats() GameStats {
	return GameStats{
		MaxAbsInventory:  g.maxAbsInventory,
		TotalUnitsTraded: g.totalUnits,
		TotalNetFillCash: g.totalNetFill,
		TotalStoragePaid: g.totalStorage,
	}
}
