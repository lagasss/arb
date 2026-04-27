package internal

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// CompetitorCompareLoop runs in a goroutine and continuously classifies new
// competitor_arbs rows by checking whether we (the bot) detected the same
// opportunity. The classification is written to the row's comparison_result
// and comparison_detail columns.
//
// It runs in arbscan (not the bot) so it doesn't compete for resources with
// the hot path. The classification logic is intentionally simple — it joins
// against arb_observations and our_trades on tx_hash + block proximity + pool
// overlap. False positives are preferable to false negatives because we want
// to know "did we even see this?" before drilling into "why didn't we trade?".
//
// settleDelay is how long to wait after a competitor row before classifying
// it (gives our bot time to record its own decision in arb_observations).
// Default 30s.
func CompetitorCompareLoop(ctx interface {
	Done() <-chan struct{}
}, db *DB, settleDelay time.Duration) {
	if settleDelay <= 0 {
		settleDelay = 30 * time.Second
	}
	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			n, err := db.compareNewCompetitorArbs(settleDelay)
			if err != nil {
				log.Printf("[compare] error: %v", err)
				continue
			}
			if n > 0 {
				log.Printf("[compare] classified %d competitor_arbs rows", n)
			}
		}
	}
}

// compareNewCompetitorArbs picks up un-classified rows older than settleDelay
// and classifies each. Returns the number of rows updated.
func (d *DB) compareNewCompetitorArbs(settleDelay time.Duration) (int, error) {
	cutoff := time.Now().Unix() - int64(settleDelay.Seconds())
	rows, err := d.db.Query(`
		SELECT id, tx_hash, block_number, hops_json, dexes, COALESCE(cycle_in_memory, -1)
		FROM competitor_arbs
		WHERE comparison_result = '' AND seen_at <= ?
		ORDER BY id ASC
		LIMIT 200
	`, cutoff)
	if err != nil {
		return 0, err
	}
	defer rows.Close()

	type pending struct {
		id            int64
		txHash        string
		blockNumber   int64
		hopsJSON      string
		dexes         string
		cycleInMemory int
	}
	var work []pending
	for rows.Next() {
		var p pending
		if err := rows.Scan(&p.id, &p.txHash, &p.blockNumber, &p.hopsJSON, &p.dexes, &p.cycleInMemory); err != nil {
			continue
		}
		work = append(work, p)
	}
	rows.Close()

	updated := 0
	for _, p := range work {
		result, detail := d.classifyCompetitor(p.txHash, p.blockNumber, p.hopsJSON, p.dexes, p.cycleInMemory)
		_, err := d.db.Exec(`
			UPDATE competitor_arbs
			SET comparison_result = ?, comparison_detail = ?, compared_at = ?
			WHERE id = ?
		`, result, detail, time.Now().Unix(), p.id)
		if err == nil {
			updated++
		}
	}
	return updated, nil
}

// classifyCompetitor decides whether we detected/executed the same arb.
// Returns (result_code, human_readable_detail_json).
//
// cycleInMemory is the value of competitor_arbs.cycle_in_memory at classify
// time: 1 = our CycleCache contained this exact cycle, 0 = it did not,
// -1 = not yet evaluated by the bot's annotator loop.
//
// The `detail` JSON is intentionally rich so the dashboard modal can show
// "what threshold we applied" and "what would have been required" without a
// second round-trip. Every result includes:
//   - `thresholds`: the active config values that gated this cycle
//     (min_profit_usd, min_sim_profit_bps, max_hops, max_pool_state_block_lag,
//     slippage_bps, gas_safety_mult). Some come from live config; some are
//     the per-observation snapshot (min_profit_usd_at_decision).
//   - `actual`: the observed values (sim_profit_bps, profit_usd, etc.).
//   - `shortfall`: how far the observed values were from the required ones.
//   - `reject_parsed`: structured breakdown of the reject_reason when it
//     matches a known pattern (SIM_PHANTOM, LATENCY_DRIFT, V4_HANDLER,
//     FLASH_BORROW, pre-dryrun regate).
//   - `latency`: per-hop block-lag info when available.
//   - `competitor`: the competitor's own notional/profit/gas for side-by-side.
//   - `pool_status`: per-pool {known, disabled, verified, reason} so the UI
//     can show exactly which pools we were missing or had filtered.
func (d *DB) classifyCompetitor(competitorTxHash string, blockNumber int64, hopsJSON, dexes string, cycleInMemory int) (string, string) {
	poolAddrs, tokenSet, ok := parseHops(hopsJSON)
	if !ok || len(poolAddrs) == 0 {
		return "unknown", `{"reason":"could not parse hops_json"}`
	}

	detail := map[string]interface{}{
		"pools":          poolAddrs,
		"tokens":         sortedKeys(tokenSet),
		"block_number":   blockNumber,
		"cycle_in_cache": cycleInMemory,
		"hop_count":      len(poolAddrs),
	}

	thresholds := d.loadLiveThresholds()
	if len(thresholds) > 0 {
		detail["thresholds"] = thresholds
	}
	if comp := d.loadCompetitorNumbers(competitorTxHash); comp != nil {
		detail["competitor"] = comp
	}
	if routing := d.inferRouting(hopsJSON, dexes); routing != nil {
		detail["routing"] = routing
	}

	ourTrade, err := d.findMatchingOurTrade(blockNumber, poolAddrs)
	if err == nil && ourTrade != nil {
		detail["our_trade_id"] = ourTrade.ID
		detail["our_trade_status"] = ourTrade.Status
		detail["our_trade_tx"] = ourTrade.TxHash
		detail["pool_overlap"] = ourTrade.Overlap
		if ourTrade.Status == "success" {
			return "executed", toJSON(detail)
		}
		detail["revert_reason"] = ourTrade.RevertReason
		detail["reject_parsed"] = parseRejectReason(ourTrade.RevertReason)
		return "submitted_failed", toJSON(detail)
	}

	obs, err := d.findMatchingObservation(blockNumber, poolAddrs)
	if err == nil && obs != nil {
		detail["observation_id"] = obs.ID
		detail["pool_overlap"] = obs.Overlap
		detail["sim_profit_bps"] = obs.SimProfitBps
		detail["profit_usd"] = obs.ProfitUSD
		detail["reject_reason"] = obs.RejectReason
		detail["min_profit_usd_at_decision"] = obs.MinProfitUSDAtDecision
		detail["actual"] = map[string]interface{}{
			"profit_usd":     obs.ProfitUSD,
			"sim_profit_bps": obs.SimProfitBps,
		}
		shortfall := map[string]interface{}{}
		if obs.MinProfitUSDAtDecision > 0 && obs.ProfitUSD < obs.MinProfitUSDAtDecision {
			shortfall["profit_usd_needed"] = obs.MinProfitUSDAtDecision
			shortfall["profit_usd_short_by"] = obs.MinProfitUSDAtDecision - obs.ProfitUSD
		}
		if msb, ok := thresholds["min_sim_profit_bps"].(float64); ok && msb > 0 && float64(obs.SimProfitBps) < msb {
			shortfall["sim_bps_needed"] = msb
			shortfall["sim_bps_short_by"] = msb - float64(obs.SimProfitBps)
		}
		if len(shortfall) > 0 {
			detail["shortfall"] = shortfall
		}
		if parsed := parseRejectReason(obs.RejectReason); parsed != nil {
			detail["reject_parsed"] = parsed
		}
		switch {
		case obs.Executed:
			return "executed", toJSON(detail)
		case strings.Contains(obs.RejectReason, "cooldown"):
			return "detected_cooldown", toJSON(detail)
		case strings.Contains(obs.RejectReason, "buildHops"):
			return "detected_buildhops_reject", toJSON(detail)
		case strings.Contains(obs.RejectReason, "sim-reject"):
			return "detected_sim_reject", toJSON(detail)
		case obs.RejectReason != "":
			return "detected_other_reject", toJSON(detail)
		default:
			return "detected_minprofit", toJSON(detail)
		}
	}

	poolStatuses, _ := d.PoolStatusBatch(poolAddrs)
	detail["pool_status"] = poolStatuses

	missing := 0
	disabled := 0
	unverified := 0
	var firstUnverifiedReason string
	for _, ps := range poolStatuses {
		switch ps.Reason {
		case "unseen":
			missing++
		case "disabled":
			disabled++
		case "unverified":
			unverified++
			if firstUnverifiedReason == "" && ps.VerifyReason != "" {
				firstUnverifiedReason = ps.VerifyReason
			}
		}
	}
	if missing > 0 {
		detail["missing_pool_count"] = missing
		return "missing_pool", toJSON(detail)
	}
	if disabled > 0 {
		detail["disabled_pool_count"] = disabled
		return "cycle_not_cached", toJSON(detail)
	}
	if unverified > 0 {
		detail["unverified_pool_count"] = unverified
		detail["unverified_first_reason"] = firstUnverifiedReason
		return "cycle_not_cached", toJSON(detail)
	}

	switch cycleInMemory {
	case 1:
		if obs == nil {
			detail["note"] = "cycle was in cache but our scorer didn't record an observation — likely the cycle cleared score+LP floor but optimalAmountIn couldn't clear min_profit_usd (no reject_reason emitted for this path today)"
		}
		return "cycle_known_unprofitable", toJSON(detail)
	case 0:
		return "cycle_not_cached", toJSON(detail)
	default:
		return "not_profitable_for_us", toJSON(detail)
	}
}

// loadLiveThresholds reads the bot-relevant config values the classifier
// surfaces in comparison_detail.thresholds. Reads from bot_health's latest
// snapshot of min_profit_usd plus the static config via pragma (we can't
// parse config.yaml from the arbscan process without a new dependency, so
// the live values come from what the bot wrote most recently). Returns an
// empty map if neither source is available.
func (d *DB) loadLiveThresholds() map[string]interface{} {
	out := make(map[string]interface{})
	var minP float64
	row := d.db.QueryRow(`SELECT COALESCE(MAX(min_profit_usd_at_decision),0) FROM arb_observations WHERE observed_at > ?`, time.Now().Unix()-300)
	if err := row.Scan(&minP); err == nil && minP > 0 {
		out["min_profit_usd_recent_max"] = minP
	}
	var avgP float64
	row = d.db.QueryRow(`SELECT COALESCE(AVG(min_profit_usd_at_decision),0) FROM arb_observations WHERE observed_at > ?`, time.Now().Unix()-300)
	if err := row.Scan(&avgP); err == nil && avgP > 0 {
		out["min_profit_usd_recent_avg"] = avgP
	}
	return out
}

// loadCompetitorNumbers fetches the competitor's own profit_usd / net_usd /
// gas_used from competitor_arbs so the modal can show side-by-side "they
// made $X at gas $Y; we would have needed > $Z to clear our floor".
func (d *DB) loadCompetitorNumbers(txHash string) map[string]interface{} {
	row := d.db.QueryRow(`SELECT profit_usd, net_usd, gas_used, hop_count, COALESCE(notional_usd,0), COALESCE(profit_bps,0)
		FROM competitor_arbs WHERE tx_hash = ? LIMIT 1`, txHash)
	var pu, nu, notional, pBps float64
	var gas int64
	var hops int
	if err := row.Scan(&pu, &nu, &gas, &hops, &notional, &pBps); err != nil {
		return nil
	}
	return map[string]interface{}{
		"profit_usd":   pu,
		"net_usd":      nu,
		"gas_used":     gas,
		"hop_count":    hops,
		"notional_usd": notional,
		"profit_bps":   pBps,
	}
}

// parseRejectReason splits a structured reject_reason string into fields
// the modal can render cleanly. Recognised patterns (produced by
// classifyValidateRevert in bot.go):
//
//   - "paused: trading (eth_call REVERTED even at 99%slippage: ...) [TAG: note] h0:DEX[sb_lag=Nb/Xs,tb_lag=Mb/Ys]"
//   - "pre-dryrun regate: edge h0 sb_lag=2b abs > max=1b (latency-drift preempt)"
//   - "cooldown"
//
// Returns nil when nothing interesting is recognised (caller should still
// display the raw string).
func parseRejectReason(s string) map[string]interface{} {
	if s == "" {
		return nil
	}
	out := map[string]interface{}{"raw": s}
	if m := regexp.MustCompile(`\[([A-Z_]+):\s*([^\]]+)\]`).FindStringSubmatch(s); m != nil {
		out["tag"] = m[1]
		out["note"] = strings.TrimSpace(m[2])
	}
	var hops []map[string]interface{}
	for _, m := range regexp.MustCompile(`h(\d+):([A-Za-z0-9]+)\[sb_lag=(-?\d+)b/(-?\d+)s(?:,tb_lag=(-?\d+)b/(-?\d+)s)?\]`).FindAllStringSubmatch(s, -1) {
		hop := map[string]interface{}{
			"hop":      atoi(m[1]),
			"dex":      m[2],
			"sb_lag":   atoi(m[3]),
			"age_sec":  atoi(m[4]),
		}
		if m[5] != "" {
			hop["tb_lag"] = atoi(m[5])
			hop["tb_age_sec"] = atoi(m[6])
		}
		hops = append(hops, hop)
	}
	if len(hops) > 0 {
		out["per_hop_freshness"] = hops
	}
	if m := regexp.MustCompile(`edge h(\d+) sb_lag=(-?\d+)b abs > max=(\d+)b`).FindStringSubmatch(s); m != nil {
		out["tag"] = "LATENCY_REGATE"
		out["note"] = fmt.Sprintf("hop %s had state-block lag %s > max %s", m[1], m[2], m[3])
		out["regate_hop"] = atoi(m[1])
		out["regate_sb_lag"] = atoi(m[2])
		out["regate_max_lag"] = atoi(m[3])
	}
	if m := regexp.MustCompile(`sim_bps=(-?\d+) fresh_bps=(-?\d+)`).FindStringSubmatch(s); m != nil {
		out["sim_bps_at_detection"] = atoi(m[1])
		out["sim_bps_at_ethcall"] = atoi(m[2])
		out["sim_bps_drift"] = atoi(m[1]) - atoi(m[2])
	}
	if m := regexp.MustCompile(`hop=(\d+) dex=([A-Za-z0-9]+) kind=(\w+)`).FindStringSubmatch(s); m != nil {
		out["failing_hop"] = atoi(m[1])
		out["failing_dex"] = m[2]
		out["failing_kind"] = m[3]
	}
	if len(out) == 1 {
		return nil
	}
	return out
}

func atoi(s string) int64 {
	n, _ := strconv.ParseInt(s, 10, 64)
	return n
}

// inferRouting predicts which executor contract our bot WOULD have routed
// this cycle through if we'd detected it, plus the gas budget that routing
// decision carries. Mirrors cycleIsV3MiniShape + qualifyForV3Mini in
// executor.go — keep in lock-step with those rules.
//
// Returns nil if routing can't be inferred (e.g. empty hops). Non-nil result
// always includes: contract_name, contract_address, gas_est_units. Optional
// gas_est_usd is computed from the most recent arb_observations row with a
// non-zero gas_price_gwei + token_price_usd for the start token.
func (d *DB) inferRouting(hopsJSON, dexesCSV string) map[string]interface{} {
	var hops []map[string]interface{}
	if err := json.Unmarshal([]byte(hopsJSON), &hops); err != nil || len(hops) == 0 {
		return nil
	}
	hopCount := len(hops)

	var miniDexes = map[string]bool{
		"UniV3": true, "SushiV3": true, "RamsesV3": true,
		"PancakeV3": true, "CamelotV3": true, "ZyberV3": true,
	}
	var dexes []string
	for _, h := range hops {
		if dx, ok := h["dex"].(string); ok {
			dexes = append(dexes, dx)
		}
	}
	if len(dexes) == 0 && dexesCSV != "" {
		dexes = strings.Split(dexesCSV, ",")
	}

	shapeOK := hopCount >= 2 && hopCount <= 5
	var nonMiniDex string
	for _, dx := range dexes {
		if !miniDexes[strings.TrimSpace(dx)] {
			shapeOK = false
			if nonMiniDex == "" {
				nonMiniDex = strings.TrimSpace(dx)
			}
		}
	}

	var contractName string
	var gasUnits int64
	var reason string
	var miniAddr, fullAddr string
	var miniGas, fullGas int64
	rows, err := d.db.Query(`SELECT address, name, COALESCE(gas_estimate, 0)
		FROM contract_ledger
		WHERE kind = 'executor' AND status = 'active'`)
	if err == nil {
		for rows.Next() {
			var addr, name string
			var gas int64
			if rows.Scan(&addr, &name, &gas) != nil {
				continue
			}
			n := strings.ToLower(name)
			switch {
			case strings.Contains(n, "v3flashmini") || strings.Contains(n, "v3 mini") || strings.Contains(n, "v3mini"):
				if miniAddr == "" {
					miniAddr = addr
					miniGas = gas
				}
			case strings.Contains(n, "arbitrageexecutor") || strings.Contains(n, "arbitrage executor"):
				if fullAddr == "" {
					fullAddr = addr
					fullGas = gas
				}
			}
		}
		rows.Close()
	}
	if miniGas == 0 {
		miniGas = 380_000
	}
	if fullGas == 0 {
		fullGas = 900_000
	}

	switch {
	case shapeOK && miniAddr != "":
		contractName = "V3FlashMini"
		gasUnits = miniGas
		reason = fmt.Sprintf("%d V3-family hops in [2..5], mini-eligible by shape — final routing also requires a V3-pool flash source for the borrow token", hopCount)
		if hopCount > 3 {
			reason += "; note: hop count > 3 may push gas beyond the baseline"
		}
	case !shapeOK && nonMiniDex != "":
		contractName = "ArbitrageExecutor"
		gasUnits = fullGas
		reason = fmt.Sprintf("non-V3-mini hop present (dex=%s) — only ArbitrageExecutor handles this DEX", nonMiniDex)
	case hopCount < 2 || hopCount > 5:
		contractName = "ArbitrageExecutor"
		gasUnits = fullGas
		reason = fmt.Sprintf("hop_count=%d outside mini range [2..5]", hopCount)
	case miniAddr == "" && shapeOK:
		contractName = "ArbitrageExecutor"
		gasUnits = fullGas
		reason = "cycle is mini-shape eligible but V3FlashMini has no active contract in contract_ledger — falls through to full executor"
	default:
		contractName = "ArbitrageExecutor"
		gasUnits = fullGas
		reason = "default route — full ArbitrageExecutor"
	}

	var contractAddr string
	if contractName == "V3FlashMini" {
		contractAddr = miniAddr
	} else {
		contractAddr = fullAddr
	}

	out := map[string]interface{}{
		"contract_name":    contractName,
		"contract_address": contractAddr,
		"gas_est_units":    gasUnits,
		"eligibility":      reason,
		"mini_shape_ok":    shapeOK,
	}
	if !shapeOK && nonMiniDex != "" {
		out["blocking_dex"] = nonMiniDex
	}

	gasPriceGwei, ethPriceUSD := d.recentGasAndEthPrice()
	if gasPriceGwei > 0 {
		out["gas_price_gwei"] = gasPriceGwei
		if ethPriceUSD > 0 {
			gasCostETH := float64(gasUnits) * gasPriceGwei * 1e-9
			out["gas_est_usd"] = gasCostETH * ethPriceUSD
			out["eth_price_usd"] = ethPriceUSD
		}
	}

	return out
}

// recentGasAndEthPrice pulls the most recent non-zero gas_price_gwei from
// arb_observations and a recent ETH price (via token_price_usd where the
// start token is WETH). Returns zeros if either is unavailable.
func (d *DB) recentGasAndEthPrice() (gwei, ethUSD float64) {
	_ = d.db.QueryRow(`SELECT gas_price_gwei FROM arb_observations
		WHERE gas_price_gwei > 0 ORDER BY id DESC LIMIT 1`).Scan(&gwei)
	_ = d.db.QueryRow(`SELECT token_price_usd FROM arb_observations
		WHERE LOWER(COALESCE(tokens,'')) LIKE '%weth%'
		  AND token_price_usd > 500 AND token_price_usd < 20000
		ORDER BY id DESC LIMIT 1`).Scan(&ethUSD)
	return
}

// ── helpers ─────────────────────────────────────────────────────────────────

// parseHops extracts the pool address list (in order) and token set from a
// competitor_arbs.hops_json blob. Returns ok=false if parsing fails.
func parseHops(hopsJSON string) (pools []string, tokens map[string]bool, ok bool) {
	if hopsJSON == "" {
		return nil, nil, false
	}
	var hops []map[string]interface{}
	if err := json.Unmarshal([]byte(hopsJSON), &hops); err != nil {
		return nil, nil, false
	}
	tokens = make(map[string]bool)
	for _, h := range hops {
		if pa, ok := h["pool"].(string); ok && pa != "" {
			pools = append(pools, strings.ToLower(pa))
		}
		if ti, ok := h["token_in"].(string); ok && ti != "" {
			tokens[strings.ToLower(ti)] = true
		}
		if to, ok := h["token_out"].(string); ok && to != "" {
			tokens[strings.ToLower(to)] = true
		}
	}
	return pools, tokens, true
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func toJSON(v interface{}) string {
	b, err := json.Marshal(v)
	if err != nil {
		return ""
	}
	return string(b)
}

// matchedTrade is a row from our_trades that overlaps with the competitor's pools.
type matchedTrade struct {
	ID           int64
	Status       string
	TxHash       string
	RevertReason string
	Overlap      float64 // 0..1
}

// findMatchingOurTrade finds an our_trades row near the given block that uses
// at least 50% of the same pools as the competitor. Returns nil if no match.
func (d *DB) findMatchingOurTrade(blockNumber int64, competitorPools []string) (*matchedTrade, error) {
	rows, err := d.db.Query(`
		SELECT id, status, COALESCE(tx_hash, ''), COALESCE(revert_reason, ''), COALESCE(pools, '')
		FROM our_trades
		WHERE block_number BETWEEN ? AND ?
		ORDER BY id DESC
	`, blockNumber-5, blockNumber+5)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var best *matchedTrade
	competitorSet := make(map[string]bool, len(competitorPools))
	for _, p := range competitorPools {
		competitorSet[p] = true
	}
	for rows.Next() {
		var t matchedTrade
		var poolsStr string
		if err := rows.Scan(&t.ID, &t.Status, &t.TxHash, &t.RevertReason, &poolsStr); err != nil {
			continue
		}
		if poolsStr == "" {
			continue
		}
		ourPools := strings.Split(strings.ToLower(poolsStr), ",")
		matches := 0
		for _, p := range ourPools {
			p = strings.TrimSpace(p)
			if competitorSet[p] {
				matches++
			}
		}
		overlap := 0.0
		denom := len(competitorPools)
		if len(ourPools) > denom {
			denom = len(ourPools)
		}
		if denom > 0 {
			overlap = float64(matches) / float64(denom)
		}
		if overlap >= 0.5 && (best == nil || overlap > best.Overlap) {
			t.Overlap = overlap
			b := t
			best = &b
		}
	}
	return best, nil
}

// matchedObservation is a row from arb_observations that overlaps the competitor's pools.
type matchedObservation struct {
	ID                     int64
	Executed               bool
	RejectReason           string
	SimProfitBps           int64
	ProfitUSD              float64
	Overlap                float64
	MinProfitUSDAtDecision float64
}

// findMatchingObservation looks for an arb_observations row near the same
// block (by observed_at within ±60s) with at least 50% pool overlap.
func (d *DB) findMatchingObservation(blockNumber int64, competitorPools []string) (*matchedObservation, error) {
	// Convert block to a time window. Arbitrum produces ~4 blocks/sec, so
	// ±5 blocks ≈ ±2s. We use a wider observed_at window because the bot's
	// observation may have been logged a few seconds before/after the
	// competitor's tx was mined.
	rows, err := d.db.Query(`
		SELECT id, executed, COALESCE(reject_reason, ''),
		       sim_profit_bps, profit_usd, COALESCE(pools, ''),
		       COALESCE(min_profit_usd_at_decision, 0)
		FROM arb_observations
		WHERE observed_at BETWEEN ? AND ?
		ORDER BY id DESC
		LIMIT 500
	`, time.Now().Unix()-180, time.Now().Unix())
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var best *matchedObservation
	competitorSet := make(map[string]bool, len(competitorPools))
	for _, p := range competitorPools {
		competitorSet[p] = true
	}
	for rows.Next() {
		var o matchedObservation
		var executed int
		var poolsStr string
		if err := rows.Scan(&o.ID, &executed, &o.RejectReason, &o.SimProfitBps, &o.ProfitUSD, &poolsStr, &o.MinProfitUSDAtDecision); err != nil {
			continue
		}
		o.Executed = executed != 0
		if poolsStr == "" {
			continue
		}
		ourPools := strings.Split(strings.ToLower(poolsStr), ",")
		matches := 0
		for _, p := range ourPools {
			p = strings.TrimSpace(p)
			if competitorSet[p] {
				matches++
			}
		}
		denom := len(competitorPools)
		if len(ourPools) > denom {
			denom = len(ourPools)
		}
		overlap := 0.0
		if denom > 0 {
			overlap = float64(matches) / float64(denom)
		}
		if overlap >= 0.5 && (best == nil || overlap > best.Overlap) {
			o.Overlap = overlap
			b := o
			best = &b
		}
	}
	return best, nil
}

// PoolStatus is the per-pool classification returned by PoolStatusBatch.
// Used by the competitor-comparison modal to distinguish "we've never seen
// this pool" from "we saw it but disabled/unverified/pruned it".
type PoolStatus struct {
	Address       string `json:"address"`
	Known         bool   `json:"known"`       // present in pools or v4_pools table
	Disabled      bool   `json:"disabled"`
	Verified      bool   `json:"verified"`
	VerifyReason  string `json:"verify_reason,omitempty"`
	DEX           string `json:"dex,omitempty"`
	TVLUSD        float64 `json:"tvl_usd,omitempty"`
	Volume24hUSD  float64 `json:"volume_24h_usd,omitempty"`
	FeeBps        int    `json:"fee_bps,omitempty"`
	FeePPM        int    `json:"fee_ppm,omitempty"`
	Reason        string `json:"reason"` // one of: unseen | disabled | unverified | quality_pruned | ok
}

// PoolStatusBatch returns a per-pool status entry for every competitorPools
// address, looked up against both `pools` and `v4_pools` tables. An "unseen"
// status means the pool has never been ingested — so we can't even score
// cycles through it. "disabled" means we flagged it. "unverified" means
// verify_pool.go rejected it (and VerifyReason carries the specific cause).
func (d *DB) PoolStatusBatch(competitorPools []string) ([]PoolStatus, error) {
	out := make([]PoolStatus, 0, len(competitorPools))
	if len(competitorPools) == 0 {
		return out, nil
	}
	addrs := make(map[string]bool, len(competitorPools))
	for _, p := range competitorPools {
		addrs[strings.ToLower(p)] = true
	}
	placeholders := make([]string, 0, len(addrs))
	args := make([]interface{}, 0, len(addrs))
	for a := range addrs {
		placeholders = append(placeholders, "?")
		args = append(args, a)
	}
	inClause := strings.Join(placeholders, ",")
	known := make(map[string]PoolStatus, len(addrs))

	q := fmt.Sprintf(`SELECT address, dex, fee_bps, COALESCE(fee_ppm,0),
	       COALESCE(tvl_usd,0), COALESCE(volume_24h_usd,0),
	       COALESCE(disabled,0), COALESCE(verified,0), COALESCE(verify_reason,'')
	  FROM pools WHERE address IN (%s)`, inClause)
	rows, err := d.db.Query(q, args...)
	if err == nil {
		for rows.Next() {
			var ps PoolStatus
			var dis, ver int
			if err := rows.Scan(&ps.Address, &ps.DEX, &ps.FeeBps, &ps.FeePPM, &ps.TVLUSD, &ps.Volume24hUSD, &dis, &ver, &ps.VerifyReason); err == nil {
				ps.Address = strings.ToLower(ps.Address)
				ps.Known = true
				ps.Disabled = dis != 0
				ps.Verified = ver != 0
				known[ps.Address] = ps
			}
		}
		rows.Close()
	}
	q2 := fmt.Sprintf(`SELECT pool_id, 'UniV4', COALESCE(fee_ppm,0), 0,
	        COALESCE(tvl_usd,0), COALESCE(volume_24h_usd,0),
	        0, COALESCE(verified,0), COALESCE(verify_reason,'')
	  FROM v4_pools WHERE pool_id IN (%s)`, inClause)
	rows2, err := d.db.Query(q2, args...)
	if err == nil {
		for rows2.Next() {
			var ps PoolStatus
			var dis, ver int
			var tvl, vol float64
			if err := rows2.Scan(&ps.Address, &ps.DEX, &ps.FeePPM, &ps.FeeBps, &tvl, &vol, &dis, &ver, &ps.VerifyReason); err == nil {
				ps.Address = strings.ToLower(ps.Address)
				ps.TVLUSD = tvl
				ps.Volume24hUSD = vol
				ps.Known = true
				ps.Disabled = dis != 0
				ps.Verified = ver != 0
				if existing, ok := known[ps.Address]; !ok || !existing.Known {
					known[ps.Address] = ps
				}
			}
		}
		rows2.Close()
	}
	for _, p := range competitorPools {
		addr := strings.ToLower(p)
		if ps, ok := known[addr]; ok {
			switch {
			case ps.Disabled:
				ps.Reason = "disabled"
			case !ps.Verified:
				ps.Reason = "unverified"
			default:
				ps.Reason = "ok"
			}
			out = append(out, ps)
		} else {
			out = append(out, PoolStatus{Address: addr, Reason: "unseen"})
		}
	}
	return out, nil
}

// findMissingPools returns the subset of competitorPools that are NOT in our
// pools table. If the result is empty, all pools are known to us.
func (d *DB) findMissingPools(competitorPools []string) ([]string, error) {
	if len(competitorPools) == 0 {
		return nil, nil
	}
	// Build a parameterised IN clause
	placeholders := make([]string, len(competitorPools))
	args := make([]interface{}, len(competitorPools))
	for i, p := range competitorPools {
		placeholders[i] = "?"
		args[i] = strings.ToLower(p)
	}
	query := fmt.Sprintf(`SELECT address FROM pools WHERE address IN (%s)`,
		strings.Join(placeholders, ","))
	rows, err := d.db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	known := make(map[string]bool)
	for rows.Next() {
		var a string
		if err := rows.Scan(&a); err == nil {
			known[strings.ToLower(a)] = true
		}
	}
	// Also check v4_pools
	v4query := fmt.Sprintf(`SELECT pool_id FROM v4_pools WHERE pool_id IN (%s)`,
		strings.Join(placeholders, ","))
	v4rows, err := d.db.Query(v4query, args...)
	if err == nil {
		defer v4rows.Close()
		for v4rows.Next() {
			var a string
			if err := v4rows.Scan(&a); err == nil {
				known[strings.ToLower(a)] = true
			}
		}
	}
	var missing []string
	for _, p := range competitorPools {
		if !known[strings.ToLower(p)] {
			missing = append(missing, p)
		}
	}
	return missing, nil
}

// CompetitorComparison is the response shape for the dashboard's
// /api/competitor-comparison endpoint.
type CompetitorComparison struct {
	ID                int64                  `json:"id"`
	TxHash            string                 `json:"tx_hash"`
	BlockNumber       int64                  `json:"block_number"`
	ComparisonResult  string                 `json:"comparison_result"`
	ComparisonDetail  map[string]interface{} `json:"comparison_detail"`
	ComparedAt        int64                  `json:"compared_at"`
	HopsJSON          string                 `json:"hops_json"`
	PathStr           string                 `json:"path_str"`
	ProfitUSD         float64                `json:"profit_usd"`
	NetUSD            float64                `json:"net_usd"`
}

// LoadCompetitorComparison fetches a single competitor_arbs row with parsed
// comparison detail. Used by the dashboard popup.
func (d *DB) LoadCompetitorComparison(id int64) (*CompetitorComparison, error) {
	row := d.db.QueryRow(`
		SELECT id, tx_hash, block_number, comparison_result, comparison_detail, compared_at,
		       hops_json, path_str, profit_usd, net_usd
		FROM competitor_arbs WHERE id = ?
	`, id)
	var c CompetitorComparison
	var detailRaw string
	err := row.Scan(&c.ID, &c.TxHash, &c.BlockNumber, &c.ComparisonResult,
		&detailRaw, &c.ComparedAt, &c.HopsJSON, &c.PathStr, &c.ProfitUSD, &c.NetUSD)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if detailRaw != "" {
		_ = json.Unmarshal([]byte(detailRaw), &c.ComparisonDetail)
	}
	return &c, nil
}
