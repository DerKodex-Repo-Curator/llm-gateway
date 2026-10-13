---
title: providers wire layer onto dd
state: horizon
created: 2026-10-13
tags: [enhancement, epic, spike]
milestone: v0.1.x
source: terminus df-binding, llm-gateway stage 1 review
---

Move the `providers` package off `encoding/json` and `json` struct tags onto `dd`, which is the repo's marshaling substrate and what `golang-conventions/df-binding` requires. The package has been outside that rule since the first release; terminus flags it on every new field. The obstacle is not effort but fidelity: `dd` is stricter than `encoding/json` in several independent ways, and this is a client-facing wire contract that must keep accepting and emitting exactly what it does today.

## Discussion

Scope: 134 `json` tags (62 in `types.go`, 56 in `anthropic.go`, 16 elsewhere), ~30 encode/decode call sites across `providers`, the handler, `cmd/dummy-model`, and the routing clients. `cmd/dummy-keys` stays as-is — it marshals plain maps on purpose, inside the quality's carve-out.

`df` v1.0.6 cleared the two original blockers: `+opaque` binds an empty-interface field (the four polymorphic fields — message `content`, `stop`, `tool_choice`, tool `parameters`), and `+nullable` emits an explicit null for a nil pointer (intermediate chunks must carry `"finish_reason": null`). `dd.UnbindJSONL` supplies the compact single-line output SSE framing needs.

What remains, all verified against v1.0.6, is where the real work is:

- **Null tolerance has to be audited field by field, not derived from Go types.** A null into a plain string or slice fails in both strict and forgiving mode, where `encoding/json` accepts it and leaves the zero value. OpenAI sends `system_fingerprint: null`; Anthropic's `message_start` carries `stop_reason: null` on every stream, against a non-pointer `string` field. `+nullable` works on non-pointer scalars and slices, so the fix is a tag on every field a peer may null — an inventory over all 134, keyed on accepted wire values.
- **`dd`'s forgiving mode coerces types.** `"12"` binds to an int, `3.7` truncates to `3`, `"true"` binds to a bool; `encoding/json` rejects all three. A fractional token count would land a wrong number in the usage counters. `dd.Strict()` refuses coercion but rejects unknown input keys, which an external API supplies constantly — a `+extra` map per struct opts out of that while keeping the rest of strict mode, the pattern `keys` already uses. Captured extras are re-emitted on unbind, so every bind-then-unbind boundary has to clear them or the gateway starts forwarding fields it drops today.
- **Strict intake narrows acceptance three more ways:** duplicate object keys are rejected (today the last wins), keys match case-sensitively (`encoding/json` is case-insensitive, so `MODEL` currently binds), and trailing data after the document is rejected. Each is arguably an improvement and each is a client-visible change.
- **One combination the tag system cannot express.** A streaming `delta` needs all three of: accepts an incoming null, omitted when nil, emitted as `{}` when present and empty. `+nullable` gives null-on-nil, `+omitempty` drops the empty object, and untagged rejects the null. **This wants a `df` change — separating `+nullable`'s intake half from its emission half — and that card should land first.**
- **Anthropic's `anthropicContentBlock.Input` changes type, not tags.** It is a `json.RawMessage`, which `dd` reads as a byte slice: inbound tool-use objects fail to bind and outbound ones emit a byte array. A `map[string]any` with `+opaque` is the shape, but it rejects a valid non-object `arguments` value that passes through today, introduces a parse error `translateRequest` has nowhere to return, and canonicalizes whitespace inside the `arguments` string the client receives.

This was swept into the per-key-metrics work order as a third stage on 2026-10-13 and pulled back out the same day: two mercurius rounds produced eight wire-level findings against the conversion plan, none touching the metering work it was riding along with. It wants its own arc, its own spec-level nullability inventory, and the `df` prerequisite first. Per-key-metrics ships with `json` tags on its three new fields, and terminus's `df-binding` finding on that stage is deferred here rather than vetoed — the quality is right.
