// SPDX-License-Identifier: MIT
pragma solidity ^0.8.24;

import "./interfaces/IBalancerVault.sol";
import "./interfaces/IUniswapV3Router.sol";
import "./interfaces/IUniswapV2Router.sol";
import "./interfaces/ICamelotV2Router.sol";
import "./interfaces/IAlgebraRouter.sol";
import "./interfaces/ICurvePool.sol";
import "./interfaces/IRamsesV3Router.sol";
import "./interfaces/IPoolManager.sol";
import "./interfaces/IPancakeV3Router.sol";
import "./interfaces/IUniswapV3Pool.sol";
import "./interfaces/IAaveV3Pool.sol";

// Note: DEX_BALANCER (type 4) is reserved but not implemented as a swap venue —
// Balancer is used only for flash loans, not as an intermediate swap pool.

interface IERC20 {
    function transfer(address to, uint256 amount) external returns (bool);
    function transferFrom(address from, address to, uint256 amount) external returns (bool);
    function approve(address spender, uint256 amount) external returns (bool);
    function balanceOf(address account) external view returns (uint256);
}

/// @title ArbitrageExecutor
/// @notice Executes flash-loan-funded arbitrage across multiple DEXes on Arbitrum.
///         Borrows from Balancer Vault (zero-fee flash loans), executes the swap path,
///         repays the loan, and transfers profit to owner.
///
/// Supported DEX types:
///   DEX_V2         — UniswapV2-compatible (Camelot, SushiSwap, TraderJoe, Ramses, …)
///   DEX_V3         — UniswapV3-compatible (UniV3, SushiV3, …)
///   DEX_CURVE      — Curve StableSwap pools (direct pool call, coin indices)
///   DEX_CAMELOT_V3 — Algebra V1 concentrated liquidity (Camelot V3, no fee param)
///   DEX_BALANCER   — Balancer weighted/stable pools (Vault.swap)
contract ArbitrageExecutor {
    address public immutable owner;
    IBalancerVault public immutable balancerVault;
    address public aavePool; // Aave V3 Pool on Arbitrum — set via setAavePool()

    uint8 constant DEX_V2         = 0;
    uint8 constant DEX_V3         = 1;
    uint8 constant DEX_CURVE      = 2;
    uint8 constant DEX_CAMELOT_V3 = 3;
    uint8 constant DEX_BALANCER   = 4;
    uint8 constant DEX_CAMELOT_V2 = 5; // Camelot V2 router (has extra referrer param)
    uint8 constant DEX_RAMSES_V3  = 6; // Ramses V3 CL (uses exactInput with path encoding)
    uint8 constant DEX_UNIV4      = 7; // Uniswap V4 (uses PoolManager.unlock → swap)
    uint8 constant DEX_PANCAKE_V3 = 8; // PancakeSwap V3 (forked UniV3 but removed `deadline`)

    /// @notice One swap step in the arbitrage path.
    /// @param dexType       One of the DEX_* constants above.
    /// @param pool          For V2/V3/Algebra: the router address.
    ///                      For Curve: the pool address directly.
    ///                      For Balancer: the Vault address.
    /// @param tokenIn       Input token address.
    /// @param tokenOut      Output token address.
    /// @param fee           V3 fee tier in hundredths of a bip (e.g. 3000 = 0.3%).
    ///                      Unused for V2, Curve, Algebra, Balancer.
    /// @param amountOutMin  Minimum acceptable output (slippage guard). Revert if not met.
    /// @param curveIndexIn  Curve coin index for tokenIn  (ignored for non-Curve hops).
    /// @param curveIndexOut Curve coin index for tokenOut (ignored for non-Curve hops).
    /// @param balancerPoolId Balancer bytes32 pool ID     (ignored for non-Balancer hops).
    struct Hop {
        uint8   dexType;
        address pool;
        address tokenIn;
        address tokenOut;
        uint24  fee;
        uint256 amountOutMin;
        int128  curveIndexIn;
        int128  curveIndexOut;
        bytes32 balancerPoolId;
    }

    event ArbExecuted(address indexed token, uint256 borrowed, uint256 profit);

    modifier onlyOwner() {
        require(msg.sender == owner, "not owner");
        _;
    }

    constructor(address _balancerVault) {
        owner = msg.sender;
        balancerVault = IBalancerVault(_balancerVault);
    }

    function setAavePool(address _aavePool) external onlyOwner {
        aavePool = _aavePool;
    }

    // ── Entry point ──────────────────────────────────────────────────────────

    /// @notice Initiate a Balancer flash loan then execute the arb path.
    /// @param tokens     Tokens to borrow (typically one).
    /// @param amounts    Borrow amounts matching tokens[].
    /// @param hops       ABI-encoded Hop[] describing the swap sequence.
    /// @param minProfit  Minimum profit in token base units. Reverts if not met — protects
    ///                   against executing when the opportunity has already been taken.
    function execute(
        address[] calldata tokens,
        uint256[] calldata amounts,
        bytes calldata hops,
        uint256 minProfit
    ) external onlyOwner {
        bytes memory userData = abi.encode(amounts, hops, minProfit);
        balancerVault.flashLoan(address(this), tokens, amounts, userData);
    }

    // ── Balancer flash loan callback ──────────────────────────────────────────

    function receiveFlashLoan(
        address[] memory tokens,
        uint256[] memory amounts,
        uint256[] memory feeAmounts,
        bytes memory userData
    ) external {
        require(msg.sender == address(balancerVault), "unauthorized");

        (uint256[] memory borrowed, bytes memory hopData, uint256 minProfit) =
            abi.decode(userData, (uint256[], bytes, uint256));
        Hop[] memory hops = abi.decode(hopData, (Hop[]));

        uint256 amountIn = borrowed[0];
        address startToken = tokens[0];

        _executeHops(hops, amountIn);

        // Repay flash loan (Balancer fee is 0 on most pools, but handle non-zero)
        for (uint256 i = 0; i < tokens.length; i++) {
            uint256 repay = amounts[i] + feeAmounts[i];
            IERC20(tokens[i]).transfer(address(balancerVault), repay);
        }

        // Enforce minimum profit — revert cheaply if the arb was frontrun or pool moved.
        uint256 profit = IERC20(startToken).balanceOf(address(this));
        require(profit >= minProfit, "profit below minimum");

        if (profit > 0) {
            IERC20(startToken).transfer(owner, profit);
            emit ArbExecuted(startToken, borrowed[0], profit);
        }
    }

    // ── Uniswap V3 flash loan entry point ──────────────────────────────────────
    //
    // V3 pools expose flash(recipient, amount0, amount1, data) which lends tokens
    // and calls back uniswapV3FlashCallback. The borrower must repay
    // amount + fee (fee = amount * pool.fee / 1_000_000). This costs 5-100 bps
    // depending on the pool's fee tier — more expensive than Balancer (0) but
    // available for any token with a V3 pool.
    //
    // The Go code selects the cheapest V3 pool (lowest fee tier) that has enough
    // liquidity for the borrow amount.

    /// @notice Borrow from a Uniswap V3 pool's flash() and execute the arb path.
    /// @param pool       The V3 pool to flash-borrow from.
    /// @param borrowToken Which of the pool's two tokens to borrow.
    /// @param amount     How much to borrow.
    /// @param hops       ABI-encoded Hop[] describing the swap sequence.
    /// @param minProfit  Minimum profit in borrowToken base units.
    function executeV3Flash(
        address pool,
        address borrowToken,
        uint256 amount,
        bytes calldata hops,
        uint256 minProfit
    ) external onlyOwner {
        IUniswapV3Pool v3 = IUniswapV3Pool(pool);
        bool borrow0 = (borrowToken == v3.token0());
        uint256 amount0 = borrow0 ? amount : 0;
        uint256 amount1 = borrow0 ? 0 : amount;
        bytes memory data = abi.encode(borrowToken, amount, hops, minProfit);
        v3.flash(address(this), amount0, amount1, data);
    }

    /// @notice Uniswap V3 flash callback — called by the V3 pool after lending.
    function uniswapV3FlashCallback(
        uint256 fee0,
        uint256 fee1,
        bytes calldata data
    ) external {
        (address borrowToken, uint256 borrowed, bytes memory hopData, uint256 minProfit) =
            abi.decode(data, (address, uint256, bytes, uint256));
        Hop[] memory hops = abi.decode(hopData, (Hop[]));

        _executeHops(hops, borrowed);

        // Repay: borrowed amount + V3 flash fee
        uint256 fee = (borrowToken == IUniswapV3Pool(msg.sender).token0()) ? fee0 : fee1;
        uint256 repay = borrowed + fee;
        IERC20(borrowToken).transfer(msg.sender, repay);

        // Profit check + transfer
        uint256 profit = IERC20(borrowToken).balanceOf(address(this));
        require(profit >= minProfit, "profit below minimum");
        if (profit > 0) {
            IERC20(borrowToken).transfer(owner, profit);
            emit ArbExecuted(borrowToken, borrowed, profit);
        }
    }

    // ── Aave V3 flash loan entry point ──────────────────────────────────────────
    //
    // Aave V3 exposes flashLoanSimple(receiver, asset, amount, params, referralCode).
    // Callback is executeOperation(asset, amount, premium, initiator, params).
    // Premium is fixed at FLASHLOAN_PREMIUM_TOTAL bps (currently 5 = 0.05% on Arbitrum).
    // More expensive than Balancer (0) and often more than V3 flash (5-30 bps),
    // but available for any Aave-listed reserve (~20-30 major tokens on Arbitrum).

    /// @notice Borrow from Aave V3 and execute the arb path.
    /// @param token      Token to borrow.
    /// @param amount     How much to borrow.
    /// @param hops       ABI-encoded Hop[] describing the swap sequence.
    /// @param minProfit  Minimum profit in token base units.
    function executeAaveFlash(
        address token,
        uint256 amount,
        bytes calldata hops,
        uint256 minProfit
    ) external onlyOwner {
        require(aavePool != address(0), "aave pool not set");
        bytes memory params = abi.encode(hops, minProfit);
        // Approve Aave to pull the repayment (amount + premium) after the callback
        IERC20(token).approve(aavePool, amount + (amount * 10 / 10000)); // generous approval (0.1%)
        IAaveV3Pool(aavePool).flashLoanSimple(
            address(this),  // receiver
            token,
            amount,
            params,
            0               // referralCode
        );
    }

    /// @notice Aave V3 flash loan callback — called by the Aave Pool after lending.
    function executeOperation(
        address asset,
        uint256 amount,
        uint256 premium,
        address initiator,
        bytes calldata params
    ) external returns (bool) {
        require(msg.sender == aavePool, "unauthorized");
        require(initiator == address(this), "bad initiator");

        (bytes memory hopData, uint256 minProfit) = abi.decode(params, (bytes, uint256));
        Hop[] memory hops = abi.decode(hopData, (Hop[]));

        _executeHops(hops, amount);

        // Repay: Aave pulls amount + premium via transferFrom (we approved above)
        // No explicit transfer needed — Aave does it via the approval.

        // Profit check: balance after Aave pulls repayment = profit
        // Since Aave hasn't pulled yet at this point, compute expected profit:
        uint256 balance = IERC20(asset).balanceOf(address(this));
        uint256 repay = amount + premium;
        require(balance >= repay, "insufficient balance for Aave repay");
        uint256 profit = balance - repay;
        require(profit >= minProfit, "profit below minimum");

        // Approve Aave to pull repayment
        IERC20(asset).approve(aavePool, repay);

        // Transfer profit to owner BEFORE Aave pulls (order matters)
        if (profit > 0) {
            IERC20(asset).transfer(owner, profit);
            emit ArbExecuted(asset, amount, profit);
        }

        return true; // signal Aave that the operation succeeded
    }

    // ── Swap routing ──────────────────────────────────────────────────────────

    /// @dev Converts a single digit (0-9) to its ASCII character byte.
    function _digit(uint256 n) internal pure returns (bytes1) {
        return bytes1(uint8(48 + n));
    }

    function _executeHops(Hop[] memory hops, uint256 amountIn) internal {
        uint256 current = amountIn;
        for (uint256 i = 0; i < hops.length; i++) {
            Hop memory hop = hops[i];
            uint256 out;
            if (hop.dexType == DEX_V3) {
                out = _swapV3(hop, current);
            } else if (hop.dexType == DEX_CURVE) {
                out = _swapCurve(hop, current);
            } else if (hop.dexType == DEX_CAMELOT_V3) {
                out = _swapCamelotV3(hop, current);
            } else if (hop.dexType == DEX_BALANCER) {
                out = _swapBalancer(hop, current);
            } else if (hop.dexType == DEX_CAMELOT_V2) {
                out = _swapCamelotV2(hop, current);
            } else if (hop.dexType == DEX_RAMSES_V3) {
                out = _swapRamsesV3(hop, current);
            } else if (hop.dexType == DEX_UNIV4) {
                out = _swapUniV4(hop, current);
            } else if (hop.dexType == DEX_PANCAKE_V3) {
                out = _swapPancakeV3(hop, current);
            } else {
                out = _swapV2(hop, current);
            }
            if (out == 0) {
                revert(string(abi.encodePacked("hop ", _digit(i), ": swap reverted")));
            }
            if (out < hop.amountOutMin) {
                revert(string(abi.encodePacked("hop ", _digit(i), ": slippage")));
            }
            current = out;
        }
    }

    function _swapV2(Hop memory hop, uint256 amountIn) internal returns (uint256) {
        IERC20(hop.tokenIn).approve(hop.pool, amountIn);
        address[] memory path = new address[](2);
        path[0] = hop.tokenIn;
        path[1] = hop.tokenOut;
        try IUniswapV2Router(hop.pool).swapExactTokensForTokens(
            amountIn,
            hop.amountOutMin,
            path,
            address(this),
            block.timestamp + 60
        ) returns (uint256[] memory amounts) {
            return amounts[amounts.length - 1];
        } catch {
            return 0;
        }
    }

    /// @dev Camelot V2 router. Camelot deprecated `swapExactTokensForTokens` —
    ///      it reverts with empty data on every call. The supported entry point
    ///      is `swapExactTokensForTokensSupportingFeeOnTransferTokens`, which
    ///      returns void; we compute amountOut via tokenOut balance diff.
    ///      Same balance-diff pattern as `_swapCurve`.
    function _swapCamelotV2(Hop memory hop, uint256 amountIn) internal returns (uint256) {
        IERC20(hop.tokenIn).approve(hop.pool, amountIn);
        address[] memory path = new address[](2);
        path[0] = hop.tokenIn;
        path[1] = hop.tokenOut;
        uint256 balBefore = IERC20(hop.tokenOut).balanceOf(address(this));
        try ICamelotV2Router(hop.pool).swapExactTokensForTokensSupportingFeeOnTransferTokens(
            amountIn,
            hop.amountOutMin,
            path,
            address(this),
            address(0), // no referrer
            block.timestamp + 60
        ) {
            return IERC20(hop.tokenOut).balanceOf(address(this)) - balBefore;
        } catch {
            return 0;
        }
    }

    function _swapV3(Hop memory hop, uint256 amountIn) internal returns (uint256) {
        IERC20(hop.tokenIn).approve(hop.pool, amountIn);
        try IUniswapV3Router(hop.pool).exactInputSingle(
            IUniswapV3Router.ExactInputSingleParams({
                tokenIn:           hop.tokenIn,
                tokenOut:          hop.tokenOut,
                fee:               hop.fee,
                recipient:         address(this),
                deadline:          block.timestamp + 60,
                amountIn:          amountIn,
                amountOutMinimum:  hop.amountOutMin,
                sqrtPriceLimitX96: 0
            })
        ) returns (uint256 amountOut) {
            return amountOut;
        } catch {
            return 0;
        }
    }

    /// @dev PancakeSwap V3 SwapRouter — same shape as UniV3 but the
    ///      `ExactInputSingleParams` struct has no `deadline` field. The
    ///      function selector is therefore different (0x04e45aaf) so this
    ///      MUST go through `IPancakeV3Router`, not `IUniswapV3Router`.
    function _swapPancakeV3(Hop memory hop, uint256 amountIn) internal returns (uint256) {
        IERC20(hop.tokenIn).approve(hop.pool, amountIn);
        try IPancakeV3Router(hop.pool).exactInputSingle(
            IPancakeV3Router.ExactInputSingleParams({
                tokenIn:           hop.tokenIn,
                tokenOut:          hop.tokenOut,
                fee:               hop.fee,
                recipient:         address(this),
                amountIn:          amountIn,
                amountOutMinimum:  hop.amountOutMin,
                sqrtPriceLimitX96: 0
            })
        ) returns (uint256 amountOut) {
            return amountOut;
        } catch {
            return 0;
        }
    }

    /// @dev Curve pools are called directly (no router). Approve the pool itself.
    /// Uses a low-level call + balance-diff approach because old-style Curve StableSwap
    /// pools (Vyper) return void from exchange(), causing ABI-decode failures with try/catch.
    function _swapCurve(Hop memory hop, uint256 amountIn) internal returns (uint256) {
        IERC20(hop.tokenIn).approve(hop.pool, amountIn);
        uint256 balBefore = IERC20(hop.tokenOut).balanceOf(address(this));
        (bool ok,) = hop.pool.call(abi.encodeWithSelector(
            bytes4(0x3df02124), // exchange(int128,int128,uint256,uint256)
            hop.curveIndexIn,
            hop.curveIndexOut,
            amountIn,
            hop.amountOutMin
        ));
        if (!ok) return 0;
        return IERC20(hop.tokenOut).balanceOf(address(this)) - balBefore;
    }

    /// @dev Algebra V1 (Camelot V3): same as UniV3 but no fee field in the struct.
    function _swapCamelotV3(Hop memory hop, uint256 amountIn) internal returns (uint256) {
        IERC20(hop.tokenIn).approve(hop.pool, amountIn);
        try IAlgebraRouter(hop.pool).exactInputSingle(
            IAlgebraRouter.ExactInputSingleParams({
                tokenIn:          hop.tokenIn,
                tokenOut:         hop.tokenOut,
                recipient:        address(this),
                deadline:         block.timestamp + 60,
                amountIn:         amountIn,
                amountOutMinimum: hop.amountOutMin,
                limitSqrtPrice:   0
            })
        ) returns (uint256 amountOut) {
            return amountOut;
        } catch {
            return 0;
        }
    }

    /// @dev Balancer V2: swap via the Vault singleton using the pool's bytes32 poolId.
    /// hop.pool = Balancer Vault address, hop.balancerPoolId = pool ID.
    function _swapBalancer(Hop memory hop, uint256 amountIn) internal returns (uint256) {
        IERC20(hop.tokenIn).approve(address(balancerVault), amountIn);
        try balancerVault.swap(
            IBalancerVault.SingleSwap({
                poolId:   hop.balancerPoolId,
                kind:     IBalancerVault.SwapKind.GIVEN_IN,
                assetIn:  hop.tokenIn,
                assetOut: hop.tokenOut,
                amount:   amountIn,
                userData: ""
            }),
            IBalancerVault.FundManagement({
                sender:              address(this),
                fromInternalBalance: false,
                recipient:           payable(address(this)),
                toInternalBalance:   false
            }),
            hop.amountOutMin,
            block.timestamp + 60
        ) returns (uint256 amountOut) {
            return amountOut;
        } catch {
            return 0;
        }
    }

    /// @dev Ramses V3 CL: uses exactInput (path-encoded) because their SwapRouter
    ///      doesn't implement exactInputSingle. Path = abi.encodePacked(tokenIn, fee, tokenOut).
    function _swapRamsesV3(Hop memory hop, uint256 amountIn) internal returns (uint256) {
        IERC20(hop.tokenIn).approve(hop.pool, amountIn);
        bytes memory path = abi.encodePacked(hop.tokenIn, hop.fee, hop.tokenOut);
        try IRamsesV3Router(hop.pool).exactInput(
            IRamsesV3Router.ExactInputParams({
                path:              path,
                recipient:         address(this),
                deadline:          block.timestamp + 60,
                amountIn:          amountIn,
                amountOutMinimum:  hop.amountOutMin
            })
        ) returns (uint256 amountOut) {
            return amountOut;
        } catch {
            return 0;
        }
    }

    /// @dev Uniswap V4: swap via the PoolManager's unlock/callback pattern.
    /// hop.pool = PoolManager address.
    /// hop.fee = pool fee tier (uint24).
    /// hop.curveIndexIn = tickSpacing (int128, reused).
    /// hop.balancerPoolId = hooks address (bytes32, right-aligned address).
    /// Currency ordering: V4 requires currency0 < currency1 in the PoolKey.
    function _swapUniV4(Hop memory hop, uint256 amountIn) internal returns (uint256) {
        // Encode all swap params for the unlockCallback.
        bytes memory cbData = abi.encode(hop, amountIn);
        // NOTE: try/catch removed so the underlying V4 revert reason surfaces
        // in eth_call traces. Re-wrap in a try/catch returning 0 once the
        // handler is known-good and the outer _executeHops uses the sentinel
        // to emit its standardized "hop N: swap reverted" error.
        bytes memory result = IPoolManager(hop.pool).unlock(cbData);
        return abi.decode(result, (uint256));
    }

    /// @dev Called by the V4 PoolManager during unlock(). Executes the swap and
    ///      settles token balances. Must return the output amount encoded as bytes.
    function unlockCallback(bytes calldata data) external returns (bytes memory) {
        (Hop memory hop, uint256 amountIn) = abi.decode(data, (Hop, uint256));
        require(msg.sender == hop.pool, "not PM");

        IPoolManager pm = IPoolManager(hop.pool);

        // Build PoolKey — currencies must be sorted (currency0 < currency1).
        address hooks = address(bytes20(hop.balancerPoolId));
        int24 tickSpacing = int24(hop.curveIndexIn);
        bool zeroForOne = hop.tokenIn < hop.tokenOut;
        address currency0 = zeroForOne ? hop.tokenIn : hop.tokenOut;
        address currency1 = zeroForOne ? hop.tokenOut : hop.tokenIn;

        IPoolManager.PoolKey memory key = IPoolManager.PoolKey({
            currency0: currency0,
            currency1: currency1,
            fee:       hop.fee,
            tickSpacing: tickSpacing,
            hooks:     hooks
        });

        // Negative amountSpecified = exactInput.
        uint160 sqrtLimit = zeroForOne
            ? 4295128740          // MIN_SQRT_RATIO + 1
            : 1461446703485210103287273052203988822378723970341; // MAX_SQRT_RATIO - 1

        int256 balDelta = pm.swap(key,
            IPoolManager.SwapParams({
                zeroForOne: zeroForOne,
                amountSpecified: -int256(amountIn),
                sqrtPriceLimitX96: sqrtLimit
            }), "");

        int128 delta0 = int128(balDelta >> 128);
        int128 delta1 = int128(balDelta);

        int128 payDelta = zeroForOne ? delta0 : delta1;
        int128 outDelta = zeroForOne ? delta1 : delta0;
        require(payDelta <= 0, "v4:pay>0");
        require(outDelta >= 0, "v4:out<0");
        uint256 payAmt = payDelta == 0 ? 0 : uint256(uint128(-payDelta));
        uint256 outAmt = outDelta == 0 ? 0 : uint256(uint128(outDelta));

        require(IERC20(hop.tokenIn).balanceOf(address(this)) >= payAmt, "v4:bal<pay");

        pm.sync(hop.tokenIn);
        IERC20(hop.tokenIn).transfer(hop.pool, payAmt);
        pm.settle();
        pm.take(hop.tokenOut, address(this), outAmt);

        return abi.encode(outAmt);
    }

    // ── Admin ─────────────────────────────────────────────────────────────────

    /// @notice Emergency token withdrawal.
    function withdraw(address token) external onlyOwner {
        uint256 bal = IERC20(token).balanceOf(address(this));
        if (bal > 0) IERC20(token).transfer(owner, bal);
    }

    /// @notice Emergency ETH withdrawal.
    function withdrawETH() external onlyOwner {
        (bool ok,) = payable(owner).call{value: address(this).balance}("");
        require(ok, "ETH transfer failed");
    }

    receive() external payable {}
}
