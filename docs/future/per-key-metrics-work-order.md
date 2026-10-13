# Per-Key Metrics — Work Order

This translates [`per-key-metrics.md`](per-key-metrics.md) into concrete code against this repo. The spec fixes the shape — usage as a domain value on the stream event, the gateway asking upstream and consuming what it asked for, two new labels on the token counters, a per-request usage record, and an optional run header — and holds all of it to a best-effort posture. This document adds the type changes, each provider's assembly rule, the handler's single metering point, the test seam, two stages, and the tests. It is sized to the posture: where a more defensive mechanism was available and the spec does not need it, it is not here.

Where the work order goes beyond the spec it says so in [Additions Beyond the Spec](#additions-beyond-the-spec).

## Types

All in `providers/`.

```go
// types.go
type StreamChunk struct {
    // ...existing fields...
    // Usage is decoded from an upstream usage-bearing chunk. readers consume it
    // and clear it before forwarding, so a forwarded chunk never carries it.
    Usage *Usage `json:"usage,omitempty"`
}
```

```go
// provider.go
type StreamEvent struct {
    Chunk *StreamChunk
    Usage *Usage // assembled by the provider; may repeat, last value wins
    Err   error
    Done  bool
}
```

`ChatCompletionRequest` does **not** gain a `StreamOptions` field. The injection only ever happens on the outbound body of a streaming request, so it is built there:

```go
// streaming.go
type streamOptions struct {
    IncludeUsage bool `json:"include_usage"`
}

// upstreamStreamRequest is the outbound body for an OpenAI-compatible
// streaming request: the client's request plus the gateway's usage ask.
type upstreamStreamRequest struct {
    *ChatCompletionRequest
    StreamOptions streamOptions `json:"stream_options"`
}
```

Verified: `encoding/json` promotes the embedded pointer's fields, so the wrapper marshals as the request with `"stream_options":{"include_usage":true}` appended, and a client-sent `stream_options` is still dropped at the handler's decode exactly as it is today.

Not modelling the field on the request is what keeps it off the non-streaming path. OpenAI's API reference documents `stream_options` as valid only with `stream: true`, and a field on the shared request type would be one forgotten reset away from sending it there.

## Providers

Each stream reader owns the translation from its wire's fragments to a domain `Usage`, emitted as `StreamEvent{Usage: u}` once the wire has reported a usable total. A reader may emit more than one, each a complete running total, and the handler keeps the last. A reader that saw no usage emits nothing.

### OpenAI and local (`openai.go`, `local.go`)

`ChatCompletionStream` in both marshals the wrapper instead of `req`:

```go
req.Stream = true
body, err := json.Marshal(upstreamStreamRequest{
    ChatCompletionRequest: req,
    StreamOptions:         streamOptions{IncludeUsage: true},
})
```

`req.Stream = true` is today's line, unchanged. `MultiLocal.ChatCompletionStream` delegates to `Local` and needs no change.

The two `readSSEStream` bodies are line-for-line identical today and stay that way. Per parsed chunk:

```go
chunk, err := parseStreamChunk(data)
if err != nil { events <- StreamEvent{Err: err}; return }
if chunk.Usage != nil {
    events <- StreamEvent{Usage: chunk.Usage}
    chunk.Usage = nil
    if len(chunk.Choices) == 0 {
        continue // a usage-only chunk exists only because the gateway asked
    }
}
events <- StreamEvent{Chunk: chunk}
```

OpenAI's reference sends `"usage": null` on every chunk but the last once usage is requested; that decodes to a nil pointer and changes nothing. A trailing usage-only chunk becomes a usage event and is not forwarded. A content chunk that also carries `usage` becomes a usage event and a forwarded chunk with `usage` cleared. Every other chunk is forwarded exactly as today, including one with no choices that never carried usage — only what the gateway asked for is withheld. `parseStreamChunk` does not change; its error-envelope probe still runs first.

### Anthropic (`anthropic.go`)

The data is already parsed and discarded. `message_start` carries `message.usage.input_tokens`. `message_delta` carries a `usage` object whose counts are **cumulative**, so the last value seen is the total. Anthropic's streaming documentation shows that object carrying `output_tokens` alone in the common case, and `input_tokens` as well when it has grown past the `message_start` value — with server tool use, one example reports 2,679 input tokens at start and 10,682 on the final delta. `anthropicStreamUsage` therefore gains `InputTokens int \`json:"input_tokens,omitempty"\``, and the delta's value wins when present.

**Usage is emitted on each `message_delta` that carries a `usage` object, immediately.** Because the counts are cumulative, each emission is a complete running total, and the handler's last-wins rule turns the sequence into the final value. Emitting immediately means a stream that errors or truncates after a delta still delivers the usage Anthropic already reported. Nothing is emitted from `message_start` alone: `anthropicResponse.Usage` is a value struct, so a missing usage object there decodes to zero input tokens rather than to absence, and emitting it would report measured zeros for a stream that reported nothing. `message_start` only contributes the starting input count. The consequence — a stream that dies before its first usage-bearing delta is recorded as unmeasured, discarding the opening input count — is accepted in the spec's deferred section rather than fixed by decoding that event's usage presence separately.

```go
var promptTokens int
// ...
case "message_start":
    if event.Message != nil {
        messageID = event.Message.ID
        promptTokens = event.Message.Usage.InputTokens
    }
case "message_delta":
    if event.Usage != nil {
        if event.Usage.InputTokens > 0 { promptTokens = event.Usage.InputTokens }
        events <- StreamEvent{Usage: &Usage{
            PromptTokens:     promptTokens,
            CompletionTokens: event.Usage.OutputTokens,
            TotalTokens:      promptTokens + event.Usage.OutputTokens,
        }}
    }
    // ...existing stop_reason handling, unchanged...
```

`message_stop` is unchanged.

**The non-streaming translation gains the same absence rule.** `anthropicResponse.Usage` is a value struct today, so a response carrying no usage object decodes to zeros and `translateResponse` builds a non-nil `Usage` from them — the handler then meters zeros and the record claims `usage_reported=true`. That is today's behavior in the token counters already; the record would make it visible. `anthropicResponse.Usage` becomes `*anthropicUsage`, `translateResponse` leaves `ChatCompletionResponse.Usage` nil when it is absent, and the `message_start` read becomes a nil check. Nothing about the settled streaming rule changes: `message_start` still contributes only the starting input count.

`message_start`'s placeholder `output_tokens` is not read. An `error` event after a delta leaves the already-emitted usage in place; an error before any delta emits none. Cache token fields appear on both events and are not read; the spec defers them.

## The Handler

`gateway/handler.go`. The handler today increments the request counter and histogram in a block at the bottom of `handleChatCompletions`, and the token counters separately inside `handleNonStreamingCompletion`. Both move to one place that also writes the record.

### Run id

Read once at the top of `handleChatCompletions`:

```go
const runHeader = "X-LLM-Gateway-Run"

var runGrammar = regexp.MustCompile(`^[A-Za-z0-9._:/-]{1,128}$`)

// runID returns the caller's run id, or "" when the header is absent, sent
// more than once, empty, or outside the grammar. it never refuses a request.
func runID(r *http.Request) string {
    values := r.Header.Values(runHeader)
    if len(values) != 1 || !runGrammar.MatchString(values[0]) {
        return ""
    }
    return values[0]
}
```

`Header.Values` canonicalizes the name, and a single header line containing a comma is one value that fails the grammar, so a folded duplicate is dropped too.

### Completion results

Both completion paths return what the metering point needs:

```go
type completionResult struct {
    usage     *providers.Usage
    id        string // completion id the client received; "" if none
    errorType string // error type written to the client; "" on success
}
```

**`writeProviderError` returns the type it wrote to the client**: `apiErr.Type` for an API error, and `providers.ErrorTypeServer` for a non-API error, since that path writes `providers.ErrProviderError`. **An empty `apiErr.Type` is normalized to `providers.ErrorTypeServer` before the error is written**, so the client and the record agree. Both OpenAI-compatible parsers return the upstream envelope verbatim when it carries a message, and an envelope that omits `type` yields an empty one; left alone it would reach the record as an empty `error_type`, which the spec reads as success — a failed request would be indistinguishable from a silent backend. The normalization also replaces a blank `type` in the client's error body with `server_error`, which is a small client-visible change and belongs in the changelog entry. The error counter keeps its existing `unknown` label for the non-API case, so the record and the counter differ there by design — the record describes what the client received. Its two existing callers take the return value.

**`handleNonStreamingCompletion`** loses its `providerType` parameter and its inline token-metering block, and returns `completionResult{usage: resp.Usage, id: resp.ID}` on success or `completionResult{errorType: g.writeProviderError(w, err)}` on failure.

**`handleStreamingCompletion`** returns a `completionResult` built as the loop runs:

```go
var result completionResult
for event := range events {
    if event.Err != nil {
        // ...existing log + sse.WriteError(providers.ErrProviderError(...)), unchanged...
        result.errorType = providers.ErrorTypeServer
        return result
    }
    if event.Usage != nil {
        result.usage = event.Usage
        continue
    }
    if event.Done {
        sse.WriteDone()
        return result
    }
    if event.Chunk != nil {
        if result.id == "" { result.id = event.Chunk.ID } // first id wins; a stream has one identity
        // ...existing model-binding rewrite and WriteChunk, unchanged;
        //    a write failure returns result as it stands...
    }
}
// ...existing truncation log + sse.WriteError, unchanged...
result.errorType = providers.ErrorTypeServer
return result
```

A stream's identity is established by the first chunk that carries an id and is never overwritten. Later chunks may carry none — the readers forward choices-less chunks that never held usage, and such a chunk can omit the id, which would otherwise blank the record's `completion_id` on an otherwise healthy completion.

The two pre-stream failure paths set `errorType` the same way: `providers.ErrorTypeServer` when the writer cannot flush, and `g.writeProviderError`'s return when the provider refuses to open the stream.

The mid-stream and truncation paths record `server_error` because that is what the handler writes to the client there, whatever type the upstream error carried. A client that disconnects mid-stream produces a result carrying whatever usage had arrived, plus whatever the handler went on to write toward it — a failed chunk write returns the result as it stands, while a read error on the upstream side still records `server_error`. Neither path checks whether the client received anything.

### The metering point

The bottom of `handleChatCompletions` becomes:

```go
var result completionResult
if req.Stream {
    result = g.handleStreamingCompletion(ctx, w, provider, &req)
} else {
    result = g.handleNonStreamingCompletion(ctx, w, provider, &req)
}
g.meterCompletion(ctx, usageRecord{
    key: keyName, run: run, provider: string(providerType), model: req.Model,
    streaming: req.Stream, duration: time.Since(start),
}, result)
```

```go
// meterCompletion is the single place a dispatched completion is measured:
// the counters when metrics are enabled, and the usage record always.
func (g *Gateway) meterCompletion(ctx context.Context, rec usageRecord, result completionResult) {
    rec.result = result
    if g.meters != nil {
        // requests (+provider, model, streaming, key) and requestDuration
        // (+provider, model, key): labels unchanged from today.
        // when result.usage != nil, tokensPrompt and tokensCompletion with
        // provider, model, streaming, key.
    }
    b := dl.ChannelLog("usage")
    for _, f := range rec.fields() {
        b = b.With(f.key, f.value)
    }
    b.Info("usage")
}
```

`keyName` is already computed at the bottom of the handler today; it moves up so both the record and the counters use one value.

**`usageRecord.fields()` is a pure function** returning the spec's field list in order, and is what the record test checks. It omits the three token fields when `result.usage` is nil, and renders `duration_s` as `duration.Seconds()`.

**The `usage` channel is used unconfigured, deliberately.** `dl.ChannelLog` on an unconfigured channel returns the default logger with a `channel` attribute attached. A *configured* channel gets its own logger, and in JSON mode that logger is built without the channel attribute — so configuring it would remove the `channel=usage` field in exactly the format pipelines read. Leave a comment at the call site saying so.

`dl` writes through `slog` and swallows handler errors, so a failed log write cannot reach the response. Nothing further is needed for the spec's never-affects-the-response rule.

**Filenames.** `usageRecord`, `runID`, `completionResult`, and `meterCompletion` are small. terminus-canon's `camelcase-file-naming` decides whether they stay in `handler.go` or move to a `usageRecord.go`; decide against the canon at the time.

## Metrics Test Seam

`gateway/metrics.go`'s `initMetrics` constructs the Prometheus exporter and the instruments together, so handler tests cannot read the gateway's counters back. Split it the way `keys/metrics.go` already is:

```go
func initMetrics() (*meters, http.Handler, error)   // exporter + provider, then newMeters
func newMeters(meter metric.Meter) (*meters, error)  // today's instrument construction, verbatim
```

Handler tests build `g.meters` from a `sdkmetric.NewManualReader()`-backed provider, following `keys/metrics_test.go`. No production behavior changes.

## Documentation

- `docs/current/metrics.md` — the Token Metrics tables gain `streaming` and `key`; the sentence "Streaming responses typically do not include token counts" is replaced with the real behavior; a new Usage Records section carries the field table, the `usage` channel and JSON rule, the run header and its grammar, the key-plus-run grouping rule, and one paragraph stating the best-effort posture; one Useful Query for tokens per key.
- `docs/current/streaming.md` — Request Flow gains the outbound `stream_options` injection and the fact that usage chunks are consumed, not forwarded; the Anthropic table gains `message_start` and `message_delta` usage rows; one sentence on the accepted residual that a local backend rejecting unknown request fields would fail streaming requests.
- `docs/current/api-keys.md` — the sentence naming `name` as the attribution identity in request metrics widens to token metrics and usage records.
- `CHANGELOG.md` under `## Unreleased`: one FEATURE for per-key token metrics covering streaming, one FEATURE for the per-request usage record and the run header. The second entry names the error-type normalization: an upstream error envelope that omits `type` now reaches the client as `server_error` rather than with an empty type.

## Stages

**Stage 1 — providers.** The two type fields, the request wrapper, both OpenAI-compatible readers, the Anthropic accumulator, and their tests. The handler's stream loop already ignores an event with no chunk, error, or done, so usage events flow and are dropped until stage 2 — the tree stays correct and the stage is reviewable alone.

**Stage 2 — handler, docs, close-out.** `newMeters`, run id, completion results, the metering point and record, handler tests, documentation, changelog. Close-out as the pipeline prescribes: the roadmap card is deleted and the spec and work order leave `docs/future/`. Deferred items still live afterward are re-synthesized first — cache accounting and estimation into the spend-limits card's Discussion, passthrough and the dedicated sink as `inbox` roadmap cards, and the injection-rejecting backend into `streaming.md` as the accepted residual above.

## Tests

**`providers/`**
- **OpenAI and local readers, one shared table** driving `readSSEStream`: trailing usage-only chunk yields one `Usage` event and no `Chunk` for it; usage on the final content chunk yields a `Usage` event and a `Chunk` with `Usage == nil`; a chunk with no choices and no usage is still forwarded as a `Chunk`; no usage yields no `Usage` event and `Done` still arrives.
- **Injection:** against an `httptest.Server`, the streaming request body carries `"stream_options":{"include_usage":true}` and the non-streaming request body carries no `stream_options`, once each for OpenAI and local.
- **Anthropic, non-streaming:** a response with no `usage` object yields a nil `Usage` on the translated response.
- **Anthropic, one table:** `message_start` input plus two cumulative `message_delta` outputs yield two `Usage` events, the last carrying the final output value and the correct total; a `message_delta` carrying a larger `input_tokens` replaces the start value; a stream whose events carry no `usage` objects yields no `Usage` event; an `error` event after a usage-bearing delta still delivers that `Usage` event before the `Err`.

**`gateway/handler_test.go`** (extend `stubStreamProvider` to replay `Usage` events; a manual-reader `Gateway`; a key store so `key` is populated)
- **Streaming with usage:** token counters carry the key and `streaming=true`; the client body contains no `usage`; a content chunk followed by a choices-less chunk with no id records the first chunk's id. The stub replays two `Usage` events with different totals, and the counters and the record both carry the second — an implementation that summed them or kept the first would otherwise pass.
- **No usage:** no token counter points; the result has `usage == nil` and empty `errorType`.
- **Non-streaming:** token counters carry the key and `streaming=false`, from the single metering point.
- **Run header, table:** a valid value is returned by `runID`; missing, empty, over-length, out-of-grammar, and duplicated values all return `""`; a request carrying an invalid value still completes.
- **Record fields:** `usageRecord.fields()` produces the spec's order and names, omits token fields when usage is nil, and carries `error_type`.
- **Error attribution, small table:** a provider API error records that error's own type, an upstream envelope carrying a message but no `type` records `server_error` in both the record and the client's error body, and a mid-stream error or truncation records `server_error` — all through the real completion paths rather than the field builder.
- **Record emission:** with `g.meters == nil`, one dispatched request through the handler writes exactly one record to the log, carrying `channel=usage`, the request's `run`, and token fields only when usage was reported. Capture output with the package's existing `captureGatewayLogs` pattern (`gateway/access_test.go`), switched to JSON mode — JSON is where a configured `usage` channel would drop the `channel` field, so that is the mode that pins the unconfigured-channel rule.

Full suite under `-race`, `go vet`, and `unfurl -i` on touched docs, each stage.

## Additions Beyond the Spec

- **An outbound wrapper type instead of a request field.** The spec says the gateway asks upstream; the wrapper is how, and it keeps `stream_options` structurally off the non-streaming path.
- **`writeProviderError` returns the type it wrote to the client,** so `error_type` always matches the client's error. It equals the error counter's label except for non-API errors, where the counter says `unknown` and the record says `server_error`.
- **Mid-stream and truncation errors record `server_error`,** the type the handler writes toward the client, not the upstream's own type. Write results are not checked before recording, per the spec: the record says what the gateway sent.

## Critical Files

| file | change |
|---|---|
| `providers/types.go` | `StreamChunk.Usage` |
| `providers/provider.go` | `StreamEvent.Usage` |
| `providers/streaming.go` | `streamOptions`, `upstreamStreamRequest` |
| `providers/openai.go`, `providers/local.go` | marshal the wrapper; usage rule in `readSSEStream` |
| `providers/anthropic.go` | `anthropicStreamUsage.InputTokens`; per-delta usage emission; `anthropicResponse.Usage` becomes a pointer so absence survives translation |
| `gateway/handler.go` | `runID`; `completionResult`; both paths return it; `writeProviderError` returns its type; `meterCompletion` and `usageRecord` |
| `gateway/metrics.go` | `newMeters` split from `initMetrics` |
| `docs/current/metrics.md`, `streaming.md`, `api-keys.md` | as listed under Documentation |
| `CHANGELOG.md` | two FEATURE entries |
| `docs/future/roadmap/per-key-metrics.md` | deleted at close-out |
| `docs/future/roadmap/gateway-side-spend-limits.md` | Discussion gains cache accounting and estimation at close-out |
