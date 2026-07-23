// Package scenario defines the server-owned educational game catalog.
package scenario

import (
	"errors"
	"fmt"
	"sort"

	"market-maker/internal/exchange"
	"market-maker/internal/fixed"
)

type Definition struct {
	ID        string
	Revision  string
	Title     string
	Briefing  string
	Objective string
	Config    exchange.Config
}

// Snapshot is persisted with every game so its lesson cannot change when the
// in-code catalog evolves.
type Snapshot struct {
	ID        string `json:"id"`
	Revision  string `json:"revision"`
	Title     string `json:"title"`
	Briefing  string `json:"briefing"`
	Objective string `json:"objective"`
	Turns     int    `json:"turns"`
}

type Coaching struct {
	Code  string `json:"code"`
	Title string `json:"title"`
	Body  string `json:"body"`
}

type Recap struct {
	Headline        string             `json:"headline"`
	Body            string             `json:"body"`
	FinalEquity     fixed.Money        `json:"final_equity"`
	TotalPnL        fixed.Money        `json:"total_pnl"`
	MaxAbsInventory fixed.Qty          `json:"max_abs_inventory"`
	UnitsTraded     fixed.Qty          `json:"units_traded"`
	StoragePaid     fixed.Money        `json:"storage_paid"`
	EndReason       exchange.EndReason `json:"end_reason"`
}

var catalog = []Definition{
	{
		ID: "first-spread-v1", Revision: "1", Title: "First Spread",
		Briefing:  "A calm opening desk with balanced customer flow. Learn how your quote width changes the chance of trading.",
		Objective: "Finish with the highest marked equity you can while staying aware of every fill.",
		Config:    config(8, 101, -25, 25, 5, 10),
	},
	{
		ID: "inventory-pressure-v1", Revision: "1", Title: "Inventory Pressure",
		Briefing:  "Larger customer clips and a livelier mark make every accumulated unit matter.",
		Objective: "Earn P&L, but notice when your position becomes the real trade.",
		Config:    config(10, 202, -90, 150, 6, 18),
	},
	{
		ID: "volatility-shock-v1", Revision: "1", Title: "Volatility Shock",
		Briefing:  "The mark can move sharply after execution. Spread income is not protection from directional exposure.",
		Objective: "Finish with the highest P&L through a more volatile price path.",
		Config:    config(8, 303, -300, 425, 4, 12),
	},
}

func config(turns int, seed uint64, minMove, maxMove int64, maxOrders int, maxQty int64) exchange.Config {
	return exchange.Config{
		Instrument: "SIM", StartingCash: fixed.Money(10_000_000_000_000), StartingMark: fixed.Price(1_000_000),
		StoragePerUnit: fixed.Price(10_000), NumTurns: turns, InitialMarginBps: 5000, MaintenanceMarginBps: 2500,
		MaxPosition: fixed.Qty(10_000_000), MaxOrdersPerTurn: maxOrders, MaxOrderQty: fixed.Qty(maxQty * 10_000),
		MaxFlowSlippageBps: 200, MinMoveBps: minMove, MaxMoveBps: maxMove, Seed: seed,
	}
}

func List() []Snapshot {
	items := make([]Snapshot, 0, len(catalog))
	for _, definition := range catalog {
		items = append(items, definition.Snapshot())
	}
	sort.Slice(items, func(i, j int) bool { return items[i].ID < items[j].ID })
	return items
}

func Get(id string) (Definition, bool) {
	for _, definition := range catalog {
		if definition.ID == id {
			return definition, true
		}
	}
	return Definition{}, false
}

func (d Definition) Snapshot() Snapshot {
	return Snapshot{ID: d.ID, Revision: d.Revision, Title: d.Title, Briefing: d.Briefing, Objective: d.Objective, Turns: d.Config.NumTurns}
}

func ValidateCatalog() error {
	seen := make(map[string]bool, len(catalog))
	for _, definition := range catalog {
		if definition.ID == "" || definition.Revision == "" || definition.Title == "" || definition.Briefing == "" || definition.Objective == "" || seen[definition.ID] {
			return errors.New("invalid scenario catalog")
		}
		seen[definition.ID] = true
		if definition.Config.NumTurns <= 0 || definition.Config.Seed == 0 {
			return fmt.Errorf("scenario %s must be a finite seeded lesson", definition.ID)
		}
		if err := definition.Config.Validate(); err != nil {
			return fmt.Errorf("scenario %s: %w", definition.ID, err)
		}
	}
	return nil
}

func Coach(before exchange.State, result exchange.Result) *Coaching {
	after := result.State
	if after.IsOver {
		return &Coaching{Code: "terminal", Title: "Session complete", Body: "The scenario is over. Review the turns where inventory and the reference mark changed the outcome."}
	}
	if abs(after.Position) > abs(before.Position) && abs(after.Position) > 0 {
		return &Coaching{Code: "inventory-built", Title: "Inventory increased", Body: fmt.Sprintf("You now carry %s units. The next reference-price move matters more than it did before.", after.Position)}
	}
	if (before.Position > 0 && after.Mark < before.Mark) || (before.Position < 0 && after.Mark > before.Mark) {
		return &Coaching{Code: "mark-against-position", Title: "The mark moved against you", Body: "Your carried inventory lost value as the reference mark moved. Spread income and directional exposure are separate."}
	}
	if result.Summary.UnitsTraded == 0 {
		return &Coaching{Code: "no-fill", Title: "No quote was crossed", Body: "You did not trade this turn. A wider quote can avoid inventory risk, but it may also leave spread income on the table."}
	}
	if result.Summary.StorageCost > 0 {
		return &Coaching{Code: "carry-cost", Title: "Inventory has a carrying cost", Body: "Storage was charged on your position. Holding inventory needs to earn more than it costs."}
	}
	return &Coaching{Code: "fill-near-flat", Title: "You traded and stayed controlled", Body: "Customer flow reached your quote without leaving a large position. Keep watching whether that balance holds."}
}

func BuildRecap(snapshot Snapshot, cfg exchange.Config, records []exchange.Result, final exchange.Result) *Recap {
	startingInventoryValue, _ := fixed.Notional(cfg.StartingMark, cfg.StartingPosition)
	startEquity, _ := fixed.AddMoney(cfg.StartingCash, startingInventoryValue)
	maxInventory, units, storage := fixed.Qty(0), fixed.Qty(0), fixed.Money(0)
	include := func(result exchange.Result) {
		if abs(result.State.Position) > maxInventory {
			maxInventory = abs(result.State.Position)
		}
		units, _ = fixed.AddQty(units, result.Summary.UnitsTraded)
		storage, _ = fixed.AddMoney(storage, result.Summary.StorageCost)
	}
	for _, result := range records {
		include(result)
	}
	include(final)
	finalInventoryValue, _ := fixed.Notional(final.State.Mark, final.State.Position)
	finalEquity, _ := fixed.AddMoney(final.State.Cash, finalInventoryValue)
	pnl, _ := fixed.AddMoney(finalEquity, -startEquity)
	body := fmt.Sprintf("%s You finished with %s P&L, traded %s units, and carried as much as %s units of inventory.", snapshot.Objective, pnl, units, maxInventory)
	return &Recap{Headline: "Review the desk", Body: body, FinalEquity: finalEquity, TotalPnL: pnl, MaxAbsInventory: maxInventory, UnitsTraded: units, StoragePaid: storage, EndReason: final.State.Reason}
}

func abs(value fixed.Qty) fixed.Qty {
	if value < 0 {
		return -value
	}
	return value
}
