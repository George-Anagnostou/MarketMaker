package realtime

import (
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"reflect"
	"time"

	"market-maker/internal/fixed"
	"market-maker/internal/game"
)

const (
	ActionCustomerArrival = "customer_arrival"
	ActionMarkMove        = "mark_move"
	ActionCarryCharge     = "carry_charge"
	ActionTimeExpired     = "time_expired"
)

type Interval struct {
	Minimum time.Duration
	Maximum time.Duration
}

type GeneratorConfig struct {
	Version            uint32
	Seed               uint64
	Duration           time.Duration
	CustomerCadence    Interval
	MarkCadence        Interval
	CarryCadence       time.Duration
	MaxOrderQuantity   fixed.Qty
	MaxFlowSlippageBps int64
	MinMoveBps         int64
	MaxMoveBps         int64
	CustomerDomain     string
	MarkDomain         string
	InformedDomain     string
}

type CustomerArrivalPayload struct {
	Buy                 bool      `json:"buy"`
	Quantity            fixed.Qty `json:"quantity"`
	SlippageBps         int64     `json:"slippage_bps"`
	InformedDraw        uint64    `json:"informed_draw"`
	HasUpcomingMark     bool      `json:"has_upcoming_mark"`
	UpcomingMarkMoveBps int64     `json:"upcoming_mark_move_bps,omitempty"`
}

type MarkMovePayload struct {
	BasisPoints int64 `json:"basis_points"`
}

type generatorCandidate struct {
	due      time.Duration
	sequence uint64
	kind     string
	payload  any
	actionID string
}

type deterministicStream struct{ state uint64 }

func (s *deterministicStream) next() uint64 {
	s.state += 0x9e3779b97f4a7c15
	value := s.state
	value = (value ^ (value >> 30)) * 0xbf58476d1ce4e5b9
	value = (value ^ (value >> 27)) * 0x94d049bb133111eb
	return value ^ (value >> 31)
}

func (s *deterministicStream) bounded(bound uint64) uint64 {
	threshold := -bound % bound
	for {
		value := s.next()
		if value >= threshold {
			return value % bound
		}
	}
}

type Generator struct {
	config       GeneratorConfig
	customer     deterministicStream
	mark         deterministicStream
	informed     deterministicStream
	generated    uint64
	committed    uint64
	lastCustomer time.Duration
	lastMark     time.Duration
	lastCarry    time.Duration
	candidates   map[string]*generatorCandidate
}

func NewGenerator(config GeneratorConfig) (*Generator, error) {
	if err := validateGeneratorConfig(config); err != nil {
		return nil, err
	}
	generator := &Generator{
		config:     config,
		customer:   deterministicStream{state: deriveStreamSeed(config.Seed, config.CustomerDomain)},
		mark:       deterministicStream{state: deriveStreamSeed(config.Seed, config.MarkDomain)},
		informed:   deterministicStream{state: deriveStreamSeed(config.Seed, config.InformedDomain)},
		candidates: make(map[string]*generatorCandidate, 4),
	}
	// Generation order is itself the deterministic tie-breaker.
	generator.generateCustomer()
	generator.generateMark()
	generator.generateCarry()
	generator.addCandidate(config.Duration, ActionTimeExpired, nil)
	return generator, nil
}

func validateGeneratorConfig(config GeneratorConfig) error {
	if config.Version != game.GeneratorVersion || config.Seed == 0 || config.Duration <= 0 || config.Duration > 24*time.Hour {
		return errors.New("unsupported generator version or invalid seed or duration")
	}
	if err := validateInterval("customer", config.CustomerCadence, config.Duration); err != nil {
		return err
	}
	if err := validateInterval("mark", config.MarkCadence, config.Duration); err != nil {
		return err
	}
	if config.CarryCadence <= 0 || config.CarryCadence > config.Duration {
		return errors.New("carry cadence must fit within duration")
	}
	if !config.MaxOrderQuantity.Positive() || config.MaxFlowSlippageBps < 0 || config.MaxFlowSlippageBps > 10_000 {
		return errors.New("invalid customer order limits")
	}
	if config.MinMoveBps > config.MaxMoveBps || config.MinMoveBps <= -10_000 || config.MaxMoveBps > 10_000 {
		return errors.New("invalid mark movement range")
	}
	if config.CustomerDomain == "" || config.MarkDomain == "" || config.InformedDomain == "" || config.CustomerDomain == config.MarkDomain || config.CustomerDomain == config.InformedDomain || config.MarkDomain == config.InformedDomain {
		return errors.New("generator domains must be non-empty and distinct")
	}
	return nil
}

func validateInterval(name string, interval Interval, duration time.Duration) error {
	if interval.Minimum <= 0 || interval.Minimum > interval.Maximum || interval.Maximum > duration {
		return fmt.Errorf("%s interval must be ordered and fit within duration", name)
	}
	return nil
}

func deriveStreamSeed(seed uint64, domain string) uint64 {
	input := make([]byte, 8+len(domain))
	binary.LittleEndian.PutUint64(input, seed)
	copy(input[8:], domain)
	digest := sha256.Sum256(input)
	return binary.LittleEndian.Uint64(digest[:8])
}

func (g *Generator) Next() (ScheduledAction, bool, error) {
	candidate := g.nextCandidate()
	if candidate == nil {
		return ScheduledAction{}, false, nil
	}
	payload := candidate.payload
	if candidate.kind == ActionCustomerArrival {
		customer := payload.(CustomerArrivalPayload)
		if mark := g.candidates[ActionMarkMove]; mark != nil {
			customer.HasUpcomingMark = true
			customer.UpcomingMarkMoveBps = mark.payload.(MarkMovePayload).BasisPoints
		}
		payload = customer
	}
	return ScheduledAction{Due: candidate.due, Action: Action{ID: candidate.actionID, Kind: candidate.kind, Source: SourceSystem, Payload: payload}}, true, nil
}

func (g *Generator) Commit(action ScheduledAction) error {
	candidate := g.nextCandidate()
	next, ok, err := g.Next()
	if err != nil {
		return err
	}
	if candidate == nil || !ok || !reflect.DeepEqual(next, action) {
		return errors.New("committed action does not match generator head")
	}
	switch candidate.kind {
	case ActionCustomerArrival, ActionMarkMove, ActionCarryCharge, ActionTimeExpired:
	default:
		return errors.New("generator contains unknown action kind")
	}
	delete(g.candidates, candidate.kind)
	g.committed++
	switch candidate.kind {
	case ActionCustomerArrival:
		g.lastCustomer = candidate.due
		g.generateCustomer()
	case ActionMarkMove:
		g.lastMark = candidate.due
		g.generateMark()
	case ActionCarryCharge:
		g.lastCarry = candidate.due
		g.generateCarry()
	case ActionTimeExpired:
	}
	return nil
}

func (g *Generator) nextCandidate() *generatorCandidate {
	var next *generatorCandidate
	for _, candidate := range g.candidates {
		if next == nil || candidate.due < next.due || candidate.due == next.due && candidate.sequence < next.sequence {
			next = candidate
		}
	}
	return next
}

func (g *Generator) generateCustomer() {
	due := g.lastCustomer + g.drawInterval(&g.customer, g.config.CustomerCadence)
	if due >= g.config.Duration {
		return
	}
	payload := CustomerArrivalPayload{
		Buy:          g.customer.bounded(2) == 0,
		Quantity:     fixed.Qty(1 + g.customer.bounded(uint64(g.config.MaxOrderQuantity))),
		SlippageBps:  int64(g.customer.bounded(uint64(g.config.MaxFlowSlippageBps + 1))),
		InformedDraw: g.informed.bounded(10_000),
	}
	g.addCandidate(due, ActionCustomerArrival, payload)
}

func (g *Generator) generateMark() {
	due := g.lastMark + g.drawInterval(&g.mark, g.config.MarkCadence)
	if due >= g.config.Duration {
		return
	}
	moveRange := uint64(g.config.MaxMoveBps - g.config.MinMoveBps + 1)
	move := g.config.MinMoveBps + int64(g.mark.bounded(moveRange))
	g.addCandidate(due, ActionMarkMove, MarkMovePayload{BasisPoints: move})
}

func (g *Generator) generateCarry() {
	due := g.lastCarry + g.config.CarryCadence
	if due >= g.config.Duration {
		return
	}
	g.addCandidate(due, ActionCarryCharge, nil)
}

func (g *Generator) drawInterval(stream *deterministicStream, interval Interval) time.Duration {
	width := uint64(interval.Maximum - interval.Minimum + 1)
	return interval.Minimum + time.Duration(stream.bounded(width))
}

func (g *Generator) addCandidate(due time.Duration, kind string, payload any) {
	g.generated++
	g.candidates[kind] = &generatorCandidate{due: due, sequence: g.generated, kind: kind, payload: payload, actionID: fmt.Sprintf("%s%s/%d", SystemActionIDPrefix, kind, g.generated)}
}

type GeneratorSnapshot struct {
	Generated uint64
	Committed uint64
}

func (g *Generator) Snapshot() GeneratorSnapshot {
	return GeneratorSnapshot{Generated: g.generated, Committed: g.committed}
}
