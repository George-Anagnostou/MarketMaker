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
	MarginBreach  EndReason = "margin_breach"
	Insolvent     EndReason = "insolvent"
	PlayerQuit    EndReason = "player_quit"
)

type Config struct {
	Instrument           string      `json:"instrument"`
	StartingCash         fixed.Money `json:"starting_cash"`
	StartingPosition     fixed.Qty   `json:"starting_position"`
	StartingMark         fixed.Price `json:"starting_mark"`
	StoragePerUnit       fixed.Price `json:"storage_per_unit"`
	NumTurns             int         `json:"num_turns"`
	InitialMarginBps     int64       `json:"initial_margin_bps"`
	MaintenanceMarginBps int64       `json:"maintenance_margin_bps"`
	MaxPosition          fixed.Qty   `json:"max_position"`
	MaxOrdersPerTurn     int         `json:"max_orders_per_turn"`
	MaxOrderQty          fixed.Qty   `json:"max_order_qty"`
	MaxFlowSlippageBps   int64       `json:"max_flow_slippage_bps"`
	MinMoveBps           int64       `json:"min_move_bps"`
	MaxMoveBps           int64       `json:"max_move_bps"`
	Seed                 uint64      `json:"seed"`
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
	if fixed.AbsQty(c.StartingPosition) > c.MaxPosition {
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
	startingGross, err := fixed.Notional(c.StartingMark, fixed.AbsQty(c.StartingPosition))
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
}

type Trade struct {
	ID           uint64      `json:"id"`
	MakerOrderID uint64      `json:"maker_order_id"`
	TakerOrderID uint64      `json:"taker_order_id"`
	Price        fixed.Price `json:"price"`
	Quantity     fixed.Qty   `json:"quantity"`
	BuyerID      string      `json:"buyer_id"`
	SellerID     string      `json:"seller_id"`
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
}

type Summary struct {
	OrdersReceived int         `json:"orders_received"`
	UnitsTraded    fixed.Qty   `json:"units_traded"`
	NetFillCash    fixed.Money `json:"net_fill_cash"`
	StorageCost    fixed.Money `json:"storage_cost"`
	TurnPnL        fixed.Money `json:"turn_pnl"`
	BuyVolume      fixed.Qty   `json:"buy_volume"`
	SellVolume     fixed.Qty   `json:"sell_volume"`
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
	cfg       Config
	rng       *rand.Rand
	pcg       *rand.PCG
	accounts  map[string]*Account
	book      *orderbook.Book
	ledger    ledger
	nextOrder uint64
	nextTrade uint64
	nextEvent uint64
	version   uint64
	turn      int
	mark      fixed.Price
	isOver    bool
	reason    EndReason
}

func New(cfg Config) (*Engine, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	pcg := rand.NewPCG(cfg.Seed, cfg.Seed>>1)
	e := &Engine{cfg: cfg, rng: rand.New(pcg), pcg: pcg, accounts: map[string]*Account{}, book: orderbook.New(), ledger: newLedger(), mark: cfg.StartingMark}
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

func (e *Engine) submitQuote(bid, ask fixed.Price) (Result, error) {
	if e.isOver {
		return Result{State: e.State()}, errors.New("game is over")
	}
	if !bid.Positive() || !ask.Positive() || bid >= ask {
		return Result{State: e.State()}, errors.New("bid must be positive and strictly less than ask")
	}
	qty := e.cfg.MaxOrderQty
	if !e.canPlaceQuotePair(bid, ask, qty) {
		return Result{State: e.State()}, errors.New("quote would violate cash, position, or initial-margin limits")
	}
	before, _ := e.equity(e.accounts[PlayerAccount])
	events := e.cancelAccountOrders(PlayerAccount)
	for _, order := range []*Order{e.newOrder(PlayerAccount, Buy, bid, qty, GTC), e.newOrder(PlayerAccount, Sell, ask, qty, GTC)} {
		events = append(events, e.emit(Event{Type: "order_accepted", Order: copyOrder(order)}))
		trades, lifecycle, err := e.submitBookOrder(order)
		if err != nil {
			return Result{State: e.State()}, err
		}
		for _, trade := range trades {
			events = append(events, e.emit(Event{Type: "trade", Trade: &trade}))
		}
		events = append(events, e.emitLifecycle(lifecycle)...)
	}
	if err := e.reconcileReservations(); err != nil {
		return Result{State: e.State()}, err
	}
	summary, err := e.advance(&events)
	if err != nil {
		return Result{State: e.State()}, err
	}
	after, _ := e.equity(e.accounts[PlayerAccount])
	summary.TurnPnL = after - before
	e.version++
	return Result{State: e.State(), Summary: summary, Events: events}, nil
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
	player := e.accounts[PlayerAccount]
	buyNotional, err := fixed.Notional(bid, qty)
	if err != nil {
		return false
	}
	if player.Cash < buyNotional {
		return false
	}
	longPosition, err := fixed.AddQty(player.Position, qty)
	if err != nil {
		return false
	}
	shortPosition, err := fixed.SubQty(player.Position, qty)
	if err != nil {
		return false
	}
	if fixed.AbsQty(longPosition) > e.cfg.MaxPosition || fixed.AbsQty(shortPosition) > e.cfg.MaxPosition {
		return false
	}
	equity, err := e.equity(player)
	if err != nil {
		return false
	}
	positionNotional, err := fixed.Notional(e.mark, fixed.AbsQty(player.Position))
	if err != nil {
		return false
	}
	sellNotional, err := fixed.Notional(ask, qty)
	if err != nil {
		return false
	}
	// ReplaceQuotes removes the player's previous quotes atomically, so they
	// must not count toward the prospective reservation.
	gross, err := fixed.AddMoney(positionNotional, buyNotional)
	if err != nil {
		return false
	}
	gross, err = fixed.AddMoney(gross, sellNotional)
	if err != nil {
		return false
	}
	required, err := fixed.ScaleMoney(gross, e.cfg.InitialMarginBps, 10_000)
	if err != nil {
		return false
	}
	return equity >= required
}

func (e *Engine) canPlace(accountID string, side Side, price fixed.Price, qty fixed.Qty) bool {
	return e.canPlaceExcluding(accountID, side, price, qty, 0)
}

func (e *Engine) canPlaceExcluding(accountID string, side Side, price fixed.Price, qty fixed.Qty, excludedOrderID uint64) bool {
	account := e.accounts[accountID]
	if account.External {
		return true
	}
	notional, err := fixed.Notional(price, qty)
	if err != nil {
		return false
	}
	longWorst, shortWorst := account.Position, account.Position
	for _, order := range e.book.Orders(accountID) {
		if order.ID == excludedOrderID {
			continue
		}
		var err error
		if order.Side == orderbook.Buy {
			longWorst, err = fixed.AddQty(longWorst, order.Remaining)
		} else {
			shortWorst, err = fixed.SubQty(shortWorst, order.Remaining)
		}
		if err != nil {
			return false
		}
	}
	var positionErr error
	if side == Buy {
		longWorst, positionErr = fixed.AddQty(longWorst, qty)
	} else {
		shortWorst, positionErr = fixed.SubQty(shortWorst, qty)
	}
	if positionErr != nil {
		return false
	}
	if fixed.AbsQty(longWorst) > e.cfg.MaxPosition || fixed.AbsQty(shortWorst) > e.cfg.MaxPosition {
		return false
	}
	equity, err := e.equity(account)
	if err != nil {
		return false
	}
	gross, err := fixed.Notional(e.mark, fixed.AbsQty(account.Position))
	if err != nil {
		return false
	}
	reservedBuy := fixed.Money(0)
	for _, order := range e.book.Orders(accountID) {
		if order.ID == excludedOrderID {
			continue
		}
		if order.OwnerID == accountID {
			n, err := fixed.Notional(order.Price, order.Remaining)
			if err != nil {
				return false
			}
			gross, err = fixed.AddMoney(gross, n)
			if err != nil {
				return false
			}
			if order.Side == orderbook.Buy {
				reservedBuy, err = fixed.AddMoney(reservedBuy, n)
				if err != nil {
					return false
				}
			}
		}
	}
	if side == Buy {
		reservedBuy, err = fixed.AddMoney(reservedBuy, notional)
		if err != nil {
			return false
		}
	}
	if account.Cash < reservedBuy {
		return false
	}
	gross, err = fixed.AddMoney(gross, notional)
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
	if fixed.AbsQty(a.Position) > e.cfg.MaxPosition {
		return false
	}
	eq, err := e.equity(a)
	if err != nil || eq <= 0 {
		return false
	}
	gross, err := fixed.Notional(e.mark, fixed.AbsQty(a.Position))
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

func (e *Engine) advance(events *[]Event) (Summary, error) {
	s := Summary{}
	if e.cfg.MaxOrdersPerTurn > 0 {
		n := 1 + e.rng.IntN(e.cfg.MaxOrdersPerTurn)
		for range n {
			s.OrdersReceived++
			side := Buy
			if e.rng.IntN(2) == 1 {
				side = Sell
			}
			qty := fixed.Qty(1 + e.rng.Int64N(int64(e.cfg.MaxOrderQty)))
			price, err := e.flowLimit(side)
			if err != nil {
				return Summary{}, err
			}
			order := e.newOrder(FlowAccount, side, price, qty, IOC)
			*events = append(*events, e.emit(Event{Type: "flow_order", Order: copyOrder(order)}))
			trades, lifecycle, err := e.submitBookOrder(order)
			if err != nil {
				return Summary{}, err
			}
			for _, trade := range trades {
				*events = append(*events, e.emit(Event{Type: "trade", Trade: &trade}))
				s.UnitsTraded += trade.Quantity
				if trade.BuyerID == PlayerAccount {
					s.SellVolume += trade.Quantity
					n, _ := fixed.Notional(trade.Price, trade.Quantity)
					s.NetFillCash -= n
				}
				if trade.SellerID == PlayerAccount {
					s.BuyVolume += trade.Quantity
					n, _ := fixed.Notional(trade.Price, trade.Quantity)
					s.NetFillCash += n
				}
			}
			*events = append(*events, e.emitLifecycle(lifecycle)...)
			if err := e.reconcileReservations(); err != nil {
				return Summary{}, err
			}
		}
	}
	player := e.accounts[PlayerAccount]
	storage, err := fixed.Notional(e.cfg.StoragePerUnit, fixed.AbsQty(player.Position))
	if err != nil {
		return Summary{}, err
	}
	if storage > 0 {
		if err := e.ledger.append(LedgerEntry{Type: "storage_charged", Postings: []Posting{{Account: cashAvailable(PlayerAccount), Money: -storage}, {Account: storageAccount, Money: storage}}}); err != nil {
			return Summary{}, err
		}
	}
	player.Cash, err = fixed.AddMoney(player.Cash, -storage)
	if err != nil {
		return Summary{}, err
	}
	s.StorageCost = storage
	if storage > 0 {
		*events = append(*events, e.emit(Event{Type: "storage_charged", Amount: storage}))
	}
	e.turn++
	previous := e.mark
	move := e.cfg.MinMoveBps
	if e.cfg.MaxMoveBps > e.cfg.MinMoveBps {
		move += e.rng.Int64N(e.cfg.MaxMoveBps - e.cfg.MinMoveBps + 1)
	}
	nextMark, err := fixed.ScalePrice(e.mark, 10_000+move, 10_000)
	if err != nil || !nextMark.Positive() {
		return Summary{}, errors.New("mark movement produced invalid price")
	}
	e.mark = nextMark
	*events = append(*events, e.emit(Event{Type: "mark_updated", Mark: e.mark, Message: fmt.Sprintf("previous=%s", previous)}))
	if !e.accountWithinLimits(PlayerAccount, true) {
		e.isOver = true
		eq, _ := e.equity(player)
		if eq <= 0 {
			e.reason = Insolvent
		} else {
			e.reason = MarginBreach
		}
		*events = append(*events, e.emit(Event{Type: "game_ended", Reason: e.reason}))
	} else if e.cfg.NumTurns > 0 && e.turn >= e.cfg.NumTurns {
		e.isOver = true
		e.reason = TurnsComplete
		*events = append(*events, e.emit(Event{Type: "game_ended", Reason: e.reason}))
	}
	return s, nil
}

func (e *Engine) flowLimit(side Side) (fixed.Price, error) {
	slip := int64(0)
	if e.cfg.MaxFlowSlippageBps > 0 {
		slip = e.rng.Int64N(e.cfg.MaxFlowSlippageBps + 1)
	}
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
	return trades, report, nil
}

func (e *Engine) settleReport(report orderbook.Report) ([]Trade, error) {
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
