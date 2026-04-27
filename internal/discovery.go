package internal

import (
	"context"
	"fmt"
	"log"
	"strings"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/ethclient"
)

// FactoryCategory tells the discovery loop which event ABI to decode for a
// given factory. Three shapes exist on Arbitrum:
//   - "v2"        → UniV2-style PairCreated (token0, token1, pair)
//   - "v3"        → UniV3-style PoolCreated (token0, token1, fee, tickSpacing, pool)
//   - "v3Algebra" → Algebra PoolCreated (token0, token1, pool) — no fee/tickSpacing
type FactoryCategory string

const (
	FactoryV2        FactoryCategory = "v2"
	FactoryV3        FactoryCategory = "v3"
	FactoryV3Algebra FactoryCategory = "v3Algebra"
)

// FactoryMeta is everything the bot and arbscan need to know about a factory:
// which DEX math to dispatch to (DEX), the human-readable label for display
// (Label — may differ from DEX.String() for forks like "SolidLizard" that run
// UniV2 math), and which event ABI to decode (Category).
type FactoryMeta struct {
	DEX      DEXType
	Label    string
	Category FactoryCategory
}

// Factories is the single source of truth for factory → (DEX, Label, Category).
// Both the bot (via the derived v2/v3/v3Algebra maps below) and arbscan (which
// imports this map directly for labelling) read from here. To add a factory:
// append one line. Category drives which discovery-event decoder runs.
//
// Forks that run identical UniV2 math (SolidLizard, MMFinance, ApeSwap, etc.)
// map to DEX=DEXUniswapV2 but keep their own Label for display. Forks that
// require genuinely different math (TraderJoe Liquidity Book, KyberSwap
// Elastic, etc.) are intentionally omitted — adding them here would dispatch
// to the wrong simulator.
var Factories = map[string]FactoryMeta{
	// ── UniV3-style (standard PoolCreated event) ─────────────────────────────
	"0x1f98431c8ad98523631ae4a59f267346ea31f984": {DEXUniswapV3, "UniV3", FactoryV3},
	"0x1af415a1eba07a4986a52b6f2e7de7003d82231e": {DEXSushiSwapV3, "SushiV3", FactoryV3},
	"0x0bfbcf9fa4f9c56b0f40a671ad40e0805a091865": {DEXPancakeV3, "PancakeV3", FactoryV3},
	"0xaaa16c016bf556fcf9a55ba26f56c5b03babc37c": {DEXRamsesV3, "RamsesV3", FactoryV3},
	"0xd0019e86edb35e1fedaab03aed5c3c60f115d28b": {DEXRamsesV3, "RamsesV3", FactoryV3}, // alt factory
	"0xf26bd9ba435395f26634c9be5b717c6d10675897": {DEXRamsesV3, "RamsesV3", FactoryV3}, // CL
	"0x582e08a1b9c02f21684e690a27ebee07e27fbafe": {DEXUniswapV3, "SummaSwapV3", FactoryV3},
	"0xf4d0512f461de21e3569c183b49d59fc2b1d2935": {DEXUniswapV3, "UnknownV3", FactoryV3},

	// ── Algebra V3 (Camelot and compatible) ──────────────────────────────────
	"0x6dd3fb9653b10e806650f107c3b5a0a6ff974f65": {DEXCamelotV3, "CamelotV3", FactoryV3Algebra},
	"0x1a3c9b1d2f0529d97f2afc5136cc23e58f1fd35b": {DEXCamelotV3, "CamelotV3", FactoryV3Algebra}, // alt (pinned pool 0x3ab5dd69…)

	// ── UniV2-style (standard PairCreated event, xy=k math) ──────────────────
	// WARNING: mapping a fork's factory to DEXUniswapV2 is a resolver-only
	// operation — it lets the bot *observe* pools for competitor analysis and
	// cycle cache visibility, but it does NOT make the executor able to trade
	// through them. The executor's _swapV2 dispatches to whichever router is
	// in dexRouter[DEXUniswapV2] (the canonical Uniswap V2 router), which has
	// no knowledge of fork pairs — any submitted cycle through a fork pool
	// would revert inside Uniswap's router. Pause-trading guards this today;
	// if trading resumes, each fork needs its own DEX type + router entry in
	// executor.go dexRouter before pools through it are allowed into the
	// cycle cache. Entries below are historical and retained as-is.
	"0x6eccab422d763ac031210895c81787e87b43a652": {DEXCamelot, "Camelot", FactoryV2},
	"0xc35dadb65012ec5796536bd9864ed8773abc74c4": {DEXSushiSwap, "Sushi", FactoryV2},
	"0xaa2cd7477c451e703f3b9ba5663334914763edf8": {DEXRamsesV2, "RamsesV2", FactoryV2},
	"0xf1d7cc64fb4452f05c498126312ebe29f30fbcf9": {DEXUniswapV2, "UniV2", FactoryV2},
	"0xd394e9cc20f43d2651293756f8d320668e850f1b": {DEXArbSwap, "ArbSwap", FactoryV2},
	"0x359f20ad0f42d75a5077e65f30274cabe6f4f01a": {DEXSwapr, "Swapr", FactoryV2},
	"0x734d84631f00dc0d3fcd18b04b6cf42bfd407074": {DEXUniswapV2, "SolidLizard", FactoryV2}, // ⚠ no dedicated router
	"0xcb85e1222f715a81b8edaeb73b28182fa37cffa8": {DEXDeltaSwap, "DeltaSwap", FactoryV2},
	"0x947bc57cefdd22420c9a6d61387fe4d4cf8a090d": {DEXUniswapV2, "MMFinance", FactoryV2},   // ⚠ no dedicated router
	"0xac2ee06a14c52570ef3b9812ed240bce359772e7": {DEXUniswapV2, "ZyberSwapV2", FactoryV2}, // ⚠ no dedicated router
	"0x5b1c257b88537d1ce2af55a1760336288ccd28b6": {DEXUniswapV2, "Horiza", FactoryV2},      // ⚠ no dedicated router
	"0x02a84c1b3bbd7401a5f7fa98a384ebc70bb5749e": {DEXUniswapV2, "PancakeV2", FactoryV2},   // ⚠ no dedicated router — sim-only; see project_pancakev2_executor.md
	"0x44b678f3d1438197e4a1ecb2c46afe73e67a6760": {DEXUniswapV2, "UnknownV2", FactoryV2},
	"0xc8b66e63589c966b7ba9151e36038c2b26e14a16": {DEXUniswapV2, "UnknownV2", FactoryV2},
	"0xd158bd9e8b6efd3ca76830b66715aa2b7bad2218": {DEXUniswapV2, "UnknownV2", FactoryV2},
	"0xf7a23b9a9dcb8d0aff67012565c5844c20c11afc": {DEXUniswapV2, "UnknownV2", FactoryV2},
	"0x6ef065576497e9764289b6adf3f9bc35745ead1d": {DEXUniswapV2, "UnknownV2", FactoryV2},

	// ── Intentionally omitted until router + validation work is done ────────
	// Each of these has been observed in competitor trades but the executor
	// can't trade through them yet. Enabling requires: (a) dedicated DEXType
	// with matching dexRouter entry, (b) Solidity dispatch case or a verified
	// "calls UniswapV2Router interface" assumption, (c) VerifyPool coverage.
	//
	//   0x1c6e968f…  ArbIdex           (needs dedicated router)
	//   0xd490f2f6…  CamelotV3 2nd     (probably safe — same router — add with verification)
	//   0x20fafd2b…  OreoSwap          (needs dedicated router)
	//   0x71539d09…  SwapFish          (needs dedicated router)
	//   0x7c7f1c8e…  MindGames         (needs dedicated router)
	//   0xa59b2044…  ElkFinance        (needs dedicated router)
	//   0xcf083be4…  ApeSwap           (needs dedicated router)
	//   0xf51d966d…  Aegis             (needs dedicated router)
	//   0xfe369930…  MMFinance V2 alt  (needs dedicated router)
	//   0xfe8ec10f…  SpartaDex         (needs dedicated router)
	//
	// Math-incompatible (different simulator required):
	//   0x8909dc15…  TraderJoe V2 Liquidity Book
	//   0x9c2c8910…  TraderJoe V2 Liquidity Book
	//   0xae4ec990…  TraderJoe V2 alt Liquidity Book
	//   0x5f1dddbf…  KyberSwap Elastic
	//   0xc7a59029…  KyberSwap Elastic v2
	//   0x70fe4a44…  SolidlyCom CL
	//   0x9c2abd63…  ZyberSwapV3 (Algebra fork)
}

// v2FactoryAddresses, v3FactoryAddresses, v3AlgebraFactoryAddresses, and
// FactoryAddresses are derived views over Factories. They stay around so the
// existing discovery and resolver code paths don't need to change.
var (
	v2FactoryAddresses        = map[string]DEXType{}
	v3FactoryAddresses        = map[string]DEXType{}
	v3AlgebraFactoryAddresses = map[string]DEXType{}
	FactoryAddresses          = map[string]DEXType{}
)

func init() {
	for k, v := range Factories {
		FactoryAddresses[k] = v.DEX
		switch v.Category {
		case FactoryV2:
			v2FactoryAddresses[k] = v.DEX
		case FactoryV3:
			v3FactoryAddresses[k] = v.DEX
		case FactoryV3Algebra:
			v3AlgebraFactoryAddresses[k] = v.DEX
		}
	}
}

var poolCreatedABI abi.ABI
var pairCreatedABI abi.ABI

func init() {
	var err error
	poolCreatedABI, err = abi.JSON(strings.NewReader(`[{
		"anonymous":false,
		"name":"PoolCreated",
		"type":"event",
		"inputs":[
			{"indexed":true,"name":"token0","type":"address"},
			{"indexed":true,"name":"token1","type":"address"},
			{"indexed":true,"name":"fee","type":"uint24"},
			{"indexed":false,"name":"tickSpacing","type":"int24"},
			{"indexed":false,"name":"pool","type":"address"}
		]
	}]`))
	if err != nil {
		panic(fmt.Sprintf("poolCreatedABI: %v", err))
	}

	pairCreatedABI, err = abi.JSON(strings.NewReader(`[{
		"anonymous":false,
		"name":"PairCreated",
		"type":"event",
		"inputs":[
			{"indexed":true,"name":"token0","type":"address"},
			{"indexed":true,"name":"token1","type":"address"},
			{"indexed":false,"name":"pair","type":"address"},
			{"indexed":false,"name":"","type":"uint256"}
		]
	}]`))
	if err != nil {
		panic(fmt.Sprintf("pairCreatedABI: %v", err))
	}
}

// Discoverer subscribes to PoolCreated/PairCreated events and adds new pools to the registry.
type Discoverer struct {
	client      *ethclient.Client
	tokens      *TokenRegistry
	registry    *PoolRegistry
	graph       *Graph
	onPoolAdded func() // optional; called when a new pool is added
}

func NewDiscoverer(client *ethclient.Client, tokens *TokenRegistry, registry *PoolRegistry, graph *Graph) *Discoverer {
	return &Discoverer{client: client, tokens: tokens, registry: registry, graph: graph}
}

func (d *Discoverer) Run(ctx context.Context) {
	allFactoryAddrs := make([]common.Address, 0, len(v3FactoryAddresses)+len(v3AlgebraFactoryAddresses)+len(v2FactoryAddresses))
	for addr := range v3FactoryAddresses {
		allFactoryAddrs = append(allFactoryAddrs, common.HexToAddress(addr))
	}
	for addr := range v3AlgebraFactoryAddresses {
		allFactoryAddrs = append(allFactoryAddrs, common.HexToAddress(addr))
	}
	for addr := range v2FactoryAddresses {
		allFactoryAddrs = append(allFactoryAddrs, common.HexToAddress(addr))
	}

	// UniV3 and Algebra both emit PoolCreated but with different topic signatures.
	// V2 emits PairCreated. Subscribe to all three topics across all factories.
	query := ethereum.FilterQuery{
		Addresses: allFactoryAddrs,
		Topics: [][]common.Hash{{
			poolCreatedABI.Events["PoolCreated"].ID,
			algebraPoolCreatedABI.Events["PoolCreated"].ID,
			pairCreatedABI.Events["PairCreated"].ID,
		}},
	}

	logs := make(chan types.Log, 64)
	sub, err := d.client.SubscribeFilterLogs(ctx, query, logs)
	if err != nil {
		log.Printf("[discovery] subscribe error: %v", err)
		return
	}
	defer sub.Unsubscribe()

	log.Println("[discovery] watching for new pools...")
	for {
		select {
		case <-ctx.Done():
			return
		case err := <-sub.Err():
			log.Printf("[discovery] subscription error: %v", err)
			return
		case lg := <-logs:
			d.handleLog(ctx, lg)
		}
	}
}

func (d *Discoverer) handleLog(ctx context.Context, lg types.Log) {
	if len(lg.Topics) == 0 {
		return
	}
	switch lg.Topics[0] {
	case poolCreatedABI.Events["PoolCreated"].ID:
		d.handleV3Log(ctx, lg)
	case algebraPoolCreatedABI.Events["PoolCreated"].ID:
		d.handleAlgebraLog(ctx, lg)
	case pairCreatedABI.Events["PairCreated"].ID:
		d.handleV2Log(ctx, lg)
	}
}

func (d *Discoverer) handleV3Log(ctx context.Context, lg types.Log) {
	factory := strings.ToLower(lg.Address.Hex())
	dexType, ok := v3FactoryAddresses[factory]
	if !ok {
		return
	}

	if len(lg.Topics) < 4 {
		return
	}
	token0 := common.HexToAddress(lg.Topics[1].Hex())
	token1 := common.HexToAddress(lg.Topics[2].Hex())
	feePips := uint32(lg.Topics[3].Big().Uint64())

	var poolAddr common.Address
	if len(lg.Data) >= 64 {
		poolAddr = common.HexToAddress(common.BytesToHash(lg.Data[32:64]).Hex())
	}
	if poolAddr == (common.Address{}) {
		return
	}

	t0 := d.resolveToken(ctx, token0)
	t1 := d.resolveToken(ctx, token1)

	p := &Pool{
		Address:      strings.ToLower(poolAddr.Hex()),
		DEX:          dexType,
		FeeBps:       feePips / 100,
		Token0:       t0,
		Token1:       t1,
		// TVL/Volume start at 0 — populated by multicall + subgraph refresh.
		TVLUSD:       0,
		Volume24hUSD: 0,
	}

	if canonical, ok := d.registry.Add(p); ok {
		d.graph.AddPool(canonical)
		log.Printf("[discovery] new V3 pool: %s %s/%s fee=%dbps", canonical.DEX, t0.Symbol, t1.Symbol, canonical.FeeBps)
		if d.onPoolAdded != nil {
			d.onPoolAdded()
		}
	}
}

func (d *Discoverer) handleAlgebraLog(ctx context.Context, lg types.Log) {
	factory := strings.ToLower(lg.Address.Hex())
	dexType, ok := v3AlgebraFactoryAddresses[factory]
	if !ok {
		return
	}

	// Algebra PoolCreated: topics[1]=token0, topics[2]=token1, data[0:32]=pool address
	if len(lg.Topics) < 3 {
		return
	}
	token0 := common.HexToAddress(lg.Topics[1].Hex())
	token1 := common.HexToAddress(lg.Topics[2].Hex())

	var poolAddr common.Address
	if len(lg.Data) >= 32 {
		poolAddr = common.HexToAddress(common.BytesToHash(lg.Data[0:32]).Hex())
	}
	if poolAddr == (common.Address{}) {
		return
	}

	t0 := d.resolveToken(ctx, token0)
	t1 := d.resolveToken(ctx, token1)

	p := &Pool{
		Address:      strings.ToLower(poolAddr.Hex()),
		DEX:          dexType,
		FeeBps:       0, // Algebra fee is dynamic; set by globalState on first multicall
		Token0:       t0,
		Token1:       t1,
		// TVL/Volume start at 0 — populated by multicall + subgraph refresh.
		TVLUSD:       0,
		Volume24hUSD: 0,
	}

	if canonical, ok := d.registry.Add(p); ok {
		d.graph.AddPool(canonical)
		log.Printf("[discovery] new Algebra pool: %s %s/%s", canonical.DEX, t0.Symbol, t1.Symbol)
		if d.onPoolAdded != nil {
			d.onPoolAdded()
		}
	}
}

func (d *Discoverer) handleV2Log(ctx context.Context, lg types.Log) {
	factory := strings.ToLower(lg.Address.Hex())
	dexType, ok := v2FactoryAddresses[factory]
	if !ok {
		return
	}

	if len(lg.Topics) < 3 {
		return
	}
	token0 := common.HexToAddress(lg.Topics[1].Hex())
	token1 := common.HexToAddress(lg.Topics[2].Hex())

	var pairAddr common.Address
	if len(lg.Data) >= 32 {
		pairAddr = common.HexToAddress(common.BytesToHash(lg.Data[0:32]).Hex())
	}
	if pairAddr == (common.Address{}) {
		return
	}

	t0 := d.resolveToken(ctx, token0)
	t1 := d.resolveToken(ctx, token1)

	p := &Pool{
		Address:      strings.ToLower(pairAddr.Hex()),
		DEX:          dexType,
		FeeBps:       30,
		Token0:       t0,
		Token1:       t1,
		// TVL/Volume start at 0 — populated by multicall + subgraph refresh.
		TVLUSD:       0,
		Volume24hUSD: 0,
	}

	if canonical, ok := d.registry.Add(p); ok {
		d.graph.AddPool(canonical)
		log.Printf("[discovery] new V2 pool: %s %s/%s fee=%dbps", canonical.DEX, t0.Symbol, t1.Symbol, canonical.FeeBps)
		if d.onPoolAdded != nil {
			d.onPoolAdded()
		}
	}
}

func (d *Discoverer) resolveToken(ctx context.Context, addr common.Address) *Token {
	if t, ok := d.tokens.Get(addr.Hex()); ok {
		return t
	}
	fetched := FetchTokenMeta(ctx, d.client, addr.Hex())
	d.tokens.Add(fetched)
	return fetched
}
