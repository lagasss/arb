// SPDX-License-Identifier: MIT
pragma solidity ^0.8.24;

import "forge-std/Script.sol";
import "../ArbitrageExecutor.sol";

// Deploy ArbitrageExecutor to Arbitrum.
//
// Usage:
//   DEPLOYER_PK=$ARB_BOT_PRIVKEY forge script script/DeployArbitrageExecutor.s.sol:DeployArbitrageExecutor \
//     --rpc-url $ARBITRUM_RPC \
//     --broadcast \
//     --slow
contract DeployArbitrageExecutor is Script {
    address constant BALANCER_VAULT = 0xBA12222222228d8Ba445958a75a0704d566BF2C8;
    address constant AAVE_V3_POOL   = 0x794a61358D6845594F94dc1DB02A252b5b4814aD;

    function run() external returns (address deployed) {
        uint256 pk = vm.envUint("DEPLOYER_PK");
        vm.startBroadcast(pk);
        ArbitrageExecutor exec = new ArbitrageExecutor(BALANCER_VAULT);
        exec.setAavePool(AAVE_V3_POOL);
        vm.stopBroadcast();
        deployed = address(exec);
        console.log("ArbitrageExecutor deployed at:", deployed);
        console.log("owner:", exec.owner());
    }
}
