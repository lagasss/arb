package internal

import (
	"context"
	"crypto/ecdsa"
	"fmt"
	"log"
	"math/big"
	"strings"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/accounts/abi/bind"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethclient"
)

var hookRegistryABI abi.ABI

func init() {
	parsed, err := abi.JSON(strings.NewReader(`[
		{"name":"isAllowed","type":"function","stateMutability":"view","inputs":[{"type":"address"}],"outputs":[{"type":"bool"}]},
		{"name":"statusOf","type":"function","stateMutability":"view","inputs":[{"type":"address"}],"outputs":[{"type":"uint8"}]},
		{"name":"setHook","type":"function","stateMutability":"nonpayable","inputs":[{"type":"address"},{"type":"bool"},{"type":"uint16"},{"type":"bytes32"},{"type":"string"}],"outputs":[]},
		{"name":"approveDeltaHook","type":"function","stateMutability":"nonpayable","inputs":[{"type":"address"},{"type":"uint16"},{"type":"bytes32"},{"type":"string"}],"outputs":[]}
	]`))
	if err != nil {
		panic(fmt.Sprintf("hookRegistryABI parse: %v", err))
	}
	hookRegistryABI = parsed
}

// HookSync classifies every non-zero V4 hook seen in the live pool registry,
// persists the classification to hook_registry, and pushes auto-whitelistable
// (safe) hooks on-chain via HookRegistry.setHook. Runs on a ticker; cheap per
// pass because CodeAt is cached and already-pushed hooks are skipped.
type HookSync struct {
	db       *DB
	client   *ethclient.Client
	registry common.Address
	privKey  *ecdsa.PrivateKey
	from     common.Address
	chainID  *big.Int
	interval time.Duration

	mu       sync.RWMutex
	cache    map[string]HookReport
	onChain  map[string]bool
}

func NewHookSync(db *DB, client *ethclient.Client, registry common.Address, privKey *ecdsa.PrivateKey, chainID *big.Int, interval time.Duration) *HookSync {
	var from common.Address
	if privKey != nil {
		pub := privKey.Public().(*ecdsa.PublicKey)
		from = crypto.PubkeyToAddress(*pub)
	}
	return &HookSync{
		db:       db,
		client:   client,
		registry: registry,
		privKey:  privKey,
		from:     from,
		chainID:  chainID,
		interval: interval,
		cache:    make(map[string]HookReport),
		onChain:  make(map[string]bool),
	}
}

// Run blocks until ctx is cancelled. Safe to call in a goroutine from Bot.Run.
func (h *HookSync) Run(ctx context.Context, hooksFn func() []common.Address) {
	if h == nil || h.interval <= 0 {
		return
	}
	warmup := time.NewTicker(15 * time.Second)
	bootstrapped := false
	t := time.NewTicker(h.interval)
	defer t.Stop()
	defer warmup.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-warmup.C:
			if bootstrapped {
				continue
			}
			if n := len(hooksFn()); n == 0 {
				continue
			}
			h.pass(ctx, hooksFn)
			bootstrapped = true
			warmup.Stop()
		case <-t.C:
			h.pass(ctx, hooksFn)
			bootstrapped = true
		}
	}
}

func (h *HookSync) pass(ctx context.Context, hooksFn func() []common.Address) {
	raw := hooksFn()
	seen := make(map[string]common.Address, len(raw))
	for _, a := range raw {
		if a == (common.Address{}) {
			continue
		}
		key := strings.ToLower(a.Hex())
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = a
	}
	hooks := make([]common.Address, 0, len(seen))
	for _, a := range seen {
		hooks = append(hooks, a)
	}
	if len(hooks) == 0 {
		return
	}

	var (
		scanned, newOrChanged, pushed int
		pushErrs                       []string
	)
	for _, hk := range hooks {
		scanned++
		rep := ClassifyHook(ctx, h.client, hk)

		h.mu.Lock()
		prev, had := h.cache[strings.ToLower(hk.Hex())]
		h.cache[strings.ToLower(hk.Hex())] = rep
		h.mu.Unlock()

		if !had || prev.Classification != rep.Classification || prev.BytecodeHash != rep.BytecodeHash {
			newOrChanged++
		}

		onChainStatus := "pending"
		if h.registry == (common.Address{}) || h.privKey == nil {
			_ = h.db.UpsertHookClassification(rep, onChainStatus)
			continue
		}

		if rep.IsAutoWhitelistable() {
			alreadyAllowed, err := h.isOnChainAllowed(ctx, hk)
			if err == nil && alreadyAllowed {
				onChainStatus = "allowed"
				_ = h.db.UpsertHookClassification(rep, onChainStatus)
				continue
			}
			if err := h.pushHook(ctx, hk, true, rep.PermissionBits, rep.BytecodeHash, string(rep.Classification)); err != nil {
				pushErrs = append(pushErrs, fmt.Sprintf("%s: %v", hk.Hex(), err))
				_ = h.db.UpsertHookClassification(rep, "pending")
				continue
			}
			pushed++
			onChainStatus = "allowed"
			_ = h.db.UpsertHookClassification(rep, onChainStatus)
			_ = h.db.MarkHookPushed(hk, onChainStatus)
		} else {
			_ = h.db.UpsertHookClassification(rep, "pending")
		}
	}

	if newOrChanged > 0 || pushed > 0 || len(pushErrs) > 0 {
		log.Printf("[hooksync] pass: scanned=%d new_or_changed=%d pushed=%d push_errs=%d",
			scanned, newOrChanged, pushed, len(pushErrs))
		if len(pushErrs) > 0 && len(pushErrs) <= 5 {
			for _, e := range pushErrs {
				log.Printf("[hooksync] push err: %s", e)
			}
		}
	}
}


func (h *HookSync) isOnChainAllowed(ctx context.Context, hook common.Address) (bool, error) {
	data, err := hookRegistryABI.Pack("isAllowed", hook)
	if err != nil {
		return false, err
	}
	to := h.registry
	res, err := h.client.CallContract(ctx, ethereum.CallMsg{From: h.from, To: &to, Data: data}, nil)
	if err != nil {
		return false, err
	}
	if len(res) < 32 {
		return false, fmt.Errorf("short response (%d)", len(res))
	}
	return res[31] != 0, nil
}

func (h *HookSync) pushHook(ctx context.Context, hook common.Address, allow bool, perms uint16, codeHashHex, label string) error {
	codeHash := [32]byte{}
	clean := strings.TrimPrefix(codeHashHex, "0x")
	if len(clean) == 64 {
		b := common.FromHex(codeHashHex)
		if len(b) == 32 {
			copy(codeHash[:], b)
		}
	}
	data, err := hookRegistryABI.Pack("setHook", hook, allow, perms, codeHash, label)
	if err != nil {
		return err
	}

	nonce, err := h.client.PendingNonceAt(ctx, h.from)
	if err != nil {
		return fmt.Errorf("nonce: %w", err)
	}
	gasTip, err := h.client.SuggestGasTipCap(ctx)
	if err != nil {
		return fmt.Errorf("tip: %w", err)
	}
	head, err := h.client.HeaderByNumber(ctx, nil)
	if err != nil {
		return fmt.Errorf("head: %w", err)
	}
	feeCap := new(big.Int).Add(new(big.Int).Mul(head.BaseFee, big.NewInt(2)), gasTip)

	tx := types.NewTx(&types.DynamicFeeTx{
		ChainID:   h.chainID,
		Nonce:     nonce,
		GasTipCap: gasTip,
		GasFeeCap: feeCap,
		Gas:       250_000,
		To:        &h.registry,
		Data:      data,
	})
	signed, err := types.SignTx(tx, types.NewLondonSigner(h.chainID), h.privKey)
	if err != nil {
		return fmt.Errorf("sign: %w", err)
	}
	if err := h.client.SendTransaction(ctx, signed); err != nil {
		return fmt.Errorf("send: %w", err)
	}
	timeoutCtx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	rcpt, err := bind.WaitMined(timeoutCtx, h.client, signed)
	if err != nil {
		return fmt.Errorf("wait: %w", err)
	}
	if rcpt.Status != 1 {
		return fmt.Errorf("tx reverted (hash=%s)", signed.Hash().Hex())
	}
	return nil
}

// IsAllowed returns the cached classifier verdict. Used by the shape filter
// at scoring time — avoids an RPC round-trip per pool per candidate.
func (h *HookSync) IsAllowed(hook string) bool {
	if h == nil {
		return true
	}
	hk := strings.ToLower(hook)
	if hk == "" || hk == "0x0000000000000000000000000000000000000000" {
		return true
	}
	h.mu.RLock()
	defer h.mu.RUnlock()
	rep, ok := h.cache[hk]
	if !ok {
		return false
	}
	return rep.Classification == HookSafe
}

// Snapshot returns a copy of the current classifier cache for /debug export.
func (h *HookSync) Snapshot() map[string]HookReport {
	h.mu.RLock()
	defer h.mu.RUnlock()
	out := make(map[string]HookReport, len(h.cache))
	for k, v := range h.cache {
		out[k] = v
	}
	return out
}
