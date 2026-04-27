// SPDX-License-Identifier: MIT
pragma solidity ^0.8.24;

/// @notice Camelot V2 router — extends UniswapV2 with a `referrer` address parameter.
interface ICamelotV2Router {
    function swapExactTokensForTokens(
        uint256 amountIn,
        uint256 amountOutMin,
        address[] calldata path,
        address to,
        address referrer,
        uint256 deadline
    ) external returns (uint256[] memory amounts);
}
