package internal

import (
	"context"
	"log"
	"math/big"
	"strings"
	"sync"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/ethclient"
)

// FlashSource identifies which flash loan provider to use for a cycle.
type FlashSource uint8

const (
	FlashBalancer FlashSource = iota // Zero fee — always preferred
	FlashV3Pool                      // V3 pool flash — costs pool's fee tier (5-100 bps)
	FlashAave                        // Aave V3 — costs FLASHLOAN_PREMIUM_TOTAL (5 bps on Arbitrum)
)

func (f FlashSource) String() string {
	switch f {
	case FlashBalancer:
		return "Balancer"
	case FlashV3Pool:
		return "V3Flash"
	case FlashAave:
		return "Aave"
	default:
		return "Unknown"
	}
}

// FlashSelection is the result of picking the best flash loan source for a
// borrow token. It tells the executor which entry point to call and what fee
// to account for in the profit calculation.
type FlashSelection struct {
	Source    FlashSource
	FeePPM    uint32         // Fee in parts-per-million (0 for Balancer, pool fee for V3, 500 for Aave)
	PoolAddr  common.Address // For FlashV3Pool: the V3 pool to call flash() on. Zero for others.
	// FlashPoolToken0: For FlashV3Pool only. The token0 address of the flash
	// pool, used by V3FlashMini to determine the `isToken0` flag when
	// invoking pool.flash() — the mini contract needs to know whether the
	// borrow token sits on the token0 or token1 side to pick the right
	// amount0/amount1 argument. Zero for non-V3 sources.
	FlashPoolToken0 common.Address
	Available       bool // False if no flash source found for this token.
}

// FlashSourceSelector picks the cheapest flash loan source for a given token.
// Priority: Balancer (0 fee) > V3 pool (lowest fee tier) > Aave (5 bps).
type FlashSourceSelector struct {
	mu sync.RWMutex

	// Balancer borrowable set — tokens the Balancer Vault holds.
	balancerBorrowable map[string]bool

	// V3 flash pools — for each token, the cheapest V3 pool (lowest fee tier)
	// that has enough liquidity. Maps lowercase token address → (pool address, fee ppm).
	v3FlashPools map[string]v3FlashEntry

	// Aave reserves — tokens that Aave V3 can lend. Maps lowercase token address → true.
	aaveReserves map[string]bool

	// Aave premium in ppm (typically 50 = 0.05% = 5 bps on Arbitrum).
	aavePremiumPPM uint32
}

type v3FlashEntry struct {
	PoolAddr common.Address
	FeePPM   uint32
	// Token0 of the flash pool. Recorded at refresh time so the Select
	// path can return it alongside PoolAddr without a pool-registry lookup.
	Token0 common.Address
}

func NewFlashSourceSelector() *FlashSourceSelector {
	return &FlashSourceSelector{
		balancerBorrowable: make(map[string]bool),
		v3FlashPools:       make(map[string]v3FlashEntry),
		aaveReserves:       make(map[string]bool),
		aavePremiumPPM:     50, // 0.05% default on Arbitrum
	}
}

// Select returns the best flash source for borrowing the given token.
func (s *FlashSourceSelector) Select(tokenAddr string) FlashSelection {
	key := strings.ToLower(tokenAddr)
	s.mu.RLock()
	defer s.mu.RUnlock()

	// Priority 1: Balancer (zero fee)
	if s.balancerBorrowable[key] {
		return FlashSelection{Source: FlashBalancer, FeePPM: 0, Available: true}
	}

	// Priority 2: V3 pool flash (cheapest fee tier)
	if entry, ok := s.v3FlashPools[key]; ok {
		return FlashSelection{
			Source:          FlashV3Pool,
			FeePPM:          entry.FeePPM,
			PoolAddr:        entry.PoolAddr,
			FlashPoolToken0: entry.Token0,
			Available:       true,
		}
	}

	// Priority 3: Aave (fixed premium)
	if s.aaveReserves[key] {
		return FlashSelection{Source: FlashAave, FeePPM: s.aavePremiumPPM, Available: true}
	}

	return FlashSelection{Available: false}
}

// SetBalancerBorrowable updates the Balancer borrowable set.
func (s *FlashSourceSelector) SetBalancerBorrowable(tokens map[string]bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.balancerBorrowable = tokens
}

// BalancerBorrowableList returns the current Balancer borrowable tokens as a
// slice. Used by the vault cache refresher goroutine to periodically re-fetch
// balances for all borrowable tokens.
func (s *FlashSourceSelector) BalancerBorrowableList() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]string, 0, len(s.balancerBorrowable))
	for addr := range s.balancerBorrowable {
		out = append(out, addr)
	}
	return out
}

// SetV3FlashPools updates the V3 flash pool map.
func (s *FlashSourceSelector) SetV3FlashPools(pools map[string]v3FlashEntry) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.v3FlashPools = pools
}

// SetAaveReserves updates the Aave reserve set.
func (s *FlashSourceSelector) SetAaveReserves(tokens map[string]bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.aaveReserves = tokens
}

// FlashFeeBps returns the flash fee in basis points for display/logging.
func (sel FlashSelection) FlashFeeBps() float64 {
	return float64(sel.FeePPM) / 100.0
}

// RefreshV3FlashPools scans the pool registry for V3 pools and builds a map of
// token → cheapest flash pool. For each token that appears as token0 or token1
// in any V3 pool with sufficient liquidity, we keep the pool with the lowest
// fee tier (cheapest flash). Called periodically (e.g. every hour alongside
// the borrowable refresh).
func RefreshV3FlashPools(registry *PoolRegistry) map[string]v3FlashEntry {
	pools := registry.All()
	best := make(map[string]v3FlashEntry)
	for _, p := range pools {
		switch p.DEX {
		case DEXUniswapV3, DEXSushiSwapV3, DEXPancakeV3:
			// Only consider pools with populated liquidity
			if p.Liquidity == nil || p.Liquidity.Sign() == 0 {
				continue
			}
			fee := p.FeePPM
			if fee == 0 {
				fee = p.FeeBps * 100
			}
			addr := common.HexToAddress(p.Address)
			token0 := common.HexToAddress(p.Token0.Address)
			for _, tok := range []string{
				strings.ToLower(p.Token0.Address),
				strings.ToLower(p.Token1.Address),
			} {
				if existing, ok := best[tok]; !ok || fee < existing.FeePPM {
					best[tok] = v3FlashEntry{PoolAddr: addr, FeePPM: fee, Token0: token0}
				}
			}
		}
	}
	return best
}

// RefreshAaveReserves queries the Aave V3 Pool for active reserves.
// On Arbitrum, this is typically 20-30 major tokens (WETH, USDC, USDT, etc.).
// Uses the Aave V3 Pool.getReservesList() view function.
func RefreshAaveReserves(ctx context.Context, client *ethclient.Client, aavePoolAddr common.Address) map[string]bool {
	if aavePoolAddr == (common.Address{}) {
		return nil
	}
	// getReservesList() selector = 0xd1946dbc
	sel := common.FromHex("0xd1946dbc")
	result, err := client.CallContract(ctx, callMsg(aavePoolAddr, sel), nil)
	if err != nil || len(result) < 64 {
		log.Printf("[flash] aave getReservesList failed: %v", err)
		return nil
	}
	// ABI decode: returns address[]
	// Offset at bytes 0-32, length at offset, then addresses
	if len(result) < 64 {
		return nil
	}
	offset := new(big.Int).SetBytes(result[0:32]).Uint64()
	if offset+32 > uint64(len(result)) {
		return nil
	}
	length := new(big.Int).SetBytes(result[offset : offset+32]).Uint64()
	reserves := make(map[string]bool, length)
	for i := uint64(0); i < length; i++ {
		start := offset + 32 + i*32
		if start+32 > uint64(len(result)) {
			break
		}
		addr := common.BytesToAddress(result[start : start+32])
		reserves[strings.ToLower(addr.Hex())] = true
	}
	log.Printf("[flash] aave reserves: %d tokens", len(reserves))
	return reserves
}

func callMsg(to common.Address, data []byte) ethereum.CallMsg {
	return ethereum.CallMsg{To: &to, Data: data}
}
