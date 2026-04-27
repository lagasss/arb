package internal

import (
	"context"
	"math/big"
	"strings"
	"time"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/ethclient"
)

// ResolvePoolFromChain takes a pool address and queries the chain to identify
// the DEX (via factory()), tokens (via token0/token1), and fee (via fee() for V3
// or by calibrating against getAmountOut for V2).
//
// It is the canonical "given an address, give me a fully-typed Pool" helper —
// used by:
//   - compwatcher (when a competitor uses a pool we don't know about)
//   - bot startup (when a `pinned_pools:` address isn't yet in the registry)
//   - any future tooling that needs to add a pool by address
//
// Tokens are looked up in the registry first; missing tokens are fetched
// on-chain via FetchTokenMeta and added to the registry.
//
// Returns nil if the pool can't be resolved (unknown factory, RPC error, etc.).
func ResolvePoolFromChain(ctx context.Context, client *ethclient.Client, registry *PoolRegistry, tokens *TokenRegistry, addr string) *Pool {
	addr = strings.ToLower(addr)
	tctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()

	a := common.HexToAddress(addr)

	callAddr := func(name string) string {
		d, err := poolQueryABI.Pack(name)
		if err != nil {
			return ""
		}
		res, err := client.CallContract(tctx, ethereum.CallMsg{To: &a, Data: d}, nil)
		if err != nil || len(res) < 32 {
			return ""
		}
		vals, err := poolQueryABI.Unpack(name, res)
		if err != nil || len(vals) == 0 {
			return ""
		}
		if v, ok := vals[0].(common.Address); ok {
			return strings.ToLower(v.Hex())
		}
		return ""
	}

	factoryLo := callAddr("factory")
	if factoryLo == "" {
		return nil
	}
	dexType, known := FactoryAddresses[factoryLo]
	if !known {
		return nil
	}

	t0addr := callAddr("token0")
	t1addr := callAddr("token1")
	if t0addr == "" || t1addr == "" {
		return nil
	}

	// V3 fee — fee() returns uint24 in ppm (e.g. 500 = 5 bps)
	var feeBps uint32
	var feePPM uint32
	if d, err := poolQueryABI.Pack("fee"); err == nil {
		if res, err := client.CallContract(tctx, ethereum.CallMsg{To: &a, Data: d}, nil); err == nil && len(res) >= 32 {
			if vals, err := poolQueryABI.Unpack("fee", res); err == nil && len(vals) > 0 {
				switch v := vals[0].(type) {
				case uint32:
					feePPM = v
					feeBps = v / 100
				case *big.Int:
					if v.IsInt64() {
						feePPM = uint32(v.Int64())
						feeBps = feePPM / 100
					}
				}
			}
		}
	}

	// V2 pools: derive from getReserves + getAmountOut
	if feeBps == 0 && feePPM == 0 {
		feeBps = calibrateV2FeeOnChain(tctx, client, a, t0addr)
	}

	t0 := resolveTokenViaRegistry(ctx, client, tokens, t0addr)
	t1 := resolveTokenViaRegistry(ctx, client, tokens, t1addr)

	p := &Pool{
		Address:      addr,
		DEX:          dexType,
		FeeBps:       feeBps,
		FeePPM:       feePPM,
		Token0:       t0,
		Token1:       t1,
		TVLUSD:       0,
		Volume24hUSD: 0,
	}

	// Run DEX-specific verification: fetches missing fields (TickSpacing,
	// sqrtPriceX96, etc.) and validates fee/state against the chain. Pools
	// that fail are still returned but marked Verified=false so the cycle
	// cache can exclude them until they pass.
	ok, reason := VerifyPool(ctx, client, p)
	p.Verified = ok
	p.VerifyReason = reason
	// Router reachability: VerifyV2RouterReachable is available as a library
	// function for pre-deployment validation of NEW V2-fork DEX types before
	// they're added to internal.Factories. It is intentionally NOT called
	// here — existing DEX types have been load-bearing for months and a
	// behavioral change at the resolver would mass-invalidate pools on
	// restart. New DEX types must be validated out-of-band (via the
	// cmd/smoketest binary or an ad-hoc tool) before wire-up.
	return p
}

// resolveTokenViaRegistry returns the token from the registry, fetching meta
// from chain and adding it if missing.
func resolveTokenViaRegistry(ctx context.Context, client *ethclient.Client, tokens *TokenRegistry, addr string) *Token {
	if t, ok := tokens.Get(addr); ok {
		return t
	}
	tctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	t := FetchTokenMeta(tctx, client, addr)
	tokens.Add(t)
	return t
}

// calibrateV2FeeOnChain derives the effective swap fee for a V2-style pool.
// Same algorithm as compwatcher.calibrateV2Fee but as a standalone function.
func calibrateV2FeeOnChain(ctx context.Context, client *ethclient.Client, poolAddr common.Address, token0Addr string) uint32 {
	// getReserves() selector: 0x0902f1ac
	resData := []byte{0x09, 0x02, 0xf1, 0xac}
	res, err := client.CallContract(ctx, ethereum.CallMsg{To: &poolAddr, Data: resData}, nil)
	if err != nil || len(res) < 64 {
		return 0
	}
	r0 := new(big.Int).SetBytes(res[0:32])
	r1 := new(big.Int).SetBytes(res[32:64])
	if r0.Sign() == 0 || r1.Sign() == 0 {
		return 0
	}

	testAmt := new(big.Int).Div(r0, big.NewInt(1000))
	if testAmt.Sign() == 0 {
		testAmt = big.NewInt(1)
	}

	// getAmountOut(uint256, address) selector: 0xf140a35a
	gaoData := make([]byte, 4+64)
	copy(gaoData[:4], []byte{0xf1, 0x40, 0xa3, 0x5a})
	testAmt.FillBytes(gaoData[4:36])
	copy(gaoData[36+12:68], common.HexToAddress(token0Addr).Bytes())

	gaoRes, err := client.CallContract(ctx, ethereum.CallMsg{To: &poolAddr, Data: gaoData}, nil)
	if err != nil || len(gaoRes) < 32 {
		return 30 // default
	}
	actualOut := new(big.Int).SetBytes(gaoRes[:32])
	if actualOut.Sign() == 0 {
		return 30
	}

	num := new(big.Int).Mul(testAmt, r1)
	denom := new(big.Int).Add(r0, testAmt)
	rawOut := new(big.Int).Div(num, denom)
	if rawOut.Sign() == 0 || rawOut.Cmp(actualOut) <= 0 {
		return 1 // stable-swap-like
	}

	diff := new(big.Int).Sub(rawOut, actualOut)
	feeBps := new(big.Int).Mul(diff, big.NewInt(10000))
	feeBps.Div(feeBps, rawOut)
	if feeBps.IsUint64() && feeBps.Uint64() > 0 && feeBps.Uint64() < 1000 {
		return uint32(feeBps.Uint64())
	}
	return 30
}
