package internal

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"math/big"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

// debug_http.go exposes a small read-only HTTP introspection server inside the
// bot process. The dashboard is a *separate* process that only sees SQLite, so
// it can't observe the bot's volatile in-memory state (pool reserves, V3 ticks,
// cycle cache contents, executor caches, health atomics). Tests need that
// state to be authoritative — see project_test_plan.md.
//
// The server listens on 127.0.0.1:<port> only (loopback). Never bind to
// 0.0.0.0 — these endpoints reveal wallet caches and pool inventory and must
// not be reachable from outside the host.
//
// All handlers are GET-only and never mutate bot state. They access shared
// fields under the appropriate locks (Pool.mu, Health.RPCRateLimitMu,
// Executor.vaultBalMu / baseFeeMu).

// startDebugHTTP boots the introspection server in a goroutine. It returns
// after registering handlers; ListenAndServe runs until ctx is cancelled.
// Pass port=0 to disable.
func startDebugHTTP(ctx context.Context, b *Bot, port int) {
	if port == 0 {
		log.Println("[debug-http] disabled (trading.debug_http_port=0)")
		return
	}
	mux := http.NewServeMux()
	h := &debugHandlers{bot: b}

	mux.HandleFunc("/debug/health", h.health)
	mux.HandleFunc("/debug/pools", h.pools)
	mux.HandleFunc("/debug/pools/", h.poolByAddr)
	mux.HandleFunc("/debug/cycles", h.cycles)
	mux.HandleFunc("/debug/cycle-cache/stats", h.cycleCacheStats)
	mux.HandleFunc("/debug/executor", h.executor)
	mux.HandleFunc("/debug/swap-listener", h.swapListener)
	mux.HandleFunc("/debug/config", h.config)
	mux.HandleFunc("/debug/tick-health", h.tickHealth)
	mux.HandleFunc("/debug/hook-registry", h.hookRegistry)
	mux.HandleFunc("/debug/", h.index) // root listing

	addr := fmt.Sprintf("127.0.0.1:%d", port)
	srv := &http.Server{
		Addr:         addr,
		Handler:      mux,
		ReadTimeout:  5 * time.Second,
		WriteTimeout: 30 * time.Second, // /debug/pools may serialize ~1500 entries
	}

	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}()

	go func() {
		log.Printf("[debug-http] listening on %s (loopback only)", addr)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Printf("[debug-http] server error: %v", err)
		}
	}()
}

type debugHandlers struct {
	bot *Bot
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(body)
}

func writeErr(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

// ─── /debug/ — root index ───────────────────────────────────────────────────

func (h *debugHandlers) index(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"endpoints": []string{
			"/debug/health",
			"/debug/pools?dex=&limit=&offset=&token=",
			"/debug/pools/{address}",
			"/debug/cycles?limit=&token=&pool=",
			"/debug/cycle-cache/stats",
			"/debug/executor",
			"/debug/swap-listener",
			"/debug/config",
			"/debug/tick-health",
		},
		"loopback_only": true,
		"read_only":     true,
	})
}

// ─── /debug/tick-health ─────────────────────────────────────────────────────
//
// Single endpoint the cmd/ticktest binary hits to get a full authoritative
// picture of tick-subsystem state: the last FetchTickMaps pass stats, the
// eager-refetch channel drop rate, the two RPCs' observed block heights, and
// per-pool fetch outcomes grouped by reason. Designed so a single GET gives
// the test suite everything it needs to verify invariants end-to-end.

type tickHealthPoolsByReason struct {
	Reason string   `json:"reason"`
	Count  int      `json:"count"`
	Pools  []string `json:"pools,omitempty"`
}

type tickHealthResponse struct {
	Now                   int64  `json:"now"`
	TickDataAt            int64  `json:"tick_data_at"`
	TickDataAgeSecs       int64  `json:"tick_data_age_secs"`
	ArbitrumBlock         uint64 `json:"arbitrum_block"`
	TickRPCBlock          uint64 `json:"tick_rpc_block"`
	RPCSkewBlocks         int64  `json:"rpc_skew_blocks"`   // signed: positive = tick RPC lags arb, negative = tick RPC leads
	TickRPCLagsBy         int64  `json:"tick_rpc_lags_by"`  // unsigned lag; 0 when tick RPC leads
	MaxRPCSkewBlocks      int64  `json:"max_rpc_skew_blocks"`
	RPCSkewOK             bool   `json:"rpc_skew_ok"`
	PassTotal             int64  `json:"pass_total"`
	PassSucceeded         int64  `json:"pass_succeeded"`
	PassEmpty             int64  `json:"pass_empty"`
	PassFailed            int64  `json:"pass_failed"`
	PassSkipped           int64  `json:"pass_skipped"`
	PassRTMismatch        int64  `json:"pass_rt_mismatch"`
	PassDurMs             int64  `json:"pass_dur_ms"`
	PassBlockTick         uint64 `json:"pass_block_tick"`
	PassBlockArb          uint64 `json:"pass_block_arb"`
	EagerDropped          uint64 `json:"eager_refetch_dropped"`
	EagerEnqueued         uint64 `json:"eager_refetch_enqueued"`
	EagerChanLen          int    `json:"eager_refetch_chan_len"`
	EagerChanCap          int    `json:"eager_refetch_chan_cap"`
	CoverageRadius        int    `json:"tick_bitmap_coverage_words"`
	// Per-pool aggregate by TicksFetchReason, limited to cycle pools.
	ByReason []tickHealthPoolsByReason `json:"by_reason"`
	// Per-pool drift statistics: how many cycle pools currently have the
	// current tick outside the fetched word range (would-be coverage rejects).
	CoverageOutOfRange int      `json:"coverage_out_of_range"`
	OutOfRangeSample   []string `json:"out_of_range_sample,omitempty"`
	// Per-pool failure loops: pools whose TicksFetchFailureCount exceeds 1.
	FailureLoopCount  int      `json:"failure_loop_count"`
	FailureLoopSample []string `json:"failure_loop_sample,omitempty"`
	// Pool count whose TicksUpdatedAt age exceeds max_tick_age_sec.
	StaleByAge     int `json:"stale_by_age"`
	MaxTickAgeSecs int64 `json:"max_tick_age_sec"`
	// Gate reject counters since last stats flush (these don't reset here —
	// they reset on the [candstats] log cadence; tick-health samples a
	// snapshot without mutating, unlike the [candstats] path).
	RejectNeverFetched  uint64 `json:"reject_tick_never_fetched"`
	RejectFetchFailed   uint64 `json:"reject_tick_fetch_failed"`
	RejectCoverageDrift uint64 `json:"reject_tick_coverage_drift"`
	RejectEmptyVerified uint64 `json:"reject_tick_empty_verified"`
}

func (h *debugHandlers) hookRegistry(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	gate := globalHookGate
	if gate == nil {
		_ = enc.Encode(map[string]any{"enabled": false})
		return
	}
	snap := gate.Snapshot()
	out := make([]map[string]any, 0, len(snap))
	for _, rep := range snap {
		out = append(out, map[string]any{
			"address":         rep.Address.Hex(),
			"permission_bits": rep.PermissionBits,
			"has_delta_flag":  rep.HasDeltaFlag,
			"classification":  string(rep.Classification),
			"bytecode_hash":   rep.BytecodeHash,
			"reason":          rep.Reason,
		})
	}
	_ = enc.Encode(map[string]any{"enabled": true, "count": len(out), "hooks": out})
}

func (h *debugHandlers) tickHealth(w http.ResponseWriter, r *http.Request) {
	now := time.Now().Unix()
	hh := &h.bot.health
	cfg := h.bot.cfg.Trading

	resp := tickHealthResponse{
		Now:                 now,
		TickDataAt:          hh.TickDataAt.Load(),
		ArbitrumBlock:       hh.LatestBlock.Load(),
		TickRPCBlock:        hh.TickRPCBlock.Load(),
		MaxRPCSkewBlocks:    cfg.TickRPCMaxSkewBlocks,
		PassTotal:           hh.TickFetchPassTotal.Load(),
		PassSucceeded:       hh.TickFetchPassSucceeded.Load(),
		PassEmpty:           hh.TickFetchPassEmpty.Load(),
		PassFailed:          hh.TickFetchPassFailed.Load(),
		PassSkipped:         hh.TickFetchPassSkipped.Load(),
		PassRTMismatch:      hh.TickFetchPassRTMismatch.Load(),
		PassDurMs:           hh.TickFetchPassDurMs.Load(),
		PassBlockTick:       hh.TickFetchPassBlockTick.Load(),
		PassBlockArb:        hh.TickFetchPassBlockArb.Load(),
		EagerDropped:        hh.TickFetchEagerDropped.Load(),
		EagerEnqueued:       hh.TickFetchEagerEnqueued.Load(),
		CoverageRadius:      cfg.TickBitmapCoverageWords,
		MaxTickAgeSecs:      cfg.MaxTickAgeSec,
		RejectNeverFetched:  h.bot.candRejectTickNeverFetched.Load(),
		RejectFetchFailed:   h.bot.candRejectTickFetchFailed.Load(),
		RejectCoverageDrift: h.bot.candRejectTickCoverageDrift.Load(),
		RejectEmptyVerified: h.bot.candRejectTickEmptyVerified.Load(),
	}
	if resp.TickDataAt > 0 {
		resp.TickDataAgeSecs = now - resp.TickDataAt
	}
	if resp.ArbitrumBlock > 0 && resp.TickRPCBlock > 0 {
		if resp.ArbitrumBlock >= resp.TickRPCBlock {
			resp.RPCSkewBlocks = int64(resp.ArbitrumBlock - resp.TickRPCBlock)
			resp.TickRPCLagsBy = resp.RPCSkewBlocks
		} else {
			resp.RPCSkewBlocks = -int64(resp.TickRPCBlock - resp.ArbitrumBlock)
		}
	}
	resp.RPCSkewOK = cfg.TickRPCMaxSkewBlocks <= 0 || resp.TickRPCLagsBy <= cfg.TickRPCMaxSkewBlocks
	if h.bot.swapListener != nil && h.bot.swapListener.TickRefetchCh != nil {
		resp.EagerChanLen = len(h.bot.swapListener.TickRefetchCh)
		resp.EagerChanCap = cap(h.bot.swapListener.TickRefetchCh)
	}

	// Compute per-reason breakdown + drift + failure loops over cycle pools.
	cycleV3 := make(map[string]bool)
	if h.bot.cycleCache != nil {
		for _, cc := range h.bot.cycleCache.All() {
			for _, e := range cc.Cycle.Edges {
				switch e.Pool.DEX {
				case DEXUniswapV3, DEXSushiSwapV3, DEXRamsesV3, DEXPancakeV3, DEXCamelotV3, DEXZyberV3, DEXUniswapV4:
					cycleV3[strings.ToLower(e.Pool.Address)] = true
				}
			}
		}
	}
	byReason := make(map[string][]string)
	var oor, failLoop, staleAge int
	var oorSample, failSample []string
	for _, p := range h.bot.registry.All() {
		addr := strings.ToLower(p.Address)
		if !cycleV3[addr] {
			continue
		}
		p.mu.RLock()
		reason := p.TicksFetchReason
		if reason == "" && p.TicksFetchOK && len(p.Ticks) > 0 {
			reason = "ok"
		} else if reason == "" && !p.TicksFetchOK {
			reason = "never"
		}
		byReason[reason] = append(byReason[reason], addr)
		radius := p.TicksWordRadius
		spacing := p.TickSpacing
		tickCur := p.Tick
		tickAt := p.TickAtFetch
		dex := p.DEX
		failCount := p.TicksFetchFailureCount
		uAt := p.TicksUpdatedAt
		ticksBlock := p.TicksBlock
		p.mu.RUnlock()
		if radius > 0 && spacing > 0 && ticksBlock > 0 {
			var curWord, centerWord int16
			if dex == DEXCamelotV3 || dex == DEXZyberV3 {
				curWord = int16(tickCur >> 8)
				centerWord = int16(tickAt >> 8)
			} else {
				curWord = int16((tickCur / spacing) >> 8)
				centerWord = int16((tickAt / spacing) >> 8)
			}
			delta := curWord - centerWord
			if delta < 0 {
				delta = -delta
			}
			if delta > radius {
				oor++
				if len(oorSample) < 25 {
					oorSample = append(oorSample, addr)
				}
			}
		}
		if failCount > 1 {
			failLoop++
			if len(failSample) < 25 {
				failSample = append(failSample, addr)
			}
		}
		if cfg.MaxTickAgeSec > 0 && !uAt.IsZero() && now-uAt.Unix() > cfg.MaxTickAgeSec {
			staleAge++
		}
	}
	for reason, pools := range byReason {
		entry := tickHealthPoolsByReason{Reason: reason, Count: len(pools)}
		if len(pools) <= 25 {
			entry.Pools = pools
		} else {
			entry.Pools = pools[:25]
		}
		resp.ByReason = append(resp.ByReason, entry)
	}
	sort.Slice(resp.ByReason, func(i, j int) bool { return resp.ByReason[i].Reason < resp.ByReason[j].Reason })
	resp.CoverageOutOfRange = oor
	resp.OutOfRangeSample = oorSample
	resp.FailureLoopCount = failLoop
	resp.FailureLoopSample = failSample
	resp.StaleByAge = staleAge
	writeJSON(w, http.StatusOK, resp)
}

// ─── /debug/health ──────────────────────────────────────────────────────────

type debugHealthResponse struct {
	Now              int64            `json:"now"`
	TickDataAt       int64            `json:"tick_data_at"`
	TickDataAgeSecs  int64            `json:"tick_data_age_secs"`
	MulticallAt      int64            `json:"multicall_at"`
	MulticallAgeSecs int64            `json:"multicall_age_secs"`
	SwapEventAt      int64            `json:"swap_event_at"`
	SwapEventAgeSecs int64            `json:"swap_event_age_secs"`
	CycleRebuildAt   int64            `json:"cycle_rebuild_at"`
	CycleRebuildAgeSecs int64         `json:"cycle_rebuild_age_secs"`
	TickedPoolsHave  int64            `json:"ticked_pools_have"`
	TickedPoolsTotal int64            `json:"ticked_pools_total"`
	LatestBlock      uint64           `json:"latest_block"`
	RPCRateLimit     map[string]int64 `json:"rpc_rate_limit"`
	RPCRateLimitLastAt map[string]int64 `json:"rpc_rate_limit_last_at"`
	MinProfitUSD     float64          `json:"min_profit_usd"`
	LastGasPriceGwei float64          `json:"last_gas_price_gwei"`
	Thresholds       map[string]int64 `json:"thresholds"`
	// Startup readiness gate. Submissions are blocked until ReadyToTrade=true.
	// ReadyReason names the first unmet condition; empty when ready.
	ReadyToTrade     bool             `json:"ready_to_trade"`
	ReadyReason      string           `json:"ready_reason"`
	Readiness        map[string]bool  `json:"readiness"`
}

func (h *debugHandlers) health(w http.ResponseWriter, r *http.Request) {
	now := time.Now().Unix()
	hh := &h.bot.health
	resp := debugHealthResponse{
		Now:                 now,
		TickDataAt:          hh.TickDataAt.Load(),
		MulticallAt:         hh.MulticallAt.Load(),
		SwapEventAt:         hh.SwapEventAt.Load(),
		CycleRebuildAt:      hh.CycleRebuildAt.Load(),
		TickedPoolsHave:     hh.TickedPoolsHave.Load(),
		TickedPoolsTotal:    hh.TickedPoolsTotal.Load(),
		LatestBlock:         hh.LatestBlock.Load(),
		Thresholds: map[string]int64{
			"max_tick_age_sec":      h.bot.cfg.Trading.MaxTickAgeSec,
			"max_multicall_age_sec": h.bot.cfg.Trading.MaxMulticallAgeSec,
			"max_swap_age_sec":      h.bot.cfg.Trading.MaxSwapAgeSec,
		},
	}
	if resp.TickDataAt > 0 {
		resp.TickDataAgeSecs = now - resp.TickDataAt
	}
	if resp.MulticallAt > 0 {
		resp.MulticallAgeSecs = now - resp.MulticallAt
	}
	if resp.SwapEventAt > 0 {
		resp.SwapEventAgeSecs = now - resp.SwapEventAt
	}
	if resp.CycleRebuildAt > 0 {
		resp.CycleRebuildAgeSecs = now - resp.CycleRebuildAt
	}
	// Snapshot the RPC rate-limit maps under their dedicated mutex.
	hh.RPCRateLimitMu.Lock()
	resp.RPCRateLimit = make(map[string]int64, len(hh.RPCRateLimit))
	resp.RPCRateLimitLastAt = make(map[string]int64, len(hh.RPCRateLimitLastAt))
	for k, v := range hh.RPCRateLimit {
		resp.RPCRateLimit[k] = v
	}
	for k, v := range hh.RPCRateLimitLastAt {
		resp.RPCRateLimitLastAt[k] = v
	}
	hh.RPCRateLimitMu.Unlock()
	if v := h.bot.minProfitUSD.Load(); v != nil {
		if f, ok := v.(float64); ok {
			resp.MinProfitUSD = f
		}
	}
	if v := h.bot.lastGasPriceGwei.Load(); v != nil {
		if f, ok := v.(float64); ok {
			resp.LastGasPriceGwei = f
		}
	}
	ready, reason := h.bot.readyOrReason()
	resp.ReadyToTrade = ready
	resp.ReadyReason = reason
	resp.Readiness = map[string]bool{
		"multicall":     h.bot.ready.MulticallDone.Load(),
		"tick_maps":     h.bot.ready.TickMapsDone.Load(),
		"borrowable":    h.bot.ready.BorrowableDone.Load(),
		"verifier":      h.bot.ready.VerifierDone.Load(),
		"cycle_rebuild": h.bot.ready.CycleRebuildDone.Load(),
		"executor_warm": h.bot.ready.ExecutorWarmDone.Load(),
	}
	writeJSON(w, http.StatusOK, resp)
}

// ─── /debug/pools ───────────────────────────────────────────────────────────

type debugPool struct {
	Address        string  `json:"address"`
	DEX            string  `json:"dex"`
	Token0         string  `json:"token0"`
	Token0Symbol   string  `json:"token0_symbol"`
	Token0Decimals uint8   `json:"token0_decimals"`
	Token1         string  `json:"token1"`
	Token1Symbol   string  `json:"token1_symbol"`
	Token1Decimals uint8   `json:"token1_decimals"`
	FeeBps         uint32  `json:"fee_bps"`
	FeePPM         uint32  `json:"fee_ppm"`
	Reserve0       string  `json:"reserve0"`
	Reserve1       string  `json:"reserve1"`
	SqrtPriceX96   string  `json:"sqrt_price_x96"`
	Liquidity      string  `json:"liquidity"`
	Tick           int32   `json:"tick"`
	TickSpacing    int32   `json:"tick_spacing"`
	TicksCount     int     `json:"ticks_count"`
	TicksUpdatedAt int64   `json:"ticks_updated_at"`
	TicksBlock     uint64  `json:"ticks_block"`
	TicksWordRadius int16  `json:"ticks_word_radius"`
	TickAtFetch    int32   `json:"tick_at_fetch"`
	TicksFetchOK            bool   `json:"ticks_fetch_ok"`
	TicksFetchReason        string `json:"ticks_fetch_reason,omitempty"`
	TicksFetchAttemptedAt   int64  `json:"ticks_fetch_attempted_at"`
	TicksFetchBitmapWords   int16  `json:"ticks_fetch_bitmap_words"`
	TicksFetchNonEmptyWords int16  `json:"ticks_fetch_non_empty_words"`
	TicksFetchFailureCount  uint32 `json:"ticks_fetch_failure_count"`
	TickDeltaApplied        uint32 `json:"tick_delta_applied"`
	V4Hooks        string  `json:"v4_hooks,omitempty"`
	IsStable       bool    `json:"is_stable,omitempty"`
	AmpFactor      uint64  `json:"amp_factor,omitempty"`
	PoolID         string  `json:"pool_id,omitempty"`
	Weight0        float64 `json:"weight0,omitempty"`
	Weight1        float64 `json:"weight1,omitempty"`
	Token0FeeBps   uint32  `json:"token0_fee_bps,omitempty"`
	Token1FeeBps   uint32  `json:"token1_fee_bps,omitempty"`
	TVLUSD         float64 `json:"tvl_usd"`
	Volume24hUSD   float64 `json:"volume_24h_usd"`
	SpotPrice      float64 `json:"spot_price"`
	LastUpdated    int64   `json:"last_updated"`
	StateBlock     uint64  `json:"state_block"`
	Disabled       bool    `json:"disabled"`
	Pinned         bool    `json:"pinned"`
	Verified       bool    `json:"verified"`
	VerifyReason   string  `json:"verify_reason,omitempty"`
	// SimRejectsInWindow is the current sliding-window count used by the
	// auto-disable mechanism. Once this crosses auto_disable_reject_threshold
	// the pool is flagged Disabled=true and persisted. Useful for watching
	// pools that are trending toward auto-disable without tripping yet.
	SimRejectsInWindow int32 `json:"sim_rejects_in_window,omitempty"`
}

func snapshotPool(p *Pool) debugPool {
	p.mu.RLock()
	defer p.mu.RUnlock()
	out := debugPool{
		Address:        p.Address,
		DEX:            p.DEX.String(),
		FeeBps:         p.FeeBps,
		FeePPM:         p.FeePPM,
		Tick:           p.Tick,
		TickSpacing:    p.TickSpacing,
		TicksCount:     len(p.Ticks),
		V4Hooks:        p.V4Hooks,
		IsStable:       p.IsStable,
		AmpFactor:      p.AmpFactor,
		PoolID:         p.PoolID,
		Weight0:        p.Weight0,
		Weight1:        p.Weight1,
		Token0FeeBps:   p.Token0FeeBps,
		Token1FeeBps:   p.Token1FeeBps,
		TVLUSD:         p.TVLUSD,
		Volume24hUSD:   p.Volume24hUSD,
		SpotPrice:      p.SpotPrice,
		Disabled:           p.Disabled,
		Pinned:             p.Pinned,
		Verified:           p.Verified,
		VerifyReason:       p.VerifyReason,
		SimRejectsInWindow: p.SimRejectsInWindow(),
	}
	if p.Token0 != nil {
		out.Token0 = p.Token0.Address
		out.Token0Symbol = p.Token0.Symbol
		out.Token0Decimals = p.Token0.Decimals
	}
	if p.Token1 != nil {
		out.Token1 = p.Token1.Address
		out.Token1Symbol = p.Token1.Symbol
		out.Token1Decimals = p.Token1.Decimals
	}
	if p.Reserve0 != nil {
		out.Reserve0 = p.Reserve0.String()
	}
	if p.Reserve1 != nil {
		out.Reserve1 = p.Reserve1.String()
	}
	if p.SqrtPriceX96 != nil {
		out.SqrtPriceX96 = p.SqrtPriceX96.String()
	}
	if p.Liquidity != nil {
		out.Liquidity = p.Liquidity.String()
	}
	if !p.TicksUpdatedAt.IsZero() {
		out.TicksUpdatedAt = p.TicksUpdatedAt.Unix()
	}
	out.TicksBlock = p.TicksBlock
	out.TicksWordRadius = p.TicksWordRadius
	out.TickAtFetch = p.TickAtFetch
	out.TicksFetchOK = p.TicksFetchOK
	out.TicksFetchReason = p.TicksFetchReason
	if !p.TicksFetchAttemptedAt.IsZero() {
		out.TicksFetchAttemptedAt = p.TicksFetchAttemptedAt.Unix()
	}
	out.TicksFetchBitmapWords = p.TicksFetchBitmapWords
	out.TicksFetchNonEmptyWords = p.TicksFetchNonEmptyWords
	out.TicksFetchFailureCount = p.TicksFetchFailureCount
	out.TickDeltaApplied = p.TickDeltaApplied
	if !p.LastUpdated.IsZero() {
		out.LastUpdated = p.LastUpdated.Unix()
	}
	out.StateBlock = p.StateBlock
	return out
}

func (h *debugHandlers) pools(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	dexFilter := q.Get("dex")
	tokenFilter := strings.ToLower(q.Get("token"))
	limit, _ := strconv.Atoi(q.Get("limit"))
	offset, _ := strconv.Atoi(q.Get("offset"))

	all := h.bot.registry.All()
	// Filter
	filtered := make([]*Pool, 0, len(all))
	for _, p := range all {
		if dexFilter != "" && p.DEX.String() != dexFilter {
			continue
		}
		if tokenFilter != "" {
			if !strings.EqualFold(p.Token0.Address, tokenFilter) &&
				!strings.EqualFold(p.Token1.Address, tokenFilter) {
				continue
			}
		}
		filtered = append(filtered, p)
	}
	// Stable order: by address
	sort.Slice(filtered, func(i, j int) bool {
		return strings.ToLower(filtered[i].Address) < strings.ToLower(filtered[j].Address)
	})
	total := len(filtered)
	// Pagination
	if offset > total {
		offset = total
	}
	filtered = filtered[offset:]
	if limit > 0 && limit < len(filtered) {
		filtered = filtered[:limit]
	}
	out := make([]debugPool, len(filtered))
	for i, p := range filtered {
		out[i] = snapshotPool(p)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"total":  total,
		"offset": offset,
		"count":  len(out),
		"pools":  out,
	})
}

func (h *debugHandlers) poolByAddr(w http.ResponseWriter, r *http.Request) {
	addr := strings.TrimPrefix(r.URL.Path, "/debug/pools/")
	if addr == "" {
		writeErr(w, http.StatusBadRequest, "missing address")
		return
	}
	p, ok := h.bot.registry.Get(addr)
	if !ok {
		writeErr(w, http.StatusNotFound, "pool not found in registry")
		return
	}
	// Single-pool path includes the full initialized-tick array. Downstream
	// tools (smoketest, ad-hoc sim reproductions) need every (tick,
	// liquidityNet) pair to run the multi-tick sim accurately — exposing
	// only ticks_count here means they reconstruct V3 pools with Ticks=nil
	// and the sim silently returns 0. See cmd/smoketest for the caller
	// that motivated this. Not added to the bulk /debug/pools list because
	// ticks can be 1000+ entries per pool and that would blow up the
	// payload on a registry-wide fetch.
	snap := snapshotPool(p)
	p.mu.RLock()
	ticks := make([]debugTick, len(p.Ticks))
	for i, t := range p.Ticks {
		ticks[i] = debugTick{
			Tick:         t.Tick,
			LiquidityNet: "",
		}
		if t.LiquidityNet != nil {
			ticks[i].LiquidityNet = t.LiquidityNet.String()
		}
	}
	p.mu.RUnlock()
	writeJSON(w, http.StatusOK, struct {
		debugPool
		Ticks []debugTick `json:"ticks"`
	}{snap, ticks})
}

// debugTick serialises a single TickData entry for the per-pool endpoint.
type debugTick struct {
	Tick         int32  `json:"tick"`
	LiquidityNet string `json:"liquidity_net"` // decimal string (int128 range)
}

// ─── /debug/cycles ──────────────────────────────────────────────────────────

type debugCycleEdge struct {
	Pool     string `json:"pool"`
	DEX      string `json:"dex"`
	TokenIn  string `json:"token_in"`
	TokenOut string `json:"token_out"`
	FeeBps   uint32 `json:"fee_bps"`
}

type debugCycle struct {
	Hops      int              `json:"hops"`
	StartToken string          `json:"start_token"`
	LogProfit float64          `json:"log_profit"`
	Path      string           `json:"path"`
	Edges     []debugCycleEdge `json:"edges"`
	PoolAddrs []string         `json:"pool_addrs"`
}

func snapshotCycle(cc cachedCycle) debugCycle {
	out := debugCycle{
		Hops:      len(cc.Cycle.Edges),
		LogProfit: cc.logProfitAt,
		Path:      cc.Cycle.Path(),
		PoolAddrs: cc.PoolAddrs,
		Edges:     make([]debugCycleEdge, len(cc.Cycle.Edges)),
	}
	if len(cc.Cycle.Edges) > 0 && cc.Cycle.Edges[0].TokenIn != nil {
		out.StartToken = cc.Cycle.Edges[0].TokenIn.Symbol
	}
	for i, e := range cc.Cycle.Edges {
		out.Edges[i] = debugCycleEdge{
			Pool:     e.Pool.Address,
			DEX:      e.Pool.DEX.String(),
			TokenIn:  e.TokenIn.Symbol,
			TokenOut: e.TokenOut.Symbol,
			FeeBps:   e.Pool.FeeBps,
		}
	}
	return out
}

func (h *debugHandlers) cycles(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	limit, _ := strconv.Atoi(q.Get("limit"))
	if limit <= 0 {
		limit = 50
	}
	tokenFilter := strings.ToLower(q.Get("token"))
	poolFilter := strings.ToLower(q.Get("pool"))
	// hops filter: exact match on edge count. 0 or empty means "all".
	hopsFilter, _ := strconv.Atoi(q.Get("hops"))
	// Per-position token filters: token1..token5 pin the token at each
	// position in the cycle. Position 1 is the start token (TokenIn of edge 0),
	// position N>=2 is the TokenOut of edge (N-2). Compose with AND semantics —
	// a cycle must match every specified position. Cycles shorter than the
	// highest-specified position are excluded.
	var tokenPos [6]string // index 1..5 used
	tokenPosSet := false
	for i := 1; i <= 5; i++ {
		tokenPos[i] = strings.ToLower(q.Get(fmt.Sprintf("token%d", i)))
		if tokenPos[i] != "" {
			tokenPosSet = true
		}
	}

	cc := h.bot.cycleCache
	if cc == nil {
		writeErr(w, http.StatusServiceUnavailable, "cycle cache not initialized")
		return
	}
	var sample []cachedCycle
	if poolFilter != "" {
		sample = cc.CyclesForPool(poolFilter)
	} else {
		sample = cc.All()
	}
	total := len(sample)

	// token filter (after collection so we can paginate consistently)
	if tokenFilter != "" {
		filtered := make([]cachedCycle, 0, len(sample))
		for _, c := range sample {
			for _, e := range c.Cycle.Edges {
				if strings.EqualFold(e.TokenIn.Address, tokenFilter) ||
					strings.EqualFold(e.TokenOut.Address, tokenFilter) {
					filtered = append(filtered, c)
					break
				}
			}
		}
		sample = filtered
	}
	// hops filter: apply after token so it composes correctly with it.
	if hopsFilter > 0 {
		filtered := make([]cachedCycle, 0, len(sample))
		for _, c := range sample {
			if len(c.Cycle.Edges) == hopsFilter {
				filtered = append(filtered, c)
			}
		}
		sample = filtered
	}
	// Position filters: exact token-at-position match.
	if tokenPosSet {
		filtered := make([]cachedCycle, 0, len(sample))
		for _, c := range sample {
			edges := c.Cycle.Edges
			if len(edges) == 0 {
				continue
			}
			ok := true
			for pos := 1; pos <= 5; pos++ {
				want := tokenPos[pos]
				if want == "" {
					continue
				}
				var got string
				if pos == 1 {
					got = strings.ToLower(edges[0].TokenIn.Address)
				} else {
					idx := pos - 2
					if idx >= len(edges) {
						ok = false
						break
					}
					got = strings.ToLower(edges[idx].TokenOut.Address)
				}
				if got != want {
					ok = false
					break
				}
			}
			if ok {
				filtered = append(filtered, c)
			}
		}
		sample = filtered
	}
	matched := len(sample)
	if limit < len(sample) {
		sample = sample[:limit]
	}
	out := make([]debugCycle, len(sample))
	for i, c := range sample {
		out[i] = snapshotCycle(c)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"total_in_cache": total,
		"matched":        matched,
		"returned":       len(out),
		"cycles":         out,
	})
}

func (h *debugHandlers) cycleCacheStats(w http.ResponseWriter, r *http.Request) {
	cc := h.bot.cycleCache
	if cc == nil {
		writeErr(w, http.StatusServiceUnavailable, "cycle cache not initialized")
		return
	}
	all := cc.All()
	// Histogram: cycles per pool
	perPool := make(map[string]int)
	for _, c := range all {
		for _, addr := range c.PoolAddrs {
			perPool[addr]++
		}
	}
	// Top 20 pools by cycle count
	type poolBucket struct {
		Pool  string `json:"pool"`
		Count int    `json:"count"`
	}
	buckets := make([]poolBucket, 0, len(perPool))
	for p, c := range perPool {
		buckets = append(buckets, poolBucket{Pool: p, Count: c})
	}
	sort.Slice(buckets, func(i, j int) bool { return buckets[i].Count > buckets[j].Count })
	if len(buckets) > 20 {
		buckets = buckets[:20]
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"total_cycles":      len(all),
		"pools_covered":     len(perPool),
		"last_rebuild_at":   cc.lastBuild.Unix(),
		"last_rebuild_count": cc.lastCount,
		"top_pools":         buckets,
	})
}

// ─── /debug/executor ────────────────────────────────────────────────────────

type debugVaultBal struct {
	Token   string `json:"token"`
	Balance string `json:"balance"`
}

func (h *debugHandlers) executor(w http.ResponseWriter, r *http.Request) {
	e := h.bot.executor
	if e == nil {
		writeErr(w, http.StatusServiceUnavailable, "executor not initialized")
		return
	}
	resp := map[string]any{
		"contract_address": e.contractAddr.Hex(),
		"wallet_address":   e.address.Hex(),
		"chain_id":         e.chainID.String(),
		"balancer_vault":   e.balancerVault.Hex(),
	}
	// Nonce cache
	if e.nonceLoaded.Load() {
		resp["nonce"] = e.nonceVal.Load()
	} else {
		resp["nonce"] = nil
	}
	// BaseFee cache
	e.baseFeeMu.RLock()
	if e.baseFeeCache != nil {
		resp["base_fee_wei"] = e.baseFeeCache.String()
	}
	e.baseFeeMu.RUnlock()
	// Vault balance cache
	e.vaultBalMu.RLock()
	bals := make([]debugVaultBal, 0, len(e.vaultBalCache))
	for tok, bal := range e.vaultBalCache {
		s := "0"
		if bal != nil {
			s = bal.String()
		}
		bals = append(bals, debugVaultBal{Token: tok.Hex(), Balance: s})
	}
	resp["vault_bal_fetched_at"] = e.vaultBalFetchedAt.Unix()
	e.vaultBalMu.RUnlock()
	sort.Slice(bals, func(i, j int) bool { return bals[i].Token < bals[j].Token })
	resp["vault_balances"] = bals
	writeJSON(w, http.StatusOK, resp)
}

// ─── /debug/swap-listener ───────────────────────────────────────────────────

func (h *debugHandlers) swapListener(w http.ResponseWriter, r *http.Request) {
	sl := h.bot.swapListener
	if sl == nil {
		writeErr(w, http.StatusServiceUnavailable, "swap listener not initialized")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"swaps_total":     atomic.LoadUint64(&sl.swapsTotal),
		"max_logs_depth":  sl.maxLogsDepth.Load(),
		"logs_chan_cap":   swapLogsChanCap,
		"pools_subscribed": len(sl.registry.All()),
	})
}

// ─── /debug/config ──────────────────────────────────────────────────────────

func (h *debugHandlers) config(w http.ResponseWriter, r *http.Request) {
	// Surface only the non-sensitive subsections that affect runtime behavior.
	// Never expose private keys, RPC URLs (which contain API keys), or wallet
	// info beyond the public address.
	writeJSON(w, http.StatusOK, map[string]any{
		"strategy": h.bot.cfg.Strategy,
		"trading": map[string]any{
			"executor_contract":             h.bot.cfg.Trading.ExecutorContract,
			"balancer_vault":                h.bot.cfg.Trading.BalancerVault,
			"min_profit_usd":                h.bot.cfg.Trading.MinProfitUSD,
			"flash_loan_amount_usd":         h.bot.cfg.Trading.FlashLoanAmountUSD,
			"max_tick_age_sec":              h.bot.cfg.Trading.MaxTickAgeSec,
			"max_multicall_age_sec":         h.bot.cfg.Trading.MaxMulticallAgeSec,
			"max_swap_age_sec":              h.bot.cfg.Trading.MaxSwapAgeSec,
			"max_pool_state_block_lag":      h.bot.cfg.Trading.MaxPoolStateBlockLag,
			"tick_bitmap_coverage_words":    h.bot.cfg.Trading.TickBitmapCoverageWords,
			"latest_block":                  h.bot.health.LatestBlock.Load(),
			"executor_supports_pancake_v3":  h.bot.cfg.Trading.ExecutorSupportsPancakeV3,
			"debug_http_port":               h.bot.cfg.Trading.DebugHTTPPort,
		},
	})
}

// ─── helper: convert *big.Int safely ────────────────────────────────────────

// bigStr is a defensive String() that never panics on nil.
// Currently unused — kept for future endpoint additions that need it.
var _ = func(b *big.Int) string {
	if b == nil {
		return ""
	}
	return b.String()
}
