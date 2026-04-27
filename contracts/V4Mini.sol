// SPDX-License-Identifier: MIT
pragma solidity ^0.8.24;

// V4Mini — minimal Uniswap-V4 multi-hop arbitrage executor.
//
// SCOPE
// -----
// Cycles that:
//   (a) borrow starting capital via a Uniswap-V3-style pool's flash() call
//       (V4 has no native flash; the V3 flash callback opens the V4 cycle)
//   (b) route ALL hops through Uniswap-V4 via the singleton PoolManager
//   (c) have between 2 and 5 V4 hops
//   (d) optionally include native ETH (currency = address(0)) at any boundary
//
// WHY IT EXISTS
// -------------
// The generic ArbitrageExecutor opens a separate `unlock()` per V4 hop, can't
// touch native-ETH pools (forces WETH), and pays ~70k gas of dispatch
// overhead per V4 hop. At sub-2-bp arb margins this overhead alone exceeds
// the entire profit. V4Mini collapses every V4 hop into ONE `unlock()`, nets
// all currency deltas at the end with one settle/take per currency, and
// branches on `currency == 0` so native-ETH legs work without WETH wrapping.
//
// Gas envelope (target):
//   2-hop V4    ~220k
//   3-hop V4    ~280k
//   4-hop V4    ~340k
//   5-hop V4    ~395k
// vs ~700k–1.2M for the generic executor's V4 path.
//
// SAFETY MODEL
// ------------
// - Owner-only entry (`flash`).
// - Hooks: refused unless their address is in the `allowedHooks` whitelist
//   set by the owner. Initial whitelist is empty → only zero-hooks pools
//   work. Add specific hook contracts as they're proven safe.
// - Native ETH: `pm.settle{value:x}()` for ETH-in legs, `pm.take(0x0,...)`
//   for ETH-out legs. WETH is treated as a normal ERC20.
// - Re-entrancy: not needed — only the PoolManager and the V3 flash pool
//   should ever call us back, and both callbacks validate `msg.sender`.
//
// CALLDATA FORMAT (hops)
// ----------------------
// Tightly packed bytes, 67 bytes per hop:
//   [  0: 20] currency0 (the lower-address currency in this pool's PoolKey;
//             0x0 = native ETH)
//   [ 20: 40] currency1 (the higher-address currency)
//   [ 40: 41] flags byte:
//                bit 0 = zeroForOne (1 if tokenIn is currency0)
//                bit 1-7 reserved
//   [ 41: 44] fee (uint24, big-endian — the V4 PoolKey fee tier in pips)
//   [ 44: 47] tickSpacing (int24, big-endian, two's-complement)
//   [ 47: 67] hooks (address; 0x0 if none)
//
// The bot must encode the hops in CYCLE ORDER and pre-compute zeroForOne
// per hop — V4Mini does no token-introspection. The first hop's tokenIn
// is the borrowToken; each hop's tokenOut becomes the next hop's tokenIn.

interface IERC20 {
    function transfer(address to, uint256 amount) external returns (bool);
    function balanceOf(address account) external view returns (uint256);
    function approve(address spender, uint256 amount) external returns (bool);
}

interface IWETH9 is IERC20 {
    function deposit() external payable;
    function withdraw(uint256) external;
}

interface IUniswapV3PoolFlash {
    function flash(address recipient, uint256 amount0, uint256 amount1, bytes calldata data) external;
    function token0() external view returns (address);
    function token1() external view returns (address);
}

interface IPoolManager {
    struct PoolKey {
        address currency0;
        address currency1;
        uint24 fee;
        int24 tickSpacing;
        address hooks;
    }
    struct SwapParams {
        bool zeroForOne;
        int256 amountSpecified;
        uint160 sqrtPriceLimitX96;
    }
    function unlock(bytes calldata data) external returns (bytes memory);
    function swap(PoolKey calldata key, SwapParams calldata params, bytes calldata hookData) external returns (int256);
    function sync(address currency) external;
    function settle() external payable returns (uint256);
    function take(address currency, address to, uint256 amount) external;
}

interface IHookRegistry {
    function isAllowed(address hook) external view returns (bool);
}

contract V4Mini {
    address public immutable owner;
    IPoolManager public immutable poolManager;
    IWETH9 public immutable weth;

    // Hook whitelist. Delegated to an external HookRegistry when set (non-zero
    // hookRegistry), so the V4Mini + MixedV3V4Executor + future executors
    // share one source of truth. Falls back to the local `allowedHooks` map
    // when hookRegistry is address(0) — kept for deploy ordering (registry
    // can be wired after deploy via setHookRegistry).
    mapping(address => bool) public allowedHooks;
    IHookRegistry public hookRegistry;

    // V3 sqrt-price tick-aligned extremes for "no limit" swaps.
    uint160 private constant MIN_SQRT_RATIO = 4295128740;
    uint160 private constant MAX_SQRT_RATIO = 1461446703485210103287273052203988822378723970341;

    uint256 private constant HOP_BYTES = 67;

    // Transient storage slots.
    uint256 private constant TSLOT_FLASH_POOL    = 0;
    uint256 private constant TSLOT_BORROW_TOKEN  = 1;
    uint256 private constant TSLOT_BORROW_AMOUNT = 2;
    uint256 private constant TSLOT_HOPS_PTR      = 3;
    uint256 private constant TSLOT_HOPS_LEN      = 4;

    constructor(address _poolManager, address _weth) {
        owner = msg.sender;
        poolManager = IPoolManager(_poolManager);
        weth = IWETH9(_weth);
        // Always allow the zero-hook (no-hook) pool key. Encoded as the
        // sentinel `address(0)` in `allowedHooks` is implicit; see _checkHook.
    }

    // setHookAllowed lets the owner toggle a specific hook address into the
    // local fallback whitelist. Use sparingly — V4 hooks can rewrite swap
    // accounting. Prefer wiring a HookRegistry via setHookRegistry so all
    // executors share one whitelist.
    function setHookAllowed(address hook, bool ok) external {
        require(msg.sender == owner, "owner");
        allowedHooks[hook] = ok;
    }

    // setHookRegistry wires the shared HookRegistry. When set, _checkHook
    // delegates to registry.isAllowed(hook) instead of the local map.
    function setHookRegistry(address registry) external {
        require(msg.sender == owner, "owner");
        hookRegistry = IHookRegistry(registry);
    }

    // ── Entry: flash-loan + V4 cycle ─────────────────────────────────────────

    // flash: owner-only entry. Borrows `amount` of borrowToken from a V3-style
    // flashPool, runs the V4-only cycle in `hops`, repays the flash, and
    // sweeps surplus to owner.
    function flash(
        address flashPool,
        address borrowToken,
        uint256 amount,
        bool isToken0,
        bytes calldata hops
    ) external {
        require(msg.sender == owner, "owner");
        require(hops.length >= HOP_BYTES && hops.length % HOP_BYTES == 0, "hops");

        assembly {
            tstore(TSLOT_FLASH_POOL, flashPool)
            tstore(TSLOT_BORROW_TOKEN, borrowToken)
            tstore(TSLOT_BORROW_AMOUNT, amount)
            tstore(TSLOT_HOPS_PTR, hops.offset)
            tstore(TSLOT_HOPS_LEN, hops.length)
        }

        uint256 amt0 = isToken0 ? amount : 0;
        uint256 amt1 = isToken0 ? 0 : amount;
        IUniswapV3PoolFlash(flashPool).flash(address(this), amt0, amt1, "");

        // Sweep any surplus borrow token (profit) to owner.
        uint256 bal = IERC20(borrowToken).balanceOf(address(this));
        if (bal > 0) {
            IERC20(borrowToken).transfer(owner, bal);
        }
        // If we ended with native ETH (e.g. final V4 hop output was ETH) sweep that too.
        uint256 ethBal = address(this).balance;
        if (ethBal > 0) {
            (bool ok, ) = payable(owner).call{value: ethBal}("");
            require(ok, "eth sweep");
        }
    }

    // V3 flash callback — invoked by the flashPool with our borrowed tokens
    // already in our balance. We then open ONE PoolManager.unlock and run
    // every V4 hop inside the same callback context.
    function uniswapV3FlashCallback(
        uint256 fee0,
        uint256 fee1,
        bytes calldata /*data*/
    ) external {
        address flashPool;
        address borrowToken;
        uint256 borrowAmount;
        assembly {
            flashPool := tload(TSLOT_FLASH_POOL)
            borrowToken := tload(TSLOT_BORROW_TOKEN)
            borrowAmount := tload(TSLOT_BORROW_AMOUNT)
        }
        require(msg.sender == flashPool, "auth");

        // Single unlock for the entire V4 cycle. unlockCallback reads hops
        // from transient storage, runs every hop, settles every currency
        // delta to net zero, and returns the final output amount.
        bytes memory result = poolManager.unlock("");
        uint256 finalOut = abi.decode(result, (uint256));

        // Compute repayment owed to the V3 flash pool (borrow + 1 fee tier).
        uint256 owed = borrowAmount + (fee0 > 0 ? fee0 : fee1);
        require(finalOut >= owed, "arb failed");

        // Repay the flash. Surplus stays in this contract for the outer
        // flash() function to sweep to owner.
        IERC20(borrowToken).transfer(flashPool, owed);
    }

    // PoolManager.unlock callback. Runs every hop in transient-stored hops,
    // accumulates per-currency net delta, settles all-in / takes all-out at
    // the end. Returns the final hop's output amount (in the borrowToken's
    // currency space) so the outer flashCallback can compute repayment.
    function unlockCallback(bytes calldata /*data*/) external returns (bytes memory) {
        require(msg.sender == address(poolManager), "not PM");

        uint256 hopsPtr;
        uint256 hopsLen;
        address borrowToken;
        uint256 currentAmount;
        assembly {
            hopsPtr := tload(TSLOT_HOPS_PTR)
            hopsLen := tload(TSLOT_HOPS_LEN)
            borrowToken := tload(TSLOT_BORROW_TOKEN)
            currentAmount := tload(TSLOT_BORROW_AMOUNT)
        }
        uint256 nHops = hopsLen / HOP_BYTES;

        // Walk hops. After each pm.swap, settle the input currency and take
        // the output currency immediately — keeps the accounting per-hop
        // clean even when hops chain through native ETH.
        address tokenIn = borrowToken;
        address tokenOut;
        for (uint256 i = 0; i < nHops; i++) {
            (address c0, address c1, bool zeroForOne, uint24 fee, int24 tickSpacing, address hooks) =
                _unpackHop(hopsPtr + i * HOP_BYTES);

            if (hooks != address(0)) {
                if (address(hookRegistry) != address(0)) {
                    require(hookRegistry.isAllowed(hooks), "hooks");
                } else {
                    require(allowedHooks[hooks], "hooks");
                }
            }

            tokenOut = zeroForOne ? c1 : c0;

            uint160 sqrtLimit = zeroForOne
                ? MIN_SQRT_RATIO + 1
                : MAX_SQRT_RATIO - 1;

            int256 balDelta = poolManager.swap(
                IPoolManager.PoolKey({
                    currency0: c0,
                    currency1: c1,
                    fee: fee,
                    tickSpacing: tickSpacing,
                    hooks: hooks
                }),
                IPoolManager.SwapParams({
                    zeroForOne: zeroForOne,
                    amountSpecified: -int256(currentAmount),
                    sqrtPriceLimitX96: sqrtLimit
                }),
                ""
            );

            int128 delta0 = int128(balDelta >> 128);
            int128 delta1 = int128(balDelta);
            int128 payDelta = zeroForOne ? delta0 : delta1;
            int128 outDelta = zeroForOne ? delta1 : delta0;

            // payDelta should be ≤ 0 (we owe the pool tokenIn);
            // outDelta should be ≥ 0 (pool owes us tokenOut).
            require(payDelta <= 0, "pay>0");
            require(outDelta >= 0, "out<0");
            uint256 payAmt = payDelta == 0 ? 0 : uint256(uint128(-payDelta));
            uint256 outAmt = outDelta == 0 ? 0 : uint256(uint128(outDelta));

            // Settle the input currency. Native ETH path: forward msg.value;
            // ERC20 path: sync, transfer, settle.
            _settleCurrency(tokenIn, payAmt);

            // Take the output currency to ourselves.
            if (outAmt > 0) {
                poolManager.take(tokenOut, address(this), outAmt);
            }

            // Chain to next hop.
            currentAmount = outAmt;
            tokenIn = tokenOut;
        }

        // Sanity: the cycle should end on the borrowToken.
        require(tokenIn == borrowToken, "cycle open");

        return abi.encode(currentAmount);
    }

    // ── Internals ────────────────────────────────────────────────────────────

    // _unpackHop reads one 67-byte packed hop record at calldata offset `base`.
    function _unpackHop(uint256 base) internal pure returns (
        address c0,
        address c1,
        bool zeroForOne,
        uint24 fee,
        int24 tickSpacing,
        address hooks
    ) {
        uint8 flags;
        uint256 feeRaw;
        uint256 tsRaw;
        assembly {
            c0       := shr(96, calldataload(base))
            c1       := shr(96, calldataload(add(base, 20)))
            flags    := byte(0, calldataload(add(base, 40)))
            // fee: 3 bytes (uint24) at offset 41
            feeRaw   := shr(232, calldataload(add(base, 41)))
            // tickSpacing: 3 bytes (int24) at offset 44 — two's complement
            tsRaw    := shr(232, calldataload(add(base, 44)))
            // hooks: 20 bytes at offset 47
            hooks    := shr(96, calldataload(add(base, 47)))
        }
        zeroForOne = (flags & 0x01) != 0;
        fee = uint24(feeRaw);
        // Sign-extend int24 from bit 23 if negative.
        int256 ts = int256(tsRaw);
        if ((ts & 0x800000) != 0) {
            ts |= ~int256(uint256(0xFFFFFF));
        }
        tickSpacing = int24(ts);
    }

    // _settleCurrency pays `payAmt` of `currency` to the PoolManager.
    // Three cases:
    //   - currency == 0x0 (native ETH): pm.settle{value: payAmt}()
    //   - currency == WETH AND we hold ETH: would need wrap; not handled — bot
    //     must encode WETH cycles uniformly OR start with WETH from the flash
    //   - currency = ERC20: pm.sync, transfer, pm.settle()
    function _settleCurrency(address currency, uint256 payAmt) internal {
        if (payAmt == 0) {
            poolManager.sync(currency);
            poolManager.settle();
            return;
        }
        if (currency == address(0)) {
            poolManager.settle{value: payAmt}();
            return;
        }
        poolManager.sync(currency);
        IERC20(currency).transfer(address(poolManager), payAmt);
        poolManager.settle();
    }

    // ── Admin / safety ──────────────────────────────────────────────────────

    function rescue(address token, uint256 amount) external {
        require(msg.sender == owner, "owner");
        if (token == address(0)) {
            (bool ok, ) = payable(owner).call{value: amount}("");
            require(ok, "rescue eth");
        } else {
            IERC20(token).transfer(owner, amount);
        }
    }

    receive() external payable {}
}
