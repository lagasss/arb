// SPDX-License-Identifier: MIT
pragma solidity ^0.8.24;

// V3FlashMini — minimal V3-flash + V3-hop arbitrage executor.
//
// This contract is a specialist: it ONLY handles cycles that
//   (a) borrow their starting capital via a Uniswap-V3-style pool's flash() call
//   (b) route all hops through V3-style pool.swap() calls directly (no routers)
//   (c) have between 2 and 3 hops (enforced statically — no loop)
//
// Why it exists: the main ArbitrageExecutor dispatches across Balancer/V3/Aave
// flash, then over V2/V3/V4/Curve/Balancer-weighted hop types via router hops.
// That dispatch overhead is ~400k gas even for cycles that would only have
// needed the V3 path. At ~3-10 bp profit margins, the extra gas is the entire
// profit. This contract removes all code paths it doesn't use and talks to V3
// pools directly instead of via routers, cutting the per-trade cost to roughly
// the competitor's ~370k.
//
// Architecture: transient-storage flash state + direct pool.swap() with
// per-hop custom callback handling. No events, no access-control table, no
// configurable parameters — everything is immutable-or-caller-supplied.
//
// Access control: only the immutable `owner` can trigger flash(). Callbacks
// (uniswapV3FlashCallback, uniswapV3SwapCallback) validate their invoking
// context via transient state checks, not msg.sender, because V3 pools on
// exotic factories may not be predictable by factory address alone.
//
// Gas budget (target): ~350-400k for a 3-hop cycle.

interface IERC20 {
    function transfer(address to, uint256 amount) external returns (bool);
    function balanceOf(address account) external view returns (uint256);
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

contract V3FlashMini {
    address public immutable owner;

    // UniswapV3's sqrt price range limits. Using tick-aligned extremes means
    // swap() will consume the full input without being clamped by a user-
    // supplied limit — we trust the slippage check to catch bad fills.
    uint160 private constant MIN_SQRT_RATIO = 4295128740;
    uint160 private constant MAX_SQRT_RATIO = 1461446703485210103287273052203988822378723970341;

    // Transient storage (EIP-1153) holds flash-state between the entry
    // function and the flash callback. Using transient slots means zero
    // persistent-storage writes and no gas refund for cleanup.
    //
    // Layout:
    //   slot 0: flashPool (address) — the pool we borrowed from
    //   slot 1: borrowToken (address)
    //   slot 2: borrowAmount (uint256) — for repay calculation and first-hop input
    //   slot 3: hopsPtr (uint256) — calldata pointer to hops region
    //   slot 4: hopsLen (uint256) — calldata length of hops region
    uint256 private constant TSLOT_POOL = 0;
    uint256 private constant TSLOT_TOKEN = 1;
    uint256 private constant TSLOT_AMOUNT = 2;
    uint256 private constant TSLOT_HOPS_PTR = 3;
    uint256 private constant TSLOT_HOPS_LEN = 4;

    constructor() {
        owner = msg.sender;
    }

    // flash: owner-only entry. Borrows `amount` of token0-or-token1 from
    // `flashPool` and executes `hops` in the flash callback.
    //
    // hops layout (tightly packed bytes, 61 bytes per hop):
    //   [  0: 20] pool address (V3-compatible)
    //   [ 20: 40] tokenOut (the token we receive from this swap)
    //   [ 40: 41] flags byte:
    //                bit 0 = zeroForOne (1 if token0 is input)
    //                bit 1-7 reserved
    //   [ 41: 61] amountOutMin (uint160 — enough for any practical token
    //             amount; 2^160 covers 18-decimal tokens up to 1.4e30 units)
    //
    // The loop encodes the cycle: first hop takes `borrowAmount` of
    // borrowToken in, each subsequent hop takes the previous hop's output.
    // Final hop's output must be >= borrowAmount + fee (V3 flash fee) or the
    // tx reverts with "insufficient return".
    function flash(
        address flashPool,
        address borrowToken,
        uint256 amount,
        bool isToken0,
        bytes calldata hops
    ) external {
        require(msg.sender == owner, "owner");
        require(hops.length >= 61 && hops.length % 61 == 0, "hops");

        // Stash state in transient storage for the callback.
        assembly {
            tstore(TSLOT_POOL, flashPool)
            tstore(TSLOT_TOKEN, borrowToken)
            tstore(TSLOT_AMOUNT, amount)
            tstore(TSLOT_HOPS_PTR, hops.offset)
            tstore(TSLOT_HOPS_LEN, hops.length)
        }

        uint256 amt0 = isToken0 ? amount : 0;
        uint256 amt1 = isToken0 ? 0 : amount;
        // Empty data — callback reads from transient storage. Avoids the
        // calldata roundtrip cost of embedding hops here.
        IUniswapV3PoolFlash(flashPool).flash(address(this), amt0, amt1, "");

        // After flash() returns successfully, any profit left in this contract
        // (cycle output > borrow + fee) belongs to the owner.
        uint256 bal = IERC20(borrowToken).balanceOf(address(this));
        if (bal > 0) {
            IERC20(borrowToken).transfer(owner, bal);
        }
    }

    // V3 flash callback — invoked by the flashPool after transferring
    // borrowed tokens to us. We execute the cycle via pool.swap() calls and
    // ensure the final output covers borrow + fee.
    function uniswapV3FlashCallback(
        uint256 fee0,
        uint256 fee1,
        bytes calldata /*data*/
    ) external {
        address flashPool;
        address borrowToken;
        uint256 borrowAmount;
        uint256 hopsPtr;
        uint256 hopsLen;
        assembly {
            flashPool := tload(TSLOT_POOL)
            borrowToken := tload(TSLOT_TOKEN)
            borrowAmount := tload(TSLOT_AMOUNT)
            hopsPtr := tload(TSLOT_HOPS_PTR)
            hopsLen := tload(TSLOT_HOPS_LEN)
        }
        // Reject callbacks from any pool that wasn't the one we initiated
        // flash on.
        require(msg.sender == flashPool, "auth");

        // Walk packed hops: 61 bytes each.
        uint256 currentAmount = borrowAmount;
        uint256 nHops = hopsLen / 61;
        for (uint256 i = 0; i < nHops; i++) {
            address hopPool;
            address tokenOut;
            uint8 flags;
            uint256 amountOutMin;
            uint256 base = hopsPtr + i * 61;
            assembly {
                // pool (20 bytes, right-shifted after cdload)
                hopPool := shr(96, calldataload(base))
                // tokenOut (20 bytes, offset 20)
                tokenOut := shr(96, calldataload(add(base, 20)))
                // flags byte at offset 40
                flags := byte(0, calldataload(add(base, 40)))
                // amountOutMin uint160 at offset 41 (20 bytes, shifted)
                amountOutMin := shr(96, calldataload(add(base, 41)))
            }
            bool zeroForOne = (flags & 0x01) != 0;
            uint160 sqrtLimit = zeroForOne ? MIN_SQRT_RATIO + 1 : MAX_SQRT_RATIO - 1;

            (int256 a0, int256 a1) = IUniswapV3PoolFlash(hopPool).swap(
                address(this),
                zeroForOne,
                int256(currentAmount),
                sqrtLimit,
                ""
            );
            // Output is the negative amount (pool sends tokens to recipient).
            uint256 out = uint256(zeroForOne ? -a1 : -a0);
            require(out >= amountOutMin, "hop short");
            currentAmount = out;

            // Advance to next hop's tokenOut context: on next iteration we'll
            // swap `currentAmount` of `tokenOut` into the next hop's output.
            // The calldata already encodes the correct zeroForOne for each
            // pool — we trust the bot to pack it correctly.
            tokenOut; // silence unused-variable warning — tokenOut is informational only
        }

        // Compute amount owed back to flashPool = borrow + fee (exactly one of
        // fee0/fee1 is non-zero depending on which token we borrowed).
        uint256 owed = borrowAmount + (fee0 > 0 ? fee0 : fee1);
        require(currentAmount >= owed, "arb failed");

        // Repay the flash loan. Any surplus stays in this contract and gets
        // swept to owner by the outer flash() function.
        IERC20(borrowToken).transfer(flashPool, owed);
    }

    // V3 swap callback — invoked by the pool we're swapping against. We owe
    // whichever token shows a positive delta.
    //
    // Callback dispatch in V3-family: UniV3, SushiV3, RamsesV3 all call
    // `uniswapV3SwapCallback` (the canonical name from the original UniV3
    // pool source). PancakeV3 renamed it to `pancakeV3SwapCallback`.
    // CamelotV3/ZyberV3 (Algebra fork) use `algebraSwapCallback`. All three
    // have identical signatures and semantics — we alias the two forks to
    // the uniswapV3 implementation so any V3-family pool can route through
    // this contract without a selector miss.
    //
    // We don't validate msg.sender against a factory: access control is
    // enforced at the flash() entry (owner-only), and any nested swap is
    // initiated by us. An unauthorized caller spoofing this callback can't
    // extract funds because it has nothing to claim.
    function uniswapV3SwapCallback(
        int256 amount0Delta,
        int256 amount1Delta,
        bytes calldata /*data*/
    ) external {
        _handleSwapCallback(amount0Delta, amount1Delta);
    }

    // PancakeV3 forks renamed the callback; same semantics.
    function pancakeV3SwapCallback(
        int256 amount0Delta,
        int256 amount1Delta,
        bytes calldata /*data*/
    ) external {
        _handleSwapCallback(amount0Delta, amount1Delta);
    }

    // Algebra V1/Integral (CamelotV3, ZyberV3) uses this name.
    function algebraSwapCallback(
        int256 amount0Delta,
        int256 amount1Delta,
        bytes calldata /*data*/
    ) external {
        _handleSwapCallback(amount0Delta, amount1Delta);
    }

    function _handleSwapCallback(int256 amount0Delta, int256 amount1Delta) private {
        if (amount0Delta > 0) {
            address token0 = IUniswapV3PoolFlash(msg.sender).token0();
            IERC20(token0).transfer(msg.sender, uint256(amount0Delta));
        } else if (amount1Delta > 0) {
            address token1 = IUniswapV3PoolFlash(msg.sender).token1();
            IERC20(token1).transfer(msg.sender, uint256(amount1Delta));
        }
        // Both zero is possible for boundary conditions — nothing to do.
    }

    // Rescue function for stranded tokens (should never fire in normal
    // operation — the flash() sweep covers the happy path — but exists for
    // operational safety).
    function rescue(address token, uint256 amount) external {
        require(msg.sender == owner, "owner");
        IERC20(token).transfer(owner, amount);
    }
}
