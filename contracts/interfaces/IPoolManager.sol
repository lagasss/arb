// SPDX-License-Identifier: MIT
pragma solidity ^0.8.24;

/// @notice Minimal Uniswap V4 PoolManager interface for executing swaps.
interface IPoolManager {
    struct PoolKey {
        address currency0;
        address currency1;
        uint24 fee;
        int24 tickSpacing;
        address hooks;
    }

    struct SwapParams {
        bool zeroForOne;
        int256 amountSpecified;   // negative = exactInput
        uint160 sqrtPriceLimitX96;
    }

    function unlock(bytes calldata data) external returns (bytes memory);

    // V4 returns a BalanceDelta (int256) packing: amount0 in the HIGH 128 bits,
    // amount1 in the LOW 128 bits. Declaring the return as `(int128, int128)`
    // produces a broken ABI decode — caller expects 64 bytes, chain returns 32
    // — so every V4 swap silently reverts. Callers must extract:
    //   int128 amount0 = int128(delta >> 128);
    //   int128 amount1 = int128(delta);
    function swap(PoolKey memory key, SwapParams memory params, bytes calldata hookData)
        external returns (int256 delta);

    function settle() external payable returns (uint256);
    function take(address currency, address to, uint256 amount) external;
    function sync(address currency) external;
}
