package realtime

import (
	"errors"
	"time"

	"market-maker/internal/exchange"
	"market-maker/internal/fixed"
)

const ActionUpdateQuote = "update_quote"

var ErrQuoteRevisionConflict = errors.New("quote revision conflict")

type UpdateQuotePayload struct {
	Bid              fixed.Price `json:"bid"`
	Ask              fixed.Price `json:"ask"`
	ExpectedRevision *uint64     `json:"expected_revision,omitempty"`
}

type ExchangeExecutorConfig struct {
	QuoteQuantity   fixed.Qty
	CarryPerUnit    fixed.Price
	InformedFlowBps int64
}

type ExchangeExecutor struct {
	engine        *exchange.Engine
	config        ExchangeExecutorConfig
	quoteRevision uint64
}

func (e *ExchangeExecutor) QuoteRevision() uint64 { return e.quoteRevision }

func NewExchangeExecutor(engine *exchange.Engine, config ExchangeExecutorConfig) (*ExchangeExecutor, error) {
	if engine == nil {
		return nil, errors.New("exchange engine is required")
	}
	engineConfig := engine.Config()
	if !config.QuoteQuantity.Positive() || config.QuoteQuantity > engineConfig.MaxOrderQty || config.QuoteQuantity > engineConfig.MaxPosition || config.CarryPerUnit < 0 || config.InformedFlowBps < 0 || config.InformedFlowBps > 10_000 {
		return nil, errors.New("invalid real-time exchange executor config")
	}
	if _, err := fixed.Notional(config.CarryPerUnit, engineConfig.MaxPosition); err != nil {
		return nil, errors.New("carry rate exceeds engine numeric limits")
	}
	return &ExchangeExecutor{engine: engine, config: config}, nil
}

func (e *ExchangeExecutor) Execute(action Action, _ time.Duration) Outcome {
	if action.Source == SourceParticipant {
		return e.executeParticipant(action)
	}
	if action.Source != SourceSystem {
		return Outcome{Disposition: DispositionFail, Err: errors.New("unsupported action source")}
	}
	return e.executeSystem(action)
}

func (e *ExchangeExecutor) executeParticipant(action Action) Outcome {
	if action.Kind == ActionQuitSession {
		if action.Payload != nil {
			return Outcome{Disposition: DispositionReject, Err: errors.New("quit does not accept a payload")}
		}
		result, err := e.engine.QuitRealTime()
		if err != nil {
			return Outcome{Disposition: DispositionReject, Err: err}
		}
		return Outcome{Result: result, Disposition: DispositionComplete}
	}
	if action.Kind != ActionUpdateQuote {
		return Outcome{Disposition: DispositionReject, Err: errors.New("unsupported participant action")}
	}
	payload, ok := action.Payload.(UpdateQuotePayload)
	if !ok {
		return Outcome{Disposition: DispositionReject, Err: errors.New("invalid quote payload")}
	}
	if payload.ExpectedRevision != nil && *payload.ExpectedRevision != e.quoteRevision {
		return Outcome{Disposition: DispositionReject, Err: ErrQuoteRevisionConflict}
	}
	result, err := e.engine.UpdateQuote(payload.Bid, payload.Ask, e.config.QuoteQuantity)
	if err != nil {
		return Outcome{Disposition: DispositionReject, Err: err}
	}
	e.quoteRevision++
	disposition := DispositionContinue
	if result.State.IsOver {
		disposition = DispositionComplete
	}
	return Outcome{Result: result, Disposition: disposition}
}

func (e *ExchangeExecutor) executeSystem(action Action) Outcome {
	var result exchange.Result
	var err error
	switch action.Kind {
	case ActionCustomerArrival:
		payload, ok := action.Payload.(CustomerArrivalPayload)
		engineConfig := e.engine.Config()
		if !ok || payload.InformedDraw >= 10_000 || !payload.HasUpcomingMark && payload.UpcomingMarkMoveBps != 0 || payload.HasUpcomingMark && (payload.UpcomingMarkMoveBps < engineConfig.MinMoveBps || payload.UpcomingMarkMoveBps > engineConfig.MaxMoveBps) {
			return Outcome{Disposition: DispositionFail, Err: errors.New("invalid customer payload")}
		}
		side := exchange.Sell
		if payload.Buy {
			side = exchange.Buy
		}
		informed := false
		informedMark := fixed.Price(0)
		if payload.HasUpcomingMark && payload.InformedDraw < uint64(e.config.InformedFlowBps) {
			mark := e.engine.State().Mark
			next, moveErr := fixed.ScalePrice(mark, 10_000+payload.UpcomingMarkMoveBps, 10_000)
			if moveErr != nil || !next.Positive() {
				return Outcome{Disposition: DispositionFail, Err: errors.New("upcoming mark movement is invalid")}
			}
			informed = next != mark
			if informed {
				informedMark = next
				if next > mark {
					side = exchange.Buy
				} else {
					side = exchange.Sell
				}
			}
		}
		result, err = e.engine.ApplyCustomerArrival(exchange.CustomerArrival{Side: side, Quantity: payload.Quantity, SlippageBps: payload.SlippageBps, Informed: informed, InformedMark: informedMark})
	case ActionMarkMove:
		payload, ok := action.Payload.(MarkMovePayload)
		if !ok {
			return Outcome{Disposition: DispositionFail, Err: errors.New("invalid mark payload")}
		}
		result, err = e.engine.ApplyMarkMove(payload.BasisPoints)
	case ActionCarryCharge:
		if action.Payload != nil {
			return Outcome{Disposition: DispositionFail, Err: errors.New("carry action does not accept a payload")}
		}
		result, err = e.engine.ApplyCarry(e.config.CarryPerUnit)
	case ActionTimeExpired:
		if action.Payload != nil {
			return Outcome{Disposition: DispositionFail, Err: errors.New("expiry action does not accept a payload")}
		}
		result, err = e.engine.ExpireTradingDay()
	default:
		return Outcome{Disposition: DispositionFail, Err: errors.New("unsupported system action")}
	}
	if err != nil {
		return Outcome{Disposition: DispositionFail, Err: err}
	}
	disposition := DispositionContinue
	if result.State.IsOver || action.Kind == ActionTimeExpired {
		disposition = DispositionComplete
	}
	return Outcome{Result: result, Disposition: disposition}
}
