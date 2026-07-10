package main

import (
	"bufio"
	"flag"
	"fmt"
	"os"
	"strconv"
	"strings"

	"market-maker/internal/game"
	"market-maker/internal/types"
)

var (
	startingCash      = flag.Float64("cash", 100000, "starting cash")
	startingInventory = flag.Float64("inventory", 0, "starting inventory")
	startingPrice     = flag.Float64("price", 100, "starting price")
	numTurns          = flag.Int("turns", 10, "number of turns (0 = unlimited)")
	storageCost       = flag.Float64("storage", 1.0, "storage cost per unit per turn")
	seed              = flag.Int64("seed", 0, "RNG seed for reproducibility (0 = random)")
	vol               = flag.String("vol", "-0.5,3", `price volatility as "min,max" percent, e.g. "-0.5,3"`)
)

func main() {
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "market-maker: single-player market making game\n\n")
		fmt.Fprintf(os.Stderr, "Usage:\n")
		flag.PrintDefaults()
		fmt.Fprintf(os.Stderr, "\nExamples:\n")
		fmt.Fprintf(os.Stderr, "  market-maker -turns 10 -seed 42\n")
		fmt.Fprintf(os.Stderr, "  market-maker -turns 0                 # unlimited practice\n")
		fmt.Fprintf(os.Stderr, "  market-maker -cash 50000 -vol \"-1,2\"\n")
	}
	flag.Parse()

	cfg := parseConfigFromFlags()

	g := game.NewGame(cfg)

	fmt.Println("=== Market Maker ===")
	fmt.Printf("Starting cash: $%.2f\n", cfg.StartingCash)
	fmt.Printf("Starting inventory: %.2f\n", cfg.StartingInventory)
	fmt.Printf("Starting price: $%.2f\n", cfg.StartingPrice)
	if cfg.NumTurns > 0 {
		fmt.Printf("Mode: %d turns\n", cfg.NumTurns)
	} else {
		fmt.Println("Mode: unlimited (quit with 'q' or go bankrupt)")
	}
	fmt.Printf("Storage cost: $%.2f / unit / turn\n", cfg.StorageCostPerUnit)
	fmt.Printf("Price move: %.2f%% to %.2f%% per turn\n", cfg.MinPriceMovePct*100, cfg.MaxPriceMovePct*100)
	if cfg.Seed != 0 {
		fmt.Printf("Seed: %d (reproducible)\n", cfg.Seed)
	}
	fmt.Println()

	reader := bufio.NewReader(os.Stdin)

	for !g.IsOver() {
		st := g.State()
		startEquity := types.StartingEquity(cfg)
		currentEquity := st.Equity()
		pnl := currentEquity - startEquity
		pct := 0.0
		if startEquity != 0 {
			pct = (pnl / startEquity) * 100
		}

		turnDisplay := fmt.Sprintf("%d", st.Turn)
		if cfg.NumTurns > 0 {
			turnDisplay = fmt.Sprintf("%d/%d", st.Turn, cfg.NumTurns)
		} else {
			turnDisplay = fmt.Sprintf("%d", st.Turn)
		}

		fmt.Printf("Turn %s | Cash: $%.2f | Inv: %.2f | Price: $%.2f | Equity: $%.2f | P&L: $%.2f (%.2f%%)\n",
			turnDisplay, st.Cash, st.Inventory, st.LastPrice, currentEquity, pnl, pct)

		fmt.Print("Enter bid ask (e.g. 99.5 100.5) or 'q' to quit: ")
		line, err := reader.ReadString('\n')
		if err != nil {
			fmt.Println("Input error:", err)
			break
		}
		line = strings.TrimSpace(line)

		if line == "q" || line == "quit" || line == "exit" {
			fmt.Println("Quitting...")
			break
		}
		if line == "" {
			fmt.Println("Enter two prices separated by space.")
			continue
		}

		parts := strings.Fields(line)
		if len(parts) != 2 {
			fmt.Println("Invalid input. Enter bid and ask separated by space.")
			continue
		}

		bid, err1 := strconv.ParseFloat(parts[0], 64)
		ask, err2 := strconv.ParseFloat(parts[1], 64)
		if err1 != nil || err2 != nil {
			fmt.Println("Invalid numbers. Try again.")
			continue
		}

		result, err := g.SubmitTurn(bid, ask)
		if err != nil {
			fmt.Println("Error:", err)
			continue
		}

		// Print events
		for _, ev := range result.Events {
			fmt.Printf("  %s\n", ev.Message)
		}
		fmt.Printf("  Turn P&L: $%.2f | Units traded: %.2f | Storage: $%.2f\n\n",
			result.Summary.TurnPnL, result.Summary.UnitsTraded, result.Summary.StorageCost)
	}

	// Final summary
	st := g.State()
	finalEquity := st.Equity()
	startEquity := types.StartingEquity(cfg)
	totalPnL := finalEquity - startEquity
	pct := 0.0
	if startEquity != 0 {
		pct = (totalPnL / startEquity) * 100
	}

	stats := g.Stats()

	fmt.Println("=== Game Over ===")
	if st.Reason != "" {
		fmt.Printf("Reason: %s\n", st.Reason)
	}
	fmt.Printf("Final Cash:      $%.2f\n", st.Cash)
	fmt.Printf("Final Inventory: %.2f units\n", st.Inventory)
	fmt.Printf("Final Price:     $%.2f\n", st.LastPrice)
	fmt.Printf("MTM Equity:      $%.2f\n", finalEquity)
	fmt.Printf("Total P&L:       $%.2f (%.2f%%)\n", totalPnL, pct)

	fmt.Println("\n--- Session Stats ---")
	fmt.Printf("Max |Inventory|: %.2f units (peak risk)\n", stats.MaxAbsInventory)
	fmt.Printf("Total units traded: %.2f\n", stats.TotalUnitsTraded)
	fmt.Printf("Total storage paid: $%.2f\n", stats.TotalStoragePaid)
	fmt.Printf("Net spread capture (pre-storage): $%.2f\n", stats.TotalNetFillCash)

	if st.Reason == "bankrupt" {
		fmt.Println("\nYou went bankrupt. Market making is about risk management, not just capturing spread.")
	} else {
		fmt.Println("\nSession ended. Good market makers manage inventory risk as carefully as they manage spreads.")
	}
}

func parseConfigFromFlags() types.GameConfig {
	cfg := game.DefaultConfig()

	cfg.StartingCash = *startingCash
	cfg.StartingInventory = *startingInventory
	cfg.StartingPrice = *startingPrice
	cfg.NumTurns = *numTurns
	cfg.StorageCostPerUnit = *storageCost
	cfg.Seed = *seed

	// Parse vol
	minP, maxP := parseVol(*vol)
	cfg.MinPriceMovePct = minP / 100
	cfg.MaxPriceMovePct = maxP / 100

	return cfg
}

func parseVol(s string) (min, max float64) {
	s = strings.TrimSpace(s)
	parts := strings.Split(s, ",")
	if len(parts) != 2 {
		// fallback to defaults
		return -0.5, 3
	}
	a, err1 := strconv.ParseFloat(strings.TrimSpace(parts[0]), 64)
	b, err2 := strconv.ParseFloat(strings.TrimSpace(parts[1]), 64)
	if err1 != nil || err2 != nil {
		return -0.5, 3
	}
	if a > b {
		a, b = b, a
	}
	return a, b
}
