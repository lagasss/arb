// SPDX-License-Identifier: MIT
pragma solidity ^0.8.24;

/// @notice Algebra V1 router interface (used by Camelot V3, THENA, etc.)
/// Identical to UniV3 exactInputSingle but without the fee field —
/// Algebra pools store the fee internally.
interface IAlgebraRouter {
    struct ExactInputSingleParams {
        address tokenIn;
        address tokenOut;
        address recipient;
        uint256 deadline;
        uint256 amountIn;
        uint256 amountOutMinimum;
        uint160 limitSqrtPrice;
    }
    function exactInputSingle(ExactInputSingleParams calldata params) external returns (uint256 amountOut);
}
