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
	ID         string
	Revision   string
	Title      string
	Briefing   string
	Objective  string
	Tutorial   []TutorialStep
	Reflection string
	Config     exchange.Config
}

type TutorialStep struct {
	Title string `json:"title"`
	Body  string `json:"body"`
}

// Snapshot is persisted with every game so its lesson cannot change when the
// in-code catalog evolves.
type Snapshot struct {
	ID         string         `json:"id"`
	Revision   string         `json:"revision"`
	Title      string         `json:"title"`
	Briefing   string         `json:"briefing"`
	Objective  string         `json:"objective"`
	Tutorial   []TutorialStep `json:"tutorial,omitempty"`
	Reflection string         `json:"reflection,omitempty"`
	Turns      int            `json:"turns"`
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
		ID: "first-spread-v1", Revision: "2", Title: "First Spread",
		Briefing:  "A calm opening desk with balanced customer flow. Learn how your quote width changes the chance of trading.",
		Objective: "Finish with the highest marked equity you can while staying aware of every fill.",
		Tutorial: []TutorialStep{
			{Title: "Start with a balanced quote", Body: "For the first turn, try a bid of $99.50 and an ask of $100.50 around the $100 reference mark. This gives customers room to cross either side."},
			{Title: "Read the customer tape", Body: "After you post, count the customer IOC orders in the turn audit. For each one, compare its limit with your bid or ask and explain why it filled or expired."},
			{Title: "Name the inventory you earned", Body: "If you bought, you are long and the next price move can hurt you if the mark falls. If you sold, you are short and a rising mark can hurt. Check the position card before quoting again."},
			{Title: "Make one deliberate change", Body: "While flat, tighten by one cent to invite more flow. While long, shift both prices down; while short, shift both prices up. Change one idea at a time, then use the next audit to judge it."},
		},
		Reflection: "Before moving on, be able to explain why an order filled or expired, whether you are long or short, and what one quote change you made to manage that risk.",
		Config:     config(8, 101, -25, 25, 5, 10),
	},
	{
		ID: "inventory-pressure-v1", Revision: "2", Title: "Inventory Pressure",
		Briefing:  "Larger customer clips and a livelier mark make every accumulated unit matter.",
		Objective: "Earn P&L, but notice when your position becomes the real trade.",
		Tutorial: []TutorialStep{
			{Title: "Start controlled, then observe", Body: "Begin around the reference mark with a moderate spread. Customer clips are larger here, so one crossed quote can change inventory faster than in First Spread."},
			{Title: "Call your position by name", Body: "After every fill, say it plainly: long means you own inventory; short means you owe it. Use the position card and turn audit to identify which customer order created it."},
			{Title: "Skew to reduce, not to predict", Body: "Long inventory: lower both bid and ask, making your ask easier to hit while discouraging more buying. Short inventory: raise both prices, making your bid easier to hit while discouraging more selling."},
			{Title: "Keep the spread deliberate", Body: "Do not automatically widen after a fill. First shift the quote in the direction that reduces risk. Widen only if you need less flow or more protection from the next mark move."},
			{Title: "Judge the adjustment", Body: "Use the next turn audit as evidence. Did your skew attract the offsetting customer flow you wanted? Did it reduce inventory, or did the mark move against the risk you still carried?"},
		},
		Reflection: "Before moving on, identify your largest inventory, name the quote skew you used to reduce it, and explain whether it balanced risk without giving away more spread than necessary.",
		Config:     config(10, 202, -90, 150, 6, 18),
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
	tutorial := append([]TutorialStep(nil), d.Tutorial...)
	return Snapshot{ID: d.ID, Revision: d.Revision, Title: d.Title, Briefing: d.Briefing, Objective: d.Objective, Tutorial: tutorial, Reflection: d.Reflection, Turns: d.Config.NumTurns}
}

func ValidateCatalog() error {
	seen := make(map[string]bool, len(catalog))
	for _, definition := range catalog {
		if definition.ID == "" || definition.Revision == "" || definition.Title == "" || definition.Briefing == "" || definition.Objective == "" || seen[definition.ID] {
			return errors.New("invalid scenario catalog")
		}
		seen[definition.ID] = true
		for _, step := range definition.Tutorial {
			if step.Title == "" || step.Body == "" {
				return fmt.Errorf("scenario %s has an invalid tutorial step", definition.ID)
			}
		}
		if len(definition.Tutorial) > 0 && definition.Reflection == "" {
			return fmt.Errorf("scenario %s needs a tutorial reflection", definition.ID)
		}
		if definition.Config.NumTurns <= 0 || definition.Config.Seed == 0 {
			return fmt.Errorf("scenario %s must be a finite seeded lesson", definition.ID)
		}
		if err := definition.Config.Validate(); err != nil {
			return fmt.Errorf("scenario %s: %w", definition.ID, err)
		}
	}
	return nil
}

func Coach(snapshot Snapshot, before exchange.State, result exchange.Result) *Coaching {
	if snapshot.ID == "inventory-pressure-v1" {
		return coachInventoryPressure(before, result)
	}
	return coachGeneral(before, result)
}

func coachGeneral(before exchange.State, result exchange.Result) *Coaching {
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

func coachInventoryPressure(before exchange.State, result exchange.Result) *Coaching {
	after := result.State
	if after.IsOver {
		return &Coaching{Code: "inventory-pressure-complete", Title: "Inventory review", Body: "Review your largest position, the quote skew you used, and whether it reduced risk before the next mark move."}
	}
	if after.Position > 0 && after.Mark < before.Mark {
		return &Coaching{Code: "long-mark-against", Title: "Long inventory, lower mark", Body: "You are long and the mark fell. Next turn, lower both bid and ask to make your ask more competitive and avoid adding more inventory."}
	}
	if after.Position < 0 && after.Mark > before.Mark {
		return &Coaching{Code: "short-mark-against", Title: "Short inventory, higher mark", Body: "You are short and the mark rose. Next turn, raise both bid and ask to make your bid more competitive and avoid selling more inventory."}
	}
	if after.Position > 0 {
		return &Coaching{Code: "long-skew", Title: "Skew down to reduce a long", Body: fmt.Sprintf("You are long %s units. On the next quote, shift both prices down: the lower ask invites offsetting buys from customers, while the lower bid discourages more buying from you.", after.Position)}
	}
	if after.Position < 0 {
		return &Coaching{Code: "short-skew", Title: "Skew up to reduce a short", Body: fmt.Sprintf("You are short %s units. On the next quote, shift both prices up: the higher bid invites offsetting sells from customers, while the higher ask discourages more selling from you.", abs(after.Position))}
	}
	if result.Summary.UnitsTraded == 0 {
		return &Coaching{Code: "pressure-no-fill", Title: "No fill is information", Body: "Your quote did not cross any customer limit. If you are comfortable being flat, tighten by one cent to test for more flow; otherwise keep your protection."}
	}
	return &Coaching{Code: "pressure-flat", Title: "Flat after the flow", Body: "The larger clips did not leave you with inventory this turn. Keep the spread deliberate and use the audit before tightening for more flow."}
}

func BuildRecap(snapshot Snapshot, cfg exchange.Config, records []exchange.Result, final exchange.Result) (*Recap, error) {
	startingInventoryValue, err := fixed.Notional(cfg.StartingMark, cfg.StartingPosition)
	if err != nil {
		return nil, err
	}
	startEquity, err := fixed.AddMoney(cfg.StartingCash, startingInventoryValue)
	if err != nil {
		return nil, err
	}
	maxInventory, units, storage := fixed.Qty(0), fixed.Qty(0), fixed.Money(0)
	position := cfg.StartingPosition
	includePosition := func(next fixed.Qty) {
		position = next
		if abs(position) > maxInventory {
			maxInventory = abs(position)
		}
	}
	includePosition(position)
	include := func(result exchange.Result) error {
		for _, event := range result.Events {
			if event.Trade == nil {
				continue
			}
			if event.Trade.BuyerID == exchange.PlayerAccount {
				next, err := fixed.AddQty(position, event.Trade.Quantity)
				if err != nil {
					return err
				}
				includePosition(next)
			}
			if event.Trade.SellerID == exchange.PlayerAccount {
				next, err := fixed.SubQty(position, event.Trade.Quantity)
				if err != nil {
					return err
				}
				includePosition(next)
			}
		}
		if abs(result.State.Position) > maxInventory {
			maxInventory = abs(result.State.Position)
		}
		position = result.State.Position
		var err error
		units, err = fixed.AddQty(units, result.Summary.UnitsTraded)
		if err != nil {
			return err
		}
		storage, err = fixed.AddMoney(storage, result.Summary.StorageCost)
		return err
	}
	for _, result := range records {
		if err := include(result); err != nil {
			return nil, err
		}
	}
	if err := include(final); err != nil {
		return nil, err
	}
	finalInventoryValue, err := fixed.Notional(final.State.Mark, final.State.Position)
	if err != nil {
		return nil, err
	}
	finalEquity, err := fixed.AddMoney(final.State.Cash, finalInventoryValue)
	if err != nil {
		return nil, err
	}
	pnl, err := fixed.AddMoney(finalEquity, -startEquity)
	if err != nil {
		return nil, err
	}
	body := fmt.Sprintf("%s You finished with %s P&L, traded %s units, and carried as much as %s units of inventory.", snapshot.Objective, pnl, units, maxInventory)
	return &Recap{Headline: "Review the desk", Body: body, FinalEquity: finalEquity, TotalPnL: pnl, MaxAbsInventory: maxInventory, UnitsTraded: units, StoragePaid: storage, EndReason: final.State.Reason}, nil
}

func abs(value fixed.Qty) fixed.Qty {
	if value < 0 {
		return -value
	}
	return value
}
