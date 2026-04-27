// SPDX-License-Identifier: MIT
pragma solidity ^0.8.24;

/// @notice Ramses V3 CL SwapRouter — uses exactInput (path-encoded) not exactInputSingle.
interface IRamsesV3Router {
    struct ExactInputParams {
        bytes   path;        // abi.encodePacked(tokenIn, fee, tokenOut)
        address recipient;
        uint256 deadline;
        uint256 amountIn;
        uint256 amountOutMinimum;
    }
    function exactInput(ExactInputParams calldata params) external payable returns (uint256 amountOut);
}
