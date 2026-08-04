package main

import (
	"bytes"
	"flag"
	"strings"
	"testing"

	"market-maker/internal/exchange"
	"market-maker/internal/fixed"
)

func TestConfigFromFlagsDefaultsToLegacy(t *testing.T) {
	originalVersion, originalInformed := *simulationVersion, *informedFlowBps
	t.Cleanup(func() { *simulationVersion, *informedFlowBps = originalVersion, originalInformed })

	*simulationVersion = 1
	*informedFlowBps = 0
	cfg, err := configFromFlags()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.SimulationVersion != exchange.SimulationVersionLegacy || cfg.InformedFlowBps != 0 {
		t.Fatalf("version=%d informed_flow_bps=%d", cfg.SimulationVersion, cfg.InformedFlowBps)
	}
	versionFlag := flag.Lookup("simulation-version")
	if versionFlag == nil || versionFlag.DefValue != "1" || !strings.Contains(versionFlag.Usage, "1 = legacy") {
		t.Fatalf("flag=%+v", versionFlag)
	}
	configuredFlag := flag.Lookup("informed-flow-bps")
	if configuredFlag == nil || configuredFlag.DefValue != "0" || !strings.Contains(configuredFlag.Usage, "0..10000") {
		t.Fatalf("flag=%+v", configuredFlag)
	}
}

func TestConfigFromFlagsAcceptsExplicitLegacy(t *testing.T) {
	originalVersion, originalInformed := *simulationVersion, *informedFlowBps
	t.Cleanup(func() { *simulationVersion, *informedFlowBps = originalVersion, originalInformed })

	*simulationVersion = 1
	*informedFlowBps = 0
	cfg, err := configFromFlags()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.SimulationVersion != exchange.SimulationVersionLegacy {
		t.Fatalf("version=%d", cfg.SimulationVersion)
	}
}

func TestConfigFromFlagsRejectsUnsupportedSimulationVersion(t *testing.T) {
	originalVersion, originalInformed := *simulationVersion, *informedFlowBps
	t.Cleanup(func() { *simulationVersion, *informedFlowBps = originalVersion, originalInformed })

	*simulationVersion = 3
	*informedFlowBps = 0
	if _, err := configFromFlags(); err == nil || !strings.Contains(err.Error(), "unsupported simulation version") {
		t.Fatalf("error=%v", err)
	}
}

func TestConfigFromFlagsRejectsInformedFlowForLegacy(t *testing.T) {
	originalVersion, originalInformed := *simulationVersion, *informedFlowBps
	t.Cleanup(func() { *simulationVersion, *informedFlowBps = originalVersion, originalInformed })

	*simulationVersion = 1
	*informedFlowBps = 1
	if _, err := configFromFlags(); err == nil || !strings.Contains(err.Error(), "legacy simulation requires zero informed flow") {
		t.Fatalf("error=%v", err)
	}
}

func TestConfigFromFlagsValidatesAdverseSelectionInformedFlow(t *testing.T) {
	originalVersion, originalInformed := *simulationVersion, *informedFlowBps
	t.Cleanup(func() { *simulationVersion, *informedFlowBps = originalVersion, originalInformed })

	*simulationVersion = 2
	*informedFlowBps = 7_500
	cfg, err := configFromFlags()
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

func TestPrintBannerPreservesLegacyDefaultAndExplainsVersionTwo(t *testing.T) {
	tests := []struct {
		name    string
		version exchange.SimulationVersion
		want    string
	}{
		{
			name:    "legacy",
			version: exchange.SimulationVersionLegacy,
			want: "=== Market Maker Exchange ===\n" +
				"Scenario seed: 42 | margin: 50.00% initial / 25.00% maintenance\n",
		},
		{
			name:    "version two",
			version: exchange.SimulationVersionAdverseSelection,
			want: "=== Market Maker Exchange ===\n" +
				"Scenario seed: 42 | simulation version: 2 | margin: 50.00% initial / 25.00% maintenance\n",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var output bytes.Buffer
			printBanner(&output, exchange.Config{Seed: 42, SimulationVersion: test.version, InitialMarginBps: 5_000, MaintenanceMarginBps: 2_500})
			if got := output.String(); got != test.want {
				t.Fatalf("banner mismatch\nwant:\n%s\ngot:\n%s", test.want, got)
			}
		})
	}
}

func TestPrintResultPreservesLegacyTradeLine(t *testing.T) {
	result := exchange.Result{Events: []exchange.Event{
		{Type: "trade", Trade: &exchange.Trade{BuyerID: exchange.PlayerAccount, SellerID: exchange.FlowAccount, Quantity: fixed.Qty(12_500), Price: fixed.Price(1_000_000)}},
	}}
	var output bytes.Buffer
	printResult(&output, result, exchange.SimulationVersionLegacy)

	want := "Trade: 1.2500 100.0000 @ player buys\n" +
		"Turn fill cash: $0.00000000 | storage: $0.00000000 | turn P&L: $0.00000000\n" +
		"Turn 0 | Cash $0.00000000 | Position 0.0000 | Mark $0.0000 | Equity $0.00000000 | Book 0.0000 / 0.0000\n"
	if got := output.String(); got != want {
		t.Fatalf("legacy result mismatch\nwant:\n%s\ngot:\n%s", want, got)
	}
}

func TestPrintResultUsesVersionTwoTradeWording(t *testing.T) {
	result := exchange.Result{Events: []exchange.Event{
		{Type: "trade", Trade: &exchange.Trade{BuyerID: exchange.PlayerAccount, SellerID: exchange.FlowAccount, Quantity: fixed.Qty(12_500), Price: fixed.Price(1_000_000), Informed: true}},
		{Type: "trade", Trade: &exchange.Trade{BuyerID: exchange.FlowAccount, SellerID: exchange.PlayerAccount, Quantity: fixed.Qty(20_000), Price: fixed.Price(1_010_000), Informed: true}},
	}}
	var output bytes.Buffer
	printResult(&output, result, exchange.SimulationVersionAdverseSelection)

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
	printResult(&output, result, exchange.SimulationVersionAdverseSelection)

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

func TestPrintResultGatesVersionTwoExplanationsFromLegacy(t *testing.T) {
	result := exchange.Result{Summary: exchange.Summary{
		NetFillCash:          fixed.Money(100_000_000),
		StorageCost:          fixed.Money(25_000_000),
		TurnPnL:              fixed.Money(75_000_000),
		PnLAttribution:       &exchange.PnLAttribution{ExecutionEdge: fixed.Money(100_000_000)},
		InformedOrders:       1,
		InformedOrdersFilled: 1,
		InformedUnitsTraded:  fixed.Qty(10_000),
		InformedFlowPnL:      fixed.Money(50_000_000),
	}}
	var output bytes.Buffer
	printResult(&output, result, exchange.SimulationVersionLegacy)

	want := "Turn fill cash: $1.00000000 | storage: $0.25000000 | turn P&L: $0.75000000"
	if !strings.Contains(output.String(), want) {
		t.Fatalf("missing %q in:\n%s", want, output.String())
	}
	for _, versionTwoText := range []string{"Turn P&L attribution:", "Informed flow:"} {
		if strings.Contains(output.String(), versionTwoText) {
			t.Fatalf("legacy result rendered %q:\n%s", versionTwoText, output.String())
		}
	}
}
