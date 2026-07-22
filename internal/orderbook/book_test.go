package orderbook

import (
	"market-maker/internal/fixed"
	"testing"
)

func price(t *testing.T, value string) fixed.Price {
	t.Helper()
	p, err := fixed.ParsePrice(value)
	if err != nil {
		t.Fatal(err)
	}
	return p
}
func qty(t *testing.T, value string) fixed.Qty {
	t.Helper()
	q, err := fixed.ParseQty(value)
	if err != nil {
		t.Fatal(err)
	}
	return q
}
func order(id, sequence uint64, owner string, side Side, p, q string, tif TimeInForce, t *testing.T) Order {
	return Order{ID: id, Sequence: sequence, OwnerID: owner, Side: side, Price: price(t, p), Quantity: qty(t, q), TIF: tif}
}

func TestPriceThenTimePriorityAndMakerPrice(t *testing.T) {
	b := New()
	for _, o := range []Order{order(1, 1, "a", Sell, "101", "1", GTC, t), order(2, 2, "b", Sell, "100", "1", GTC, t), order(3, 3, "c", Sell, "100", "1", GTC, t)} {
		if _, err := b.Submit(o, RejectTaker); err != nil {
			t.Fatal(err)
		}
	}
	report, err := b.Submit(order(4, 4, "buyer", Buy, "102", "3.5", IOC, t), RejectTaker)
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Fills) != 3 {
		t.Fatalf("fills=%d", len(report.Fills))
	}
	if report.Fills[0].Maker.ID != 2 || report.Fills[1].Maker.ID != 3 || report.Fills[2].Maker.ID != 1 {
		t.Fatalf("maker order=%+v", report.Fills)
	}
	if got := report.Fills[0].Price.String(); got != "100.0000" {
		t.Fatalf("price=%s", got)
	}
	if report.Expired == nil || report.Expired.Remaining.String() != "0.5000" {
		t.Fatalf("ioc result=%+v", report.Expired)
	}
}

func TestPartialFillCancelAndBestPrices(t *testing.T) {
	b := New()
	if _, err := b.Submit(order(1, 1, "seller", Sell, "100", "3", GTC, t), RejectTaker); err != nil {
		t.Fatal(err)
	}
	report, err := b.Submit(order(2, 2, "buyer", Buy, "101", "1", IOC, t), RejectTaker)
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Fills) != 1 || report.Fills[0].Quantity != qty(t, "1") {
		t.Fatalf("fills=%+v", report.Fills)
	}
	remaining, ok := b.Order(1)
	if !ok || remaining.Remaining != qty(t, "2") {
		t.Fatalf("remaining=%+v", remaining)
	}
	if b.BestAsk() != price(t, "100") || b.BestBid() != 0 {
		t.Fatal("incorrect best prices")
	}
	canceled, ok := b.Cancel(1)
	if !ok || canceled.Remaining != qty(t, "2") || b.Len() != 0 {
		t.Fatalf("cancel=%+v len=%d", canceled, b.Len())
	}
}

func TestDepthSnapshotsAreBestPriceFirst(t *testing.T) {
	b := New()
	for _, o := range []Order{order(1, 1, "a", Buy, "99", "1", GTC, t), order(2, 2, "b", Buy, "100", "2", GTC, t), order(3, 3, "c", Buy, "100", "3", GTC, t), order(4, 4, "d", Sell, "102", "1", GTC, t), order(5, 5, "e", Sell, "101", "2", GTC, t)} {
		if _, err := b.Submit(o, RejectTaker); err != nil {
			t.Fatal(err)
		}
	}
	bids, asks := b.Bids(), b.Asks()
	if len(bids) != 2 || bids[0].Price != price(t, "100") || bids[0].Quantity != qty(t, "5") || bids[0].OrderCount != 2 {
		t.Fatalf("bids=%+v", bids)
	}
	if len(asks) != 2 || asks[0].Price != price(t, "101") || asks[0].Quantity != qty(t, "2") {
		t.Fatalf("asks=%+v", asks)
	}
}

func TestSelfTradeRejectedBeforeAnyFill(t *testing.T) {
	b := New()
	if _, err := b.Submit(order(1, 1, "same", Sell, "101", "1", GTC, t), RejectTaker); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Submit(order(2, 2, "other", Sell, "100", "1", GTC, t), RejectTaker); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Submit(order(3, 3, "same", Buy, "101", "1", IOC, t), RejectTaker); err == nil {
		t.Fatal("expected self trade rejection")
	}
	if _, ok := b.Order(2); !ok {
		t.Fatal("rejected order consumed third-party liquidity")
	}
}

func TestReplacePreviewsAndCommitsWithoutOldOrder(t *testing.T) {
	b := New()
	if _, err := b.Submit(order(1, 1, "buyer", Buy, "99", "1", GTC, t), RejectTaker); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Submit(order(2, 2, "seller", Sell, "100", "1", GTC, t), RejectTaker); err != nil {
		t.Fatal(err)
	}
	replacement := order(3, 3, "buyer", Buy, "101", "1", GTC, t)
	preview, err := b.PreviewReplace(1, replacement, RejectTaker)
	if err != nil || len(preview.Fills) != 1 {
		t.Fatalf("preview=%+v err=%v", preview, err)
	}
	if _, ok := b.Order(1); !ok {
		t.Fatal("preview changed book")
	}
	report, err := b.Replace(1, replacement, RejectTaker)
	if err != nil || len(report.Fills) != 1 {
		t.Fatalf("replace=%+v err=%v", report, err)
	}
	if _, ok := b.Order(1); ok {
		t.Fatal("old order remains")
	}
	if b.Len() != 0 {
		t.Fatalf("book len=%d", b.Len())
	}
}
