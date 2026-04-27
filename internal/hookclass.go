package internal

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"math/big"
	"strings"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/ethclient"
)

// V4 encodes hook permission flags in the bottom 14 bits of the hook contract
// address. See https://docs.uniswap.org/contracts/v4/concepts/hooks — hooks
// are forced to deploy to an address whose low bits match the permissions
// they request. We read those bits directly instead of invoking the hook.
const (
	HookPermBeforeInitialize         = 1 << 13
	HookPermAfterInitialize          = 1 << 12
	HookPermBeforeAddLiquidity       = 1 << 11
	HookPermAfterAddLiquidity        = 1 << 10
	HookPermBeforeRemoveLiquidity    = 1 << 9
	HookPermAfterRemoveLiquidity     = 1 << 8
	HookPermBeforeSwap               = 1 << 7
	HookPermAfterSwap                = 1 << 6
	HookPermBeforeDonate             = 1 << 5
	HookPermAfterDonate              = 1 << 4
	HookPermBeforeSwapReturnDelta    = 1 << 3
	HookPermAfterSwapReturnDelta     = 1 << 2
	HookPermAfterAddLiquidityReturnDelta    = 1 << 1
	HookPermAfterRemoveLiquidityReturnDelta = 1 << 0
)

// HookClassification labels how safely we can route a V4 pool whose PoolKey
// carries the given hook address through our lean fleet (V4Mini /
// MixedV3V4Executor).
type HookClassification string

const (
	// HookSafe — hook has no delta-rewriting permissions at all. It can
	// observe swaps (beforeSwap/afterSwap) and liquidity events but cannot
	// modify the amounts the PoolManager credits/debits. Auto-whitelistable.
	HookSafe HookClassification = "safe"

	// HookFeeOnly — hook uses beforeSwapReturnDelta BUT the runtime
	// bytecode matches a known benign fee pattern (dynamic-fee / LP-fee
	// rebate). Requires a manual source-review step before promotion, but
	// the classifier flags it as a promotion candidate instead of
	// auto-rejecting.
	HookFeeOnly HookClassification = "fee_only"

	// HookDeltaRewriting — hook carries afterSwapReturnDelta and/or
	// beforeSwapReturnDelta bits. Cannot be auto-whitelisted because a
	// custom-accounting hook can route funds out of our unlock callback.
	// Only promotable via explicit human approveDeltaHook.
	HookDeltaRewriting HookClassification = "delta_rewriting"

	// HookUnknown — RPC or parse failure. Treated as unsafe by the shape
	// filter until a subsequent pass re-classifies.
	HookUnknown HookClassification = "unknown"
)

// HookReport is the structured output of ClassifyHook — persisted verbatim
// into hook_registry and surfaced by the /debug/hook-registry endpoint.
type HookReport struct {
	Address        common.Address
	PermissionBits uint16
	HasDeltaFlag   bool
	Classification HookClassification
	BytecodeHash   string
	Reason         string
}

// permissionBitsOf extracts the bottom 14 bits of the hook address, which V4
// defines as the permission-flag encoding.
func permissionBitsOf(addr common.Address) uint16 {
	return uint16(new(big.Int).SetBytes(addr.Bytes()).Uint64() & 0x3FFF)
}

// hasDeltaFlag returns true if the hook has any `*ReturnDelta` bit set.
// Those bits mean the hook can rewrite swap accounting; they are the
// dividing line between auto-whitelistable and manual-review-required.
func hasDeltaFlag(bits uint16) bool {
	return bits&(HookPermBeforeSwapReturnDelta|
		HookPermAfterSwapReturnDelta|
		HookPermAfterAddLiquidityReturnDelta|
		HookPermAfterRemoveLiquidityReturnDelta) != 0
}

// ClassifyHook runs the offline classifier on a hook address and returns a
// HookReport. RPC is used to fetch runtime bytecode so the same hash can be
// re-verified by a human reviewer. If the bytecode fetch fails the
// classifier still returns a result (HookUnknown).
func ClassifyHook(ctx context.Context, client *ethclient.Client, addr common.Address) HookReport {
	bits := permissionBitsOf(addr)
	delta := hasDeltaFlag(bits)

	rep := HookReport{
		Address:        addr,
		PermissionBits: bits,
		HasDeltaFlag:   delta,
	}

	code, err := client.CodeAt(ctx, addr, nil)
	if err != nil || len(code) == 0 {
		rep.Classification = HookUnknown
		rep.Reason = fmt.Sprintf("bytecode fetch failed or EOA (len=%d, err=%v)", len(code), err)
		return rep
	}
	sum := sha256.Sum256(code)
	rep.BytecodeHash = "0x" + hex.EncodeToString(sum[:])

	switch {
	case !delta:
		rep.Classification = HookSafe
		rep.Reason = fmt.Sprintf("no delta-rewriting bits (perms=0x%04x)", bits)
	case delta && bits&HookPermAfterSwapReturnDelta == 0:
		rep.Classification = HookFeeOnly
		rep.Reason = fmt.Sprintf("beforeSwapReturnDelta only — candidate fee hook (perms=0x%04x)", bits)
	default:
		rep.Classification = HookDeltaRewriting
		rep.Reason = fmt.Sprintf("afterSwapReturnDelta set — requires manual approveDeltaHook (perms=0x%04x)", bits)
	}
	return rep
}

// IsAutoWhitelistable returns true when the classifier is confident enough
// to push the hook on-chain via setHook(allow=true) without human review.
// Currently: only HookSafe. fee_only and delta_rewriting need human review.
func (r HookReport) IsAutoWhitelistable() bool {
	return r.Classification == HookSafe && !r.HasDeltaFlag
}

// UpsertHookClassification writes a HookReport into the hook_registry table.
// Runs on every classify pass; idempotent — existing rows are updated in
// place so the pushed_at / classification columns stay current.
func (d *DB) UpsertHookClassification(rep HookReport, onChainStatus string) error {
	now := time.Now().Unix()
	_, err := d.db.Exec(`
		INSERT INTO hook_registry
		  (address, permission_bits, has_delta_flag, classification,
		   bytecode_hash, on_chain_status, reviewer_note,
		   classified_at, pushed_at, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, 0, ?, ?)
		ON CONFLICT(address) DO UPDATE SET
		  permission_bits = excluded.permission_bits,
		  has_delta_flag  = excluded.has_delta_flag,
		  classification  = excluded.classification,
		  bytecode_hash   = excluded.bytecode_hash,
		  on_chain_status = CASE WHEN hook_registry.on_chain_status='manual' THEN hook_registry.on_chain_status ELSE excluded.on_chain_status END,
		  classified_at   = excluded.classified_at,
		  updated_at      = excluded.updated_at
	`,
		strings.ToLower(rep.Address.Hex()),
		rep.PermissionBits,
		hookDeltaBit(rep.HasDeltaFlag),
		string(rep.Classification),
		rep.BytecodeHash,
		onChainStatus,
		rep.Reason,
		now, now, now,
	)
	return err
}

// MarkHookPushed stamps pushed_at when the on-chain setHook tx confirms.
func (d *DB) MarkHookPushed(addr common.Address, status string) error {
	now := time.Now().Unix()
	_, err := d.db.Exec(
		`UPDATE hook_registry SET on_chain_status=?, pushed_at=?, updated_at=? WHERE address=?`,
		status, now, now, strings.ToLower(addr.Hex()),
	)
	return err
}

// LoadHookRegistry returns every hook_registry row — used by the shape
// filter at cycle-scoring time. The map is keyed by lowercase hex address.
func (d *DB) LoadHookRegistry() (map[string]HookRegistryRow, error) {
	rows, err := d.db.Query(`SELECT address, classification, on_chain_status FROM hook_registry`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make(map[string]HookRegistryRow)
	for rows.Next() {
		var r HookRegistryRow
		if err := rows.Scan(&r.Address, &r.Classification, &r.OnChainStatus); err != nil {
			return nil, err
		}
		out[strings.ToLower(r.Address)] = r
	}
	return out, nil
}

type HookRegistryRow struct {
	Address        string
	Classification string
	OnChainStatus  string
}

func hookDeltaBit(b bool) int {
	if b {
		return 1
	}
	return 0
}
