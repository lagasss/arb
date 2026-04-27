package internal

import (
	"context"
	"fmt"
	"log"
	"math"
	"math/big"
	"strings"
	"time"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/ethclient"
)

var (
	wstETHRateABI   abi.ABI
	aavePoolRateABI abi.ABI
	erc4626RateABI  abi.ABI
	chainlinkABI    abi.ABI
)

func init() {
	var err error
	wstETHRateABI, err = abi.JSON(strings.NewReader(`[{
		"name":"stEthPerToken","type":"function","stateMutability":"view",
		"inputs":[],"outputs":[{"name":"","type":"uint256"}]
	}]`))
	if err != nil {
		panic(fmt.Sprintf("wstETHRateABI: %v", err))
	}
	aavePoolRateABI, err = abi.JSON(strings.NewReader(`[{
		"name":"getReserveNormalizedIncome","type":"function","stateMutability":"view",
		"inputs":[{"name":"asset","type":"address"}],
		"outputs":[{"name":"","type":"uint256"}]
	}]`))
	if err != nil {
		panic(fmt.Sprintf("aavePoolRateABI: %v", err))
	}
	erc4626RateABI, err = abi.JSON(strings.NewReader(`[{
		"name":"convertToAssets","type":"function","stateMutability":"view",
		"inputs":[{"name":"shares","type":"uint256"}],
		"outputs":[{"name":"","type":"uint256"}]
	}]`))
	if err != nil {
		panic(fmt.Sprintf("erc4626RateABI: %v", err))
	}
	// Chainlink AggregatorV3Interface
	chainlinkABI, err = abi.JSON(strings.NewReader(`[{
		"name":"latestRoundData","type":"function","stateMutability":"view",
		"inputs":[],
		"outputs":[
			{"name":"roundId","type":"uint80"},
			{"name":"answer","type":"int256"},
			{"name":"startedAt","type":"uint256"},
			{"name":"updatedAt","type":"uint256"},
			{"name":"answeredInRound","type":"uint80"}
		]
	},{
		"name":"decimals","type":"function","stateMutability":"view",
		"inputs":[],"outputs":[{"name":"","type":"uint8"}]
	}]`))
	if err != nil {
		panic(fmt.Sprintf("chainlinkABI: %v", err))
	}
}

// YieldPairCfg configures a yield-bearing / base-token pair to monitor.
type YieldPairCfg struct {
	BaseAddr     string  `yaml:"base_address"`
	WrapperAddr  string  `yaml:"wrapper_address"`
	Protocol     string  `yaml:"protocol"`           // "lido_wsteth" | "aave_v3" | "erc4626" | "chainlink"
	RateContract string  `yaml:"rate_contract"`      // pool/feed/vault contract address
	MinDivBps    float64 `yaml:"min_divergence_bps"` // per-pair override; 0 = use global
	// MaxStaleSecs sets how old a Chainlink answer can be before it is ignored (0 = no check).
	MaxStaleSecs int64 `yaml:"max_stale_secs"`
}

// YieldPegMonitor polls protocol exchange rates and compares them to AMM spot prices.
// When an AMM misprices a yield-bearing token relative to its true redemption value,
// it logs the divergence as a trade opportunity.
type YieldPegMonitor struct {
	client       *ethclient.Client
	registry     *PoolRegistry
	tokens       *TokenRegistry
	pairs        []YieldPairCfg
	minDivBps    float64
	minProfitUSD float64
}

func NewYieldPegMonitor(
	client *ethclient.Client,
	registry *PoolRegistry,
	tokens *TokenRegistry,
	pairs []YieldPairCfg,
	minDivBps, minProfitUSD float64,
) *YieldPegMonitor {
	return &YieldPegMonitor{
		client:       client,
		registry:     registry,
		tokens:       tokens,
		pairs:        pairs,
		minDivBps:    minDivBps,
		minProfitUSD: minProfitUSD,
	}
}

func (m *YieldPegMonitor) Run(ctx context.Context) {
	log.Println("[yield-peg] monitor started")
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	m.checkAll(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			m.checkAll(ctx)
		}
	}
}

func (m *YieldPegMonitor) checkAll(ctx context.Context) {
	for _, pair := range m.pairs {
		m.checkPair(ctx, pair)
	}
}

func (m *YieldPegMonitor) checkPair(ctx context.Context, pair YieldPairCfg) {
	base, okB := m.tokens.Get(pair.BaseAddr)
	wrapper, okW := m.tokens.Get(pair.WrapperAddr)
	if !okB || !okW {
		return
	}

	trueRate, err := m.fetchTrueRate(ctx, pair, base, wrapper)
	if err != nil {
		log.Printf("[yield-peg] rate error (%s/%s): %v", wrapper.Symbol, base.Symbol, err)
		return
	}
	if trueRate <= 0 {
		return
	}

	for _, p := range m.registry.All() {
		wrapIsT0 := strings.EqualFold(p.Token0.Address, wrapper.Address)
		wrapIsT1 := strings.EqualFold(p.Token1.Address, wrapper.Address)
		baseIsT0 := strings.EqualFold(p.Token0.Address, base.Address)
		baseIsT1 := strings.EqualFold(p.Token1.Address, base.Address)

		if !((wrapIsT0 && baseIsT1) || (wrapIsT1 && baseIsT0)) {
			continue
		}

		spot := p.SpotRate() // token1 per token0, human-readable
		if spot == 0 {
			continue
		}

		// Normalize to "base per wrapper"
		var ammRate float64
		if wrapIsT0 && baseIsT1 {
			ammRate = spot
		} else {
			ammRate = 1.0 / spot
		}

		// Sanity check: ammRate should be within 30% of trueRate for a yield-bearing token.
		// A larger gap means the pool has stale/uninitialized price data — skip it.
		if ammRate < trueRate*0.7 || ammRate > trueRate*1.3 {
			continue
		}

		// divBps > 0: wrapper underpriced on AMM (buy on AMM, redeem at protocol)
		// divBps < 0: wrapper overpriced on AMM (mint at protocol, sell on AMM)
		divBps := (trueRate - ammRate) / trueRate * 10_000
		threshold := m.minDivBps
		if pair.MinDivBps > 0 {
			threshold = pair.MinDivBps
		}
		if math.Abs(divBps) < threshold {
			continue
		}

		// Rough profit estimate assuming $100k capital deployment.
		profitUSD := 100_000.0 * math.Abs(divBps) / 10_000
		if profitUSD < m.minProfitUSD {
			continue
		}

		direction := "underpriced"
		if divBps < 0 {
			direction = "overpriced"
		}
		short := p.Address
		if len(short) > 10 {
			short = short[:10] + "..."
		}
		log.Printf("[yield-peg] %s/%s pool=%s trueRate=%.6f ammRate=%.6f div=%.2fbps (%s) ~$%.0f",
			wrapper.Symbol, base.Symbol, short,
			trueRate, ammRate, divBps, direction, profitUSD)
	}
}

// fetchTrueRate returns the protocol exchange rate as "base per wrapper" in human-readable units.
func (m *YieldPegMonitor) fetchTrueRate(ctx context.Context, pair YieldPairCfg, base, wrapper *Token) (float64, error) {
	switch pair.Protocol {
	case "lido_wsteth":
		return m.fetchLidoRate(ctx, wrapper.Address)
	case "aave_v3":
		if pair.RateContract == "" {
			return 0, fmt.Errorf("aave_v3 requires rate_contract")
		}
		return m.fetchAaveRate(ctx, pair.RateContract, base.Address)
	case "erc4626":
		return m.fetchERC4626Rate(ctx, wrapper.Address, wrapper.Decimals, base.Decimals)
	case "chainlink":
		if pair.RateContract == "" {
			return 0, fmt.Errorf("chainlink requires rate_contract (feed address)")
		}
		return m.fetchChainlinkRate(ctx, pair.RateContract, pair.MaxStaleSecs)
	default:
		return 0, fmt.Errorf("unknown protocol: %q", pair.Protocol)
	}
}

// fetchLidoRate reads wstETH.stEthPerToken() — stETH wei per wstETH wei.
// Returns stETH (≈ WETH) per wstETH as float64, e.g. 1.19.
func (m *YieldPegMonitor) fetchLidoRate(ctx context.Context, wstETHAddr string) (float64, error) {
	data, err := wstETHRateABI.Pack("stEthPerToken")
	if err != nil {
		return 0, err
	}
	addr := common.HexToAddress(wstETHAddr)
	res, err := m.client.CallContract(ctx, ethereum.CallMsg{To: &addr, Data: data}, nil)
	if err != nil {
		return 0, fmt.Errorf("stEthPerToken: %w", err)
	}
	vals, err := wstETHRateABI.Unpack("stEthPerToken", res)
	if err != nil || len(vals) == 0 {
		return 0, fmt.Errorf("stEthPerToken unpack: %w", err)
	}
	rate, _ := vals[0].(*big.Int)
	f, _ := new(big.Float).SetInt(rate).Float64()
	return f / 1e18, nil
}

// fetchAaveRate reads AavePool.getReserveNormalizedIncome(asset) — RAY-scaled (1e27).
// Returns base tokens per aToken, e.g. 1.05 means 5% yield accrued.
func (m *YieldPegMonitor) fetchAaveRate(ctx context.Context, poolAddr, assetAddr string) (float64, error) {
	data, err := aavePoolRateABI.Pack("getReserveNormalizedIncome", common.HexToAddress(assetAddr))
	if err != nil {
		return 0, err
	}
	addr := common.HexToAddress(poolAddr)
	res, err := m.client.CallContract(ctx, ethereum.CallMsg{To: &addr, Data: data}, nil)
	if err != nil {
		return 0, fmt.Errorf("getReserveNormalizedIncome: %w", err)
	}
	vals, err := aavePoolRateABI.Unpack("getReserveNormalizedIncome", res)
	if err != nil || len(vals) == 0 {
		return 0, fmt.Errorf("getReserveNormalizedIncome unpack: %w", err)
	}
	ray, _ := vals[0].(*big.Int)
	f, _ := new(big.Float).Quo(
		new(big.Float).SetInt(ray),
		new(big.Float).SetFloat64(1e27),
	).Float64()
	return f, nil
}

// fetchChainlinkRate reads latestRoundData() from a Chainlink AggregatorV3 feed.
// Returns the answer scaled to a float64 (e.g. 1.195 for wstETH/ETH).
// maxStaleSecs > 0 rejects answers older than that many seconds.
func (m *YieldPegMonitor) fetchChainlinkRate(ctx context.Context, feedAddr string, maxStaleSecs int64) (float64, error) {
	addr := common.HexToAddress(feedAddr)

	// Fetch decimals
	decData, err := chainlinkABI.Pack("decimals")
	if err != nil {
		return 0, err
	}
	decRes, err := m.client.CallContract(ctx, ethereum.CallMsg{To: &addr, Data: decData}, nil)
	if err != nil {
		return 0, fmt.Errorf("chainlink decimals: %w", err)
	}
	decVals, err := chainlinkABI.Unpack("decimals", decRes)
	if err != nil || len(decVals) == 0 {
		return 0, fmt.Errorf("chainlink decimals unpack: %w", err)
	}
	decimals, _ := decVals[0].(uint8)

	// Fetch latest round data
	roundData, err := chainlinkABI.Pack("latestRoundData")
	if err != nil {
		return 0, err
	}
	roundRes, err := m.client.CallContract(ctx, ethereum.CallMsg{To: &addr, Data: roundData}, nil)
	if err != nil {
		return 0, fmt.Errorf("chainlink latestRoundData: %w", err)
	}
	roundVals, err := chainlinkABI.Unpack("latestRoundData", roundRes)
	if err != nil || len(roundVals) < 4 {
		return 0, fmt.Errorf("chainlink latestRoundData unpack: %w", err)
	}
	answer, _ := roundVals[1].(*big.Int)
	updatedAt, _ := roundVals[3].(*big.Int)

	if answer == nil || answer.Sign() <= 0 {
		return 0, fmt.Errorf("chainlink: invalid answer")
	}

	if maxStaleSecs > 0 && updatedAt != nil {
		age := time.Now().Unix() - updatedAt.Int64()
		if age > maxStaleSecs {
			return 0, fmt.Errorf("chainlink: stale feed (age=%ds, max=%ds)", age, maxStaleSecs)
		}
	}

	f, _ := new(big.Float).SetInt(answer).Float64()
	return f / math.Pow(10, float64(decimals)), nil
}

// fetchERC4626Rate reads convertToAssets(1 wrapper unit) from an ERC-4626 vault.
// Returns base per wrapper in human-readable units, e.g. 1.08 sUSDe → 1.08 USDe.
func (m *YieldPegMonitor) fetchERC4626Rate(ctx context.Context, wrapperAddr string, wrapperDecimals, baseDecimals uint8) (float64, error) {
	oneWrapper := new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(wrapperDecimals)), nil)
	data, err := erc4626RateABI.Pack("convertToAssets", oneWrapper)
	if err != nil {
		return 0, err
	}
	addr := common.HexToAddress(wrapperAddr)
	res, err := m.client.CallContract(ctx, ethereum.CallMsg{To: &addr, Data: data}, nil)
	if err != nil {
		return 0, fmt.Errorf("convertToAssets: %w", err)
	}
	vals, err := erc4626RateABI.Unpack("convertToAssets", res)
	if err != nil || len(vals) == 0 {
		return 0, fmt.Errorf("convertToAssets unpack: %w", err)
	}
	assets, _ := vals[0].(*big.Int)
	f, _ := new(big.Float).SetInt(assets).Float64()
	return f / math.Pow(10, float64(baseDecimals)), nil
}
