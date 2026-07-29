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
	ID            string
	Revision      string
	Title         string
	Briefing      string
	Objective     string
	Tutorial      []TutorialStep
	Reflection    string
	ScorecardKind string
	Config        exchange.Config
}

type TutorialStep struct {
	Title string `json:"title"`
	Body  string `json:"body"`
}

// Snapshot is persisted with every game so its lesson cannot change when the
// in-code catalog evolves.
type Snapshot struct {
	ID            string         `json:"id"`
	Revision      string         `json:"revision"`
	Title         string         `json:"title"`
	Briefing      string         `json:"briefing"`
	Objective     string         `json:"objective"`
	Tutorial      []TutorialStep `json:"tutorial,omitempty"`
	Reflection    string         `json:"reflection,omitempty"`
	ScorecardKind string         `json:"scorecard_kind,omitempty"`
	Turns         int            `json:"turns"`
}

type Coaching struct {
	Code  string `json:"code"`
	Title string `json:"title"`
	Body  string `json:"body"`
}

type Recap struct {
	Headline              string             `json:"headline"`
	Body                  string             `json:"body"`
	FinalEquity           fixed.Money        `json:"final_equity"`
	TotalPnL              fixed.Money        `json:"total_pnl"`
	MaxAbsInventory       fixed.Qty          `json:"max_abs_inventory"`
	UnitsTraded           fixed.Qty          `json:"units_traded"`
	StoragePaid           fixed.Money        `json:"storage_paid"`
	AdverseSelectionTurns int                `json:"adverse_selection_turns,omitempty"`
	Scorecard             *Scorecard         `json:"scorecard,omitempty"`
	EndReason             exchange.EndReason `json:"end_reason"`
}

type Scorecard struct {
	FocusLabel string `json:"focus_label"`
	FocusValue string `json:"focus_value"`
	FocusNote  string `json:"focus_note"`
	Reflection string `json:"reflection"`
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
		Reflection:    "Before moving on, be able to explain why an order filled or expired, whether you are long or short, and what one quote change you made to manage that risk.",
		ScorecardKind: "matched_volume",
		Config:        config(8, 101, -25, 25, 5, 10),
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
		Reflection:    "Before moving on, identify your largest inventory, name the quote skew you used to reduce it, and explain whether it balanced risk without giving away more spread than necessary.",
		ScorecardKind: "peak_inventory",
		Config:        config(10, 202, -90, 150, 6, 18),
	},
	{
		ID: "volatility-shock-v1", Revision: "2", Title: "Volatility Shock",
		Briefing:  "The mark can move sharply after execution. Spread income is not protection from directional exposure.",
		Objective: "Finish with the highest P&L through a more volatile price path.",
		Tutorial: []TutorialStep{
			{Title: "Start protective", Body: "Begin with enough spread to make each fill intentional. In this lesson the reference mark can move sharply after customer flow, so a tight quote is not automatically a better quote."},
			{Title: "Spot adverse selection", Body: "When a customer fills you and the mark then moves against the inventory you received, treat the sequence as adverse selection. The fill earned spread, but the new position immediately became more expensive to carry."},
			{Title: "Protect before pursuing flow", Body: "After an adverse move, widen first to slow new exposure. Then skew: long inventory means shift both prices down; short inventory means shift both prices up. Do not tighten just to win the next trade."},
			{Title: "Separate two P&Ls", Body: "Use the turn narrative and audit to separate execution from marking. A trade can look favorable at its fill price while the reference mark makes the resulting inventory less valuable."},
			{Title: "Use the quiet turns", Body: "No-fill turns can be useful protection. Decide whether your next quote should stay defensive, or whether inventory is controlled enough to cautiously invite flow again."},
		},
		Reflection:    "Before moving on, identify the turn where a fill and mark move worked against you, explain why that was adverse selection, and describe how you widened or skewed the next quote to protect the desk.",
		ScorecardKind: "adverse_selection_turns",
		Config:        config(8, 303, -300, 425, 4, 12),
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
			definition.Tutorial = cloneTutorial(definition.Tutorial)
			return definition, true
		}
	}
	return Definition{}, false
}

func (d Definition) Snapshot() Snapshot {
	return Snapshot{ID: d.ID, Revision: d.Revision, Title: d.Title, Briefing: d.Briefing, Objective: d.Objective, Tutorial: cloneTutorial(d.Tutorial), Reflection: d.Reflection, ScorecardKind: d.ScorecardKind, Turns: d.Config.NumTurns}
}

func cloneTutorial(tutorial []TutorialStep) []TutorialStep {
	return append([]TutorialStep(nil), tutorial...)
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
		if definition.ScorecardKind == "" {
			return fmt.Errorf("scenario %s needs a scorecard kind", definition.ID)
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
	if snapshot.ID == "volatility-shock-v1" {
		return coachVolatilityShock(before, result)
	}
	return coachGeneral(before, result)
}

func coachGeneral(before exchange.State, result exchange.Result) *Coaching {
	after := result.State
	if after.IsOver {
		return &Coaching{Code: "terminal", Title: "Session complete", Body: "The scenario is over. Review the turns where inventory and the reference mark changed the outcome."}
	}
	if hasGreaterMagnitude(after.Position, before.Position) && after.Position != 0 {
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
		return &Coaching{Code: "short-skew", Title: "Skew up to reduce a short", Body: fmt.Sprintf("You are short %s units. On the next quote, shift both prices up: the higher bid invites offsetting sells from customers, while the higher ask discourages more selling from you.", magnitudeString(after.Position))}
	}
	if result.Summary.UnitsTraded == 0 {
		return &Coaching{Code: "pressure-no-fill", Title: "No fill is information", Body: "Your quote did not cross any customer limit. If you are comfortable being flat, tighten by one cent to test for more flow; otherwise keep your protection."}
	}
	return &Coaching{Code: "pressure-flat", Title: "Flat after the flow", Body: "The larger clips did not leave you with inventory this turn. Keep the spread deliberate and use the audit before tightening for more flow."}
}

func coachVolatilityShock(before exchange.State, result exchange.Result) *Coaching {
	after := result.State
	if after.IsOver {
		return &Coaching{Code: "volatility-shock-complete", Title: "Adverse selection review", Body: "Find the fill-and-mark sequence that created the most risk, then review whether your next quote widened and skewed enough to protect the desk."}
	}
	if isAdverseSelection(before, result) && after.Position > 0 && after.Mark < before.Mark {
		return &Coaching{Code: "long-adverse-selection", Title: "Adverse selection: long into a drop", Body: "You were filled into a long position and the mark fell. Protect first: widen the spread, then shift both prices down to make your ask more competitive and avoid buying more."}
	}
	if isAdverseSelection(before, result) && after.Position < 0 && after.Mark > before.Mark {
		return &Coaching{Code: "short-adverse-selection", Title: "Adverse selection: short into a rise", Body: "You were filled into a short position and the mark rose. Protect first: widen the spread, then shift both prices up to make your bid more competitive and avoid selling more."}
	}
	if after.Position > 0 {
		return &Coaching{Code: "shock-long-protect", Title: "Long inventory is live risk", Body: "The mark can move sharply in this lesson. Keep a protective spread and shift both prices down until your long inventory is reduced; do not tighten merely to recover P&L."}
	}
	if after.Position < 0 {
		return &Coaching{Code: "shock-short-protect", Title: "Short inventory is live risk", Body: "The mark can move sharply in this lesson. Keep a protective spread and shift both prices up until your short inventory is reduced; do not tighten merely to recover P&L."}
	}
	if result.Summary.UnitsTraded == 0 {
		return &Coaching{Code: "shock-no-fill", Title: "A quiet turn can protect you", Body: "No customer limit crossed your quote. With no inventory, use this pause to decide whether your spread should remain defensive before inviting more flow."}
	}
	return &Coaching{Code: "shock-flat", Title: "Flat after the shock", Body: "You traded without carrying inventory into the next move. Keep distinguishing spread capture from mark risk before you tighten again."}
}

func isAdverseSelection(before exchange.State, result exchange.Result) bool {
	after := result.State
	return result.Summary.UnitsTraded > 0 && addedRisk(before.Position, after.Position) && ((after.Position > 0 && after.Mark < before.Mark) || (after.Position < 0 && after.Mark > before.Mark))
}

func addedRisk(before, after fixed.Qty) bool {
	if after == 0 {
		return false
	}
	if before == 0 || (before > 0 && after < 0) || (before < 0 && after > 0) {
		return true
	}
	return hasGreaterMagnitude(after, before)
}

func hasGreaterMagnitude(after, before fixed.Qty) bool {
	afterMagnitude, afterErr := fixed.AbsQtyChecked(after)
	beforeMagnitude, beforeErr := fixed.AbsQtyChecked(before)
	if afterErr != nil {
		return beforeErr == nil
	}
	if beforeErr != nil {
		return false
	}
	return afterMagnitude > beforeMagnitude
}

func magnitudeString(value fixed.Qty) string {
	magnitude, err := fixed.AbsQtyChecked(value)
	if err == nil {
		return magnitude.String()
	}
	// Qty.String handles MinInt64 without negating it; remove its sign for prose.
	return value.String()[1:]
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
	adverseSelectionTurns := 0
	position := cfg.StartingPosition
	previousMark := cfg.StartingMark
	includePosition := func(next fixed.Qty) error {
		magnitude, err := fixed.AbsQtyChecked(next)
		if err != nil {
			return err
		}
		position = next
		if magnitude > maxInventory {
			maxInventory = magnitude
		}
		return nil
	}
	if err := includePosition(position); err != nil {
		return nil, err
	}
	include := func(result exchange.Result) error {
		priorPosition := position
		for _, event := range result.Events {
			if event.Trade == nil {
				continue
			}
			if event.Trade.BuyerID == exchange.PlayerAccount {
				next, err := fixed.AddQty(position, event.Trade.Quantity)
				if err != nil {
					return err
				}
				if err := includePosition(next); err != nil {
					return err
				}
			}
			if event.Trade.SellerID == exchange.PlayerAccount {
				next, err := fixed.SubQty(position, event.Trade.Quantity)
				if err != nil {
					return err
				}
				if err := includePosition(next); err != nil {
					return err
				}
			}
		}
		stateMagnitude, err := fixed.AbsQtyChecked(result.State.Position)
		if err != nil {
			return err
		}
		if stateMagnitude > maxInventory {
			maxInventory = stateMagnitude
		}
		units, err = fixed.AddQty(units, result.Summary.UnitsTraded)
		if err != nil {
			return err
		}
		storage, err = fixed.AddMoney(storage, result.Summary.StorageCost)
		if err != nil {
			return err
		}
		if result.Summary.UnitsTraded > 0 && addedRisk(priorPosition, result.State.Position) && ((result.State.Position > 0 && result.State.Mark < previousMark) || (result.State.Position < 0 && result.State.Mark > previousMark)) {
			adverseSelectionTurns++
		}
		position = result.State.Position
		previousMark = result.State.Mark
		return nil
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
	pnl, err := fixed.SubMoney(finalEquity, startEquity)
	if err != nil {
		return nil, err
	}
	body := fmt.Sprintf("%s You finished with %s P&L, traded %s units, and carried as much as %s units of inventory.", snapshot.Objective, pnl, units, maxInventory)
	return &Recap{Headline: "Review the desk", Body: body, FinalEquity: finalEquity, TotalPnL: pnl, MaxAbsInventory: maxInventory, UnitsTraded: units, StoragePaid: storage, AdverseSelectionTurns: adverseSelectionTurns, Scorecard: buildScorecard(snapshot, units, maxInventory, adverseSelectionTurns), EndReason: final.State.Reason}, nil
}

func buildScorecard(snapshot Snapshot, units, maxInventory fixed.Qty, adverseSelectionTurns int) *Scorecard {
	scorecard := &Scorecard{Reflection: snapshot.Reflection}
	if scorecard.Reflection == "" {
		scorecard.Reflection = legacyReflection(snapshot.ID)
	}
	kind := snapshot.ScorecardKind
	if kind == "" {
		kind = legacyScorecardKind(snapshot.ID)
	}
	switch kind {
	case "matched_volume":
		scorecard.FocusLabel = "Matched volume"
		scorecard.FocusValue = units.String()
		scorecard.FocusNote = "Units matched by customer flow. More volume is useful only when the inventory it creates remains manageable."
	case "peak_inventory":
		scorecard.FocusLabel = "Peak inventory"
		scorecard.FocusValue = maxInventory.String()
		scorecard.FocusNote = "Largest position carried during the lesson. Review whether your skew reduced it after the larger clips arrived."
	case "adverse_selection_turns":
		scorecard.FocusLabel = "Adverse selection turns"
		scorecard.FocusValue = fmt.Sprintf("%d", adverseSelectionTurns)
		scorecard.FocusNote = "Turns where customer flow left inventory exposed to a mark move against it. Protection matters more than chasing the next fill."
	default:
		return nil
	}
	return scorecard
}

func legacyScorecardKind(scenarioID string) string {
	switch scenarioID {
	case "first-spread-v1":
		return "matched_volume"
	case "inventory-pressure-v1":
		return "peak_inventory"
	case "volatility-shock-v1":
		return "adverse_selection_turns"
	default:
		return ""
	}
}

func legacyReflection(scenarioID string) string {
	switch scenarioID {
	case "first-spread-v1":
		return "Explain why an order filled or expired, whether you were long or short, and what quote adjustment you made to manage risk."
	case "inventory-pressure-v1":
		return "Identify your largest inventory, the quote skew you used to reduce it, and whether that protected the desk without giving away too much spread."
	case "volatility-shock-v1":
		return "Identify the adverse fill-and-mark sequence, then explain how you widened or skewed the next quote to protect the desk."
	default:
		return "Review the risk carried and the quote adjustment used to manage it."
	}
}
