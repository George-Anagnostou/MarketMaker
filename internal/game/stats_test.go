package game

import "testing"

func TestStatsAccumulation(t *testing.T) {
	cfg := TestConfigWithSeed(777)
	cfg.NumTurns = 3
	cfg.MaxOrdersPerTurn = 2
	cfg.MaxOrderSize = 5
	cfg.StorageCostPerUnit = 1
	g := NewGame(cfg)

	// Play a few turns
	for i := 0; i < 3; i++ {
		if g.IsOver() {
			break
		}
		_, _ = g.SubmitTurn(99, 101)
	}

	st := g.Stats()

	// Basic sanity: with positive flow, these should be non-zero
	if st.TotalUnitsTraded <= 0 {
		t.Errorf("TotalUnitsTraded = %v, want > 0", st.TotalUnitsTraded)
	}
	// Max abs inventory should be >= 0
	if st.MaxAbsInventory < 0 {
		t.Errorf("MaxAbsInventory = %v, want >= 0", st.MaxAbsInventory)
	}
	// Storage paid should be >= 0
	if st.TotalStoragePaid < 0 {
		t.Errorf("TotalStoragePaid = %v, want >= 0", st.TotalStoragePaid)
	}
}
