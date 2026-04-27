// SPDX-License-Identifier: MIT
pragma solidity ^0.8.24;

import "forge-std/Script.sol";
import "../V4Mini.sol";

// Deploy V4Mini to Arbitrum.
//
// Constants:
//   POOL_MANAGER  = 0x360E68faCcca8cA495c1B759Fd9EEe466db9FB32  (Uniswap V4 PoolManager on Arbitrum)
//   WETH          = 0x82aF49447D8a07e3bd95BD0d56f35241523fBab1  (WETH9 on Arbitrum)
//
// Usage (broadcast):
//   forge script script/DeployV4Mini.s.sol:DeployV4Mini \
//     --rpc-url $ARBITRUM_RPC \
//     --private-key $DEPLOYER_PK \
//     --broadcast --slow
//
// Dry-run (no broadcast):
//   forge script script/DeployV4Mini.s.sol:DeployV4Mini \
//     --rpc-url $ARBITRUM_RPC -vvv
contract DeployV4Mini is Script {
    address constant POOL_MANAGER = 0x360E68faCcca8cA495c1B759Fd9EEe466db9FB32;
    address constant WETH = 0x82aF49447D8a07e3bd95BD0d56f35241523fBab1;

    function run() external returns (address deployed) {
        uint256 pk = vm.envUint("DEPLOYER_PK");
        vm.startBroadcast(pk);
        V4Mini mini = new V4Mini(POOL_MANAGER, WETH);
        vm.stopBroadcast();
        deployed = address(mini);
        console.log("V4Mini deployed at:", deployed);
        console.log("owner:", mini.owner());
        console.log("poolManager:", address(mini.poolManager()));
        console.log("weth:", address(mini.weth()));
    }
}
