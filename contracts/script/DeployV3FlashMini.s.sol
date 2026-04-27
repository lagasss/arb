// SPDX-License-Identifier: MIT
pragma solidity ^0.8.24;

import "forge-std/Script.sol";
import "../V3FlashMini.sol";

// Deploy V3FlashMini to Arbitrum.
//
// Usage:
//   forge script script/DeployV3FlashMini.s.sol:DeployV3FlashMini \
//     --rpc-url $ARBITRUM_RPC \
//     --private-key $ARB_BOT_PRIVKEY \
//     --broadcast \
//     --slow
//
// Dry-run (no broadcast):
//   forge script script/DeployV3FlashMini.s.sol:DeployV3FlashMini \
//     --rpc-url $ARBITRUM_RPC -vvv
contract DeployV3FlashMini is Script {
    function run() external returns (address deployed) {
        uint256 pk = vm.envUint("DEPLOYER_PK");
        vm.startBroadcast(pk);
        V3FlashMini mini = new V3FlashMini();
        vm.stopBroadcast();
        deployed = address(mini);
        console.log("V3FlashMini deployed at:", deployed);
        console.log("owner:", mini.owner());
    }
}
