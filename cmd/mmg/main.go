// Command mmg runs the same fixed-point exchange kernel as the web server.
package main

import (
	"bufio"
	"flag"
	"fmt"
	"io"
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
	informedFlowBps   = flag.Int64("informed-flow-bps", 0, "informed customer flow probability in basis points after a mark move (0..10000)")
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
	printState(os.Stdout, engine.State())
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
			result, err := engine.Quit()
			if err != nil {
				fmt.Fprintln(os.Stderr, "quit rejected:", err)
				return
			}
			printResult(os.Stdout, result)
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
		result, err := engine.Execute(exchange.Command{ID: fmt.Sprintf("cli-turn-%d", engine.State().Version+1), Type: exchange.CommandSubmitQuote, Bid: bid, Ask: ask})
		if err != nil {
			fmt.Println("Rejected:", err)
			continue
		}
		printResult(os.Stdout, result)
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
	cfg := exchange.Config{Instrument: "SIM", StartingCash: cash, StartingPosition: position, StartingMark: mark, StoragePerUnit: storage, NumTurns: *numTurns, InitialMarginBps: 5000, MaintenanceMarginBps: 2500, MaxPosition: fixed.Qty(10_000_000), MaxOrdersPerTurn: 5, MaxOrderQty: fixed.Qty(100_000), MaxFlowSlippageBps: 200, MinMoveBps: minMove, MaxMoveBps: maxMove, Seed: *seed, SimulationVersion: exchange.SimulationVersionAdverseSelection, InformedFlowBps: *informedFlowBps}
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

func printResult(w io.Writer, result exchange.Result) {
	for _, event := range result.Events {
		switch event.Type {
		case "trade":
			if event.Trade == nil {
				continue
			}
			customer := "a customer"
			if event.Trade.Informed {
				customer = "an informed customer"
			}
			switch {
			case event.Trade.BuyerID == exchange.PlayerAccount:
				fmt.Fprintf(w, "Trade: you bought %s @ $%s from %s\n", event.Trade.Quantity, event.Trade.Price, customer)
			case event.Trade.SellerID == exchange.PlayerAccount:
				fmt.Fprintf(w, "Trade: you sold %s @ $%s to %s\n", event.Trade.Quantity, event.Trade.Price, customer)
			}
		case "storage_charged":
			fmt.Fprintf(w, "Storage charged: $%s\n", event.Amount)
		case "mark_updated":
			fmt.Fprintf(w, "Reference mark: $%s\n", event.Mark)
		case "game_ended":
			fmt.Fprintln(w, "Game ended:", event.Reason)
		}
	}
	if attribution := result.Summary.PnLAttribution; attribution != nil {
		fmt.Fprintln(w, "Turn P&L attribution:")
		fmt.Fprintf(w, "  Execution edge: %s\n", signedMoney(attribution.ExecutionEdge))
		fmt.Fprintf(w, "  Inventory mark: %s\n", signedMoney(attribution.InventoryMarkPnL))
		fmt.Fprintf(w, "  Storage: %s\n", signedMoney(attribution.StoragePnL))
		fmt.Fprintf(w, "  Total: %s\n", signedMoney(result.Summary.TurnPnL))
	} else {
		fmt.Fprintf(w, "Turn fill cash: $%s | storage: $%s | turn P&L: $%s\n", result.Summary.NetFillCash, result.Summary.StorageCost, result.Summary.TurnPnL)
	}
	if result.Summary.InformedOrders > 0 || result.Summary.InformedOrdersFilled > 0 || result.Summary.InformedUnitsTraded != 0 || result.Summary.InformedFlowPnL != 0 {
		fmt.Fprintf(w, "Informed flow: %d arrived | %d filled | %s units | P&L %s\n", result.Summary.InformedOrders, result.Summary.InformedOrdersFilled, result.Summary.InformedUnitsTraded, signedMoney(result.Summary.InformedFlowPnL))
	}
	printState(w, result.State)
}

func signedMoney(amount fixed.Money) string {
	value := amount.String()
	if strings.HasPrefix(value, "-") {
		return "-$" + strings.TrimPrefix(value, "-")
	}
	if amount > 0 {
		return "+$" + value
	}
	return "$" + value
}

func printState(w io.Writer, state exchange.State) {
	fmt.Fprintf(w, "Turn %d | Cash $%s | Position %s | Mark $%s | Equity $%s | Book %s / %s\n", state.Turn, state.Cash, state.Position, state.Mark, state.Equity, state.BestBid, state.BestAsk)
}
