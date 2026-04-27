package internal

// ArbLogger captures profitable cycle detections to SQLite on a background
// goroutine so the hot path (fastEvalCycles) is never blocked by I/O.
//
// Flow:
//   fastEvalCycles → Record() → buffered channel → goroutine → batch INSERT
//
// Dedup: the same cycle key is suppressed for dedupWindow to avoid flooding
// the table when one cycle stays profitable for minutes.
//
// Pruning: runs every pruneInterval, deletes rows older than RetentionHours
// and trims the table to MaxRows (newest rows kept).

import (
	"context"
	"log"
	"math"
	"strings"
	"sync"
	"time"
)

const (
	arbLogChanSize  = 2000
	dedupWindow     = 30 * time.Second
	pruneInterval   = 1 * time.Hour
	batchFlushEvery = 5 * time.Second
)

// ArbLoggerConfig controls retention and size limits.
type ArbLoggerConfig struct {
	RetentionHours int // default 72
	MaxRows        int // default 500_000 (0 = unlimited)
}

// ArbLogger batches arb observations to SQLite without blocking callers.
type ArbLogger struct {
	db       *DB
	cfg      ArbLoggerConfig
	executor string // executor contract address (constant per bot instance)

	ch chan ArbObservation

	mu    sync.Mutex
	dedup map[string]time.Time // cycle key → last logged time
}

func NewArbLogger(db *DB, executor string, cfg ArbLoggerConfig) *ArbLogger {
	if cfg.RetentionHours <= 0 {
		cfg.RetentionHours = 72
	}
	if cfg.MaxRows <= 0 {
		cfg.MaxRows = 500_000
	}
	return &ArbLogger{
		db:       db,
		cfg:      cfg,
		executor: strings.ToLower(executor),
		ch:       make(chan ArbObservation, arbLogChanSize),
		dedup:    make(map[string]time.Time),
	}
}

// Record enqueues a profitable cycle detection. Non-blocking — drops silently
// if the channel is full (backpressure from a slow DB write).
// cycleKey is used for dedup; pass an empty txHash if not yet executed.
func (al *ArbLogger) Record(cc cachedCycle, lp float64, profitUSD float64, tokenPriceUSD float64, hopPricesJSON string, amountInWei string, simProfitWei string, simProfitBps float64, gasPriceGwei float64, minProfitUSD float64, gasCostUSD float64, poolStatesJSON string, detectRPC string, executed bool, txHash string, rejectReason string) {
	key := cycleKey(cc.Cycle.Edges)

	al.mu.Lock()
	if last, ok := al.dedup[key]; ok && time.Since(last) < dedupWindow {
		al.mu.Unlock()
		return
	}
	al.dedup[key] = time.Now()
	al.mu.Unlock()

	obs := ArbObservation{
		ObservedAt:             time.Now(),
		Hops:                   len(cc.Cycle.Edges),
		LogProfit:              lp,
		ProfitPct:              (math.Exp(lp) - 1) * 100,
		ProfitUSD:              profitUSD,
		GasCostUSD:             gasCostUSD,
		NetProfitUSD:           profitUSD - gasCostUSD,
		TokenPriceUSD:          tokenPriceUSD,
		HopPricesJSON:          hopPricesJSON,
		AmountInWei:            amountInWei,
		SimProfitWei:           simProfitWei,
		SimProfitBps:           simProfitBps,
		GasPriceGwei:           gasPriceGwei,
		MinProfitUSDAtDecision: minProfitUSD,
		PoolStatesJSON:         poolStatesJSON,
		Dexes:                  buildDexes(cc),
		Tokens:                 buildTokens(cc),
		Pools:                  buildPools(cc),
		Executor:               al.executor,
		DetectRPC:              detectRPC,
		Executed:               executed,
		TxHash:                 txHash,
		RejectReason:           rejectReason,
	}

	select {
	case al.ch <- obs:
	default:
		// channel full — drop rather than block the swap-listener goroutine
	}
}

// Run drains the channel and writes to DB in batches. Blocks until ctx is cancelled.
func (al *ArbLogger) Run(ctx context.Context) {
	flushTicker := time.NewTicker(batchFlushEvery)
	pruneTicker := time.NewTicker(pruneInterval)
	dedupCleanTicker := time.NewTicker(5 * time.Minute)
	defer flushTicker.Stop()
	defer pruneTicker.Stop()
	defer dedupCleanTicker.Stop()

	var batch []ArbObservation

	flush := func() {
		if len(batch) == 0 {
			return
		}
		for _, obs := range batch {
			if err := al.db.InsertArbObservation(obs); err != nil {
				log.Printf("[arblog] insert error: %v", err)
			}
		}
		batch = batch[:0]
	}

	for {
		select {
		case <-ctx.Done():
			// Drain remaining items before exit
			for {
				select {
				case obs := <-al.ch:
					batch = append(batch, obs)
				default:
					flush()
					return
				}
			}

		case obs := <-al.ch:
			batch = append(batch, obs)

		case <-flushTicker.C:
			flush()

		case <-pruneTicker.C:
			n, err := al.db.PruneOldObservations(al.cfg.RetentionHours, al.cfg.MaxRows)
			if err != nil {
				log.Printf("[arblog] prune error: %v", err)
			} else if n > 0 {
				total, _ := al.db.ObservationCount()
				log.Printf("[arblog] pruned %d old observations (%d remaining)", n, total)
			}

		case <-dedupCleanTicker.C:
			// Evict dedup entries older than dedupWindow to prevent unbounded growth.
			al.mu.Lock()
			cutoff := time.Now().Add(-dedupWindow)
			for k, t := range al.dedup {
				if t.Before(cutoff) {
					delete(al.dedup, k)
				}
			}
			al.mu.Unlock()
		}
	}
}

func buildDexes(cc cachedCycle) string {
	parts := make([]string, len(cc.Cycle.Edges))
	for i, e := range cc.Cycle.Edges {
		parts[i] = e.Pool.DEX.String()
	}
	return strings.Join(parts, ",")
}

func buildTokens(cc cachedCycle) string {
	edges := cc.Cycle.Edges
	parts := make([]string, len(edges)+1)
	for i, e := range edges {
		parts[i] = e.TokenIn.Symbol
	}
	parts[len(edges)] = edges[len(edges)-1].TokenOut.Symbol
	return strings.Join(parts, ",")
}

func buildPools(cc cachedCycle) string {
	return strings.Join(cc.PoolAddrs, ",")
}
