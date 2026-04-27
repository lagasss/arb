package internal

import (
	"context"
	"fmt"
	"log"
	"math/big"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/ethclient"
)

var (
	v3SwapABI abi.ABI
	v2SwapABI abi.ABI
	v4SwapABI abi.ABI
	// v3MintBurnABI carries the Mint + Burn events for UniV3-family pools.
	// Emitted by the pool itself when LPs add/remove liquidity. Changes the set
	// of initialized ticks in the pool's bitmap, so our cached bitmap must
	// re-fetch to stay accurate. Without this subscription the sim silently
	// mispriced hops whose bitmap had ticks added since the last 5s sweep.
	v3MintBurnABI abi.ABI
	// v4ModifyLiquidityABI is the V4 equivalent emitted by the PoolManager
	// singleton (indexed poolId in Topics[1]).
	v4ModifyLiquidityABI abi.ABI
)

func init() {
	var err error
	// UniV3 / SushiV3 / Algebra Swap event — non-indexed: amount0, amount1, sqrtPriceX96, liquidity, tick
	v3SwapABI, err = abi.JSON(strings.NewReader(`[{
		"anonymous":false,"name":"Swap","type":"event",
		"inputs":[
			{"indexed":true, "name":"sender",       "type":"address"},
			{"indexed":true, "name":"recipient",    "type":"address"},
			{"indexed":false,"name":"amount0",      "type":"int256"},
			{"indexed":false,"name":"amount1",      "type":"int256"},
			{"indexed":false,"name":"sqrtPriceX96", "type":"uint160"},
			{"indexed":false,"name":"liquidity",    "type":"uint128"},
			{"indexed":false,"name":"tick",         "type":"int24"}
		]
	}]`))
	if err != nil {
		panic(fmt.Sprintf("v3SwapABI: %v", err))
	}

	// UniV2 / Camelot / SushiV2 Swap event — non-indexed: amount0In, amount1In, amount0Out, amount1Out
	v2SwapABI, err = abi.JSON(strings.NewReader(`[{
		"anonymous":false,"name":"Swap","type":"event",
		"inputs":[
			{"indexed":true, "name":"sender",    "type":"address"},
			{"indexed":false,"name":"amount0In", "type":"uint256"},
			{"indexed":false,"name":"amount1In", "type":"uint256"},
			{"indexed":false,"name":"amount0Out","type":"uint256"},
			{"indexed":false,"name":"amount1Out","type":"uint256"},
			{"indexed":true, "name":"to",        "type":"address"}
		]
	}]`))
	if err != nil {
		panic(fmt.Sprintf("v2SwapABI: %v", err))
	}

	// UniV4 Swap event — emitted by the PoolManager singleton.
	// indexed: id (bytes32 poolId), sender (address)
	// non-indexed: amount0 (int128), amount1 (int128), sqrtPriceX96 (uint160), liquidity (uint128), tick (int24), fee (uint24)
	v4SwapABI, err = abi.JSON(strings.NewReader(`[{
		"anonymous":false,"name":"Swap","type":"event",
		"inputs":[
			{"indexed":true,  "name":"id",           "type":"bytes32"},
			{"indexed":true,  "name":"sender",       "type":"address"},
			{"indexed":false, "name":"amount0",      "type":"int128"},
			{"indexed":false, "name":"amount1",      "type":"int128"},
			{"indexed":false, "name":"sqrtPriceX96", "type":"uint160"},
			{"indexed":false, "name":"liquidity",    "type":"uint128"},
			{"indexed":false, "name":"tick",         "type":"int24"}
		]
	}]`))
	if err != nil {
		panic(fmt.Sprintf("v4SwapABI: %v", err))
	}

	// UniV3 Mint + Burn. Both change the set of initialized ticks between
	// tickLower and tickUpper, invalidating the cached bitmap for this pool.
	// We only use the topic signatures — payload decoding is unnecessary since
	// the handler just enqueues the pool for tick refetch.
	v3MintBurnABI, err = abi.JSON(strings.NewReader(`[
		{"anonymous":false,"name":"Mint","type":"event","inputs":[
			{"indexed":false,"name":"sender",    "type":"address"},
			{"indexed":true, "name":"owner",     "type":"address"},
			{"indexed":true, "name":"tickLower", "type":"int24"},
			{"indexed":true, "name":"tickUpper", "type":"int24"},
			{"indexed":false,"name":"amount",    "type":"uint128"},
			{"indexed":false,"name":"amount0",   "type":"uint256"},
			{"indexed":false,"name":"amount1",   "type":"uint256"}
		]},
		{"anonymous":false,"name":"Burn","type":"event","inputs":[
			{"indexed":true, "name":"owner",     "type":"address"},
			{"indexed":true, "name":"tickLower", "type":"int24"},
			{"indexed":true, "name":"tickUpper", "type":"int24"},
			{"indexed":false,"name":"amount",    "type":"uint128"},
			{"indexed":false,"name":"amount0",   "type":"uint256"},
			{"indexed":false,"name":"amount1",   "type":"uint256"}
		]}
	]`))
	if err != nil {
		panic(fmt.Sprintf("v3MintBurnABI: %v", err))
	}

	// V4 ModifyLiquidity on the PoolManager singleton. poolId in Topics[1].
	v4ModifyLiquidityABI, err = abi.JSON(strings.NewReader(`[{
		"anonymous":false,"name":"ModifyLiquidity","type":"event",
		"inputs":[
			{"indexed":true, "name":"id",             "type":"bytes32"},
			{"indexed":true, "name":"sender",         "type":"address"},
			{"indexed":false,"name":"tickLower",      "type":"int24"},
			{"indexed":false,"name":"tickUpper",      "type":"int24"},
			{"indexed":false,"name":"liquidityDelta", "type":"int256"},
			{"indexed":false,"name":"salt",           "type":"bytes32"}
		]
	}]`))
	if err != nil {
		panic(fmt.Sprintf("v4ModifyLiquidityABI: %v", err))
	}
}

// SwapListener subscribes to Swap events on all known pool addresses and
// updates pool state directly from event data — no additional RPC call needed.
// If FastEval is set, it is called inline after each pool-state update for
// low-latency evaluation of pre-cached cycles touching that pool.
type SwapListener struct {
	client   *ethclient.Client
	registry *PoolRegistry
	graph    *Graph
	resubCh  chan struct{}    // signals that new pools were added
	FastEval     func(pool *Pool)       // optional: called inline after state update
	FastEvalPeer func(pool *Pool)       // optional: called for same-pair peers (skips price-delta throttle)
	RefreshPools func([]*Pool)          // optional: targeted multicall refresh before cross-pool scoring
	OnSwap       func()                 // optional: called on every swap event (for health tracking)
	// OnBlockSeen is invoked with lg.BlockNumber for every swap log the
	// listener processes. The bot wires this to advance Health.LatestBlock
	// as a fallback when the WSS new-head subscription drops (Chainstack
	// reconnects every 10-20 min; during the resubscribe window LatestBlock
	// would otherwise freeze while pool StateBlocks advance via this very
	// stream, producing the sb_lag=-30b observation artifact).
	OnBlockSeen  func(uint64)

	// TickRefetchCh is an optional channel the listener writes to when a V3
	// swap event indicates the pool's tick has crossed enough boundaries that
	// the cached tick bitmap may not cover the new active range. The receiver
	// (watchTickMapsLoop) does an immediate single-pool re-fetch instead of
	// waiting for the next periodic 5s pass. Buffered, non-blocking sends —
	// drops on overflow because the periodic sweep will catch up.
	TickRefetchCh chan *Pool

	// AlgebraFeeRefreshCh is an optional channel the listener writes to when
	// an Algebra-style swap event lands (CamelotV3, ZyberV3). The Algebra
	// Swap event payload doesn't include the post-swap dynamic fee — the
	// receiver (an async worker) calls globalState() to read the fresh fee
	// and updates the pool. Buffered, non-blocking sends.
	AlgebraFeeRefreshCh chan *Pool

	// TickEagerRefetchSpacings is the |newTick - TickAtFetch| / TickSpacing
	// threshold above which a Swap event triggers an eager TickRefetchCh send.
	// 0 disables the eager path entirely. Set from cfg.Trading.
	TickEagerRefetchSpacings int32
	// TickEagerRefetchCooldown is the per-pool minimum interval between
	// successive eager TickRefetchCh sends — prevents thrashing when a pool
	// swaps many times per block. Set from cfg.Trading.
	TickEagerRefetchCooldown time.Duration
	// lastTickRefetch records the most recent eager-fire time per pool address
	// (lowercased). Read on every Swap; written when a pool is sent to the
	// TickRefetchCh. Uses sync.Map for lock-free hot-path access from the
	// single listener goroutine plus any future fan-out.
	lastTickRefetch sync.Map // map[string]time.Time
	// lastAlgebraFeeRefresh records the most recent AlgebraFeeRefreshCh send
	// time per pool address (lowercased). Same role as lastTickRefetch but
	// for the Algebra dynamic-fee path.
	lastAlgebraFeeRefresh sync.Map // map[string]time.Time
	// AlgebraFeeRefreshCooldown is the per-pool minimum interval between
	// successive AlgebraFeeRefreshCh sends. 0 disables the cooldown.
	AlgebraFeeRefreshCooldown time.Duration

	// TickDropCounter and TickEnqueueCounter are bumped by every attempted
	// send on TickRefetchCh. If the channel buffer is full, TickDropCounter
	// records the drop; otherwise TickEnqueueCounter records the enqueue.
	// Wired to b.health.TickFetchEagerDropped / .TickFetchEagerEnqueued so
	// the stats logger and /debug/tick-health can surface the drop rate.
	// nil counters are safe: the send path skips the bump but still attempts
	// the non-blocking send.
	TickDropCounter    *atomic.Uint64
	TickEnqueueCounter *atomic.Uint64

	swapsTotal uint64 // cumulative swap events received

	// Instrumentation for the logs-channel depth. Sampled on every receive;
	// peak is periodically logged and reset by a background goroutine so we
	// can detect when the listener is falling behind (i.e. fastEvalCycles is
	// CPU-bound and the RPC is producing swaps faster than we can process).
	maxLogsDepth atomic.Int64
}

// swapLogsChanCap is the buffer size of the logs channel. When the peak depth
// approaches this value, the listener is CPU-bound and swap events are being
// buffered at the RPC layer (or dropped). See `[swap] logs chan` log lines.
// Previously 256; observed full under bursty conditions (peak=257/cap=256)
// which adds pure queueing latency to every downstream event. Raised to 2048
// so a 30s burst of ~60 swaps/s can ride through without back-pressure.
const swapLogsChanCap = 2048

// swapDepthLogInterval is how often the background depth logger emits the
// current/peak occupancy of the logs channel.
const swapDepthLogInterval = 30 * time.Second

func NewSwapListener(client *ethclient.Client, registry *PoolRegistry, graph *Graph) *SwapListener {
	return &SwapListener{
		client:   client,
		registry: registry,
		graph:    graph,
		resubCh:  make(chan struct{}, 1),
	}
}

// NotifyPoolAdded signals that a new pool was added and the subscription
// should be rebuilt with the updated address set.
func (sl *SwapListener) NotifyPoolAdded() {
	select {
	case sl.resubCh <- struct{}{}:
	default:
	}
}

// Run manages the subscription lifecycle, restarting on error or when new pools are added.
func (sl *SwapListener) Run(ctx context.Context) {
	log.Println("[swap] listener started")
	for {
		err := sl.runOnce(ctx)
		if ctx.Err() != nil {
			return
		}
		if err != nil {
			log.Printf("[swap] restarting: %v", err)
			// Drain any pending resub signals, then immediately reconnect.
			for len(sl.resubCh) > 0 {
				<-sl.resubCh
			}
			// Brief pause to avoid hammering the RPC on repeated errors.
			select {
			case <-ctx.Done():
				return
			case <-time.After(2 * time.Second):
			}
		} else {
			// Clean exit (resub signal or ctx cancel) — restart immediately.
			for len(sl.resubCh) > 0 {
				<-sl.resubCh
			}
		}
	}
}

func (sl *SwapListener) runOnce(ctx context.Context) error {
	pools := sl.registry.All()
	if len(pools) == 0 {
		return nil
	}

	addrSet := make(map[common.Address]bool)
	hasV4 := false
	for _, p := range pools {
		if p.DEX == DEXUniswapV4 {
			hasV4 = true
			continue // V4 pools use PoolManager address, added below
		}
		addrSet[common.HexToAddress(p.Address)] = true
	}
	// V4: subscribe to the PoolManager singleton for all V4 swap events
	if hasV4 {
		addrSet[common.HexToAddress("0x360E68faCcca8cA495c1B759Fd9EEe466db9FB32")] = true
	}
	addrs := make([]common.Address, 0, len(addrSet))
	for a := range addrSet {
		addrs = append(addrs, a)
	}

	v3Topic := v3SwapABI.Events["Swap"].ID
	v2Topic := v2SwapABI.Events["Swap"].ID
	v4Topic := v4SwapABI.Events["Swap"].ID
	v3MintTopic := v3MintBurnABI.Events["Mint"].ID
	v3BurnTopic := v3MintBurnABI.Events["Burn"].ID
	v4ModLiqTopic := v4ModifyLiquidityABI.Events["ModifyLiquidity"].ID

	topics := []common.Hash{v3Topic, v2Topic, v3MintTopic, v3BurnTopic}
	if hasV4 {
		topics = append(topics, v4Topic, v4ModLiqTopic)
	}

	query := ethereum.FilterQuery{
		Addresses: addrs,
		Topics:    [][]common.Hash{topics},
	}

	logs := make(chan types.Log, swapLogsChanCap)
	sub, err := sl.client.SubscribeFilterLogs(ctx, query, logs)
	if err != nil {
		return fmt.Errorf("subscribe: %w", err)
	}
	defer sub.Unsubscribe()

	log.Printf("[swap] subscribed to %d pools", len(pools))

	// Start a background depth logger tied to this subscription's lifetime.
	// It emits `[swap] logs chan` lines every swapDepthLogInterval so we can
	// see whether the listener is keeping up with the RPC event stream.
	depthCtx, depthCancel := context.WithCancel(ctx)
	defer depthCancel()
	go func() {
		ticker := time.NewTicker(swapDepthLogInterval)
		defer ticker.Stop()
		for {
			select {
			case <-depthCtx.Done():
				return
			case <-ticker.C:
				peak := sl.maxLogsDepth.Swap(0)
				log.Printf("[swap] logs chan: current=%d peak_%ds=%d cap=%d",
					len(logs), int(swapDepthLogInterval.Seconds()), peak, swapLogsChanCap)
			}
		}
	}()

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-sl.resubCh:
			// Put it back so the outer Run loop sees it
			select {
			case sl.resubCh <- struct{}{}:
			default:
			}
			return nil
		case err := <-sub.Err():
			return fmt.Errorf("subscription: %w", err)
		case lg := <-logs:
			// Sample the channel depth BEFORE handleSwap so the peak reflects
			// actual queuing (handleSwap has already removed `lg`, so we add
			// 1 back to represent the receive-instant depth).
			if d := int64(len(logs) + 1); d > sl.maxLogsDepth.Load() {
				sl.maxLogsDepth.Store(d)
			}
			sl.handleSwap(lg)
		}
	}
}

func (sl *SwapListener) handleSwap(lg types.Log) {
	if len(lg.Topics) == 0 {
		return
	}
	// Advance the bot's LatestBlock reference from every swap log. Swap
	// events flow continuously via the SubscribeFilterLogs subscription —
	// even during the ~2-5s windows when the separate SubscribeNewHead WSS
	// drops and reconnects. Without this fallback LatestBlock goes stale
	// while pool StateBlocks (also driven by lg.BlockNumber) advance, and
	// the freshness gates compute negative sb_lag (pool ahead of LatestBlock).
	if sl.OnBlockSeen != nil && lg.BlockNumber > 0 {
		sl.OnBlockSeen(lg.BlockNumber)
	}

	v3Topic := v3SwapABI.Events["Swap"].ID
	v2Topic := v2SwapABI.Events["Swap"].ID
	v4Topic := v4SwapABI.Events["Swap"].ID
	v3MintTopic := v3MintBurnABI.Events["Mint"].ID
	v3BurnTopic := v3MintBurnABI.Events["Burn"].ID
	v4ModLiqTopic := v4ModifyLiquidityABI.Events["ModifyLiquidity"].ID

	// V4: Swap + ModifyLiquidity from the PoolManager singleton, keyed by
	// poolId in Topics[1].
	if (lg.Topics[0] == v4Topic || lg.Topics[0] == v4ModLiqTopic) && len(lg.Topics) >= 2 {
		poolID := strings.ToLower(lg.Topics[1].Hex())
		p, ok := sl.registry.Get(poolID)
		if !ok {
			return
		}
		// ModifyLiquidity: new initialized ticks may be added, or existing
		// ones removed. Apply the delta incrementally (fast path) + fire a
		// full refetch (safety net). Do NOT update slot0/liquidity from this
		// event — those change on swaps, not on liquidity adjustments.
		if lg.Topics[0] == v4ModLiqTopic {
			applyV4ModifyLiquidity(p, lg.Data, lg.BlockNumber)
			sl.fireTickRefetchImmediate(p)
			return
		}
		sl.applyV4Swap(p, lg.Data, lg.BlockNumber)
		atomic.AddUint64(&sl.swapsTotal, 1)
		sl.maybeFireTickRefetch(p)
		sl.graph.UpdateEdgeWeights(p)
		if sl.FastEval != nil {
			sl.FastEval(p)
		}
		if sl.FastEvalPeer != nil {
			peers := sl.registry.PeersFor(p)
			if len(peers) > 0 {
				if sl.RefreshPools != nil {
					sl.RefreshPools(peers)
				}
				for _, peer := range peers {
					sl.FastEvalPeer(peer)
				}
			}
		}
		return
	}

	// V2/V3: keyed by pool contract address
	p, ok := sl.registry.Get(lg.Address.Hex())
	if !ok {
		return
	}

	// Mint/Burn on a V3-family pool — changes the set of initialized ticks
	// and per-tick liquidityNet. Apply the delta incrementally so our sim
	// matches chain within one event-handler goroutine (~ms) rather than
	// waiting 100-250 ms for the full FetchTickMaps refetch to complete.
	// Still fire the refetch as a safety net so any missed event or decoder
	// bug is reconciled on the next periodic sweep.
	if lg.Topics[0] == v3MintTopic {
		applyV3LiquidityEvent(p, lg.Topics, lg.Data, true, lg.BlockNumber)
		sl.fireTickRefetchImmediate(p)
		return
	}
	if lg.Topics[0] == v3BurnTopic {
		applyV3LiquidityEvent(p, lg.Topics, lg.Data, false, lg.BlockNumber)
		sl.fireTickRefetchImmediate(p)
		return
	}

	switch lg.Topics[0] {
	case v3Topic:
		sl.applyV3Swap(p, lg.Data, lg.BlockNumber)
		sl.maybeFireTickRefetch(p)
		sl.maybeFireAlgebraFeeRefresh(p)
	case v2Topic:
		sl.applyV2Swap(p, lg.Data, lg.BlockNumber)
	default:
		return
	}

	atomic.AddUint64(&sl.swapsTotal, 1)
	if sl.OnSwap != nil {
		sl.OnSwap()
	}
	sl.graph.UpdateEdgeWeights(p)

	// Fast-path: evaluate pre-cached cycles for this pool inline. The CPU-bound
	// scoring runs across N workers in parallel; see fastEvalCycles for details.
	if sl.FastEval != nil {
		sl.FastEval(p)
	}

	// Cross-pool propagation: when pool P swaps and moves its price, OTHER pools
	// for the same token pair (different DEX/fee tier) become arb targets.
	//
	// CRITICAL: before scoring peers, refresh their on-chain state via a
	// targeted multicall. Without this, peer state is 5s stale (from the
	// last bulk multicall), creating phantom spreads: the swapped pool has
	// fresh state, peers have old state → sim sees a spread that doesn't
	// exist on-chain → 100% eth_call revert rate.
	//
	// The refresh fetches slot0+liquidity for just the peer pools (~50-100ms
	// for a typical 10-30 peer set via Multicall3). This brings all pools
	// in the cycle to the same block before ScoreCycle runs.
	if sl.FastEvalPeer != nil {
		peers := sl.registry.PeersFor(p)
		if len(peers) > 0 {
			if sl.RefreshPools != nil {
				sl.RefreshPools(peers)
			}
			for _, peer := range peers {
				sl.FastEvalPeer(peer)
			}
		}
	}
}

// maybeFireTickRefetch sends p to TickRefetchCh when the post-swap tick has
// drifted ≥ TickEagerRefetchSpacings from p.TickAtFetch (the tick at the time
// of the last successful FetchTickMaps pass). Per-pool cooldown protects
// against thrashing on hot pools that swap many times per block. Non-blocking
// send: if the channel is full the periodic 5s sweep will catch up.
// fireTickRefetchImmediate is the Mint/Burn path: the tick bitmap is known to
// be stale (initialized-tick set just changed on-chain) regardless of how far
// the current tick has drifted. Bypasses the drift threshold and per-pool
// cooldown used by maybeFireTickRefetch — every Mint/Burn must trigger a
// refetch, otherwise the sim keeps using a stale bitmap until the next 5s
// periodic sweep. Non-blocking send; drops on overflow (bitmap will be
// refreshed by the sweep shortly afterward anyway).
func (sl *SwapListener) fireTickRefetchImmediate(p *Pool) {
	if sl.TickRefetchCh == nil {
		return
	}
	select {
	case sl.TickRefetchCh <- p:
		if sl.TickEnqueueCounter != nil {
			sl.TickEnqueueCounter.Add(1)
		}
	default:
		if sl.TickDropCounter != nil {
			sl.TickDropCounter.Add(1)
		}
	}
}

func (sl *SwapListener) maybeFireTickRefetch(p *Pool) {
	if sl.TickRefetchCh == nil || sl.TickEagerRefetchSpacings <= 0 {
		return
	}
	p.mu.RLock()
	spacing := p.TickSpacing
	cur := p.Tick
	last := p.TickAtFetch
	p.mu.RUnlock()
	if spacing <= 0 {
		return
	}
	delta := cur - last
	if delta < 0 {
		delta = -delta
	}
	if delta/spacing < sl.TickEagerRefetchSpacings {
		return
	}
	now := time.Now()
	if sl.TickEagerRefetchCooldown > 0 {
		key := strings.ToLower(p.Address)
		if v, ok := sl.lastTickRefetch.Load(key); ok {
			if t, ok := v.(time.Time); ok && now.Sub(t) < sl.TickEagerRefetchCooldown {
				return
			}
		}
		sl.lastTickRefetch.Store(key, now)
	}
	select {
	case sl.TickRefetchCh <- p:
		if sl.TickEnqueueCounter != nil {
			sl.TickEnqueueCounter.Add(1)
		}
	default:
		if sl.TickDropCounter != nil {
			sl.TickDropCounter.Add(1)
		}
	}
}

// maybeFireAlgebraFeeRefresh sends p to AlgebraFeeRefreshCh when the pool is
// an Algebra-style DEX (CamelotV3, ZyberV3) whose dynamic fee isn't carried in
// the Swap event payload. A background worker drains the channel and calls
// globalState() to refresh the fee. Per-pool cooldown enforced. Non-blocking.
func (sl *SwapListener) maybeFireAlgebraFeeRefresh(p *Pool) {
	if sl.AlgebraFeeRefreshCh == nil {
		return
	}
	if p.DEX != DEXCamelotV3 && p.DEX != DEXZyberV3 {
		return
	}
	now := time.Now()
	if sl.AlgebraFeeRefreshCooldown > 0 {
		key := strings.ToLower(p.Address)
		if v, ok := sl.lastAlgebraFeeRefresh.Load(key); ok {
			if t, ok := v.(time.Time); ok && now.Sub(t) < sl.AlgebraFeeRefreshCooldown {
				return
			}
		}
		sl.lastAlgebraFeeRefresh.Store(key, now)
	}
	select {
	case sl.AlgebraFeeRefreshCh <- p:
	default:
		// Channel full — next swap or periodic refresh will catch up.
	}
}

func (sl *SwapListener) applyV3Swap(p *Pool, data []byte, blockNumber uint64) {
	if len(data) == 0 {
		return
	}
	vals, err := v3SwapABI.Unpack("Swap", data)
	if err != nil || len(vals) < 5 {
		return
	}
	// vals[0]=amount0 (int256), [1]=amount1 (int256),
	// [2]=sqrtPriceX96 (uint160), [3]=liquidity (uint128), [4]=tick (int24)
	sqrtPrice, _ := vals[2].(*big.Int)
	liquidity, _ := vals[3].(*big.Int)
	// go-ethereum decodes int24 as *big.Int, not int32
	var tick int32
	switch v := vals[4].(type) {
	case int32:
		tick = v
	case *big.Int:
		if v.IsInt64() {
			tick = int32(v.Int64())
		}
	}
	p.UpdateFromV3Swap(sqrtPrice, liquidity, tick, blockNumber)
}

func (sl *SwapListener) applyV2Swap(p *Pool, data []byte, blockNumber uint64) {
	if len(data) == 0 {
		return
	}
	vals, err := v2SwapABI.Unpack("Swap", data)
	if err != nil || len(vals) < 4 {
		return
	}
	// vals[0]=amount0In, [1]=amount1In, [2]=amount0Out, [3]=amount1Out
	a0In, _ := vals[0].(*big.Int)
	a1In, _ := vals[1].(*big.Int)
	a0Out, _ := vals[2].(*big.Int)
	a1Out, _ := vals[3].(*big.Int)
	if a0In == nil { a0In = new(big.Int) }
	if a1In == nil { a1In = new(big.Int) }
	if a0Out == nil { a0Out = new(big.Int) }
	if a1Out == nil { a1Out = new(big.Int) }
	p.UpdateFromV2Swap(a0In, a1In, a0Out, a1Out, blockNumber)
}

// applyV4Swap handles UniswapV4 Swap events from the PoolManager.
// V4 uses the same sqrtPrice/liquidity/tick model as V3.
func (sl *SwapListener) applyV4Swap(p *Pool, data []byte, blockNumber uint64) {
	if len(data) == 0 {
		return
	}
	vals, err := v4SwapABI.Unpack("Swap", data)
	if err != nil || len(vals) < 5 {
		return
	}
	// vals[0]=amount0 (int128), [1]=amount1 (int128),
	// [2]=sqrtPriceX96 (uint160), [3]=liquidity (uint128), [4]=tick (int24)
	sqrtPrice, _ := vals[2].(*big.Int)
	liquidity, _ := vals[3].(*big.Int)
	// go-ethereum decodes int24 as *big.Int, not int32
	var tick int32
	switch v := vals[4].(type) {
	case int32:
		tick = v
	case *big.Int:
		if v.IsInt64() {
			tick = int32(v.Int64())
		}
	}
	p.UpdateFromV3Swap(sqrtPrice, liquidity, tick, blockNumber)
}

// applyV3LiquidityEvent decodes a V3 Mint or Burn event and applies the
// signed liquidity delta to the pool's cached tick state.
//
// V3 event layouts (see v3MintBurnABI):
//   Mint: indexed(owner, tickLower, tickUpper); data=(sender, amount uint128, amount0, amount1)
//   Burn: indexed(owner, tickLower, tickUpper); data=(amount uint128, amount0, amount1)
//
// For Mint, isMint=true → delta = +amount.
// For Burn, isMint=false → delta = -amount.
// Signed int24 ticks are stored in the indexed topic slots as 32-byte
// big-endian two's-complement; we sign-extend from the low 3 bytes.
func applyV3LiquidityEvent(p *Pool, topics []common.Hash, data []byte, isMint bool, blockNumber uint64) {
	if len(topics) < 4 {
		return
	}
	tickLower := sign24FromHash(topics[2])
	tickUpper := sign24FromHash(topics[3])
	var amt *big.Int
	if isMint {
		if len(data) < 32*2 {
			return
		}
		amt = new(big.Int).SetBytes(data[32 : 64])
	} else {
		if len(data) < 32 {
			return
		}
		amt = new(big.Int).SetBytes(data[:32])
	}
	if amt.Sign() == 0 {
		return
	}
	if !isMint {
		amt = new(big.Int).Neg(amt)
	}
	p.ApplyTickLiquidityDelta(tickLower, tickUpper, amt, blockNumber)
}

// applyV4ModifyLiquidity decodes a V4 ModifyLiquidity event (non-indexed
// tickLower, tickUpper, liquidityDelta, salt) and applies the signed delta.
//
// V4 layout (see v4ModifyLiquidityABI):
//   indexed(id, sender); data=(tickLower int24, tickUpper int24, liquidityDelta int256, salt bytes32)
// All four data fields are 32-byte-aligned in the log payload.
func applyV4ModifyLiquidity(p *Pool, data []byte, blockNumber uint64) {
	if len(data) < 32*4 {
		return
	}
	tickLower := sign24FromBytes32(data[0:32])
	tickUpper := sign24FromBytes32(data[32:64])
	liqDelta := new(big.Int).SetBytes(data[64:96])
	if data[64]&0x80 != 0 {
		off := new(big.Int).Lsh(big.NewInt(1), 256)
		liqDelta = new(big.Int).Sub(liqDelta, off)
	}
	if liqDelta.Sign() == 0 {
		return
	}
	p.ApplyTickLiquidityDelta(tickLower, tickUpper, liqDelta, blockNumber)
}

// sign24FromHash extracts a signed int24 tick from an indexed-topic hash.
// The int24 is right-aligned in the 32-byte topic; sign-extend from bit 23.
func sign24FromHash(h common.Hash) int32 {
	raw := int32(uint32(h[29])<<16 | uint32(h[30])<<8 | uint32(h[31]))
	if raw&0x800000 != 0 {
		raw |= ^0xFFFFFF
	}
	return raw
}

// sign24FromBytes32 is the non-indexed-payload variant: int24 padded to 32
// bytes in the log's data section (right-aligned, sign-extended in the
// high bytes by the ABI encoder).
func sign24FromBytes32(b []byte) int32 {
	if len(b) < 32 {
		return 0
	}
	raw := int32(uint32(b[29])<<16 | uint32(b[30])<<8 | uint32(b[31]))
	if raw&0x800000 != 0 {
		raw |= ^0xFFFFFF
	}
	return raw
}
