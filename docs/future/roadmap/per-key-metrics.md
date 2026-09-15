---
title: per-key metrics
state: building
created: 2026-07-24
tags: [feature, spike]
milestone: v0.1.x
---

Give operators a per-agent view of gateway usage — keyed by the `sk-gw-*` API key on each request:

- request volume
- token usage (prompt + completion)
- model usage (which models it calls)

## Discussion

Request volume and model usage are already keyed — the `requests` meter carries `key` and `model` labels. The real gap is token usage: the token counters aren't keyed, and the streaming path meters no usage at all. The spike is that streaming case, where usage doesn't arrive in a tidy response and has to be assembled as chunks flow. The same per-key token measurement is what gateway-side-spend-limits would enforce a spend ceiling against — this card is the observability half.
