package game

import "market-maker/internal/types"

// DefaultConfig returns a sensible starting configuration for Phase 1,
// based on the original product spec.
func DefaultConfig() types.GameConfig {
	return types.GameConfig{
		StartingCash:       100_000,
		StartingInventory:  0,
		StartingPrice:      100,
		NumTurns:           10, // 0 = unlimited
		StorageCostPerUnit: 1.0,
		MinPriceMovePct:    -0.005, // -0.5%
		MaxPriceMovePct:    0.03,   // +3%
		MaxOrdersPerTurn:   5,
		MaxOrderSize:       10,
		Seed:               0, // caller can override for determinism
	}
}
