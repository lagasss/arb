package internal

import "math"

// dynamicLPFloorConfig holds all tunable parameters for the dynamic LP floor.
// Nested under strategy.dynamic_lp_floor in config.yaml.
type dynamicLPFloorConfig struct {
	// Base buffer per hop by DEX type (bps).
	BaseBpsCurve     float64 `yaml:"base_bps_curve"`
	BaseBpsV2        float64 `yaml:"base_bps_v2"`
	BaseBpsV3        float64 `yaml:"base_bps_v3"`
	BaseBpsCamelotV3 float64 `yaml:"base_bps_camelot_v3"`

	// TVL reference point ($). Pools thinner than this get a larger buffer via
	// sqrt(tvl_ref_usd / pool_tvl), clamped to [tvl_scale_min, tvl_scale_max].
	TVLRefUSD   float64 `yaml:"tvl_ref_usd"`
	TVLScaleMin float64 `yaml:"tvl_scale_min"`
	TVLScaleMax float64 `yaml:"tvl_scale_max"`

	// Volatility class multipliers applied to the per-hop base buffer.
	VolStableMult   float64 `yaml:"vol_stable_mult"`   // both tokens are stablecoins
	VolVolatileMult float64 `yaml:"vol_volatile_mult"` // at least one small-cap token

	// Fixed per-cycle overhead added regardless of composition (bps).
	LatencyOverheadBps float64 `yaml:"latency_overhead_bps"`

	// Gas price thresholds for congestion multiplier (gwei).
	// Above gas_elevated_gwei: 1.2× multiplier.
	// Above gas_high_gwei:     1.5× multiplier.
	GasElevatedGwei float64 `yaml:"gas_elevated_gwei"`
	GasHighGwei     float64 `yaml:"gas_high_gwei"`
}

// dynamicLPFloor computes the minimum log-profit a cycle must have before it
// proceeds to simulation. The threshold is built up per-hop and accounts for
// the real sources of execution uncertainty between detection and on-chain
// inclusion:
//
//   - DEX type: V3 concentrated-liquidity pools can move sharply at tick
//     boundaries; Curve stable pools barely move at all.
//   - Pool TVL: thin pools shift more from a single competing swap.
//   - Token pair volatility class: stablecoin/stablecoin pairs drift far less
//     than small-cap/volatile pairs between blocks.
//   - Hop count: each pool is an independent source of drift; risks compound.
//   - Gas price: elevated gas implies network congestion and more blocks between
//     detection and inclusion, amplifying all of the above.
//
// A fixed latency overhead is added on top to cover sequencer scheduling
// variance regardless of cycle composition.
func dynamicLPFloor(cycle Cycle, gasPriceGwei float64, cfg *Config) float64 {
	return dynamicLPFloorScaled(cycle, gasPriceGwei, cfg, 1.0)
}

// dynamicLPFloorScaled is dynamicLPFloor with a final multiplier applied to
// the per-hop buffer sum (latency overhead is unaffected). Used by the
// per-executor routing: cycles routed to the lighter V3FlashMini contract
// can absorb thinner margins because the contract itself has a smaller gas
// footprint AND a narrower set of code paths that could drift between sim
// and on-chain, so we accept a lower minimum by scaling the per-hop buffer
// down. Typical values:
//
//	1.0 — full executor (default behavior)
//	0.5 — V3FlashMini (V3-only hops, direct pool.swap, ~350k gas budget)
//	0.3 — hypothetical tighter future contract
//
// The latency overhead is NOT scaled because sequencer variance doesn't
// depend on which contract we use.
func dynamicLPFloorScaled(cycle Cycle, gasPriceGwei float64, cfg *Config, hopMult float64) float64 {
	dlp := &cfg.Strategy.DynamicLPFloor

	gasMultiplier := 1.0
	switch {
	case gasPriceGwei >= dlp.GasHighGwei && dlp.GasHighGwei > 0:
		gasMultiplier = 1.5
	case gasPriceGwei >= dlp.GasElevatedGwei && dlp.GasElevatedGwei > 0:
		gasMultiplier = 1.2
	}

	total := 0.0
	for _, edge := range cycle.Edges {
		// Base buffer by DEX type.
		var baseBps float64
		switch edge.Pool.DEX {
		case DEXCurve:
			baseBps = dlp.BaseBpsCurve
		case DEXUniswapV2, DEXSushiSwap, DEXTraderJoe, DEXCamelot:
			baseBps = dlp.BaseBpsV2
		case DEXCamelotV3, DEXZyberV3:
			baseBps = dlp.BaseBpsCamelotV3
		default: // UniV3, PancakeV3, SushiV3, RamsesV3
			baseBps = dlp.BaseBpsV3
		}

		// TVL scaling: thin pools move more from competing swaps.
		// scale = sqrt(ref / pool_tvl), clamped to [min, max].
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

		// Volatility class multiplier based on the token pair.
		volMult := pairVolMult(edge.TokenIn, edge.TokenOut, dlp)

		total += baseBps * tvlScale * volMult * gasMultiplier
	}

	// Apply the executor-specific hop multiplier BEFORE adding the fixed
	// latency overhead. The latency overhead is a property of the chain, not
	// of our contract, so it doesn't scale with executor choice.
	total *= hopMult

	// Fixed per-cycle overhead: sequencer scheduling and mempool variance.
	total += dlp.LatencyOverheadBps

	return total / 10000.0 // bps → log-profit units
}

// pairVolMult returns the volatility class multiplier for a token pair.
//
//   - stable/stable (USDC/USDT etc.): minimal drift → low multiplier
//   - major/major or major/stable (WETH/USDC, WETH/WBTC): normal
//   - anything involving a small-cap token (ARB, GMX, LINK…): high multiplier
func pairVolMult(a, b *Token, dlp *dynamicLPFloorConfig) float64 {
	if isStablecoin(a) && isStablecoin(b) {
		return dlp.VolStableMult
	}
	if !isCoreToken(a) || !isCoreToken(b) {
		return dlp.VolVolatileMult
	}
	return 1.0
}

// isStablecoin returns true for known USD-pegged tokens.
func isStablecoin(t *Token) bool {
	switch t.Symbol {
	case "USDC", "USDT", "DAI", "USDC.e", "USDT.e", "FRAX",
		"MIM", "LUSD", "TUSD", "BUSD", "USDCE", "USD+", "DOLA":
		return true
	}
	return false
}

// isCoreToken returns true for stablecoins and the two major crypto assets.
// Everything else is considered small-cap volatile.
func isCoreToken(t *Token) bool {
	return isStablecoin(t) || t.Symbol == "WETH" || t.Symbol == "WBTC"
}
