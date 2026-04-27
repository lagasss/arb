---
name: Lean specialised contract fleet — in progress
description: Fleet of narrow executors to compete at sub-$0.05 layer vs competitors at 257k gas; 3 specialised contracts live (V3FlashMini, V4Mini, MixedV3V4Executor), OwnCapitalMini still the main gap
type: project
originSessionId: 3815f914-2e9b-4525-9ac4-3f915b6c5a87
---
Competitor 15191 benchmark (2026-04-19): 2-hop USDC→WETH→USDC PancakeV3+DeltaSwap, **257k gas, own_capital, $0.016 net on $58 notional**. Our old generalist `ArbitrageExecutor` at ~900k gas was locked out of this layer entirely. The fleet approach — one narrow contract per cycle shape, router picks the leanest eligible — is in progress, not a roadmap item.

## Fleet status (as of 2026-04-23)

**Active specialised executors:**
| Address | Name | Shape | Gas (est/measured) | Flash | Status |
|---|---|---|---|---|---|
| `0x33347af4` | V3FlashMini | V3-only 2-5 hop | 375-385k | V3 pool | live |
| `0x615ba27d` | V4Mini | V4-only 2-5 hop, ETH-native, hook-gated | 220-395k | V3 pool | live |
| `0x448494C9` | MixedV3V4Executor | Mixed V3+V4 2-5 hop | — | via PoolManager.unlock | live |
| `0x7bc72cb0` | ArbitrageExecutor (generic) | Multi-hop mixed fallback | ~900k | Balancer/V3/Aave | live (fallback) |

**Still missing (the critical gap):**
- `OwnCapitalMini` (2-3 hop, no flash, ~340k target) — ledger row `not_deployed:own_capital_mini`. **This is the contract that actually closes the 257k-gas competitor layer.** Flash-loan minis don't help at the <$0.05 layer because flash fees (Balancer 0, V3 5-100bps) + flash overhead still wedge margin. The competitor's `own_capital` strategy avoids that entirely.
- `SplitArb` (CEX-DEX, 600k) — CEX-DEX scope, separate from DEX-DEX arb.

## Router
- `contract_ledger.gas_estimate` per contract feeds the profit-gate math.
- `evalOneCandidate` picks executor by cycle shape: V4-only → V4Mini, V3-only → V3FlashMini, V3+V4 → MixedV3V4Executor, anything else → generic.
- No own-capital route yet — every cycle pays a flash wrapper even when we could just use held inventory.

## Why this still matters
HARD pre-live blocker on `pause_trading=false` being *economically meaningful*. Phase-1 equivalent (V3/V4 flash minis) is done and V4_HANDLER reject class is closed, but without OwnCapitalMini we still can't land the sub-$0.05 backruns that dominate competitor activity on thin USDC/WETH cycles.

## How to apply
When planning next contract work: OwnCapitalMini first. 2-hop USDC⇄WETH is the highest-volume shape; fund with ~$500 USDC + $500 WETH, wire `ownCapitalQualify` path into `evalOneCandidate`, add `trading.executor_own_capital_mini` config key, flip `not_deployed:own_capital_mini` → live row in contract_ledger.

Latency work (sequencer feed, tx submit path tuning) should queue *behind* OwnCapitalMini — a 200ms head start is wasted if the gas gate still rejects the cycle.
