// SPDX-License-Identifier: MIT
pragma solidity ^0.8.24;

import "forge-std/Test.sol";
import "../V3FlashMini.sol";

// Fork tests for V3FlashMini against live Arbitrum state. Requires an archive
// RPC via $ARBITRUM_RPC. Run:
//   forge test --match-contract V3FlashMiniTest --fork-url $ARBITRUM_RPC -vvv
//
// Unit tests (ownership, calldata parsing) run without a fork:
//   forge test --match-contract V3FlashMiniUnitTest -vvv
contract V3FlashMiniUnitTest is Test {
    V3FlashMini mini;
    address owner = address(this);

    function setUp() public {
        mini = new V3FlashMini();
    }

    function test_OwnerIsDeployer() public view {
        assertEq(mini.owner(), owner);
    }

    function test_OnlyOwnerCanFlash() public {
        address attacker = address(0xBEEF);
        vm.prank(attacker);
        vm.expectRevert(bytes("owner"));
        mini.flash(
            address(0x1234),
            address(0x5678),
            1e18,
            true,
            hex"00"
        );
    }

    function test_HopsMustBeMultipleOf61Bytes() public {
        // Empty hops reverts with "hops".
        vm.expectRevert(bytes("hops"));
        mini.flash(address(0x1234), address(0x5678), 1e18, true, hex"");

        // 60 bytes reverts.
        bytes memory bad = new bytes(60);
        vm.expectRevert(bytes("hops"));
        mini.flash(address(0x1234), address(0x5678), 1e18, true, bad);

        // 61 bytes is valid (1 hop) — won't revert at the length check.
        // Will revert later when flash() tries to call a non-existent pool,
        // which is expected.
        bytes memory ok = new bytes(61);
        vm.expectRevert(); // any revert from the flash() call
        mini.flash(address(0x1234), address(0x5678), 1e18, true, ok);
    }

    function test_RescueOnlyOwner() public {
        address attacker = address(0xDEAD);
        vm.prank(attacker);
        vm.expectRevert(bytes("owner"));
        mini.rescue(address(0x1234), 1e18);
    }
}

// Fork test: real 2-hop cycle via WETH/USDC on UniV3 at two fee tiers.
// Skipped when ARBITRUM_RPC isn't set so CI without archive access still
// runs the unit tests above.
contract V3FlashMiniForkTest is Test {
    V3FlashMini mini;

    // Well-known Arbitrum pools (WETH/USDC 500 bps and 3000 bps)
    address constant WETH_USDC_500 = 0xC6962004f452bE9203591991D15f6b388e09E8D0;
    address constant WETH_USDC_3000 = 0x17c14D2c404D167802b16C450d3c99F88F2c4F4d;
    address constant WETH = 0x82aF49447D8a07e3bd95BD0d56f35241523fBab1;
    address constant USDC = 0xaf88d065e77c8cC2239327C5EDb3A432268e5831;

    function setUp() public {
        // Skip fork tests unless ARBITRUM_RPC is set.
        try vm.envString("ARBITRUM_RPC") returns (string memory rpc) {
            vm.createSelectFork(rpc);
            mini = new V3FlashMini();
        } catch {
            vm.skip(true);
        }
    }

    // This test just verifies the contract deploys onto a fork and the
    // owner accessor works. Real end-to-end cycle tests need a historical
    // block where a profitable WETH→USDC→WETH arb existed, which is
    // outside the scope of unit testing.
    function test_DeploysOnFork() public view {
        assertEq(mini.owner(), address(this));
    }
}
