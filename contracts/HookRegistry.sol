// SPDX-License-Identifier: UNLICENSED
pragma solidity ^0.8.20;

/// @title HookRegistry
/// @notice Single source of truth for V4 hook whitelisting. V4Mini and
///   MixedV3V4Executor both query `isAllowed(hook)` instead of maintaining
///   their own per-contract whitelists. Lets the off-chain classifier
///   auto-promote safe hooks (no `*ReturnDelta` permission bits) without a
///   per-executor migration, and keeps unsafe hooks reject-by-default.
contract HookRegistry {
    address public immutable owner;

    enum Status { Unknown, Allowed, Rejected }

    mapping(address => Status) private _status;
    mapping(address => bytes32) public bytecodeHash;
    mapping(address => uint16)  public permissionBits;
    mapping(address => string)  public classification;

    event HookSet(address indexed hook, Status status, uint16 permissions, bytes32 bytecodeHash, string classification);

    constructor() {
        owner = msg.sender;
    }

    /// @notice Auto-whitelist path: called by the off-chain sync loop once a
    ///   hook has been classified. Only a hook with no `*ReturnDelta` bit
    ///   should ever be passed with `allow=true` by the automated path.
    function setHook(
        address hook,
        bool allow,
        uint16 permissions,
        bytes32 codeHash,
        string calldata classificationLabel
    ) external {
        require(msg.sender == owner, "owner");
        Status s = allow ? Status.Allowed : Status.Rejected;
        _status[hook] = s;
        permissionBits[hook] = permissions;
        bytecodeHash[hook]   = codeHash;
        classification[hook] = classificationLabel;
        emit HookSet(hook, s, permissions, codeHash, classificationLabel);
    }

    /// @notice Explicit human-approved whitelist entry for a hook that has
    ///   delta-rewriting permissions. Reviewer must manually call this after
    ///   auditing source. Separate from setHook so the audit trail is clear.
    function approveDeltaHook(
        address hook,
        uint16 permissions,
        bytes32 codeHash,
        string calldata reviewerNote
    ) external {
        require(msg.sender == owner, "owner");
        _status[hook] = Status.Allowed;
        permissionBits[hook] = permissions;
        bytecodeHash[hook]   = codeHash;
        classification[hook] = reviewerNote;
        emit HookSet(hook, Status.Allowed, permissions, codeHash, reviewerNote);
    }

    /// @notice isAllowed is the sole gate for V4Mini/MixedV3V4Executor. The
    ///   zero-address (no hook) always returns true for convenience.
    function isAllowed(address hook) external view returns (bool) {
        if (hook == address(0)) return true;
        return _status[hook] == Status.Allowed;
    }

    function statusOf(address hook) external view returns (Status) {
        if (hook == address(0)) return Status.Allowed;
        return _status[hook];
    }
}
