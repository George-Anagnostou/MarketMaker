package main

import (
	"bytes"
	"flag"
	"strings"
	"testing"

	"market-maker/internal/exchange"
	"market-maker/internal/fixed"
)

func TestConfigFromFlagsUsesAdverseSelectionAndValidatesInformedFlow(t *testing.T) {
	original := *informedFlowBps
	t.Cleanup(func() { *informedFlowBps = original })

	*informedFlowBps = 0
	cfg, err := configFromFlags()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.SimulationVersion != exchange.SimulationVersionAdverseSelection || cfg.InformedFlowBps != 0 {
		t.Fatalf("version=%d informed_flow_bps=%d", cfg.SimulationVersion, cfg.InformedFlowBps)
	}
	configuredFlag := flag.Lookup("informed-flow-bps")
	if configuredFlag == nil || configuredFlag.DefValue != "0" || !strings.Contains(configuredFlag.Usage, "0..10000") {
		t.Fatalf("flag=%+v", configuredFlag)
	}
	*informedFlowBps = 7_500
	cfg, err = configFromFlags()
	if err != nil || cfg.InformedFlowBps != 7_500 {
		t.Fatalf("valid informed flow: config=%+v error=%v", cfg, err)
	}

	for _, invalid := range []int64{-1, 10_001} {
		*informedFlowBps = invalid
		if _, err := configFromFlags(); err == nil || !strings.Contains(err.Error(), "informed flow must be between 0 and 10000 bps") {
			t.Fatalf("informed_flow_bps=%d error=%v", invalid, err)
		}
	}
}

func TestPrintResultUsesPlayerPerspectiveForInformedTrades(t *testing.T) {
	result := exchange.Result{Events: []exchange.Event{
		{Type: "trade", Trade: &exchange.Trade{BuyerID: exchange.PlayerAccount, SellerID: exchange.FlowAccount, Quantity: fixed.Qty(12_500), Price: fixed.Price(1_000_000), Informed: true}},
		{Type: "trade", Trade: &exchange.Trade{BuyerID: exchange.FlowAccount, SellerID: exchange.PlayerAccount, Quantity: fixed.Qty(20_000), Price: fixed.Price(1_010_000), Informed: true}},
	}}
	var output bytes.Buffer
	printResult(&output, result)

	for _, expected := range []string{
		"Trade: you bought 1.2500 @ $100.0000 from an informed customer",
		"Trade: you sold 2.0000 @ $101.0000 to an informed customer",
	} {
		if !strings.Contains(output.String(), expected) {
			t.Errorf("missing %q in:\n%s", expected, output.String())
		}
	}
}

func TestPrintResultShowsAuthoritativeAttributionAndInformedMetrics(t *testing.T) {
	result := exchange.Result{Summary: exchange.Summary{
		TurnPnL:              fixed.Money(-250_000_000),
		PnLAttribution:       &exchange.PnLAttribution{ExecutionEdge: fixed.Money(150_000_000), InventoryMarkPnL: fixed.Money(-300_000_000), StoragePnL: fixed.Money(-100_000_000)},
		InformedOrders:       3,
		InformedOrdersFilled: 2,
		InformedUnitsTraded:  fixed.Qty(25_000),
		InformedFlowPnL:      fixed.Money(-75_000_000),
	}}
	var output bytes.Buffer
	printResult(&output, result)

	for _, expected := range []string{
		"Turn P&L attribution:",
		"Execution edge: +$1.50000000",
		"Inventory mark: -$3.00000000",
		"Storage: -$1.00000000",
		"Total: -$2.50000000",
		"Informed flow: 3 arrived | 2 filled | 2.5000 units | P&L -$0.75000000",
	} {
		if !strings.Contains(output.String(), expected) {
			t.Errorf("missing %q in:\n%s", expected, output.String())
		}
	}
}

func TestPrintResultKeepsLegacySummaryFallback(t *testing.T) {
	result := exchange.Result{Summary: exchange.Summary{NetFillCash: fixed.Money(100_000_000), StorageCost: fixed.Money(25_000_000), TurnPnL: fixed.Money(75_000_000)}}
	var output bytes.Buffer
	printResult(&output, result)

	want := "Turn fill cash: $1.00000000 | storage: $0.25000000 | turn P&L: $0.75000000"
	if !strings.Contains(output.String(), want) {
		t.Fatalf("missing %q in:\n%s", want, output.String())
	}
	if strings.Contains(output.String(), "Turn P&L attribution:") {
		t.Fatalf("legacy result rendered attribution:\n%s", output.String())
	}
}
