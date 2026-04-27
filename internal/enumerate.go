package internal

// EnumerateFactoryPools performs a historical eth_getLogs scan across all known
// DEX factories to discover every pool created since fromBlock.
//
// Strategy:
//  1. Scan each factory concurrently, chunked into enumChunkSize-block ranges.
//  2. Pre-filter: skip pools where neither token is in the TokenRegistry.
//  3. Return stubs — caller must call BatchUpdatePools to populate state, then
//     discard pools with zero liquidity/reserves before adding to the registry.

import (
	"context"
	"fmt"
	"log"
	"math/big"
	"strings"
	"sync"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/ethclient"
)

const (
	enumChunkSize = 100_000 // blocks per eth_getLogs request (Chainstack allows up to 100k)
	enumWorkers   = 6       // max concurrent factory scanners
)

// algebraPoolCreatedABI covers Algebra Integral's PoolCreated event (used by Camelot V3).
// Signature: PoolCreated(address indexed token0, address indexed token1, address pool)
// (no fee or tickSpacing compared to UniV3)
var algebraPoolCreatedABI abi.ABI

func init() {
	var err error
	algebraPoolCreatedABI, err = abi.JSON(strings.NewReader(`[{
		"anonymous":false,
		"name":"PoolCreated",
		"type":"event",
		"inputs":[
			{"indexed":true, "name":"token0","type":"address"},
			{"indexed":true, "name":"token1","type":"address"},
			{"indexed":false,"name":"pool",  "type":"address"}
		]
	}]`))
	if err != nil {
		panic(fmt.Sprintf("algebraPoolCreatedABI: %v", err))
	}
}

type factoryEventType uint8

const (
	factoryEventV3       factoryEventType = iota // UniV3 PoolCreated (token0, token1, fee indexed)
	factoryEventV2                               // UniV2 PairCreated (token0, token1 indexed)
	factoryEventAlgebra                          // Algebra PoolCreated (token0, token1 indexed, pool non-indexed)
)

type factorySpec struct {
	Address     string
	DEXType     DEXType
	EventType   factoryEventType
	FeeBps      uint32 // fixed fee for V2-style factories; 0 = read from event
	DeployBlock uint64 // first block containing a factory event (skip earlier blocks)
}

// allFactorySpecs lists every DEX factory we want to enumerate on Arbitrum.
var allFactorySpecs = []factorySpec{
	// ── V3 (UniV3-compatible PoolCreated) ───────────────────────────────────────
	{
		Address:     "0x1f98431c8ad98523631ae4a59f267346ea31f984",
		DEXType:     DEXUniswapV3,
		EventType:   factoryEventV3,
		DeployBlock: 165,
	},
	{
		Address:     "0x1af415a1eba07a4986a52b6f2e7de7003d82231e",
		DEXType:     DEXSushiSwapV3,
		EventType:   factoryEventV3,
		DeployBlock: 101_030_000,
	},
	{
		Address:     "0x0bfbcf9fa4f9c56b0f40a671ad40e0805a091865",
		DEXType:     DEXPancakeV3,
		EventType:   factoryEventV3,
		DeployBlock: 101_000_000,
	},
	// ── Algebra (CamelotV3) ──────────────────────────────────────────────────────
	{
		Address:     "0x6dd3fb9653b10e806650f107c3b5a0a6ff974f65",
		DEXType:     DEXCamelotV3,
		EventType:   factoryEventAlgebra,
		DeployBlock: 56_000_000,
	},
	// ── V2 (UniV2-compatible PairCreated) ───────────────────────────────────────
	{
		Address:     "0x6eccab422d763ac031210895c81787e87b43a652",
		DEXType:     DEXCamelot,
		EventType:   factoryEventV2,
		FeeBps:      30,
		DeployBlock: 3_237_671,
	},
	{
		Address:     "0x8909dc15e40173ff4699343b6eb8132c65e18ec6",
		DEXType:     DEXTraderJoe,
		EventType:   factoryEventV2,
		FeeBps:      30,
		DeployBlock: 3_500_000,
	},
	{
		Address:     "0xc35dadb65012ec5796536bd9864ed8773abc74c4",
		DEXType:     DEXSushiSwap,
		EventType:   factoryEventV2,
		FeeBps:      30,
		DeployBlock: 175_804,
	},
}

// EnumerateFactoryPools scans all known factories for pools created between
// fromBlock and the current chain head. Returns pool stubs with no state.
// Pools are pre-filtered: at least one token must be in the TokenRegistry.
func EnumerateFactoryPools(ctx context.Context, client *ethclient.Client, tokens *TokenRegistry, fromBlock uint64) []*Pool {
	header, err := client.HeaderByNumber(ctx, nil)
	if err != nil {
		log.Printf("[enumerate] get current block: %v", err)
		return nil
	}
	toBlock := header.Number.Uint64()

	v3Topic := poolCreatedABI.Events["PoolCreated"].ID
	v2Topic := pairCreatedABI.Events["PairCreated"].ID
	algTopic := algebraPoolCreatedABI.Events["PoolCreated"].ID

	var mu sync.Mutex
	seen := make(map[string]bool)
	var allPools []*Pool

	sem := make(chan struct{}, enumWorkers)
	var wg sync.WaitGroup

	for _, spec := range allFactorySpecs {
		spec := spec

		start := fromBlock
		if spec.DeployBlock > start {
			start = spec.DeployBlock
		}
		if start > toBlock {
			continue // factory didn't exist in the scan window
		}

		var topic common.Hash
		switch spec.EventType {
		case factoryEventV3:
			topic = v3Topic
		case factoryEventV2:
			topic = v2Topic
		case factoryEventAlgebra:
			topic = algTopic
		}

		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			factoryAddr := common.HexToAddress(spec.Address)
			var found []*Pool
			total := 0

			for blk := start; blk <= toBlock; {
				end := blk + enumChunkSize - 1
				if end > toBlock {
					end = toBlock
				}

				logs, err := filterLogsWithRetry(ctx, client, ethereum.FilterQuery{
					FromBlock: new(big.Int).SetUint64(blk),
					ToBlock:   new(big.Int).SetUint64(end),
					Addresses: []common.Address{factoryAddr},
					Topics:    [][]common.Hash{{topic}},
				})
				if err != nil {
					log.Printf("[enumerate] %s blocks %d-%d: %v (skipping chunk)", spec.DEXType, blk, end, err)
					blk = end + 1
					continue
				}

				for _, lg := range logs {
					p := parseFactoryLog(lg, spec, tokens)
					if p != nil {
						found = append(found, p)
					}
				}
				total += len(logs)
				blk = end + 1
			}

			mu.Lock()
			added := 0
			for _, p := range found {
				addr := strings.ToLower(p.Address)
				if !seen[addr] {
					seen[addr] = true
					allPools = append(allPools, p)
					added++
				}
			}
			mu.Unlock()
			log.Printf("[enumerate] %s: %d events → %d new pools", spec.DEXType, total, added)
		}()
	}

	wg.Wait()
	return allPools
}

// filterLogsWithRetry tries eth_getLogs; on error it halves the range and retries once.
func filterLogsWithRetry(ctx context.Context, client *ethclient.Client, q ethereum.FilterQuery) ([]types.Log, error) {
	logs, err := client.FilterLogs(ctx, q)
	if err == nil {
		return logs, nil
	}

	// Retry with half the block range
	from := q.FromBlock.Uint64()
	to := q.ToBlock.Uint64()
	if to-from < 1000 {
		return nil, fmt.Errorf("block range too small to split: %w", err)
	}
	mid := (from + to) / 2

	q1 := q
	q1.FromBlock = new(big.Int).SetUint64(from)
	q1.ToBlock = new(big.Int).SetUint64(mid)
	logs1, err1 := client.FilterLogs(ctx, q1)
	if err1 != nil {
		return nil, fmt.Errorf("retry first half: %w", err1)
	}

	q2 := q
	q2.FromBlock = new(big.Int).SetUint64(mid + 1)
	q2.ToBlock = new(big.Int).SetUint64(to)
	logs2, err2 := client.FilterLogs(ctx, q2)
	if err2 != nil {
		return nil, fmt.Errorf("retry second half: %w", err2)
	}

	return append(logs1, logs2...), nil
}

// parseFactoryLog converts a single PoolCreated/PairCreated log into a Pool stub.
// Returns nil if the pool should be skipped (unknown tokens, bad data).
func parseFactoryLog(lg types.Log, spec factorySpec, tokens *TokenRegistry) *Pool {
	switch spec.EventType {
	case factoryEventV3:
		return parseV3PoolCreated(lg, spec.DEXType, tokens)
	case factoryEventV2:
		return parseV2PairCreated(lg, spec.DEXType, spec.FeeBps, tokens)
	case factoryEventAlgebra:
		return parseAlgebraPoolCreated(lg, spec.DEXType, tokens)
	}
	return nil
}

func parseV3PoolCreated(lg types.Log, dex DEXType, tokens *TokenRegistry) *Pool {
	if len(lg.Topics) < 4 {
		return nil
	}
	token0Addr := strings.ToLower(common.HexToAddress(lg.Topics[1].Hex()).Hex())
	token1Addr := strings.ToLower(common.HexToAddress(lg.Topics[2].Hex()).Hex())
	feePips := uint32(lg.Topics[3].Big().Uint64())

	t0, t1 := resolveTokenPair(token0Addr, token1Addr, tokens)
	if t0 == nil && t1 == nil {
		return nil // neither token known — skip
	}
	if t0 == nil {
		t0 = &Token{Address: token0Addr, Symbol: shortAddr(token0Addr), Decimals: 18}
	}
	if t1 == nil {
		t1 = &Token{Address: token1Addr, Symbol: shortAddr(token1Addr), Decimals: 18}
	}

	if len(lg.Data) < 64 {
		return nil
	}
	poolAddr := strings.ToLower(common.HexToAddress(common.BytesToHash(lg.Data[32:64]).Hex()).Hex())
	if poolAddr == "0x0000000000000000000000000000000000000000" {
		return nil
	}

	// TVL/Volume start at 0 — populated by multicall (reserves) + subgraph (metrics).
	return &Pool{
		Address:      poolAddr,
		DEX:          dex,
		FeeBps:       feePips / 100,
		Token0:       t0,
		Token1:       t1,
		TVLUSD:       0,
		Volume24hUSD: 0,
	}
}

func parseV2PairCreated(lg types.Log, dex DEXType, feeBps uint32, tokens *TokenRegistry) *Pool {
	if len(lg.Topics) < 3 {
		return nil
	}
	token0Addr := strings.ToLower(common.HexToAddress(lg.Topics[1].Hex()).Hex())
	token1Addr := strings.ToLower(common.HexToAddress(lg.Topics[2].Hex()).Hex())

	t0, t1 := resolveTokenPair(token0Addr, token1Addr, tokens)
	if t0 == nil && t1 == nil {
		return nil
	}
	if t0 == nil {
		t0 = &Token{Address: token0Addr, Symbol: shortAddr(token0Addr), Decimals: 18}
	}
	if t1 == nil {
		t1 = &Token{Address: token1Addr, Symbol: shortAddr(token1Addr), Decimals: 18}
	}

	if len(lg.Data) < 32 {
		return nil
	}
	pairAddr := strings.ToLower(common.HexToAddress(common.BytesToHash(lg.Data[0:32]).Hex()).Hex())
	if pairAddr == "0x0000000000000000000000000000000000000000" {
		return nil
	}

	return &Pool{
		Address:      pairAddr,
		DEX:          dex,
		FeeBps:       feeBps,
		Token0:       t0,
		Token1:       t1,
		TVLUSD:       0,
		Volume24hUSD: 0,
	}
}

func parseAlgebraPoolCreated(lg types.Log, dex DEXType, tokens *TokenRegistry) *Pool {
	// Algebra: topics[1]=token0, topics[2]=token1, data=pool address
	if len(lg.Topics) < 3 {
		return nil
	}
	token0Addr := strings.ToLower(common.HexToAddress(lg.Topics[1].Hex()).Hex())
	token1Addr := strings.ToLower(common.HexToAddress(lg.Topics[2].Hex()).Hex())

	t0, t1 := resolveTokenPair(token0Addr, token1Addr, tokens)
	if t0 == nil && t1 == nil {
		return nil
	}
	if t0 == nil {
		t0 = &Token{Address: token0Addr, Symbol: shortAddr(token0Addr), Decimals: 18}
	}
	if t1 == nil {
		t1 = &Token{Address: token1Addr, Symbol: shortAddr(token1Addr), Decimals: 18}
	}

	if len(lg.Data) < 32 {
		return nil
	}
	poolAddr := strings.ToLower(common.HexToAddress(common.BytesToHash(lg.Data[0:32]).Hex()).Hex())
	if poolAddr == "0x0000000000000000000000000000000000000000" {
		return nil
	}

	return &Pool{
		Address:      poolAddr,
		DEX:          dex,
		FeeBps:       0, // Algebra fee is dynamic per-pool; will be set by globalState decode
		Token0:       t0,
		Token1:       t1,
		TVLUSD:       0,
		Volume24hUSD: 0,
	}
}

// resolveTokenPair returns (t0, t1) from the registry; either may be nil if unknown.
// Returns (nil, nil) only if both are unknown.
func resolveTokenPair(addr0, addr1 string, tokens *TokenRegistry) (*Token, *Token) {
	t0, _ := tokens.Get(addr0)
	t1, _ := tokens.Get(addr1)
	return t0, t1
}

func shortAddr(addr string) string {
	if len(addr) > 10 {
		return addr[:6] + "…" + addr[len(addr)-4:]
	}
	return addr
}

// isPoolAlive returns true if the pool has non-zero liquidity/reserves after a state fetch.
func isPoolAlive(p *Pool) bool {
	switch p.DEX {
	case DEXUniswapV3, DEXSushiSwapV3, DEXRamsesV3, DEXPancakeV3, DEXCamelotV3, DEXZyberV3:
		return p.SqrtPriceX96 != nil && p.SqrtPriceX96.Sign() > 0 &&
			p.Liquidity != nil && p.Liquidity.Sign() > 0
	default:
		return p.Reserve0 != nil && p.Reserve0.Sign() > 0 &&
			p.Reserve1 != nil && p.Reserve1.Sign() > 0
	}
}
