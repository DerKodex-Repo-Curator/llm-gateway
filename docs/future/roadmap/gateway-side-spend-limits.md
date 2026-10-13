---
title: gateway-side spend limits
state: horizon
created: 2026-07-24
tags: [feature]
milestone: v0.1.x
---

Enforce a per-key spend ceiling at the gateway, so an over-budget key is capped here rather than by each client harness policing its own spend.

## Discussion

Builds on the [per-key token metrics and usage records](../../current/metrics.md) now available on both completion paths. A dollar ceiling additionally needs per-model prices the gateway doesn't carry today, so a token-count ceiling (per key, per window) is the simpler first cut. Enforcing at the gateway gives one locus instead of every client policing its own spend; the request path already lets the enforcement point migrate without a client-facing contract change.

Cache-aware accounting remains open: Anthropic's `input_tokens` excludes cache creation and cache read tokens, while OpenAI reports cached tokens separately. The current domain `Usage` carries neither; richer usage belongs here because cached tokens have different prices. Token estimation for silent backends also belongs with enforcement: current records carry `usage_reported=false` rather than a tokenizer or word-count guess, and spend limits need an explicit decision about whether and how to enforce estimates.

A spend ledger, totals over a window, and a query endpoint such as "what has this key used this month" still need to be scoped here or in a management plane. The current gateway emits individual records and cumulative metric counters; it keeps no run ledger or windowed spend state.
