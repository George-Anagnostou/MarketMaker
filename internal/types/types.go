package types

// GameConfig holds all parameters for a game session.
// All fields are exported for easy config and (de)serialization in later phases.
type GameConfig struct {
	// StartingCash is the initial cash balance in dollars.
	StartingCash float64 `json:"starting_cash"`

	// StartingInventory is the initial position in units (can be negative for short).
	StartingInventory float64 `json:"starting_inventory"`

	// StartingPrice is the initial market reference price used for MTM and first display.
	StartingPrice float64 `json:"starting_price"`

	// NumTurns is the number of turns in a finite game. 0 means unlimited (play until bankrupt or explicit quit via game layer).
	NumTurns int `json:"num_turns"`

	// StorageCostPerUnit is the cash cost deducted each turn per unit of absolute inventory.
	StorageCostPerUnit float64 `json:"storage_cost_per_unit"`

	// MinPriceMovePct and MaxPriceMovePct define the uniform range for the price random walk
	// applied after order execution each turn. Example: -0.03 to +0.03 for ±3%.
	MinPriceMovePct float64 `json:"min_price_move_pct"`
	MaxPriceMovePct float64 `json:"max_price_move_pct"`

	// MaxOrdersPerTurn is the maximum number of market orders generated per turn (inclusive).
	// Actual count is uniform [1, MaxOrdersPerTurn] (at least 1 if MaxOrdersPerTurn >= 1).
	MaxOrdersPerTurn int `json:"max_orders_per_turn"`

	// MaxOrderSize is the maximum size of each market order (uniform [1, MaxOrderSize]).
	MaxOrderSize float64 `json:"max_order_size"`

	// Seed is used for deterministic RNG. If 0, a time-based seed is used (non-reproducible).
	Seed int64 `json:"seed"`
}

// EndReason describes why a game/session ended. Empty means not over.
// These are first-class and used by game layer (on top of engine).
type EndReason string

const (
	EndReasonNotOver      EndReason = ""
	EndReasonBankrupt     EndReason = "bankrupt"
	EndReasonTurnsComplete EndReason = "turns_complete"
	EndReasonPlayerQuit   EndReason = "player_quit"
)

// GameState is the observable state after each turn (or at start).
type GameState struct {
	Turn      int     `json:"turn"` // current turn number (0 before first turn)
	Cash      float64 `json:"cash"`
	Inventory float64 `json:"inventory"`
	LastPrice float64 `json:"last_price"` // reference market price for display and MTM
	IsOver    bool    `json:"is_over"`
	Reason    EndReason `json:"reason"` // one of the EndReason* constants
}

// Equity computes mark-to-market equity using the current LastPrice.
// This is the standard way to value inventory at the prevailing market price.
func (s GameState) Equity() float64 {
	return s.Cash + s.Inventory*s.LastPrice
}

// StartingEquity is a helper to compute initial equity from config.
func StartingEquity(cfg GameConfig) float64 {
	return cfg.StartingCash + cfg.StartingInventory*cfg.StartingPrice
}

// Event represents something that happened during a turn.
// Type is one of: "flow", "fill", "price_update", "storage", "game_over".
// Message is human-readable. The TurnSummary lives in TurnResult, not as an event.
type Event struct {
	Type    string `json:"type"`
	Message string `json:"message"`
}

// TurnResult is returned after each SubmitTurn.
type TurnResult struct {
	State   GameState   `json:"state"`
	Events  []Event     `json:"events"`
	Summary TurnSummary `json:"summary"`
}

// TurnSummary aggregates key numbers for the turn (useful for UI and scoring later).
type TurnSummary struct {
	OrdersReceived int     `json:"orders_received"`
	UnitsTraded    float64 `json:"units_traded"`
	// NetFillCash is the net cash impact from fills this turn before storage.
	// Positive = net cash received (mostly from selling to market buys).
	// Negative = net cash paid (mostly from buying from market sells).
	NetFillCash float64 `json:"net_fill_cash"`

	StorageCost float64 `json:"storage_cost"`
	TurnPnL     float64 `json:"turn_pnl"` // change in equity this turn

	// BuyVolume: market buy orders that hit your ask (you sold to aggressive buyers)
	BuyVolume float64 `json:"buy_volume"`
	// SellVolume: market sell orders that hit your bid (you bought from aggressive sellers)
	SellVolume float64 `json:"sell_volume"`
}
