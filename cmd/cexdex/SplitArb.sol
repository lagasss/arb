// SPDX-License-Identifier: MIT
pragma solidity ^0.8.20;

/**
 * SplitArb — split-route directional arbitrage using Balancer flash loans.
 *
 * Flow:
 *   1. Flash borrow `borrowToken` (e.g. USDC) from Balancer Vault
 *   2. Buy `tradeToken` (e.g. WETH) from a single cheap pool (buy leg)
 *   3. Split-sell `tradeToken` across N expensive pools (sell legs)
 *   4. Repay flash loan + keep profit in borrowToken
 *
 * All pool interactions are direct (no router) for minimal gas overhead.
 * Supports UniV2-style and UniV3-style (including Algebra/PancakeV3) pools.
 */

// ── Interfaces ─────────────────────────────────────────────────────────────

interface IERC20 {
    function transfer(address to, uint256 amount) external returns (bool);
    function balanceOf(address account) external view returns (uint256);
}

interface IBalancerVault {
    function flashLoan(
        address recipient,
        address[] memory tokens,
        uint256[] memory amounts,
        bytes memory userData
    ) external;
}

interface IUniswapV3Pool {
    function swap(
        address recipient,
        bool zeroForOne,
        int256 amountSpecified,
        uint160 sqrtPriceLimitX96,
        bytes calldata data
    ) external returns (int256 amount0, int256 amount1);
}

interface IUniswapV2Pair {
    function swap(uint amount0Out, uint amount1Out, address to, bytes calldata data) external;
    function getReserves() external view returns (uint112 reserve0, uint112 reserve1, uint32);
    function token0() external view returns (address);
}

// ── Contract ──────────────────────────────────────────────────────────────

contract SplitArb {
    address public immutable owner;

    IBalancerVault public constant BALANCER =
        IBalancerVault(0xBA12222222228d8Ba445958a75a0704d566BF2C8);

    // V3 sqrtPriceLimit sentinels — allows full range traversal
    uint160 internal constant MIN_SQRT_RATIO = 4295128739 + 1;
    uint160 internal constant MAX_SQRT_RATIO =
        1461446703485210103287273052203988822378723970342 - 1;

    // ── Data types ──────────────────────────────────────────────────────────

    struct SellHop {
        address pool;
        bool    isV3;
        bool    zeroForOne; // true = tradeToken is token0 of pool
        uint256 amountIn;   // amount of tradeToken to route through this pool
    }

    struct TradeParams {
        address borrowToken;   // token to flash-borrow (stablecoin, e.g. USDC)
        uint256 borrowAmount;  // amount to borrow
        address tradeToken;    // token being bought then split-sold (e.g. WETH)
        // Buy leg
        address buyPool;
        bool    buyIsV3;
        bool    buyZeroForOne; // true = borrowToken is token0 of buyPool
        uint256 buyMinOut;     // min tradeToken to receive
        // Sell legs (split)
        SellHop[] sellHops;
        uint256   minProfitWei; // min net profit in borrowToken (after repayment)
    }

    // ── Constructor ─────────────────────────────────────────────────────────

    constructor() {
        owner = msg.sender;
    }

    modifier onlyOwner() {
        require(msg.sender == owner, "SplitArb: not owner");
        _;
    }

    // ── External entry ───────────────────────────────────────────────────────

    /**
     * Initiates the flash loan. Called by the bot off-chain.
     */
    function execute(TradeParams calldata p) external onlyOwner {
        address[] memory tokens = new address[](1);
        tokens[0] = p.borrowToken;
        uint256[] memory amounts = new uint256[](1);
        amounts[0] = p.borrowAmount;
        BALANCER.flashLoan(address(this), tokens, amounts, abi.encode(p));
    }

    // ── Balancer flash loan callback ─────────────────────────────────────────

    function receiveFlashLoan(
        address[] memory tokens,
        uint256[] memory amounts,
        uint256[] memory feeAmounts,
        bytes memory userData
    ) external {
        require(msg.sender == address(BALANCER), "SplitArb: caller not Balancer");

        TradeParams memory p = abi.decode(userData, (TradeParams));

        // Step 1: Buy tradeToken with borrowToken
        uint256 tradeAmount;
        if (p.buyIsV3) {
            tradeAmount = _swapV3(
                p.buyPool, p.borrowToken, p.buyZeroForOne, p.borrowAmount
            );
        } else {
            tradeAmount = _swapV2(
                p.buyPool, p.borrowToken, p.tradeToken, p.buyZeroForOne, p.borrowAmount
            );
        }
        require(tradeAmount >= p.buyMinOut, "SplitArb: buy slippage");

        // Validate sell hops don't exceed what we bought
        uint256 totalSellIn;
        for (uint i = 0; i < p.sellHops.length; i++) {
            totalSellIn += p.sellHops[i].amountIn;
        }
        require(totalSellIn <= tradeAmount, "SplitArb: oversell");

        // Step 2: Split-sell tradeToken across N pools
        for (uint i = 0; i < p.sellHops.length; i++) {
            SellHop memory h = p.sellHops[i];
            if (h.isV3) {
                _swapV3(h.pool, p.tradeToken, h.zeroForOne, h.amountIn);
            } else {
                _swapV2(h.pool, p.tradeToken, p.borrowToken, h.zeroForOne, h.amountIn);
            }
        }

        // Step 3: Check profit and repay
        uint256 repay = amounts[0] + feeAmounts[0];
        uint256 bal   = IERC20(p.borrowToken).balanceOf(address(this));
        require(bal >= repay + p.minProfitWei, "SplitArb: insufficient profit");

        IERC20(p.borrowToken).transfer(address(BALANCER), repay);

        // Step 4: Return profit to owner
        uint256 profit = IERC20(p.borrowToken).balanceOf(address(this));
        if (profit > 0) {
            IERC20(p.borrowToken).transfer(owner, profit);
        }
    }

    // ── V3 swap (direct pool, callback model) ────────────────────────────────

    function _swapV3(
        address pool,
        address tokenIn,
        bool zeroForOne,
        uint256 amountIn
    ) internal returns (uint256 amountOut) {
        uint160 limit = zeroForOne ? MIN_SQRT_RATIO : MAX_SQRT_RATIO;
        bytes memory cbData = abi.encode(tokenIn, amountIn);

        (int256 a0, int256 a1) = IUniswapV3Pool(pool).swap(
            address(this),
            zeroForOne,
            int256(amountIn),
            limit,
            cbData
        );
        amountOut = uint256(zeroForOne ? -a1 : -a0);
    }

    // UniV3 / SushiV3 / RamsesV3 callback
    function uniswapV3SwapCallback(
        int256 amount0Delta,
        int256 amount1Delta,
        bytes calldata data
    ) external {
        (address tokenIn,) = abi.decode(data, (address, uint256));
        uint256 owed = amount0Delta > 0 ? uint256(amount0Delta) : uint256(amount1Delta);
        IERC20(tokenIn).transfer(msg.sender, owed);
    }

    // PancakeSwap V3 callback (different name, same signature)
    function pancakeV3SwapCallback(
        int256 amount0Delta,
        int256 amount1Delta,
        bytes calldata data
    ) external {
        (address tokenIn,) = abi.decode(data, (address, uint256));
        uint256 owed = amount0Delta > 0 ? uint256(amount0Delta) : uint256(amount1Delta);
        IERC20(tokenIn).transfer(msg.sender, owed);
    }

    // Algebra (CamelotV3) callback
    function algebraSwapCallback(
        int256 amount0Delta,
        int256 amount1Delta,
        bytes calldata data
    ) external {
        (address tokenIn,) = abi.decode(data, (address, uint256));
        uint256 owed = amount0Delta > 0 ? uint256(amount0Delta) : uint256(amount1Delta);
        IERC20(tokenIn).transfer(msg.sender, owed);
    }

    // ── V2 swap (transfer-then-swap model) ───────────────────────────────────

    function _swapV2(
        address pair,
        address tokenIn,
        address tokenOut,
        bool zeroForOne, // tokenIn is token0
        uint256 amountIn
    ) internal returns (uint256 amountOut) {
        (uint112 r0, uint112 r1,) = IUniswapV2Pair(pair).getReserves();
        uint256 rIn  = zeroForOne ? uint256(r0) : uint256(r1);
        uint256 rOut = zeroForOne ? uint256(r1) : uint256(r0);

        // Standard V2 output with 0.3% fee (997/1000)
        uint256 amountInFee = amountIn * 997;
        amountOut = (amountInFee * rOut) / (rIn * 1000 + amountInFee);

        IERC20(tokenIn).transfer(pair, amountIn);
        (uint256 out0, uint256 out1) = zeroForOne
            ? (uint256(0), amountOut)
            : (amountOut, uint256(0));
        IUniswapV2Pair(pair).swap(out0, out1, address(this), new bytes(0));
    }

    // ── Admin ────────────────────────────────────────────────────────────────

    /// Rescue any tokens stuck in the contract (emergency use only)
    function withdraw(address token, uint256 amount) external onlyOwner {
        IERC20(token).transfer(owner, amount);
    }

    receive() external payable {}
}
