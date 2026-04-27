package internal

import (
	"context"
	"log"
	"math/big"
	"strings"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/ethclient"
)

// borrowable.go — Balancer Vault flash-loan eligibility tracking.
//
// The bot uses Balancer V2's flash loans (zero fee on most pools) to fund
// every arbitrage trade. Balancer can only flash-loan tokens that the vault
// actually holds — for any other token, vault.flashLoan(token, amount) reverts
// with BAL#528 (INSUFFICIENT_FLASH_LOAN_BALANCE).
//
// Without filtering, the cycle cache happily generates cycles starting at
// ANY token in the registry, including exotic tokens like EVA, ZRO, #BB that
// Balancer doesn't hold at all. The bot then detects these as profitable,
// scores them, runs the eth_call dry-run, and gets BAL#528 every time —
// burning RPC budget and crowding out the cycles that could actually execute.
// (Confirmed 2026-04-11: 51% of all sim-rejects in a busy hour were BAL#528,
//  every single one for a token with vault balance == 0.)
//
// RefreshBorrowableTokens queries the vault's balanceOf for every registered
// token in a single multicall pass and returns the set of addresses with
// balance > minBalanceWei. The cycle cache uses this set to filter start
// tokens at DFS time, eliminating the entire failure class.

// RefreshBorrowableTokens returns the lowercase addresses of tokens the
// Balancer Vault currently holds at least `minBalanceWei` of. Pass 0 for
// minBalanceWei to include any non-zero balance.
//
// Costs one multicall round-trip to chain (~1-2s for ~1300 tokens). Safe to
// call from a background goroutine; reads are atomic via the registry's lock.
func RefreshBorrowableTokens(
	ctx context.Context,
	client *ethclient.Client,
	vault common.Address,
	tokens *TokenRegistry,
	minBalanceWei *big.Int,
) (map[string]bool, error) {
	if minBalanceWei == nil {
		minBalanceWei = big.NewInt(0)
	}

	all := tokens.All()
	if len(all) == 0 {
		return map[string]bool{}, nil
	}

	// balanceOf(vault) calldata: selector 0x70a08231 + 32-byte padded address
	selector := []byte{0x70, 0xa0, 0x82, 0x31}
	vaultPadded := common.LeftPadBytes(vault.Bytes(), 32)
	balanceOfCalldata := append(selector, vaultPadded...)

	calls := make([]call, len(all))
	for i, t := range all {
		calls[i] = call{
			Target:   common.HexToAddress(t.Address),
			CallData: balanceOfCalldata,
		}
	}

	results, err := doMulticall(ctx, client, calls, nil)
	if err != nil {
		return nil, err
	}

	out := make(map[string]bool, len(all)/4)
	for i, r := range results {
		if !r.Success || len(r.ReturnData) < 32 {
			continue
		}
		bal := new(big.Int).SetBytes(r.ReturnData[:32])
		if bal.Cmp(minBalanceWei) > 0 {
			out[strings.ToLower(all[i].Address)] = true
		}
	}
	return out, nil
}

// borrowableTracker keeps a refreshable copy of the borrowable token set so
// that consumers (the cycle cache, fast-eval, the executor's pre-flight
// check) can read it without each one having to schedule its own refresh.
type borrowableTracker struct {
	mu        sync.RWMutex
	tokens    map[string]bool
	updatedAt time.Time
}

func (b *borrowableTracker) Set(set map[string]bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.tokens = set
	b.updatedAt = time.Now()
}

func (b *borrowableTracker) Has(addr string) bool {
	b.mu.RLock()
	defer b.mu.RUnlock()
	if len(b.tokens) == 0 {
		// Empty / not yet refreshed → fail open: pretend everything is
		// borrowable so the bot doesn't go dark before the first refresh.
		return true
	}
	return b.tokens[strings.ToLower(addr)]
}

func (b *borrowableTracker) Snapshot() map[string]bool {
	b.mu.RLock()
	defer b.mu.RUnlock()
	out := make(map[string]bool, len(b.tokens))
	for k := range b.tokens {
		out[k] = true
	}
	return out
}

// RunBorrowableRefreshLoop refreshes the borrowable token set on a fixed
// interval. The first refresh runs after a short startup delay so the token
// registry has had a chance to rehydrate. Cancellation via ctx is honored.
func RunBorrowableRefreshLoop(
	ctx context.Context,
	client *ethclient.Client,
	vault common.Address,
	tokens *TokenRegistry,
	tracker *borrowableTracker,
	minBalanceWei *big.Int,
	interval time.Duration,
	onUpdate func(set map[string]bool),
) {
	// Initial delay so subgraph + LoadPools have populated the token registry.
	select {
	case <-ctx.Done():
		return
	case <-time.After(15 * time.Second):
	}

	for {
		t0 := time.Now()
		set, err := RefreshBorrowableTokens(ctx, client, vault, tokens, minBalanceWei)
		if err != nil {
			log.Printf("[borrowable] refresh failed: %v", err)
		} else {
			tracker.Set(set)
			log.Printf("[borrowable] refreshed: %d/%d tokens flash-loanable from Balancer (took %s)",
				len(set), len(tokens.All()), time.Since(t0).Round(time.Millisecond))
			if onUpdate != nil {
				onUpdate(set)
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(interval):
		}
	}
}
