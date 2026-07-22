// Package orderbook implements a deterministic, single-instrument limit book.
// It has no knowledge of accounts, balances, risk, games, or persistence.
package orderbook

import (
	"errors"
	"sort"

	"market-maker/internal/fixed"
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

// SelfTradePolicy makes the matching behavior explicit. RejectTaker rejects
// an incoming order before any fill when it would cross an order of its owner.
type SelfTradePolicy string

const RejectTaker SelfTradePolicy = "reject_taker"

type Order struct {
	ID        uint64      `json:"id"`
	Sequence  uint64      `json:"sequence"`
	OwnerID   string      `json:"owner_id"`
	Side      Side        `json:"side"`
	Price     fixed.Price `json:"price"`
	Quantity  fixed.Qty   `json:"quantity"`
	Remaining fixed.Qty   `json:"remaining"`
	TIF       TimeInForce `json:"time_in_force"`
}

type Fill struct {
	Maker    Order       `json:"maker"`
	Taker    Order       `json:"taker"`
	Price    fixed.Price `json:"price"`
	Quantity fixed.Qty   `json:"quantity"`
}

type Report struct {
	Accepted  Order   `json:"accepted"`
	Fills     []Fill  `json:"fills"`
	Resting   *Order  `json:"resting,omitempty"`
	Expired   *Order  `json:"expired,omitempty"`
	Completed []Order `json:"completed,omitempty"`
}

// Level is an immutable aggregate used for market-data snapshots.
type Level struct {
	Price      fixed.Price `json:"price"`
	Quantity   fixed.Qty   `json:"quantity"`
	OrderCount int         `json:"order_count"`
}

type node struct {
	order *Order
	level *level
	prev  *node
	next  *node
}

type level struct {
	price fixed.Price
	head  *node
	tail  *node
	qty   fixed.Qty
}

type side struct {
	buy    bool
	levels map[fixed.Price]*level
	prices []fixed.Price
}

// Book maintains a price-level index and FIFO queues. It is not concurrent;
// the exchange sequencer is responsible for serializing mutations.
type Book struct {
	bids   side
	asks   side
	orders map[uint64]*node
}

func New() *Book {
	return &Book{bids: side{buy: true, levels: make(map[fixed.Price]*level)}, asks: side{levels: make(map[fixed.Price]*level)}, orders: make(map[uint64]*node)}
}

func (b *Book) BestBid() fixed.Price { return b.bids.best() }
func (b *Book) BestAsk() fixed.Price { return b.asks.best() }
func (b *Book) Len() int             { return len(b.orders) }
func (b *Book) Bids() []Level        { return b.bids.snapshot(true) }
func (b *Book) Asks() []Level        { return b.asks.snapshot(false) }

func (b *Book) Submit(order Order, policy SelfTradePolicy) (Report, error) {
	if err := validate(order); err != nil {
		return Report{}, err
	}
	if b.orders[order.ID] != nil {
		return Report{}, errors.New("duplicate order id")
	}
	if policy != RejectTaker {
		return Report{}, errors.New("unsupported self-trade policy")
	}
	if b.wouldSelfTrade(order) {
		return Report{}, errors.New("self trade prevention rejected order")
	}

	order.Remaining = order.Quantity
	report := Report{Accepted: order}
	for order.Remaining > 0 {
		maker := b.bestCrossing(order.Side, order.Price)
		if maker == nil {
			break
		}
		quantity := order.Remaining
		if maker.order.Remaining < quantity {
			quantity = maker.order.Remaining
		}
		makerBefore := clone(*maker.order)
		orderBefore := clone(order)
		maker.order.Remaining -= quantity
		maker.level.qty -= quantity
		order.Remaining -= quantity
		fill := Fill{Maker: makerBefore, Taker: orderBefore, Price: maker.order.Price, Quantity: quantity}
		report.Fills = append(report.Fills, fill)
		if maker.order.Remaining == 0 {
			completed := clone(*maker.order)
			b.remove(maker)
			report.Completed = append(report.Completed, completed)
		}
	}
	if order.Remaining == 0 {
		report.Completed = append(report.Completed, clone(order))
		return report, nil
	}
	if order.TIF == IOC {
		expired := clone(order)
		report.Expired = &expired
		return report, nil
	}
	b.rest(order)
	resting := clone(order)
	report.Resting = &resting
	return report, nil
}

func (b *Book) Cancel(orderID uint64) (Order, bool) {
	n := b.orders[orderID]
	if n == nil {
		return Order{}, false
	}
	order := clone(*n.order)
	b.remove(n)
	return order, true
}

// PreviewReplace validates replacement against a copy of the current book.
// The old order is absent from matching and self-trade checks in the preview.
func (b *Book) PreviewReplace(oldOrderID uint64, replacement Order, policy SelfTradePolicy) (Report, error) {
	clone, err := b.without(oldOrderID)
	if err != nil {
		return Report{}, err
	}
	return clone.Submit(replacement, policy)
}

// Replace atomically swaps a resting order for a new order. The replacement
// receives a new ID/sequence from the caller and therefore loses time priority.
func (b *Book) Replace(oldOrderID uint64, replacement Order, policy SelfTradePolicy) (Report, error) {
	clone, err := b.without(oldOrderID)
	if err != nil {
		return Report{}, err
	}
	report, err := clone.Submit(replacement, policy)
	if err != nil {
		return Report{}, err
	}
	*b = *clone
	return report, nil
}

func (b *Book) Order(orderID uint64) (Order, bool) {
	n := b.orders[orderID]
	if n == nil {
		return Order{}, false
	}
	return clone(*n.order), true
}

// Orders returns a stable sequence-ordered view for risk checks and account
// bulk cancellation. Matching itself always uses price then FIFO order.
func (b *Book) Orders(ownerID string) []Order {
	orders := make([]Order, 0)
	for _, n := range b.orders {
		if ownerID == "" || n.order.OwnerID == ownerID {
			orders = append(orders, clone(*n.order))
		}
	}
	sort.Slice(orders, func(i, j int) bool { return orders[i].Sequence < orders[j].Sequence })
	return orders
}

func (b *Book) rest(order Order) {
	s := b.forSide(order.Side)
	l := s.levels[order.Price]
	if l == nil {
		l = &level{price: order.Price}
		s.levels[order.Price] = l
		s.insertPrice(order.Price)
	}
	n := &node{order: &order, level: l, prev: l.tail}
	if l.tail != nil {
		l.tail.next = n
	} else {
		l.head = n
	}
	l.tail = n
	l.qty += order.Remaining
	b.orders[order.ID] = n
}

func (b *Book) remove(n *node) {
	l := n.level
	if n.prev != nil {
		n.prev.next = n.next
	} else {
		l.head = n.next
	}
	if n.next != nil {
		n.next.prev = n.prev
	} else {
		l.tail = n.prev
	}
	l.qty -= n.order.Remaining
	delete(b.orders, n.order.ID)
	if l.head == nil {
		s := b.forSide(n.order.Side)
		delete(s.levels, l.price)
		s.removePrice(l.price)
	}
}

func (b *Book) bestCrossing(takerSide Side, limit fixed.Price) *node {
	opposite := &b.asks
	if takerSide == Sell {
		opposite = &b.bids
	}
	best := opposite.best()
	if best == 0 || (takerSide == Buy && best > limit) || (takerSide == Sell && best < limit) {
		return nil
	}
	return opposite.levels[best].head
}

func (b *Book) wouldSelfTrade(order Order) bool {
	for _, resting := range b.Orders(order.OwnerID) {
		if resting.Side == order.Side {
			continue
		}
		if (order.Side == Buy && resting.Price <= order.Price) || (order.Side == Sell && resting.Price >= order.Price) {
			return true
		}
	}
	return false
}

func (b *Book) without(orderID uint64) (*Book, error) {
	if b.orders[orderID] == nil {
		return nil, errors.New("order not found")
	}
	clone := New()
	for _, order := range b.Orders("") {
		if order.ID == orderID {
			continue
		}
		clone.rest(order)
	}
	return clone, nil
}

func (b *Book) forSide(side Side) *side {
	if side == Buy {
		return &b.bids
	}
	return &b.asks
}

func (s *side) best() fixed.Price {
	if len(s.prices) == 0 {
		return 0
	}
	if s.buy {
		return s.prices[len(s.prices)-1]
	}
	return s.prices[0]
}

func (s *side) insertPrice(price fixed.Price) {
	i := sort.Search(len(s.prices), func(i int) bool { return s.prices[i] >= price })
	s.prices = append(s.prices, 0)
	copy(s.prices[i+1:], s.prices[i:])
	s.prices[i] = price
}

func (s *side) removePrice(price fixed.Price) {
	i := sort.Search(len(s.prices), func(i int) bool { return s.prices[i] >= price })
	if i < len(s.prices) && s.prices[i] == price {
		s.prices = append(s.prices[:i], s.prices[i+1:]...)
	}
}

func (s *side) snapshot(bestFirst bool) []Level {
	result := make([]Level, 0, len(s.prices))
	for i := 0; i < len(s.prices); i++ {
		index := i
		if bestFirst {
			index = len(s.prices) - 1 - i
		}
		l := s.levels[s.prices[index]]
		count := 0
		for n := l.head; n != nil; n = n.next {
			count++
		}
		result = append(result, Level{Price: l.price, Quantity: l.qty, OrderCount: count})
	}
	return result
}

func validate(order Order) error {
	if order.ID == 0 || order.Sequence == 0 || order.OwnerID == "" || (order.Side != Buy && order.Side != Sell) || !order.Price.Positive() || !order.Quantity.Positive() || (order.TIF != GTC && order.TIF != IOC) {
		return errors.New("invalid order")
	}
	return nil
}

func clone(order Order) Order { return order }
