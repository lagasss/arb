// SPDX-License-Identifier: MIT
pragma solidity ^0.8.24;

// MixedV3V4Executor — minimal V3+V4 multi-hop arbitrage executor.
//
// SCOPE
// -----
// Cycles that:
//   (a) borrow starting capital via a Uniswap-V3-style pool's flash() call
//   (b) route hops through ANY MIX of:
//         - Uniswap V4 (PoolManager singleton, native-ETH-aware)
//         - Uniswap V3 family — UniV3, SushiV3, RamsesV3, PancakeV3,
//           CamelotV3/ZyberV3 (Algebra) — via direct pool.swap() calls
//   (c) have between 2 and 5 hops total
//
// This is one of an expanding "Mixed*" family of contracts (e.g. future
// MixedV2V3Executor, MixedV3V4CurveExecutor) — each handles a specific
// blend of DEX dispatch primitives. Naming convention: MixedXyzExecutor
// where Xyz is the alphabetised set of DEX families it handles.
//
// WHY IT EXISTS
// -------------
// V4Mini handles cycles where every hop is V4. V3FlashMini handles cycles
// where every hop is V3-family. The generic ArbitrageExecutor handles
// mixed cycles but its V4 path has the V4_HANDLER bug class (no
// native-ETH support, broken settlement on hops with hooks, ~70k gas
// overhead per V4 hop because each hop opens its own unlock). Result:
// mixed cycles like UniV4→UniV4→UniV3 hit V4_HANDLER reverts even when
// the simulator agrees the cycle is profitable.
//
// MixedV3V4Executor closes that gap by:
//  - Wrapping the WHOLE cycle in ONE PoolManager.unlock callback
//  - Doing V4 hops via pm.swap inside that callback (with sync/settle/take
//    accounting per hop, native-ETH branch handled inline)
//  - Doing V3 hops via direct pool.swap inside the same callback — the V3
//    pool calls back into our uniswapV3SwapCallback (or its alias for
//    Pancake / Algebra) which transfers the input token; control returns
//    to the unlock loop and continues
//  - Hook gate: V4 hops with non-zero hooks revert unless the hook is in
//    the owner-whitelist (matches V4Mini's safety model)
//
// Gas envelope (target):
//   2-hop mixed (V4+V3)     ~350k
//   3-hop mixed             ~430k
//   4-hop mixed             ~510k
//   5-hop mixed             ~590k
// vs ~880k–1.2M for the generic executor's mixed-cycle path.
//
// CALLDATA FORMAT (hops)
// ----------------------
// Tightly packed bytes, 67 bytes per hop. The high bit of the flags byte
// (offset 40, bit 7) discriminates V4 vs V3 dispatch:
//
// V3 hop (flags bit 7 = 0):
//   [ 0:20] pool address (V3-compatible)
//   [20:40] tokenOut (informational; receive side derived from swap result)
//   [40:41] flags: bit 0 = zeroForOne; bit 7 = 0 (V3)
//   [41:67] zero/padding (ignored)
//
// V4 hop (flags bit 7 = 1):
//   [ 0:20] currency0 (PoolKey lower-address; 0x0 = native ETH)
//   [20:40] currency1 (PoolKey higher-address)
//   [40:41] flags: bit 0 = zeroForOne; bit 7 = 1 (V4)
//   [41:44] fee uint24 BE (PoolKey fee tier)
//   [44:47] tickSpacing int24 BE, two's complement
//   [47:67] hooks (address; 0x0 for no-hook pools)
//
// All hops share the same 67-byte stride for easy walking. The bot encodes
// the hops in cycle order; the contract walks them sequentially, chaining
// each hop's output as the next hop's input.

interface IERC20 {
    function transfer(address to, uint256 amount) external returns (bool);
    function balanceOf(address account) external view returns (uint256);
    function approve(address spender, uint256 amount) external returns (bool);
}

interface IUniswapV3PoolFlash {
    function flash(address recipient, uint256 amount0, uint256 amount1, bytes calldata data) external;
    function swap(
        address recipient,
        bool zeroForOne,
        int256 amountSpecified,
        uint160 sqrtPriceLimitX96,
        bytes calldata data
    ) external returns (int256 amount0, int256 amount1);
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

contract MixedV3V4Executor {
    address public immutable owner;
    IPoolManager public immutable poolManager;

    mapping(address => bool) public allowedHooks;
    IHookRegistry public hookRegistry;

    uint160 private constant MIN_SQRT_RATIO = 4295128740;
    uint160 private constant MAX_SQRT_RATIO = 1461446703485210103287273052203988822378723970341;

    uint256 private constant HOP_BYTES = 67;
    uint8 private constant FLAG_ZERO_FOR_ONE = 0x01;
    uint8 private constant FLAG_IS_V4        = 0x80;

    uint256 private constant TSLOT_FLASH_POOL    = 0;
    uint256 private constant TSLOT_BORROW_TOKEN  = 1;
    uint256 private constant TSLOT_BORROW_AMOUNT = 2;
    uint256 private constant TSLOT_HOPS_PTR      = 3;
    uint256 private constant TSLOT_HOPS_LEN      = 4;

    constructor(address _poolManager) {
        owner = msg.sender;
        poolManager = IPoolManager(_poolManager);
    }

    function setHookAllowed(address hook, bool ok) external {
        require(msg.sender == owner, "owner");
        allowedHooks[hook] = ok;
    }

    function setHookRegistry(address registry) external {
        require(msg.sender == owner, "owner");
        hookRegistry = IHookRegistry(registry);
    }

    // Owner-only entry. Borrows `amount` of borrowToken from a V3 flashPool,
    // runs the mixed V3+V4 cycle in `hops`, repays the flash, sweeps surplus.
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

        uint256 bal = IERC20(borrowToken).balanceOf(address(this));
        if (bal > 0) {
            IERC20(borrowToken).transfer(owner, bal);
        }
        uint256 ethBal = address(this).balance;
        if (ethBal > 0) {
            (bool ok, ) = payable(owner).call{value: ethBal}("");
            require(ok, "eth sweep");
        }
    }

    // V3 flash callback. Opens ONE PoolManager.unlock around the whole
    // cycle so V4 and V3 hops all run inside the same callback context.
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

        bytes memory result = poolManager.unlock("");
        uint256 finalOut = abi.decode(result, (uint256));

        uint256 owed = borrowAmount + (fee0 > 0 ? fee0 : fee1);
        require(finalOut >= owed, "arb failed");

        IERC20(borrowToken).transfer(flashPool, owed);
    }

    // PoolManager unlock callback. Walks every hop sequentially.
    //   - V4 hop: pm.swap → settle input → take output (deltas zero per-hop)
    //   - V3 hop: pool.swap → V3 calls back into uniswapV3SwapCallback to
    //             collect input → returns with output already in our balance
    //
    // V4 deltas net to zero per V4 hop, so the unlock's net-zero invariant
    // holds at exit. V3 hops touch tokens that are NOT part of V4's
    // accounting, so they don't interfere.
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

        address tokenIn = borrowToken;
        address tokenOut;
        for (uint256 i = 0; i < nHops; i++) {
            uint256 base = hopsPtr + i * HOP_BYTES;
            uint8 flags;
            assembly {
                flags := byte(0, calldataload(add(base, 40)))
            }
            bool zeroForOne = (flags & FLAG_ZERO_FOR_ONE) != 0;
            bool isV4 = (flags & FLAG_IS_V4) != 0;

            if (isV4) {
                (currentAmount, tokenOut) = _swapV4(base, zeroForOne, currentAmount, tokenIn);
            } else {
                (currentAmount, tokenOut) = _swapV3(base, zeroForOne, currentAmount, tokenIn);
            }

            tokenIn = tokenOut;
        }

        require(tokenIn == borrowToken, "cycle open");
        return abi.encode(currentAmount);
    }

    // ── V4 dispatch ─────────────────────────────────────────────────────────

    function _swapV4(
        uint256 base,
        bool zeroForOne,
        uint256 amountIn,
        address tokenInExpected
    ) internal returns (uint256 outAmt, address tokenOut) {
        address c0;
        address c1;
        uint256 feeRaw;
        uint256 tsRaw;
        address hooks;
        assembly {
            c0     := shr(96, calldataload(base))
            c1     := shr(96, calldataload(add(base, 20)))
            feeRaw := shr(232, calldataload(add(base, 41)))
            tsRaw  := shr(232, calldataload(add(base, 44)))
            hooks  := shr(96, calldataload(add(base, 47)))
        }
        if (hooks != address(0)) {
            if (address(hookRegistry) != address(0)) {
                require(hookRegistry.isAllowed(hooks), "hooks");
            } else {
                require(allowedHooks[hooks], "hooks");
            }
        }

        int256 ts = int256(tsRaw);
        if ((ts & 0x800000) != 0) {
            ts |= ~int256(uint256(0xFFFFFF));
        }

        address tokenIn = zeroForOne ? c0 : c1;
        tokenOut = zeroForOne ? c1 : c0;
        require(tokenIn == tokenInExpected, "v4:tokenIn");

        uint160 sqrtLimit = zeroForOne
            ? MIN_SQRT_RATIO + 1
            : MAX_SQRT_RATIO - 1;

        int256 balDelta = poolManager.swap(
            IPoolManager.PoolKey({
                currency0: c0,
                currency1: c1,
                fee: uint24(feeRaw),
                tickSpacing: int24(ts),
                hooks: hooks
            }),
            IPoolManager.SwapParams({
                zeroForOne: zeroForOne,
                amountSpecified: -int256(amountIn),
                sqrtPriceLimitX96: sqrtLimit
            }),
            ""
        );

        int128 delta0 = int128(balDelta >> 128);
        int128 delta1 = int128(balDelta);
        int128 payDelta = zeroForOne ? delta0 : delta1;
        int128 outDelta = zeroForOne ? delta1 : delta0;
        require(payDelta <= 0, "v4:pay>0");
        require(outDelta >= 0, "v4:out<0");
        uint256 payAmt = payDelta == 0 ? 0 : uint256(uint128(-payDelta));
        outAmt = outDelta == 0 ? 0 : uint256(uint128(outDelta));

        _settleCurrency(tokenIn, payAmt);
        if (outAmt > 0) {
            poolManager.take(tokenOut, address(this), outAmt);
        }
    }

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

    // ── V3 dispatch ─────────────────────────────────────────────────────────

    function _swapV3(
        uint256 base,
        bool zeroForOne,
        uint256 amountIn,
        address tokenInExpected
    ) internal returns (uint256 outAmt, address tokenOut) {
        address pool;
        address declaredOut;
        assembly {
            pool        := shr(96, calldataload(base))
            declaredOut := shr(96, calldataload(add(base, 20)))
        }
        tokenInExpected;

        uint160 sqrtLimit = zeroForOne ? MIN_SQRT_RATIO + 1 : MAX_SQRT_RATIO - 1;
        (int256 a0, int256 a1) = IUniswapV3PoolFlash(pool).swap(
            address(this),
            zeroForOne,
            int256(amountIn),
            sqrtLimit,
            ""
        );
        outAmt = uint256(zeroForOne ? -a1 : -a0);
        tokenOut = declaredOut;
    }

    // V3 swap callback aliases (UniV3/SushiV3/RamsesV3 use uniswapV3,
    // PancakeV3 renamed it, Algebra/CamelotV3/ZyberV3 use algebra).
    function uniswapV3SwapCallback(int256 amount0Delta, int256 amount1Delta, bytes calldata /*data*/) external {
        _handleSwapCallback(amount0Delta, amount1Delta);
    }
    function pancakeV3SwapCallback(int256 amount0Delta, int256 amount1Delta, bytes calldata /*data*/) external {
        _handleSwapCallback(amount0Delta, amount1Delta);
    }
    function algebraSwapCallback(int256 amount0Delta, int256 amount1Delta, bytes calldata /*data*/) external {
        _handleSwapCallback(amount0Delta, amount1Delta);
    }

    function _handleSwapCallback(int256 amount0Delta, int256 amount1Delta) private {
        if (amount0Delta > 0) {
            address t0 = IUniswapV3PoolFlash(msg.sender).token0();
            IERC20(t0).transfer(msg.sender, uint256(amount0Delta));
        } else if (amount1Delta > 0) {
            address t1 = IUniswapV3PoolFlash(msg.sender).token1();
            IERC20(t1).transfer(msg.sender, uint256(amount1Delta));
        }
    }

    // ── Admin ───────────────────────────────────────────────────────────────

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
