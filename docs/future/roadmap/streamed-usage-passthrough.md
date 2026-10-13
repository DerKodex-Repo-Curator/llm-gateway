---
title: streamed usage passthrough
state: inbox
created: 2026-10-13
tags: [feature]
source: per-key-metrics close-out
---

Let a client that sends `stream_options.include_usage` receive a synthesized usage chunk before `[DONE]`. Define the client-facing wire shape and absent-usage behavior while preserving the gateway's independent usage collection.

## Discussion

The gateway currently drops client-sent `stream_options` during decoding and consumes all streaming usage for [token metrics and usage records](../../current/metrics.md), so clients receive no usage in the stream. Passthrough is a separate client feature; it was deferred because no caller had requested it.
