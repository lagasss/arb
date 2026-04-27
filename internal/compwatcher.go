package internal

// compwatcher polls the competitor_arbs SQLite table for new high-profit
// observations and auto-adds any unknown pools to the live registry so the
// bot can compete on the same routes.
//
// Flow:
//   arbscan → competitor_arbs row → compwatcher sees profit > threshold
//   → on-chain: token0/token1/fee/factory per hop pool
//   → map factory → DEXType
//   → ForceAdd to registry + graph
//   → notify swap listener to resubscribe
//   → trigger cycle cache rebuild

import (
	"context"
	"encoding/json"
	"log"
	"math/big"
	"strings"
	"time"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/ethclient"
)

// compHop is the minimal shape we need from competitor hops_json.
type compHop struct {
	Pool     string `json:"pool"`
	TokenIn  string `json:"token_in"`
	TokenOut string `json:"token_out"`
}

// poolQueryABI is used to call token0/token1/fee/factory on any pool.
var poolQueryABI abi.ABI

func init() {
	var err error
	poolQueryABI, err = abi.JSON(strings.NewReader(`[
		{"name":"token0","type":"function","stateMutability":"view","inputs":[],"outputs":[{"type":"address"}]},
		{"name":"token1","type":"function","stateMutability":"view","inputs":[],"outputs":[{"type":"address"}]},
		{"name":"fee","type":"function","stateMutability":"view","inputs":[],"outputs":[{"type":"uint24"}]},
		{"name":"factory","type":"function","stateMutability":"view","inputs":[],"outputs":[{"type":"address"}]}
	]`))
	if err != nil {
		panic("poolQueryABI: " + err.Error())
	}
}

// CompWatcher watches competitor_arbs and adds new profitable pools.
type CompWatcher struct {
	db          *DB
	client      *ethclient.Client
	registry    *PoolRegistry
	graph       *Graph
	tokens      *TokenRegistry
	swapListen  *SwapListener
	cycleCache  *CycleCache
	minProfitUSD float64
}

func NewCompWatcher(db *DB, client *ethclient.Client, registry *PoolRegistry,
	graph *Graph, tokens *TokenRegistry, sl *SwapListener, cc *CycleCache,
	minProfitUSD float64) *CompWatcher {
	return &CompWatcher{
		db:           db,
		client:       client,
		registry:     registry,
		graph:        graph,
		tokens:       tokens,
		swapListen:   sl,
		cycleCache:   cc,
		minProfitUSD: minProfitUSD,
	}
}

// Run polls every 30 s. Blocks until ctx is cancelled.
//
// On startup it FIRST scans historical competitor_arbs (from rowid 0 forward)
// to ensure any pools observed by competitors before the bot started are
// added to the registry. This is a one-time backfill — subsequent ticks only
// look at new rows. It runs in chunks of 100 rows at a time to avoid
// blocking startup if there are millions of historical entries.
func (cw *CompWatcher) Run(ctx context.Context) {
	// Historical backfill: start from rowid 0 and walk forward until we
	// reach the current max. Do it in tight chunks because each pool resolution
	// is an RPC call (~100ms) — the loop is rate-limited by RPC, not by DB.
	var lastID int64
	maxID, _ := cw.db.maxCompetitorArbID()
	if maxID > 0 {
		log.Printf("[compwatcher] backfilling historical pools (rows 1..%d)...", maxID)
		start := time.Now()
		for lastID < maxID {
			select {
			case <-ctx.Done():
				return
			default:
			}
			before := lastID
			cw.check(ctx, &lastID)
			if lastID == before {
				// No new rows returned (all below profit threshold) — skip ahead
				lastID += 100
			}
		}
		log.Printf("[compwatcher] historical backfill done in %s", time.Since(start).Round(time.Second))
	}

	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			cw.check(ctx, &lastID)
		}
	}
}

func (cw *CompWatcher) check(ctx context.Context, lastID *int64) {
	rows, err := cw.db.newCompetitorArbs(*lastID, cw.minProfitUSD)
	if err != nil {
		log.Printf("[compwatcher] query error: %v", err)
		return
	}
	if len(rows) == 0 {
		return
	}

	added := 0
	for _, row := range rows {
		if row.ID > *lastID {
			*lastID = row.ID
		}
		var hops []compHop
		if err := json.Unmarshal([]byte(row.HopsJSONStr), &hops); err != nil {
			continue
		}
		for _, hop := range hops {
			if hop.Pool == "" {
				continue
			}
			lo := strings.ToLower(hop.Pool)
			if _, exists := cw.registry.Get(lo); exists {
				continue // already known
			}
			// Use the canonical resolver which also runs VerifyPool.
			p := ResolvePoolFromChain(ctx, cw.client, cw.registry, cw.tokens, lo)
			if p == nil {
				continue
			}
			// Persist verification status to DB.
			p.LastUpdated = time.Now()
			_ = cw.db.UpsertPool(p)
			_ = cw.db.SetPoolVerified(lo, p.Verified, p.VerifyReason)
			if canonical, ok := cw.registry.ForceAdd(p); ok {
				cw.graph.AddPool(canonical)
				added++
				log.Printf("[compwatcher] added pool %s (%s %s/%s fee=%d) from competitor arb $%.2f",
					lo[:14], canonical.DEX, canonical.Token0.Symbol, canonical.Token1.Symbol, canonical.FeeBps, row.ProfitUSD)
			}
		}
	}

	if added > 0 {
		// Trigger swap listener resubscription and cycle cache rebuild.
		cw.swapListen.NotifyPoolAdded()
	}
}

// resolvePool calls token0/token1/fee/factory on-chain and builds a Pool.
// Returns nil if the pool can't be resolved or its factory is unknown.
func (cw *CompWatcher) resolvePool(ctx context.Context, addr string) *Pool {
	tctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()

	a := common.HexToAddress(addr)

	callStr := func(name string) string {
		d, err := poolQueryABI.Pack(name)
		if err != nil {
			return ""
		}
		res, err := cw.client.CallContract(tctx, ethereum.CallMsg{To: &a, Data: d}, nil)
		if err != nil || len(res) < 32 {
			return ""
		}
		vals, err := poolQueryABI.Unpack(name, res)
		if err != nil || len(vals) == 0 {
			return ""
		}
		if addr, ok := vals[0].(common.Address); ok {
			return strings.ToLower(addr.Hex())
		}
		return ""
	}

	factoryLo := callStr("factory")
	dexType, known := FactoryAddresses[factoryLo]
	if !known {
		// Unknown factory — skip silently (likely a DEX we don't support)
		return nil
	}

	t0addr := callStr("token0")
	t1addr := callStr("token1")
	if t0addr == "" || t1addr == "" {
		return nil
	}

	// Resolve fee from on-chain data.
	// V3 pools: fee() returns uint24 in ppm (e.g. 3000 = 30 bps).
	// V2 pools: no fee() getter. Derive from getAmountOut vs xy=k math.
	var feeBps uint32
	if d, err := poolQueryABI.Pack("fee"); err == nil {
		if res, err := cw.client.CallContract(tctx, ethereum.CallMsg{To: &a, Data: d}, nil); err == nil && len(res) >= 32 {
			if vals, err := poolQueryABI.Unpack("fee", res); err == nil && len(vals) > 0 {
				switch v := vals[0].(type) {
				case uint32:
					feeBps = v / 100
				case *big.Int:
					if v.IsInt64() {
						feeBps = uint32(v.Int64() / 100)
					}
				}
			}
		}
	}

	// V2 pools: derive effective fee from getReserves + getAmountOut.
	// getAmountOut(uint256,address) on the pair returns the true output including all fees.
	if feeBps == 0 {
		feeBps = cw.calibrateV2Fee(tctx, a, t0addr)
	}

	t0 := cw.resolveToken(ctx, t0addr)
	t1 := cw.resolveToken(ctx, t1addr)

	return &Pool{
		Address:      addr,
		DEX:          dexType,
		FeeBps:       feeBps,
		Token0:       t0,
		Token1:       t1,
		// TVL/volume start at 0 — populated on next multicall. See bot.go seed
		// pool comment for rationale.
		TVLUSD:       0,
		Volume24hUSD: 0,
	}
}

func (cw *CompWatcher) resolveToken(ctx context.Context, addr string) *Token {
	if t, ok := cw.tokens.Get(addr); ok {
		return t
	}
	tctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	t := FetchTokenMeta(tctx, cw.client, addr)
	cw.tokens.Add(t)
	return t
}

// calibrateV2Fee derives the effective swap fee for a V2-style pool by calling
// getReserves() and getAmountOut(testAmt, token0) on-chain, then computing:
//   fee_bps = (rawXYK - actualOut) * 10000 / rawXYK
// Returns 0 if the call fails (caller should use a sensible default).
func (cw *CompWatcher) calibrateV2Fee(ctx context.Context, poolAddr common.Address, token0Addr string) uint32 {
	// getReserves()
	resData := []byte{0x09, 0x02, 0xf1, 0xac}
	res, err := cw.client.CallContract(ctx, ethereum.CallMsg{To: &poolAddr, Data: resData}, nil)
	if err != nil || len(res) < 64 {
		return 0
	}
	r0 := new(big.Int).SetBytes(res[0:32])
	r1 := new(big.Int).SetBytes(res[32:64])
	if r0.Sign() == 0 || r1.Sign() == 0 {
		return 0
	}

	// Test amount: 0.1% of reserve0
	testAmt := new(big.Int).Div(r0, big.NewInt(1000))
	if testAmt.Sign() == 0 {
		testAmt = big.NewInt(1)
	}

	// getAmountOut(uint256, address) — selector 0xf140a35a
	gaoData := make([]byte, 4+64)
	copy(gaoData[:4], []byte{0xf1, 0x40, 0xa3, 0x5a})
	testAmt.FillBytes(gaoData[4:36])
	copy(gaoData[36+12:68], common.HexToAddress(token0Addr).Bytes())

	gaoRes, err := cw.client.CallContract(ctx, ethereum.CallMsg{To: &poolAddr, Data: gaoData}, nil)
	if err != nil || len(gaoRes) < 32 {
		// Pool doesn't support getAmountOut — try standard xy=k fee derivation
		// by calling the router's getAmountsOut (not available without path).
		// Fall back to 30 bps as a last resort.
		return 30
	}
	actualOut := new(big.Int).SetBytes(gaoRes[:32])
	if actualOut.Sign() == 0 {
		return 30
	}

	// rawXYK = testAmt * r1 / (r0 + testAmt)
	num := new(big.Int).Mul(testAmt, r1)
	denom := new(big.Int).Add(r0, testAmt)
	rawOut := new(big.Int).Div(num, denom)
	if rawOut.Sign() == 0 || rawOut.Cmp(actualOut) <= 0 {
		// Stable-swap math: actual > xy=k. Mark as low fee.
		return 1
	}

	// fee_bps = (rawOut - actualOut) * 10000 / rawOut
	diff := new(big.Int).Sub(rawOut, actualOut)
	feeBps := new(big.Int).Mul(diff, big.NewInt(10000))
	feeBps.Div(feeBps, rawOut)
	if feeBps.IsUint64() && feeBps.Uint64() > 0 && feeBps.Uint64() < 1000 {
		return uint32(feeBps.Uint64())
	}
	return 30 // unreasonable result — default
}
