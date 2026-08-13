// Package exchange implements the deterministic single-instrument exchange
// kernel used by the game and future bot-facing services.
package exchange

import (
	"errors"
	"fmt"
	"math/rand/v2"
	"sort"

	"market-maker/internal/fixed"
	"market-maker/internal/orderbook"
)

const (
	PlayerAccount = "player"
	FlowAccount   = "simulated-flow"
)

type Side string

const (
	Buy  Side = "buy"
	Sell Side = "sell"
)

type TimeInForce string

const (
	GTC TimeInForce = "gtc"
	IOC TimeInForce = "ioc"
)

type EndReason string

const (
	NotOver       EndReason = ""
	TurnsComplete EndReason = "turns_complete"
	TimeExpired   EndReason = "time_expired"
	MarginBreach  EndReason = "margin_breach"
	Insolvent     EndReason = "insolvent"
	PlayerQuit    EndReason = "player_quit"
)

type SimulationVersion int

const (
	SimulationVersionLegacy           SimulationVersion = 1
	SimulationVersionAdverseSelection SimulationVersion = 2
)

type Config struct {
	Instrument           string            `json:"instrument"`
	StartingCash         fixed.Money       `json:"starting_cash"`
	StartingPosition     fixed.Qty         `json:"starting_position"`
	StartingMark         fixed.Price       `json:"starting_mark"`
	StoragePerUnit       fixed.Price       `json:"storage_per_unit"`
	NumTurns             int               `json:"num_turns"`
	InitialMarginBps     int64             `json:"initial_margin_bps"`
	MaintenanceMarginBps int64             `json:"maintenance_margin_bps"`
	MaxPosition          fixed.Qty         `json:"max_position"`
	MaxOrdersPerTurn     int               `json:"max_orders_per_turn"`
	MaxOrderQty          fixed.Qty         `json:"max_order_qty"`
	MaxFlowSlippageBps   int64             `json:"max_flow_slippage_bps"`
	MinMoveBps           int64             `json:"min_move_bps"`
	MaxMoveBps           int64             `json:"max_move_bps"`
	Seed                 uint64            `json:"seed"`
	SimulationVersion    SimulationVersion `json:"simulation_version,omitempty"`
	InformedFlowBps      int64             `json:"informed_flow_bps,omitempty"`
}

func (c Config) Validate() error {
	if c.Instrument == "" {
		return errors.New("instrument is required")
	}
	if c.StartingCash < 0 {
		return errors.New("starting cash must be non-negative")
	}
	if !c.StartingMark.Positive() {
		return errors.New("starting mark must be positive")
	}
	if c.StoragePerUnit < 0 {
		return errors.New("storage per unit must be non-negative")
	}
	if c.NumTurns < 0 {
		return errors.New("number of turns must be non-negative")
	}
	if c.InitialMarginBps < 0 || c.InitialMarginBps > 10_000 || c.MaintenanceMarginBps < 0 || c.MaintenanceMarginBps > c.InitialMarginBps {
		return errors.New("invalid margin scenario")
	}
	if c.MaxPosition <= 0 {
		return errors.New("max position must be positive")
	}
	startingPositionAbs, err := fixed.AbsQtyChecked(c.StartingPosition)
	if err != nil {
		return errors.New("starting position exceeds supported range")
	}
	if startingPositionAbs > c.MaxPosition {
		return errors.New("starting position exceeds maximum position")
	}
	if c.MaxOrdersPerTurn < 0 || c.MaxOrdersPerTurn > 1_000 || !c.MaxOrderQty.Positive() || c.MaxFlowSlippageBps < 0 || c.MaxFlowSlippageBps > 10_000 {
		return errors.New("invalid flow scenario")
	}
	if c.MinMoveBps > c.MaxMoveBps || c.MinMoveBps <= -10_000 || c.MaxMoveBps > 10_000 {
		return errors.New("invalid mark movement range")
	}
	if _, err := fixed.Notional(c.StartingMark, c.MaxPosition); err != nil {
		return errors.New("position and mark exceed supported range")
	}
	if _, err := fixed.Notional(c.StoragePerUnit, c.MaxPosition); err != nil {
		return errors.New("storage and position exceed supported range")
	}
	startingNotional, err := fixed.Notional(c.StartingMark, c.StartingPosition)
	if err != nil {
		return errors.New("starting equity exceeds supported range")
	}
	startingEquity, err := fixed.AddMoney(c.StartingCash, startingNotional)
	if err != nil || startingEquity <= 0 {
		return errors.New("starting account has non-positive equity")
	}
	startingGross, err := fixed.Notional(c.StartingMark, startingPositionAbs)
	if err != nil {
		return errors.New("starting exposure exceeds supported range")
	}
	startingMargin, err := fixed.ScaleMoney(startingGross, c.InitialMarginBps, 10_000)
	if err != nil || startingEquity < startingMargin {
		return errors.New("starting account violates initial margin")
	}
	if c.Seed == 0 {
		return errors.New("resolved seed must be non-zero")
	}
	switch c.SimulationVersion {
	case 0, SimulationVersionLegacy:
		if c.InformedFlowBps != 0 {
			return errors.New("legacy simulation requires zero informed flow")
		}
	case SimulationVersionAdverseSelection:
		if c.InformedFlowBps < 0 || c.InformedFlowBps > 10_000 {
			return errors.New("informed flow must be between 0 and 10000 bps")
		}
	default:
		return errors.New("unsupported simulation version")
	}
	return nil
}

type Account struct {
	ID               string      `json:"id"`
	Cash             fixed.Money `json:"cash"`
	ReservedCash     fixed.Money `json:"reserved_cash"`
	Position         fixed.Qty   `json:"position"`
	OpenBuyQuantity  fixed.Qty   `json:"open_buy_quantity"`
	OpenSellQuantity fixed.Qty   `json:"open_sell_quantity"`
	External         bool        `json:"external"`
}

// AccountState is the read model exposed by the venue. Cash remains total
// settled cash; AvailableCash excludes cash reserved by live buy orders.
type AccountState struct {
	ID               string      `json:"id"`
	Cash             fixed.Money `json:"cash"`
	ReservedCash     fixed.Money `json:"reserved_cash"`
	AvailableCash    fixed.Money `json:"available_cash"`
	Position         fixed.Qty   `json:"position"`
	OpenBuyQuantity  fixed.Qty   `json:"open_buy_quantity"`
	OpenSellQuantity fixed.Qty   `json:"open_sell_quantity"`
	Equity           fixed.Money `json:"equity"`
}

type Order struct {
	ID        uint64      `json:"id"`
	Sequence  uint64      `json:"sequence"`
	AccountID string      `json:"account_id"`
	Side      Side        `json:"side"`
	Price     fixed.Price `json:"price"`
	Quantity  fixed.Qty   `json:"quantity"`
	Remaining fixed.Qty   `json:"remaining"`
	TIF       TimeInForce `json:"time_in_force"`
	Informed  bool        `json:"informed,omitempty"`
}

type Trade struct {
	ID           uint64      `json:"id"`
	MakerOrderID uint64      `json:"maker_order_id"`
	TakerOrderID uint64      `json:"taker_order_id"`
	Price        fixed.Price `json:"price"`
	Quantity     fixed.Qty   `json:"quantity"`
	BuyerID      string      `json:"buyer_id"`
	SellerID     string      `json:"seller_id"`
	Informed     bool        `json:"informed,omitempty"`
}

type Event struct {
	Sequence      uint64        `json:"sequence"`
	CommandID     string        `json:"command_id,omitempty"`
	Type          string        `json:"type"`
	Order         *Order        `json:"order,omitempty"`
	PreviousOrder *Order        `json:"previous_order,omitempty"`
	Trade         *Trade        `json:"trade,omitempty"`
	Account       *AccountState `json:"account,omitempty"`
	Amount        fixed.Money   `json:"amount,omitempty"`
	Mark          fixed.Price   `json:"mark,omitempty"`
	Reason        EndReason     `json:"reason,omitempty"`
	Message       string        `json:"message,omitempty"`
	PreviousMark  fixed.Price   `json:"previous_mark,omitempty"`
}

type PnLAttribution struct {
	ExecutionEdge    fixed.Money `json:"execution_edge"`
	InventoryMarkPnL fixed.Money `json:"inventory_mark_pnl"`
	StoragePnL       fixed.Money `json:"storage_pnl"`
}

type Summary struct {
	OrdersReceived       int             `json:"orders_received"`
	UnitsTraded          fixed.Qty       `json:"units_traded"`
	NetFillCash          fixed.Money     `json:"net_fill_cash"`
	StorageCost          fixed.Money     `json:"storage_cost"`
	TurnPnL              fixed.Money     `json:"turn_pnl"`
	ActionPnL            fixed.Money     `json:"action_pnl,omitempty"`
	BuyVolume            fixed.Qty       `json:"buy_volume"`
	SellVolume           fixed.Qty       `json:"sell_volume"`
	PnLAttribution       *PnLAttribution `json:"pnl_attribution,omitempty"`
	InformedOrders       int             `json:"informed_orders,omitempty"`
	InformedOrdersFilled int             `json:"informed_orders_filled,omitempty"`
	InformedUnitsTraded  fixed.Qty       `json:"informed_units_traded,omitempty"`
	InformedFlowPnL      fixed.Money     `json:"informed_flow_pnl,omitempty"`
}

type State struct {
	Version  uint64      `json:"version"`
	Turn     int         `json:"turn"`
	Cash     fixed.Money `json:"cash"`
	Position fixed.Qty   `json:"position"`
	Mark     fixed.Price `json:"mark"`
	Equity   fixed.Money `json:"equity"`
	IsOver   bool        `json:"is_over"`
	Reason   EndReason   `json:"reason"`
	BestBid  fixed.Price `json:"best_bid"`
	BestAsk  fixed.Price `json:"best_ask"`
}

type DepthLevel struct {
	Price         fixed.Price `json:"price"`
	Quantity      fixed.Qty   `json:"quantity"`
	OrderCount    int         `json:"order_count"`
	OwnedQuantity fixed.Qty   `json:"owned_quantity"`
}

type Depth struct {
	Bids []DepthLevel `json:"bids"`
	Asks []DepthLevel `json:"asks"`
}

type Result struct {
	State   State         `json:"state"`
	Summary Summary       `json:"summary"`
	Events  []Event       `json:"events"`
	Ledger  []LedgerEntry `json:"ledger,omitempty"`
}

// Command is persisted verbatim before its result is acknowledged. IDs are
// client-generated idempotency keys; version is checked by the server before
// calling Execute.
type Command struct {
	ID              string      `json:"id"`
	Type            string      `json:"type"`
	ExpectedVersion uint64      `json:"expected_version"`
	AccountID       string      `json:"account_id,omitempty"`
	Bid             fixed.Price `json:"bid,omitempty"`
	Ask             fixed.Price `json:"ask,omitempty"`
	Side            Side        `json:"side,omitempty"`
	Price           fixed.Price `json:"price,omitempty"`
	Quantity        fixed.Qty   `json:"quantity,omitempty"`
	TIF             TimeInForce `json:"time_in_force,omitempty"`
	OrderID         uint64      `json:"order_id,omitempty"`
	InitialCash     fixed.Money `json:"initial_cash,omitempty"`
	InitialPosition fixed.Qty   `json:"initial_position,omitempty"`
}

const (
	CommandOpenAccount  = "open_account"
	CommandPlaceOrder   = "place_order"
	CommandCancelOrder  = "cancel_order"
	CommandReplaceOrder = "replace_order"
	CommandSubmitQuote  = "submit_quote" // scenario adapter, not a venue primitive
	CommandQuit         = "quit"         // scenario adapter, not a venue primitive
)

// Engine owns the authoritative account state and price-time-priority book.
// It is deliberately single-writer; callers serialize it per game/market.
type Engine struct {
	cfg         Config
	rng         *rand.Rand
	pcg         *rand.PCG
	flowRNG     splitMix64
	markRNG     splitMix64
	informedRNG splitMix64
	accounts    map[string]*Account
	book        *orderbook.Book
	ledger      ledger
	nextOrder   uint64
	nextTrade   uint64
	nextEvent   uint64
	version     uint64
	turn        int
	mark        fixed.Price
	isOver      bool
	reason      EndReason
}

const (
	flowSeedDomain     uint64 = 0x666c6f772d737472
	markSeedDomain     uint64 = 0x6d61726b2d737472
	informedSeedDomain uint64 = 0x696e666f726d6564
)

type splitMix64 struct {
	state uint64
}

func (r *splitMix64) next() uint64 {
	r.state += 0x9e3779b97f4a7c15
	z := r.state
	z = (z ^ (z >> 30)) * 0xbf58476d1ce4e5b9
	z = (z ^ (z >> 27)) * 0x94d049bb133111eb
	return z ^ (z >> 31)
}

func (r *splitMix64) bounded(bound uint64) uint64 {
	threshold := -bound % bound
	for {
		value := r.next()
		if value >= threshold {
			return value % bound
		}
	}
}

func New(cfg Config) (*Engine, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	pcg := rand.NewPCG(cfg.Seed, cfg.Seed>>1)
	e := &Engine{
		cfg: cfg, rng: rand.New(pcg), pcg: pcg,
		flowRNG: splitMix64{state: cfg.Seed ^ flowSeedDomain}, markRNG: splitMix64{state: cfg.Seed ^ markSeedDomain}, informedRNG: splitMix64{state: cfg.Seed ^ informedSeedDomain},
		accounts: map[string]*Account{}, book: orderbook.New(), ledger: newLedger(), mark: cfg.StartingMark,
	}
	e.accounts[PlayerAccount] = &Account{ID: PlayerAccount, Cash: cfg.StartingCash, Position: cfg.StartingPosition}
	e.accounts[FlowAccount] = &Account{ID: FlowAccount, External: true}
	if err := e.postOpening(PlayerAccount, cfg.StartingCash, cfg.StartingPosition); err != nil {
		return nil, err
	}
	if !e.accountWithinLimits(PlayerAccount, false) {
		return nil, errors.New("starting account violates configured risk limits")
	}
	return e, nil
}

func (e *Engine) Config() Config { return e.cfg }

// transact applies a command to an isolated copy, committing only after every
// book, settlement, reservation, and journal operation has succeeded.
func (e *Engine) transact(apply func(*Engine) (Result, error)) (Result, error) {
	ledgerStart := len(e.ledger.entries)
	candidate := e.clone()
	result, err := apply(candidate)
	if err != nil {
		return Result{State: e.State()}, err
	}
	journal := candidate.ledger.entriesCopy()
	result.Ledger = journal[ledgerStart:]
	*e = *candidate
	return result, nil
}

func (e *Engine) clone() *Engine {
	clone := *e
	rngState, err := e.pcg.MarshalBinary()
	if err != nil {
		panic(err)
	}
	clone.pcg = &rand.PCG{}
	if err := clone.pcg.UnmarshalBinary(rngState); err != nil {
		panic(err)
	}
	clone.rng = rand.New(clone.pcg)
	clone.accounts = make(map[string]*Account, len(e.accounts))
	for id, account := range e.accounts {
		copy := *account
		clone.accounts[id] = &copy
	}
	clone.book = e.book.Clone()
	clone.ledger = e.ledger.clone()
	return &clone
}

func (e *Engine) State() State {
	a := e.accounts[PlayerAccount]
	eq, _ := e.equity(a)
	return State{Version: e.version, Turn: e.turn, Cash: a.Cash, Position: a.Position, Mark: e.mark, Equity: eq, IsOver: e.isOver, Reason: e.reason, BestBid: e.book.BestBid(), BestAsk: e.book.BestAsk()}
}

func (e *Engine) OpenOrders(accountID string) []Order {
	orders := e.book.Orders(accountID)
	result := make([]Order, len(orders))
	for i := range orders {
		result[i] = fromBookOrder(orders[i])
	}
	return result
}

func (e *Engine) Depth(levels int, owner string) Depth {
	if levels < 0 {
		levels = 0
	}
	owned := make(map[Side]map[fixed.Price]fixed.Qty, 2)
	owned[Buy], owned[Sell] = make(map[fixed.Price]fixed.Qty), make(map[fixed.Price]fixed.Qty)
	for _, order := range e.OpenOrders(owner) {
		owned[order.Side][order.Price] += order.Remaining
	}
	project := func(source []orderbook.Level, side Side) []DepthLevel {
		if len(source) > levels {
			source = source[:levels]
		}
		result := make([]DepthLevel, len(source))
		for index, level := range source {
			result[index] = DepthLevel{Price: level.Price, Quantity: level.Quantity, OrderCount: level.OrderCount, OwnedQuantity: owned[side][level.Price]}
		}
		return result
	}
	return Depth{Bids: project(e.book.Bids(), Buy), Asks: project(e.book.Asks(), Sell)}
}

func (e *Engine) Account(accountID string) (AccountState, bool) {
	account := e.accounts[accountID]
	if account == nil {
		return AccountState{}, false
	}
	available, err := fixed.AddMoney(account.Cash, -account.ReservedCash)
	if err != nil {
		return AccountState{}, false
	}
	equity, err := e.equity(account)
	if err != nil {
		return AccountState{}, false
	}
	return AccountState{ID: account.ID, Cash: account.Cash, ReservedCash: account.ReservedCash, AvailableCash: available, Position: account.Position, OpenBuyQuantity: account.OpenBuyQuantity, OpenSellQuantity: account.OpenSellQuantity, Equity: equity}, true
}

func (e *Engine) LedgerEntries() []LedgerEntry                { return e.ledger.entriesCopy() }
func (e *Engine) LedgerBalance(account LedgerAccount) Posting { return e.ledger.balance(account) }

// AddAccount creates a real account. It is the in-process primitive used by
// future authenticated account provisioning; the local game only creates player.
func (e *Engine) AddAccount(id string, cash fixed.Money, position fixed.Qty) error {
	if id == "" || id == FlowAccount || e.accounts[id] != nil {
		return errors.New("invalid or duplicate account")
	}
	e.accounts[id] = &Account{ID: id, Cash: cash, Position: position}
	if !e.accountWithinLimits(id, false) {
		delete(e.accounts, id)
		return errors.New("account violates configured risk limits")
	}
	if err := e.postOpening(id, cash, position); err != nil {
		delete(e.accounts, id)
		return err
	}
	return nil
}

// OpenAccount is the durable venue command for provisioning an account. The
// caller that authorizes this command belongs outside the venue; the venue
// only enforces its financial invariants.
func (e *Engine) OpenAccount(id string, cash fixed.Money, position fixed.Qty) (Result, error) {
	return e.transact(func(candidate *Engine) (Result, error) { return candidate.openAccount(id, cash, position) })
}

func (e *Engine) openAccount(id string, cash fixed.Money, position fixed.Qty) (Result, error) {
	if e.isOver {
		return Result{State: e.State()}, errors.New("game is over")
	}
	if err := e.AddAccount(id, cash, position); err != nil {
		return Result{State: e.State()}, err
	}
	e.version++
	account, _ := e.Account(id)
	return Result{State: e.State(), Events: []Event{e.emit(Event{Type: "account_opened", Account: &account})}}, nil
}

func (e *Engine) Execute(command Command) (Result, error) {
	switch command.Type {
	case CommandSubmitQuote:
		return e.SubmitQuote(command.Bid, command.Ask)
	case CommandQuit:
		return e.Quit()
	case CommandOpenAccount:
		return e.OpenAccount(command.AccountID, command.InitialCash, command.InitialPosition)
	case CommandPlaceOrder:
		return e.PlaceLimit(command.AccountID, command.Side, command.Price, command.Quantity, command.TIF)
	case CommandCancelOrder:
		return e.Cancel(command.AccountID, command.OrderID)
	case CommandReplaceOrder:
		return e.ReplaceLimit(command.AccountID, command.OrderID, command.Side, command.Price, command.Quantity, command.TIF)
	default:
		return Result{State: e.State()}, fmt.Errorf("unsupported command type %q", command.Type)
	}
}

// PlaceLimit submits an order to the price-time-priority book. It is exposed
// now so bot accounts can use the same matching kernel as simulated flow.
func (e *Engine) PlaceLimit(accountID string, side Side, price fixed.Price, qty fixed.Qty, tif TimeInForce) (Result, error) {
	return e.transact(func(candidate *Engine) (Result, error) { return candidate.placeLimit(accountID, side, price, qty, tif) })
}

func (e *Engine) placeLimit(accountID string, side Side, price fixed.Price, qty fixed.Qty, tif TimeInForce) (Result, error) {
	if e.isOver {
		return Result{State: e.State()}, errors.New("game is over")
	}
	if side != Buy && side != Sell || !price.Positive() || !qty.Positive() || (tif != GTC && tif != IOC) {
		return Result{State: e.State()}, errors.New("invalid order")
	}
	if e.accounts[accountID] == nil {
		return Result{State: e.State()}, errors.New("account not found")
	}
	if !e.canPlace(accountID, side, price, qty) {
		return Result{State: e.State()}, errors.New("order would violate cash, position, or initial-margin limits")
	}
	order := e.newOrder(accountID, side, price, qty, tif)
	events := []Event{e.emit(Event{Type: "order_accepted", Order: copyOrder(order)})}
	trades, lifecycle, err := e.submitBookOrder(order)
	if err != nil {
		return Result{State: e.State()}, err
	}
	if err := e.reconcileReservations(); err != nil {
		return Result{State: e.State()}, err
	}
	for _, trade := range trades {
		events = append(events, e.emit(Event{Type: "trade", Trade: &trade}))
	}
	events = append(events, e.emitLifecycle(lifecycle)...)
	e.version++
	return Result{State: e.State(), Events: events}, nil
}

func (e *Engine) Cancel(accountID string, orderID uint64) (Result, error) {
	return e.transact(func(candidate *Engine) (Result, error) { return candidate.cancel(accountID, orderID) })
}

func (e *Engine) cancel(accountID string, orderID uint64) (Result, error) {
	if e.isOver {
		return Result{State: e.State()}, errors.New("game is over")
	}
	bookOrder, ok := e.book.Order(orderID)
	if !ok || bookOrder.OwnerID != accountID {
		return Result{State: e.State()}, errors.New("order not found")
	}
	order, _ := e.book.Cancel(orderID)
	if err := e.reconcileReservations(); err != nil {
		return Result{State: e.State()}, err
	}
	e.version++
	exchangeOrder := fromBookOrder(order)
	return Result{State: e.State(), Events: []Event{e.emit(Event{Type: "order_canceled", Order: copyOrder(&exchangeOrder)})}}, nil
}

// ReplaceLimit atomically removes a live order and submits its replacement.
// The caller supplies a new price/quantity but cannot change side; use cancel
// plus a new order for an intentional side change. A replacement always gets
// a new sequence and loses its prior queue position.
func (e *Engine) ReplaceLimit(accountID string, oldOrderID uint64, side Side, price fixed.Price, qty fixed.Qty, tif TimeInForce) (Result, error) {
	return e.transact(func(candidate *Engine) (Result, error) {
		return candidate.replaceLimit(accountID, oldOrderID, side, price, qty, tif)
	})
}

func (e *Engine) replaceLimit(accountID string, oldOrderID uint64, side Side, price fixed.Price, qty fixed.Qty, tif TimeInForce) (Result, error) {
	if e.isOver {
		return Result{State: e.State()}, errors.New("game is over")
	}
	if side != Buy && side != Sell || !price.Positive() || !qty.Positive() || (tif != GTC && tif != IOC) {
		return Result{State: e.State()}, errors.New("invalid replacement")
	}
	oldBookOrder, ok := e.book.Order(oldOrderID)
	if !ok || oldBookOrder.OwnerID != accountID {
		return Result{State: e.State()}, errors.New("order not found")
	}
	if Side(oldBookOrder.Side) != side {
		return Result{State: e.State()}, errors.New("replacement side must match existing order")
	}
	if !e.canPlaceExcluding(accountID, side, price, qty, oldOrderID) {
		return Result{State: e.State()}, errors.New("replacement would violate cash, position, or initial-margin limits")
	}
	replacement := &Order{ID: e.nextOrder + 1, Sequence: e.nextOrder + 1, AccountID: accountID, Side: side, Price: price, Quantity: qty, Remaining: qty, TIF: tif}
	if _, err := e.book.PreviewReplace(oldOrderID, toBookOrder(*replacement), orderbook.RejectTaker); err != nil {
		return Result{State: e.State()}, err
	}
	e.nextOrder++
	report, err := e.book.Replace(oldOrderID, toBookOrder(*replacement), orderbook.RejectTaker)
	if err != nil {
		return Result{State: e.State()}, err
	}
	trades, err := e.settleReport(report)
	if err != nil {
		return Result{State: e.State()}, err
	}
	if err := e.reconcileReservations(); err != nil {
		return Result{State: e.State()}, err
	}
	oldOrder := fromBookOrder(oldBookOrder)
	events := []Event{e.emit(Event{Type: "order_replaced", PreviousOrder: copyOrder(&oldOrder), Order: copyOrder(replacement)})}
	for _, trade := range trades {
		events = append(events, e.emit(Event{Type: "trade", Trade: &trade}))
	}
	events = append(events, e.emitLifecycle(report)...)
	e.version++
	return Result{State: e.State(), Events: events}, nil
}

func (e *Engine) SubmitQuote(bid, ask fixed.Price) (Result, error) {
	return e.transact(func(candidate *Engine) (Result, error) { return candidate.submitQuote(bid, ask) })
}

// UpdateQuote atomically replaces the player's two-sided real-time quote.
// Time and autonomous market activity are owned by the caller's sequencer.
func (e *Engine) UpdateQuote(bid, ask fixed.Price, quantity fixed.Qty) (Result, error) {
	return e.transact(func(candidate *Engine) (Result, error) {
		if !quantity.Positive() || quantity > candidate.cfg.MaxOrderQty {
			return Result{State: candidate.State()}, errors.New("quote quantity exceeds configured limits")
		}
		if err := candidate.validatePlayerQuote(bid, ask, quantity); err != nil {
			return Result{State: candidate.State()}, err
		}
		before, err := candidate.equity(candidate.accounts[PlayerAccount])
		if err != nil {
			return Result{State: candidate.State()}, err
		}
		attribution := &PnLAttribution{}
		summary := Summary{PnLAttribution: attribution}
		events, err := candidate.replacePlayerQuote(bid, ask, quantity, func(trade Trade) error {
			if err := addExecutionEdge(attribution, trade, candidate.mark); err != nil {
				return err
			}
			return addPlayerTrade(&summary, trade)
		})
		if err != nil {
			return Result{State: candidate.State()}, err
		}
		maintenanceEvents, err := candidate.endRealTimeOnMaintenanceBreach()
		if err != nil {
			return Result{State: candidate.State()}, err
		}
		events = append(events, maintenanceEvents...)
		after, err := candidate.equity(candidate.accounts[PlayerAccount])
		if err != nil {
			return Result{State: candidate.State()}, err
		}
		summary.ActionPnL, err = fixed.SubMoney(after, before)
		if err != nil || summary.ActionPnL != attribution.ExecutionEdge {
			return Result{State: candidate.State()}, errors.New("quote action P&L does not reconcile")
		}
		candidate.version++
		return Result{State: candidate.State(), Summary: summary, Events: events}, nil
	})
}

type CustomerArrival struct {
	Side         Side
	Quantity     fixed.Qty
	SlippageBps  int64
	Informed     bool
	InformedMark fixed.Price
}

// ApplyCustomerArrival executes one scheduled IOC customer order.
func (e *Engine) ApplyCustomerArrival(arrival CustomerArrival) (Result, error) {
	return e.transact(func(candidate *Engine) (Result, error) {
		if candidate.isOver {
			return Result{State: candidate.State()}, errors.New("game is over")
		}
		if arrival.Side != Buy && arrival.Side != Sell || !arrival.Quantity.Positive() || arrival.Quantity > candidate.cfg.MaxOrderQty || arrival.SlippageBps < 0 || arrival.SlippageBps > candidate.cfg.MaxFlowSlippageBps || arrival.Informed && !arrival.InformedMark.Positive() || !arrival.Informed && arrival.InformedMark != 0 {
			return Result{State: candidate.State()}, errors.New("invalid customer arrival")
		}
		before, err := candidate.equity(candidate.accounts[PlayerAccount])
		if err != nil {
			return Result{State: candidate.State()}, err
		}
		attribution := &PnLAttribution{}
		summary := Summary{OrdersReceived: 1, PnLAttribution: attribution}
		playerFilled := false
		informedTrades := make([]Trade, 0)
		events, err := candidate.executeCustomerArrival(customerArrival{side: arrival.Side, quantity: arrival.Quantity, slippageBps: arrival.SlippageBps, informed: arrival.Informed}, func(trade Trade) error {
			if err := addFlowTrade(&summary, trade); err != nil {
				return err
			}
			if err := addExecutionEdge(attribution, trade, candidate.mark); err != nil {
				return err
			}
			if arrival.Informed && isPlayerTrade(trade) {
				playerFilled = true
				summary.InformedUnitsTraded, err = fixed.AddQty(summary.InformedUnitsTraded, trade.Quantity)
				if err != nil {
					return err
				}
				informedTrades = append(informedTrades, trade)
			}
			return nil
		})
		if err != nil {
			return Result{State: candidate.State()}, err
		}
		if arrival.Informed {
			summary.InformedOrders = 1
			if playerFilled {
				summary.InformedOrdersFilled = 1
			}
		}
		for _, trade := range informedTrades {
			pnl, err := playerFillPnL(trade, arrival.InformedMark)
			if err != nil {
				return Result{State: candidate.State()}, err
			}
			summary.InformedFlowPnL, err = fixed.AddMoney(summary.InformedFlowPnL, pnl)
			if err != nil {
				return Result{State: candidate.State()}, err
			}
		}
		maintenanceEvents, err := candidate.endRealTimeOnMaintenanceBreach()
		if err != nil {
			return Result{State: candidate.State()}, err
		}
		events = append(events, maintenanceEvents...)
		after, err := candidate.equity(candidate.accounts[PlayerAccount])
		if err != nil {
			return Result{State: candidate.State()}, err
		}
		summary.ActionPnL, err = fixed.SubMoney(after, before)
		if err != nil || summary.ActionPnL != attribution.ExecutionEdge {
			return Result{State: candidate.State()}, errors.New("customer action P&L does not reconcile")
		}
		candidate.version++
		return Result{State: candidate.State(), Summary: summary, Events: events}, nil
	})
}

// ApplyMarkMove applies one scheduled reference-price move.
func (e *Engine) ApplyMarkMove(moveBps int64) (Result, error) {
	return e.transact(func(candidate *Engine) (Result, error) {
		if candidate.isOver {
			return Result{State: candidate.State()}, errors.New("game is over")
		}
		if moveBps < candidate.cfg.MinMoveBps || moveBps > candidate.cfg.MaxMoveBps {
			return Result{State: candidate.State()}, errors.New("mark move exceeds configured limits")
		}
		previous := candidate.mark
		before, err := candidate.equity(candidate.accounts[PlayerAccount])
		if err != nil {
			return Result{State: candidate.State()}, err
		}
		next, err := fixed.ScalePrice(previous, 10_000+moveBps, 10_000)
		if err != nil || !next.Positive() {
			return Result{State: candidate.State()}, errors.New("mark movement produced invalid price")
		}
		attribution := &PnLAttribution{}
		attribution.InventoryMarkPnL, err = markPnL(candidate.accounts[PlayerAccount].Position, previous, next)
		if err != nil {
			return Result{State: candidate.State()}, err
		}
		events := []Event{candidate.applyMark(next, true)}
		maintenanceEvents, err := candidate.endRealTimeOnMaintenanceBreach()
		if err != nil {
			return Result{State: candidate.State()}, err
		}
		events = append(events, maintenanceEvents...)
		after, err := candidate.equity(candidate.accounts[PlayerAccount])
		if err != nil {
			return Result{State: candidate.State()}, err
		}
		delta, err := fixed.SubMoney(after, before)
		if err != nil || delta != attribution.InventoryMarkPnL {
			return Result{State: candidate.State()}, errors.New("mark action P&L does not reconcile")
		}
		candidate.version++
		return Result{State: candidate.State(), Summary: Summary{ActionPnL: attribution.InventoryMarkPnL, PnLAttribution: attribution}, Events: events}, nil
	})
}

// ApplyCarry charges one scheduled inventory-carry interval.
func (e *Engine) ApplyCarry(rate fixed.Price) (Result, error) {
	return e.transact(func(candidate *Engine) (Result, error) {
		if candidate.isOver {
			return Result{State: candidate.State()}, errors.New("game is over")
		}
		if rate < 0 {
			return Result{State: candidate.State()}, errors.New("carry rate must be non-negative")
		}
		before, err := candidate.equity(candidate.accounts[PlayerAccount])
		if err != nil {
			return Result{State: candidate.State()}, err
		}
		storage, events, err := candidate.chargeStorage(PlayerAccount, rate)
		if err != nil {
			return Result{State: candidate.State()}, err
		}
		storagePnL, err := fixed.NegMoney(storage)
		if err != nil {
			return Result{State: candidate.State()}, err
		}
		after, err := candidate.equity(candidate.accounts[PlayerAccount])
		if err != nil {
			return Result{State: candidate.State()}, err
		}
		delta, err := fixed.SubMoney(after, before)
		if err != nil || delta != storagePnL {
			return Result{State: candidate.State()}, errors.New("carry action P&L does not reconcile")
		}
		maintenanceEvents, err := candidate.endRealTimeOnMaintenanceBreach()
		if err != nil {
			return Result{State: candidate.State()}, err
		}
		events = append(events, maintenanceEvents...)
		attribution := &PnLAttribution{StoragePnL: storagePnL}
		candidate.version++
		return Result{State: candidate.State(), Summary: Summary{StorageCost: storage, ActionPnL: storagePnL, PnLAttribution: attribution}, Events: events}, nil
	})
}

// ExpireTradingDay cancels the live player quote and marks the solo session at
// the current reference price without synthesizing a liquidation trade.
func (e *Engine) ExpireTradingDay() (Result, error) {
	return e.transact(func(candidate *Engine) (Result, error) {
		if candidate.isOver {
			return Result{State: candidate.State()}, errors.New("game is over")
		}
		events := candidate.cancelAccountOrders(PlayerAccount)
		if err := candidate.reconcileReservations(); err != nil {
			return Result{State: candidate.State()}, err
		}
		events = append(events, candidate.endSession(TimeExpired))
		candidate.version++
		return Result{State: candidate.State(), Events: events}, nil
	})
}

// QuitRealTime completes a solo session and releases its live quote without
// changing the historical turn-based quit contract.
func (e *Engine) QuitRealTime() (Result, error) {
	return e.transact(func(candidate *Engine) (Result, error) {
		if candidate.isOver {
			return Result{State: candidate.State()}, errors.New("game is over")
		}
		events := candidate.cancelAccountOrders(PlayerAccount)
		if err := candidate.reconcileReservations(); err != nil {
			return Result{State: candidate.State()}, err
		}
		events = append(events, candidate.endSession(PlayerQuit))
		candidate.version++
		return Result{State: candidate.State(), Events: events}, nil
	})
}

func (e *Engine) submitQuote(bid, ask fixed.Price) (Result, error) {
	if e.cfg.SimulationVersion == SimulationVersionAdverseSelection {
		return e.submitQuoteV2(bid, ask)
	}
	return e.submitQuoteLegacy(bid, ask)
}

func (e *Engine) submitQuoteLegacy(bid, ask fixed.Price) (Result, error) {
	qty := e.cfg.MaxOrderQty
	if err := e.validatePlayerQuote(bid, ask, qty); err != nil {
		return Result{State: e.State()}, err
	}
	before, err := e.equity(e.accounts[PlayerAccount])
	if err != nil {
		return Result{State: e.State()}, err
	}
	events, err := e.replacePlayerQuote(bid, ask, qty, nil)
	if err != nil {
		return Result{State: e.State()}, err
	}
	summary, err := e.advance(&events)
	if err != nil {
		return Result{State: e.State()}, err
	}
	after, err := e.equity(e.accounts[PlayerAccount])
	if err != nil {
		return Result{State: e.State()}, err
	}
	summary.TurnPnL, err = fixed.SubMoney(after, before)
	if err != nil {
		return Result{State: e.State()}, err
	}
	e.version++
	return Result{State: e.State(), Summary: summary, Events: events}, nil
}

func (e *Engine) submitQuoteV2(bid, ask fixed.Price) (Result, error) {
	qty := e.cfg.MaxOrderQty
	if err := e.validatePlayerQuote(bid, ask, qty); err != nil {
		return Result{State: e.State()}, err
	}
	player := e.accounts[PlayerAccount]
	before, err := e.equity(player)
	if err != nil {
		return Result{State: e.State()}, err
	}
	openingMark := e.mark
	attribution := &PnLAttribution{}
	events, err := e.replacePlayerQuote(bid, ask, qty, func(trade Trade) error {
		return addExecutionEdge(attribution, trade, openingMark)
	})
	if err != nil {
		return Result{State: e.State()}, err
	}
	summary, err := e.advanceV2(&events, openingMark, attribution)
	if err != nil {
		return Result{State: e.State()}, err
	}
	after, err := e.equity(player)
	if err != nil {
		return Result{State: e.State()}, err
	}
	equityDelta, err := fixed.SubMoney(after, before)
	if err != nil {
		return Result{State: e.State()}, err
	}
	if summary.TurnPnL != equityDelta {
		return Result{State: e.State()}, errors.New("turn P&L attribution does not reconcile to equity")
	}
	e.version++
	return Result{State: e.State(), Summary: summary, Events: events}, nil
}

func (e *Engine) validatePlayerQuote(bid, ask fixed.Price, qty fixed.Qty) error {
	if e.isOver {
		return errors.New("game is over")
	}
	if !bid.Positive() || !ask.Positive() || bid >= ask {
		return errors.New("bid must be positive and strictly less than ask")
	}
	if !e.canPlaceQuotePair(bid, ask, qty) {
		return errors.New("quote would violate cash, position, or initial-margin limits")
	}
	return nil
}

func (e *Engine) replacePlayerQuote(bid, ask fixed.Price, qty fixed.Qty, onTrade func(Trade) error) ([]Event, error) {
	events := e.cancelAccountOrders(PlayerAccount)
	for _, order := range []*Order{e.newOrder(PlayerAccount, Buy, bid, qty, GTC), e.newOrder(PlayerAccount, Sell, ask, qty, GTC)} {
		events = append(events, e.emit(Event{Type: "order_accepted", Order: copyOrder(order)}))
		trades, lifecycle, err := e.submitBookOrder(order)
		if err != nil {
			return nil, err
		}
		for _, trade := range trades {
			if onTrade != nil {
				if err := onTrade(trade); err != nil {
					return nil, err
				}
			}
			events = append(events, e.emit(Event{Type: "trade", Trade: &trade}))
		}
		events = append(events, e.emitLifecycle(lifecycle)...)
	}
	if err := e.reconcileReservations(); err != nil {
		return nil, err
	}
	return events, nil
}

func (e *Engine) Quit() (Result, error) {
	if e.isOver {
		return Result{State: e.State()}, errors.New("game is over")
	}
	e.isOver = true
	e.reason = PlayerQuit
	e.version++
	events := []Event{e.emit(Event{Type: "game_ended", Reason: e.reason})}
	return Result{State: e.State(), Events: events}, nil
}

func (e *Engine) canPlaceQuotePair(bid, ask fixed.Price, qty fixed.Qty) bool {
	// A quote renewal cancels every player order before either new side rests.
	// Assess the proposed pair against that post-replacement order set.
	return e.canPlaceOrders(PlayerAccount, []riskOrder{{side: Buy, price: bid, qty: qty}, {side: Sell, price: ask, qty: qty}}, 0, false)
}

func (e *Engine) canPlace(accountID string, side Side, price fixed.Price, qty fixed.Qty) bool {
	return e.canPlaceExcluding(accountID, side, price, qty, 0)
}

func (e *Engine) canPlaceExcluding(accountID string, side Side, price fixed.Price, qty fixed.Qty, excludedOrderID uint64) bool {
	return e.canPlaceOrders(accountID, []riskOrder{{side: side, price: price, qty: qty}}, excludedOrderID, true)
}

type riskOrder struct {
	side  Side
	price fixed.Price
	qty   fixed.Qty
}

func (e *Engine) canPlaceOrders(accountID string, proposed []riskOrder, excludedOrderID uint64, includeExisting bool) bool {
	account := e.accounts[accountID]
	if account.External {
		return true
	}
	longWorst, shortWorst := account.Position, account.Position
	buyNotional, sellNotional := fixed.Money(0), fixed.Money(0)
	positionAbs, err := fixed.AbsQtyChecked(account.Position)
	if err != nil {
		return false
	}
	gross, err := fixed.Notional(e.mark, positionAbs)
	if err != nil {
		return false
	}

	orders := make([]riskOrder, 0, len(proposed))
	if includeExisting {
		orders = make([]riskOrder, 0, len(e.book.Orders(accountID))+len(proposed))
		for _, order := range e.book.Orders(accountID) {
			if order.ID == excludedOrderID {
				continue
			}
			orders = append(orders, riskOrder{side: Side(order.Side), price: order.Price, qty: order.Remaining})
		}
	}
	orders = append(orders, proposed...)
	for _, order := range orders {
		notional, err := fixed.Notional(order.price, order.qty)
		if err != nil {
			return false
		}
		gross, err = fixed.AddMoney(gross, notional)
		if err != nil {
			return false
		}
		if order.side == Buy {
			longWorst, err = fixed.AddQty(longWorst, order.qty)
			if err == nil {
				buyNotional, err = fixed.AddMoney(buyNotional, notional)
			}
		} else {
			shortWorst, err = fixed.SubQty(shortWorst, order.qty)
			if err == nil {
				sellNotional, err = fixed.AddMoney(sellNotional, notional)
			}
		}
		if err != nil {
			return false
		}
	}
	longWorstAbs, err := fixed.AbsQtyChecked(longWorst)
	if err != nil || longWorstAbs > e.cfg.MaxPosition {
		return false
	}
	shortWorstAbs, err := fixed.AbsQtyChecked(shortWorst)
	if err != nil || shortWorstAbs > e.cfg.MaxPosition {
		return false
	}
	equity, err := e.equity(account)
	if err != nil {
		return false
	}
	if account.Cash < buyNotional {
		return false
	}
	required, err := fixed.ScaleMoney(gross, e.cfg.InitialMarginBps, 10_000)
	if err != nil || equity < required {
		return false
	}
	longCash, err := fixed.SubMoney(account.Cash, buyNotional)
	if err != nil {
		return false
	}
	shortCash, err := fixed.AddMoney(account.Cash, sellNotional)
	if err != nil {
		return false
	}
	return e.fillStateWithinInitialMargin(longCash, longWorst) &&
		e.fillStateWithinInitialMargin(shortCash, shortWorst)
}

func (e *Engine) fillStateWithinInitialMargin(cash fixed.Money, position fixed.Qty) bool {
	marked, err := fixed.Notional(e.mark, position)
	if err != nil {
		return false
	}
	equity, err := fixed.AddMoney(cash, marked)
	if err != nil {
		return false
	}
	positionAbs, err := fixed.AbsQtyChecked(position)
	if err != nil || positionAbs > e.cfg.MaxPosition {
		return false
	}
	gross, err := fixed.Notional(e.mark, positionAbs)
	if err != nil {
		return false
	}
	required, err := fixed.ScaleMoney(gross, e.cfg.InitialMarginBps, 10_000)
	return err == nil && equity >= required
}

// reconcileReservations derives all reservation balances from the live book.
// Keeping reservations derived for now avoids a second mutable source of truth
// while giving account consumers explicit available and committed balances.
func (e *Engine) reconcileReservations() error {
	type reservation struct {
		cash      fixed.Money
		buy, sell fixed.Qty
	}
	desired := make(map[string]reservation, len(e.accounts))
	for _, order := range e.book.Orders("") {
		account := e.accounts[order.OwnerID]
		if account == nil {
			return errors.New("book references unknown account")
		}
		if account.External {
			continue
		}
		current := desired[order.OwnerID]
		if order.Side == orderbook.Buy {
			notional, err := fixed.Notional(order.Price, order.Remaining)
			if err != nil {
				return err
			}
			current.cash, err = fixed.AddMoney(current.cash, notional)
			if err != nil {
				return err
			}
			current.buy, err = fixed.AddQty(current.buy, order.Remaining)
			if err != nil {
				return err
			}
		} else {
			var err error
			current.sell, err = fixed.AddQty(current.sell, order.Remaining)
			if err != nil {
				return err
			}
		}
		desired[order.OwnerID] = current
	}
	postings := make([]Posting, 0)
	accountIDs := make([]string, 0, len(e.accounts))
	for id := range e.accounts {
		accountIDs = append(accountIDs, id)
	}
	sort.Strings(accountIDs)
	for _, id := range accountIDs {
		account := e.accounts[id]
		if account.External {
			continue
		}
		next := desired[id]
		cashDelta, err := fixed.AddMoney(next.cash, -account.ReservedCash)
		if err != nil {
			return err
		}
		sellDelta, err := fixed.SubQty(next.sell, account.OpenSellQuantity)
		if err != nil {
			return err
		}
		if cashDelta != 0 {
			postings = append(postings, Posting{Account: cashAvailable(id), Money: -cashDelta}, Posting{Account: cashReserved(id), Money: cashDelta})
		}
		if sellDelta != 0 {
			postings = append(postings, Posting{Account: instrumentAvailable(id), Instrument: -sellDelta}, Posting{Account: instrumentReserved(id), Instrument: sellDelta})
		}
	}
	if len(postings) > 0 {
		if err := e.ledger.append(LedgerEntry{Type: "order_reservation_updated", Postings: postings}); err != nil {
			return err
		}
	}
	for id, account := range e.accounts {
		if account.External {
			continue
		}
		next := desired[id]
		account.ReservedCash, account.OpenBuyQuantity, account.OpenSellQuantity = next.cash, next.buy, next.sell
	}
	return nil
}

func (e *Engine) accountWithinLimits(accountID string, maintenance bool) bool {
	a := e.accounts[accountID]
	if a.External {
		return true
	}
	positionAbs, err := fixed.AbsQtyChecked(a.Position)
	if err != nil || positionAbs > e.cfg.MaxPosition {
		return false
	}
	eq, err := e.equity(a)
	if err != nil || eq <= 0 {
		return false
	}
	gross, err := fixed.Notional(e.mark, positionAbs)
	if err != nil {
		return false
	}
	bps := e.cfg.InitialMarginBps
	if maintenance {
		bps = e.cfg.MaintenanceMarginBps
	}
	required, err := fixed.ScaleMoney(gross, bps, 10_000)
	return err == nil && eq >= required
}

type customerArrival struct {
	side        Side
	quantity    fixed.Qty
	slippageBps int64
	informed    bool
}

func (e *Engine) drawLegacyCustomerArrival() customerArrival {
	arrival := customerArrival{side: Buy}
	if e.rng.IntN(2) == 1 {
		arrival.side = Sell
	}
	arrival.quantity = fixed.Qty(1 + e.rng.Int64N(int64(e.cfg.MaxOrderQty)))
	if e.cfg.MaxFlowSlippageBps > 0 {
		arrival.slippageBps = e.rng.Int64N(e.cfg.MaxFlowSlippageBps + 1)
	}
	return arrival
}

func (e *Engine) drawV2CustomerArrival(openingMark, nextMark fixed.Price) customerArrival {
	arrival := customerArrival{side: Buy}
	if e.flowRNG.bounded(2) == 1 {
		arrival.side = Sell
	}
	arrival.quantity = fixed.Qty(1 + e.flowRNG.bounded(uint64(e.cfg.MaxOrderQty)))
	arrival.slippageBps = int64(e.flowRNG.bounded(uint64(e.cfg.MaxFlowSlippageBps + 1)))
	informedDraw := e.informedRNG.bounded(10_000)
	arrival.informed = nextMark != openingMark && informedDraw < uint64(e.cfg.InformedFlowBps)
	if arrival.informed {
		if nextMark > openingMark {
			arrival.side = Buy
		} else {
			arrival.side = Sell
		}
	}
	return arrival
}

func (e *Engine) executeCustomerArrival(arrival customerArrival, onTrade func(Trade) error) ([]Event, error) {
	price, err := e.flowLimitV2(arrival.side, arrival.slippageBps)
	if err != nil {
		return nil, err
	}
	order := e.newOrder(FlowAccount, arrival.side, price, arrival.quantity, IOC)
	order.Informed = arrival.informed
	events := []Event{e.emit(Event{Type: "flow_order", Order: copyOrder(order)})}
	trades, lifecycle, err := e.submitBookOrder(order)
	if err != nil {
		return nil, err
	}
	for _, trade := range trades {
		events = append(events, e.emit(Event{Type: "trade", Trade: &trade}))
		if onTrade != nil {
			if err := onTrade(trade); err != nil {
				return nil, err
			}
		}
	}
	lifecycleEvents := e.emitLifecycle(lifecycle)
	markLifecycleOrderInformed(lifecycleEvents, order.ID, arrival.informed)
	events = append(events, lifecycleEvents...)
	if err := e.reconcileReservations(); err != nil {
		return nil, err
	}
	return events, nil
}

func (e *Engine) chargeStorage(accountID string, rate fixed.Price) (fixed.Money, []Event, error) {
	account := e.accounts[accountID]
	positionAbs, err := fixed.AbsQtyChecked(account.Position)
	if err != nil {
		return 0, nil, err
	}
	storage, err := fixed.Notional(rate, positionAbs)
	if err != nil {
		return 0, nil, err
	}
	if storage > 0 {
		if err := e.ledger.append(LedgerEntry{Type: "storage_charged", Postings: []Posting{{Account: cashAvailable(accountID), Money: -storage}, {Account: storageAccount, Money: storage}}}); err != nil {
			return 0, nil, err
		}
	}
	account.Cash, err = fixed.AddMoney(account.Cash, -storage)
	if err != nil {
		return 0, nil, err
	}
	if storage == 0 {
		return 0, nil, nil
	}
	return storage, []Event{e.emit(Event{Type: "storage_charged", Amount: storage})}, nil
}

func (e *Engine) advance(events *[]Event) (Summary, error) {
	s := Summary{}
	if e.cfg.MaxOrdersPerTurn > 0 {
		n := 1 + e.rng.IntN(e.cfg.MaxOrdersPerTurn)
		for range n {
			s.OrdersReceived++
			arrival := e.drawLegacyCustomerArrival()
			arrivalEvents, err := e.executeCustomerArrival(arrival, func(trade Trade) error {
				return addLegacyFlowTrade(&s, trade)
			})
			if err != nil {
				return Summary{}, err
			}
			*events = append(*events, arrivalEvents...)
		}
	}
	storage, storageEvents, err := e.chargeStorage(PlayerAccount, e.cfg.StoragePerUnit)
	if err != nil {
		return Summary{}, err
	}
	s.StorageCost = storage
	*events = append(*events, storageEvents...)
	e.turn++
	nextMark, err := e.nextLegacyMark()
	if err != nil {
		return Summary{}, err
	}
	*events = append(*events, e.applyMark(nextMark, false))
	if terminal := e.evaluatePlayerMaintenance(); terminal != nil {
		*events = append(*events, *terminal)
	} else if terminal := e.completeTurnsIfDue(); terminal != nil {
		*events = append(*events, *terminal)
	}
	return s, nil
}

func (e *Engine) advanceV2(events *[]Event, openingMark fixed.Price, attribution *PnLAttribution) (Summary, error) {
	s := Summary{PnLAttribution: attribution}
	nextMark, err := e.nextMarkV2()
	if err != nil {
		return Summary{}, err
	}
	informedTrades := make([]Trade, 0)
	if e.cfg.MaxOrdersPerTurn > 0 {
		n := 1 + int(e.flowRNG.bounded(uint64(e.cfg.MaxOrdersPerTurn)))
		for range n {
			s.OrdersReceived++
			arrival := e.drawV2CustomerArrival(openingMark, nextMark)
			if arrival.informed {
				s.InformedOrders++
			}
			informedPlayerFill := false
			arrivalEvents, err := e.executeCustomerArrival(arrival, func(trade Trade) error {
				if err := addFlowTrade(&s, trade); err != nil {
					return err
				}
				if err := addExecutionEdge(attribution, trade, openingMark); err != nil {
					return err
				}
				if arrival.informed && isPlayerTrade(trade) {
					informedPlayerFill = true
					s.InformedUnitsTraded, err = fixed.AddQty(s.InformedUnitsTraded, trade.Quantity)
					if err != nil {
						return err
					}
					informedTrades = append(informedTrades, trade)
				}
				return nil
			})
			if err != nil {
				return Summary{}, err
			}
			*events = append(*events, arrivalEvents...)
			if informedPlayerFill {
				s.InformedOrdersFilled++
			}
		}
	}
	player := e.accounts[PlayerAccount]
	storage, storageEvents, err := e.chargeStorage(PlayerAccount, e.cfg.StoragePerUnit)
	if err != nil {
		return Summary{}, err
	}
	s.StorageCost = storage
	*events = append(*events, storageEvents...)
	e.turn++
	*events = append(*events, e.applyMark(nextMark, true))

	attribution.InventoryMarkPnL, err = markPnL(player.Position, openingMark, e.mark)
	if err != nil {
		return Summary{}, err
	}
	attribution.StoragePnL, err = fixed.NegMoney(storage)
	if err != nil {
		return Summary{}, err
	}
	s.TurnPnL, err = fixed.AddMoney(attribution.ExecutionEdge, attribution.InventoryMarkPnL)
	if err != nil {
		return Summary{}, err
	}
	s.TurnPnL, err = fixed.AddMoney(s.TurnPnL, attribution.StoragePnL)
	if err != nil {
		return Summary{}, err
	}
	for _, trade := range informedTrades {
		pnl, err := playerFillPnL(trade, e.mark)
		if err != nil {
			return Summary{}, err
		}
		s.InformedFlowPnL, err = fixed.AddMoney(s.InformedFlowPnL, pnl)
		if err != nil {
			return Summary{}, err
		}
	}
	if terminal := e.evaluatePlayerMaintenance(); terminal != nil {
		*events = append(*events, *terminal)
	} else if terminal := e.completeTurnsIfDue(); terminal != nil {
		*events = append(*events, *terminal)
	}
	return s, nil
}

func (e *Engine) nextLegacyMark() (fixed.Price, error) {
	move := e.cfg.MinMoveBps
	if e.cfg.MaxMoveBps > e.cfg.MinMoveBps {
		move += e.rng.Int64N(e.cfg.MaxMoveBps - e.cfg.MinMoveBps + 1)
	}
	nextMark, err := fixed.ScalePrice(e.mark, 10_000+move, 10_000)
	if err != nil || !nextMark.Positive() {
		return 0, errors.New("mark movement produced invalid price")
	}
	return nextMark, nil
}

func (e *Engine) applyMark(nextMark fixed.Price, includePreviousMark bool) Event {
	previous := e.mark
	e.mark = nextMark
	event := Event{Type: "mark_updated", Mark: e.mark, Message: fmt.Sprintf("previous=%s", previous)}
	if includePreviousMark {
		event.PreviousMark = previous
	}
	return e.emit(event)
}

func (e *Engine) evaluatePlayerMaintenance() *Event {
	reason, breached := e.playerMaintenanceReason()
	if !breached {
		return nil
	}
	event := e.endSession(reason)
	return &event
}

func (e *Engine) playerMaintenanceReason() (EndReason, bool) {
	if e.accountWithinLimits(PlayerAccount, true) {
		return NotOver, false
	}
	reason := MarginBreach
	equity, _ := e.equity(e.accounts[PlayerAccount])
	if equity <= 0 {
		reason = Insolvent
	}
	return reason, true
}

func (e *Engine) endRealTimeOnMaintenanceBreach() ([]Event, error) {
	reason, breached := e.playerMaintenanceReason()
	if !breached {
		return nil, nil
	}
	events := e.cancelAccountOrders(PlayerAccount)
	if err := e.reconcileReservations(); err != nil {
		return nil, err
	}
	events = append(events, e.endSession(reason))
	return events, nil
}

func (e *Engine) completeTurnsIfDue() *Event {
	if e.cfg.NumTurns <= 0 || e.turn < e.cfg.NumTurns {
		return nil
	}
	event := e.endSession(TurnsComplete)
	return &event
}

func (e *Engine) endSession(reason EndReason) Event {
	e.isOver = true
	e.reason = reason
	return e.emit(Event{Type: "game_ended", Reason: reason})
}

func (e *Engine) nextMarkV2() (fixed.Price, error) {
	moveRange := uint64(e.cfg.MaxMoveBps - e.cfg.MinMoveBps + 1)
	move := e.cfg.MinMoveBps + int64(e.markRNG.bounded(moveRange))
	nextMark, err := fixed.ScalePrice(e.mark, 10_000+move, 10_000)
	if err != nil || !nextMark.Positive() {
		return 0, errors.New("mark movement produced invalid price")
	}
	return nextMark, nil
}

func (e *Engine) flowLimitV2(side Side, slip int64) (fixed.Price, error) {
	factor := int64(10_000) + slip
	if side == Sell {
		factor = 10_000 - slip
	}
	p, err := fixed.ScalePrice(e.mark, factor, 10_000)
	if err != nil {
		return 0, err
	}
	if p < 1 {
		return 1, nil
	}
	return p, nil
}

func addFlowTrade(summary *Summary, trade Trade) error {
	if !isPlayerTrade(trade) {
		return nil
	}
	var err error
	summary.UnitsTraded, err = fixed.AddQty(summary.UnitsTraded, trade.Quantity)
	if err != nil {
		return err
	}
	notional, err := fixed.Notional(trade.Price, trade.Quantity)
	if err != nil {
		return err
	}
	if trade.BuyerID == PlayerAccount {
		summary.SellVolume, err = fixed.AddQty(summary.SellVolume, trade.Quantity)
		if err != nil {
			return err
		}
		summary.NetFillCash, err = fixed.SubMoney(summary.NetFillCash, notional)
		if err != nil {
			return err
		}
	}
	if trade.SellerID == PlayerAccount {
		summary.BuyVolume, err = fixed.AddQty(summary.BuyVolume, trade.Quantity)
		if err != nil {
			return err
		}
		summary.NetFillCash, err = fixed.AddMoney(summary.NetFillCash, notional)
		if err != nil {
			return err
		}
	}
	return nil
}

func addPlayerTrade(summary *Summary, trade Trade) error {
	if !isPlayerTrade(trade) {
		return nil
	}
	var err error
	summary.UnitsTraded, err = fixed.AddQty(summary.UnitsTraded, trade.Quantity)
	if err != nil {
		return err
	}
	notional, err := fixed.Notional(trade.Price, trade.Quantity)
	if err != nil {
		return err
	}
	if trade.BuyerID == PlayerAccount {
		summary.NetFillCash, err = fixed.SubMoney(summary.NetFillCash, notional)
	} else {
		summary.NetFillCash, err = fixed.AddMoney(summary.NetFillCash, notional)
	}
	return err
}

func addLegacyFlowTrade(summary *Summary, trade Trade) error {
	var err error
	summary.UnitsTraded, err = fixed.AddQty(summary.UnitsTraded, trade.Quantity)
	if err != nil {
		return err
	}
	if trade.BuyerID == PlayerAccount {
		summary.SellVolume, err = fixed.AddQty(summary.SellVolume, trade.Quantity)
		if err != nil {
			return err
		}
		notional, err := fixed.Notional(trade.Price, trade.Quantity)
		if err != nil {
			return err
		}
		summary.NetFillCash, err = fixed.SubMoney(summary.NetFillCash, notional)
		if err != nil {
			return err
		}
	}
	if trade.SellerID == PlayerAccount {
		summary.BuyVolume, err = fixed.AddQty(summary.BuyVolume, trade.Quantity)
		if err != nil {
			return err
		}
		notional, err := fixed.Notional(trade.Price, trade.Quantity)
		if err != nil {
			return err
		}
		summary.NetFillCash, err = fixed.AddMoney(summary.NetFillCash, notional)
		if err != nil {
			return err
		}
	}
	return nil
}

func addExecutionEdge(attribution *PnLAttribution, trade Trade, mark fixed.Price) error {
	pnl, err := playerFillPnL(trade, mark)
	if err != nil {
		return err
	}
	attribution.ExecutionEdge, err = fixed.AddMoney(attribution.ExecutionEdge, pnl)
	return err
}

func playerFillPnL(trade Trade, mark fixed.Price) (fixed.Money, error) {
	signedQty := fixed.Qty(0)
	if trade.BuyerID == PlayerAccount {
		signedQty = trade.Quantity
	} else if trade.SellerID == PlayerAccount {
		var err error
		signedQty, err = fixed.NegQty(trade.Quantity)
		if err != nil {
			return 0, err
		}
	}
	markValue, err := fixed.Notional(mark, signedQty)
	if err != nil {
		return 0, err
	}
	fillValue, err := fixed.Notional(trade.Price, signedQty)
	if err != nil {
		return 0, err
	}
	return fixed.SubMoney(markValue, fillValue)
}

func isPlayerTrade(trade Trade) bool {
	return trade.BuyerID == PlayerAccount || trade.SellerID == PlayerAccount
}

func markPnL(position fixed.Qty, previous, current fixed.Price) (fixed.Money, error) {
	currentValue, err := fixed.Notional(current, position)
	if err != nil {
		return 0, err
	}
	previousValue, err := fixed.Notional(previous, position)
	if err != nil {
		return 0, err
	}
	return fixed.SubMoney(currentValue, previousValue)
}

func markLifecycleOrderInformed(events []Event, orderID uint64, informed bool) {
	if !informed {
		return
	}
	for i := range events {
		if events[i].Order != nil && events[i].Order.ID == orderID {
			events[i].Order.Informed = true
		}
	}
}

func (e *Engine) newOrder(accountID string, side Side, price fixed.Price, qty fixed.Qty, tif TimeInForce) *Order {
	e.nextOrder++
	return &Order{ID: e.nextOrder, Sequence: e.nextOrder, AccountID: accountID, Side: side, Price: price, Quantity: qty, Remaining: qty, TIF: tif}
}

func (e *Engine) submitBookOrder(order *Order) ([]Trade, orderbook.Report, error) {
	report, err := e.book.Submit(toBookOrder(*order), orderbook.RejectTaker)
	if err != nil {
		return nil, orderbook.Report{}, err
	}
	trades, err := e.settleReport(report)
	if err != nil {
		return nil, orderbook.Report{}, err
	}
	if order.Informed {
		for i := range trades {
			trades[i].Informed = true
		}
	}
	return trades, report, nil
}

func (e *Engine) settleReport(report orderbook.Report) ([]Trade, error) {
	if err := e.validateSettlement(report); err != nil {
		return nil, err
	}
	trades := make([]Trade, 0, len(report.Fills))
	for _, fill := range report.Fills {
		e.nextTrade++
		trade := Trade{ID: e.nextTrade, MakerOrderID: fill.Maker.ID, TakerOrderID: fill.Taker.ID, Price: fill.Price, Quantity: fill.Quantity}
		if fill.Taker.Side == orderbook.Buy {
			trade.BuyerID, trade.SellerID = fill.Taker.OwnerID, fill.Maker.OwnerID
		} else {
			trade.BuyerID, trade.SellerID = fill.Maker.OwnerID, fill.Taker.OwnerID
		}
		buyerOrder, sellerOrder := fill.Taker, fill.Maker
		if fill.Taker.Side == orderbook.Sell {
			buyerOrder, sellerOrder = fill.Maker, fill.Taker
		}
		if err := e.applyTrade(trade, buyerOrder, sellerOrder); err != nil {
			return nil, err
		}
		trades = append(trades, trade)
	}
	return trades, nil
}

func (e *Engine) validateSettlement(report orderbook.Report) error {
	type balance struct {
		cash     fixed.Money
		position fixed.Qty
	}
	balances := make(map[string]balance)
	for _, fill := range report.Fills {
		buyerID, sellerID := fill.Taker.OwnerID, fill.Maker.OwnerID
		if fill.Taker.Side == orderbook.Sell {
			buyerID, sellerID = sellerID, buyerID
		}
		notional, err := fixed.Notional(fill.Price, fill.Quantity)
		if err != nil {
			return err
		}
		for _, change := range []struct {
			id  string
			buy bool
		}{
			{id: buyerID, buy: true},
			{id: sellerID},
		} {
			account := e.accounts[change.id]
			current, ok := balances[change.id]
			if !ok {
				current = balance{cash: account.Cash, position: account.Position}
			}
			if change.buy {
				current.cash, err = fixed.SubMoney(current.cash, notional)
				if err == nil {
					current.position, err = fixed.AddQty(current.position, fill.Quantity)
				}
			} else {
				current.cash, err = fixed.AddMoney(current.cash, notional)
				if err == nil {
					current.position, err = fixed.SubQty(current.position, fill.Quantity)
				}
			}
			if err != nil {
				return err
			}
			if !account.External {
				marked, err := fixed.Notional(e.mark, current.position)
				if err != nil {
					return err
				}
				if _, err := fixed.AddMoney(current.cash, marked); err != nil {
					return err
				}
			}
			balances[change.id] = current
		}
	}
	return nil
}

func (e *Engine) applyTrade(trade Trade, buyerOrder, sellerOrder orderbook.Order) error {
	n, err := fixed.Notional(trade.Price, trade.Quantity)
	if err != nil {
		return err
	}
	buyer, seller := e.accounts[trade.BuyerID], e.accounts[trade.SellerID]
	buyerCash, err := fixed.AddMoney(buyer.Cash, -n)
	if err != nil {
		return err
	}
	sellerCash, err := fixed.AddMoney(seller.Cash, n)
	if err != nil {
		return err
	}
	buyerPosition, err := fixed.AddQty(buyer.Position, trade.Quantity)
	if err != nil {
		return err
	}
	sellerPosition, err := fixed.SubQty(seller.Position, trade.Quantity)
	if err != nil {
		return err
	}
	postings := []Posting{{Account: cashAvailable(trade.SellerID), Money: n}, {Account: instrumentAvailable(trade.BuyerID), Instrument: trade.Quantity}}
	buyerReserved := buyerOrder.ID == trade.MakerOrderID && !buyer.External
	reservedBuyerCash := fixed.Money(0)
	if buyerReserved {
		limitNotional, err := fixed.Notional(buyerOrder.Price, trade.Quantity)
		if err != nil {
			return err
		}
		refund, err := fixed.AddMoney(limitNotional, -n)
		if err != nil {
			return err
		}
		reservedBuyerCash = limitNotional
		postings = append(postings, Posting{Account: cashReserved(trade.BuyerID), Money: -limitNotional}, Posting{Account: cashAvailable(trade.BuyerID), Money: refund})
	} else {
		postings = append(postings, Posting{Account: cashAvailable(trade.BuyerID), Money: -n})
	}
	sellerReserved := sellerOrder.ID == trade.MakerOrderID && !seller.External
	if sellerReserved {
		postings = append(postings, Posting{Account: instrumentReserved(trade.SellerID), Instrument: -trade.Quantity})
	} else {
		postings = append(postings, Posting{Account: instrumentAvailable(trade.SellerID), Instrument: -trade.Quantity})
	}
	if err := e.ledger.append(LedgerEntry{Type: "trade_settled", Reference: LedgerReference{TradeID: trade.ID, MakerOrderID: trade.MakerOrderID, TakerOrderID: trade.TakerOrderID}, Postings: postings}); err != nil {
		return err
	}
	if buyerReserved {
		buyer.ReservedCash, err = fixed.AddMoney(buyer.ReservedCash, -reservedBuyerCash)
		if err != nil {
			return err
		}
		buyer.OpenBuyQuantity, err = fixed.SubQty(buyer.OpenBuyQuantity, trade.Quantity)
		if err != nil {
			return err
		}
	}
	if sellerReserved {
		seller.OpenSellQuantity, err = fixed.SubQty(seller.OpenSellQuantity, trade.Quantity)
		if err != nil {
			return err
		}
	}
	buyer.Cash, seller.Cash = buyerCash, sellerCash
	buyer.Position, seller.Position = buyerPosition, sellerPosition
	return nil
}

func (e *Engine) cancelAccountOrders(accountID string) []Event {
	events := []Event{}
	for _, bookOrder := range e.book.Orders(accountID) {
		order, _ := e.book.Cancel(bookOrder.ID)
		exchangeOrder := fromBookOrder(order)
		events = append(events, e.emit(Event{Type: "order_canceled", Order: copyOrder(&exchangeOrder)}))
	}
	return events
}

func (e *Engine) emitLifecycle(report orderbook.Report) []Event {
	events := make([]Event, 0, len(report.Completed)+2)
	if report.Resting != nil {
		order := fromBookOrder(*report.Resting)
		events = append(events, e.emit(Event{Type: "order_resting", Order: copyOrder(&order)}))
	}
	if report.Expired != nil {
		order := fromBookOrder(*report.Expired)
		events = append(events, e.emit(Event{Type: "order_canceled", Order: copyOrder(&order), Message: "ioc remainder"}))
	}
	for _, completed := range report.Completed {
		order := fromBookOrder(completed)
		events = append(events, e.emit(Event{Type: "order_filled", Order: copyOrder(&order)}))
	}
	return events
}

func toBookOrder(order Order) orderbook.Order {
	return orderbook.Order{ID: order.ID, Sequence: order.Sequence, OwnerID: order.AccountID, Side: orderbook.Side(order.Side), Price: order.Price, Quantity: order.Quantity, Remaining: order.Remaining, TIF: orderbook.TimeInForce(order.TIF)}
}

func (e *Engine) postOpening(id string, cash fixed.Money, position fixed.Qty) error {
	postings := []Posting{{Account: cashAvailable(id), Money: cash}, {Account: openingCashAccount(id), Money: -cash}, {Account: instrumentAvailable(id), Instrument: position}, {Account: openingInstrumentAccount(id), Instrument: -position}}
	return e.ledger.append(LedgerEntry{Type: "account_opened", Postings: postings})
}
func fromBookOrder(order orderbook.Order) Order {
	return Order{ID: order.ID, Sequence: order.Sequence, AccountID: order.OwnerID, Side: Side(order.Side), Price: order.Price, Quantity: order.Quantity, Remaining: order.Remaining, TIF: TimeInForce(order.TIF)}
}

func (e *Engine) equity(a *Account) (fixed.Money, error) {
	n, err := fixed.Notional(e.mark, a.Position)
	if err != nil {
		return 0, err
	}
	return fixed.AddMoney(a.Cash, n)
}

func (e *Engine) emit(event Event) Event { e.nextEvent++; event.Sequence = e.nextEvent; return event }
func copyOrder(order *Order) *Order      { out := *order; return &out }
