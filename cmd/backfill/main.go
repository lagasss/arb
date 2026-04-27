// backfill: one-shot tool to populate hop_prices_json and token_price_usd for
// existing rows in competitor_arbs and arb_observations.
//
// competitor_arbs: derives prices from hops_json amounts using stablecoin-seeded
// propagation — exact same algorithm as the live arbscan scanner.
//
// arb_observations: no hop amounts are stored, so only stablecoin tokens can be
// priced accurately ($1.0); all other tokens are left at 0.
package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"strconv"
	"strings"

	_ "modernc.org/sqlite"
)

// stableAddrs mirrors the set in arbscan — Arbitrum stablecoin addresses.
var stableAddrs = map[string]bool{
	"0xff970a61a04b1ca14834a43f5de4533ebddb5cc8": true, // USDC.e
	"0xaf88d065e77c8cc2239327c5edb3a432268e5831": true, // USDC
	"0xfd086bc7cd5c481dcc9c85ebe478a1c0b69fcbb9": true, // USDT
	"0xda10009cbd5d07dd0cecc66161fc93d7c9000da1": true, // DAI
	"0x17fc002b466eec40dae837fc4be5c67993ddbd6f": true, // FRAX
}

// stableSymbols maps lower-case symbol → true.
var stableSymbols = map[string]bool{
	"usdc": true, "usdc.e": true, "usdt": true, "dai": true, "frax": true,
}

type swapHop struct {
	TokenIn   string `json:"token_in"`
	AmountIn  string `json:"amount_in_human"`
	TokenOut  string `json:"token_out"`
	AmountOut string `json:"amount_out_human"`
}

type priceEntry struct {
	seenAt int64
	prices map[string]float64
}

// buildHopPrices propagates stablecoin seed prices through hop exchange rates.
func buildHopPrices(hops []swapHop) map[string]float64 {
	prices := make(map[string]float64)
	for addr := range stableAddrs {
		prices[addr] = 1.0
	}
	for changed := true; changed; {
		changed = false
		for _, h := range hops {
			in := strings.ToLower(h.TokenIn)
			out := strings.ToLower(h.TokenOut)
			amtIn, err1 := strconv.ParseFloat(h.AmountIn, 64)
			amtOut, err2 := strconv.ParseFloat(h.AmountOut, 64)
			if err1 != nil || err2 != nil || amtIn <= 0 || amtOut <= 0 {
				continue
			}
			if pIn, ok := prices[in]; ok {
				if _, known := prices[out]; !known {
					prices[out] = pIn * amtIn / amtOut
					changed = true
				}
			} else if pOut, ok := prices[out]; ok {
				if _, known := prices[in]; !known {
					prices[in] = pOut * amtOut / amtIn
					changed = true
				}
			}
		}
	}
	return prices
}

func marshalPrices(prices map[string]float64) string {
	b, err := json.Marshal(prices)
	if err != nil {
		return "{}"
	}
	return string(b)
}

func main() {
	dbPath := "arb.db"
	if len(os.Args) > 1 {
		dbPath = os.Args[1]
	}

	db, err := sql.Open("sqlite", dbPath+"?_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)")
	if err != nil {
		log.Fatalf("open db: %v", err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)

	// Ensure new columns exist — ignored if already present.
	db.Exec(`ALTER TABLE competitor_arbs  ADD COLUMN token_price_usd  REAL NOT NULL DEFAULT 0`)
	db.Exec(`ALTER TABLE competitor_arbs  ADD COLUMN hop_prices_json  TEXT NOT NULL DEFAULT '{}'`)
	db.Exec(`ALTER TABLE arb_observations ADD COLUMN token_price_usd  REAL NOT NULL DEFAULT 0`)
	db.Exec(`ALTER TABLE arb_observations ADD COLUMN hop_prices_json  TEXT NOT NULL DEFAULT '{}'`)
	db.Exec(`ALTER TABLE our_trades       ADD COLUMN token_price_usd  REAL NOT NULL DEFAULT 0`)
	db.Exec(`ALTER TABLE our_trades       ADD COLUMN hop_prices_json  TEXT NOT NULL DEFAULT '{}'`)

	// ── competitor_arbs ───────────────────────────────────────────────────────

	rows, err := db.Query(`SELECT rowid, hops_json, borrow_token FROM competitor_arbs WHERE hop_prices_json='{}'`)
	if err != nil {
		log.Fatalf("query competitor_arbs: %v", err)
	}

	type compRow struct {
		id          int64
		hopsJSON    string
		borrowToken string
	}
	var compRows []compRow
	for rows.Next() {
		var r compRow
		if err := rows.Scan(&r.id, &r.hopsJSON, &r.borrowToken); err == nil {
			compRows = append(compRows, r)
		}
	}
	rows.Close()

	compUpdated := 0
	compSkipped := 0
	for _, r := range compRows {
		var hops []swapHop
		if err := json.Unmarshal([]byte(r.hopsJSON), &hops); err != nil || len(hops) == 0 {
			compSkipped++
			continue
		}
		prices := buildHopPrices(hops)
		hopJSON := marshalPrices(prices)
		tokenPrice := prices[strings.ToLower(r.borrowToken)]

		_, err := db.Exec(
			`UPDATE competitor_arbs SET hop_prices_json=?, token_price_usd=? WHERE rowid=?`,
			hopJSON, tokenPrice, r.id,
		)
		if err != nil {
			log.Printf("update competitor_arbs rowid=%d: %v", r.id, err)
			continue
		}
		compUpdated++
	}
	fmt.Printf("competitor_arbs: updated %d rows, skipped %d (no/invalid hops)\n", compUpdated, compSkipped)

	// ── arb_observations ─────────────────────────────────────────────────────
	// arb_observations stores token SYMBOLS (not addresses) and no hop amounts.
	// Strategy:
	//   1. Build symbol→address map from the tokens table.
	//   2. Build a time-bucketed price cache from competitor_arbs (already backfilled).
	//   3. For each observation, convert its token symbols to addresses, then find
	//      the nearest competitor_arbs rows within ±30 min and extract prices.

	// Step 1: symbol→[]address from tokens table (lower-case both).
	symToAddrs := make(map[string][]string)
	trows, err := db.Query(`SELECT address, symbol FROM tokens`)
	if err != nil {
		log.Fatalf("query tokens: %v", err)
	}
	for trows.Next() {
		var addr, sym string
		if trows.Scan(&addr, &sym) == nil {
			s := strings.ToLower(strings.TrimSpace(sym))
			symToAddrs[s] = append(symToAddrs[s], strings.ToLower(addr))
		}
	}
	trows.Close()
	// Seed stablecoins by address directly (already known).
	for addr := range stableAddrs {
		symToAddrs[addr] = []string{addr} // addr→addr passthrough for lookup below
	}

	// Step 2: load all competitor_arbs prices with timestamps.
	// priceCache: seen_at (unix) → map[addr]float64
	compPriceRows, err := db.Query(`SELECT seen_at, hop_prices_json FROM competitor_arbs WHERE hop_prices_json != '{}'`)
	if err != nil {
		log.Fatalf("query competitor_arbs prices: %v", err)
	}
	var priceCache []priceEntry
	for compPriceRows.Next() {
		var seenAt int64
		var hopJSON string
		if compPriceRows.Scan(&seenAt, &hopJSON) != nil {
			continue
		}
		var prices map[string]float64
		if json.Unmarshal([]byte(hopJSON), &prices) != nil {
			continue
		}
		priceCache = append(priceCache, priceEntry{seenAt, prices})
	}
	compPriceRows.Close()
	fmt.Printf("loaded %d competitor_arbs price snapshots for reference\n", len(priceCache))

	// Step 3: for each observation, find nearest prices within ±30 min.
	const windowSecs = 30 * 60

	// Process all rows — re-running is safe (UPDATE is idempotent).
	// Rows previously set to stablecoin-only will be enriched with WETH/ARB etc.
	backfillSymbolTable(db, priceCache, symToAddrs,
		"arb_observations", "observed_at")
	backfillSymbolTable(db, priceCache, symToAddrs,
		"our_trades", "submitted_at")

	// ── recompute competitor_arbs profit_usd using multi-token net flows ─────
	// Old code picked max-positive single token; correct is Σ(net_flow × price).
	recomputeCompetitorProfits(db)
}

// recomputeCompetitorProfits fixes profit_usd / net_usd for every competitor_arbs row.
// It derives net token flows from hops_json and prices them via hop_prices_json.
// Gas cost is preserved: new_net_usd = new_profit_usd - (old_profit_usd - old_net_usd).
func recomputeCompetitorProfits(db *sql.DB) {
	rows, err := db.Query(`SELECT rowid, hops_json, hop_prices_json, profit_usd, net_usd FROM competitor_arbs`)
	if err != nil {
		log.Fatalf("query competitor_arbs profits: %v", err)
	}
	type row struct {
		id            int64
		hopsJSON      string
		hopPricesJSON string
		oldProfitUSD  float64
		oldNetUSD     float64
	}
	var all []row
	for rows.Next() {
		var r row
		if rows.Scan(&r.id, &r.hopsJSON, &r.hopPricesJSON, &r.oldProfitUSD, &r.oldNetUSD) == nil {
			all = append(all, r)
		}
	}
	rows.Close()

	updated := 0
	skipped := 0
	for _, r := range all {
		var hops []swapHop
		if err := json.Unmarshal([]byte(r.hopsJSON), &hops); err != nil || len(hops) == 0 {
			skipped++
			continue
		}
		var prices map[string]float64
		if err := json.Unmarshal([]byte(r.hopPricesJSON), &prices); err != nil || len(prices) == 0 {
			skipped++
			continue
		}

		// Compute net token flow from hop amounts: for each hop,
		// tokenOut gains amountOut, tokenIn loses amountIn.
		netFlow := make(map[string]float64)
		for _, h := range hops {
			in := strings.ToLower(h.TokenIn)
			out := strings.ToLower(h.TokenOut)
			amtIn, err1 := strconv.ParseFloat(h.AmountIn, 64)
			amtOut, err2 := strconv.ParseFloat(h.AmountOut, 64)
			if err1 != nil || err2 != nil {
				continue
			}
			netFlow[out] += amtOut
			netFlow[in] -= amtIn
		}

		// Convert each token's net flow to USD and sum.
		newProfitUSD := 0.0
		for tok, flow := range netFlow {
			price, ok := prices[tok]
			if !ok || price <= 0 {
				continue
			}
			newProfitUSD += flow * price
		}

		// Preserve existing gas cost: gas_cost = old_profit_usd - old_net_usd
		gasCostUSD := r.oldProfitUSD - r.oldNetUSD
		newNetUSD := newProfitUSD - gasCostUSD

		_, err := db.Exec(
			`UPDATE competitor_arbs SET profit_usd=?, net_usd=? WHERE rowid=?`,
			newProfitUSD, newNetUSD, r.id,
		)
		if err != nil {
			log.Printf("update competitor_arbs rowid=%d: %v", r.id, err)
			continue
		}
		updated++
	}
	fmt.Printf("competitor_arbs profit recompute: updated %d rows, skipped %d (missing hops/prices)\n", updated, skipped)
}

// backfillSymbolTable enriches hop_prices_json and token_price_usd for a table that
// stores token symbols (not addresses) and has no hop amounts. Prices are derived
// by matching tokens against nearby competitor_arbs snapshots within ±30 min.
func backfillSymbolTable(db *sql.DB, priceCache []priceEntry, symToAddrs map[string][]string, table, tsCol string) {
	const windowSecs = 30 * 60

	q := fmt.Sprintf(`SELECT rowid, %s, tokens FROM %s`, tsCol, table)
	rows, err := db.Query(q)
	if err != nil {
		log.Fatalf("query %s: %v", table, err)
	}
	type row struct {
		id        int64
		timestamp int64
		tokens    string
	}
	var all []row
	for rows.Next() {
		var r row
		if rows.Scan(&r.id, &r.timestamp, &r.tokens) == nil {
			all = append(all, r)
		}
	}
	rows.Close()

	updated := 0
	partial := 0
	for _, r := range all {
		syms := strings.Split(r.tokens, ",")

		// Collect unique addresses for every token in the path.
		addrSet := make(map[string]string) // addr → symbol
		for _, sym := range syms {
			s := strings.ToLower(strings.TrimSpace(sym))
			if addrs, ok := symToAddrs[s]; ok {
				for _, a := range addrs {
					addrSet[a] = sym
				}
			}
			if stableSymbols[s] {
				addrSet[s] = sym
			}
		}

		// Seed stablecoins, then merge prices from nearby competitor snapshots.
		merged := make(map[string]float64)
		for addr := range stableAddrs {
			if _, need := addrSet[addr]; need {
				merged[addr] = 1.0
			}
		}
		for _, entry := range priceCache {
			if entry.seenAt < r.timestamp-windowSecs || entry.seenAt > r.timestamp+windowSecs {
				continue
			}
			for addr := range addrSet {
				if p, ok := entry.prices[addr]; ok && p > 0 {
					if existing, has := merged[addr]; has {
						merged[addr] = (existing + p) / 2
					} else {
						merged[addr] = p
					}
				}
			}
		}

		if len(merged) == 0 {
			continue
		}

		hopJSON := marshalPrices(merged)

		// token_price_usd = USD price of the first (start) token.
		startPrice := 0.0
		if len(syms) > 0 {
			s := strings.ToLower(strings.TrimSpace(syms[0]))
			if addrs, ok := symToAddrs[s]; ok {
				for _, a := range addrs {
					if p, ok := merged[a]; ok && p > 0 {
						startPrice = p
						break
					}
				}
			}
			if startPrice == 0 && stableSymbols[s] {
				startPrice = 1.0
			}
		}

		_, err := db.Exec(
			fmt.Sprintf(`UPDATE %s SET hop_prices_json=?, token_price_usd=? WHERE rowid=?`, table),
			hopJSON, startPrice, r.id,
		)
		if err != nil {
			log.Printf("update %s rowid=%d: %v", table, r.id, err)
			continue
		}
		if len(merged) < len(addrSet) {
			partial++
		}
		updated++
	}
	fmt.Printf("%s: updated %d rows (%d fully priced, %d partial)\n",
		table, updated, updated-partial, partial)
}