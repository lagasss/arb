// SPDX-License-Identifier: MIT
pragma solidity ^0.8.24;

import "forge-std/Script.sol";
import "../MixedV3V4Executor.sol";

// Deploy MixedV3V4Executor to Arbitrum.
//
// Constants:
//   POOL_MANAGER  = 0x360E68faCcca8cA495c1B759Fd9EEe466db9FB32  (UniV4 PoolManager on Arbitrum)
//
// Usage (broadcast):
//   forge script script/DeployMixedV3V4Executor.s.sol:DeployMixedV3V4Executor \
//     --rpc-url $ARBITRUM_RPC \
//     --private-key $DEPLOYER_PK \
//     --broadcast --slow
//
// Dry-run:
//   forge script script/DeployMixedV3V4Executor.s.sol:DeployMixedV3V4Executor \
//     --rpc-url $ARBITRUM_RPC -vvv
contract DeployMixedV3V4Executor is Script {
    address constant POOL_MANAGER = 0x360E68faCcca8cA495c1B759Fd9EEe466db9FB32;

    function run() external returns (address deployed) {
        uint256 pk = vm.envUint("DEPLOYER_PK");
        vm.startBroadcast(pk);
        MixedV3V4Executor mix = new MixedV3V4Executor(POOL_MANAGER);
        vm.stopBroadcast();
        deployed = address(mix);
        console.log("MixedV3V4Executor deployed at:", deployed);
        console.log("owner:", mix.owner());
        console.log("poolManager:", address(mix.poolManager()));
    }
}
