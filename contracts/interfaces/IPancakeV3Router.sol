// SPDX-License-Identifier: MIT
pragma solidity ^0.8.24;

/// @title IPancakeV3Router
/// @notice PancakeSwap V3 SwapRouter on Arbitrum: 0x32226588378236Fd0c7C4053999F88aC0e5cAC77
/// @dev PancakeV3 forked Uniswap V3 but **removed the `deadline` field** from
///      ExactInputSingleParams. The function selector is therefore different
///      from Uniswap V3 (0x04e45aaf vs 0x414bf389) and the calldata layout has
///      one fewer 32-byte slot.
///
///      Calling Pancake's router with the Uniswap V3 layout silently fails the
///      `try` (selector mismatch) and the parent contract surfaces a "swap
///      reverted" error. This dedicated interface fixes that.
interface IPancakeV3Router {
    struct ExactInputSingleParams {
        address tokenIn;
        address tokenOut;
        uint24  fee;
        address recipient;
        uint256 amountIn;
        uint256 amountOutMinimum;
        uint160 sqrtPriceLimitX96;
    }

    function exactInputSingle(ExactInputSingleParams calldata params)
        external
        payable
        returns (uint256 amountOut);
}
