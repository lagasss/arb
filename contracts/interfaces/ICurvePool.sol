// SPDX-License-Identifier: MIT
pragma solidity ^0.8.24;

interface ICurvePool {
    /// @notice Swap dx of coin i for coin j.
    /// @param i      Index of the input coin (0 or 1 for 2-pools)
    /// @param j      Index of the output coin
    /// @param dx     Amount of coin i to send
    /// @param min_dy Minimum amount of coin j to receive (slippage guard)
    /// @return       Amount of coin j received
    function exchange(int128 i, int128 j, uint256 dx, uint256 min_dy) external returns (uint256);
}
