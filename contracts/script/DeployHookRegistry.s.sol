// SPDX-License-Identifier: MIT
pragma solidity ^0.8.24;

import "forge-std/Script.sol";
import "../HookRegistry.sol";

// Deploys HookRegistry only. Wiring it into V4Mini + MixedV3V4Executor
// requires those contracts to expose setHookRegistry — which the currently
// deployed bytecode (deployed 2026-04-19) does NOT. Redeploy V4Mini and
// MixedV3V4Executor with the updated source before calling setHookRegistry.
//
// Usage (broadcast):
//   forge script script/DeployHookRegistry.s.sol:DeployHookRegistry \
//     --rpc-url $ARBITRUM_RPC \
//     --private-key $DEPLOYER_PK \
//     --broadcast --slow
contract DeployHookRegistry is Script {
    function run() external returns (address deployed) {
        uint256 pk = vm.envUint("DEPLOYER_PK");
        vm.startBroadcast(pk);
        HookRegistry registry = new HookRegistry();
        vm.stopBroadcast();
        deployed = address(registry);
        console.log("HookRegistry deployed at:", deployed);
        console.log("owner:", registry.owner());
    }
}
