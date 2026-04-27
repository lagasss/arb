// SPDX-License-Identifier: MIT
pragma solidity ^0.8.24;

/// @notice Camelot V2 router. Camelot extends UniswapV2 with a `referrer` parameter
///         AND DEPRECATES the regular `swapExactTokensForTokens` variant — calling
///         it on Arbitrum reverts with empty data ("CamelotRouter: METHOD_DEPRECATED").
///         All swaps must go through the SupportingFeeOnTransferTokens variant, which
///         returns void; the caller must compute amountOut via balance diff.
///
///         See _swapCamelotV2 in ArbitrageExecutor.sol for the balance-diff pattern.
interface ICamelotV2Router {
    function swapExactTokensForTokensSupportingFeeOnTransferTokens(
        uint256 amountIn,
        uint256 amountOutMin,
        address[] calldata path,
        address to,
        address referrer,
        uint256 deadline
    ) external;
}
