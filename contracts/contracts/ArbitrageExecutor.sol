// SPDX-License-Identifier: MIT
pragma solidity ^0.8.24;

import "./interfaces/IBalancerVault.sol";
import "./interfaces/IUniswapV3Router.sol";
import "./interfaces/IUniswapV2Router.sol";
import "./interfaces/ICamelotV2Router.sol";
import "./interfaces/IAlgebraRouter.sol";
import "./interfaces/ICurvePool.sol";

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

    uint8 constant DEX_V2         = 0;
    uint8 constant DEX_V3         = 1;
    uint8 constant DEX_CURVE      = 2;
    uint8 constant DEX_CAMELOT_V3 = 3;
    uint8 constant DEX_BALANCER   = 4;
    uint8 constant DEX_CAMELOT_V2 = 5; // Camelot V2 router (has extra referrer param)

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
    event ArbFailed(string reason);

    modifier onlyOwner() {
        require(msg.sender == owner, "not owner");
        _;
    }

    constructor(address _balancerVault) {
        owner = msg.sender;
        balancerVault = IBalancerVault(_balancerVault);
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

        bool success = _executeHops(hops, amountIn);
        if (!success) {
            emit ArbFailed("swap path failed");
            revert("arb failed");
        }

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

    // ── Swap routing ──────────────────────────────────────────────────────────

    function _executeHops(Hop[] memory hops, uint256 amountIn) internal returns (bool) {
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
            } else {
                out = _swapV2(hop, current);
            }
            if (out < hop.amountOutMin) {
                return false;
            }
            current = out;
        }
        return true;
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

    /// @dev Camelot V2 router — same as UniV2 but with an extra `referrer` param (pass address(0)).
    function _swapCamelotV2(Hop memory hop, uint256 amountIn) internal returns (uint256) {
        IERC20(hop.tokenIn).approve(hop.pool, amountIn);
        address[] memory path = new address[](2);
        path[0] = hop.tokenIn;
        path[1] = hop.tokenOut;
        try ICamelotV2Router(hop.pool).swapExactTokensForTokens(
            amountIn,
            hop.amountOutMin,
            path,
            address(this),
            address(0), // no referrer
            block.timestamp + 60
        ) returns (uint256[] memory amounts) {
            return amounts[amounts.length - 1];
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

    /// @dev Curve pools are called directly (no router). Approve the pool itself.
    function _swapCurve(Hop memory hop, uint256 amountIn) internal returns (uint256) {
        IERC20(hop.tokenIn).approve(hop.pool, amountIn);
        try ICurvePool(hop.pool).exchange(
            hop.curveIndexIn,
            hop.curveIndexOut,
            amountIn,
            hop.amountOutMin
        ) returns (uint256 amountOut) {
            return amountOut;
        } catch {
            return 0;
        }
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

    /// @dev Balancer single-hop swap via the Vault. pool field = Vault address.
    function _swapBalancer(Hop memory hop, uint256 amountIn) internal returns (uint256) {
        IERC20(hop.tokenIn).approve(hop.pool, amountIn);
        try IBalancerVault(hop.pool).swap(
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
