package internal

import (
	"context"
	"crypto/ecdsa"
	"fmt"
	"log"
	"math/big"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethclient"
	"github.com/ethereum/go-ethereum/rpc"
)

// executorSupportsPancakeV3 is set at startup from
// trading.executor_supports_pancake_v3. When true, dexTypeOnChain dispatches
// PancakeV3 swaps to the dedicated DEX_PANCAKE_V3=8 handler in the contract.
// When false (the default), it falls back to DEX_V3=1 — which is BROKEN for
// PancakeV3 (calldata layout mismatch — see _swapPancakeV3 in ArbitrageExecutor.sol)
// but matches what's deployed at the current executor_contract address.
//
// The flip from 1→8 must happen AT THE SAME TIME as deploying the rebuilt
// contract that contains _swapPancakeV3, otherwise PancakeV3 hops will route
// into the contract's V2 fallback branch and fail unconditionally.
var executorSupportsPancakeV3 atomic.Bool

// lastHopShortfallBps allows the last hop's amountOutMin to dip slightly
// below the flash-borrow amount. SIM_PHANTOM analysis (2026-04-18) showed
// 100% of SIM_PHANTOM reverts were on the last hop, caused by the router's
// `amountOutMinimum >= borrow` check firing when sim over-estimated final-
// hop output by fractions of a basis point. Accepting a 1-bps shortfall
// converts most of those reverts into successes, at the cost of a dust
// loss on the trades where sim was mildly wrong. Set via cfg.Strategy.
// LastHopShortfallBps; 0 preserves the strict old behavior. Clamped to 50 bps.
var lastHopShortfallBps atomic.Int64

// SetLastHopShortfallBps applies the config value at startup. Bot.NewBot
// sets it from cfg.Strategy.LastHopShortfallBps after applyStrategyDefaults.
func SetLastHopShortfallBps(bps int64) {
	if bps < 0 {
		bps = 0
	}
	if bps > 50 {
		bps = 50
	}
	lastHopShortfallBps.Store(bps)
}

// SetExecutorSupportsPancakeV3 is a public setter so external binaries that
// import this package (e.g. cmd/smoketest) can mirror the running bot's
// dispatch policy. The bot sets this from config in NewBot(); standalone
// tools must read the same config and call this themselves, otherwise
// dexTypeOnChain will return the legacy DEX_V3 path that doesn't match the
// redeployed contract's bytecode.
func SetExecutorSupportsPancakeV3(b bool) {
	executorSupportsPancakeV3.Store(b)
}

// BuildExecuteCalldata constructs the full ABI-encoded calldata for the
// ArbitrageExecutor's `execute(tokens, amounts, hops, minProfit)` entrypoint
// for a given cycle. Used by `cmd/smoketest` to drive synthetic eth_call
// regressions per DEX dispatch path without going through the live executor.
//
// Mirrors the production path in `Executor.Submit`: buildHops → encodeHops →
// executeABI.Pack. Returns the calldata bytes ready for `eth_call`. Not used
// by the bot itself — the live submit path uses Submit() which inlines this
// plus vault capping, nonce handling, signing, and broadcast.
//
// When `forTest` is true the underlying buildHops bypasses its
// "last hop sim out < borrow" reject. Set this only from the smoketest binary
// where the goal is to verify the on-chain dispatch path for an intentionally
// unprofitable round-trip cycle.
func BuildExecuteCalldata(cycle Cycle, amountIn *big.Int, slippageBps int64, minProfitNative *big.Int, forTest bool) ([]byte, error) {
	if len(cycle.Edges) == 0 {
		return nil, fmt.Errorf("empty cycle")
	}
	hops, err := buildHopsOpt(cycle, amountIn, slippageBps, forTest)
	if err != nil {
		return nil, fmt.Errorf("buildHops: %w", err)
	}
	hopData, err := encodeHops(hops)
	if err != nil {
		return nil, fmt.Errorf("encodeHops: %w", err)
	}
	borrowToken := common.HexToAddress(cycle.Edges[0].TokenIn.Address)
	tokens := []common.Address{borrowToken}
	amounts := []*big.Int{amountIn}
	if minProfitNative == nil {
		minProfitNative = big.NewInt(1)
	}
	return executeABI.Pack("execute", tokens, amounts, hopData, minProfitNative)
}

// BuildExecuteV3FlashCalldata packs calldata for executeV3Flash(pool, borrowToken,
// amount, hopData, minProfit). v3FlashPool is the V3 pool the contract will call
// flash() on. Used by smoketest to probe the V3-flash callback path.
func BuildExecuteV3FlashCalldata(cycle Cycle, amountIn *big.Int, slippageBps int64, minProfitNative *big.Int, v3FlashPool common.Address, forTest bool) ([]byte, error) {
	if len(cycle.Edges) == 0 {
		return nil, fmt.Errorf("empty cycle")
	}
	hops, err := buildHopsOpt(cycle, amountIn, slippageBps, forTest)
	if err != nil {
		return nil, fmt.Errorf("buildHops: %w", err)
	}
	hopData, err := encodeHops(hops)
	if err != nil {
		return nil, fmt.Errorf("encodeHops: %w", err)
	}
	borrowToken := common.HexToAddress(cycle.Edges[0].TokenIn.Address)
	if minProfitNative == nil {
		minProfitNative = big.NewInt(1)
	}
	return executeV3FlashABI.Pack("executeV3Flash", v3FlashPool, borrowToken, amountIn, hopData, minProfitNative)
}

// BuildExecuteAaveFlashCalldata packs calldata for executeAaveFlash(token,
// amount, hopData, minProfit). Used by smoketest to probe the Aave-flash
// callback path.
func BuildExecuteAaveFlashCalldata(cycle Cycle, amountIn *big.Int, slippageBps int64, minProfitNative *big.Int, forTest bool) ([]byte, error) {
	if len(cycle.Edges) == 0 {
		return nil, fmt.Errorf("empty cycle")
	}
	hops, err := buildHopsOpt(cycle, amountIn, slippageBps, forTest)
	if err != nil {
		return nil, fmt.Errorf("buildHops: %w", err)
	}
	hopData, err := encodeHops(hops)
	if err != nil {
		return nil, fmt.Errorf("encodeHops: %w", err)
	}
	borrowToken := common.HexToAddress(cycle.Edges[0].TokenIn.Address)
	if minProfitNative == nil {
		minProfitNative = big.NewInt(1)
	}
	return executeAaveFlashABI.Pack("executeAaveFlash", borrowToken, amountIn, hopData, minProfitNative)
}

// BuildV3MiniFlashCalldata packs calldata for the V3FlashMini contract's
// flash(pool, borrowToken, amount, isToken0, packedHops, minProfit).
// Used by smoketest to probe the mini executor path end-to-end.
func BuildV3MiniFlashCalldata(cycle Cycle, amountIn *big.Int, slippageBps int64, v3FlashPool, flashPoolToken0 common.Address) ([]byte, error) {
	if len(cycle.Edges) == 0 {
		return nil, fmt.Errorf("empty cycle")
	}
	packed, err := packV3MiniHops(cycle, amountIn, slippageBps)
	if err != nil {
		return nil, fmt.Errorf("packV3MiniHops: %w", err)
	}
	borrowToken := common.HexToAddress(cycle.Edges[0].TokenIn.Address)
	isToken0 := strings.EqualFold(cycle.Edges[0].TokenIn.Address, flashPoolToken0.Hex())
	return executeV3MiniABI.Pack("flash", v3FlashPool, borrowToken, amountIn, isToken0, packed)
}

// BuildV4MiniFlashCalldata packs calldata for the V4Mini contract's
// flash(flashPool, borrowToken, amount, isToken0, packedV4Hops). The packed
// hops layout differs from V3FlashMini's (67 vs 61 bytes per hop because V4
// hops carry currency0/currency1/fee/tickSpacing/hooks alongside flags).
// See packV4MiniHops for the exact byte layout.
func BuildV4MiniFlashCalldata(cycle Cycle, amountIn *big.Int, v3FlashPool, flashPoolToken0 common.Address) ([]byte, error) {
	if len(cycle.Edges) == 0 {
		return nil, fmt.Errorf("empty cycle")
	}
	packed, err := packV4MiniHops(cycle)
	if err != nil {
		return nil, fmt.Errorf("packV4MiniHops: %w", err)
	}
	borrowToken := common.HexToAddress(cycle.Edges[0].TokenIn.Address)
	isToken0 := strings.EqualFold(cycle.Edges[0].TokenIn.Address, flashPoolToken0.Hex())
	return executeV4MiniABI.Pack("flash", v3FlashPool, borrowToken, amountIn, isToken0, packed)
}

// BuildMixedV3V4FlashCalldata packs calldata for MixedV3V4Executor.flash(...).
// Same shape as V3FlashMini/V4Mini but the per-hop layout uses the high bit
// of the flags byte to discriminate V3 vs V4 dispatch (see packMixedV3V4Hops).
func BuildMixedV3V4FlashCalldata(cycle Cycle, amountIn *big.Int, v3FlashPool, flashPoolToken0 common.Address) ([]byte, error) {
	if len(cycle.Edges) == 0 {
		return nil, fmt.Errorf("empty cycle")
	}
	packed, err := packMixedV3V4Hops(cycle)
	if err != nil {
		return nil, fmt.Errorf("packMixedV3V4Hops: %w", err)
	}
	borrowToken := common.HexToAddress(cycle.Edges[0].TokenIn.Address)
	isToken0 := strings.EqualFold(cycle.Edges[0].TokenIn.Address, flashPoolToken0.Hex())
	return executeMixedV3V4ABI.Pack("flash", v3FlashPool, borrowToken, amountIn, isToken0, packed)
}

// dexTypeOnChain maps our internal DEXType to the uint8 constant in ArbitrageExecutor.sol.
// Must stay in lock-step with the DEX_* constants in ArbitrageExecutor.sol.
//   DEX_V2=0, DEX_V3=1, DEX_CURVE=2, DEX_CAMELOT_V3=3, DEX_BALANCER=4,
//   DEX_CAMELOT_V2=5, DEX_RAMSES_V3=6, DEX_UNIV4=7, DEX_PANCAKE_V3=8
func dexTypeOnChain(d DEXType) uint8 {
	switch d {
	case DEXUniswapV3, DEXSushiSwapV3:
		return 1
	case DEXPancakeV3:
		// See executorSupportsPancakeV3 — gated on the contract being redeployed
		// with the dedicated _swapPancakeV3 handler that uses the no-deadline
		// calldata layout. Until then we route to DEX_V3 (which doesn't actually
		// work for Pancake — but neither does DEX_PANCAKE_V3 against the old
		// contract, so DEX_V3 keeps the failure mode unchanged rather than
		// silently turning every PancakeV3 hop into a misrouted V2 call).
		if executorSupportsPancakeV3.Load() {
			return 8
		}
		return 1
	case DEXRamsesV3:
		return 6
	case DEXUniswapV4:
		return 7
	case DEXCurve:
		return 2
	case DEXCamelotV3, DEXZyberV3: // Algebra-based pools share CamelotV3 handler
		return 3
	case DEXBalancerWeighted:
		return 4
	case DEXCamelot: // Camelot V2 router has extra referrer param — needs dedicated handler
		return 5
	default: // DEXUniswapV2, DEXTraderJoe, DEXSushiSwap, DEXRamsesV2, DEXChronos
		return 0
	}
}

// routerAddress returns the appropriate router/pool address for a swap hop.
// For V2/V3: the DEX router. For Curve/Balancer: the pool/vault directly.
// These are Arbitrum mainnet addresses.
var dexRouter = map[DEXType]string{
	DEXUniswapV2:   "0x4752ba5DBc23f44D87826276BF6Fd6b1C372aD24", // Uniswap V2 router
	DEXUniswapV3:   "0xE592427A0AEce92De3Edee1F18E0157C05861564", // Uniswap V3 SwapRouter
	DEXCamelot:     "0xc873fEcbd354f5A56E00E710B90EF4201db2448d", // Camelot V2 router
	DEXCamelotV3:   "0x1F721E2E82F6676FCE4eA07A5958cF098D339e18", // Camelot V3 (Algebra) router
	DEXTraderJoe:   "0xb4315e873dBcf96Ffd0acd8EA43f689D8c20fB30", // TraderJoe LB router
	DEXSushiSwap:   "0x1b02dA8Cb0d097eB8D57A175b88c7D8b47997506", // SushiSwap V2 router
	DEXSushiSwapV3: "0x8A21F6768C1f8075791D08546Dadf6daA0bE820c", // SushiSwap V3 router
	DEXRamsesV2:    "0xAAA87963EFeB6f7E0a2711F397663105Acb1805e", // Ramses V2 router
	DEXRamsesV3:    "0x4730e03EB4a58A5e20244062D5f9A99bCf5770a6", // Ramses V3 CL SwapRouter (uses exactInput)
	DEXPancakeV3:   "0x32226588378236fd0c7c4053999f88ac0e5cac77", // PancakeSwap V3 router
	DEXChronos:     "0xe708aa9e887980750c040a6a2cb901c37aa34f3b", // Chronos router
	DEXZyberV3:     "0xFa58b8024B49836772180f2Df902f231ba712F72", // Zyberswap V3 (Algebra) router
	DEXDeltaSwap:   "0x5FbE219e88f6c6F214Ce6f5B1fcAa0294F31aE1b", // DeltaSwap (GammaSwap) V2 router
	DEXSwapr:       "0x530476d5583724A89c8841eB6Da76E7Af4C0F17E", // Swapr (DXswap) V2 router
	DEXArbSwap:     "0xD01319f4b65b79124549dE409D36F25e04B3e551", // ArbSwap V2 router
	DEXUniswapV4:   "0x360E68faCcca8cA495c1B759Fd9EEe466db9FB32", // Uniswap V4 PoolManager
}

// Hop mirrors ArbitrageExecutor.sol's Hop struct for ABI encoding.
// Must match the 9-field Solidity struct exactly.
type Hop struct {
	DexType        uint8
	Pool           common.Address
	TokenIn        common.Address
	TokenOut       common.Address
	Fee            *big.Int // uint24, as *big.Int for ABI
	AmountOutMin   *big.Int
	CurveIndexIn   *big.Int // int128
	CurveIndexOut  *big.Int // int128
	BalancerPoolId [32]byte
}

// hopABIType is the ABI tuple type for a single Hop, matching the Solidity struct layout.
var hopABIType abi.Type
var hopArrayABIType abi.Type

func init() {
	var err error
	// Build the tuple type that mirrors Hop struct in ArbitrageExecutor.sol.
	hopABIType, err = abi.NewType("tuple", "Hop", []abi.ArgumentMarshaling{
		{Name: "dexType", Type: "uint8"},
		{Name: "pool", Type: "address"},
		{Name: "tokenIn", Type: "address"},
		{Name: "tokenOut", Type: "address"},
		{Name: "fee", Type: "uint24"},
		{Name: "amountOutMin", Type: "uint256"},
		{Name: "curveIndexIn", Type: "int128"},
		{Name: "curveIndexOut", Type: "int128"},
		{Name: "balancerPoolId", Type: "bytes32"},
	})
	if err != nil {
		panic(fmt.Sprintf("hopABIType: %v", err))
	}
	hopArrayABIType, err = abi.NewType("tuple[]", "Hop[]", []abi.ArgumentMarshaling{
		{Name: "dexType", Type: "uint8"},
		{Name: "pool", Type: "address"},
		{Name: "tokenIn", Type: "address"},
		{Name: "tokenOut", Type: "address"},
		{Name: "fee", Type: "uint24"},
		{Name: "amountOutMin", Type: "uint256"},
		{Name: "curveIndexIn", Type: "int128"},
		{Name: "curveIndexOut", Type: "int128"},
		{Name: "balancerPoolId", Type: "bytes32"},
	})
	if err != nil {
		panic(fmt.Sprintf("hopArrayABIType: %v", err))
	}
}

// hopTuple is the concrete Go struct that go-ethereum's ABI encoder maps to the Solidity Hop tuple.
// Field names must match the ABI argument names exactly (case-insensitive match by go-ethereum).
type hopTuple struct {
	DexType        uint8          `abi:"dexType"`
	Pool           common.Address `abi:"pool"`
	TokenIn        common.Address `abi:"tokenIn"`
	TokenOut       common.Address `abi:"tokenOut"`
	Fee            *big.Int       `abi:"fee"`
	AmountOutMin   *big.Int       `abi:"amountOutMin"`
	CurveIndexIn   *big.Int       `abi:"curveIndexIn"`
	CurveIndexOut  *big.Int       `abi:"curveIndexOut"`
	BalancerPoolId [32]byte       `abi:"balancerPoolId"`
}

// encodeHops ABI-encodes a []Hop into bytes for the contract's `hops` parameter.
func encodeHops(hops []Hop) ([]byte, error) {
	tuples := make([]hopTuple, len(hops))
	for i, h := range hops {
		tuples[i] = hopTuple{
			DexType:        h.DexType,
			Pool:           h.Pool,
			TokenIn:        h.TokenIn,
			TokenOut:       h.TokenOut,
			Fee:            h.Fee,
			AmountOutMin:   h.AmountOutMin,
			CurveIndexIn:   h.CurveIndexIn,
			CurveIndexOut:  h.CurveIndexOut,
			BalancerPoolId: h.BalancerPoolId,
		}
	}
	args := abi.Arguments{{Type: hopArrayABIType}}
	return args.Pack(tuples)
}

// v3Fee returns the Uniswap-native fee tier (parts-per-million) for a V3 pool.
// FeePPM takes priority when set (for sub-1bps tiers like RamsesV3 fee=1).
// Otherwise converts FeeBps (e.g. 5 bps) → ppm (500).
func v3Fee(pool *Pool) *big.Int {
	if pool.FeePPM > 0 {
		return big.NewInt(int64(pool.FeePPM))
	}
	return big.NewInt(int64(pool.FeeBps) * 100)
}

// buildHops converts a Cycle's edges and per-hop output amounts into on-chain Hop structs.
// slippage is applied as (1 - slippageBps/10000) to each simulated output.
func buildHops(cycle Cycle, amountIn *big.Int, slippageBps int64) ([]Hop, error) {
	return buildHopsOpt(cycle, amountIn, slippageBps, false)
}

// buildHopsOpt is the internal implementation. When `bypassProfitCheck` is true
// it skips the "last hop sim out < borrow" reject — only set this from the
// smoketest binary, where the goal is to exercise the contract dispatch path
// for a synthetic round-trip cycle that's intentionally unprofitable. The
// production submit path always uses buildHops with the strict check.
func buildHopsOpt(cycle Cycle, amountIn *big.Int, slippageBps int64, bypassProfitCheck bool) ([]Hop, error) {
	hops := make([]Hop, len(cycle.Edges))
	current := new(big.Int).Set(amountIn)

	for i, edge := range cycle.Edges {
		pool := edge.Pool
		dex := pool.DEX
		onChainType := dexTypeOnChain(dex)

		// Resolve pool address (for Curve/Balancer we swap against the pool directly)
		var poolAddr common.Address
		switch dex {
		case DEXCurve, DEXBalancerWeighted:
			poolAddr = common.HexToAddress(pool.Address)
		default:
			r, ok := dexRouter[dex]
			if !ok {
				return nil, fmt.Errorf("no router for DEX %s", dex)
			}
			poolAddr = common.HexToAddress(r)
		}

		// Simulate this hop to get expected output
		sim := SimulatorFor(dex)
		out := sim.AmountOut(pool, edge.TokenIn, current)
		if out == nil || out.Sign() <= 0 {
			return nil, fmt.Errorf("hop %d simulation returned 0", i)
		}

		// Early-exit: if the last hop's simulated output is already below the borrow
		// amount, the cycle is unprofitable at current pool state. Reject immediately
		// to avoid submitting a guaranteed-to-fail transaction. The smoketest path
		// bypasses this check because it intentionally builds round-trip cycles to
		// exercise the contract's per-DEX dispatch.
		isLastHop := i == len(cycle.Edges)-1
		if !bypassProfitCheck && isLastHop && out.Cmp(amountIn) < 0 {
			return nil, fmt.Errorf("cycle unprofitable: last hop sim out %s < borrow %s", out, amountIn)
		}

		// amountOutMin: real slippage guard so a moved pool reverts at the hop level
		// rather than reaching the flash loan repayment and failing with
		// "ERC20: transfer amount exceeds balance".
		//
		// Intermediate hops: simulated output × (1 - slippageBps/10000), minimum 1 wei.
		// Last hop: additionally floored at amountIn so the contract can always repay the
		// flash loan. The contract's minProfit check enforces actual profitability on top.
		slippedOut := new(big.Int).Mul(out, big.NewInt(10_000-slippageBps))
		slippedOut.Div(slippedOut, big.NewInt(10_000))
		if slippedOut.Sign() <= 0 {
			slippedOut = big.NewInt(1)
		}
		amountOutMin := slippedOut
		if isLastHop && amountOutMin.Cmp(amountIn) < 0 {
			// Clamp the last hop's amountOutMin at or just-below the flash
			// borrow amount. `lastHopShortfallBps` controls how much below:
			//   0   → strict: amountOutMin = borrow (old behavior; every
			//         router-level min-out miss reverts)
			//   1-50 → borrow × (10000 - bps) / 10000
			// See SIM_PHANTOM analysis 2026-04-18 for rationale.
			shortfall := lastHopShortfallBps.Load()
			if shortfall > 0 {
				numer := new(big.Int).Mul(amountIn, big.NewInt(10_000-shortfall))
				amountOutMin = numer.Div(numer, big.NewInt(10_000))
			} else {
				amountOutMin = new(big.Int).Set(amountIn)
			}
		}

		// Curve coin indices: token0 = index 0, token1 = index 1
		curveIn, curveOut := big.NewInt(0), big.NewInt(1)
		if dex == DEXCurve && strings.EqualFold(edge.TokenIn.Address, pool.Token1.Address) {
			curveIn, curveOut = big.NewInt(1), big.NewInt(0)
		}

		// Balancer pool ID / UniV4 hooks address (packed into bytes32)
		var balPoolID [32]byte
		if dex == DEXBalancerWeighted && pool.PoolID != "" {
			id := common.FromHex(pool.PoolID)
			copy(balPoolID[:], id)
		} else if dex == DEXUniswapV4 && pool.V4Hooks != "" {
			// Right-align the hooks address in bytes32 (matches Solidity address(bytes20(...)))
			hooks := common.FromHex(pool.V4Hooks)
			copy(balPoolID[32-len(hooks):], hooks)
		}

		// V4: tickSpacing goes in curveIndexIn (reused field)
		if dex == DEXUniswapV4 && pool.TickSpacing != 0 {
			curveIn = big.NewInt(int64(pool.TickSpacing))
		}

		hops[i] = Hop{
			DexType:        onChainType,
			Pool:           poolAddr,
			TokenIn:        common.HexToAddress(edge.TokenIn.Address),
			TokenOut:       common.HexToAddress(edge.TokenOut.Address),
			Fee:            v3Fee(pool), // V3 fee in Uniswap ppm units
			AmountOutMin:   amountOutMin,
			CurveIndexIn:   curveIn,
			CurveIndexOut:  curveOut,
			BalancerPoolId: balPoolID,
		}

		current = out
	}
	return hops, nil
}

// cycleIsV3MiniShape returns true when a cycle's STRUCTURE (hop count,
// DEX types) is compatible with the V3FlashMini contract. It does NOT check
// flash source availability — that decision happens later in the pipeline
// (after optimalAmountIn resolves the borrow token) and requires qualifyForV3Mini
// below. Split from qualifyForV3Mini because evalOneCandidate needs to know
// shape-eligibility BEFORE flash source selection, to pick the right
// dynamicLPFloor scaling.
//
// Requirements:
//  1. Hop count in [2, 3] — the mini contract's execution is sized for
//     short cycles; longer cycles fall through to the full executor.
//  2. Every hop is a V3-family pool with the standard pool.swap() interface:
//     UniV3, SushiV3, RamsesV3, PancakeV3, CamelotV3, ZyberV3. UniV4 is NOT
//     supported (V4 uses the PoolManager singleton, not direct pool.swap).
//     V2/Curve/Balancer/Solidly are not supported either.
func cycleIsV3MiniShape(cycle Cycle) bool {
	n := len(cycle.Edges)
	if n < 2 || n > 5 {
		return false
	}
	for _, e := range cycle.Edges {
		switch e.Pool.DEX {
		case DEXUniswapV3, DEXSushiSwapV3, DEXRamsesV3, DEXPancakeV3, DEXCamelotV3, DEXZyberV3:
			// ok — callbacks handled by the mini contract's uniswapV3/
			// pancakeV3/algebra callback aliases.
		default:
			return false
		}
	}
	return true
}

// qualifyForV3Mini returns true when a cycle can actually be executed by the
// V3FlashMini contract right now — structural check AND the flash source is
// a V3 pool (V3FlashMini only knows how to borrow via IUniswapV3Pool.flash).
// Called in the submit path after flash source is resolved, to make the
// final routing decision.
func qualifyForV3Mini(cycle Cycle, flashSel FlashSelection) bool {
	if flashSel.Source != FlashV3Pool {
		return false
	}
	return cycleIsV3MiniShape(cycle)
}

// cycleIsV4MiniShape returns true when a cycle's STRUCTURE is compatible with
// V4Mini: 2-5 hops AND every hop is DEXUniswapV4. Mixed-DEX cycles must
// fall through to the generic ArbitrageExecutor (V4Mini has no V2/V3/Curve
// dispatch). Native-ETH legs are allowed (handled inline by V4Mini's
// `_settleCurrency`).
//
// The hook gate (`allowedHooks` whitelist) is enforced ON-CHAIN at the
// V4Mini level, not here — letting unknown-hook pools through the shape
// check is fine because they revert at execution before any harm.
func cycleIsV4MiniShape(cycle Cycle) bool {
	n := len(cycle.Edges)
	if n < 2 || n > 5 {
		return false
	}
	for _, e := range cycle.Edges {
		if e.Pool.DEX != DEXUniswapV4 {
			return false
		}
		if !hookGateAllows(e.Pool.V4Hooks) {
			return false
		}
	}
	return true
}

// hookGateAllows consults the HookSync classifier cache. Pools with no hook
// (address(0)) always pass. Pools with an unclassified hook fail — the
// shape filter refuses to route cycles through hooks the off-chain
// classifier hasn't vetted yet. Set at startup by Bot.Run.
var globalHookGate *HookSync

func SetGlobalHookGate(h *HookSync) { globalHookGate = h }

func hookGateAllows(hooks string) bool {
	hk := strings.ToLower(strings.TrimSpace(hooks))
	if hk == "" || hk == "0x0000000000000000000000000000000000000000" {
		return true
	}
	if globalHookGate == nil {
		return false
	}
	return globalHookGate.IsAllowed(hk)
}

// qualifyForV4Mini is the runtime gate: shape OK + V3 pool flash source
// available (V4Mini, like V3FlashMini, borrows via IUniswapV3Pool.flash).
// Called after flash source resolution.
func qualifyForV4Mini(cycle Cycle, flashSel FlashSelection) bool {
	if flashSel.Source != FlashV3Pool {
		return false
	}
	return cycleIsV4MiniShape(cycle)
}

// cycleIsMixedV3V4Shape returns true when a cycle's STRUCTURE fits
// MixedV3V4Executor: 2-5 hops AND every hop is either UniV4 or a
// V3-family pool with the standard pool.swap()+callback interface.
// Cycles entirely V3 should still go to V3FlashMini; cycles entirely V4
// should still go to V4Mini. This shape is specifically for the MIXED
// case where neither single-DEX mini qualifies but we'd otherwise have
// to fall through to the generic executor with its buggy V4 path.
func cycleIsMixedV3V4Shape(cycle Cycle) bool {
	n := len(cycle.Edges)
	if n < 2 || n > 5 {
		return false
	}
	hasV4 := false
	hasV3 := false
	for _, e := range cycle.Edges {
		switch e.Pool.DEX {
		case DEXUniswapV4:
			hasV4 = true
			if !hookGateAllows(e.Pool.V4Hooks) {
				return false
			}
		case DEXUniswapV3, DEXSushiSwapV3, DEXRamsesV3, DEXPancakeV3, DEXCamelotV3, DEXZyberV3:
			hasV3 = true
		default:
			return false
		}
	}
	return hasV4 && hasV3
}

// qualifyForMixedV3V4: shape OK + V3-pool flash source. Called after the
// flash source is resolved.
func qualifyForMixedV3V4(cycle Cycle, flashSel FlashSelection) bool {
	if flashSel.Source != FlashV3Pool {
		return false
	}
	return cycleIsMixedV3V4Shape(cycle)
}

// packMixedV3V4Hops serialises a mixed V3+V4 cycle into MixedV3V4Executor's
// 67-byte-per-hop blob. The flags byte (offset 40) carries:
//   bit 0 = zeroForOne
//   bit 7 = isV4 (1 = V4 hop with PoolKey payload at [41:67],
//                  0 = V3 hop, [41:67] is zero-padding)
//
// V4 hop payload at [41:67]:
//   [41:44] fee uint24 BE
//   [44:47] tickSpacing int24 BE two's-complement
//   [47:67] hooks address
//
// V3 hop payload at [0:40] (currency0/currency1 fields are reused as
// pool/tokenOut for V3 hops — same byte positions):
//   [ 0:20] pool address (V3-compatible)
//   [20:40] tokenOut (informational; receive side derived from swap result)
func packMixedV3V4Hops(cycle Cycle) ([]byte, error) {
	if len(cycle.Edges) == 0 {
		return nil, fmt.Errorf("empty cycle")
	}
	out := make([]byte, 0, 67*len(cycle.Edges))
	for i, edge := range cycle.Edges {
		if edge.Pool.Token0 == nil || edge.Pool.Token1 == nil {
			return nil, fmt.Errorf("hop %d pool missing token0/token1", i)
		}
		isV4 := edge.Pool.DEX == DEXUniswapV4
		zeroForOne := strings.EqualFold(edge.TokenIn.Address, edge.Pool.Token0.Address)

		var flags byte
		if zeroForOne {
			flags |= 0x01
		}
		if isV4 {
			flags |= 0x80
		}

		if isV4 {
			if edge.Pool.TickSpacing == 0 {
				return nil, fmt.Errorf("hop %d V4 pool has zero tickSpacing", i)
			}
			c0 := common.HexToAddress(edge.Pool.Token0.Address)
			c1 := common.HexToAddress(edge.Pool.Token1.Address)
			out = append(out, c0.Bytes()...)
			out = append(out, c1.Bytes()...)
			out = append(out, flags)
			feePPM := edge.Pool.FeePPM
			if feePPM == 0 {
				feePPM = edge.Pool.FeeBps * 100
			}
			out = append(out, byte(feePPM>>16), byte(feePPM>>8), byte(feePPM))
			ts := uint32(edge.Pool.TickSpacing)
			if edge.Pool.TickSpacing < 0 {
				ts = uint32(int32(edge.Pool.TickSpacing))
			}
			out = append(out, byte(ts>>16), byte(ts>>8), byte(ts))
			var hooks common.Address
			if edge.Pool.V4Hooks != "" {
				hooks = common.HexToAddress(edge.Pool.V4Hooks)
			}
			out = append(out, hooks.Bytes()...)
		} else {
			// V3 hop: pool + tokenOut at [0:40], flags at [40], zeros at [41:67].
			poolAddr := common.HexToAddress(edge.Pool.Address)
			tokenOut := common.HexToAddress(edge.TokenOut.Address)
			out = append(out, poolAddr.Bytes()...)
			out = append(out, tokenOut.Bytes()...)
			out = append(out, flags)
			padding := make([]byte, 26)
			out = append(out, padding...)
		}
	}
	return out, nil
}

// packV4MiniHops serialises a V4-only cycle into V4Mini's packed hops blob.
// Layout (67 bytes per hop):
//
//	[ 0:20] currency0 (PoolKey lower-address; address(0) = native ETH)
//	[20:40] currency1 (PoolKey higher-address)
//	[40:41] flags byte: bit 0 = zeroForOne (1 if hop's tokenIn is currency0)
//	[41:44] fee uint24 big-endian (PoolKey fee tier in pips)
//	[44:47] tickSpacing int24 big-endian, two's-complement
//	[47:67] hooks (address; address(0) for no-hook pools)
//
// Each hop's currency0/currency1 is derived from pool.Token0/Token1 (the
// canonical low-address-first ordering). zeroForOne is true when the hop's
// tokenIn equals Token0. Native-ETH boundaries are handled by the bot's
// upstream cycle builder — this function trusts whatever Token addresses
// it's given.
func packV4MiniHops(cycle Cycle) ([]byte, error) {
	if len(cycle.Edges) == 0 {
		return nil, fmt.Errorf("empty cycle")
	}
	out := make([]byte, 0, 67*len(cycle.Edges))
	for i, edge := range cycle.Edges {
		if edge.Pool.DEX != DEXUniswapV4 {
			return nil, fmt.Errorf("hop %d is not UniV4 (got %s)", i, edge.Pool.DEX)
		}
		if edge.Pool.Token0 == nil || edge.Pool.Token1 == nil {
			return nil, fmt.Errorf("hop %d pool missing token0/token1", i)
		}
		if edge.Pool.TickSpacing == 0 {
			return nil, fmt.Errorf("hop %d pool has zero tickSpacing (V4 PoolKey requires it)", i)
		}
		c0 := common.HexToAddress(edge.Pool.Token0.Address)
		c1 := common.HexToAddress(edge.Pool.Token1.Address)
		out = append(out, c0.Bytes()...)
		out = append(out, c1.Bytes()...)
		var flags byte
		zeroForOne := strings.EqualFold(edge.TokenIn.Address, edge.Pool.Token0.Address)
		if zeroForOne {
			flags |= 0x01
		}
		out = append(out, flags)
		// fee: use FeePPM if non-zero, else FeeBps*100. Encoded as uint24 BE.
		feePPM := edge.Pool.FeePPM
		if feePPM == 0 {
			feePPM = edge.Pool.FeeBps * 100
		}
		out = append(out, byte(feePPM>>16), byte(feePPM>>8), byte(feePPM))
		// tickSpacing as int24 BE two's-complement.
		ts := uint32(edge.Pool.TickSpacing)
		if edge.Pool.TickSpacing < 0 {
			ts = uint32(int32(edge.Pool.TickSpacing))
		}
		out = append(out, byte(ts>>16), byte(ts>>8), byte(ts))
		// hooks address (or 0x0 if absent / "no hooks").
		var hooks common.Address
		if edge.Pool.V4Hooks != "" {
			hooks = common.HexToAddress(edge.Pool.V4Hooks)
		}
		out = append(out, hooks.Bytes()...)
	}
	return out, nil
}

// packV3MiniHops serialises a cycle into V3FlashMini's packed hops blob.
// Layout (61 bytes per hop):
//
//	[ 0:20] pool address (V3-compatible)
//	[20:40] tokenOut address
//	[40:41] flags byte: bit 0 = zeroForOne
//	[41:61] amountOutMin as big-endian uint160
//
// The per-hop amountOutMin is the simulated output with slippage applied,
// exactly matching the intermediate-hop treatment in buildHopsOpt. The last
// hop's amountOutMin is floored at `amountIn` (borrow amount), same as
// buildHopsOpt, so the flash loan can always be repaid at the hop level.
//
// Simulation uses the EXACT same SimulatorFor(pool.DEX).AmountOut() code
// path as buildHops, so the sim and on-chain amounts track each other with
// the same accuracy as the main executor.
func packV3MiniHops(cycle Cycle, amountIn *big.Int, slippageBps int64) ([]byte, error) {
	out := make([]byte, 0, 61*len(cycle.Edges))
	current := new(big.Int).Set(amountIn)
	lastIdx := len(cycle.Edges) - 1

	for i, edge := range cycle.Edges {
		// Pool address (20 bytes)
		poolAddr := common.HexToAddress(edge.Pool.Address)
		out = append(out, poolAddr.Bytes()...)

		// tokenOut (20 bytes)
		tokenOut := common.HexToAddress(edge.TokenOut.Address)
		out = append(out, tokenOut.Bytes()...)

		// flags: bit0 = zeroForOne
		var flags byte
		zeroForOne := strings.EqualFold(edge.TokenIn.Address, edge.Pool.Token0.Address)
		if zeroForOne {
			flags |= 0x01
		}
		out = append(out, flags)

		// Simulate this hop to get expected output (matches buildHopsOpt logic).
		sim := SimulatorFor(edge.Pool.DEX)
		sOut := sim.AmountOut(edge.Pool, edge.TokenIn, current)
		if sOut == nil || sOut.Sign() <= 0 {
			return nil, fmt.Errorf("hop %d sim returned 0", i)
		}

		// Apply per-hop slippage.
		slipped := new(big.Int).Mul(sOut, big.NewInt(10_000-slippageBps))
		slipped.Div(slipped, big.NewInt(10_000))
		if slipped.Sign() <= 0 {
			slipped = big.NewInt(1)
		}
		// Last hop: floor at amountIn so flash loan can always repay.
		if i == lastIdx && slipped.Cmp(amountIn) < 0 {
			slipped = new(big.Int).Set(amountIn)
		}
		// Pack as big-endian uint160 (20 bytes, left-padded with zeros).
		amtBytes := slipped.Bytes()
		if len(amtBytes) > 20 {
			return nil, fmt.Errorf("hop %d amountOutMin overflows uint160", i)
		}
		pad := make([]byte, 20-len(amtBytes))
		out = append(out, pad...)
		out = append(out, amtBytes...)

		current = sOut
	}
	return out, nil
}

// Executor builds and submits arbitrage transactions to the ArbitrageExecutor contract.
type Executor struct {
	client        *ethclient.Client // used for reads (nonce, baseFee)
	submitClient  *ethclient.Client // used only for SendTransaction — points at sequencer directly
	simClient     *ethclient.Client // used only for eth_call simulations — dedicated RPC to avoid rate limits
	privateKey    *ecdsa.PrivateKey
	address       common.Address
	contractAddr  common.Address // full ArbitrageExecutor — handles any cycle
	// v3MiniAddr: V3FlashMini contract address, used when a cycle qualifies
	// for the lightweight V3-only path (see qualifyForV3Mini). Zero when
	// not configured — in which case all cycles route to contractAddr.
	v3MiniAddr    common.Address
	// v4MiniAddr: V4Mini contract address, used when a cycle qualifies for
	// the lightweight V4-only path (see qualifyForV4Mini). Zero = disabled,
	// V4 cycles fall through to the generic executor (which produces
	// V4_HANDLER reverts on any cycle touching native ETH or active hooks).
	v4MiniAddr    common.Address
	// mixedV3V4Addr: MixedV3V4Executor address. Used when a cycle has at
	// least one V4 hop AND at least one V3 hop (mixed). Zero = disabled,
	// mixed cycles fall to the generic executor (still V4_HANDLER on any
	// V4 hop touching native ETH or active hooks). See qualifyForMixedV3V4.
	mixedV3V4Addr common.Address
	chainID       *big.Int
	balancerVault common.Address // Balancer vault address for flash loan balance checks

	// ── Vault balance cache (per token, refreshed every block) ───────────────
	// Eliminates the 20-40ms eth_call to balanceOf on every trade submission.
	vaultBalMu          sync.RWMutex
	vaultBalCache       map[common.Address]*big.Int // token → cached balance
	vaultBalFetchedAt   time.Time                   // when the cache was last populated
	lastVaultRefreshLog time.Time                   // throttle for refresher log line

	// Block-tick vault refresh. On every new block header, onNewBlock kicks a
	// goroutine that reads all borrowable token balances in one batched
	// multicall. In-flight guard drops the request if the previous batch is
	// still running, so a slow RPC can't queue up refreshes. Set via
	// SetVaultRefreshTokens; if nil, the block-tick refresh is disabled.
	vaultRefreshTokens   func() []string
	vaultRefreshInFlight atomic.Bool

	// ── Nonce cache (refreshed continuously) ─────────────────────────────────
	// Eliminates the ~10ms PendingNonceAt round-trip on every trade submission.
	// Updated by warmupNonce goroutine; incremented atomically by Submit on use.
	nonceVal     atomic.Uint64
	nonceLoaded  atomic.Bool

	// ── BaseFee cache (refreshed every block via headers subscription) ───────
	// Eliminates the HeaderByNumber round-trip on every trade submission.
	baseFeeMu    sync.RWMutex
	baseFeeCache *big.Int
}

var executeABI abi.ABI         // Balancer flash loan entry point (ArbitrageExecutor)
var executeV3FlashABI abi.ABI  // V3 pool flash entry point (ArbitrageExecutor)
var executeAaveFlashABI abi.ABI // Aave V3 flash loan entry point (ArbitrageExecutor)
var executeV3MiniABI abi.ABI   // V3FlashMini.flash() — specialised V3-only executor
var executeV4MiniABI abi.ABI   // V4Mini.flash() — specialised V4-only executor (single-unlock multi-hop)
var executeMixedV3V4ABI abi.ABI // MixedV3V4Executor.flash() — mixed V3+V4 multi-hop, single unlock
var erc20BalanceOfABI abi.ABI

func init() {
	var err error
	executeABI, err = abi.JSON(strings.NewReader(`[{
		"name":"execute","type":"function","stateMutability":"nonpayable",
		"inputs":[
			{"name":"tokens","type":"address[]"},
			{"name":"amounts","type":"uint256[]"},
			{"name":"hops","type":"bytes"},
			{"name":"minProfit","type":"uint256"}
		],
		"outputs":[]
	}]`))
	if err != nil {
		panic(fmt.Sprintf("executeABI: %v", err))
	}
	executeV3FlashABI, err = abi.JSON(strings.NewReader(`[{
		"name":"executeV3Flash","type":"function","stateMutability":"nonpayable",
		"inputs":[
			{"name":"pool","type":"address"},
			{"name":"borrowToken","type":"address"},
			{"name":"amount","type":"uint256"},
			{"name":"hops","type":"bytes"},
			{"name":"minProfit","type":"uint256"}
		],
		"outputs":[]
	}]`))
	if err != nil {
		panic(fmt.Sprintf("executeV3FlashABI: %v", err))
	}
	executeAaveFlashABI, err = abi.JSON(strings.NewReader(`[{
		"name":"executeAaveFlash","type":"function","stateMutability":"nonpayable",
		"inputs":[
			{"name":"token","type":"address"},
			{"name":"amount","type":"uint256"},
			{"name":"hops","type":"bytes"},
			{"name":"minProfit","type":"uint256"}
		],
		"outputs":[]
	}]`))
	if err != nil {
		panic(fmt.Sprintf("executeAaveFlashABI: %v", err))
	}
	// V3FlashMini entry point: `flash(flashPool, borrowToken, amount, isToken0, hops)`.
	// Hops is NOT the abi-encoded Hop[] used by ArbitrageExecutor — it's a
	// packed bytes blob (61 bytes/hop: 20 bytes pool + 20 bytes tokenOut
	// + 1 byte flags + 20 bytes amountOutMin). See packV3MiniHops.
	executeV3MiniABI, err = abi.JSON(strings.NewReader(`[{
		"name":"flash","type":"function","stateMutability":"nonpayable",
		"inputs":[
			{"name":"flashPool","type":"address"},
			{"name":"borrowToken","type":"address"},
			{"name":"amount","type":"uint256"},
			{"name":"isToken0","type":"bool"},
			{"name":"hops","type":"bytes"}
		],
		"outputs":[]
	}]`))
	if err != nil {
		panic(fmt.Sprintf("executeV3MiniABI: %v", err))
	}
	// V4Mini entry point: same flash(...) shape as V3FlashMini, different
	// hops layout (67 bytes/hop: 20 currency0 + 20 currency1 + 1 flags +
	// 3 fee uint24 + 3 tickSpacing int24 + 20 hooks). See packV4MiniHops.
	executeV4MiniABI, err = abi.JSON(strings.NewReader(`[{
		"name":"flash","type":"function","stateMutability":"nonpayable",
		"inputs":[
			{"name":"flashPool","type":"address"},
			{"name":"borrowToken","type":"address"},
			{"name":"amount","type":"uint256"},
			{"name":"isToken0","type":"bool"},
			{"name":"hops","type":"bytes"}
		],
		"outputs":[]
	}]`))
	if err != nil {
		panic(fmt.Sprintf("executeV4MiniABI: %v", err))
	}
	// MixedV3V4Executor entry point — same flash(...) shape as V3FlashMini /
	// V4Mini, but the per-hop layout uses the high bit of the flags byte to
	// discriminate V3 vs V4 dispatch (see packMixedV3V4Hops).
	executeMixedV3V4ABI, err = abi.JSON(strings.NewReader(`[{
		"name":"flash","type":"function","stateMutability":"nonpayable",
		"inputs":[
			{"name":"flashPool","type":"address"},
			{"name":"borrowToken","type":"address"},
			{"name":"amount","type":"uint256"},
			{"name":"isToken0","type":"bool"},
			{"name":"hops","type":"bytes"}
		],
		"outputs":[]
	}]`))
	if err != nil {
		panic(fmt.Sprintf("executeMixedV3V4ABI: %v", err))
	}
	erc20BalanceOfABI, err = abi.JSON(strings.NewReader(`[{
		"name":"balanceOf","type":"function","stateMutability":"view",
		"inputs":[{"name":"account","type":"address"}],
		"outputs":[{"name":"","type":"uint256"}]
	}]`))
	if err != nil {
		panic(fmt.Sprintf("erc20BalanceOfABI: %v", err))
	}
}

// dialFastSubmitClient builds an ethclient backed by a http.Transport tuned
// for low-latency burst submissions. Two goals:
//
//  1. Keep HTTP/2 — the default. HTTP/2 multiplexes all requests over ONE
//     TCP connection, eliminating the "pool empty under burst" failure
//     mode that bit trades 143-152 (submissions 1-7 each paid 350-500 ms
//     TLS handshake because the default HTTP/1.1 pool only keeps 2 idle
//     connections per host). With HTTP/2 we only need 1 warm connection
//     no matter how many concurrent submissions we fire.
//
//  2. Bump the idle-conn timeout well above Cloudflare's server-side
//     idle timer so our connection doesn't get pruned during long inter-
//     trade gaps (typical 30-60 min between submissions). Paired with the
//     2-second keepalive ping in Warmup, the single warm connection stays
//     live indefinitely.
//
// Previous attempt (trade 153/154 with custom DialContext) broke HTTP/2
// auto-negotiation — Go's http.Transport falls back to HTTP/1.1 when
// DialContext is overridden unless http2.ConfigureTransport is called
// explicitly. We now clone http.DefaultTransport which preserves all of
// Go's HTTP/2 machinery and only tweak the knobs that matter for our
// workload.
func dialFastSubmitClient(ctx context.Context, url string) (*ethclient.Client, error) {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	// Preserve HTTP/2 auto-negotiation (the clone already has it).
	transport.ForceAttemptHTTP2 = true
	// Keep a generous idle pool — with HTTP/2 we really only need 1, but
	// this gives breathing room in case the server ever downgrades to
	// HTTP/1.1 (which would serialize our submissions on 1 conn anyway,
	// so having more in the pool would let concurrent requests fan out).
	transport.MaxIdleConns = 20
	transport.MaxIdleConnsPerHost = 20
	// Beat Cloudflare's ~5 min server-side idle close with a 10 min client
	// side keepalive window. The real keepalive mechanism is the 2-second
	// ping loop in Warmup — this just prevents Go's transport from
	// pruning connections prematurely between pings.
	transport.IdleConnTimeout = 10 * time.Minute
	// Fail fast on dead endpoints so a stuck connection can't stall the
	// submission path.
	transport.TLSHandshakeTimeout = 2 * time.Second
	transport.ResponseHeaderTimeout = 5 * time.Second
	httpClient := &http.Client{
		Transport: transport,
		Timeout:   5 * time.Second,
	}
	rpcClient, err := rpc.DialOptions(ctx, url, rpc.WithHTTPClient(httpClient))
	if err != nil {
		return nil, err
	}
	return ethclient.NewClient(rpcClient), nil
}

// SetV3MiniAddress configures the V3FlashMini contract address. When set,
// cycles that qualify (all V3-family hops, 2-3 hops, V3-flash available) will
// be routed to this contract instead of the full executor. Empty string or
// zero address disables the mini path — Bot.Run calls this right after
// NewExecutor so NewExecutor's signature stays stable.
func (e *Executor) SetV3MiniAddress(addr string) {
	if addr == "" {
		e.v3MiniAddr = common.Address{}
		return
	}
	e.v3MiniAddr = common.HexToAddress(addr)
}

// V3MiniAddress returns the configured V3FlashMini contract address, or the
// zero address if the mini path is disabled.
func (e *Executor) V3MiniAddress() common.Address { return e.v3MiniAddr }

// SetV4MiniAddress configures the V4Mini contract address. Empty/zero
// disables the V4-mini path — V4-only cycles will then route to the generic
// executor and continue to revert on native-ETH legs and active-hook pools.
func (e *Executor) SetV4MiniAddress(addr string) {
	if addr == "" {
		e.v4MiniAddr = common.Address{}
		return
	}
	e.v4MiniAddr = common.HexToAddress(addr)
}

// V4MiniAddress returns the configured V4Mini contract address, or the zero
// address if the V4-mini path is disabled.
func (e *Executor) V4MiniAddress() common.Address { return e.v4MiniAddr }

// SetMixedV3V4Address configures the MixedV3V4Executor address. Empty/zero
// disables the mixed path — mixed V4+V3 cycles will then route to the
// generic executor and continue to revert via the buggy unlockCallback.
func (e *Executor) SetMixedV3V4Address(addr string) {
	if addr == "" {
		e.mixedV3V4Addr = common.Address{}
		return
	}
	e.mixedV3V4Addr = common.HexToAddress(addr)
}

// MixedV3V4Address returns the configured MixedV3V4Executor address, or the
// zero address if the mixed path is disabled.
func (e *Executor) MixedV3V4Address() common.Address { return e.mixedV3V4Addr }

func NewExecutor(client *ethclient.Client, privKeyHex string, contractAddr string, chainID *big.Int, balancerVault string, sequencerRPC string, simulationRPC string) (*Executor, error) {
	privKey, err := crypto.HexToECDSA(strings.TrimPrefix(privKeyHex, "0x"))
	if err != nil {
		return nil, fmt.Errorf("invalid private key: %w", err)
	}

	// Connect directly to the sequencer for tx submission via a custom
	// HTTP transport with aggressive connection pooling — see
	// dialFastSubmitClient for the rationale. Fall back to the main client
	// if this fails — better to submit slowly than not at all.
	if sequencerRPC == "" {
		sequencerRPC = "https://arb1-sequencer.arbitrum.io/rpc"
	}
	submitClient, err := dialFastSubmitClient(context.Background(), sequencerRPC)
	if err != nil {
		log.Printf("[executor] warning: could not connect to sequencer RPC (%v), falling back to main client", err)
		submitClient = client
	} else {
		log.Printf("[executor] submit client connected to sequencer: %s (fast HTTP pool)", sequencerRPC)
	}

	// Connect to dedicated simulation RPC for eth_call — keeps simulation traffic off
	// the live swap-tracking RPC to avoid rate-limit failures on that connection.
	// Uses DialHTTP2 so concurrent eth_call dry-runs multiplex over a single
	// TLS connection (Chainstack negotiates h2 via ALPN).
	simClient := client // fallback: use main client if no dedicated sim RPC configured
	if simulationRPC != "" {
		sc, err := DialHTTP2(simulationRPC)
		if err != nil {
			log.Printf("[executor] warning: could not connect to simulation RPC (%v), falling back to main client", err)
		} else {
			simClient = sc
			log.Printf("[executor] simulation client connected to %s (HTTP/2)", simulationRPC)
		}
	}

	pub := privKey.Public().(*ecdsa.PublicKey)
	return &Executor{
		client:        client,
		submitClient:  submitClient,
		simClient:     simClient,
		privateKey:    privKey,
		address:       crypto.PubkeyToAddress(*pub),
		contractAddr:  common.HexToAddress(contractAddr),
		chainID:       chainID,
		balancerVault: common.HexToAddress(balancerVault),
		vaultBalCache: make(map[common.Address]*big.Int),
	}, nil
}

// vaultBalance returns the Balancer vault's balance of tokenAddr.
// Uses an in-memory cache that expires when the block number changes.
// Returns nil on error (caller should proceed without cap).
func (e *Executor) vaultBalance(ctx context.Context, tokenAddr common.Address) *big.Int {
	if e.balancerVault == (common.Address{}) {
		return nil
	}

	// Cache hit: always return cached value regardless of age. A background
	// goroutine (see startVaultCacheRefresher) refreshes all borrowable
	// tokens every 20s, so the cache is guaranteed warm for any token in
	// the borrowable set. The only cold-miss path is the first-ever query
	// for a token that just became borrowable — extremely rare.
	//
	// Vault balances only change on flash-loan execution or deposit/withdraw,
	// both rare events. The 95% cap in Submit absorbs minor drift.
	e.vaultBalMu.RLock()
	if cached, ok := e.vaultBalCache[tokenAddr]; ok {
		e.vaultBalMu.RUnlock()
		return cached
	}
	e.vaultBalMu.RUnlock()

	// Cache miss: fetch from chain (synchronous, ~20-40ms). This path
	// should only fire on the very first Submit for a token — subsequent
	// Submits hit the warm cache.
	bal := e.fetchVaultBalance(ctx, tokenAddr)
	if bal != nil {
		e.vaultBalMu.Lock()
		e.vaultBalCache[tokenAddr] = bal
		e.vaultBalFetchedAt = time.Now()
		e.vaultBalMu.Unlock()
	}
	return bal
}

// StartVaultCacheRefresher launches a background goroutine that re-fetches
// vault balances for all given tokens every `interval`. Called from Bot.Run
// with the borrowable token list. Ensures the vault cache is always warm so
// Submit never pays a synchronous RPC cost on the hot path.
func (e *Executor) StartVaultCacheRefresher(ctx context.Context, getTokens func() []string, interval time.Duration) {
	go func() {
		// Warm immediately at startup (ticker only fires AFTER the first
		// interval elapses — we don't want the first 20s of bot operation
		// to hit cold cache).
		e.refreshVaultCacheOnce(ctx, getTokens())

		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				e.refreshVaultCacheOnce(ctx, getTokens())
			}
		}
	}()
}

// refreshVaultCacheOnce fetches vault balances for all given tokens and
// stores them in the cache. Logs a summary line every 5 minutes so we can
// verify the refresher is firing without spamming the log.
//
// Note: this is the sequential path, kept as a startup warm-up helper. The
// hot path is refreshVaultCacheBatched (called on every new block header).
func (e *Executor) refreshVaultCacheOnce(ctx context.Context, tokens []string) {
	if len(tokens) == 0 {
		return
	}
	start := time.Now()
	warmed := 0
	for _, addr := range tokens {
		tok := common.HexToAddress(addr)
		bal := e.fetchVaultBalance(ctx, tok)
		if bal != nil {
			e.vaultBalMu.Lock()
			e.vaultBalCache[tok] = bal
			e.vaultBalFetchedAt = time.Now()
			e.vaultBalMu.Unlock()
			warmed++
		}
	}
	// Rate-limit the log line to once every 5 minutes.
	if time.Since(e.lastVaultRefreshLog) >= 5*time.Minute {
		log.Printf("[executor] vault cache refreshed: %d/%d tokens in %v", warmed, len(tokens), time.Since(start))
		e.lastVaultRefreshLog = time.Now()
	}
}

// refreshVaultCacheBatched reads all borrowable token vault balances in a
// single Multicall2 batch (1 RPC call) and updates the cache atomically.
// Called on every new block header from onNewBlock. Typical cost per block:
// 1 eth_call, ~10-20 ms, ~70 sub-balance reads. Staleness bound: 1 block
// (~250 ms) instead of the 20 s of the legacy ticker-based refresher.
//
// Why batched multicall: the previous refreshVaultCacheOnce made N sequential
// eth_calls (one per token). On 70 tokens at ~11 ms RTT that's ~770 ms, which
// is longer than an Arbitrum block — the refresh couldn't keep up with block
// cadence. Multicall bundles all balanceOf reads into one eth_call that
// returns every balance at the same block snapshot.
func (e *Executor) refreshVaultCacheBatched(ctx context.Context, tokens []string) {
	if len(tokens) == 0 || e.balancerVault == (common.Address{}) {
		return
	}
	data, err := erc20BalanceOfABI.Pack("balanceOf", e.balancerVault)
	if err != nil {
		return
	}
	targets := make([]common.Address, len(tokens))
	calls := make([]call, len(tokens))
	for i, addr := range tokens {
		targets[i] = common.HexToAddress(addr)
		calls[i] = call{Target: targets[i], CallData: data}
	}
	start := time.Now()
	results, err := doMulticall(ctx, e.client, calls, nil)
	if err != nil {
		// Swallow errors — onNewBlock will try again on the next block. The
		// previous cached values remain usable as a stale fallback.
		return
	}
	fresh := make(map[common.Address]*big.Int, len(results))
	for i, r := range results {
		if !r.Success || len(r.ReturnData) < 32 {
			continue
		}
		fresh[targets[i]] = new(big.Int).SetBytes(r.ReturnData[:32])
	}
	e.vaultBalMu.Lock()
	for k, v := range fresh {
		e.vaultBalCache[k] = v
	}
	e.vaultBalFetchedAt = time.Now()
	e.vaultBalMu.Unlock()
	if time.Since(e.lastVaultRefreshLog) >= 5*time.Minute {
		log.Printf("[executor] vault cache refreshed (block-tick batched): %d/%d tokens in %v", len(fresh), len(tokens), time.Since(start))
		e.lastVaultRefreshLog = time.Now()
	}
}

// SetVaultRefreshTokens registers the borrowable-token provider used by the
// block-tick vault cache refresh. Must be called from Bot.Run after the
// borrowable refresh loop has identified which tokens Balancer can lend.
// Passing nil disables the block-tick refresh.
func (e *Executor) SetVaultRefreshTokens(fn func() []string) {
	e.vaultRefreshTokens = fn
}

// WarmVaultBalances pre-fetches vault balances for the given token addresses
// so the first Submit call per token hits a warm cache instead of paying ~60ms
// for a synchronous RPC round-trip. Called from Bot.Run after the borrowable
// refresh loop has identified which tokens Balancer can flash-loan.
func (e *Executor) WarmVaultBalances(ctx context.Context, tokenAddrs []string) {
	if e.balancerVault == (common.Address{}) {
		return
	}
	warmed := 0
	for _, addr := range tokenAddrs {
		tok := common.HexToAddress(addr)
		bal := e.fetchVaultBalance(ctx, tok)
		if bal != nil && bal.Sign() > 0 {
			e.vaultBalMu.Lock()
			e.vaultBalCache[tok] = bal
			e.vaultBalFetchedAt = time.Now()
			e.vaultBalMu.Unlock()
			warmed++
		}
	}
	if warmed > 0 {
		log.Printf("[executor] vault cache warmed: %d/%d borrowable tokens", warmed, len(tokenAddrs))
	}
}

// Warmup runs background goroutines that keep caches fresh:
//  1. Subscribes to new block headers — on each block, invalidates vault balance
//     cache and refreshes baseFee
//  2. Pre-fetches the nonce once at startup, then maintains it locally (Submit
//     atomically increments). External nonce drift is detected and re-synced.
//
// This eliminates 30-50ms of RPC round-trips per trade submission.
func (e *Executor) Warmup(ctx context.Context) {
	// Pre-fetch initial nonce (one-time, ~10ms)
	if nonce, err := e.client.PendingNonceAt(ctx, e.address); err == nil {
		e.nonceVal.Store(nonce)
		e.nonceLoaded.Store(true)
		log.Printf("[executor] warmup: nonce=%d", nonce)
	} else {
		log.Printf("[executor] warmup: nonce fetch failed: %v", err)
	}

	// Pre-fetch initial baseFee (one-time, ~20ms)
	if head, err := e.client.HeaderByNumber(ctx, nil); err == nil && head.BaseFee != nil {
		e.baseFeeMu.Lock()
		e.baseFeeCache = new(big.Int).Set(head.BaseFee)
		e.baseFeeMu.Unlock()
	}

	// Startup connection volley: open several connections to the submit RPC
	// concurrently so the first real submission finds a populated pool
	// instead of paying TLS handshake cost on a cold connection. Observed
	// on trade 152's burst: submissions 1-7 each paid 340-512ms for TLS
	// handshake because the default HTTP transport's 2-connection pool was
	// exhausted under concurrent load. With the fast submit transport
	// (MaxIdleConnsPerHost=20) this volley pre-warms 5 idle connections.
	go func() {
		var wg sync.WaitGroup
		for i := 0; i < 5; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				// Use a short-lived context — if the sequencer is unreachable,
				// we'd rather fail fast than hold the warmup goroutine.
				volleyCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
				defer cancel()
				if _, err := e.submitClient.ChainID(volleyCtx); err != nil {
					log.Printf("[executor] warmup volley ping failed (non-fatal): %v", err)
				}
			}()
		}
		wg.Wait()
		log.Printf("[executor] submit client: pre-warmed connection pool with 5 concurrent pings")
	}()

	// Keep the submit RPC's TLS connections warm. Log a sample of per-ping
	// latency once per minute so we can tell at a glance whether the pool
	// is actually warm (fast pings) or repeatedly hitting cold handshakes
	// (slow pings — means the previous connection was closed between pings).
	go func() {
		ticker := time.NewTicker(2 * time.Second)
		defer ticker.Stop()
		var pingCount int
		var sumMicros int64
		var maxMicros int64
		var lastLog time.Time
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				pingCtx, cancel := context.WithTimeout(ctx, 1*time.Second)
				tPing := time.Now()
				_, err := e.submitClient.ChainID(pingCtx)
				cancel()
				dt := time.Since(tPing).Microseconds()
				pingCount++
				sumMicros += dt
				if dt > maxMicros {
					maxMicros = dt
				}
				// Submit-only sequencer endpoints (e.g. arb1-sequencer.arbitrum.io/rpc)
				// reject eth_chainId with "method not available"; the TCP+TLS+JSON
				// round-trip still happened, so the latency measurement is valid —
				// just suppress the error log to avoid spamming.
				if err != nil && !strings.Contains(err.Error(), "does not exist") &&
					!strings.Contains(err.Error(), "not available") &&
					!strings.Contains(err.Error(), "not found") {
					log.Printf("[executor] submit-ping error: %v (took %dus)", err, dt)
				}
				if time.Since(lastLog) >= 60*time.Second {
					avg := int64(0)
					if pingCount > 0 {
						avg = sumMicros / int64(pingCount)
					}
					log.Printf("[executor] submit-ping stats: n=%d avg=%dus max=%dus", pingCount, avg, maxMicros)
					pingCount = 0
					sumMicros = 0
					maxMicros = 0
					lastLog = time.Now()
				}
			}
		}
	}()

	// Background: subscribe to new block headers and refresh caches per block
	go func() {
		headers := make(chan *types.Header, 8)
		sub, err := e.client.SubscribeNewHead(ctx, headers)
		if err != nil {
			log.Printf("[executor] warmup: header subscription failed: %v — falling back to polling", err)
			// Fallback: poll every 250ms (Arbitrum block time)
			ticker := time.NewTicker(250 * time.Millisecond)
			defer ticker.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
					e.refreshCachesFromHead(ctx)
				}
			}
		}
		defer sub.Unsubscribe()
		for {
			select {
			case <-ctx.Done():
				return
			case err := <-sub.Err():
				log.Printf("[executor] header sub error: %v — re-subscribing in 2s", err)
				time.Sleep(2 * time.Second)
				newSub, err2 := e.client.SubscribeNewHead(ctx, headers)
				if err2 != nil {
					log.Printf("[executor] re-subscribe failed: %v", err2)
					return
				}
				sub = newSub
			case h := <-headers:
				e.onNewBlock(h)
			}
		}
	}()
}

// onNewBlock is called for each new block header. Does two things:
//  1. Refresh baseFee from the header (no RPC needed — already in the header).
//  2. Kick a batched multicall that refreshes vault balances for all
//     borrowable tokens. Runs in a goroutine so the block-header receive
//     loop never blocks; an in-flight guard drops the request if a previous
//     refresh is still running (so a slow RPC can't queue refreshes up).
//
// Design note: the submit path still reads from the cache instantly — the
// refresh runs out-of-band and updates the cache atomically. This gives us
// submission-path latency of 0 with a freshness bound of 1 block (~250 ms).
// The old approach had a 20 s staleness window driven by a ticker, which is
// what produced the BAL#528 revert on trade 124 (AAVE vault balance dropped
// between the cache fetch and the submission).
func (e *Executor) onNewBlock(h *types.Header) {
	if h == nil {
		return
	}
	// Refresh baseFee from header (no extra RPC needed — already in the header)
	if h.BaseFee != nil {
		e.baseFeeMu.Lock()
		e.baseFeeCache = new(big.Int).Set(h.BaseFee)
		e.baseFeeMu.Unlock()
	}
	// Block-tick vault balance refresh. Fire-and-forget goroutine, in-flight
	// guarded so overlapping block arrivals coalesce into a single refresh.
	if e.vaultRefreshTokens != nil && e.vaultRefreshInFlight.CompareAndSwap(false, true) {
		go func() {
			defer e.vaultRefreshInFlight.Store(false)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			e.refreshVaultCacheBatched(ctx, e.vaultRefreshTokens())
		}()
	}
}

// refreshCachesFromHead is the polling fallback when WSS subscription is unavailable.
func (e *Executor) refreshCachesFromHead(ctx context.Context) {
	head, err := e.client.HeaderByNumber(ctx, nil)
	if err != nil {
		return
	}
	e.onNewBlock(head)
}

// nextNonce returns the cached nonce and atomically increments it.
// If the cache wasn't loaded, falls back to a synchronous PendingNonceAt call.
func (e *Executor) nextNonce(ctx context.Context) (uint64, error) {
	if e.nonceLoaded.Load() {
		return e.nonceVal.Add(1) - 1, nil
	}
	// Cold path: warmup hasn't run yet — fetch synchronously
	nonce, err := e.client.PendingNonceAt(ctx, e.address)
	if err != nil {
		return 0, err
	}
	e.nonceVal.Store(nonce + 1)
	e.nonceLoaded.Store(true)
	return nonce, nil
}

// resyncNonce forces a re-sync of the cached nonce from chain. Call this after
// a "nonce too low" send error to recover from drift.
func (e *Executor) resyncNonce(ctx context.Context) {
	if nonce, err := e.client.PendingNonceAt(ctx, e.address); err == nil {
		e.nonceVal.Store(nonce)
		log.Printf("[executor] nonce re-synced to %d", nonce)
	}
}

// cachedBaseFee returns the cached baseFee, or nil if not yet loaded.
func (e *Executor) cachedBaseFee() *big.Int {
	e.baseFeeMu.RLock()
	defer e.baseFeeMu.RUnlock()
	if e.baseFeeCache == nil {
		return nil
	}
	return new(big.Int).Set(e.baseFeeCache)
}

// fetchVaultBalance does the actual on-chain balanceOf call (no cache).
func (e *Executor) fetchVaultBalance(ctx context.Context, tokenAddr common.Address) *big.Int {
	data, err := erc20BalanceOfABI.Pack("balanceOf", e.balancerVault)
	if err != nil {
		return nil
	}
	res, err := e.client.CallContract(ctx, ethereum.CallMsg{To: &tokenAddr, Data: data}, nil)
	if err != nil {
		return nil
	}
	vals, err := erc20BalanceOfABI.Unpack("balanceOf", res)
	if err != nil || len(vals) == 0 {
		return nil
	}
	bal, _ := vals[0].(*big.Int)
	return bal
}

// DryRun builds the full calldata (buildHops + encode) and runs an eth_call
// without broadcasting. Returns (nil) if the on-chain execution would succeed,
// or an error with the revert reason. Used in pause_trading mode to validate
// sim accuracy: if the Go sim says +$2 but the eth_call reverts, the sim is
// wrong and we can log the discrepancy.
func (e *Executor) DryRun(ctx context.Context, cycle Cycle, amountIn *big.Int, flashSel FlashSelection, slippageBps int64, minProfitNative *big.Int, useV3Mini, useV4Mini, useMixedV3V4 bool) error {
	if len(cycle.Edges) == 0 {
		return fmt.Errorf("empty cycle")
	}
	borrowToken := common.HexToAddress(cycle.Edges[0].TokenIn.Address)
	useMini := useV3Mini && e.v3MiniAddr != (common.Address{}) && qualifyForV3Mini(cycle, flashSel)
	useV4 := !useMini && useV4Mini && e.v4MiniAddr != (common.Address{}) && qualifyForV4Mini(cycle, flashSel)
	useMix := !useMini && !useV4 && useMixedV3V4 && e.mixedV3V4Addr != (common.Address{}) && qualifyForMixedV3V4(cycle, flashSel)
	targetAddr := e.contractAddr
	switch {
	case useMini:
		targetAddr = e.v3MiniAddr
	case useV4:
		targetAddr = e.v4MiniAddr
	case useMix:
		targetAddr = e.mixedV3V4Addr
	}

	var data []byte
	switch {
	case useMini:
		packed, err := packV3MiniHops(cycle, amountIn, slippageBps)
		if err != nil {
			return fmt.Errorf("pack mini: %w", err)
		}
		isToken0 := strings.EqualFold(cycle.Edges[0].TokenIn.Address, flashSel.FlashPoolToken0.Hex())
		data, err = executeV3MiniABI.Pack("flash", flashSel.PoolAddr, borrowToken, amountIn, isToken0, packed)
		if err != nil {
			return fmt.Errorf("pack flash: %w", err)
		}
	case useV4:
		packed, err := packV4MiniHops(cycle)
		if err != nil {
			return fmt.Errorf("pack v4mini: %w", err)
		}
		isToken0 := strings.EqualFold(cycle.Edges[0].TokenIn.Address, flashSel.FlashPoolToken0.Hex())
		data, err = executeV4MiniABI.Pack("flash", flashSel.PoolAddr, borrowToken, amountIn, isToken0, packed)
		if err != nil {
			return fmt.Errorf("pack v4 flash: %w", err)
		}
	case useMix:
		packed, err := packMixedV3V4Hops(cycle)
		if err != nil {
			return fmt.Errorf("pack mixedv3v4: %w", err)
		}
		isToken0 := strings.EqualFold(cycle.Edges[0].TokenIn.Address, flashSel.FlashPoolToken0.Hex())
		data, err = executeMixedV3V4ABI.Pack("flash", flashSel.PoolAddr, borrowToken, amountIn, isToken0, packed)
		if err != nil {
			return fmt.Errorf("pack mixedv3v4 flash: %w", err)
		}
	default:
		hops, err := buildHops(cycle, amountIn, slippageBps)
		if err != nil {
			return fmt.Errorf("buildHops: %w", err)
		}
		hopData, err := encodeHops(hops)
		if err != nil {
			return fmt.Errorf("encode: %w", err)
		}
		switch flashSel.Source {
		case FlashBalancer:
			tokens := []common.Address{borrowToken}
			amounts := []*big.Int{amountIn}
			data, err = executeABI.Pack("execute", tokens, amounts, hopData, minProfitNative)
		case FlashV3Pool:
			data, err = executeV3FlashABI.Pack("executeV3Flash", flashSel.PoolAddr, borrowToken, amountIn, hopData, minProfitNative)
		case FlashAave:
			data, err = executeAaveFlashABI.Pack("executeAaveFlash", borrowToken, amountIn, hopData, minProfitNative)
		}
		if err != nil {
			return fmt.Errorf("pack: %w", err)
		}
	}

	msg := ethereum.CallMsg{
		From: e.address,
		To:   &targetAddr,
		Data: data,
	}
	_, err := e.simClient.CallContract(ctx, msg, nil)
	return err
}

// simulate does a dry-run eth_call of the execute() calldata against the contract.
// Returns nil if the call would succeed, or an error with the revert reason.
func (e *Executor) simulate(ctx context.Context, data []byte) error {
	msg := ethereum.CallMsg{
		From: e.address,
		To:   &e.contractAddr,
		Data: data,
	}
	_, err := e.simClient.CallContract(ctx, msg, nil)
	return err
}

// refreshCyclePools does a single batched multicall to pull fresh
// slot0/globalState + liquidity for every unique pool in the cycle, then
// updates the snapshot Pool instances in place. Called from Submit right
// before buildHops to guarantee the sim runs against the freshest possible
// state, eliminating phantom cycles caused by event-stream lag during
// burst windows.
//
// Designed to be cheap: one multicall, ~3-6 pools, ~15-25 ms round-trip
// to the simClient. Only runs on cycles that have cleared all pre-submit
// gates (score, lpfloor, tick_stale, optimal, flash) — not every eval.
// Errors are logged and swallowed: if the refresh fails (RPC down,
// timeout), we fall through to buildHops with the snapshot state we had.
// Uses the existing BatchUpdatePools infrastructure so every DEX type is
// covered — UniV3/V4, PancakeV3, Camelot/Zyber V3 (Algebra), Curve,
// Balancer weighted — with the correct ABI per DEX handled upstream.
func (e *Executor) refreshCyclePools(ctx context.Context, cycle Cycle) error {
	if len(cycle.Edges) == 0 {
		return nil
	}
	// Dedupe snapshot pools by address — a cycle can visit the same pool
	// twice (A/B then B/A), and BatchUpdatePools would otherwise issue
	// duplicate calls for it.
	seen := make(map[string]*Pool, len(cycle.Edges))
	for _, edge := range cycle.Edges {
		key := strings.ToLower(edge.Pool.Address)
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = edge.Pool
	}
	pools := make([]*Pool, 0, len(seen))
	for _, p := range seen {
		pools = append(pools, p)
	}
	// Tight timeout so a hung RPC can't stall the submission path. If
	// refresh runs longer than 400 ms we're better off skipping it and
	// falling through with the cached state — buildHops will still detect
	// obviously-broken cycles via its last-hop short check.
	rctx, cancel := context.WithTimeout(ctx, 400*time.Millisecond)
	defer cancel()
	return BatchUpdatePools(rctx, e.simClient, pools)
}

// SubmitTiming holds microsecond-precision durations for each sub-step of Submit.
type SubmitTiming struct {
	Vault     time.Duration
	Refresh   time.Duration // pre-submission pool state refresh (multicall)
	Hops      time.Duration
	Encode    time.Duration
	EthCall   time.Duration
	// Broadcast is the total of nonce fetch + header fetch + sign + send.
	// See the sub-fields below for the per-step breakdown — used to diagnose
	// slow broadcasts on the submission path (trade 142 logged broadcast=348ms
	// when the network benchmark for Ohio→arb1 RTT is ~11ms).
	Broadcast   time.Duration
	BcastNonce  time.Duration // PendingNonceAt (usually 0 — cached nonce path)
	BcastHeader time.Duration // HeaderByNumber (usually 0 — cached baseFee path)
	BcastSign   time.Duration // types.SignTx (CPU-only, should be ~0.1ms)
	BcastSend   time.Duration // submitClient.SendTransaction (the actual RPC round-trip)
	// TargetAddr is the executor contract actually used for this submission.
	// Differs from the configured default when the cycle qualified for
	// V3FlashMini routing (e.v3MiniAddr) — callers persist this into
	// our_trades.executor so the dashboard attributes each trade to the
	// actual contract that executed it, not just the default configured one.
	TargetAddr common.Address
}

// Submit encodes and broadcasts an arbitrage transaction for the given cycle.
// flashSel: which flash loan source to use (Balancer/V3/Aave) and its fee.
// slippageBps: per-hop slippage tolerance (e.g. 50 = 0.5%).
// minProfitNative: minimum profit in token base units — the contract reverts if not met.
// Submit builds and broadcasts an arbitrage transaction.
//
// useV3Mini: when true, the tx is routed to the V3FlashMini contract using
// its packed-hops calldata format. Only valid when qualifyForV3Mini(cycle,
// flashSel) is true and e.v3MiniAddr is non-zero. Submit falls back to
// the full executor path when either check fails.
func (e *Executor) Submit(ctx context.Context, cycle Cycle, amountIn *big.Int, flashSel FlashSelection, slippageBps int64, minProfitNative *big.Int, skipSim bool, useV3Mini, useV4Mini, useMixedV3V4 bool) (string, *SubmitTiming, error) {
	var st SubmitTiming
	if len(cycle.Edges) == 0 {
		return "", &st, fmt.Errorf("empty cycle")
	}

	borrowToken := common.HexToAddress(cycle.Edges[0].TokenIn.Address)

	// 5a: source-specific liquidity check + cap.
	t0 := time.Now()
	switch flashSel.Source {
	case FlashBalancer:
		// Vault liquidity check — BAL#528 prevention.
		vaultBal := e.vaultBalance(ctx, borrowToken)
		if vaultBal == nil {
			st.Vault = time.Since(t0)
			return "", &st, fmt.Errorf("vault balance lookup failed for %s", cycle.Edges[0].TokenIn.Symbol)
		}
		if vaultBal.Sign() == 0 {
			st.Vault = time.Since(t0)
			return "", &st, fmt.Errorf("borrow token %s not held by Balancer Vault (would BAL#528)", cycle.Edges[0].TokenIn.Symbol)
		}
		cap95 := new(big.Int).Mul(vaultBal, big.NewInt(95))
		cap95.Div(cap95, big.NewInt(100))
		if amountIn.Cmp(cap95) > 0 {
			log.Printf("[executor] capping amountIn from %s to 95%% vault balance (%s) for %s",
				amountIn, cap95, cycle.Edges[0].TokenIn.Symbol)
			amountIn = cap95
		}
	case FlashV3Pool:
		// V3 pool flash — no vault balance check needed, pool has its own
		// liquidity. The flash() call reverts if amount > pool reserves.
	case FlashAave:
		// Aave flash — no balance check, Aave handles it.
	}
	st.Vault = time.Since(t0)

	// 5a2: pre-submission pool state refresh — DISABLED.
	//
	// History: this step originally did a multicall to refresh the snapshot
	// pool state right before buildHops, to prevent phantom-cycle
	// submissions from stale data. But we discovered (2026-04-14, after
	// looking at 200 "cycle unprofitable" rejects in arb_observations)
	// that refreshing the snapshot in place BETWEEN optimalAmountIn and
	// buildHops creates a disagreement: the optimizer ran on the old
	// snapshot, found a profitable size, and stored its result; then the
	// refresh mutated the snapshot's sqrtPriceX96/liquidity to fresh chain
	// values; then buildHops re-simulated on the NEW snapshot and found
	// the cycle unprofitable. Both steps were "correct" in isolation, but
	// disagreed on the state they ran against. Net effect: ~59% of
	// profitable observations ($3-$202 each) rejected by buildHops.
	//
	// The fix: keep the snapshot frozen across optimalAmountIn → buildHops
	// so they see identical state. If the bot is operating on stale data
	// the eth_call simulate step below catches real-state reverts at the
	// tx-to-chain boundary. We lose the ability to pre-emptively skip
	// "we'd have gotten a stale-revert if we'd submitted" scenarios, but
	// those are a tiny fraction of the rejects compared to the real
	// profitable cycles the refresh was destroying.
	st.Refresh = 0

	// Routing: when useV3Mini is requested, the tx target is the mini
	// contract and calldata is packed differently (see packV3MiniHops).
	// Fall back to the full executor if the mini address isn't configured
	// or the cycle doesn't actually qualify — paranoid check in case the
	// caller's eligibility evaluation disagrees with ours.
	useMini := useV3Mini && e.v3MiniAddr != (common.Address{}) && qualifyForV3Mini(cycle, flashSel)
	useV4 := !useMini && useV4Mini && e.v4MiniAddr != (common.Address{}) && qualifyForV4Mini(cycle, flashSel)
	useMix := !useMini && !useV4 && useMixedV3V4 && e.mixedV3V4Addr != (common.Address{}) && qualifyForMixedV3V4(cycle, flashSel)
	targetAddr := e.contractAddr
	switch {
	case useMini:
		targetAddr = e.v3MiniAddr
	case useV4:
		targetAddr = e.v4MiniAddr
	case useMix:
		targetAddr = e.mixedV3V4Addr
	}
	st.TargetAddr = targetAddr

	// 5b: buildHops (and for mini, also packV3MiniHops in the encode step below).
	// Full-executor codepath still needs `hops` for its abi-encoded Hop[]
	// layout; mini uses its own packed bytes and doesn't rely on buildHops'
	// Hop structs. We skip the full buildHops when routing to mini to save
	// the tiny CPU cost and avoid its "last hop short" check tripping on
	// cycles where the mini's own per-hop amountOutMin check will catch the
	// same condition.
	t0 = time.Now()
	var hops []Hop
	if !useMini && !useV4 && !useMix {
		var err error
		hops, err = buildHops(cycle, amountIn, slippageBps)
		if err != nil {
			st.Hops = time.Since(t0)
			return "", &st, fmt.Errorf("build hops: %w", err)
		}
	}
	st.Hops = time.Since(t0)

	// 5c: encode + ABI pack (source-specific and executor-specific)
	t0 = time.Now()
	var data []byte
	switch {
	case useMini:
		miniHops, perr := packV3MiniHops(cycle, amountIn, slippageBps)
		if perr != nil {
			st.Encode = time.Since(t0)
			return "", &st, fmt.Errorf("packV3MiniHops: %w", perr)
		}
		isToken0 := strings.EqualFold(cycle.Edges[0].TokenIn.Address, flashSel.FlashPoolToken0.Hex())
		var perr2 error
		data, perr2 = executeV3MiniABI.Pack("flash", flashSel.PoolAddr, borrowToken, amountIn, isToken0, miniHops)
		if perr2 != nil {
			st.Encode = time.Since(t0)
			return "", &st, fmt.Errorf("pack v3mini: %w", perr2)
		}
	case useV4:
		v4Hops, perr := packV4MiniHops(cycle)
		if perr != nil {
			st.Encode = time.Since(t0)
			return "", &st, fmt.Errorf("packV4MiniHops: %w", perr)
		}
		isToken0 := strings.EqualFold(cycle.Edges[0].TokenIn.Address, flashSel.FlashPoolToken0.Hex())
		var perr2 error
		data, perr2 = executeV4MiniABI.Pack("flash", flashSel.PoolAddr, borrowToken, amountIn, isToken0, v4Hops)
		if perr2 != nil {
			st.Encode = time.Since(t0)
			return "", &st, fmt.Errorf("pack v4mini: %w", perr2)
		}
	case useMix:
		mixHops, perr := packMixedV3V4Hops(cycle)
		if perr != nil {
			st.Encode = time.Since(t0)
			return "", &st, fmt.Errorf("packMixedV3V4Hops: %w", perr)
		}
		isToken0 := strings.EqualFold(cycle.Edges[0].TokenIn.Address, flashSel.FlashPoolToken0.Hex())
		var perr2 error
		data, perr2 = executeMixedV3V4ABI.Pack("flash", flashSel.PoolAddr, borrowToken, amountIn, isToken0, mixHops)
		if perr2 != nil {
			st.Encode = time.Since(t0)
			return "", &st, fmt.Errorf("pack mixedv3v4: %w", perr2)
		}
	default:
		hopData, perr := encodeHops(hops)
		if perr != nil {
			st.Encode = time.Since(t0)
			return "", &st, fmt.Errorf("encode hops: %w", perr)
		}
		var perr2 error
		switch flashSel.Source {
		case FlashBalancer:
			tokens := []common.Address{borrowToken}
			amounts := []*big.Int{amountIn}
			data, perr2 = executeABI.Pack("execute", tokens, amounts, hopData, minProfitNative)
		case FlashV3Pool:
			data, perr2 = executeV3FlashABI.Pack("executeV3Flash", flashSel.PoolAddr, borrowToken, amountIn, hopData, minProfitNative)
		case FlashAave:
			data, perr2 = executeAaveFlashABI.Pack("executeAaveFlash", borrowToken, amountIn, hopData, minProfitNative)
		}
		if perr2 != nil {
			st.Encode = time.Since(t0)
			return "", &st, fmt.Errorf("pack %s: %w", flashSel.Source, perr2)
		}
	}
	st.Encode = time.Since(t0)

	// 5d: Dry-run via eth_call before broadcasting. If the tx would revert at current
	// on-chain state (swaps return less than borrowed, slippage, etc.) we catch it
	// here for free instead of paying gas for a failed tx.
	// skipSim=true bypasses this for high-profit cycles where the cost of a missed
	// opportunity exceeds the cost of an occasional failed tx.
	t0 = time.Now()
	if !skipSim {
		if err := e.simulate(ctx, data); err != nil {
			st.EthCall = time.Since(t0)
			return "", &st, fmt.Errorf("simulation: %w", err)
		}
	}
	st.EthCall = time.Since(t0)

	// 5e: nonce + header + sign + broadcast
	bcastStart := time.Now()

	// Step 5e.1: Use cached nonce (incremented atomically). Falls back to
	// PendingNonceAt if Warmup hasn't run yet — that is the only slow path.
	tSub := time.Now()
	nonce, err := e.nextNonce(ctx)
	st.BcastNonce = time.Since(tSub)
	if err != nil {
		st.Broadcast = time.Since(bcastStart)
		return "", &st, fmt.Errorf("nonce: %w", err)
	}

	// Step 5e.2: Use cached baseFee (refreshed every block by Warmup goroutine).
	// Falls back to a synchronous HeaderByNumber if cache isn't loaded yet.
	tSub = time.Now()
	baseFee := e.cachedBaseFee()
	if baseFee == nil {
		head, err := e.client.HeaderByNumber(ctx, nil)
		if err != nil {
			st.BcastHeader = time.Since(tSub)
			st.Broadcast = time.Since(bcastStart)
			return "", &st, fmt.Errorf("header: %w", err)
		}
		baseFee = head.BaseFee
	}
	st.BcastHeader = time.Since(tSub)
	// tip = 1 wei (Arbitrum doesn't need tips, sequencer is FCFS)
	tip := big.NewInt(1)
	// maxFeePerGas = 2× baseFee + tip (generous headroom)
	maxFee := new(big.Int).Add(new(big.Int).Mul(baseFee, big.NewInt(2)), tip)

	tx := types.NewTx(&types.DynamicFeeTx{
		ChainID:   e.chainID,
		Nonce:     nonce,
		To:        &targetAddr,
		Gas:       1_200_000,
		GasTipCap: tip,
		GasFeeCap: maxFee,
		Data:      data,
	})

	// Step 5e.3: sign (CPU-only, ~0.1ms)
	tSub = time.Now()
	signed, err := types.SignTx(tx, types.NewLondonSigner(e.chainID), e.privateKey)
	st.BcastSign = time.Since(tSub)
	if err != nil {
		st.Broadcast = time.Since(bcastStart)
		return "", &st, fmt.Errorf("sign: %w", err)
	}

	// Step 5e.4: SendTransaction — the actual submit-RPC round-trip. This
	// is expected to be ~11ms Ohio→arb1 on a warm TLS connection; anything
	// higher indicates a TCP/TLS handshake renegotiation or provider-side
	// queueing. Isolating this sub-timing is the whole point of the
	// BcastNonce/Header/Sign/Send split.
	tSub = time.Now()
	err = e.submitClient.SendTransaction(ctx, signed)
	st.BcastSend = time.Since(tSub)
	if err != nil {
		st.Broadcast = time.Since(bcastStart)
		// Re-sync nonce on drift errors so the next Submit recovers
		errStr := err.Error()
		if strings.Contains(errStr, "nonce too low") || strings.Contains(errStr, "nonce too high") || strings.Contains(errStr, "already known") {
			e.resyncNonce(ctx)
		}
		return "", &st, fmt.Errorf("send: %w", err)
	}
	st.Broadcast = time.Since(bcastStart)

	txHash := signed.Hash().Hex()
	log.Printf("[executor] submitted tx=%s borrow=%s path=%s", txHash, amountIn, cycle.Path())
	return txHash, &st, nil
}

// HopForensic captures per-hop diagnostic data for a reverted transaction.
type HopForensic struct {
	Idx          int    `json:"idx"`
	DEX          string `json:"dex"`
	Pool         string `json:"pool"`
	TokenIn      string `json:"token_in"`
	TokenInDec   int    `json:"token_in_decimals"`
	TokenOut     string `json:"token_out"`
	TokenOutDec  int    `json:"token_out_decimals"`
	FeeBps       uint32 `json:"fee_bps"`
	AmountInRaw  string `json:"amount_in_raw"`
	GoSimOutRaw  string `json:"go_sim_out_raw"`
	AmtOutMinRaw string `json:"amount_out_min_raw"`
	// GoSimFails is true when the Go simulator predicts this hop's output is below
	// its amountOutMin — i.e. the contract would revert at this hop.
	GoSimFails bool `json:"go_sim_fails"`
	// PoolLastUpdatedUnix is the unix-seconds timestamp stamped on the pool
	// the last time its cached state was refreshed (swap event or multicall).
	// Used post-hoc to correlate revert root cause with pool staleness at
	// the time of Diagnose — if a hop fails with a last_updated older than
	// the submission timestamp, the cycle was simulated against a stale
	// snapshot and the optimizer's profit estimate was phantom.
	PoolLastUpdatedUnix int64 `json:"pool_last_updated_unix,omitempty"`
}

// RevertForensics is the post-mortem report for a reverted arb transaction.
type RevertForensics struct {
	// ReSimError is the eth_call revert reason at time of diagnosis (current on-chain
	// state). Empty means the tx would succeed if re-submitted right now.
	ReSimError string        `json:"re_sim_error"`
	Hops       []HopForensic `json:"hops"`
}

// Diagnose re-simulates a reverted transaction via eth_call (current on-chain state)
// and traces every hop through the Go simulator step-by-step so you can see exactly
// where the model diverges from on-chain reality.
//
// Note: pool state used for the Go-sim trace is the CURRENT live state of each pool
// (pool pointers in cycle.Edges), not the state that existed at submission time.
// A "FAILS" hop with a passing re-sim means the opportunity is open again now.
// A "FAILS" hop with a failing re-sim confirms the root cause is still present.
func (e *Executor) Diagnose(ctx context.Context, cycle Cycle, amountIn *big.Int, slippageBps int64) *RevertForensics {
	var f RevertForensics

	// Attempt the eth_call re-sim. Any failure in the rebuild chain just gets
	// captured in ReSimError — we still fall through to the per-hop Go trace
	// below, which is the forensic we care about most (it shows where the Go
	// simulator first diverges from "enough output to cover the next hop").
	// Pre-fix, an early return here produced `hop_forensics_json.hops = []`
	// on every revert, hiding the exact failing hop for 99% of landed trades.
	borrowToken := common.HexToAddress(cycle.Edges[0].TokenIn.Address)
	if hops, err := buildHops(cycle, amountIn, slippageBps); err != nil {
		f.ReSimError = fmt.Sprintf("[buildHops: %v]", err)
	} else if hopData, err := encodeHops(hops); err != nil {
		f.ReSimError = fmt.Sprintf("[encodeHops: %v]", err)
	} else if data, err := executeABI.Pack("execute",
		[]common.Address{borrowToken},
		[]*big.Int{amountIn},
		hopData,
		big.NewInt(1), // minProfit=1 to isolate "arb failed" vs "profit below minimum"
	); err != nil {
		f.ReSimError = fmt.Sprintf("[pack: %v]", err)
	} else if simErr := e.simulate(ctx, data); simErr != nil {
		f.ReSimError = simErr.Error()
	}

	// Walk each hop through the Go simulator to find where it diverges.
	current := new(big.Int).Set(amountIn)
	for i, edge := range cycle.Edges {
		sim := SimulatorFor(edge.Pool.DEX)
		out := sim.AmountOut(edge.Pool, edge.TokenIn, current)
		if out == nil {
			out = big.NewInt(0)
		}

		slippedOut := new(big.Int).Mul(out, big.NewInt(10_000-slippageBps))
		slippedOut.Div(slippedOut, big.NewInt(10_000))
		if slippedOut.Sign() <= 0 {
			slippedOut = big.NewInt(1)
		}
		aom := new(big.Int).Set(slippedOut)
		if i == len(cycle.Edges)-1 && aom.Cmp(amountIn) < 0 {
			aom = new(big.Int).Set(amountIn)
		}

		f.Hops = append(f.Hops, HopForensic{
			Idx:          i,
			DEX:          edge.Pool.DEX.String(),
			Pool:         edge.Pool.Address,
			TokenIn:      edge.TokenIn.Symbol,
			TokenInDec:   int(edge.TokenIn.Decimals),
			TokenOut:     edge.TokenOut.Symbol,
			TokenOutDec:  int(edge.TokenOut.Decimals),
			FeeBps:       edge.Pool.FeeBps,
			AmountInRaw:  current.String(),
			GoSimOutRaw:  out.String(),
			AmtOutMinRaw: aom.String(),
			GoSimFails:   out.Cmp(aom) < 0,
			PoolLastUpdatedUnix: func() int64 {
				edge.Pool.mu.RLock()
				defer edge.Pool.mu.RUnlock()
				if edge.Pool.LastUpdated.IsZero() {
					return 0
				}
				return edge.Pool.LastUpdated.Unix()
			}(),
		})

		if out.Sign() > 0 {
			current = out
		} else {
			current = big.NewInt(0)
		}
	}

	return &f
}

// CheckReceipt waits for a tx to be mined and logs whether it succeeded or failed.
func (e *Executor) CheckReceipt(ctx context.Context, txHash string, path string) {
	hash := common.HexToHash(txHash)
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		receipt, err := e.client.TransactionReceipt(ctx, hash)
		if err != nil {
			time.Sleep(2 * time.Second)
			continue
		}
		if receipt.Status == 1 {
			log.Printf("[executor] tx SUCCESS hash=%s path=%s gasUsed=%d", txHash[:12], path, receipt.GasUsed)
		} else {
			log.Printf("[executor] tx REVERTED hash=%s path=%s gasUsed=%d", txHash[:12], path, receipt.GasUsed)
		}
		return
	}
	log.Printf("[executor] tx TIMEOUT (not mined in 30s) hash=%s", txHash[:12])
}
