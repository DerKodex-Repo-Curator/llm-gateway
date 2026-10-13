# Per-Key Metrics

The gateway already knows who is asking. Every request that clears the bearer-token middleware carries a key record, and the record's `name` is the attribution identity — it is the `key` label on the request counter and the duration histogram today. What the gateway does not yet know, per key, is what each request *cost*: the token counters carry provider and model but no key, and the streaming path meters no tokens at all. An operator looking at a fleet of agents can see which one is chatty and which one is slow, but not which one is expensive.

And a key is not a run. Keys belong to agents, one agent runs many times, and runs on the same key overlap. A counter has already summed those runs together by the time anyone scrapes it, so no amount of per-key metering answers "what did this run consume." That question needs a per-request record carrying an identity only the caller can supply.

This spec delivers both: per-key token counters on both completion paths, and a per-request usage record that carries an optional caller-supplied run id. It is the observability half of a pair; [gateway-side spend limits](roadmap/gateway-side-spend-limits.md) is the enforcement half.

## Best Effort

Everything here is best-effort observability, and the design is sized to that. Usage data is measured where a backend reports it and absent where it doesn't. The gateway never refuses, delays, or alters a completion on behalf of metering: a malformed run id is ignored rather than rejected, a backend that reports nothing is recorded as reporting nothing, and a failed log write has no effect on the response. Counters and records will agree in practice because they are written at the same point, but that agreement is a property of the implementation, not a contract.

The reason to say this up front is that the pieces invite hardening. Each one — the run id, the record, the streaming assembly — could be made fail-loud, validated end to end, or reconciled. None of them is, deliberately, and a proposal to make one so is a proposal to change this posture rather than to fix a gap in it.

## The Organizing Idea

**Usage is a domain value, assembled where the wire is understood and metered where the request is understood.**

The non-streaming path already has this shape. A provider translates its upstream response into the gateway's `ChatCompletionResponse`, `Usage` included, and the handler reads that field and increments the counters. The provider knows the wire; the handler knows the key, the model binding, and the meters.

The streaming path has no equivalent because usage does not arrive in a tidy response. It arrives as fragments, differently per provider: Anthropic reports input tokens at the start of the stream and a cumulative output count at the end, in two different events; OpenAI-compatible backends report both in a trailing chunk that carries no choices, and only if the request asked for it; some backends never report at all. Today those fragments are parsed and discarded, or never requested, or never sent.

So the streaming path grows the same shape. A stream event may carry a `Usage`. Each provider's stream reader assembles its wire's fragments into a domain `Usage` and emits it once the wire has reported a usable total, as a running total the handler keeps the last of. The handler meters it at the same place, with the same attributes, as a non-streaming response. Providers never touch a meter; the handler never learns that Anthropic counts cumulatively.

## Asking Upstream

Metering must not depend on the client remembering to ask. On every streaming request to an OpenAI-compatible backend — OpenAI itself, and anything behind the local provider — the gateway asks for usage by setting `stream_options.include_usage` on the outbound request. Anthropic always reports; there is nothing to inject.

The client's stream does not change. Usage the gateway requested is consumed by the gateway: a usage-only chunk is not forwarded, and `usage` is stripped from any content chunk that carries it. A client that sent `stream_options` itself gets what it gets today, which is nothing — the gateway's request shape does not model the field, so it is dropped at decode. Passing usage through to a client that asks is a separate feature, deferred below.

## The Counters

The two token counters gain two labels, so their attribute set matches the request counter's:

| instrument | attributes |
|---|---|
| `llm_gateway.tokens.prompt` | `provider`, `model`, `streaming`, `key` |
| `llm_gateway.tokens.completion` | `provider`, `model`, `streaming`, `key` |

`key` is the record's `name`, spelled as the request counter spells it, empty when authentication is off. `streaming` costs no new cardinality — the request counter already carries it — and it is what makes a backend that silently under-reports on one path show up as a flat series next to a climbing one.

A request whose backend reported no usage increments no token counter. It still increments the request counter, as it does today.

## The Usage Record

Every dispatched chat completion writes one structured log record, at the same point and for the same requests that increment the request counter, whether or not metrics are enabled. Requests refused before dispatch — a malformed body, a denied model or route, an unroutable model — produce no record; they consumed nothing and are logged elsewhere already. A client that disconnects mid-stream still produces a record, carrying whatever usage the reader had already emitted.

| field | value |
|---|---|
| `key` | key record name; empty when authentication is off |
| `run` | caller-supplied run id; empty when absent or invalid |
| `provider` | `openai`, `anthropic`, `local` |
| `model` | the gateway's model binding |
| `streaming` | `true` or `false` |
| `usage_reported` | `true` or `false` |
| `prompt_tokens` | integer; omitted when usage was not reported |
| `completion_tokens` | integer; omitted when usage was not reported |
| `total_tokens` | integer; omitted when usage was not reported |
| `duration_s` | request duration in seconds |
| `completion_id` | the completion id the gateway sent; empty when none was produced |
| `error_type` | the error type the gateway wrote toward the client, e.g. `rate_limit_error`; empty on success |

The spellings are borrowed, not invented: dimensions from the metric labels, token counts from OpenAI's `usage` object, `error_type` from the error types the gateway already sends clients. An operator moving between a Prometheus query and a log query uses the same words for the same things.

The record describes what the gateway sent, not what the client received — a gateway cannot know whether a response reached a client that has gone away, and checking every write result to find out would put bookkeeping in the request path for a client that is already gone. A request whose client disconnected therefore carries the id and error type the gateway wrote, even though nothing arrived.

Token fields are omitted rather than zeroed when usage was not reported, so a pipeline summing a run's tokens sums only measured values. `error_type` is what separates a failed request from a backend that simply doesn't report. `key` and `run` are always present so a filter on either never has to handle a missing field.

The record is written on a `usage` log channel, interleaved with the gateway's ordinary logs, and carries `channel=usage` for filtering. It is JSON whenever the gateway's output is not a terminal — which is every real deployment — and `DL_USE_JSON=true` forces JSON locally.

## Run Attribution

A caller that wants per-run numbers sends one header on every request in the run:

```http
X-LLM-Gateway-Run: ci/build-4411:attempt-2
```

The gateway reads it, carries it to the record, and forgets it. It keeps no run state: it does not know when a run starts or ends, does not total anything, and does not check whether an id is unique or who is using it. Per-run totals are computed downstream by whatever reads the records.

A run id is valid when it matches:

```abnf
run = 1*128( ALPHA / DIGIT / "-" / "." / "_" / ":" / "/" )
```

That covers UUIDs, ULIDs, and namespaced ids, and it excludes whitespace, quotes, `=`, and control characters, so the value reads unambiguously in both the JSON and the text log formats. An id that fails the rule, an empty header, or the header sent more than once is treated as absent: the request proceeds normally and the record's `run` is empty. Nothing is logged at the point the value is dropped. A client with a broken header template finds out by seeing empty `run` fields, which is the right trade for best-effort attribution.

**A run is identified by `key` and `run` together, never by `run` alone.** The key is authenticated; the run id is whatever the key holder chose to send. Two keys can send the same run id, and a pipeline that groups by run alone will merge them — every record carries both fields so the correct grouping is the easy one.

The run id goes nowhere but the record. It is not a metric label, it does not appear on other log lines, and it is never sent upstream. One caller-supplied string, one destination.

## Scenarios

**An agent's key on Anthropic, streaming, inside a run.** The client sends `stream: true` and `X-LLM-Gateway-Run: 7f3c…`. The Anthropic reader sees `message_start` with `input_tokens: 412`, content deltas, `message_delta` with cumulative `output_tokens: 97`, then `message_stop`, and emits one usage event before done. The client receives exactly the stream it would have received before this work. The token counters gain 412 and 97 under the agent's key with `streaming=true`, and one record lands with `run=7f3c…`, `usage_reported=true`, the three token counts, and an empty `error_type`.

**Two runs on one key, concurrently.** Both agents' requests increment the same counter series, which is what the counters are for. Each request's record carries its own `run`, and summing records by `key` and `run` separates them.

**A key on a local vLLM.** The outbound request carries `stream_options.include_usage`. vLLM's trailing chunk arrives with `usage` and empty choices; the local reader turns it into a usage event and forwards nothing to the client for that chunk.

**A backend that puts usage on the final content chunk.** Some servers attach `usage` to the chunk carrying `finish_reason`. The reader takes the usage and forwards the chunk with `usage` removed.

**A key on an old Ollama.** The backend ignores the option and streams to `[DONE]` with no usage. No token counters move. The record has `usage_reported=false`, no token fields, and an empty `error_type` — a silent backend, distinguishable from a failure.

**A rate-limited request.** The provider returns a rate-limit error before streaming begins. The client gets the error it gets today. The record has `usage_reported=false` and `error_type=rate_limit_error`.

**A broken header template.** A client sends `X-LLM-Gateway-Run: ` with an unset variable. The completion succeeds; the record's `run` is empty.

## Seam Census

- **model / transport — separate.** `Usage` is a domain value on the stream event, assembled per provider from that provider's wire fragments. Wire knowledge stays in the providers; the handler sees only the domain value. The run id is request metadata and never enters a provider request. Revisit if a provider needs to report something the domain `Usage` cannot carry.
- **contract circumvention — the handler is the one metering point.** Providers never hold a meter or write a record. Both completion paths hand their result to one place in the handler that increments the counters and writes the record together. A provider that starts recording its own usage has reached around this; spend limits, when they arrive, hang off the same point.
- **error by tier — metering never fails a request.** A malformed usage payload surfaces through the existing stream-error path like any unparseable chunk, because it is a stream problem, not a metering one. Everything metering-specific — a missing usage value, an invalid run id, a failed log write — degrades to absence in the record and never reaches the client.

## Deferred (and Why)

**Streamed usage passthrough to clients.** A client that sets `stream_options.include_usage` could receive a synthesized usage chunk before `[DONE]`. It is a client-facing feature rather than observability, it brings its own wire-shape rules, and nobody has asked. Revisit when a client does.

**Estimating tokens for backends that do not report.** A tokenizer or word count would fill the gap for a silent backend, and put guesses into a series spend limits will later enforce against. `usage_reported=false` makes the gap visible instead.

**Cache-aware prompt accounting.** Anthropic's `input_tokens` excludes cache creation and cache read tokens, and OpenAI reports cached tokens separately. The domain `Usage` carries neither, on either path. This work keeps the paths at parity; a richer `Usage` belongs to spend limits, since cached tokens are priced differently.

**A dedicated usage sink.** A separate file, an HTTP push, or OpenTelemetry traces would give the record a home apart from ordinary logs. Revisit if an operator cannot separate the `usage` channel from the rest of the log stream in their pipeline.

**Usage on a stream that ends early.** A stream interrupted before its usage arrives is recorded as `usage_reported=false`, even where the backend had reported part of the picture — Anthropic's opening event carries an input count that the reader does not turn into a usage value on its own, and OpenAI's own documentation warns that an interrupted stream may never deliver its usage chunk. Recovering those partial counts would mean a second decode of the opening event and a rule for what a half-measured request means. Under best effort, an interrupted request reading as unmeasured is the honest answer.

**A backend that rejects the injected field.** A local backend that refuses unknown request fields would fail every streaming request once the gateway asks for usage. None of the backends the docs name is known to do this. If one appears, a per-provider way to turn injection off is the fix.

**The spend ledger and a usage query surface.** Totals over a window, a ceiling, an endpoint that answers "what has key X used this month" — all belong to the enforcement card or to a management plane. What this spec leaves for them is the single point every request's usage passes through, with its key and run attached.
