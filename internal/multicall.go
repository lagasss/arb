package internal

import (
	"context"
	"fmt"
	"log"
	"math/big"
	"strings"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/ethclient"
)

// Multicall3 contract address — same on all EVM chains including Arbitrum.
// Supports tryAggregate(bool requireSuccess, ...) unlike the original Multicall.
const Multicall2Address = "0xcA11bde05977b3631167028862bE2a173976CA11"

// Balancer Vault address on Arbitrum (same across all networks)
const BalancerVaultAddress = "0xBA12222222228d8Ba445958a75a0704d566BF2C8"
const V4StateViewAddress = "0x76fd297e2d437cd7f76d50f01afe6160f86e9990"

// Minimal ABI definitions for pool state reads
var (
	uniV3Slot0ABI         abi.ABI
	pancakeV3Slot0ABI     abi.ABI // PancakeV3 slot0 has uint32 feeProtocol (vs uint8 in UniV3)
	uniV2ReservesABI      abi.ABI
	multicall2ABI         abi.ABI
	algebraGlobalStateABI abi.ABI
	balancerVaultABI      abi.ABI
	balancerPoolABI       abi.ABI
	curvePoolABI          abi.ABI
	tickSpacingABI        abi.ABI
	tickBitmapABI         abi.ABI
	tickInfoABI           abi.ABI
	algebraTickTableABI   abi.ABI // CamelotV3/Algebra: tickTable(int16) → uint256
	algebraTicksABI       abi.ABI // CamelotV3/Algebra: ticks(int24) → (liquidityTotal, liquidityDelta, ...)
	v4TickBitmapABI       abi.ABI
	v4TickLiqABI          abi.ABI
	v4Slot0ABI            abi.ABI
	v4LiquidityABI        abi.ABI
)

func init() {
	var err error
	uniV3Slot0ABI, err = abi.JSON(strings.NewReader(`[{
		"name":"slot0","type":"function","stateMutability":"view",
		"inputs":[],
		"outputs":[
			{"name":"sqrtPriceX96","type":"uint160"},
			{"name":"tick","type":"int24"},
			{"name":"observationIndex","type":"uint16"},
			{"name":"observationCardinality","type":"uint16"},
			{"name":"observationCardinalityNext","type":"uint16"},
			{"name":"feeProtocol","type":"uint8"},
			{"name":"unlocked","type":"bool"}
		]
	},{
		"name":"liquidity","type":"function","stateMutability":"view",
		"inputs":[],"outputs":[{"name":"","type":"uint128"}]
	}]`))
	if err != nil {
		panic(fmt.Sprintf("uniV3Slot0ABI: %v", err))
	}
	// PancakeSwap V3 slot0 is identical to UniV3 except feeProtocol is uint32 (not uint8).
	// Using the wrong ABI causes silent decode failure (sqrtPriceX96 returns nil).
	pancakeV3Slot0ABI, err = abi.JSON(strings.NewReader(`[{
		"name":"slot0","type":"function","stateMutability":"view",
		"inputs":[],
		"outputs":[
			{"name":"sqrtPriceX96","type":"uint160"},
			{"name":"tick","type":"int24"},
			{"name":"observationIndex","type":"uint16"},
			{"name":"observationCardinality","type":"uint16"},
			{"name":"observationCardinalityNext","type":"uint16"},
			{"name":"feeProtocol","type":"uint32"},
			{"name":"unlocked","type":"bool"}
		]
	},{
		"name":"liquidity","type":"function","stateMutability":"view",
		"inputs":[],"outputs":[{"name":"","type":"uint128"}]
	}]`))
	if err != nil {
		panic(fmt.Sprintf("pancakeV3Slot0ABI: %v", err))
	}

	uniV2ReservesABI, err = abi.JSON(strings.NewReader(`[{
		"name":"getReserves","type":"function","stateMutability":"view",
		"inputs":[],
		"outputs":[
			{"name":"reserve0","type":"uint112"},
			{"name":"reserve1","type":"uint112"},
			{"name":"blockTimestampLast","type":"uint32"}
		]
	}]`))
	if err != nil {
		panic(fmt.Sprintf("uniV2ReservesABI: %v", err))
	}

	multicall2ABI, err = abi.JSON(strings.NewReader(`[{
		"name":"tryAggregate","type":"function","stateMutability":"view",
		"inputs":[
			{"name":"requireSuccess","type":"bool"},
			{"components":[{"name":"target","type":"address"},{"name":"callData","type":"bytes"}],"name":"calls","type":"tuple[]"}
		],
		"outputs":[{"components":[{"name":"success","type":"bool"},{"name":"returnData","type":"bytes"}],"name":"returnData","type":"tuple[]"}]
	}]`))
	if err != nil {
		panic(fmt.Sprintf("multicall2ABI: %v", err))
	}

	algebraGlobalStateABI, err = abi.JSON(strings.NewReader(`[{
		"name":"globalState","type":"function","stateMutability":"view",
		"inputs":[],
		"outputs":[
			{"name":"price","type":"uint160"},
			{"name":"tick","type":"int24"},
			{"name":"feeZto","type":"int16"},
			{"name":"feeOtz","type":"int16"},
			{"name":"timepointIndex","type":"uint16"},
			{"name":"communityFeeToken0","type":"uint8"},
			{"name":"communityFeeToken1","type":"uint8"},
			{"name":"unlocked","type":"bool"}
		]
	},{
		"name":"liquidity","type":"function","stateMutability":"view",
		"inputs":[],"outputs":[{"name":"","type":"uint128"}]
	}]`))
	if err != nil {
		panic(fmt.Sprintf("algebraGlobalStateABI: %v", err))
	}

	balancerVaultABI, err = abi.JSON(strings.NewReader(`[{
		"name":"getPoolTokens","type":"function","stateMutability":"view",
		"inputs":[{"name":"poolId","type":"bytes32"}],
		"outputs":[
			{"name":"tokens","type":"address[]"},
			{"name":"balances","type":"uint256[]"},
			{"name":"lastChangeBlock","type":"uint256"}
		]
	}]`))
	if err != nil {
		panic(fmt.Sprintf("balancerVaultABI: %v", err))
	}

	balancerPoolABI, err = abi.JSON(strings.NewReader(`[{
		"name":"getNormalizedWeights","type":"function","stateMutability":"view",
		"inputs":[],"outputs":[{"name":"","type":"uint256[]"}]
	},{
		"name":"getSwapFeePercentage","type":"function","stateMutability":"view",
		"inputs":[],"outputs":[{"name":"","type":"uint256"}]
	}]`))
	if err != nil {
		panic(fmt.Sprintf("balancerPoolABI: %v", err))
	}

	// tickSpacing() — same signature for UniV3, PancakeV3, Algebra, and all forks.
	// Returns int24 (decoded by go-ethereum as int32).
	tickSpacingABI, err = abi.JSON(strings.NewReader(`[{
		"name":"tickSpacing","type":"function","stateMutability":"view",
		"inputs":[],"outputs":[{"name":"","type":"int24"}]
	}]`))
	if err != nil {
		panic(fmt.Sprintf("tickSpacingABI: %v", err))
	}

	// V3 tick data: tickBitmap(int16) → uint256, ticks(int24) → (liquidityGross, liquidityNet, ...)
	tickBitmapABI, err = abi.JSON(strings.NewReader(`[{
		"name":"tickBitmap","type":"function","stateMutability":"view",
		"inputs":[{"name":"wordPosition","type":"int16"}],
		"outputs":[{"name":"","type":"uint256"}]
	}]`))
	if err != nil {
		panic(fmt.Sprintf("tickBitmapABI: %v", err))
	}
	tickInfoABI, err = abi.JSON(strings.NewReader(`[{
		"name":"ticks","type":"function","stateMutability":"view",
		"inputs":[{"name":"tick","type":"int24"}],
		"outputs":[
			{"name":"liquidityGross","type":"uint128"},
			{"name":"liquidityNet","type":"int128"},
			{"name":"feeGrowthOutside0X128","type":"uint256"},
			{"name":"feeGrowthOutside1X128","type":"uint256"},
			{"name":"tickCumulativeOutside","type":"int56"},
			{"name":"secondsPerLiquidityOutsideX128","type":"uint160"},
			{"name":"secondsOutside","type":"uint32"},
			{"name":"initialized","type":"bool"}
		]
	}]`))
	if err != nil {
		panic(fmt.Sprintf("tickInfoABI: %v", err))
	}

	// CamelotV3/Algebra uses different selectors than Uniswap V3:
	//   - tickTable(int16) instead of tickBitmap(int16)
	//   - ticks(int24) returns (uint128 liquidityTotal, int128 liquidityDelta, ...)
	//     liquidityDelta is functionally equivalent to UniV3's liquidityNet.
	algebraTickTableABI, err = abi.JSON(strings.NewReader(`[{
		"name":"tickTable","type":"function","stateMutability":"view",
		"inputs":[{"name":"wordPosition","type":"int16"}],
		"outputs":[{"name":"","type":"uint256"}]
	}]`))
	if err != nil {
		panic(fmt.Sprintf("algebraTickTableABI: %v", err))
	}
	algebraTicksABI, err = abi.JSON(strings.NewReader(`[{
		"name":"ticks","type":"function","stateMutability":"view",
		"inputs":[{"name":"tick","type":"int24"}],
		"outputs":[
			{"name":"liquidityTotal","type":"uint128"},
			{"name":"liquidityDelta","type":"int128"},
			{"name":"outerFeeGrowth0Token","type":"uint256"},
			{"name":"outerFeeGrowth1Token","type":"uint256"},
			{"name":"outerTickCumulative","type":"int56"},
			{"name":"outerSecondsPerLiquidity","type":"uint160"},
			{"name":"outerSecondsSpent","type":"uint32"},
			{"name":"initialized","type":"bool"}
		]
	}]`))
	if err != nil {
		panic(fmt.Sprintf("algebraTicksABI: %v", err))
	}

	// V4 StateView: getTickBitmap(bytes32 poolId, int16 word) → uint256
	//                getTickLiquidity(bytes32 poolId, int24 tick) → (uint128, int128)
	v4TickBitmapABI, err = abi.JSON(strings.NewReader(`[{
		"name":"getTickBitmap","type":"function","stateMutability":"view",
		"inputs":[{"name":"poolId","type":"bytes32"},{"name":"tick","type":"int16"}],
		"outputs":[{"name":"","type":"uint256"}]
	}]`))
	if err != nil {
		panic(fmt.Sprintf("v4TickBitmapABI: %v", err))
	}
	v4TickLiqABI, err = abi.JSON(strings.NewReader(`[{
		"name":"getTickLiquidity","type":"function","stateMutability":"view",
		"inputs":[{"name":"poolId","type":"bytes32"},{"name":"tick","type":"int24"}],
		"outputs":[
			{"name":"liquidityGross","type":"uint128"},
			{"name":"liquidityNet","type":"int128"}
		]
	}]`))
	if err != nil {
		panic(fmt.Sprintf("v4TickLiqABI: %v", err))
	}

	// V4 StateView: getSlot0(bytes32 poolId) → (sqrtPriceX96, tick, protocolFee, lpFee)
	v4Slot0ABI, err = abi.JSON(strings.NewReader(`[{
		"name":"getSlot0","type":"function","stateMutability":"view",
		"inputs":[{"name":"poolId","type":"bytes32"}],
		"outputs":[
			{"name":"sqrtPriceX96","type":"uint160"},
			{"name":"tick","type":"int24"},
			{"name":"protocolFee","type":"uint24"},
			{"name":"lpFee","type":"uint24"}
		]
	}]`))
	if err != nil {
		panic(fmt.Sprintf("v4Slot0ABI: %v", err))
	}
	// V4 StateView: getLiquidity(bytes32 poolId) → uint128
	v4LiquidityABI, err = abi.JSON(strings.NewReader(`[{
		"name":"getLiquidity","type":"function","stateMutability":"view",
		"inputs":[{"name":"poolId","type":"bytes32"}],
		"outputs":[{"name":"","type":"uint128"}]
	}]`))
	if err != nil {
		panic(fmt.Sprintf("v4LiquidityABI: %v", err))
	}

	// Curve StableSwap (plain and factory pools): balances(uint256), A(), fee()
	curvePoolABI, err = abi.JSON(strings.NewReader(`[{
		"name":"balances","type":"function","stateMutability":"view",
		"inputs":[{"name":"i","type":"uint256"}],
		"outputs":[{"name":"","type":"uint256"}]
	},{
		"name":"A","type":"function","stateMutability":"view",
		"inputs":[],"outputs":[{"name":"","type":"uint256"}]
	},{
		"name":"fee","type":"function","stateMutability":"view",
		"inputs":[],"outputs":[{"name":"","type":"uint256"}]
	}]`))
	if err != nil {
		panic(fmt.Sprintf("curvePoolABI: %v", err))
	}
}

type call struct {
	Target   common.Address
	CallData []byte
}

// callKind classifies every Multicall3 sub-call so the decoder doesn't depend
// on fixed per-pool offsets (needed because we skip already-loaded immutables
// like tickSpacing and Balancer weights on the hot path).
type callKind uint8

const (
	kindV3Slot0 callKind = iota
	kindPancakeSlot0
	kindV3Liquidity
	kindAlgebraLiquidity
	kindAlgebraGlobalState
	kindTickSpacing
	kindV2Reserves
	kindBalancerTokens
	kindBalancerWeights
	kindBalancerFee
	kindCurveBal0
	kindCurveBal1
	kindCurveA
	kindCurveFee
	kindCamelotGAO0
	kindCamelotGAO1
)

// taggedCall is one Multicall3 sub-call with enough metadata for the decoder
// to apply the result back to the right pool without caring about positional
// offsets.
type taggedCall struct {
	pool    *Pool
	kind    callKind
	target  common.Address
	data    []byte
	testAmt *big.Int // only for Camelot GAO calls, nil otherwise
}

// BatchUpdatePools reads all pool states in a single eth_call via Multicall2 (latest block).
func BatchUpdatePools(ctx context.Context, client *ethclient.Client, pools []*Pool) error {
	return BatchUpdatePoolsAtBlock(ctx, client, pools, nil)
}

// BatchUpdatePoolsAtBlock reads all pool states via Multicall3 at the given
// block. Pass nil for blockNum to read `latest` and auto-stamp pools with the
// head block the RPC observed at read time (the correct stamp for the
// block-lag freshness gate: it reflects "the chain state this pool saw", not
// "the block that happened to trigger this dispatch").
//
// Optimisations landed 2026-04-19:
//  1. Per-block pass reads ONLY mutable state (slot0/globalState/reserves/
//     balances). Immutable-or-slow-changing fields (tickSpacing, Balancer
//     weights+fee, Curve A+fee, Camelot directional-fee calibration) are
//     served from the pool's existing fields when non-zero, and refreshed
//     by RefreshPoolImmutables on a 5-minute background ticker.
//  2. Multicall3 sub-call batches are dispatched in PARALLEL via
//     doMulticallParallel. A 5,000-call set that previously took 11 × ~80ms
//     serial round-trips now finishes in the time of the slowest single
//     batch (~100ms). This is the big unlock for sb_lag=0-1 consistency.
//  3. When blockNum is nil we call client.BlockNumber() right after the
//     multicall completes and stamp pools with THAT block — not the trigger
//     block. Eliminates the "multicall for block B, but by the time it
//     returned block B+3 had arrived" class of pre-dryrun regate reject.
func BatchUpdatePoolsAtBlock(ctx context.Context, client *ethclient.Client, pools []*Pool, blockNum *big.Int) error {
	if len(pools) == 0 {
		return nil
	}

	// Build the mutable-state sub-calls for every pool. Immutable fields
	// (tickSpacing, Balancer weights+fee, Curve A+fee, Camelot directional
	// fees) are read from Pool's existing fields when non-zero; callers must
	// prime those via RefreshPoolImmutables at startup and on a slow ticker.
	tagged := make([]taggedCall, 0, len(pools)*2)
	for _, p := range pools {
		addr := common.HexToAddress(p.Address)
		switch p.DEX {
		case DEXUniswapV3, DEXSushiSwapV3, DEXRamsesV3:
			slot0Data, err := uniV3Slot0ABI.Pack("slot0")
			if err == nil {
				tagged = append(tagged, taggedCall{p, kindV3Slot0, addr, slot0Data, nil})
			}
			liqData, err := uniV3Slot0ABI.Pack("liquidity")
			if err == nil {
				tagged = append(tagged, taggedCall{p, kindV3Liquidity, addr, liqData, nil})
			}
			if p.TickSpacing == 0 {
				if tsData, err := tickSpacingABI.Pack("tickSpacing"); err == nil {
					tagged = append(tagged, taggedCall{p, kindTickSpacing, addr, tsData, nil})
				}
			}
		case DEXPancakeV3:
			slot0Data, err := pancakeV3Slot0ABI.Pack("slot0")
			if err == nil {
				tagged = append(tagged, taggedCall{p, kindPancakeSlot0, addr, slot0Data, nil})
			}
			liqData, err := pancakeV3Slot0ABI.Pack("liquidity")
			if err == nil {
				tagged = append(tagged, taggedCall{p, kindV3Liquidity, addr, liqData, nil})
			}
			if p.TickSpacing == 0 {
				if tsData, err := tickSpacingABI.Pack("tickSpacing"); err == nil {
					tagged = append(tagged, taggedCall{p, kindTickSpacing, addr, tsData, nil})
				}
			}
		case DEXCamelotV3, DEXZyberV3:
			gsData, err := algebraGlobalStateABI.Pack("globalState")
			if err == nil {
				tagged = append(tagged, taggedCall{p, kindAlgebraGlobalState, addr, gsData, nil})
			}
			liqData, err := algebraGlobalStateABI.Pack("liquidity")
			if err == nil {
				tagged = append(tagged, taggedCall{p, kindAlgebraLiquidity, addr, liqData, nil})
			}
			if p.TickSpacing == 0 {
				if tsData, err := tickSpacingABI.Pack("tickSpacing"); err == nil {
					tagged = append(tagged, taggedCall{p, kindTickSpacing, addr, tsData, nil})
				}
			}
		case DEXBalancerWeighted:
			if p.PoolID == "" {
				continue
			}
			poolIDBytes, err := hexToBytes32(p.PoolID)
			if err != nil {
				continue
			}
			vault := common.HexToAddress(BalancerVaultAddress)
			if tokensData, err := balancerVaultABI.Pack("getPoolTokens", poolIDBytes); err == nil {
				tagged = append(tagged, taggedCall{p, kindBalancerTokens, vault, tokensData, nil})
			}
			if p.Weight0 == 0 || p.Weight1 == 0 {
				if weightsData, err := balancerPoolABI.Pack("getNormalizedWeights"); err == nil {
					tagged = append(tagged, taggedCall{p, kindBalancerWeights, addr, weightsData, nil})
				}
			}
			if p.FeeBps == 0 && p.FeePPM == 0 {
				if feeData, err := balancerPoolABI.Pack("getSwapFeePercentage"); err == nil {
					tagged = append(tagged, taggedCall{p, kindBalancerFee, addr, feeData, nil})
				}
			}
		case DEXCurve:
			if bal0Data, err := curvePoolABI.Pack("balances", big.NewInt(0)); err == nil {
				tagged = append(tagged, taggedCall{p, kindCurveBal0, addr, bal0Data, nil})
			}
			if bal1Data, err := curvePoolABI.Pack("balances", big.NewInt(1)); err == nil {
				tagged = append(tagged, taggedCall{p, kindCurveBal1, addr, bal1Data, nil})
			}
			if p.AmpFactor == 0 {
				if aData, err := curvePoolABI.Pack("A"); err == nil {
					tagged = append(tagged, taggedCall{p, kindCurveA, addr, aData, nil})
				}
			}
			if p.CurveFee1e10 == 0 {
				if feeData, err := curvePoolABI.Pack("fee"); err == nil {
					tagged = append(tagged, taggedCall{p, kindCurveFee, addr, feeData, nil})
				}
			}
		case DEXCamelot:
			resData, err := uniV2ReservesABI.Pack("getReserves")
			if err == nil {
				tagged = append(tagged, taggedCall{p, kindV2Reserves, addr, resData, nil})
			}
			// Directional fee calibration only runs when fees are not yet
			// known (first time this pool is seen). After calibration the
			// Token0FeeBps / Token1FeeBps fields stick and we skip.
			if p.Token0FeeBps == 0 || p.Token1FeeBps == 0 {
				testAmt0 := big.NewInt(1e14)
				if p.Reserve0 != nil && p.Reserve0.Sign() > 0 {
					testAmt0 = new(big.Int).Div(p.Reserve0, big.NewInt(1000))
					if testAmt0.Sign() == 0 {
						testAmt0 = big.NewInt(1)
					}
				}
				testAmt1 := big.NewInt(1e14)
				if p.Reserve1 != nil && p.Reserve1.Sign() > 0 {
					testAmt1 = new(big.Int).Div(p.Reserve1, big.NewInt(1000))
					if testAmt1.Sign() == 0 {
						testAmt1 = big.NewInt(1)
					}
				}
				gao0 := make([]byte, 4+64)
				copy(gao0[:4], []byte{0xf1, 0x40, 0xa3, 0x5a})
				testAmt0.FillBytes(gao0[4:36])
				copy(gao0[36+12:68], common.HexToAddress(p.Token0.Address).Bytes())
				gao1 := make([]byte, 4+64)
				copy(gao1[:4], []byte{0xf1, 0x40, 0xa3, 0x5a})
				testAmt1.FillBytes(gao1[4:36])
				copy(gao1[36+12:68], common.HexToAddress(p.Token1.Address).Bytes())
				tagged = append(tagged, taggedCall{p, kindCamelotGAO0, addr, gao0, testAmt0})
				tagged = append(tagged, taggedCall{p, kindCamelotGAO1, addr, gao1, testAmt1})
			}
		default:
			if resData, err := uniV2ReservesABI.Pack("getReserves"); err == nil {
				tagged = append(tagged, taggedCall{p, kindV2Reserves, addr, resData, nil})
			}
		}
	}

	if len(tagged) == 0 {
		return nil
	}

	flat := make([]call, len(tagged))
	for i, t := range tagged {
		flat[i] = call{Target: t.target, CallData: t.data}
	}
	results, err := doMulticallParallel(ctx, client, flat, blockNum)
	if err != nil {
		return fmt.Errorf("multicall call: %w", err)
	}

	// If caller wanted "latest" semantics, query the block number we're
	// stamping with AFTER the read completes. The multicall itself read
	// `latest` from the RPC's point of view at the moment it ran, so using
	// client.BlockNumber() here gives us the block the RPC observed when it
	// served the read — which is exactly what p.StateBlock should reflect
	// for the freshness gate.
	var stampBlock uint64
	if blockNum != nil && blockNum.IsUint64() {
		stampBlock = blockNum.Uint64()
	} else {
		if bn, berr := client.BlockNumber(ctx); berr == nil {
			stampBlock = bn
		}
	}

	// Per-pool accumulated state. We can't decode each sub-call in isolation
	// (e.g. V3 needs both slot0 AND liquidity before we can promote the
	// state atomically), so batch per-pool results and apply them all at
	// once under p.mu.Lock.
	type poolDecode struct {
		slot0           []byte
		pancakeSlot0    []byte
		liquidity       []byte
		algebraGS       []byte
		algebraLiq      []byte
		tickSpacing     []byte
		reserves        []byte
		balTokens       []byte
		balWeights      []byte
		balFee          []byte
		curveBal0       []byte
		curveBal1       []byte
		curveA          []byte
		curveFee        []byte
		camelotGAO0     []byte
		camelotGAO0Amt  *big.Int
		camelotGAO1     []byte
		camelotGAO1Amt  *big.Int
	}
	perPool := make(map[*Pool]*poolDecode, len(pools))
	for i, r := range results {
		if i >= len(tagged) {
			break
		}
		if !r.Success {
			continue
		}
		tc := tagged[i]
		pd := perPool[tc.pool]
		if pd == nil {
			pd = &poolDecode{}
			perPool[tc.pool] = pd
		}
		switch tc.kind {
		case kindV3Slot0:
			pd.slot0 = r.ReturnData
		case kindPancakeSlot0:
			pd.pancakeSlot0 = r.ReturnData
		case kindV3Liquidity:
			pd.liquidity = r.ReturnData
		case kindAlgebraGlobalState:
			pd.algebraGS = r.ReturnData
		case kindAlgebraLiquidity:
			pd.algebraLiq = r.ReturnData
		case kindTickSpacing:
			pd.tickSpacing = r.ReturnData
		case kindV2Reserves:
			pd.reserves = r.ReturnData
		case kindBalancerTokens:
			pd.balTokens = r.ReturnData
		case kindBalancerWeights:
			pd.balWeights = r.ReturnData
		case kindBalancerFee:
			pd.balFee = r.ReturnData
		case kindCurveBal0:
			pd.curveBal0 = r.ReturnData
		case kindCurveBal1:
			pd.curveBal1 = r.ReturnData
		case kindCurveA:
			pd.curveA = r.ReturnData
		case kindCurveFee:
			pd.curveFee = r.ReturnData
		case kindCamelotGAO0:
			pd.camelotGAO0 = r.ReturnData
			pd.camelotGAO0Amt = tc.testAmt
		case kindCamelotGAO1:
			pd.camelotGAO1 = r.ReturnData
			pd.camelotGAO1Amt = tc.testAmt
		}
	}

	now := time.Now()
	for p, pd := range perPool {
		p.mu.Lock()
		switch p.DEX {
		case DEXUniswapV3, DEXSushiSwapV3, DEXRamsesV3:
			if pd.slot0 != nil {
				_ = decodeSlot0(pd.slot0, p)
			}
			if pd.liquidity != nil {
				_ = decodeLiquidity(pd.liquidity, p)
			}
			if pd.tickSpacing != nil {
				decodeTickSpacing(pd.tickSpacing, p)
			}
		case DEXPancakeV3:
			if pd.pancakeSlot0 != nil {
				_ = decodePancakeSlot0(pd.pancakeSlot0, p)
			}
			if pd.liquidity != nil {
				_ = decodeLiquidity(pd.liquidity, p)
			}
			if pd.tickSpacing != nil {
				decodeTickSpacing(pd.tickSpacing, p)
			}
		case DEXCamelotV3, DEXZyberV3:
			if pd.algebraGS != nil {
				_ = decodeGlobalState(pd.algebraGS, p)
			}
			if pd.algebraLiq != nil {
				_ = decodeLiquidityAlgebra(pd.algebraLiq, p)
			}
			if pd.tickSpacing != nil {
				decodeTickSpacing(pd.tickSpacing, p)
			}
		case DEXCurve:
			// decodeCurveState requires all 4 slots; synthesise zero-slot
			// buffers when an immutable is cached and not re-fetched.
			bal0, bal1 := pd.curveBal0, pd.curveBal1
			aData, feeData := pd.curveA, pd.curveFee
			if aData == nil && p.AmpFactor > 0 {
				aData = packUint256(p.AmpFactor)
			}
			if feeData == nil && p.CurveFee1e10 > 0 {
				feeData = packUint256(p.CurveFee1e10)
			}
			if bal0 != nil && bal1 != nil {
				_ = decodeCurveState(bal0, bal1, aData, feeData, p)
			}
		case DEXBalancerWeighted:
			if pd.balTokens != nil {
				_ = decodeBalancerTokens(pd.balTokens, p)
			}
			if pd.balWeights != nil {
				_ = decodeBalancerWeights(pd.balWeights, p)
			}
			if pd.balFee != nil {
				decodeBalancerFee(pd.balFee, p)
			}
		case DEXCamelot:
			if pd.reserves != nil {
				_ = decodeReserves(pd.reserves, p)
			}
			if pd.camelotGAO0 != nil && pd.camelotGAO0Amt != nil {
				calibrateCamelotDirectionalFee(pd.camelotGAO0, p, pd.camelotGAO0Amt, true)
			}
			if pd.camelotGAO1 != nil && pd.camelotGAO1Amt != nil {
				calibrateCamelotDirectionalFee(pd.camelotGAO1, p, pd.camelotGAO1Amt, false)
			}
			var maxDirFee uint32
			if f := p.Token0FeeBps / 10; f > maxDirFee {
				maxDirFee = f
			}
			if f := p.Token1FeeBps / 10; f > maxDirFee {
				maxDirFee = f
			}
			if maxDirFee > 0 {
				p.FeeBps = maxDirFee
			}
		default:
			if pd.reserves != nil {
				_ = decodeReserves(pd.reserves, p)
			}
		}
		p.LastUpdated = now
		if stampBlock > p.StateBlock {
			p.StateBlock = stampBlock
		}
		p.mu.Unlock()
		p.UpdateSpotPrice()
	}

	// Fallback: individually query V3 pools that still have nil sqrtPrice after
	// the batch multicall. These pools' slot0() may have failed in the batch
	// (e.g. pool was locked mid-swap). A direct call often succeeds.
	//
	// Same locking discipline as the main parse loop: hold p.mu.Lock around
	// the in-memory decode work, release before UpdateSpotPrice. The RPC calls
	// run OUTSIDE the lock so we never hold it across network IO.
	var fallbackAttempted, fallbackSuccess int
	for _, p := range pools {
		p.mu.RLock()
		hasPrice := p.SqrtPriceX96 != nil
		p.mu.RUnlock()
		if hasPrice || p.DEX == DEXCurve || p.DEX == DEXBalancerWeighted {
			continue
		}
		switch p.DEX {
		case DEXUniswapV3, DEXSushiSwapV3, DEXRamsesV3, DEXPancakeV3, DEXCamelotV3, DEXZyberV3:
		default:
			continue // only V3-style pools
		}
		addr := common.HexToAddress(p.Address)
		// Direct slot0 / globalState call
		var callData []byte
		if p.DEX == DEXCamelotV3 || p.DEX == DEXZyberV3 {
			callData, _ = algebraGlobalStateABI.Pack("globalState")
		} else if p.DEX == DEXPancakeV3 {
			callData, _ = pancakeV3Slot0ABI.Pack("slot0")
		} else {
			callData, _ = uniV3Slot0ABI.Pack("slot0")
		}
		if callData == nil {
			continue
		}
		fallbackAttempted++
		res, err := client.CallContract(ctx, ethereum.CallMsg{To: &addr, Data: callData}, blockNum)
		if err != nil || len(res) == 0 {
			continue
		}
		// Decode + (conditionally) fetch liquidity. Liquidity fetch happens
		// outside the lock; we re-acquire to write the decoded value.
		p.mu.Lock()
		switch p.DEX {
		case DEXCamelotV3, DEXZyberV3:
			decodeGlobalState(res, p)
		case DEXPancakeV3:
			decodePancakeSlot0(res, p)
		default:
			decodeSlot0(res, p)
		}
		gotPrice := p.SqrtPriceX96 != nil
		p.mu.Unlock()
		if !gotPrice {
			continue
		}
		// Also fetch liquidity
		liqData, _ := uniV3Slot0ABI.Pack("liquidity")
		if p.DEX == DEXCamelotV3 || p.DEX == DEXZyberV3 {
			liqData, _ = algebraGlobalStateABI.Pack("liquidity")
		} else if p.DEX == DEXPancakeV3 {
			liqData, _ = pancakeV3Slot0ABI.Pack("liquidity")
		}
		if liqRes, err := client.CallContract(ctx, ethereum.CallMsg{To: &addr, Data: liqData}, blockNum); err == nil {
			p.mu.Lock()
			decodeLiquidity(liqRes, p)
			p.LastUpdated = time.Now()
			if stampBlock > p.StateBlock {
				p.StateBlock = stampBlock
			}
			p.mu.Unlock()
		} else {
			p.mu.Lock()
			p.LastUpdated = time.Now()
			if stampBlock > p.StateBlock {
				p.StateBlock = stampBlock
			}
			p.mu.Unlock()
		}
		p.UpdateSpotPrice()
		fallbackSuccess++
	}
	if fallbackAttempted > 0 {
		log.Printf("[multicall] fallback: %d/%d pools rescued via direct call", fallbackSuccess, fallbackAttempted)
	}

	return nil
}

func decodeSlot0(data []byte, p *Pool) error {
	if len(data) == 0 {
		return fmt.Errorf("empty slot0 data")
	}
	vals, err := uniV3Slot0ABI.Unpack("slot0", data)
	if err != nil {
		return err
	}
	if len(vals) < 2 {
		return fmt.Errorf("slot0: insufficient values")
	}
	p.SqrtPriceX96, _ = vals[0].(*big.Int)
	// go-ethereum decodes int24 as *big.Int, not int32
	switch v := vals[1].(type) {
	case int32:
		p.Tick = v
	case *big.Int:
		if v.IsInt64() {
			p.Tick = int32(v.Int64())
		}
	}
	return nil
}

func decodePancakeSlot0(data []byte, p *Pool) error {
	if len(data) == 0 {
		return fmt.Errorf("empty pancake slot0 data")
	}
	vals, err := pancakeV3Slot0ABI.Unpack("slot0", data)
	if err != nil {
		return err
	}
	if len(vals) < 2 {
		return fmt.Errorf("pancake slot0: insufficient values")
	}
	p.SqrtPriceX96, _ = vals[0].(*big.Int)
	switch v := vals[1].(type) {
	case int32:
		p.Tick = v
	case *big.Int:
		if v.IsInt64() {
			p.Tick = int32(v.Int64())
		}
	}
	return nil
}

func decodeLiquidity(data []byte, p *Pool) error {
	if len(data) == 0 {
		return fmt.Errorf("empty liquidity data")
	}
	vals, err := uniV3Slot0ABI.Unpack("liquidity", data)
	if err != nil {
		return err
	}
	if len(vals) < 1 {
		return fmt.Errorf("liquidity: insufficient values")
	}
	liq, _ := vals[0].(*big.Int)
	p.Liquidity = liq
	return nil
}

func decodeGlobalState(data []byte, p *Pool) error {
	if len(data) == 0 {
		return fmt.Errorf("empty globalState data")
	}
	vals, err := algebraGlobalStateABI.Unpack("globalState", data)
	if err != nil {
		return err
	}
	if len(vals) < 3 {
		return fmt.Errorf("globalState: insufficient values")
	}
	p.SqrtPriceX96, _ = vals[0].(*big.Int)
	switch v := vals[1].(type) {
	case int32:
		p.Tick = v
	case *big.Int:
		if v.IsInt64() {
			p.Tick = int32(v.Int64())
		}
	}
	// Algebra V1.9 returns directional fees feeZto (vals[2]) and feeOtz (vals[3]).
	// Both are in pips (1/1,000,000). Store as both FeePPM (lossless) and FeeBps.
	if fee, ok := vals[2].(int16); ok && fee > 0 {
		p.FeePPM = uint32(fee)         // pips — used by simulator (no precision loss)
		p.FeeBps = uint32(fee) / 100   // bps — used by graph edge weights
	}
	return nil
}

func decodeTickSpacing(data []byte, p *Pool) {
	if len(data) == 0 {
		return
	}
	vals, err := tickSpacingABI.Unpack("tickSpacing", data)
	if err != nil || len(vals) == 0 {
		return
	}
	// go-ethereum decodes int24 as *big.Int, not int32
	switch v := vals[0].(type) {
	case int32:
		if v > 0 {
			p.TickSpacing = v
		}
	case *big.Int:
		if v.Sign() > 0 && v.IsInt64() {
			p.TickSpacing = int32(v.Int64())
		}
	}
}

func decodeLiquidityAlgebra(data []byte, p *Pool) error {
	if len(data) == 0 {
		return fmt.Errorf("empty liquidity data")
	}
	vals, err := algebraGlobalStateABI.Unpack("liquidity", data)
	if err != nil {
		return err
	}
	if len(vals) < 1 {
		return fmt.Errorf("liquidity: insufficient values")
	}
	liq, _ := vals[0].(*big.Int)
	p.Liquidity = liq
	return nil
}

// decodeBalancerTokens unpacks getPoolTokens() and sets Reserve0/Reserve1 on the pool.
// Balances are ordered by token address (ascending), which must match pool.Token0/Token1.
func decodeBalancerTokens(data []byte, p *Pool) error {
	if len(data) == 0 {
		return fmt.Errorf("empty getPoolTokens data")
	}
	vals, err := balancerVaultABI.Unpack("getPoolTokens", data)
	if err != nil {
		return err
	}
	if len(vals) < 2 {
		return fmt.Errorf("getPoolTokens: insufficient values")
	}
	balances, ok := vals[1].([]*big.Int)
	if !ok || len(balances) < 2 {
		return fmt.Errorf("getPoolTokens: unexpected balances type")
	}
	// Balancer orders tokens by address ascending; Token0/Token1 in our Pool
	// are configured to match that order (same as UniV2 convention).
	p.Reserve0 = balances[0]
	p.Reserve1 = balances[1]
	return nil
}

// decodeBalancerWeights unpacks getNormalizedWeights() and sets Weight0/Weight1.
// Weights are 1e18-scaled (1e18 == 100%).
func decodeBalancerWeights(data []byte, p *Pool) error {
	if len(data) == 0 {
		return fmt.Errorf("empty getNormalizedWeights data")
	}
	vals, err := balancerPoolABI.Unpack("getNormalizedWeights", data)
	if err != nil {
		return err
	}
	if len(vals) < 1 {
		return fmt.Errorf("getNormalizedWeights: insufficient values")
	}
	weights, ok := vals[0].([]*big.Int)
	if !ok || len(weights) < 2 {
		return fmt.Errorf("getNormalizedWeights: unexpected weights type")
	}
	scale := new(big.Float).SetInt(new(big.Int).Exp(big.NewInt(10), big.NewInt(18), nil))
	w0, _ := new(big.Float).Quo(new(big.Float).SetInt(weights[0]), scale).Float64()
	w1, _ := new(big.Float).Quo(new(big.Float).SetInt(weights[1]), scale).Float64()
	p.Weight0 = w0
	p.Weight1 = w1
	return nil
}

// decodeBalancerFee unpacks getSwapFeePercentage() and sets FeePPM/FeeBps.
// The on-chain value is an 18-decimal fraction (e.g. 3e15 = 0.3% = 30 bps).
// Convert to ppm: value / 1e12.
func decodeBalancerFee(data []byte, p *Pool) {
	if len(data) == 0 {
		return
	}
	vals, err := balancerPoolABI.Unpack("getSwapFeePercentage", data)
	if err != nil || len(vals) < 1 {
		return
	}
	feeRaw, ok := vals[0].(*big.Int)
	if !ok || feeRaw.Sign() <= 0 {
		return
	}
	ppm := new(big.Int).Div(feeRaw, big.NewInt(1e12)).Uint64()
	p.FeePPM = uint32(ppm)
	p.FeeBps = uint32(ppm / 100)
}

// hexToBytes32 converts a 0x-prefixed hex string to a [32]byte pool ID.
func hexToBytes32(s string) ([32]byte, error) {
	var result [32]byte
	s = strings.TrimPrefix(s, "0x")
	if len(s) != 64 {
		return result, fmt.Errorf("pool ID must be 32 bytes (64 hex chars), got %d", len(s))
	}
	b := common.FromHex("0x" + s)
	copy(result[:], b)
	return result, nil
}

// decodeCurveState unpacks balances(0), balances(1), A(), fee() into pool state.
// Curve fee() uses 1e10 denominator; we convert to bps (1e4 denominator).
func decodeCurveState(bal0Data, bal1Data, aData, feeData []byte, p *Pool) error {
	if len(bal0Data) == 0 || len(bal1Data) == 0 {
		return fmt.Errorf("empty Curve balance data")
	}
	unpackUint256 := func(data []byte, name string) (*big.Int, error) {
		if len(data) < 32 {
			return nil, fmt.Errorf("curve %s: short data (%d bytes)", name, len(data))
		}
		return new(big.Int).SetBytes(data[:32]), nil
	}
	b0, err := unpackUint256(bal0Data, "balances(0)")
	if err != nil {
		return err
	}
	b1, err := unpackUint256(bal1Data, "balances(1)")
	if err != nil {
		return err
	}
	p.Reserve0 = b0
	p.Reserve1 = b1

	if len(aData) >= 32 {
		A := new(big.Int).SetBytes(aData[:32])
		if A.IsUint64() {
			p.AmpFactor = A.Uint64()
		}
	}
	// Curve fee denominator is 1e10. Store both raw (for precise sim) and bps (for display).
	// fee=100000 → 0.001% → 0.1 bps → FeeBps rounds to 0 but CurveFee1e10 keeps precision.
	if len(feeData) >= 32 {
		feeRaw := new(big.Int).SetBytes(feeData[:32])
		if feeRaw.IsUint64() {
			p.CurveFee1e10 = feeRaw.Uint64()
		}
		feeBps := new(big.Int).Div(feeRaw, big.NewInt(1_000_000))
		if feeBps.IsUint64() {
			p.FeeBps = uint32(feeBps.Uint64())
		}
	}
	return nil
}

func decodeReserves(data []byte, p *Pool) error {
	if len(data) == 0 {
		return fmt.Errorf("empty reserves data")
	}
	vals, err := uniV2ReservesABI.Unpack("getReserves", data)
	if err != nil {
		return err
	}
	if len(vals) < 2 {
		return fmt.Errorf("reserves: insufficient values")
	}
	r0, _ := vals[0].(*big.Int)
	r1, _ := vals[1].(*big.Int)
	p.Reserve0 = r0
	p.Reserve1 = r1
	return nil
}

// calibrateCamelotFee derives the effective fee (LP + protocol) from the pair's
// getAmountOut response. Camelot V2 pairs charge a protocol fee on top of the LP
// fee, and older pairs don't expose it via any getter — only getAmountOut reflects
// the true total. We compute: effectiveFee = 1 - actualOut / rawXYKOut, then store
// FeeBps with the calibrated value so the simulator matches on-chain behavior.
// calibrateCamelotDirectionalFee derives the effective fee for one swap direction
// from the pair's getAmountOut. token0Dir=true means token0→token1 direction.
// Stores result in Token0FeeBps (for token0→token1) or Token1FeeBps (for token1→token0).
// For stable pairs where getAmountOut > rawXYK, marks the pool as stable and stores
// the fee as 0 for that direction (the stable math already gives the correct output).
func calibrateCamelotDirectionalFee(gaoData []byte, p *Pool, testAmt *big.Int, token0Dir bool) {
	if len(gaoData) < 32 || p.Reserve0 == nil || p.Reserve0.Sign() == 0 || p.Reserve1 == nil {
		return
	}
	actualOut := new(big.Int).SetBytes(gaoData[:32])
	if actualOut.Sign() == 0 || testAmt == nil || testAmt.Sign() == 0 {
		return
	}

	// Raw xy=k output (zero fee) for the given direction
	var reserveIn, reserveOut *big.Int
	if token0Dir {
		reserveIn, reserveOut = p.Reserve0, p.Reserve1
	} else {
		reserveIn, reserveOut = p.Reserve1, p.Reserve0
	}
	num := new(big.Int).Mul(testAmt, reserveOut)
	denom := new(big.Int).Add(reserveIn, testAmt)
	if denom.Sign() == 0 {
		return
	}
	rawOut := new(big.Int).Div(num, denom)
	if rawOut.Sign() == 0 {
		return
	}

	if rawOut.Cmp(actualOut) <= 0 {
		// actualOut >= rawOut: stable swap math gives better output than xy=k.
		// Flag pool as stable; fee for this direction is effectively 0
		// (the getAmountOut already accounts for everything).
		p.IsStable = true
		if token0Dir {
			p.Token0FeeBps = 0
		} else {
			p.Token1FeeBps = 0
		}
		// Reset FeeBps to a sane default for Solidly stable pairs (typically
		// ~0.04% = 4 bps). This corrects contaminated values from the
		// 10x-scale calibration bug that produced FeeBps=994 on stable pools.
		if p.FeeBps > 10 {
			p.FeeBps = 4
		}
		return
	}

	// effectiveFee = (rawOut - actualOut) * 100_000 / rawOut
	// Uses Camelot's native 100,000 denominator so the value can be fed
	// directly into CamelotSim (which divides by 100,000). Previous code
	// used 10,000 — a 10× scale error that made the sim apply 2.9 bps
	// instead of 29 bps (= 0.29%) for a typical 0.3% Camelot pool. The
	// 27 bps systematic over-estimate in the smoketest was exactly the
	// missing 27 bps of fee.
	diff := new(big.Int).Sub(rawOut, actualOut)
	feeBps := new(big.Int).Mul(diff, big.NewInt(100_000))
	feeBps.Div(feeBps, rawOut)
	if feeBps.IsUint64() && feeBps.Uint64() < 10_000 { // sanity cap at 10%
		fee := uint32(feeBps.Uint64())
		if token0Dir {
			p.Token0FeeBps = fee
		} else {
			p.Token1FeeBps = fee
		}
	}
}

// FetchV4PoolStates fetches sqrtPriceX96, tick, and liquidity for UniswapV4 pools
// via the StateView contract. UniV4 pools are stored inside the singleton PoolManager
// and indexed by poolId (bytes32) — there's no per-pool address, so the regular
// BatchUpdatePools path can't read them. This function fills that gap.
//
// Pools that already have fresh state from Swap events are still refreshed; the cost
// is small and the result is authoritative (no race with mempool ordering). On any
// per-call failure the pool is left untouched.
func FetchV4PoolStates(ctx context.Context, client *ethclient.Client, pools []*Pool, blockNum *big.Int) error {
	if len(pools) == 0 {
		return nil
	}
	var stampBlock uint64
	if blockNum != nil && blockNum.IsUint64() {
		stampBlock = blockNum.Uint64()
	}
	stateView := common.HexToAddress(V4StateViewAddress)

	// Build interleaved calls: [getSlot0(p1), getLiquidity(p1), getSlot0(p2), getLiquidity(p2), ...]
	// so a single multicall round-trip handles everything.
	var calls []call
	var v4Pools []*Pool
	for _, p := range pools {
		if p.DEX != DEXUniswapV4 || p.PoolID == "" {
			continue
		}
		poolIDBytes := common.HexToHash(p.PoolID)
		slot0Data, err := v4Slot0ABI.Pack("getSlot0", poolIDBytes)
		if err != nil {
			continue
		}
		liqData, err := v4LiquidityABI.Pack("getLiquidity", poolIDBytes)
		if err != nil {
			continue
		}
		calls = append(calls, call{Target: stateView, CallData: slot0Data})
		calls = append(calls, call{Target: stateView, CallData: liqData})
		v4Pools = append(v4Pools, p)
	}
	if len(calls) == 0 {
		return nil
	}

	results, err := doMulticall(ctx, client, calls, blockNum)
	if err != nil {
		return fmt.Errorf("v4 state multicall: %w", err)
	}
	if len(results) != 2*len(v4Pools) {
		return fmt.Errorf("v4 state multicall: expected %d results, got %d", 2*len(v4Pools), len(results))
	}

	updated := 0
	for i, p := range v4Pools {
		slot0Res := results[i*2]
		liqRes := results[i*2+1]
		if !slot0Res.Success || !liqRes.Success {
			continue
		}
		slot0Out, err := v4Slot0ABI.Unpack("getSlot0", slot0Res.ReturnData)
		if err != nil || len(slot0Out) < 4 {
			continue
		}
		liqOut, err := v4LiquidityABI.Unpack("getLiquidity", liqRes.ReturnData)
		if err != nil || len(liqOut) < 1 {
			continue
		}

		sqrtP, _ := slot0Out[0].(*big.Int)
		tick, _ := slot0Out[1].(*big.Int) // int24 unpacks to *big.Int from go-ethereum abi
		liq, _ := liqOut[0].(*big.Int)
		if sqrtP == nil || liq == nil {
			continue
		}

		p.mu.Lock()
		p.SqrtPriceX96 = sqrtP
		p.Liquidity = liq
		if tick != nil && tick.IsInt64() {
			p.Tick = int32(tick.Int64())
		}
		p.LastUpdated = time.Now()
		if stampBlock > p.StateBlock {
			p.StateBlock = stampBlock
		}
		p.mu.Unlock()
		// UpdateSpotPrice takes its own lock — must be called outside the locked section.
		p.UpdateSpotPrice()
		updated++
	}
	return nil
}

// TickFetchStats summarises one FetchTickMaps pass for callers and tests.
// Every field is populated before FetchTickMaps returns, regardless of
// whether the multicall itself errored — callers use these to distinguish
// transient failure (Err != nil || PoolsFailed > 0) from "nothing to fetch"
// (Pools == 0).
type TickFetchStats struct {
	Pools            int
	PoolsSucceeded   int
	PoolsEmpty       int    // verified empty bitmap (OK, but Ticks=nil)
	PoolsFailed      int    // bitmap or liquidity RPC call failed for at least one needed entry
	PoolsSkipped     int    // no tick spacing / no sqrtPrice / no poolID
	BitmapCalls      int
	BitmapSuccess    int
	TickLiqCalls     int
	TickLiqSuccess   int
	AlgebraRTMismatches int // Algebra round-trip reconstruction bugs detected
	Err              error
}

// FetchTickMaps fetches tick bitmap + liquidityNet data for the given V3 pools
// via Multicall at the given block. For each pool it scans ±bitmapRadius bitmap
// words around the current tick and then fetches ticks(int24).liquidityNet for
// each initialized tick. Results are stored in pool.Ticks (sorted by tick index).
//
// CRITICAL RELIABILITY INVARIANT: every pool in `pools` receives exactly one
// outcome stamp before this function returns (either via the per-pool success
// path OR the markFailure helper). A pool whose bitmap calls or tick-liq calls
// encountered ANY per-entry failure is stamped with TicksFetchOK=false so the
// coverage gate can reject it. Pools whose bitmap genuinely had no initialized
// ticks in the scanned range are stamped with TicksFetchOK=true and
// TicksFetchReason="empty-bitmap" — legitimately "checked, nothing in range".
// This distinction is the difference between "silently drop real opportunities
// during a 429 storm" and "only drop cycles whose bitmap we couldn't verify".
//
// Pass blockNum=nil for startup/test paths where the block stamp is irrelevant.
func FetchTickMaps(ctx context.Context, client *ethclient.Client, pools []*Pool, bitmapRadius int, blockNum *big.Int) (TickFetchStats, error) {
	stats := TickFetchStats{Pools: len(pools)}
	if len(pools) == 0 {
		return stats, nil
	}
	if bitmapRadius <= 0 {
		return stats, fmt.Errorf("FetchTickMaps: bitmapRadius must be > 0 (got %d) — config.tick_bitmap_coverage_words is mandatory", bitmapRadius)
	}
	var stampBlock uint64
	if blockNum != nil && blockNum.IsUint64() {
		stampBlock = blockNum.Uint64()
	}

	now := time.Now()

	// Per-pool tracking of the fetch lifecycle. The key is the Pool pointer's
	// address (stable for the life of the process); we can't use p.Address
	// because there could theoretically be two Pool instances with the same
	// address, and we want to key on object identity.
	type poolTrack struct {
		p                 *Pool
		bitmapTotal       int
		bitmapSuccess     int
		tickTotal         int
		tickSuccess       int
		algebraMismatches int
		initTicks         []int32
		nonEmptyWords     int
		skipped           bool
		skipReason        string
	}
	tracks := make(map[*Pool]*poolTrack, len(pools))
	for _, p := range pools {
		tracks[p] = &poolTrack{p: p}
	}

	// Record the attempt wall-clock time on every pool up front so the
	// test suite can detect pools currently being fetched even if the call
	// errors out before the per-pool outcome stamps are written.
	for _, p := range pools {
		p.mu.Lock()
		p.TicksFetchAttemptedAt = now
		p.mu.Unlock()
	}

	// markPool writes the final outcome of this FetchTickMaps pass onto the
	// pool atomically under p.mu.Lock(). tds==nil + ok==true means verified
	// empty; tds==nil + ok==false means fetch failure (do NOT simulate); a
	// populated tds always means ok==true.
	markPool := func(p *Pool, tds []TickData, ok bool, reason string, bmWords, nonEmpty int) {
		p.mu.Lock()
		defer p.mu.Unlock()
		p.Ticks = tds
		p.TicksUpdatedAt = now
		p.TickAtFetch = p.Tick
		p.TicksBlock = stampBlock
		p.TicksWordRadius = int16(bitmapRadius)
		p.TicksFetchOK = ok
		p.TicksFetchReason = reason
		p.TicksFetchBitmapWords = int16(bmWords)
		p.TicksFetchNonEmptyWords = int16(nonEmpty)
		if ok {
			p.TicksFetchFailureCount = 0
		} else {
			p.TicksFetchFailureCount++
		}
	}

	// ── Phase 1: fetch tick bitmap words ──
	type bitmapReq struct {
		pool    *Pool
		wordPos int16
	}
	var bmCalls []call
	var bmReqs []bitmapReq

	stateView := common.HexToAddress(V4StateViewAddress)
	for _, p := range pools {
		tr := tracks[p]
		p.mu.RLock()
		ts := p.TickSpacing
		sqrtP := p.SqrtPriceX96
		dex := p.DEX
		poolID := p.PoolID
		curTick := p.Tick
		addrStr := p.Address
		p.mu.RUnlock()
		if ts <= 0 {
			tr.skipped = true
			tr.skipReason = "no-tick-spacing"
			continue
		}
		if sqrtP == nil {
			tr.skipped = true
			tr.skipReason = "no-sqrt-price"
			continue
		}
		if dex == DEXUniswapV4 && poolID == "" {
			tr.skipped = true
			tr.skipReason = "v4-no-poolid"
			continue
		}
		var centerWord int16
		if dex == DEXCamelotV3 || dex == DEXZyberV3 {
			centerWord = int16(curTick >> 8)
		} else {
			centerWord = int16((curTick / ts) >> 8)
		}

		var poolIDBytes common.Hash
		if dex == DEXUniswapV4 {
			poolIDBytes = common.HexToHash(poolID)
		}
		addr := common.HexToAddress(addrStr)
		expectedWords := 0
		for w := centerWord - int16(bitmapRadius); w <= centerWord+int16(bitmapRadius); w++ {
			var data []byte
			var err error
			switch {
			case dex == DEXUniswapV4:
				data, err = v4TickBitmapABI.Pack("getTickBitmap", poolIDBytes, w)
			case dex == DEXCamelotV3 || dex == DEXZyberV3:
				data, err = algebraTickTableABI.Pack("tickTable", w)
			default:
				data, err = tickBitmapABI.Pack("tickBitmap", w)
			}
			if err != nil {
				continue
			}
			target := addr
			if dex == DEXUniswapV4 {
				target = stateView
			}
			bmCalls = append(bmCalls, call{Target: target, CallData: data})
			bmReqs = append(bmReqs, bitmapReq{pool: p, wordPos: w})
			expectedWords++
		}
		tr.bitmapTotal = expectedWords
	}

	stats.BitmapCalls = len(bmCalls)
	if len(bmCalls) == 0 {
		for _, p := range pools {
			tr := tracks[p]
			if tr.skipped {
				markPool(p, nil, false, tr.skipReason, 0, 0)
				stats.PoolsSkipped++
			} else {
				markPool(p, nil, false, "no-bitmap-calls", 0, 0)
				stats.PoolsFailed++
			}
		}
		return stats, nil
	}

	bmResults, err := doMulticall(ctx, client, bmCalls, blockNum)
	if err != nil {
		for _, p := range pools {
			tr := tracks[p]
			if tr.skipped {
				markPool(p, nil, false, tr.skipReason, 0, 0)
				stats.PoolsSkipped++
			} else {
				markPool(p, nil, false, "bitmap-rpc-failed", tr.bitmapTotal, 0)
				stats.PoolsFailed++
			}
		}
		stats.Err = fmt.Errorf("tick bitmap multicall: %w", err)
		return stats, stats.Err
	}

	// Parse bitmap results, tracking per-pool bitmap success and non-empty words.
	for i, res := range bmResults {
		req := bmReqs[i]
		tr := tracks[req.pool]
		if !res.Success {
			continue
		}
		if len(res.ReturnData) < 32 {
			continue
		}
		tr.bitmapSuccess++
		bitmap := new(big.Int).SetBytes(res.ReturnData[:32])
		if bitmap.Sign() == 0 {
			continue
		}
		tr.nonEmptyWords++
		algebra := req.pool.DEX == DEXCamelotV3 || req.pool.DEX == DEXZyberV3
		ts := req.pool.TickSpacing
		for bit := 0; bit < 256; bit++ {
			if bitmap.Bit(bit) == 0 {
				continue
			}
			var realTick int32
			if algebra {
				realTick = int32(req.wordPos)*256 + int32(bit)
				// Algebra round-trip validation: recovering the wordPos + bit
				// from realTick MUST reproduce req.wordPos and bit. Previously
				// there was no check, so any off-by-one in word indexing would
				// silently corrupt every recovered tick and mis-sim every
				// CamelotV3 pool. The check is near-free (two shifts + cmp).
				rtWord := int16(realTick >> 8)
				rtBit := int(uint32(realTick) & 0xff)
				if rtWord != req.wordPos || rtBit != bit {
					tr.algebraMismatches++
					stats.AlgebraRTMismatches++
					continue
				}
			} else {
				realTick = (int32(req.wordPos)*256 + int32(bit)) * ts
				// UniV3/V4 round-trip: compressed = realTick / ts; compressed>>8 == wordPos
				// and compressed & 0xff == bit. A mismatch would imply a tick
				// spacing that doesn't divide the recovered tick — never valid.
				if ts != 0 {
					compressed := realTick / ts
					rtWord := int16(compressed >> 8)
					rtBit := int(uint32(compressed) & 0xff)
					if rtWord != req.wordPos || rtBit != bit || realTick%ts != 0 {
						tr.algebraMismatches++
						stats.AlgebraRTMismatches++
						continue
					}
				}
			}
			tr.initTicks = append(tr.initTicks, realTick)
		}
	}
	stats.BitmapSuccess = 0
	for _, tr := range tracks {
		stats.BitmapSuccess += tr.bitmapSuccess
	}

	// ── Phase 2: fetch liquidityNet for each initialized tick ──
	type poolTickReq struct {
		pool *Pool
		tick int32
	}
	var tickCalls []call
	var tickReqs []poolTickReq

	for _, p := range pools {
		tr := tracks[p]
		if tr.skipped {
			continue
		}
		if len(tr.initTicks) == 0 {
			continue
		}
		addr := common.HexToAddress(p.Address)
		var poolIDBytes common.Hash
		if p.DEX == DEXUniswapV4 {
			poolIDBytes = common.HexToHash(p.PoolID)
		}
		for _, t := range tr.initTicks {
			var data []byte
			var err error
			target := addr
			switch {
			case p.DEX == DEXUniswapV4:
				data, err = v4TickLiqABI.Pack("getTickLiquidity", poolIDBytes, big.NewInt(int64(t)))
				target = stateView
			case p.DEX == DEXCamelotV3 || p.DEX == DEXZyberV3:
				data, err = algebraTicksABI.Pack("ticks", big.NewInt(int64(t)))
			default:
				data, err = tickInfoABI.Pack("ticks", big.NewInt(int64(t)))
			}
			if err != nil {
				continue
			}
			tickCalls = append(tickCalls, call{Target: target, CallData: data})
			tickReqs = append(tickReqs, poolTickReq{pool: p, tick: t})
			tr.tickTotal++
		}
	}
	stats.TickLiqCalls = len(tickCalls)

	// Even if there are no tick calls (all bitmaps empty), we still need to
	// finalize per-pool outcomes. A pool whose bitmap calls ALL succeeded but
	// yielded zero initialized ticks is "verified empty" and OK; a pool whose
	// bitmap calls partially failed is marked as fetch-failure so the sim gate
	// rejects it.
	if len(tickCalls) == 0 {
		for _, p := range pools {
			tr := tracks[p]
			if tr.skipped {
				markPool(p, nil, false, tr.skipReason, tr.bitmapTotal, 0)
				stats.PoolsSkipped++
			} else if tr.bitmapSuccess < tr.bitmapTotal {
				markPool(p, nil, false, "bitmap-rpc-failed", tr.bitmapTotal, tr.nonEmptyWords)
				stats.PoolsFailed++
			} else if tr.algebraMismatches > 0 {
				markPool(p, nil, false, "algebra-roundtrip", tr.bitmapTotal, tr.nonEmptyWords)
				stats.PoolsFailed++
			} else {
				markPool(p, nil, true, "empty-bitmap", tr.bitmapTotal, tr.nonEmptyWords)
				stats.PoolsEmpty++
				stats.PoolsSucceeded++
			}
		}
		return stats, nil
	}

	tickResults, err := doMulticall(ctx, client, tickCalls, blockNum)
	if err != nil {
		for _, p := range pools {
			tr := tracks[p]
			if tr.skipped {
				markPool(p, nil, false, tr.skipReason, tr.bitmapTotal, tr.nonEmptyWords)
				stats.PoolsSkipped++
			} else {
				markPool(p, nil, false, "ticks-rpc-failed", tr.bitmapTotal, tr.nonEmptyWords)
				stats.PoolsFailed++
			}
		}
		stats.Err = fmt.Errorf("tick info multicall: %w", err)
		return stats, stats.Err
	}

	type tickEntry struct {
		tick   int32
		liqNet *big.Int
	}
	poolTicks := make(map[*Pool][]tickEntry)
	for i, res := range tickResults {
		req := tickReqs[i]
		tr := tracks[req.pool]
		if !res.Success || len(res.ReturnData) < 64 {
			continue
		}
		var liqNet *big.Int
		var decodeErr error
		switch {
		case req.pool.DEX == DEXUniswapV4:
			vals, uerr := v4TickLiqABI.Unpack("getTickLiquidity", res.ReturnData)
			if uerr != nil || len(vals) < 2 {
				decodeErr = fmt.Errorf("v4 unpack")
				break
			}
			liqNet, _ = vals[1].(*big.Int)
		case req.pool.DEX == DEXCamelotV3 || req.pool.DEX == DEXZyberV3:
			vals, uerr := algebraTicksABI.Unpack("ticks", res.ReturnData)
			if uerr != nil || len(vals) < 2 {
				decodeErr = fmt.Errorf("algebra unpack")
				break
			}
			liqNet, _ = vals[1].(*big.Int)
		default:
			vals, uerr := tickInfoABI.Unpack("ticks", res.ReturnData)
			if uerr != nil || len(vals) < 2 {
				decodeErr = fmt.Errorf("v3 unpack")
				break
			}
			liqNet, _ = vals[1].(*big.Int)
		}
		if decodeErr != nil {
			continue
		}
		tr.tickSuccess++
		// liqNet == 0 is a legitimate value for a tick that used to be
		// initialized but has been burned back to zero. Skip these so the
		// sim doesn't try to cross them — but still count them as
		// successful reads so the per-pool gate doesn't flag a fetch
		// failure when the bitmap and the ticks() table disagree.
		if liqNet == nil || liqNet.Sign() == 0 {
			continue
		}
		poolTicks[req.pool] = append(poolTicks[req.pool], tickEntry{tick: req.tick, liqNet: new(big.Int).Set(liqNet)})
	}

	for _, tr := range tracks {
		stats.TickLiqSuccess += tr.tickSuccess
	}

	// Finalize per-pool outcomes.
	for _, p := range pools {
		tr := tracks[p]
		if tr.skipped {
			markPool(p, nil, false, tr.skipReason, tr.bitmapTotal, tr.nonEmptyWords)
			stats.PoolsSkipped++
			continue
		}
		if tr.bitmapSuccess < tr.bitmapTotal {
			markPool(p, nil, false, "bitmap-rpc-failed", tr.bitmapTotal, tr.nonEmptyWords)
			stats.PoolsFailed++
			continue
		}
		if tr.algebraMismatches > 0 {
			markPool(p, nil, false, "algebra-roundtrip", tr.bitmapTotal, tr.nonEmptyWords)
			stats.PoolsFailed++
			continue
		}
		if tr.tickSuccess < tr.tickTotal {
			markPool(p, nil, false, "ticks-rpc-failed", tr.bitmapTotal, tr.nonEmptyWords)
			stats.PoolsFailed++
			continue
		}
		entries := poolTicks[p]
		if len(entries) == 0 {
			markPool(p, nil, true, "empty-bitmap", tr.bitmapTotal, tr.nonEmptyWords)
			stats.PoolsEmpty++
			stats.PoolsSucceeded++
			continue
		}
		for i := 0; i < len(entries); i++ {
			for j := i + 1; j < len(entries); j++ {
				if entries[j].tick < entries[i].tick {
					entries[i], entries[j] = entries[j], entries[i]
				}
			}
		}
		tds := make([]TickData, len(entries))
		for i, e := range entries {
			tds[i] = TickData{Tick: e.tick, LiquidityNet: e.liqNet}
		}
		markPool(p, tds, true, "", tr.bitmapTotal, tr.nonEmptyWords)
		stats.PoolsSucceeded++
	}

	return stats, nil
}

// doMulticall executes a batch of calls via Multicall2.tryAggregate and returns results.
// blockNum==nil means latest block.
func doMulticall(ctx context.Context, client *ethclient.Client, calls []call, blockNum *big.Int) ([]multicallResult, error) {
	// Split into batches of 500 to stay under gas limits
	const batchSize = 500
	var allResults []multicallResult

	for start := 0; start < len(calls); start += batchSize {
		end := start + batchSize
		if end > len(calls) {
			end = len(calls)
		}
		batch := calls[start:end]

		type mcCall struct {
			Target   common.Address
			CallData []byte
		}
		mc := make([]mcCall, len(batch))
		for i, c := range batch {
			mc[i] = mcCall{Target: c.Target, CallData: c.CallData}
		}

		packed, err := multicall2ABI.Pack("tryAggregate", false, mc)
		if err != nil {
			return nil, fmt.Errorf("pack tryAggregate: %w", err)
		}

		mcAddr := common.HexToAddress(Multicall2Address)
		msg := ethereum.CallMsg{To: &mcAddr, Data: packed}
		out, err := client.CallContract(ctx, msg, blockNum)
		if err != nil {
			return nil, fmt.Errorf("call multicall: %w", err)
		}

		var decoded struct {
			ReturnData []multicallResult
		}
		if err := multicall2ABI.UnpackIntoInterface(&decoded, "tryAggregate", out); err != nil {
			return nil, fmt.Errorf("unpack tryAggregate: %w", err)
		}
		allResults = append(allResults, decoded.ReturnData...)
	}

	return allResults, nil
}

type multicallResult struct {
	Success    bool
	ReturnData []byte
}

// doMulticallParallel is the concurrent version of doMulticall. Each batch
// is dispatched as its own eth_call via a goroutine; results are stitched
// back together in batch order so callers still see a flat result slice
// matching the input order. Wall-clock latency collapses from
// sum(batch_RTT) to max(batch_RTT). This is the primary reason the per-block
// state refresh now finishes in ~100ms instead of ~1s on Chainstack.
//
// On any single-batch error the first error short-circuits the whole pass.
func doMulticallParallel(ctx context.Context, client *ethclient.Client, calls []call, blockNum *big.Int) ([]multicallResult, error) {
	const batchSize = 500
	if len(calls) == 0 {
		return nil, nil
	}
	var batches [][]call
	for start := 0; start < len(calls); start += batchSize {
		end := start + batchSize
		if end > len(calls) {
			end = len(calls)
		}
		batches = append(batches, calls[start:end])
	}

	results := make([][]multicallResult, len(batches))
	errs := make([]error, len(batches))
	var wg sync.WaitGroup
	wg.Add(len(batches))
	for i := range batches {
		go func(i int, batch []call) {
			defer wg.Done()
			type mcCall struct {
				Target   common.Address
				CallData []byte
			}
			mc := make([]mcCall, len(batch))
			for j, c := range batch {
				mc[j] = mcCall{Target: c.Target, CallData: c.CallData}
			}
			packed, err := multicall2ABI.Pack("tryAggregate", false, mc)
			if err != nil {
				errs[i] = fmt.Errorf("pack tryAggregate: %w", err)
				return
			}
			mcAddr := common.HexToAddress(Multicall2Address)
			msg := ethereum.CallMsg{To: &mcAddr, Data: packed}
			out, err := client.CallContract(ctx, msg, blockNum)
			if err != nil {
				errs[i] = fmt.Errorf("call multicall: %w", err)
				return
			}
			var decoded struct {
				ReturnData []multicallResult
			}
			if err := multicall2ABI.UnpackIntoInterface(&decoded, "tryAggregate", out); err != nil {
				errs[i] = fmt.Errorf("unpack tryAggregate: %w", err)
				return
			}
			results[i] = decoded.ReturnData
		}(i, batches[i])
	}
	wg.Wait()
	for _, e := range errs {
		if e != nil {
			return nil, e
		}
	}
	var flat []multicallResult
	for _, r := range results {
		flat = append(flat, r...)
	}
	return flat, nil
}

// packUint256 big-endian-encodes a uint64 into the 32-byte layout expected
// by decodeCurveState for the A() and fee() slots. Used when those
// immutables are cached on the Pool and not re-fetched this pass.
func packUint256(v uint64) []byte {
	buf := make([]byte, 32)
	big.NewInt(int64(v)).FillBytes(buf)
	return buf
}
