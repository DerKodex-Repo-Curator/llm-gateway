---
title: dedicated usage sink
state: inbox
created: 2026-10-13
tags: [feature]
source: per-key-metrics close-out
---

Give per-request usage records a dedicated destination when filtering the ordinary log stream by `channel=usage` does not meet an operator's needs. Evaluate a separate file, HTTP push, or OpenTelemetry traces while retaining best-effort behavior that cannot fail a completion.

## Discussion

[Usage records](../../current/metrics.md#usage-records) currently share the gateway's ordinary log output. A separate sink was deferred until an operator needs more than channel filtering; key and run attribution should keep the same field names across destinations.
