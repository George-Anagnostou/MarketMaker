package main

import (
	"bufio"
	"flag"
	"fmt"
	"math"
	"os"
	"strconv"
	"strings"

	"market-maker/internal/game"
	"market-maker/internal/types"
)

// ANSI colors (pure stdlib, no deps)
const (
	reset  = "\033[0m"
	red    = "\033[31m"
	green  = "\033[32m"
	yellow = "\033[33m"
	cyan   = "\033[36m"
	bold   = "\033[1m"
)

// formatMoney formats a float as USD with American thousands separators.
// Example: 1234567.89 -> "$1,234,567.89"
func formatMoney(f float64) string {
	neg := f < 0
	if neg {
		f = -f
	}
	s := fmt.Sprintf("%.2f", f)
	parts := strings.Split(s, ".")
	ints := parts[0]
	dec := parts[1]

	var b strings.Builder
	n := len(ints)
	for i, c := range ints {
		if i > 0 && (n-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteRune(c)
	}
	res := "$" + b.String() + "." + dec
	if neg {
		res = "-" + res
	}
	return res
}

// formatNum is a lighter formatter for non-money quantities.
func formatNum(f float64, decimals int) string {
	if decimals < 0 {
		decimals = 2
	}
	return fmt.Sprintf("%."+strconv.Itoa(decimals)+"f", f)
}

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

	fmt.Println(bold + "=== Market Maker ===" + reset)
	fmt.Printf("Starting cash: %s\n", formatMoney(cfg.StartingCash))
	fmt.Printf("Starting inventory: %s units\n", formatNum(cfg.StartingInventory, 2))
	fmt.Printf("Starting price: %s\n", formatMoney(cfg.StartingPrice))
	if cfg.NumTurns > 0 {
		fmt.Printf("Mode: %d turns\n", cfg.NumTurns)
	} else {
		fmt.Println("Mode: " + yellow + "unlimited" + reset + " (quit with 'q' or go bankrupt)")
	}
	fmt.Printf("Storage cost: %s / unit / turn\n", formatMoney(cfg.StorageCostPerUnit))
	fmt.Printf("Price move: %.2f%% to %.2f%% per turn\n", cfg.MinPriceMovePct*100, cfg.MaxPriceMovePct*100)
	if cfg.Seed != 0 {
		fmt.Printf("Seed: %d (reproducible)\n", cfg.Seed)
	}
	fmt.Println()

	reader := bufio.NewReader(os.Stdin)
	lastBid, lastAsk := 0.0, 0.0
	prevMid := cfg.StartingPrice

	for !g.IsOver() {
		st := g.State()
		startEquity := types.StartingEquity(cfg)
		currentEquity := st.Equity()
		pnl := currentEquity - startEquity
		pct := 0.0
		if startEquity != 0 {
			pct = (pnl / startEquity) * 100
		}

		pnlColor := green
		if pnl < 0 {
			pnlColor = red
		}

		turnDisplay := fmt.Sprintf("%d", st.Turn)
		if cfg.NumTurns > 0 {
			turnDisplay = fmt.Sprintf("%d/%d", st.Turn, cfg.NumTurns)
		}

		// Price move since last turn (very important for MM to see drift vs their position)
		priceDelta := st.LastPrice - prevMid
		pricePct := 0.0
		if prevMid != 0 {
			pricePct = (priceDelta / prevMid) * 100
		}
		priceMoveColor := green
		if priceDelta < 0 {
			priceMoveColor = red
		}
		priceMoveStr := fmt.Sprintf("%s%+.2f (%+.2f%%)%s", priceMoveColor, priceDelta, pricePct, reset)

		// Richer status line
		fmt.Printf("%sTurn %s%s | Cash: %s | Inv: %s | Mid: %s (Δ %s) | Equity: %s | P&L: %s%s (%+.2f%%)%s\n",
			bold, turnDisplay, reset,
			formatMoney(st.Cash),
			formatNum(st.Inventory, 2),
			formatMoney(st.LastPrice),
			priceMoveStr,
			formatMoney(currentEquity),
			pnlColor, formatMoney(pnl), pct, reset,
		)

		// Market maker context - things a good MM watches every turn
		inv := st.Inventory
		absInv := math.Abs(inv)
		posDir := "FLAT"
		posColor := reset
		if inv > 0.001 {
			posDir = "LONG"
			posColor = green
		} else if inv < -0.001 {
			posDir = "SHORT"
			posColor = red
		}

		// Rough MTM sensitivity: how much your equity moves for a $1 move in the mid.
		exposure := absInv // $1 move → this much P&L

		fmt.Printf("  Position: %s%s %.2f units%s | $1 mid move ≈ %s%s MTM%s\n",
			posColor, posDir, inv, reset,
			posColor, formatMoney(exposure), reset)

		if absInv > 0.001 {
			if inv > 0 {
				fmt.Printf("  %sRisk note:%s You are LONG. Price down = MTM pain. Price up = MTM gain.\n", yellow, reset)
			} else {
				fmt.Printf("  %sRisk note:%s You are SHORT. Price up = MTM pain. Price down = MTM gain.\n", yellow, reset)
			}
		} else {
			fmt.Printf("  %sRisk note:%s Flat position. No inventory risk this turn.\n", cyan, reset)
		}

		// Helpful context for the market maker
		if lastBid > 0 && lastAsk > 0 {
			spread := lastAsk - lastBid
			fmt.Printf("  Last quote: Bid %s / Ask %s  (width %s)\n",
				formatMoney(lastBid), formatMoney(lastAsk), formatMoney(spread))
		}
		fmt.Printf("  %sHint:%s Tighter spread = more flow, more inventory risk. Wider = safer but less volume.\n", cyan, reset)

		fmt.Print("Enter bid ask (e.g. 99.50 100.50) or 'q' to quit: ")
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

		// Remember for display
		lastBid, lastAsk = bid, ask
		spreadW := ask - bid

		result, err := g.SubmitTurn(bid, ask)
		if err != nil {
			fmt.Println("Error:", err)
			continue
		}

		// Print events with some visual grouping
		fmt.Println("  " + cyan + "--- Turn events ---" + reset)
		for _, ev := range result.Events {
			msg := ev.Message
			// Light coloring for important events
			if ev.Type == "game_over" {
				msg = red + msg + reset
			} else if ev.Type == "fill" {
				// fills are already quite descriptive
			} else if ev.Type == "price_update" {
				msg = msg // could color green/red based on move, but keep simple
			}
			fmt.Printf("  %s\n", msg)
		}

		// Clearer trade summary - this is gold for learning
		bs := result.Summary
		fmt.Println("  " + cyan + "--- Trade summary ---" + reset)
		fmt.Printf("  Your quote this turn: Bid %s / Ask %s   width=%s\n",
			formatMoney(bid), formatMoney(ask), formatMoney(spreadW))

		if bs.BuyVolume > 0 || bs.SellVolume > 0 {
			fmt.Printf("  Flow hit you at:\n")
			if bs.BuyVolume > 0 {
				fmt.Printf("    • MARKET BUY  %.2f units @ your ASK %s  → you SOLD (buyers were aggressive)\n",
					bs.BuyVolume, formatMoney(ask))
			}
			if bs.SellVolume > 0 {
				fmt.Printf("    • MARKET SELL %.2f units @ your BID %s  → you BOUGHT (sellers were aggressive)\n",
					bs.SellVolume, formatMoney(bid))
			}
			// Net inventory impact from this turn's flow (positive = you got longer)
			netInvFromFlow := bs.SellVolume - bs.BuyVolume
			fmt.Printf("  Net inventory from flow this turn: %+.2f units\n", netInvFromFlow)
		} else {
			fmt.Println("  Flow: No orders hit your quotes this turn.")
		}

		turnPnlColor := green
		if bs.TurnPnL < 0 {
			turnPnlColor = red
		}
		fmt.Printf("  Net fill cash: %s | Storage: %s | %sTurn P&L: %s%s\n\n",
			formatMoney(bs.NetFillCash),
			formatMoney(bs.StorageCost),
			turnPnlColor, formatMoney(bs.TurnPnL), reset,
		)
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

	finalPnlColor := green
	if totalPnL < 0 {
		finalPnlColor = red
	}

	fmt.Println(bold + "=== Game Over ===" + reset)
	if st.Reason != "" {
		reason := st.Reason
		if reason == "bankrupt" {
			reason = red + "BANKRUPT" + reset
		}
		fmt.Printf("Reason: %s\n", reason)
	}
	fmt.Printf("Final Cash:      %s\n", formatMoney(st.Cash))
	fmt.Printf("Final Inventory: %s units\n", formatNum(st.Inventory, 2))
	fmt.Printf("Final Mid Price: %s\n", formatMoney(st.LastPrice))
	fmt.Printf("MTM Equity:      %s\n", formatMoney(finalEquity))
	fmt.Printf("Total P&L:       %s%s (%+.2f%%)%s\n", finalPnlColor, formatMoney(totalPnL), pct, reset)

	fmt.Println()
	fmt.Println(bold + "--- Session Stats ---" + reset)
	fmt.Printf("Max |Inventory| (peak risk): %s units\n", formatNum(stats.MaxAbsInventory, 2))
	fmt.Printf("Total units traded:          %s\n", formatNum(stats.TotalUnitsTraded, 2))
	fmt.Printf("Total storage paid:          %s\n", formatMoney(stats.TotalStoragePaid))
	fmt.Printf("Net spread capture (pre-storage): %s\n", formatMoney(stats.TotalNetFillCash))

	if st.Reason == "bankrupt" {
		fmt.Println("\n" + red + "You went bankrupt." + reset + " Market making is risk management first, spread capture second.")
	} else {
		fmt.Println("\nSession ended. Review your inventory discipline and how one-sided flow affected your position.")
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

	minP, maxP := parseVol(*vol)
	cfg.MinPriceMovePct = minP / 100
	cfg.MaxPriceMovePct = maxP / 100

	return cfg
}

func parseVol(s string) (min, max float64) {
	s = strings.TrimSpace(s)
	parts := strings.Split(s, ",")
	if len(parts) != 2 {
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
