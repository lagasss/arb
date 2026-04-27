---
name: Always add config parameters for tunable values
description: Every hardcoded tunable value must be exposed as a config parameter so the user can tweak without recompiling
type: feedback
---

When implementing any optimization, threshold, interval, or limit — always expose it as a config parameter in config.yaml under the appropriate section (e.g. `strategy:`), with a clear comment explaining what it does and what the default means.

**Why:** User wants to be able to see and tweak every metric/parameter to understand strategy behavior without recompiling.

**How to apply:** Before hardcoding any numeric constant in logic code, check if it belongs in Config. If it's tunable (thresholds, intervals, caps, fractions), add it to the Config struct and config.yaml with a default value and comment. Never leave strategy-relevant magic numbers in code.
