---
id: JK-7QK3M
title: Incremental compile keeps a deleted class in a merging processor's output
status: in_progress
priority: P1
kind: bug
labels:
  - cache
  - compile
assignee: agent/claude
owner: bsant
claim:
  by: agent/claude
  session: 01JC4N7Q2V8X
  at: 2026-11-07T12:00:00Z
parent: JK-3F9KA
blocked_by:
  - JK-K2M7P
links:
  - jk://compiler/IncrementalCompile.java@3f2a9c1#L42
  - jk://compiler/Cache.java#L10-L20
  - https://github.com/example/jk/pull/912
acceptance:
  - text: A deleted class is absent after incremental compile
    done: true
  - text: "The cache key covers generated output: no stale restores"
    done: false
fields:
  areas:
    - compiler
    - cache
  estimate: M
  points: 3
  reviewed: false
  tracking: https://example.com/t/1
created: 2026-11-07T12:00:00Z
updated: 2026-11-07T12:00:00Z
---

## Problem

Helidon's processor merges into the existing file.

```yaml
## not a heading
- [ ]
```

## Approach

Snapshot and diff.
