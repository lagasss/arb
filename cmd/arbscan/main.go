// arbscan: captures live arbitrage transactions on Arbitrum by watching for
// transactions with 2+ swap events (V2 or V3). Works regardless of flash loan
// source (own capital, Uniswap V3 flash swap, Aave, Balancer, etc.).
// Output: arb.db competitor_arbs table (SQLite, same db as the bot)
//         /tmp/arbscan_summary.txt (written on exit)
package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log"
	"math"
	"math/big"
	"net/http"
	"os"
	"os/signal"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"arb-bot/internal"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethclient"
)

// ── Known tokens (address → symbol, decimals) ───────────────────────────────

type tokenMeta struct {
	Symbol   string
	Decimals uint8
}

var knownTokens = map[string]tokenMeta{
	"0x82af49447d8a07e3bd95bd0d56f35241523fbab1": {"WETH", 18},
	"0xff970a61a04b1ca14834a43f5de4533ebddb5cc8": {"USDC.e", 6},
	"0xaf88d065e77c8cc2239327c5edb3a432268e5831": {"USDC", 6},
	"0xfd086bc7cd5c481dcc9c85ebe478a1c0b69fcbb9": {"USDT", 6},
	"0x912ce59144191c1204e64559fe8253a0e49e6548": {"ARB", 18},
	"0xda10009cbd5d07dd0cecc66161fc93d7c9000da1": {"DAI", 18},
	"0x2f2a2543b76a4166549f7aab2e75bef0aefc5b0f": {"WBTC", 8},
	"0x5979d7b546e38e414f7e9822514be443a4800529": {"wstETH", 18},
	"0xec70dcb4a1efa46b8f2d97c310c9c4790ba5ffu8": {"rETH", 18},
	"0x17fc002b466eec40dae837fc4be5c67993ddbd6f": {"FRAX", 18},
	"0xfc5a1a6eb076a2c7ad06ed22c90d7e710e35ad0a": {"GMX", 18},
	"0x3d9907f9a368ad0a51be60f7da3b97cf940982d8": {"GRAIL", 18},
	"0x539bde0d7dbd336b79148aa742883198bbf60342": {"MAGIC", 18},
	"0x0c4681e6c0235179ec3d4f4fc4df3d14fdd96017": {"RDNT", 18},
	"0xf97f4df75117a78c1a5a0dbb814af92458539fb4": {"LINK", 18},
	"0x11cdb42b0eb46d95f990bedd4695a6e3fa034978": {"CRV", 18},
	"0x6694340fc020c5e6b96567843da2df01b2ce1eb6": {"STG", 18},
	"0x4d15a3a2286d883af0aa1b3f21367843fac63e07": {"TUSD", 18},
	"0x13ad51ed4f1b7e9dc168d8a00cb3f4ddd85efa60": {"LDO", 18},
}

// Factory → display label is sourced from internal.Factories (the bot's
// authoritative list, shared between bot and arbscan so they can't drift).
// arbscanOnlyLabels covers factories arbscan watches but the bot's simulator
// can't trade yet (different math): KyberSwap Elastic, SolidlyCom CL,
// ZyberV3 Algebra, TraderJoe Liquidity Book. If/when bot support lands,
// move the entry to internal.Factories.
var arbscanOnlyLabels = map[string]string{
	"0x9c2c8910f113181783c249d8f6aa41b51cde0f0c": "TJoe",        // Liquidity Book
	"0xae4ec9901c3076d0ddbe76a520f9e90a6227acb7": "TJoe",        // Liquidity Book
	"0x5f1dddbf348ac2fbe22a163e30f99f9ece3dd50a": "KyberSwap",   // Elastic CL
	"0xc7a590291e07b9fe9e64b86c58fd8fc764308c4a": "KyberSwap",   // Elastic CL
	"0x70fe4a44ea505cfa3a57b95cf2862d4fd5f0f687": "SolidlyCom",  // CL
	"0x9c2abd632771b433e5e7507bcaa41ca3b25d8544": "ZyberSwapV3", // Algebra fork
}

// factoryLabel returns the display label for a factory address. It prefers
// internal.Factories (shared with the bot) and falls back to arbscanOnlyLabels.
func factoryLabel(factoryLo string) (string, bool) {
	if m, ok := internal.Factories[factoryLo]; ok {
		return m.Label, true
	}
	if l, ok := arbscanOnlyLabels[factoryLo]; ok {
		return l, true
	}
	return "", false
}

// ── Event topics ─────────────────────────────────────────────────────────────

var (
	swapV2Topic    = crypto.Keccak256Hash([]byte("Swap(address,uint256,uint256,uint256,uint256,address)"))
	swapV3Topic    = crypto.Keccak256Hash([]byte("Swap(address,address,int256,int256,uint160,uint128,int24)"))
	swapAlgTopic   = crypto.Keccak256Hash([]byte("Swap(address,address,int256,int256,uint160,uint128,int24,uint128,uint128)"))
	transferTopic  = crypto.Keccak256Hash([]byte("Transfer(address,address,uint256)"))

	// Curve: emitted by individual pool contracts
	curveExchangeTopic            = crypto.Keccak256Hash([]byte("TokenExchange(address,int128,uint256,int128,uint256)"))
	curveExchangeUnderlyingTopic  = crypto.Keccak256Hash([]byte("TokenExchangeUnderlying(address,int128,uint256,int128,uint256)"))

	// Balancer V2: emitted by the singleton Vault (not the pool contract)
	// Signature: Swap(bytes32 poolId, address tokenIn, address tokenOut, uint256 amountIn, uint256 amountOut)
	balancerSwapTopic = crypto.Keccak256Hash([]byte("Swap(bytes32,address,address,uint256,uint256)"))

	// DODO V2: emitted by individual pool contracts
	dodoSwapTopic = crypto.Keccak256Hash([]byte("DODOSwap(address,address,uint256,uint256,address,address)"))

	// Uniswap V4: both emitted by the singleton PoolManager
	// Initialize fires once per pool at creation — used to seed poolId→tokens cache.
	// Swap carries only poolId (bytes32); tokens are resolved from the cache.
	v4InitializeTopic = crypto.Keccak256Hash([]byte("Initialize(bytes32,address,address,uint24,int24,address,uint160,int24)"))
	v4SwapTopic       = crypto.Keccak256Hash([]byte("Swap(bytes32,address,int128,int128,uint160,uint128,int24,uint24)"))

	// Aave V3 flash loan
	aaveFlashTopic = crypto.Keccak256Hash([]byte("FlashLoan(address,address,address,uint256,uint8,uint256,uint16)"))
	// Balancer flash loan
	balFlashTopic  = crypto.Keccak256Hash([]byte("FlashLoan(address,address,uint256,uint256)"))

	balancerVault = strings.ToLower("0xBA12222222228d8Ba445958a75a0704d566BF2C8")
	aavePool      = strings.ToLower("0x794a61358D6845594F94dc1DB02A252b5b4814aD")
	v4PoolManager    = strings.ToLower("0x360E68faCcca8cA495c1B759Fd9EEe466db9FB32")
	v4StartBlock  uint64 = 297_842_872 // block at which V4 was deployed on Arbitrum
)

// ── ABI helpers ───────────────────────────────────────────────────────────────

var (
	erc20ABI  abi.ABI
	poolABIs  abi.ABI
	curveABI  abi.ABI
)

func initABIs() {
	var err error
	erc20ABI, err = abi.JSON(strings.NewReader(`[
		{"name":"decimals","type":"function","stateMutability":"view","inputs":[],"outputs":[{"name":"","type":"uint8"}]},
		{"name":"symbol","type":"function","stateMutability":"view","inputs":[],"outputs":[{"name":"","type":"string"}]}
	]`))
	if err != nil { panic(err) }
	poolABIs, err = abi.JSON(strings.NewReader(`[
		{"name":"token0","type":"function","stateMutability":"view","inputs":[],"outputs":[{"name":"","type":"address"}]},
		{"name":"token1","type":"function","stateMutability":"view","inputs":[],"outputs":[{"name":"","type":"address"}]},
		{"name":"fee","type":"function","stateMutability":"view","inputs":[],"outputs":[{"name":"","type":"uint24"}]},
		{"name":"factory","type":"function","stateMutability":"view","inputs":[],"outputs":[{"name":"","type":"address"}]}
	]`))
	if err != nil { panic(err) }
	curveABI, err = abi.JSON(strings.NewReader(`[
		{"name":"coins","type":"function","stateMutability":"view","inputs":[{"name":"i","type":"uint256"}],"outputs":[{"name":"","type":"address"}]}
	]`))
	if err != nil { panic(err) }
}

// getCurveCoins returns the token address at index idx for a Curve pool.
// Results are cached in-memory for the lifetime of the process.
func getCurveCoins(ctx context.Context, c *ethclient.Client, poolAddr string, idx int64) string {
	key := fmt.Sprintf("%s:%d", strings.ToLower(poolAddr), idx)
	curveCoinsMu.Lock()
	if cached, ok := curveCoinsCache[key]; ok {
		curveCoinsMu.Unlock()
		return cached
	}
	curveCoinsMu.Unlock()

	a := common.HexToAddress(poolAddr)
	d, err := curveABI.Pack("coins", new(big.Int).SetInt64(idx))
	if err != nil { return "" }
	tctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	res, err := c.CallContract(tctx, ethereum.CallMsg{To: &a, Data: d}, nil)
	if err != nil || len(res) < 32 { return "" }
	vals, err := curveABI.Unpack("coins", res)
	if err != nil || len(vals) == 0 { return "" }
	addr, ok := vals[0].(common.Address)
	if !ok { return "" }
	result := strings.ToLower(addr.Hex())

	curveCoinsMu.Lock()
	curveCoinsCache[key] = result
	curveCoinsMu.Unlock()
	return result
}

// setV4Cache writes a poolId → tokens mapping to both the in-memory cache and the DB.
// db may be nil (skips DB write). poolId must be a common.Hash.
func setV4Cache(poolId common.Hash, tok0, tok1 string, db *internal.DB) {
	v4CacheMu.Lock()
	v4Cache[poolId] = [2]string{tok0, tok1}
	v4CacheMu.Unlock()
	if db != nil {
		db.UpsertV4PoolToken(poolId.Hex(), tok0, tok1)
	}
}

// fetchV4PoolsFromAPI queries the Uniswap gateway for the top V4 pools on Arbitrum
// and populates v4Cache with their poolId → (token0, token1) mappings.
// The gateway returns IDs as base64-encoded "V4Pool:ARBITRUM_{poolId_hex}" strings.
// Native ETH pools (token0.address == null) use address(0) as the token address.
func fetchV4PoolsFromAPI(ctx context.Context, apiKey string, limit int, db *internal.DB) {
	const wethArbitrum = "0x82af49447d8a07e3bd95bd0d56f35241523fbab1"
	const zeroAddr    = "0x0000000000000000000000000000000000000000"

	query := fmt.Sprintf(`{"query":"{ topV4Pools(first: %d, chain: ARBITRUM) { id token0 { address symbol decimals } token1 { address symbol decimals } feeTier } }"}`, limit)
	req, err := http.NewRequestWithContext(ctx, "POST", "https://interface.gateway.uniswap.org/v1/graphql",
		bytes.NewBufferString(query))
	if err != nil {
		log.Printf("[v4] api fetch: build request: %v", err)
		return
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-api-key", apiKey)
	req.Header.Set("Origin", "https://app.uniswap.org")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		log.Printf("[v4] api fetch: %v", err)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		log.Printf("[v4] api fetch: HTTP %d", resp.StatusCode)
		return
	}

	var result struct {
		Data struct {
			TopV4Pools []struct {
				ID     string `json:"id"`
				Token0 struct {
					Address *string `json:"address"`
					Symbol  string  `json:"symbol"`
				} `json:"token0"`
				Token1 struct {
					Address *string `json:"address"`
					Symbol  string  `json:"symbol"`
				} `json:"token1"`
			} `json:"topV4Pools"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		log.Printf("[v4] api fetch: decode: %v", err)
		return
	}
	count := 0
	for _, p := range result.Data.TopV4Pools {
		// Decode base64 id → "V4Pool:ARBITRUM_0x{poolId}"
		raw, err := base64.StdEncoding.DecodeString(p.ID)
		if err != nil { continue }
		parts := strings.SplitN(string(raw), "_", 2)
		if len(parts) != 2 { continue }
		poolIdHex := strings.ToLower(parts[1])
		if len(poolIdHex) != 66 { continue } // must be 0x + 64 hex chars

		tok0 := zeroAddr
		if p.Token0.Address != nil {
			tok0 = strings.ToLower(*p.Token0.Address)
		}
		tok1 := zeroAddr
		if p.Token1.Address != nil {
			tok1 = strings.ToLower(*p.Token1.Address)
		}

		// Preserve native ETH (0x0000...) as-is. Substituting WETH (0x82af49...)
		// here breaks V4's currency0 < currency1 invariant — the on-chain pool_id
		// was computed with ETH in slot 0, so cache rows with WETH in slot 0
		// disagree with chain reality and cause silent SIM_PHANTOM reverts.
		// TokenRegistry.Get aliases 0x0000 → WETH metadata so cycle routing
		// still works.
		_ = wethArbitrum

		poolId := common.HexToHash(poolIdHex)
		setV4Cache(poolId, tok0, tok1, db)
		count++
	}
	log.Printf("[v4] fetched %d pool(s) from Uniswap API", count)
}

// backfillV4PoolMetadata fetches the Initialize event for each pool in poolIDs
// across the full V4 history. Uses topic filtering on the indexed poolId so the
// RPC only returns matching events — fast even over millions of blocks.
// Block range is chunked to respect RPC provider limits (Chainstack: ~10k blocks).
func backfillV4PoolMetadata(ctx context.Context, c *ethclient.Client, poolIDs []common.Hash, db *internal.DB) int {
	if len(poolIDs) == 0 || db == nil {
		return 0
	}
	const poolChunkSize = 50
	const blockChunkSize uint64 = 10_000

	cur, err := c.BlockNumber(ctx)
	if err != nil {
		log.Printf("[v4-backfill] could not get current block: %v", err)
		return 0
	}

	pm := common.HexToAddress(v4PoolManager)
	updatedSet := make(map[common.Hash]bool)

	// Outer loop: chunks of poolIds (topic filter has a limit ~50 on most RPCs).
	// Inner loop: chunks of blocks (10k each, walking backwards from latest so the
	// most recent pools resolve first and we can stop early once all are found).
	for pStart := 0; pStart < len(poolIDs); pStart += poolChunkSize {
		pEnd := pStart + poolChunkSize
		if pEnd > len(poolIDs) {
			pEnd = len(poolIDs)
		}
		chunk := poolIDs[pStart:pEnd]
		// Track which pools in this chunk we still need to find
		need := make(map[common.Hash]bool, len(chunk))
		for _, p := range chunk {
			need[p] = true
		}

		// Walk backwards from current block to v4StartBlock
		for end := cur; end > v4StartBlock && len(need) > 0; {
			var start uint64
			if end > blockChunkSize {
				start = end - blockChunkSize + 1
			} else {
				start = 0
			}
			if start < v4StartBlock {
				start = v4StartBlock
			}

			tctx, cancel := context.WithTimeout(ctx, 30*time.Second)
			logs, err := c.FilterLogs(tctx, ethereum.FilterQuery{
				FromBlock: new(big.Int).SetUint64(start),
				ToBlock:   new(big.Int).SetUint64(end),
				Addresses: []common.Address{pm},
				Topics:    [][]common.Hash{{v4InitializeTopic}, chunk},
			})
			cancel()
			if err != nil {
				log.Printf("[v4-backfill] block range %d-%d error: %v", start, end, err)
			} else {
				for _, l := range logs {
					if len(l.Topics) < 4 || len(l.Data) < 160 {
						continue
					}
					poolHash := l.Topics[1]
					if !need[poolHash] {
						continue
					}
					tok0 := strings.ToLower(common.HexToAddress(l.Topics[2].Hex()).Hex())
					tok1 := strings.ToLower(common.HexToAddress(l.Topics[3].Hex()).Hex())
					feePPM := new(big.Int).SetBytes(l.Data[0:32]).Uint64()
					tickSpacingRaw := new(big.Int).SetBytes(l.Data[32:64])
					tickSpacing := tickSpacingRaw.Int64()
					if tickSpacing >= (1 << 23) {
						tickSpacing -= (1 << 24)
					}
					hooks := strings.ToLower(common.BytesToAddress(l.Data[76:96]).Hex())
					sqrtP := new(big.Int).SetBytes(l.Data[96:128])
					tickRaw := new(big.Int).SetBytes(l.Data[128:160])
					tickVal := tickRaw.Int64()
					if tickVal >= (1 << 23) {
						tickVal -= (1 << 24)
					}
					db.UpsertV4Pool(
						poolHash.Hex(), tok0, tok1, hooks,
						int(feePPM), int(tickSpacing), int(tickVal),
						sqrtP.String(), "0", 0, 0, 0,
					)
					// Synchronously verify the pool: check the hook permissions
					// (reject swap-affecting hooks) and fetch live state from
					// StateView. Persists verified=1/0 to v4_pools so the bot
					// can filter unverified pools out of cycles.
					ok, reason := internal.VerifyV4Pool(ctx, c, poolHash.Hex(), int32(tickSpacing), uint32(feePPM), hooks)
					_ = db.SetV4PoolVerified(poolHash.Hex(), ok, reason)
					updatedSet[poolHash] = true
					delete(need, poolHash)
				}
			}
			if start == 0 || start == v4StartBlock {
				break
			}
			end = start - 1
		}
	}
	return len(updatedSet)
}

// seedV4Cache backfills the v4Cache with Initialize events from the V4 PoolManager,
// starting from the V4 deployment block (v4StartBlock) up to current.
// batchSize controls how many blocks per eth_getLogs call — set based on your RPC's limit.
// maxBatches caps total RPC calls so startup stays fast; 0 = no cap (full history).
func seedV4Cache(ctx context.Context, c *ethclient.Client, fromBlock uint64, batchSize uint64, maxBatches int, db *internal.DB) {
	cur, err := c.BlockNumber(ctx)
	if err != nil {
		log.Printf("[v4] could not get current block for seed: %v", err)
		return
	}
	// Work backwards from the current block so the most-recent (most active) pools
	// are seeded first, and we stop early if maxBatches is hit.
	batches := 0
	pm := common.HexToAddress(v4PoolManager)
	count := 0
	for start := fromBlock; start <= cur; start += batchSize {
		if maxBatches > 0 && batches >= maxBatches { break }
		end := start + batchSize - 1
		if end > cur { end = cur }
		tctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		logs, err := c.FilterLogs(tctx, ethereum.FilterQuery{
			FromBlock: new(big.Int).SetUint64(start),
			ToBlock:   new(big.Int).SetUint64(end),
			Addresses: []common.Address{pm},
			Topics:    [][]common.Hash{{v4InitializeTopic}},
		})
		cancel()
		if err != nil {
			log.Printf("[v4] seed batch %d-%d error: %v — skipping", start, end, err)
			batches++
			continue
		}
		for _, l := range logs {
			if len(l.Topics) < 4 { continue }
			tok0 := strings.ToLower(common.HexToAddress(l.Topics[2].Hex()).Hex())
			tok1 := strings.ToLower(common.HexToAddress(l.Topics[3].Hex()).Hex())
			setV4Cache(l.Topics[1], tok0, tok1, db)
			count++

			// Decode fee, tickSpacing, hooks, sqrtPriceX96, tick from data field.
			// Initialize(bytes32 id, address indexed currency0, address indexed currency1,
			//            uint24 fee, int24 tickSpacing, IHooks hooks, uint160 sqrtPriceX96, int24 tick)
			// Data: fee(32) | tickSpacing(32) | hooks(32) | sqrtPriceX96(32) | tick(32) = 160 bytes
			if len(l.Data) >= 160 && db != nil {
				feePPM := new(big.Int).SetBytes(l.Data[0:32]).Uint64()
				tickSpacingRaw := new(big.Int).SetBytes(l.Data[32:64])
				tickSpacing := tickSpacingRaw.Int64()
				if tickSpacing >= (1 << 23) {
					tickSpacing -= (1 << 24)
				}
				hooks := strings.ToLower(common.BytesToAddress(l.Data[76:96]).Hex())
				sqrtP := new(big.Int).SetBytes(l.Data[96:128])
				tickRaw := new(big.Int).SetBytes(l.Data[128:160])
				tickVal := tickRaw.Int64()
				if tickVal >= (1 << 23) {
					tickVal -= (1 << 24)
				}
				db.UpsertV4Pool(
					l.Topics[1].Hex(), tok0, tok1, hooks,
					int(feePPM), int(tickSpacing), int(tickVal),
					sqrtP.String(), "0", 0, 0, 0,
				)
			}
		}
		batches++
	}
	log.Printf("[v4] seeded %d pool(s) (%d batches from block %d)", count, batches, fromBlock)
}

// ── Caches ────────────────────────────────────────────────────────────────────

var (
	tokCacheMu sync.Mutex
	tokCache   = make(map[string]tokenMeta)

	poolCacheMu sync.Mutex
	poolCache   = make(map[string]poolInfo)

	// V4 pool cache: poolId (bytes32) → [token0, token1] (lowercase hex).
	// Populated from Initialize events. Pools created before arbscan started
	// are seeded at startup via seedV4Cache(). Persisted to DB across restarts.
	v4CacheMu  sync.Mutex
	v4Cache    = make(map[common.Hash][2]string)

	// Curve coins cache: "poolAddr:idx" → token address.
	// Avoids repeated RPC calls for the same (pool, index) pair within a run.
	curveCoinsMu    sync.Mutex
	curveCoinsCache = make(map[string]string)
)

type poolInfo struct {
	Token0 string
	Token1 string
	DEX    string
	FeeBps uint32
	IsV3   bool
}

func getToken(ctx context.Context, c *ethclient.Client, addr string) tokenMeta {
	lo := strings.ToLower(addr)
	if m, ok := knownTokens[lo]; ok { return m }
	tokCacheMu.Lock()
	if m, ok := tokCache[lo]; ok { tokCacheMu.Unlock(); return m }
	tokCacheMu.Unlock()

	a := common.HexToAddress(addr)
	sym := lo[:8] + "..."
	var dec uint8 = 18

	tctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	if d, _ := erc20ABI.Pack("symbol"); d != nil {
		if res, err := c.CallContract(tctx, ethereum.CallMsg{To: &a, Data: d}, nil); err == nil {
			if vals, err := erc20ABI.Unpack("symbol", res); err == nil && len(vals) > 0 {
				if s, ok := vals[0].(string); ok && s != "" { sym = s }
			}
		}
	}
	if d, _ := erc20ABI.Pack("decimals"); d != nil {
		if res, err := c.CallContract(tctx, ethereum.CallMsg{To: &a, Data: d}, nil); err == nil {
			if vals, err := erc20ABI.Unpack("decimals", res); err == nil && len(vals) > 0 {
				if v, ok := vals[0].(uint8); ok { dec = v }
			}
		}
	}
	m := tokenMeta{sym, dec}
	tokCacheMu.Lock(); tokCache[lo] = m; tokCacheMu.Unlock()
	return m
}

func getPool(ctx context.Context, c *ethclient.Client, addr string, v3hint bool) poolInfo {
	lo := strings.ToLower(addr)
	poolCacheMu.Lock()
	if p, ok := poolCache[lo]; ok { poolCacheMu.Unlock(); return p }
	poolCacheMu.Unlock()

	a := common.HexToAddress(addr)
	p := poolInfo{DEX: "unknown", IsV3: v3hint}

	tctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	call := func(name string, out interface{}) bool {
		d, err := poolABIs.Pack(name)
		if err != nil { return false }
		res, err := c.CallContract(tctx, ethereum.CallMsg{To: &a, Data: d}, nil)
		if err != nil { return false }
		vals, err := poolABIs.Unpack(name, res)
		if err != nil || len(vals) == 0 { return false }
		switch v := vals[0].(type) {
		case common.Address:
			*(out.(*string)) = strings.ToLower(v.Hex())
		case uint32:
			*(out.(*uint32)) = v
		}
		return true
	}
	call("token0", &p.Token0)
	call("token1", &p.Token1)
	var feeRaw uint32
	if v3hint && call("fee", &feeRaw) {
		p.FeeBps = feeRaw / 100
	}
	var factLo string
	if call("factory", &factLo) {
		if label, ok := factoryLabel(factLo); ok {
			p.DEX = label
		} else if v3hint {
			p.DEX = fmt.Sprintf("V3(%s)", factLo[:10]+"...")
		} else {
			p.DEX = fmt.Sprintf("V2(%s)", factLo[:10]+"...")
		}
	}

	poolCacheMu.Lock(); poolCache[lo] = p; poolCacheMu.Unlock()
	return p
}

// ── Output structs ────────────────────────────────────────────────────────────

type SwapHop struct {
	Pool      string `json:"pool"`
	DEX       string `json:"dex"`
	FeeBps    uint32 `json:"fee_bps"`
	TokenIn   string `json:"token_in"`
	SymbolIn  string `json:"symbol_in"`
	AmountIn  string `json:"amount_in_human"`
	DecIn     uint8  `json:"decimals_in"`
	TokenOut  string `json:"token_out"`
	SymbolOut string `json:"symbol_out"`
	AmountOut string `json:"amount_out_human"`
	DecOut    uint8  `json:"decimals_out"`
}

// comparisonConfig mirrors the subset of config.yaml needed to compute
// OUR bot's dynamic LP floor + min-profit threshold for a hypothetical
// cycle. Loaded once from disk at arbscan startup; populates
// our_lpfloor_bps and our_min_profit_usd_at_block on every competitor_arbs
// row so the dashboard can render head-to-head "would we have taken this"
// without consulting the live bot.
type comparisonConfig struct {
	GasSafetyMult      float64
	BaseBpsCurve       float64
	BaseBpsV2          float64
	BaseBpsV3          float64
	BaseBpsCamelotV3   float64
	TVLRefUSD          float64
	TVLScaleMin        float64
	TVLScaleMax        float64
	VolStableMult      float64
	VolVolatileMult    float64
	LatencyOverheadBps float64
	GasElevatedGwei    float64
	GasHighGwei        float64
}

var cmpCfg comparisonConfig

// stablecoins (Arbitrum). Used by lpFloorForCompetitor to classify hop pairs
// as stable/stable (tight peg, low vol mult) vs containing a volatile leg.
var stableLower = map[string]bool{
	"0xff970a61a04b1ca14834a43f5de4533ebddb5cc8": true, // USDC.e
	"0xaf88d065e77c8cc2239327c5edb3a432268e5831": true, // USDC
	"0xfd086bc7cd5c481dcc9c85ebe478a1c0b69fcbb9": true, // USDT
	"0xda10009cbd5d07dd0cecc66161fc93d7c9000da1": true, // DAI
	"0x17fc002b466eec40dae837fc4be5c67993ddbd6f": true, // FRAX
}

// coreLower: tokens we treat as "major" for the lpfloor vol-mult check.
// Matches isCoreToken in the bot (WETH/WBTC/stables).
var coreLower = map[string]bool{
	"0x82af49447d8a07e3bd95bd0d56f35241523fbab1": true, // WETH
	"0x2f2a2543b76a4166549f7aab2e75bef0aefc5b0f": true, // WBTC
	"0xaf88d065e77c8cc2239327c5edb3a432268e5831": true,
	"0xfd086bc7cd5c481dcc9c85ebe478a1c0b69fcbb9": true,
	"0xff970a61a04b1ca14834a43f5de4533ebddb5cc8": true,
	"0xda10009cbd5d07dd0cecc66161fc93d7c9000da1": true,
	"0x17fc002b466eec40dae837fc4be5c67993ddbd6f": true,
}

// dexBaseBps maps a hop's DEX string to the configured per-hop base bps.
// Keeps the bot's dexTypeOnChain → BaseBps map local to arbscan so we don't
// have to construct a full *internal.Config for dynamicLPFloor.
func (cc *comparisonConfig) baseBpsFor(dex string) float64 {
	switch dex {
	case "Curve":
		return cc.BaseBpsCurve
	case "CamelotV3", "ZyberV3":
		return cc.BaseBpsCamelotV3
	case "UniV3", "SushiV3", "PancakeV3", "RamsesV3", "UniV4":
		return cc.BaseBpsV3
	default:
		return cc.BaseBpsV2
	}
}

// lpFloorForCompetitor returns our dynamic-LP-floor value IN BPS for a
// reconstructed competitor cycle, given the gas price at their block.
// Mirrors internal.dynamicLPFloorScaled with hopMult=1.0. Returns 0 on
// any missing input (avoids reporting a spurious "we'd have rejected"
// decision when the data is incomplete).
func lpFloorForCompetitor(hops []SwapHop, poolTVLs []float64, gasPriceGwei float64) float64 {
	if len(hops) == 0 || len(poolTVLs) != len(hops) {
		return 0
	}
	gasMult := 1.0
	switch {
	case gasPriceGwei >= cmpCfg.GasHighGwei && cmpCfg.GasHighGwei > 0:
		gasMult = 1.5
	case gasPriceGwei >= cmpCfg.GasElevatedGwei && cmpCfg.GasElevatedGwei > 0:
		gasMult = 1.2
	}
	total := 0.0
	for i, h := range hops {
		base := cmpCfg.baseBpsFor(h.DEX)
		tvl := poolTVLs[i]
		tvlScale := 1.0
		if tvl > 0 && cmpCfg.TVLRefUSD > 0 {
			tvlScale = math.Sqrt(cmpCfg.TVLRefUSD / tvl)
			if tvlScale < cmpCfg.TVLScaleMin {
				tvlScale = cmpCfg.TVLScaleMin
			}
			if tvlScale > cmpCfg.TVLScaleMax {
				tvlScale = cmpCfg.TVLScaleMax
			}
		}
		a := strings.ToLower(h.TokenIn)
		b := strings.ToLower(h.TokenOut)
		volMult := 1.0
		if stableLower[a] && stableLower[b] {
			volMult = cmpCfg.VolStableMult
		} else if !coreLower[a] || !coreLower[b] {
			volMult = cmpCfg.VolVolatileMult
		}
		total += base * tvlScale * volMult * gasMult
	}
	total += cmpCfg.LatencyOverheadBps
	return total
}

// fetchCompetitorPoolTVLs looks up TVL for every pool in the competitor's
// hop list via the shared arb.db pools table. Returns a slice aligned with
// hops. Missing pool → 0 (lpFloorForCompetitor's tvl_scale then stays at 1.0).
func fetchCompetitorPoolTVLs(db *internal.DB, hops []SwapHop) []float64 {
	out := make([]float64, len(hops))
	for i, h := range hops {
		out[i] = db.PoolTVL(strings.ToLower(h.Pool))
	}
	return out
}

type ArbTx struct {
	TxHash      string    `json:"tx_hash"`
	BlockNumber uint64    `json:"block"`
	Timestamp   time.Time `json:"timestamp"`
	BotContract string    `json:"bot_contract"` // tx.To (the contract called)
	Sender      string    `json:"sender"`       // tx.From

	FlashLoanSource string `json:"flash_loan_source"` // "Aave", "Balancer", "UniV3", "own_capital"
	BorrowToken     string `json:"borrow_token,omitempty"`
	BorrowSymbol    string `json:"borrow_symbol,omitempty"`
	BorrowAmount    string `json:"borrow_amount_human,omitempty"`

	IsCircular    bool    `json:"is_circular"` // start token == end token
	StartToken    string  `json:"start_token"`
	StartSymbol   string  `json:"start_symbol"`

	ProfitToken   string  `json:"profit_token"`
	ProfitSymbol  string  `json:"profit_symbol"`
	ProfitHuman   string  `json:"profit_human"`
	ProfitNative   float64 `json:"profit_native"` // profit in the profit token's own units
	ProfitUSD      float64 `json:"profit_usd"`
	TokenPriceUSD  float64 `json:"token_price_usd"`  // USD price per unit of profit token
	HopPricesJSON  string  `json:"hop_prices_json"`  // JSON map: token_address → usd_price for all path tokens

	GasUsed uint64  `json:"gas_used"`
	GasGwei string  `json:"gas_price_gwei"`
	CostETH float64 `json:"cost_eth"`
	CostUSD float64 `json:"cost_usd"`
	NetUSD  float64 `json:"net_profit_usd"`

	// Comparison metadata (mirrors our_trades + arb_observations so SQL
	// joins on tx_hash / block_number produce head-to-head rows without
	// per-query recomputation).
	TxIndex       uint   `json:"tx_index"`        // receipt.TransactionIndex (position in block)
	BlockTotalTxs int    `json:"block_total_txs"` // len(block.Transactions())
	NotionalUSD   float64 `json:"notional_usd"`   // borrow_amount × token_price_usd
	ProfitBps     float64 `json:"profit_bps"`     // ProfitUSD / NotionalUSD × 10000

	HopCount int       `json:"hop_count"`
	Hops     []SwapHop `json:"hops"`
	PathStr  string    `json:"path_str"`
}

// ── Main ──────────────────────────────────────────────────────────────────────

// stableAddrs is the set of known stablecoin addresses on Arbitrum (lower-case).
// These seed the hop-based price propagation with a known USD value of $1.
var stableAddrs = map[string]bool{
	"0xff970a61a04b1ca14834a43f5de4533ebddb5cc8": true, // USDC.e
	"0xaf88d065e77c8cc2239327c5edb3a432268e5831": true, // USDC
	"0xfd086bc7cd5c481dcc9c85ebe478a1c0b69fcbb9": true, // USDT
	"0xda10009cbd5d07dd0cecc66161fc93d7c9000da1": true, // DAI
	"0x17fc002b466eec40dae837fc4be5c67993ddbd6f": true, // FRAX
}

// buildHopPrices propagates known stablecoin prices ($1) through hop exchange rates
// and returns a map of token_address (lower-case) → USD price for every reachable token.
func buildHopPrices(hops []SwapHop) map[string]float64 {
	prices := make(map[string]float64)
	for addr := range stableAddrs {
		prices[addr] = 1.0
	}
	for changed := true; changed; {
		changed = false
		for _, h := range hops {
			inAddr := strings.ToLower(h.TokenIn)
			outAddr := strings.ToLower(h.TokenOut)
			amtIn, errIn := strconv.ParseFloat(h.AmountIn, 64)
			amtOut, errOut := strconv.ParseFloat(h.AmountOut, 64)
			if errIn != nil || errOut != nil || amtIn <= 0 || amtOut <= 0 {
				continue
			}
			if pIn, ok := prices[inAddr]; ok {
				if _, known := prices[outAddr]; !known {
					prices[outAddr] = pIn * amtIn / amtOut
					changed = true
				}
			} else if pOut, ok := prices[outAddr]; ok {
				if _, known := prices[inAddr]; !known {
					prices[inAddr] = pOut * amtOut / amtIn
					changed = true
				}
			}
		}
	}
	return prices
}

// hopPriceUSD returns (profitUSD, tokenPriceUSD, hopPricesJSON) for a given profit token.
func hopPriceUSD(hops []SwapHop, profitToken string, profitAmount float64) (float64, float64, string) {
	prices := buildHopPrices(hops)
	tokenPrice := prices[strings.ToLower(profitToken)]
	b, err := json.Marshal(prices)
	hopJSON := "{}"
	if err == nil {
		hopJSON = string(b)
	}
	return profitAmount * tokenPrice, tokenPrice, hopJSON
}

func main() {
	initABIs()

	rpcURL := "" // read from config.yaml below
	if len(os.Args) > 1 { rpcURL = os.Args[1] }

	dbPath := "/home/arbitrator/go/arb-bot/arb.db"
	if len(os.Args) > 2 { dbPath = os.Args[2] }

	cfgPath := "/home/arbitrator/go/arb-bot/config.yaml"
	if len(os.Args) > 4 { cfgPath = os.Args[4] }
	var uniswapAPIKey string
	if cfgData, err := os.ReadFile(cfgPath); err == nil {
		// Minimal parse: extract top-level keys without pulling in yaml dep.
		parseKV := func(trimmed string) (string, bool) {
			parts := strings.SplitN(trimmed, ":", 2)
			if len(parts) != 2 {
				return "", false
			}
			return strings.TrimSpace(strings.Trim(strings.TrimSpace(parts[1]), `"`)), true
		}
		parseFloat := func(s string) float64 {
			f, _ := strconv.ParseFloat(s, 64)
			return f
		}
		for _, line := range strings.Split(string(cfgData), "\n") {
			trimmed := strings.TrimSpace(line)
			if strings.HasPrefix(trimmed, "uniswap_api_key:") {
				if v, ok := parseKV(trimmed); ok { uniswapAPIKey = v }
			}
			if strings.HasPrefix(trimmed, "arbscan_rpc:") && rpcURL == "" {
				if v, ok := parseKV(trimmed); ok { rpcURL = v }
			}
			if strings.HasPrefix(trimmed, "arbitrum_rpc:") && rpcURL == "" {
				if v, ok := parseKV(trimmed); ok { rpcURL = v }
			}
			// Comparison-floor parameters (Phase 2 of the competitor head-to-head
			// columns). These feed the pre-computed our_min_profit_usd_at_block
			// and our_lpfloor_bps fields on every competitor_arbs row. Any
			// field left unpopulated falls back to our observed defaults so a
			// missing config entry never crashes arbscan.
			if strings.HasPrefix(trimmed, "gas_safety_mult:") {
				if v, ok := parseKV(trimmed); ok { cmpCfg.GasSafetyMult = parseFloat(v) }
			}
			if strings.HasPrefix(trimmed, "base_bps_curve:") {
				if v, ok := parseKV(trimmed); ok { cmpCfg.BaseBpsCurve = parseFloat(v) }
			}
			if strings.HasPrefix(trimmed, "base_bps_v2:") {
				if v, ok := parseKV(trimmed); ok { cmpCfg.BaseBpsV2 = parseFloat(v) }
			}
			if strings.HasPrefix(trimmed, "base_bps_v3:") {
				if v, ok := parseKV(trimmed); ok { cmpCfg.BaseBpsV3 = parseFloat(v) }
			}
			if strings.HasPrefix(trimmed, "base_bps_camelot_v3:") {
				if v, ok := parseKV(trimmed); ok { cmpCfg.BaseBpsCamelotV3 = parseFloat(v) }
			}
			if strings.HasPrefix(trimmed, "tvl_ref_usd:") {
				if v, ok := parseKV(trimmed); ok { cmpCfg.TVLRefUSD = parseFloat(v) }
			}
			if strings.HasPrefix(trimmed, "tvl_scale_min:") {
				if v, ok := parseKV(trimmed); ok { cmpCfg.TVLScaleMin = parseFloat(v) }
			}
			if strings.HasPrefix(trimmed, "tvl_scale_max:") {
				if v, ok := parseKV(trimmed); ok { cmpCfg.TVLScaleMax = parseFloat(v) }
			}
			if strings.HasPrefix(trimmed, "vol_stable_mult:") {
				if v, ok := parseKV(trimmed); ok { cmpCfg.VolStableMult = parseFloat(v) }
			}
			if strings.HasPrefix(trimmed, "vol_volatile_mult:") {
				if v, ok := parseKV(trimmed); ok { cmpCfg.VolVolatileMult = parseFloat(v) }
			}
			if strings.HasPrefix(trimmed, "latency_overhead_bps:") {
				if v, ok := parseKV(trimmed); ok { cmpCfg.LatencyOverheadBps = parseFloat(v) }
			}
			if strings.HasPrefix(trimmed, "gas_elevated_gwei:") {
				if v, ok := parseKV(trimmed); ok { cmpCfg.GasElevatedGwei = parseFloat(v) }
			}
			if strings.HasPrefix(trimmed, "gas_high_gwei:") {
				if v, ok := parseKV(trimmed); ok { cmpCfg.GasHighGwei = parseFloat(v) }
			}
		}
	}
	// Fallback defaults mirroring applyStrategyDefaults.
	if cmpCfg.GasSafetyMult <= 0 { cmpCfg.GasSafetyMult = 1.0 }
	if cmpCfg.BaseBpsV3 <= 0 { cmpCfg.BaseBpsV3 = 6.0 }
	if cmpCfg.BaseBpsV2 <= 0 { cmpCfg.BaseBpsV2 = 6.0 }
	if cmpCfg.BaseBpsCurve <= 0 { cmpCfg.BaseBpsCurve = 2.0 }
	if cmpCfg.BaseBpsCamelotV3 <= 0 { cmpCfg.BaseBpsCamelotV3 = 14.0 }
	if cmpCfg.TVLRefUSD <= 0 { cmpCfg.TVLRefUSD = 500_000 }
	if cmpCfg.TVLScaleMin <= 0 { cmpCfg.TVLScaleMin = 0.3 }
	if cmpCfg.TVLScaleMax <= 0 { cmpCfg.TVLScaleMax = 4.0 }
	if cmpCfg.VolStableMult <= 0 { cmpCfg.VolStableMult = 0.3 }
	if cmpCfg.VolVolatileMult <= 0 { cmpCfg.VolVolatileMult = 1.5 }
	if cmpCfg.LatencyOverheadBps <= 0 { cmpCfg.LatencyOverheadBps = 2.0 }
	if cmpCfg.GasElevatedGwei <= 0 { cmpCfg.GasElevatedGwei = 0.05 }
	if cmpCfg.GasHighGwei <= 0 { cmpCfg.GasHighGwei = 0.20 }
	if rpcURL == "" {
		log.Fatal("no RPC URL: set arbitrum_rpc in config.yaml or pass as arg 1")
	}

	db, err := internal.OpenDB(dbPath)
	if err != nil { log.Fatalf("open db %s: %v", dbPath, err) }
	defer db.Close()

	client, err := ethclient.Dial(rpcURL)
	if err != nil { log.Fatalf("dial: %v", err) }
	defer client.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)

	log.Printf("[arbscan] capturing until killed → %s", dbPath)

	// Load junk token addresses from DB — arbs involving these are skipped.
	junkAddrs := make(map[string]bool)
	if addrs, err := db.JunkTokenAddresses(); err != nil {
		log.Printf("[arbscan] WARNING: could not load junk tokens: %v", err)
	} else {
		for _, a := range addrs {
			junkAddrs[strings.ToLower(a)] = true
		}
		log.Printf("[arbscan] loaded %d junk token(s) to exclude", len(addrs))
	}

	// Load persisted V4 pool tokens from DB (instant — avoids redundant API/RPC work).
	if rows, err := db.LoadV4PoolTokens(); err == nil {
		v4CacheMu.Lock()
		for _, r := range rows {
			v4Cache[common.HexToHash(r[0])] = [2]string{r[1], r[2]}
		}
		v4CacheMu.Unlock()
		log.Printf("[v4] loaded %d pool(s) from DB cache", len(rows))
	}

	// Seed V4 pool cache: API first (fast, covers all pools by TVL).
	// RPC backfill is skipped when an API key is present — the API covers the
	// important pools and live Initialize events handle new ones. Without an API
	// key we do a short RPC backfill (last ~1M blocks = ~10 batches) as a fallback.
	if uniswapAPIKey != "" {
		log.Printf("[v4] fetching top pools from Uniswap API...")
		fetchV4PoolsFromAPI(ctx, uniswapAPIKey, 100, db)
	}
	// Always backfill from Initialize logs too: the API gives token mappings only,
	// but we also need fee_ppm/tick_spacing/hooks from the event data so the bot
	// can properly fetch tick maps for V4 pools with non-standard fee tiers.
	{
		log.Printf("[v4] backfilling fee/tickSpacing/hooks via RPC (last ~1M blocks)...")
		cur, _ := client.BlockNumber(ctx)
		startBlock := v4StartBlock
		if cur > 1_000_000 && cur-1_000_000 > startBlock {
			startBlock = cur - 1_000_000
		}
		seedV4Cache(ctx, client, startBlock, 10_000, 100, db)
	}
	// Targeted backfill: any V4 pool still missing tickSpacing gets a topic-filtered
	// log query across the FULL V4 history (much faster than scanning all blocks).
	if db != nil {
		missing, err := db.V4PoolIDsMissingTickSpacing()
		if err == nil && len(missing) > 0 {
			log.Printf("[v4] %d pools still missing tickSpacing — running targeted backfill...", len(missing))
			poolHashes := make([]common.Hash, 0, len(missing))
			for _, id := range missing {
				poolHashes = append(poolHashes, common.HexToHash(id))
			}
			updated := backfillV4PoolMetadata(ctx, client, poolHashes, db)
			log.Printf("[v4] targeted backfill: %d/%d pools updated", updated, len(missing))
		}
	}

	var mu sync.Mutex
	var captured []ArbTx
	txCount := 0
	blockCount := 0

	// Process blocks concurrently, 4 at a time max
	sem := make(chan struct{}, 4)

	// reconnect establishes a fresh WebSocket client + block subscription.
	// Retries indefinitely with backoff until ctx is cancelled (SIGINT/SIGTERM).
	// Also starts an HTTP polling goroutine as a fallback — some RPC endpoints
	// (Chainstack free tier observed) accept WSS Subscribe calls without error
	// but never deliver headers. The poller pushes fresh headers into the same
	// channel whenever block_number advances.
	reconnect := func() (*ethclient.Client, ethereum.Subscription, chan *types.Header) {
		headers := make(chan *types.Header, 64)
		for {
			select {
			case <-ctx.Done():
				return nil, nil, nil
			default:
			}
			c, err := ethclient.Dial(rpcURL)
			if err != nil {
				log.Printf("[arbscan] dial error: %v — retrying in 5s", err)
				time.Sleep(5 * time.Second)
				continue
			}
			s, err := c.SubscribeNewHead(ctx, headers)
			if err != nil {
				c.Close()
				log.Printf("[arbscan] subscribe error: %v — retrying in 5s", err)
				time.Sleep(5 * time.Second)
				continue
			}
			// Start HTTP polling fallback. Pushes headers on block-number advance.
			// Safe to run alongside WSS: if WSS delivers a header first, the poller
			// just sees the same block and skips (deduped by lastBlock tracking).
			go func(client *ethclient.Client, hch chan *types.Header) {
				t := time.NewTicker(250 * time.Millisecond)
				defer t.Stop()
				var lastBlock uint64
				for {
					select {
					case <-ctx.Done():
						return
					case <-t.C:
						pctx, cancel := context.WithTimeout(ctx, 2*time.Second)
						h, err := client.HeaderByNumber(pctx, nil)
						cancel()
						if err != nil || h == nil {
							continue
						}
						bn := h.Number.Uint64()
						if bn <= lastBlock {
							continue
						}
						lastBlock = bn
						// Non-blocking send: if channel is full (WSS also delivering),
						// drop the poll — block will be caught on the next tick.
						select {
						case hch <- h:
						default:
						}
					}
				}
			}(c, headers)
			return c, s, headers
		}
	}

	// Background comparison loop: classifies new competitor_arbs rows by
	// matching them against arb_observations + our_trades to figure out whether
	// we (the bot) detected/executed the same opportunity. Updates the
	// comparison_result column on each row, which the dashboard renders as a
	// clickable badge in the Competitors table.
	if db != nil {
		go internal.CompetitorCompareLoop(ctx, db, 30*time.Second)
		log.Printf("[arbscan] competitor comparison loop started")
	}

	log.Printf("[arbscan] listening for new blocks...")
	client, sub, headers := reconnect()
	if client == nil {
		return // context cancelled during initial connect
	}
	defer sub.Unsubscribe()
	defer client.Close()

	// Watchdog: if no header arrives within this window, assume the subscription
	// is silently broken (seen on Chainstack free tier — Subscribe succeeds but
	// no headers ever flow). Force a reconnect.
	const headerSilenceTimeout = 15 * time.Second
	watchdog := time.NewTicker(5 * time.Second)
	defer watchdog.Stop()
	lastHeaderAt := time.Now()

	running := true
	for running {
		select {
		case <-sig:
			running = false
		case <-watchdog.C:
			if time.Since(lastHeaderAt) > headerSilenceTimeout {
				log.Printf("[arbscan] watchdog: no headers for %v — forcing reconnect", time.Since(lastHeaderAt).Round(time.Second))
				sub.Unsubscribe()
				client.Close()
				newClient, newSub, newHeaders := reconnect()
				if newClient == nil {
					running = false
					break
				}
				client, sub, headers = newClient, newSub, newHeaders
				lastHeaderAt = time.Now()
			}
		case err := <-sub.Err():
			log.Printf("[arbscan] sub error: %v — reconnecting...", err)
			sub.Unsubscribe()
			client.Close()
			newClient, newSub, newHeaders := reconnect()
			if newClient == nil {
				running = false
				break
			}
			client, sub, headers = newClient, newSub, newHeaders
			lastHeaderAt = time.Now()
		case header := <-headers:
			lastHeaderAt = time.Now()
			h := header
			sem <- struct{}{}
			go func() {
				defer func() { <-sem }()
				arbs := processBlock(ctx, client, h, db)
				if len(arbs) == 0 { return }
				mu.Lock()
				for _, a := range arbs {
					// Skip arbs involving junk/honeypot tokens.
					isJunk := false
					for _, h := range a.Hops {
						if junkAddrs[strings.ToLower(h.TokenIn)] || junkAddrs[strings.ToLower(h.TokenOut)] {
							isJunk = true
							break
						}
					}
					if isJunk {
						continue
					}
					// Skip zero/near-zero profit arbs — these are noise (honeypot
					// tokens, wash trading, failed attempts). Only keep arbs that
					// made at least $0.01 net profit.
					if a.ProfitUSD < 0.01 {
						continue
					}
					captured = append(captured, a)
					txCount++
					gasPriceGwei := 0.0
					if a.GasGwei != "" {
						if v, err := strconv.ParseFloat(a.GasGwei, 64); err == nil {
							gasPriceGwei = v
						}
					}
					// Phase 2: compute our bot's floors for this hypothetical
					// cycle at the competitor's block gas price. Answers the
					// "would we have taken this trade?" question directly in SQL.
					ourMinProfitUSD := cmpCfg.GasSafetyMult * a.CostUSD
					ourLPFloorBps := lpFloorForCompetitor(
						a.Hops, fetchCompetitorPoolTVLs(db, a.Hops), gasPriceGwei,
					)
					rec := &internal.CompetitorArb{
						SeenAt:       a.Timestamp.Unix(),
						BlockNumber:  a.BlockNumber,
						TxHash:       a.TxHash,
						Sender:       a.Sender,
						BotContract:  a.BotContract,
						FlashLoanSrc: a.FlashLoanSource,
						BorrowToken:  a.BorrowToken,
						BorrowSymbol: a.BorrowSymbol,
						BorrowAmount: a.BorrowAmount,
						PathStr:      a.PathStr,
						HopCount:     a.HopCount,
						Dexes:        hopDexes(a.Hops),
						ProfitUSD:     a.ProfitUSD,
						NetUSD:        a.NetUSD,
						GasUsed:       a.GasUsed,
						HopsJSON:      a.Hops,
						TokenPriceUSD: a.TokenPriceUSD,
						HopPricesJSON: a.HopPricesJSON,
						// Comparison metadata (Phase 1 — derived from existing
						// per-tx data without additional RPC calls).
						NotionalUSD:   a.NotionalUSD,
						ProfitBps:     a.ProfitBps,
						GasPriceGwei:  gasPriceGwei,
						GasCostUSD:    a.CostUSD,
						BlockPosition: a.TxIndex,
						BlockTotalTxs: a.BlockTotalTxs,
						// Phase 2: OurMinProfitUSDAtBlock + OurLPFloorBps
						// let the dashboard show "would we have taken this"
						// instantly. PoolStatesJSON still left empty — that
						// one requires a historical multicall pinned to
						// a.BlockNumber, which costs an extra RPC roundtrip
						// per observed arb. Follow-up.
						OurMinProfitUSDAtBlock: ourMinProfitUSD,
						OurLPFloorBps:          ourLPFloorBps,
					}
					if err := db.InsertCompetitorArb(rec); err != nil {
						log.Printf("[arbscan] db insert: %v", err)
					}
					botStr := a.BotContract
					if len(botStr) > 10 { botStr = botStr[:10]+"..." }
					log.Printf("[arbscan] #%d block=%d tx=%s bot=%s hops=%d path=%s profit=$%.4f net=$%.4f flash=%s",
						txCount, a.BlockNumber, a.TxHash[:12]+"...",
						botStr, a.HopCount, a.PathStr,
						a.ProfitUSD, a.NetUSD, a.FlashLoanSource)
				}
				mu.Unlock()
				blockCount++
				if blockCount%30 == 0 {
					log.Printf("[arbscan] processed %d blocks, captured %d arbs so far", blockCount, txCount)
				}
			}()
		}
	}
	for i := 0; i < cap(sem); i++ { sem <- struct{}{} }

	mu.Lock()
	final := make([]ArbTx, len(captured))
	copy(final, captured)
	mu.Unlock()

	log.Printf("[arbscan] done — %d blocks, %d arbs captured", blockCount, len(final))
	writeSummary(final)
}

// processBlock fetches all logs for a block, groups by tx, finds multi-swap txs.
func processBlock(ctx context.Context, client *ethclient.Client, header *types.Header, db *internal.DB) []ArbTx {
	bn := header.Number
	tctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	swapTopics := []common.Hash{swapV2Topic, swapV3Topic, swapAlgTopic, curveExchangeTopic, curveExchangeUnderlyingTopic, balancerSwapTopic, dodoSwapTopic, v4SwapTopic}
	allTopics := append(swapTopics, transferTopic, aaveFlashTopic, balFlashTopic, v4InitializeTopic)

	logs, err := client.FilterLogs(tctx, ethereum.FilterQuery{
		FromBlock: bn,
		ToBlock:   bn,
		Topics:    [][]common.Hash{allTopics},
	})
	if err != nil {
		return nil
	}

	// Pre-pass: cache any V4 pool Initialize events seen in this block so that
	// Swap events later in the same block (or future blocks) can be decoded.
	// Initialize(bytes32 id, Currency indexed currency0, Currency indexed currency1,
	//            uint24 fee, int24 tickSpacing, IHooks hooks, uint160 sqrtPriceX96, int24 tick)
	// Topics: [sig, id, currency0, currency1]
	// Data:   fee(uint24, padded 32) | tickSpacing(int24, padded 32) | hooks(address, padded 32) |
	//         sqrtPriceX96(uint160, padded 32) | tick(int24, padded 32)
	for _, l := range logs {
		if len(l.Topics) < 4 || l.Topics[0] != v4InitializeTopic { continue }
		if strings.ToLower(l.Address.Hex()) != v4PoolManager { continue }
		tok0 := strings.ToLower(common.HexToAddress(l.Topics[2].Hex()).Hex())
		tok1 := strings.ToLower(common.HexToAddress(l.Topics[3].Hex()).Hex())
		setV4Cache(l.Topics[1], tok0, tok1, db)

		// Decode fee, tickSpacing, hooks, sqrtPriceX96, tick from data field
		if len(l.Data) >= 160 && db != nil {
			feePPM := new(big.Int).SetBytes(l.Data[0:32]).Uint64()
			tickSpacingRaw := new(big.Int).SetBytes(l.Data[32:64])
			// int24 sign-extension: if top byte indicates negative, convert
			tickSpacing := tickSpacingRaw.Int64()
			if tickSpacing >= (1 << 23) {
				tickSpacing -= (1 << 24)
			}
			hooks := strings.ToLower(common.BytesToAddress(l.Data[76:96]).Hex())
			sqrtP := new(big.Int).SetBytes(l.Data[96:128])
			tickRaw := new(big.Int).SetBytes(l.Data[128:160])
			tickVal := tickRaw.Int64()
			if tickVal >= (1 << 23) {
				tickVal -= (1 << 24)
			}
			db.UpsertV4Pool(
				l.Topics[1].Hex(), tok0, tok1, hooks,
				int(feePPM), int(tickSpacing), int(tickVal),
				sqrtP.String(), "0", 0, 0, 0,
			)
			// Verify the new pool: hook permissions + live state. Persists
			// the verified flag so the bot can include/exclude appropriately.
			ok, reason := internal.VerifyV4Pool(ctx, client, l.Topics[1].Hex(), int32(tickSpacing), uint32(feePPM), hooks)
			_ = db.SetV4PoolVerified(l.Topics[1].Hex(), ok, reason)
		}
	}

	// Group logs by tx hash
	byTx := make(map[common.Hash][]types.Log)
	txOrder := []common.Hash{}
	seen := make(map[common.Hash]bool)
	for _, l := range logs {
		if !seen[l.TxHash] {
			seen[l.TxHash] = true
			txOrder = append(txOrder, l.TxHash)
		}
		byTx[l.TxHash] = append(byTx[l.TxHash], l)
	}

	var arbs []ArbTx
	for _, txHash := range txOrder {
		txLogs := byTx[txHash]
		swapCount := 0
		for _, l := range txLogs {
			t := l.Topics[0]
			if t == swapV2Topic || t == swapV3Topic || t == swapAlgTopic ||
				t == curveExchangeTopic || t == curveExchangeUnderlyingTopic ||
				t == balancerSwapTopic || t == dodoSwapTopic || t == v4SwapTopic {
				swapCount++
			}
		}
		if swapCount < 2 || swapCount > 5 {
			continue // not an arb (single swap) or too many hops (>5 = noise)
		}

		arb, ok := analyzeTx(ctx, client, header, txHash, txLogs)
		if !ok { continue }
		arbs = append(arbs, arb)
	}
	return arbs
}

func analyzeTx(ctx context.Context, client *ethclient.Client, header *types.Header, txHash common.Hash, logs []types.Log) (ArbTx, bool) {
	tctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()

	tx, _, err := client.TransactionByHash(tctx, txHash)
	if err != nil || tx == nil { return ArbTx{}, false }
	receipt, err := client.TransactionReceipt(tctx, txHash)
	if err != nil || receipt == nil { return ArbTx{}, false }
	if receipt.Status == 0 { return ArbTx{}, false } // failed tx — skip

	sender, _ := types.NewLondonSigner(tx.ChainId()).Sender(tx)

	botContract := ""
	if tx.To() != nil {
		botContract = strings.ToLower(tx.To().Hex())
	}

	ts := time.Unix(int64(header.Time), 0)

	// Decode swap hops
	hops := decodeHops(ctx, client, logs)
	if len(hops) < 2 { return ArbTx{}, false } // require ≥2 decoded hops — single swaps are not arbs

	// Detect flash loan source
	flashSource := "own_capital"
	var borrowToken, borrowSymbol, borrowAmount string
	for _, l := range logs {
		t := l.Topics[0]
		la := strings.ToLower(l.Address.Hex())
		if t == balFlashTopic && la == balancerVault {
			flashSource = "Balancer"
			if len(l.Topics) >= 3 {
				borrowToken = strings.ToLower(common.HexToAddress(l.Topics[2].Hex()).Hex())
				m := getToken(ctx, client, borrowToken)
				borrowSymbol = m.Symbol
				if len(l.Data) >= 32 {
					amt := new(big.Int).SetBytes(l.Data[:32])
					borrowAmount = toHuman(amt, m.Decimals)
				}
			}
		} else if t == aaveFlashTopic && la == aavePool {
			flashSource = "Aave"
			if len(l.Topics) >= 4 {
				borrowToken = strings.ToLower(common.HexToAddress(l.Topics[3].Hex()).Hex())
				m := getToken(ctx, client, borrowToken)
				borrowSymbol = m.Symbol
				if len(l.Data) >= 32 {
					amt := new(big.Int).SetBytes(l.Data[:32])
					borrowAmount = toHuman(amt, m.Decimals)
				}
			}
		} else if (t == swapV3Topic || t == swapAlgTopic) && flashSource == "own_capital" {
			// Check if it's a UniV3 flash (loan within swap callback).
			// Hard to detect without traces; mark as V3-flash if > 3 hops.
			if len(hops) >= 3 {
				flashSource = "V3-flash-or-capital"
			}
		}
	}

	// Build path string
	var pathParts []string
	for _, h := range hops {
		pathParts = append(pathParts, fmt.Sprintf("%s→[%s]", h.SymbolIn, h.DEX))
	}
	if len(hops) > 0 {
		pathParts = append(pathParts, hops[len(hops)-1].SymbolOut)
	}
	pathStr := strings.Join(pathParts, "")

	// Check circularity: is start token == end token?
	isCircular := false
	startToken, startSymbol := "", ""
	if len(hops) > 0 {
		startToken = hops[0].TokenIn
		startSymbol = hops[0].SymbolIn
		endToken := hops[len(hops)-1].TokenOut
		isCircular = strings.EqualFold(startToken, endToken)
	}

	// Reject non-circular trades — DEX aggregator swaps (A→B→C) are not arbs.
	if !isCircular {
		return ArbTx{}, false
	}

	// Estimate profit from net token transfers to/from the bot contract.
	// For each token, sum(transfers in) - sum(transfers out) = net balance change.
	netFlow := make(map[string]*big.Int)
	botLo := botContract
	for _, l := range logs {
		if l.Topics[0] != transferTopic || len(l.Topics) < 3 { continue }
		tok := strings.ToLower(l.Address.Hex())
		from := strings.ToLower(common.HexToAddress(l.Topics[1].Hex()).Hex())
		to := strings.ToLower(common.HexToAddress(l.Topics[2].Hex()).Hex())
		if len(l.Data) < 32 { continue }
		val := new(big.Int).SetBytes(l.Data[:32])

		if to == botLo {
			if netFlow[tok] == nil { netFlow[tok] = new(big.Int) }
			netFlow[tok].Add(netFlow[tok], val)
		}
		if from == botLo {
			if netFlow[tok] == nil { netFlow[tok] = new(big.Int) }
			netFlow[tok].Sub(netFlow[tok], val)
		}
	}

	// Compute true profit = sum of all net token flows priced in USD.
	// Picking only the max-positive token is wrong for multi-token arbs where
	// the bot spends one token (e.g. WETH) to receive another (e.g. USDC) —
	// the cost must be subtracted to get the real net.
	hopPrices := buildHopPrices(hops)
	b, _ := json.Marshal(hopPrices)
	hopPricesJSON := string(b)
	if hopPricesJSON == "" { hopPricesJSON = "{}" }

	// Check for discontinuous path (gap between consecutive hops).
	// V4 PoolManager unlock() patterns settle intermediate tokens internally
	// without ERC20 Transfer events, creating asymmetric netFlow: e.g., USDC
	// appears as +$433 inflow but the offsetting WETH outflow is invisible
	// (settled inside PoolManager). For these arbs, only count tokens that
	// appeared in BOTH Transfer-in AND Transfer-out directions — those are
	// round-trip tokens whose net is reliable. One-directional tokens are
	// settlement artifacts.
	isDiscontinuous := false
	for i := 0; i+1 < len(hops); i++ {
		if !strings.EqualFold(hops[i].TokenOut, hops[i+1].TokenIn) {
			isDiscontinuous = true
			break
		}
	}
	// Track which tokens had inflows vs outflows.
	tokHasIn := make(map[string]bool)
	tokHasOut := make(map[string]bool)
	for _, l := range logs {
		if l.Topics[0] != transferTopic || len(l.Topics) < 3 { continue }
		tok := strings.ToLower(l.Address.Hex())
		from := strings.ToLower(common.HexToAddress(l.Topics[1].Hex()).Hex())
		to := strings.ToLower(common.HexToAddress(l.Topics[2].Hex()).Hex())
		if to == botContract { tokHasIn[tok] = true }
		if from == botContract { tokHasOut[tok] = true }
	}

	var profitUSD float64
	for tok, amt := range netFlow {
		price, ok := hopPrices[strings.ToLower(tok)]
		if !ok || price <= 0 { continue }
		// For discontinuous arbs: skip one-directional tokens (settlement artifacts).
		if isDiscontinuous {
			tokLo := strings.ToLower(tok)
			if !tokHasIn[tokLo] || !tokHasOut[tokLo] {
				continue
			}
		}
		meta := getToken(ctx, client, tok)
		f, _ := new(big.Float).SetInt(amt).Float64()
		humanAmt := f / pow10f(meta.Decimals)
		profitUSD += humanAmt * price
	}

	// For display/DB: find the token with the largest positive net flow as the
	// "profit token" label, but use the USD-summed value above as the real number.
	var bestTok string
	var bestAmt *big.Int
	for tok, amt := range netFlow {
		if amt.Sign() <= 0 { continue }
		if bestAmt == nil || amt.Cmp(bestAmt) > 0 {
			bestTok = tok
			bestAmt = amt
		}
	}

	profitMeta := tokenMeta{"?", 18}
	profitHuman := "0"
	var profitFloat float64
	tokenPriceUSD := 0.0
	if bestAmt != nil && bestAmt.Sign() > 0 {
		profitMeta = getToken(ctx, client, bestTok)
		profitHuman = toHuman(bestAmt, profitMeta.Decimals)
		pf, _ := new(big.Float).SetInt(bestAmt).Float64()
		profitFloat = pf / pow10f(profitMeta.Decimals)
		tokenPriceUSD = hopPrices[strings.ToLower(bestTok)]
	}

	// Gas cost in USD: derive ETH price from the same hop price map.
	gasPriceWei := tx.GasPrice()
	gasCostWei := new(big.Int).Mul(gasPriceWei, big.NewInt(int64(receipt.GasUsed)))
	costETH, _ := new(big.Float).Quo(new(big.Float).SetInt(gasCostWei), new(big.Float).SetFloat64(1e18)).Float64()
	const wethAddr = "0x82af49447d8a07e3bd95bd0d56f35241523fbab1"
	wethPrice := hopPrices[wethAddr]
	if wethPrice <= 0 {
		wethPrice = tokenPriceUSD // best guess if WETH not in path
	}
	costUSD := costETH * wethPrice
	gasPriceGwei, _ := new(big.Float).Quo(new(big.Float).SetInt(gasPriceWei), new(big.Float).SetFloat64(1e9)).Float64()

	// NotionalUSD: competitor's notional = borrow amount (human) × token price.
	notionalUSD := 0.0
	if borrowAmount != "" && tokenPriceUSD > 0 {
		if bf, _, err := big.ParseFloat(borrowAmount, 10, 64, big.ToNearestEven); err == nil {
			amt, _ := bf.Float64()
			notionalUSD = amt * tokenPriceUSD
		}
	}
	profitBps := 0.0
	if notionalUSD > 0 {
		profitBps = profitUSD / notionalUSD * 10000.0
	}
	return ArbTx{
		TxHash:          txHash.Hex(),
		BlockNumber:     header.Number.Uint64(),
		Timestamp:       ts,
		BotContract:     botContract,
		Sender:          strings.ToLower(sender.Hex()),
		FlashLoanSource: flashSource,
		BorrowToken:     borrowToken,
		BorrowSymbol:    borrowSymbol,
		BorrowAmount:    borrowAmount,
		IsCircular:      isCircular,
		StartToken:      startToken,
		StartSymbol:     startSymbol,
		ProfitToken:     bestTok,
		ProfitSymbol:    profitMeta.Symbol,
		ProfitHuman:     profitHuman,
		ProfitNative:    profitFloat,
		ProfitUSD:       profitUSD,
		TokenPriceUSD:   tokenPriceUSD,
		HopPricesJSON:   hopPricesJSON,
		GasUsed:         receipt.GasUsed,
		GasGwei:         fmt.Sprintf("%.4f", gasPriceGwei),
		CostETH:         costETH,
		CostUSD:         costUSD,
		NetUSD:          profitUSD - costUSD,
		TxIndex:         receipt.TransactionIndex,
		NotionalUSD:     notionalUSD,
		ProfitBps:       profitBps,
		HopCount:        len(hops),
		Hops:            hops,
		PathStr:         pathStr,
	}, true
}

func decodeHops(ctx context.Context, client *ethclient.Client, logs []types.Log) []SwapHop {
	var hops []SwapHop
	for _, l := range logs {
		if len(l.Topics) == 0 { continue }
		t := l.Topics[0]
		poolLo := strings.ToLower(l.Address.Hex())

		if t == swapV3Topic || t == swapAlgTopic {
			if len(l.Data) < 64 { continue }
			pool := getPool(ctx, client, poolLo, true)
			if pool.Token0 == "" || pool.Token1 == "" { continue }

			a0 := new(big.Int).SetBytes(l.Data[:32])
			if a0.Bit(255) == 1 { a0.Sub(a0, new(big.Int).Lsh(big.NewInt(1), 256)) }
			a1 := new(big.Int).SetBytes(l.Data[32:64])
			if a1.Bit(255) == 1 { a1.Sub(a1, new(big.Int).Lsh(big.NewInt(1), 256)) }

			var tokIn, tokOut string
			var amtIn, amtOut *big.Int
			if a0.Sign() > 0 {
				tokIn, tokOut = pool.Token0, pool.Token1
				amtIn, amtOut = a0, new(big.Int).Neg(a1)
			} else {
				tokIn, tokOut = pool.Token1, pool.Token0
				amtIn, amtOut = a1, new(big.Int).Neg(a0)
			}
			if amtIn.Sign() <= 0 || amtOut.Sign() <= 0 { continue }

			mIn := getToken(ctx, client, tokIn)
			mOut := getToken(ctx, client, tokOut)
			hops = append(hops, SwapHop{
				Pool: poolLo, DEX: pool.DEX, FeeBps: pool.FeeBps,
				TokenIn: tokIn, SymbolIn: mIn.Symbol, AmountIn: toHuman(amtIn, mIn.Decimals), DecIn: mIn.Decimals,
				TokenOut: tokOut, SymbolOut: mOut.Symbol, AmountOut: toHuman(amtOut, mOut.Decimals), DecOut: mOut.Decimals,
			})

		} else if t == v4SwapTopic {
			// V4 Swap(bytes32 indexed id, address indexed sender,
			//         int128 amount0, int128 amount1, uint160 sqrtPriceX96After,
			//         uint128 liquidityAfter, int24 tick, uint24 fee)
			// Topics[1]=poolId, Topics[2]=sender. Data: 6 fields × 32 bytes = 192 bytes.
			if len(l.Topics) < 2 || len(l.Data) < 64 { continue }
			poolId := l.Topics[1]

			v4CacheMu.Lock()
			tokens, ok := v4Cache[poolId]
			v4CacheMu.Unlock()
			if !ok { continue }

			tok0, tok1 := tokens[0], tokens[1]
			a0 := new(big.Int).SetBytes(l.Data[0:32])
			if a0.Bit(255) == 1 { a0.Sub(a0, new(big.Int).Lsh(big.NewInt(1), 256)) }
			a1 := new(big.Int).SetBytes(l.Data[32:64])
			if a1.Bit(255) == 1 { a1.Sub(a1, new(big.Int).Lsh(big.NewInt(1), 256)) }

			// Extract fee (uint24) from data[160:192] if available
			var feeBps uint32
			if len(l.Data) >= 192 {
				feeRaw := new(big.Int).SetBytes(l.Data[160:192])
				// V4 fee is in ppm (parts per million): 3000 = 0.30% = 30 bps
				if feeRaw.IsUint64() && feeRaw.Uint64() > 0 {
					feeBps = uint32(feeRaw.Uint64() / 100)
				}
			}

			var tokIn, tokOut string
			var amtIn, amtOut *big.Int
			if a0.Sign() > 0 {
				tokIn, tokOut = tok0, tok1
				amtIn, amtOut = a0, new(big.Int).Neg(a1)
			} else {
				tokIn, tokOut = tok1, tok0
				amtIn, amtOut = a1, new(big.Int).Neg(a0)
			}
			if amtIn.Sign() <= 0 || amtOut.Sign() <= 0 { continue }

			mIn := getToken(ctx, client, tokIn)
			mOut := getToken(ctx, client, tokOut)
			// Store the FULL V4 poolId (bytes32, 66 chars) so downstream tools
			// (comparison loop, dashboard) can join it against v4_pools.pool_id.
			// Earlier versions truncated to 12 chars which broke that lookup.
			poolIdStr := strings.ToLower(poolId.Hex())
			hops = append(hops, SwapHop{
				Pool: poolIdStr, DEX: "UniV4", FeeBps: feeBps,
				TokenIn: tokIn, SymbolIn: mIn.Symbol, AmountIn: toHuman(amtIn, mIn.Decimals), DecIn: mIn.Decimals,
				TokenOut: tokOut, SymbolOut: mOut.Symbol, AmountOut: toHuman(amtOut, mOut.Decimals), DecOut: mOut.Decimals,
			})

		} else if t == curveExchangeTopic || t == curveExchangeUnderlyingTopic {
			// TokenExchange(address indexed buyer, int128 sold_id, uint256 tokens_sold, int128 bought_id, uint256 tokens_bought)
			// Data layout (each field padded to 32 bytes): sold_id | tokens_sold | bought_id | tokens_bought
			if len(l.Data) < 128 { continue }
			soldIdx := new(big.Int).SetBytes(l.Data[0:32]).Int64()
			tokensSold := new(big.Int).SetBytes(l.Data[32:64])
			boughtIdx := new(big.Int).SetBytes(l.Data[64:96]).Int64()
			tokensBought := new(big.Int).SetBytes(l.Data[96:128])
			if tokensSold.Sign() <= 0 || tokensBought.Sign() <= 0 { continue }

			tokIn := getCurveCoins(ctx, client, poolLo, soldIdx)
			tokOut := getCurveCoins(ctx, client, poolLo, boughtIdx)
			if tokIn == "" || tokOut == "" { continue }

			mIn := getToken(ctx, client, tokIn)
			mOut := getToken(ctx, client, tokOut)
			hops = append(hops, SwapHop{
				Pool: poolLo, DEX: "Curve", FeeBps: 4, // Curve base fee ~0.04%
				TokenIn: tokIn, SymbolIn: mIn.Symbol, AmountIn: toHuman(tokensSold, mIn.Decimals), DecIn: mIn.Decimals,
				TokenOut: tokOut, SymbolOut: mOut.Symbol, AmountOut: toHuman(tokensBought, mOut.Decimals), DecOut: mOut.Decimals,
			})

		} else if t == balancerSwapTopic {
			// Swap(bytes32 indexed poolId, address indexed tokenIn, address indexed tokenOut, uint256 amountIn, uint256 amountOut)
			// Emitted by the Balancer Vault singleton.
			// Topics[1]=poolId (first 20 bytes = pool address), Topics[2]=tokenIn, Topics[3]=tokenOut
			// Data: amountIn (32 bytes) | amountOut (32 bytes)
			if len(l.Topics) < 4 || len(l.Data) < 64 { continue }
			poolAddr := strings.ToLower(common.BytesToAddress(l.Topics[1][:20]).Hex())
			tokIn := strings.ToLower(common.HexToAddress(l.Topics[2].Hex()).Hex())
			tokOut := strings.ToLower(common.HexToAddress(l.Topics[3].Hex()).Hex())
			amtIn := new(big.Int).SetBytes(l.Data[0:32])
			amtOut := new(big.Int).SetBytes(l.Data[32:64])
			if amtIn.Sign() <= 0 || amtOut.Sign() <= 0 { continue }

			mIn := getToken(ctx, client, tokIn)
			mOut := getToken(ctx, client, tokOut)
			hops = append(hops, SwapHop{
				Pool: poolAddr, DEX: "Balancer",
				TokenIn: tokIn, SymbolIn: mIn.Symbol, AmountIn: toHuman(amtIn, mIn.Decimals), DecIn: mIn.Decimals,
				TokenOut: tokOut, SymbolOut: mOut.Symbol, AmountOut: toHuman(amtOut, mOut.Decimals), DecOut: mOut.Decimals,
			})

		} else if t == dodoSwapTopic {
			// DODOSwap(address fromToken, address toToken, uint256 fromAmount, uint256 toAmount, address trader, address receiver)
			// All params non-indexed — packed in Data as 6 × 32 bytes.
			if len(l.Data) < 128 { continue }
			tokIn := strings.ToLower(common.BytesToAddress(l.Data[12:32]).Hex())   // address is right-aligned in 32 bytes
			tokOut := strings.ToLower(common.BytesToAddress(l.Data[44:64]).Hex())
			amtIn := new(big.Int).SetBytes(l.Data[64:96])
			amtOut := new(big.Int).SetBytes(l.Data[96:128])
			if amtIn.Sign() <= 0 || amtOut.Sign() <= 0 { continue }

			mIn := getToken(ctx, client, tokIn)
			mOut := getToken(ctx, client, tokOut)
			hops = append(hops, SwapHop{
				Pool: poolLo, DEX: "DODO",
				TokenIn: tokIn, SymbolIn: mIn.Symbol, AmountIn: toHuman(amtIn, mIn.Decimals), DecIn: mIn.Decimals,
				TokenOut: tokOut, SymbolOut: mOut.Symbol, AmountOut: toHuman(amtOut, mOut.Decimals), DecOut: mOut.Decimals,
			})

		} else if t == swapV2Topic {
			if len(l.Data) < 128 { continue }
			pool := getPool(ctx, client, poolLo, false)
			if pool.Token0 == "" || pool.Token1 == "" { continue }

			a0in := new(big.Int).SetBytes(l.Data[0:32])
			a1in := new(big.Int).SetBytes(l.Data[32:64])
			a0out := new(big.Int).SetBytes(l.Data[64:96])
			a1out := new(big.Int).SetBytes(l.Data[96:128])

			var tokIn, tokOut string
			var amtIn, amtOut *big.Int
			if a0in.Sign() > 0 {
				tokIn, tokOut, amtIn, amtOut = pool.Token0, pool.Token1, a0in, a1out
			} else {
				tokIn, tokOut, amtIn, amtOut = pool.Token1, pool.Token0, a1in, a0out
			}
			if amtIn.Sign() <= 0 || amtOut.Sign() <= 0 { continue }

			mIn := getToken(ctx, client, tokIn)
			mOut := getToken(ctx, client, tokOut)
			hops = append(hops, SwapHop{
				Pool: poolLo, DEX: pool.DEX, FeeBps: pool.FeeBps,
				TokenIn: tokIn, SymbolIn: mIn.Symbol, AmountIn: toHuman(amtIn, mIn.Decimals), DecIn: mIn.Decimals,
				TokenOut: tokOut, SymbolOut: mOut.Symbol, AmountOut: toHuman(amtOut, mOut.Decimals), DecOut: mOut.Decimals,
			})
		}
	}
	return hops
}

// ── Helpers ───────────────────────────────────────────────────────────────────

func toHuman(v *big.Int, dec uint8) string {
	if v == nil { return "0" }
	f, _ := new(big.Float).SetInt(v).Float64()
	return fmt.Sprintf("%.6f", f/pow10f(dec))
}

func pow10f(n uint8) float64 {
	r := 1.0
	for i := 0; i < int(n); i++ { r *= 10 }
	return r
}

// hopDexes returns a comma-separated list of unique DEX names from a hop slice.
func hopDexes(hops []SwapHop) string {
	seen := make(map[string]struct{})
	var out []string
	for _, h := range hops {
		if _, ok := seen[h.DEX]; !ok {
			seen[h.DEX] = struct{}{}
			out = append(out, h.DEX)
		}
	}
	return strings.Join(out, ",")
}

// ── Summary ───────────────────────────────────────────────────────────────────

func writeSummary(txs []ArbTx) {
	f, _ := os.Create("/tmp/arbscan_summary.txt")
	if f == nil { return }
	defer f.Close()
	p := func(s string, a ...interface{}) { fmt.Fprintf(f, s, a...) }

	p("=== Arbitrum Arb Scan Summary (%d transactions) ===\n\n", len(txs))
	if len(txs) == 0 { p("No arb transactions found.\n"); return }

	var totGross, totCost, totNet float64
	for _, tx := range txs { totGross += tx.ProfitUSD; totCost += tx.CostUSD; totNet += tx.NetUSD }
	p("Gross profit: $%.4f\nGas cost:    $%.4f\nNet profit:  $%.4f\n\n", totGross, totCost, totNet)

	count := func(field func(ArbTx)string) map[string]int {
		m := make(map[string]int)
		for _, tx := range txs { m[field(tx)]++ }
		return m
	}
	printTop := func(title string, m map[string]int, n int) {
		type kv struct{ k string; v int }
		var sl []kv
		for k, v := range m { sl = append(sl, kv{k, v}) }
		sort.Slice(sl, func(i, j int) bool { return sl[i].v > sl[j].v })
		p("--- %s ---\n", title)
		for i, x := range sl { if i >= n { break }; p("  %4d  %s\n", x.v, x.k) }
		p("\n")
	}

	printTop("Flash Loan Sources", count(func(t ArbTx)string{return t.FlashLoanSource}), 10)
	printTop("Bot Contracts (top 20)", count(func(t ArbTx)string{return t.BotContract}), 20)
	printTop("Borrow/Start Token", count(func(t ArbTx)string{return t.StartSymbol}), 15)
	printTop("Profit Token", count(func(t ArbTx)string{return t.ProfitSymbol}), 15)

	hopDist := make(map[int]int)
	for _, tx := range txs { hopDist[tx.HopCount]++ }
	var hk []int
	for k := range hopDist { hk = append(hk, k) }
	sort.Ints(hk)
	p("--- Hop Count Distribution ---\n")
	for _, k := range hk { p("  %d hops: %d txs\n", k, hopDist[k]) }
	p("\n")

	dexCount := make(map[string]int)
	for _, tx := range txs { for _, h := range tx.Hops { dexCount[h.DEX]++ } }
	printTop("DEX Usage (hops)", dexCount, 15)

	pathCount := make(map[string]int)
	for _, tx := range txs { pathCount[tx.PathStr]++ }
	printTop("Top 40 Paths", pathCount, 40)

	buckets := []struct{ l string; lo, hi float64 }{
		{"<$0.01",0,0.01},{"$0.01-$0.10",0.01,0.10},{"$0.10-$1",0.10,1},
		{"$1-$10",1,10},{"$10-$100",10,100},{">$100",100,1e18},
	}
	p("--- Gross Profit Buckets ---\n")
	for _, b := range buckets {
		c := 0
		for _, tx := range txs { if tx.ProfitUSD >= b.lo && tx.ProfitUSD < b.hi { c++ } }
		p("  %-18s %d txs\n", b.l, c)
	}
	p("\n")

	sorted := make([]ArbTx, len(txs))
	copy(sorted, txs)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].ProfitUSD > sorted[j].ProfitUSD })
	p("--- Top 30 Transactions ---\n")
	for i, tx := range sorted {
		if i >= 30 { break }
		p("  #%2d  profit=$%8.4f  net=$%8.4f  hops=%d  flash=%-20s  %s\n",
			i+1, tx.ProfitUSD, tx.NetUSD, tx.HopCount, tx.FlashLoanSource, tx.PathStr)
	}

	log.Println("[arbscan] summary → /tmp/arbscan_summary.txt")
}
