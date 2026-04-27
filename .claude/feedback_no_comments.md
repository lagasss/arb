---
name: Stop adding comments to code
description: User finds code comments misleading and unnecessary — don't add them, existing ones are often wrong
type: feedback
originSessionId: f9c1aecb-b07c-4ae0-8117-5d6b4ab5e5ee
---
Don't add comments to code. The user considers them misleading and noisy. Existing comments in the codebase have been wrong multiple times (e.g., the TicksUpdatedAt "intentional" comment that masked a critical bug).

**Why:** Comments drift from reality and create false confidence. The code is the source of truth.

**How to apply:** Write self-documenting code. No inline comments, no block comments explaining logic. Only add comments if the user explicitly asks for documentation.
