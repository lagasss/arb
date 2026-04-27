package internal

import (
	"context"
	"fmt"
	"math"
	"strings"
	"sync"
	"time"
)

// lpCalibratorConfig holds tunable parameters for the LP floor auto-tuner.
// Nested under strategy.lp_calibrator in config.yaml.
type lpCalibratorConfig struct {
	// How often to re-run the calibration.
	IntervalSecs int `yaml:"interval_secs"`

	// How far back to look in our_trades for revert rate calculation.
	LookbackHours float64 `yaml:"lookback_hours"`

	// Target revert rate (fraction, e.g. 0.20 = 20%).
	// Above target → base_bps increases (floor too low, too many reverts).
	// Below target → base_bps decreases (floor too high, leaving trades behind).
	TargetRevertRate float64 `yaml:"target_revert_rate"`

	// Minimum resolved trades per class before adjusting.
	MinSamples int `yaml:"min_samples"`

	// Proportional control gain: new = current * (1 + alpha * (revert_rate - target)).
	Alpha float64 `yaml:"alpha"`

	// Hard limits on each per-hop base_bps value.
	MinBaseBps float64 `yaml:"min_base_bps"`
	MaxBaseBps float64 `yaml:"max_base_bps"`

	// Print calibration results every interval even when no change occurs.
	Verbose bool `yaml:"verbose"`
}

// dexRiskClass is the risk tier assigned to a trade for calibration.
type dexRiskClass int

const (
	dexRiskCurve     dexRiskClass = iota // Curve stable pools
	dexRiskV2                            // UniV2, SushiSwap, TraderJoe, Camelot
	dexRiskV3                            // UniV3, PancakeV3, SushiV3, RamsesV3
	dexRiskCamelotV3                     // CamelotV3, ZyberV3 (Algebra)
)

func (c dexRiskClass) String() string {
	return [...]string{"Curve", "V2", "V3", "CamelotV3"}[c]
}

// classifyDexes assigns the highest-risk class found in a comma-separated dexes
// string. Priority: CamelotV3 > V3 > V2 > Curve.
// Returns (class, true) on success; (_, false) if no known DEX is found.
func classifyDexes(dexes string) (dexRiskClass, bool) {
	best := dexRiskCurve
	found := false
	for _, d := range strings.Split(dexes, ",") {
		d = strings.TrimSpace(d)
		var cls dexRiskClass
		switch d {
		case "CamelotV3", "ZyberV3":
			cls = dexRiskCamelotV3
		case "UniV3", "PancakeV3", "SushiV3", "RamsesV3":
			cls = dexRiskV3
		case "UniV2", "SushiSwap", "TraderJoe", "Camelot":
			cls = dexRiskV2
		case "Curve":
			cls = dexRiskCurve
		default:
			continue
		}
		found = true
		if cls > best {
			best = cls
		}
	}
	return best, found
}

// LPCalibrator periodically queries recent trade outcomes and adjusts the
// dynamic_lp_floor base_bps values to drive each DEX class's revert rate
// toward the configured target.
type LPCalibrator struct {
	cfg  *Config
	db   *DB
	mu   sync.RWMutex
	live dynamicLPFloorConfig // live (auto-adjusted) copy, seeded from config.yaml
}

// NewLPCalibrator creates a calibrator seeded with the static floor config.
func NewLPCalibrator(cfg *Config, db *DB) *LPCalibrator {
	return &LPCalibrator{
		cfg:  cfg,
		db:   db,
		live: cfg.Strategy.DynamicLPFloor, // struct copy
	}
}

// ComputeFloor returns the dynamic LP floor (log-profit units) for a cycle
// using the current auto-adjusted base_bps values. Thread-safe.
func (lc *LPCalibrator) ComputeFloor(cycle Cycle, gasPriceGwei float64) float64 {
	lc.mu.RLock()
	dlp := lc.live // copy under lock
	lc.mu.RUnlock()

	gasMultiplier := 1.0
	switch {
	case gasPriceGwei >= dlp.GasHighGwei && dlp.GasHighGwei > 0:
		gasMultiplier = 1.5
	case gasPriceGwei >= dlp.GasElevatedGwei && dlp.GasElevatedGwei > 0:
		gasMultiplier = 1.2
	}

	total := 0.0
	for _, edge := range cycle.Edges {
		var baseBps float64
		switch edge.Pool.DEX {
		case DEXCurve:
			baseBps = dlp.BaseBpsCurve
		case DEXUniswapV2, DEXSushiSwap, DEXTraderJoe, DEXCamelot:
			baseBps = dlp.BaseBpsV2
		case DEXCamelotV3, DEXZyberV3:
			baseBps = dlp.BaseBpsCamelotV3
		default:
			baseBps = dlp.BaseBpsV3
		}

		tvlScale := 1.0
		if edge.Pool.TVLUSD > 0 && dlp.TVLRefUSD > 0 {
			tvlScale = math.Sqrt(dlp.TVLRefUSD / edge.Pool.TVLUSD)
			if tvlScale < dlp.TVLScaleMin {
				tvlScale = dlp.TVLScaleMin
			}
			if tvlScale > dlp.TVLScaleMax {
				tvlScale = dlp.TVLScaleMax
			}
		}

		volMult := pairVolMult(edge.TokenIn, edge.TokenOut, &dlp)
		total += baseBps * tvlScale * volMult * gasMultiplier
	}

	total += dlp.LatencyOverheadBps
	return total / 10000.0
}

// Run starts the calibration loop; blocks until ctx is cancelled.
func (lc *LPCalibrator) Run(ctx context.Context) {
	cal := &lc.cfg.Strategy.LPCalibrator
	interval := time.Duration(cal.IntervalSecs) * time.Second
	if interval <= 0 {
		interval = 5 * time.Minute
	}

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if lc.db == nil {
				continue
			}
			lc.calibrate()
		}
	}
}

// calibrate performs one pass: queries recent trades, computes per-class revert
// rates, and adjusts base_bps via proportional control.
func (lc *LPCalibrator) calibrate() {
	cal := &lc.cfg.Strategy.LPCalibrator
	since := time.Now().Add(-time.Duration(float64(time.Hour) * cal.LookbackHours)).Unix()

	trades, err := lc.db.QueryTradesSince(since)
	if err != nil {
		fmt.Printf("[lpcal] query error: %v\n", err)
		return
	}

	type classStat struct{ total, reverted int }
	stats := map[dexRiskClass]*classStat{
		dexRiskCurve:     {},
		dexRiskV2:        {},
		dexRiskV3:        {},
		dexRiskCamelotV3: {},
	}

	for _, t := range trades {
		if t.Status == "pending" {
			continue
		}
		cls, ok := classifyDexes(t.Dexes)
		if !ok {
			continue
		}
		stats[cls].total++
		if t.Status == "reverted" {
			stats[cls].reverted++
		}
	}

	lc.mu.Lock()
	defer lc.mu.Unlock()

	type adj struct {
		cls    dexRiskClass
		before float64
		after  float64
		n      int
		rate   float64
	}
	var adjs []adj

	doAdjust := func(cls dexRiskClass, field *float64) {
		s := stats[cls]
		if s.total < cal.MinSamples {
			return
		}
		rate := float64(s.reverted) / float64(s.total)
		delta := rate - cal.TargetRevertRate
		before := *field
		*field = clampLPBps(*field*(1+cal.Alpha*delta), cal.MinBaseBps, cal.MaxBaseBps)
		adjs = append(adjs, adj{cls, before, *field, s.total, rate})
	}

	doAdjust(dexRiskCurve, &lc.live.BaseBpsCurve)
	doAdjust(dexRiskV2, &lc.live.BaseBpsV2)
	doAdjust(dexRiskV3, &lc.live.BaseBpsV3)
	doAdjust(dexRiskCamelotV3, &lc.live.BaseBpsCamelotV3)

	if cal.Verbose || len(adjs) > 0 {
		fmt.Printf("[lpcal] %.1fh window: %d resolved trades\n",
			cal.LookbackHours, len(trades))
		for _, cls := range []dexRiskClass{dexRiskCurve, dexRiskV2, dexRiskV3, dexRiskCamelotV3} {
			s := stats[cls]
			if s.total == 0 {
				fmt.Printf("  %-12s  (no data)\n", cls)
				continue
			}
			fmt.Printf("  %-12s  %d trades  %.0f%% revert\n",
				cls, s.total, float64(s.reverted)/float64(s.total)*100)
		}
		if len(adjs) > 0 {
			for _, a := range adjs {
				fmt.Printf("  %-12s  base_bps %.2f → %.2f  (revert=%.0f%% target=%.0f%%)\n",
					a.cls, a.before, a.after, a.rate*100, cal.TargetRevertRate*100)
			}
		} else {
			fmt.Printf("  no class had >= %d samples — no adjustment made\n", cal.MinSamples)
		}
	}
}

func clampLPBps(v, lo, hi float64) float64 {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}
