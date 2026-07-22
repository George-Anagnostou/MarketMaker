// Command mmg runs the same fixed-point exchange kernel as the web server.
package main

import (
	"bufio"
	"flag"
	"fmt"
	"math"
	"os"
	"strconv"
	"strings"

	"market-maker/internal/exchange"
	"market-maker/internal/fixed"
)

var (
	startingCash      = flag.String("cash", "100000", "starting cash")
	startingInventory = flag.String("inventory", "0", "starting inventory")
	startingPrice     = flag.String("price", "100", "starting reference price")
	numTurns          = flag.Int("turns", 10, "number of turns (0 = unlimited)")
	storageCost       = flag.String("storage", "1", "storage cost per unit per turn")
	seed              = flag.Uint64("seed", 42, "non-zero deterministic scenario seed")
	vol               = flag.String("vol", "-0.5,3", `price movement as "min,max" percent`)
)

func main() {
	flag.Parse()
	cfg, err := configFromFlags()
	if err != nil {
		fmt.Fprintln(os.Stderr, "invalid configuration:", err)
		os.Exit(2)
	}
	engine, err := exchange.New(cfg)
	if err != nil {
		fmt.Fprintln(os.Stderr, "cannot create exchange:", err)
		os.Exit(2)
	}

	fmt.Println("=== Market Maker Exchange ===")
	fmt.Printf("Scenario seed: %d | margin: %.2f%% initial / %.2f%% maintenance\n", cfg.Seed, float64(cfg.InitialMarginBps)/100, float64(cfg.MaintenanceMarginBps)/100)
	printState(engine.State())
	reader := bufio.NewReader(os.Stdin)
	for !engine.State().IsOver {
		fmt.Print("Quote bid ask, or q to quit: ")
		line, err := reader.ReadString('\n')
		if err != nil {
			fmt.Fprintln(os.Stderr, "input ended; session remains replayable only in the server mode")
			return
		}
		line = strings.TrimSpace(line)
		if line == "q" || line == "quit" || line == "exit" {
			result := engine.Quit()
			printResult(result)
			break
		}
		parts := strings.Fields(line)
		if len(parts) != 2 {
			fmt.Println("Enter exactly two decimal prices.")
			continue
		}
		bid, bidErr := fixed.ParsePrice(parts[0])
		ask, askErr := fixed.ParsePrice(parts[1])
		if bidErr != nil || askErr != nil {
			fmt.Println("Quotes must be finite decimal prices with at most four decimal places.")
			continue
		}
		result, err := engine.Execute(exchange.Command{ID: fmt.Sprintf("cli-turn-%d", engine.State().Version+1), Type: "submit_quote", Bid: bid, Ask: ask})
		if err != nil {
			fmt.Println("Rejected:", err)
			continue
		}
		printResult(result)
	}
}

func configFromFlags() (exchange.Config, error) {
	cash, err := fixed.ParseMoney(*startingCash)
	if err != nil {
		return exchange.Config{}, err
	}
	position, err := fixed.ParseQty(*startingInventory)
	if err != nil {
		return exchange.Config{}, err
	}
	mark, err := fixed.ParsePrice(*startingPrice)
	if err != nil {
		return exchange.Config{}, err
	}
	storage, err := fixed.ParsePrice(*storageCost)
	if err != nil {
		return exchange.Config{}, err
	}
	minMove, maxMove, err := parseVol(*vol)
	if err != nil {
		return exchange.Config{}, err
	}
	if *seed == 0 {
		return exchange.Config{}, fmt.Errorf("seed must be non-zero; use an explicit seed for replay")
	}
	cfg := exchange.Config{Instrument: "SIM", StartingCash: cash, StartingPosition: position, StartingMark: mark, StoragePerUnit: storage, NumTurns: *numTurns, InitialMarginBps: 5000, MaintenanceMarginBps: 2500, MaxPosition: fixed.Qty(10_000_000), MaxOrdersPerTurn: 5, MaxOrderQty: fixed.Qty(100_000), MaxFlowSlippageBps: 200, MinMoveBps: minMove, MaxMoveBps: maxMove, Seed: *seed}
	return cfg, cfg.Validate()
}

func parseVol(input string) (int64, int64, error) {
	parts := strings.Split(input, ",")
	if len(parts) != 2 {
		return 0, 0, fmt.Errorf("volatility must be min,max")
	}
	min, err := strconv.ParseFloat(strings.TrimSpace(parts[0]), 64)
	if err != nil || math.IsNaN(min) || math.IsInf(min, 0) {
		return 0, 0, fmt.Errorf("invalid minimum movement")
	}
	max, err := strconv.ParseFloat(strings.TrimSpace(parts[1]), 64)
	if err != nil || math.IsNaN(max) || math.IsInf(max, 0) {
		return 0, 0, fmt.Errorf("invalid maximum movement")
	}
	return int64(math.Round(min * 100)), int64(math.Round(max * 100)), nil
}

func printResult(result exchange.Result) {
	for _, event := range result.Events {
		switch event.Type {
		case "trade":
			fmt.Printf("Trade: %s %s @ %s\n", event.Trade.Quantity, event.Trade.Price, event.Trade.BuyerID+" buys")
		case "storage_charged":
			fmt.Printf("Storage charged: $%s\n", event.Amount)
		case "mark_updated":
			fmt.Printf("Reference mark: $%s\n", event.Mark)
		case "game_ended":
			fmt.Println("Game ended:", event.Reason)
		}
	}
	fmt.Printf("Turn fill cash: $%s | storage: $%s | turn P&L: $%s\n", result.Summary.NetFillCash, result.Summary.StorageCost, result.Summary.TurnPnL)
	printState(result.State)
}

func printState(state exchange.State) {
	fmt.Printf("Turn %d | Cash $%s | Position %s | Mark $%s | Equity $%s | Book %s / %s\n", state.Turn, state.Cash, state.Position, state.Mark, state.Equity, state.BestBid, state.BestAsk)
}
