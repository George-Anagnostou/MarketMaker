package engine

import (
	"errors"
	"fmt"
	"math"
	"math/rand/v2"

	"market-maker/internal/market"
	"market-maker/internal/types"
)

// Engine is the pure, deterministic market-making simulation engine.
// It performs turn steps (bid/ask quotes vs generated flow + price walk + storage).
// It has no concept of "game over", sessions, or quit. It can be stepped forever.
// Terminal conditions (bankruptcy, turn limits) are the responsibility of a higher
// game/session layer that wraps this.
//
// Determinism: same seed + same sequence of bids/asks => identical results.
// All randomness goes through the internal RNG.
type Engine struct {
	cfg types.GameConfig
	rng *rand.Rand

	// simulation state (no IsOver/Reason here)
	cash      float64
	inventory float64
	lastPrice float64
	turn      int // turns completed

	// lifetime stats
	maxAbsInventory float64
	totalUnits      float64
	totalNetFill    float64
	totalStorage    float64
}

// NewEngine creates a new pure simulation engine from config.
func NewEngine(cfg types.GameConfig) *Engine {
	seed := cfg.Seed
	if seed == 0 {
		seed = int64(rand.Uint64())
	}
	r := rand.New(rand.NewPCG(uint64(seed), uint64(seed>>32)))

	e := &Engine{
		cfg:       cfg,
		rng:       r,
		cash:      cfg.StartingCash,
		inventory: cfg.StartingInventory,
		lastPrice: cfg.StartingPrice,
		turn:      0,
	}
	e.maxAbsInventory = math.Abs(cfg.StartingInventory)
	return e
}

// Step executes one turn with the player's bid/ask against generated flow.
// It always succeeds for valid inputs (no "already over" concept in engine).
// Returns TurnResult with State.IsOver=false and Reason="" always.
// Callers (game layer) decide whether to treat this as ending a session.
func (e *Engine) Step(bid, ask float64) (types.TurnResult, error) {
	if bid <= 0 || ask <= 0 {
		return types.TurnResult{State: e.currentState()}, errors.New("bid and ask must be positive")
	}
	if bid >= ask {
		return types.TurnResult{State: e.currentState()}, errors.New("bid must be strictly less than ask")
	}

	events := []types.Event{}
	beforeCash := e.cash
	beforeInv := e.inventory
	beforeEquity := beforeCash + beforeInv*e.lastPrice

	// 1. Generate flow
	flow := market.GenerateFlow(e.rng, e.cfg.MaxOrdersPerTurn, e.cfg.MaxOrderSize)
	ordersReceived := len(flow)

	// 2. Execute fills
	var unitsTraded float64
	var realizedCash float64
	var buyVol, sellVol float64

	for _, ord := range flow {
		if ord.IsBuy {
			notional := ask * ord.Qty
			e.cash += notional
			e.inventory -= ord.Qty
			unitsTraded += ord.Qty
			realizedCash += notional
			buyVol += ord.Qty
			events = append(events, types.Event{
				Type:    "fill",
				Message: fmt.Sprintf("MARKET BUY %.2f @ your ASK $%.2f → you SOLD (aggressive buyers hit you)", ord.Qty, ask),
			})
		} else {
			notional := bid * ord.Qty
			e.cash -= notional
			e.inventory += ord.Qty
			unitsTraded += ord.Qty
			realizedCash -= notional
			sellVol += ord.Qty
			events = append(events, types.Event{
				Type:    "fill",
				Message: fmt.Sprintf("MARKET SELL %.2f @ your BID $%.2f → you BOUGHT (aggressive sellers hit you)", ord.Qty, bid),
			})
		}
	}

	// Update stats
	absInv := math.Abs(e.inventory)
	if absInv > e.maxAbsInventory {
		e.maxAbsInventory = absInv
	}
	e.totalUnits += unitsTraded
	e.totalNetFill += realizedCash

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

	// 3. Storage
	storage := e.cfg.StorageCostPerUnit * math.Abs(e.inventory)
	e.cash -= storage
	e.totalStorage += storage
	if storage > 0 {
		events = append(events, types.Event{
			Type:    "storage",
			Message: fmt.Sprintf("Storage costs: $%.2f (%.2f units held)", storage, math.Abs(e.inventory)),
		})
	}

	// 4. Price walk
	oldPrice := e.lastPrice
	e.lastPrice = market.NextPrice(e.rng, oldPrice, e.cfg.MinPriceMovePct, e.cfg.MaxPriceMovePct)
	pct := (e.lastPrice - oldPrice) / oldPrice * 100
	events = append(events, types.Event{
		Type:    "price_update",
		Message: fmt.Sprintf("Price moved from $%.2f to $%.2f (%+.2f%%)", oldPrice, e.lastPrice, pct),
	})

	// 5. Accounting
	afterEquity := e.cash + e.inventory*e.lastPrice
	turnPnL := afterEquity - beforeEquity

	// 6. Advance turn (engine does not decide "over" here)
	e.turn++

	summary := types.TurnSummary{
		OrdersReceived: ordersReceived,
		UnitsTraded:    unitsTraded,
		NetFillCash:    realizedCash,
		StorageCost:    storage,
		TurnPnL:        turnPnL,
		BuyVolume:      buyVol,
		SellVolume:     sellVol,
	}

	// Always return with IsOver=false; game layer overlays end state if desired
	state := e.currentState()

	result := types.TurnResult{
		State:   state,
		Events:  events,
		Summary: summary,
	}
	return result, nil
}

// currentState returns the observable state (IsOver/Reason always false/empty from engine)
func (e *Engine) currentState() types.GameState {
	return types.GameState{
		Turn:      e.turn,
		Cash:      e.cash,
		Inventory: e.inventory,
		LastPrice: e.lastPrice,
		IsOver:    false,
		Reason:    types.EndReasonNotOver,
	}
}

// State returns a copy of current observable state (never over from engine view).
func (e *Engine) State() types.GameState {
	return e.currentState()
}

// Config returns a copy of the config.
func (e *Engine) Config() types.GameConfig {
	return e.cfg
}

// Stats returns lifetime aggregates.
func (e *Engine) Stats() types.GameStats {
	return types.GameStats{
		MaxAbsInventory:  e.maxAbsInventory,
		TotalUnitsTraded: e.totalUnits,
		TotalNetFillCash: e.totalNetFill,
		TotalStoragePaid: e.totalStorage,
	}
}
