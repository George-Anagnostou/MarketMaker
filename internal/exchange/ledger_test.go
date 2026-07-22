package exchange

import (
	"testing"

	"market-maker/internal/fixed"
)

func TestLedgerRejectsUnbalancedEntriesWithoutMutation(t *testing.T) {
	ledger := newLedger()
	err := ledger.append(LedgerEntry{Type: "bad", Postings: []Posting{{Account: "cash/a", Money: fixed.Money(1)}, {Account: "cash/b"}}})
	if err == nil {
		t.Fatal("expected unbalanced entry rejection")
	}
	if len(ledger.entries) != 0 || len(ledger.balances) != 0 {
		t.Fatal("rejected entry changed ledger")
	}
}

func TestLedgerAppendsBatchesAtomically(t *testing.T) {
	ledger := newLedger()
	valid := LedgerEntry{Type: "valid", Postings: []Posting{{Account: "cash/a", Money: fixed.Money(10)}, {Account: "cash/b", Money: fixed.Money(-10)}}}
	invalid := LedgerEntry{Type: "invalid", Postings: []Posting{{Account: "cash/a", Money: fixed.Money(1)}, {Account: "cash/b"}}}
	if err := ledger.append(valid, invalid); err == nil {
		t.Fatal("expected batch rejection")
	}
	if len(ledger.entries) != 0 || len(ledger.balances) != 0 {
		t.Fatal("rejected batch partially applied")
	}
	if err := ledger.append(valid); err != nil {
		t.Fatal(err)
	}
	if len(ledger.entries) != 1 || ledger.balance("cash/a").Money != fixed.Money(10) || ledger.balance("cash/b").Money != fixed.Money(-10) {
		t.Fatalf("ledger=%+v", ledger)
	}
}
