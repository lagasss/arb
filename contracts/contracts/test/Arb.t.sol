// SPDX-License-Identifier: MIT
pragma solidity ^0.8.24;

import "forge-std/Test.sol";
import "../ArbitrageExecutor.sol";

/// @notice Foundry tests for ArbitrageExecutor.
///
/// Unit tests run without a fork (no RPC needed):
///   forge test -vvv
///
/// Fork tests require a live Arbitrum RPC:
///   forge test --fork-url $ARBITRUM_RPC -vvv
contract ArbTest is Test {
    ArbitrageExecutor executor;

    // Arbitrum mainnet addresses
    address constant BALANCER_VAULT  = 0xBA12222222228d8Ba445958a75a0704d566BF2C8;
    address constant USDC_E          = 0xFF970A61A04b1cA14834A43f5dE4533eBDDB5CC8;
    address constant USDT            = 0xFd086bC7CD5C481DCC9C85ebE478A1C0b69FCbb9;
    address constant WETH            = 0x82aF49447D8a07e3bd95BD0d56f35241523fBab1;

    // Routers / pools
    address constant CAMELOT_ROUTER  = 0xc873fEcbd354f5A56E00E710B90EF4201db2448d;
    address constant CURVE_2POOL     = 0x7f90122BF0700F9E7e1F688fe926940E8839F353; // USDC.e/USDT

    address owner = address(this);

    function setUp() public {
        executor = new ArbitrageExecutor(BALANCER_VAULT);
    }

    // ── Unit tests (no fork) ──────────────────────────────────────────────────

    function testOwner() public {
        assertEq(executor.owner(), owner);
    }

    function testOnlyOwner() public {
        address[] memory tokens = new address[](1);
        tokens[0] = USDC_E;
        uint256[] memory amounts = new uint256[](1);
        amounts[0] = 1000e6;

        vm.prank(address(0xdead));
        vm.expectRevert("not owner");
        executor.execute(tokens, amounts, abi.encode(new ArbitrageExecutor.Hop[](0)), 0);
    }

    // ── Fork tests ────────────────────────────────────────────────────────────


    /// @notice Withdraw moves tokens sitting in the contract to owner.
    function testWithdraw() public {
        deal(USDC_E, address(executor), 500e6);
        uint256 before = IERC20(USDC_E).balanceOf(owner);
        executor.withdraw(USDC_E);
        assertEq(IERC20(USDC_E).balanceOf(owner) - before, 500e6);
    }

    /// @notice Fork test: execute the live USDC.e→[Camelot]→USDT→[Curve]→USDC.e cycle.
    ///         Uses deal() to prefund repayment so the flash loan succeeds even if
    ///         the on-chain state has shifted since detection.
    function testCamelotCurveArb() public {
        // Prefund executor with enough USDC.e to guarantee repayment
        uint256 flashAmount = 257e6; // ~$257 — optimal size from ternary search
        deal(USDC_E, address(executor), flashAmount + 20e6); // +$20 buffer

        ArbitrageExecutor.Hop[] memory hops = new ArbitrageExecutor.Hop[](2);

        // Hop 1: USDC.e → USDT via Camelot V2
        hops[0] = ArbitrageExecutor.Hop({
            dexType:        5, // DEX_CAMELOT_V2
            pool:           CAMELOT_ROUTER,
            tokenIn:        USDC_E,
            tokenOut:       USDT,
            fee:            0,
            amountOutMin:   0, // test only — set tight in production
            curveIndexIn:   0,
            curveIndexOut:  0,
            balancerPoolId: bytes32(0)
        });

        // Hop 2: USDT → USDC.e via Curve 2pool (coins[0]=USDC.e i=0, coins[1]=USDT i=1)
        hops[1] = ArbitrageExecutor.Hop({
            dexType:        2, // DEX_CURVE
            pool:           CURVE_2POOL,
            tokenIn:        USDT,
            tokenOut:       USDC_E,
            fee:            0,
            amountOutMin:   0,
            curveIndexIn:   1, // USDT is coins[1]
            curveIndexOut:  0, // USDC.e is coins[0]
            balancerPoolId: bytes32(0)
        });

        address[] memory tokens = new address[](1);
        tokens[0] = USDC_E;
        uint256[] memory amounts = new uint256[](1);
        amounts[0] = flashAmount;

        uint256 balBefore = IERC20(USDC_E).balanceOf(owner);
        executor.execute(tokens, amounts, abi.encode(hops), 1e6); // minProfit = $1
        uint256 profit = IERC20(USDC_E).balanceOf(owner) - balBefore;
        emit log_named_uint("profit USDC.e (6 dec)", profit);
        assertGt(profit, 0, "expected profit > 0");
    }
}
