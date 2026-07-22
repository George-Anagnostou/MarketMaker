package exchange

import (
	"errors"

	"market-maker/internal/fixed"
)

type LedgerAccount string

type Posting struct {
	Account    LedgerAccount `json:"account"`
	Money      fixed.Money   `json:"money,omitempty"`
	Instrument fixed.Qty     `json:"instrument,omitempty"`
}

type LedgerReference struct {
	OrderID      uint64 `json:"order_id,omitempty"`
	TradeID      uint64 `json:"trade_id,omitempty"`
	MakerOrderID uint64 `json:"maker_order_id,omitempty"`
	TakerOrderID uint64 `json:"taker_order_id,omitempty"`
}

// LedgerEntry is an immutable balanced journal entry. Money and instrument
// each balance independently to zero.
type LedgerEntry struct {
	ID        uint64          `json:"id"`
	Type      string          `json:"type"`
	Reference LedgerReference `json:"reference,omitempty"`
	Postings  []Posting       `json:"postings"`
}

type ledger struct {
	nextID   uint64
	entries  []LedgerEntry
	balances map[LedgerAccount]Posting
}

func newLedger() ledger { return ledger{balances: make(map[LedgerAccount]Posting)} }

func (l *ledger) append(entries ...LedgerEntry) error {
	balances := make(map[LedgerAccount]Posting, len(l.balances))
	for account, balance := range l.balances {
		balances[account] = balance
	}
	for _, entry := range entries {
		if entry.Type == "" || len(entry.Postings) < 2 {
			return errors.New("invalid ledger entry")
		}
		money, instrument := fixed.Money(0), fixed.Qty(0)
		for _, posting := range entry.Postings {
			if posting.Account == "" {
				return errors.New("ledger posting account is required")
			}
			var err error
			money, err = fixed.AddMoney(money, posting.Money)
			if err != nil {
				return err
			}
			instrument, err = fixed.AddQty(instrument, posting.Instrument)
			if err != nil {
				return err
			}
			balance := balances[posting.Account]
			balance.Account = posting.Account
			balance.Money, err = fixed.AddMoney(balance.Money, posting.Money)
			if err != nil {
				return err
			}
			balance.Instrument, err = fixed.AddQty(balance.Instrument, posting.Instrument)
			if err != nil {
				return err
			}
			balances[posting.Account] = balance
		}
		if money != 0 || instrument != 0 {
			return errors.New("unbalanced ledger entry")
		}
	}
	for _, entry := range entries {
		l.nextID++
		entry.ID = l.nextID
		entry.Postings = append([]Posting(nil), entry.Postings...)
		l.entries = append(l.entries, entry)
	}
	l.balances = balances
	return nil
}

func (l *ledger) entriesCopy() []LedgerEntry {
	entries := make([]LedgerEntry, len(l.entries))
	for i, entry := range l.entries {
		entries[i] = entry
		entries[i].Postings = append([]Posting(nil), entry.Postings...)
	}
	return entries
}

func (l *ledger) balance(account LedgerAccount) Posting { return l.balances[account] }

func (l *ledger) clone() ledger {
	clone := newLedger()
	clone.nextID = l.nextID
	clone.entries = l.entriesCopy()
	for account, balance := range l.balances {
		clone.balances[account] = balance
	}
	return clone
}

func cashAvailable(id string) LedgerAccount       { return LedgerAccount("cash/available/" + id) }
func cashReserved(id string) LedgerAccount        { return LedgerAccount("cash/reserved/" + id) }
func instrumentAvailable(id string) LedgerAccount { return LedgerAccount("instrument/available/" + id) }
func instrumentReserved(id string) LedgerAccount  { return LedgerAccount("instrument/reserved/" + id) }
func openingCashAccount(id string) LedgerAccount  { return LedgerAccount("cash/external/opening/" + id) }
func openingInstrumentAccount(id string) LedgerAccount {
	return LedgerAccount("instrument/external/opening/" + id)
}

const (
	storageAccount LedgerAccount = "cash/venue/storage"
)
