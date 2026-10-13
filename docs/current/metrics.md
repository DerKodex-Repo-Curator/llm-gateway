# Metrics

The gateway exposes OpenTelemetry metrics via a Prometheus exporter. When enabled, metrics are available at `GET /metrics` in the standard Prometheus text format.

## Enabling Metrics

```yaml
metrics:
  enabled: true
```

## Instruments

All metric names are prefixed with `llm_gateway.`.

### Request Metrics

**`llm_gateway.requests`** (counter) -- total chat completion requests.

| Attribute | Values | Description |
|---|---|---|
| `provider` | `openai`, `anthropic`, `local` | which provider handled the request |
| `model` | model name | the model used |
| `streaming` | `true`, `false` | whether the request was streaming |
| `key` | key name or empty | the API key name (when [virtual API keys](api-keys.md) are enabled) |

**`llm_gateway.request.duration`** (histogram, seconds) -- end-to-end request duration including upstream provider latency.

| Attribute | Values | Description |
|---|---|---|
| `provider` | `openai`, `anthropic`, `local` | which provider handled the request |
| `model` | model name | the model used |
| `key` | key name or empty | the API key name (when [virtual API keys](api-keys.md) are enabled) |

**`llm_gateway.requests.inflight`** (up-down counter) -- number of requests currently being processed. Incremented when a request enters the handler, decremented when it completes. Useful for understanding concurrency and detecting request pileups.

### Token Metrics

**`llm_gateway.tokens.prompt`** (counter) -- total prompt (input) tokens across all requests.

| Attribute | Values | Description |
|---|---|---|
| `provider` | provider name | which provider reported the usage |
| `model` | model name | the model used |
| `streaming` | `true`, `false` | whether the request was streaming |
| `key` | key name or empty | the API key record's attribution name |

**`llm_gateway.tokens.completion`** (counter) -- total completion (output) tokens across all requests.

| Attribute | Values | Description |
|---|---|---|
| `provider` | provider name | which provider reported the usage |
| `model` | model name | the model used |
| `streaming` | `true`, `false` | whether the request was streaming |
| `key` | key name or empty | the API key record's attribution name |

Token metrics use the backend's reported usage on both streaming and non-streaming requests. OpenAI and local streaming requests ask for usage with `stream_options.include_usage`; the providers consume usage-bearing chunks without exposing usage in the client's stream. Anthropic combines the opening input count with cumulative counts from `message_delta`, retaining the latest reported total. Each dispatched request is measured once. When a backend reports no usage, no token counter is incremented.

### Routing Metrics

**`llm_gateway.routing.decisions`** (counter) -- semantic routing decisions, counted each time the router selects a model.

| Attribute | Values | Description |
|---|---|---|
| `method` | `explicit`, `heuristic`, `semantic`, `classifier`, `default` | which routing layer made the decision |

A high proportion of `default` decisions may indicate that thresholds are too strict or that route examples don't cover your traffic well.

### Error Metrics

**`llm_gateway.provider.errors`** (counter) -- errors returned by upstream providers.

| Attribute | Values | Description |
|---|---|---|
| `error_type` | `invalid_request_error`, `authentication_error`, `rate_limit_error`, `server_error`, `not_found_error`, `service_unavailable`, `unknown` | the error category |

### Health Metrics

**`llm_gateway.endpoint.healthy`** (up-down counter) -- per-endpoint health status for multi-endpoint mode. Value is 1 for healthy endpoints and 0 for unhealthy endpoints. The `endpoint` attribute identifies the endpoint by name.

### Key Source Metrics

**`llm_gateway.keys.source.staleness`** (observable gauge, seconds) -- age of each reloadable key source's last successful load. The `source` attribute identifies the configured source. Boot-resident `config` keys are omitted, as is an optional source that has never loaded successfully.

**`llm_gateway.keys.source.excluded`** (observable gauge) -- whether a reloadable key source is currently excluded by `max_staleness`. Value is 1 when excluded and 0 when contributing. It has the same `source` scope as the staleness gauge.

**`llm_gateway.keys.refresh`** (counter) -- key-source refresh attempts. Attributes are `source` and `result`, where `result` is `success`, `not_modified`, or `failure`.

**`llm_gateway.keys.resident`** (observable gauge) -- records in the current composed key snapshot after precedence and staleness exclusion. It has no attributes.

See [Key Sources](key-sources.md) for the reload and exclusion semantics behind these instruments.

## Usage Records

Every dispatched chat completion writes one structured `usage` log record, whether or not metrics are enabled. Requests rejected before provider dispatch -- malformed bodies, denied models or routes, and unroutable models -- produce no usage record. The record carries `channel=usage` for filtering alongside the gateway's ordinary logs. Keep this channel unconfigured: a separately configured channel's JSON logger would omit that attribute. Output is JSON when the gateway's output is not a terminal; `DL_USE_JSON=true` forces JSON locally.

| Field | Value |
|---|---|
| `key` | API key record name; empty when authentication is off |
| `run` | caller-supplied run id; empty when absent or invalid |
| `provider` | `openai`, `anthropic`, `local` |
| `model` | the gateway's concrete model binding |
| `streaming` | `true` or `false` |
| `usage_reported` | `true` or `false` |
| `prompt_tokens` | integer; omitted when usage was not reported |
| `completion_tokens` | integer; omitted when usage was not reported |
| `total_tokens` | integer; omitted when usage was not reported |
| `duration_s` | request duration in seconds |
| `completion_id` | first completion id written toward the client; empty when none was produced |
| `error_type` | error type written toward the client; empty on success |

For per-run attribution, send `X-LLM-Gateway-Run: ci/build-4411:attempt-2` on each request in the run. A valid value is 1–128 ASCII characters from `A-Z`, `a-z`, `0-9`, `.`, `_`, `:`, `/`, and `-`, matching `^[A-Za-z0-9._:/-]{1,128}$`. Missing, empty, repeated, comma-folded, or invalid headers are treated as absent; the completion proceeds normally. The run id appears only in the usage record, never in metric labels or upstream requests. The gateway keeps no run state and does not enforce uniqueness. Group records by **`key` and `run` together**: different keys may use the same run id. An empty run id carries no attribution to a particular run.

Metering is best effort. A backend that reports no usage yields `usage_reported=false` with token fields omitted, while reported zero counts remain measured zeros. Interrupted streams retain whatever complete usage total had arrived; a stream interrupted before that total is unmeasured, including Anthropic streams that reported only an opening input count. Client disconnects still produce a record describing what the gateway wrote toward the client, without claiming receipt. Pre-stream API errors keep their own type, an upstream envelope with an empty type is normalized to `server_error`, and mid-stream errors or truncation record `server_error`. Logging failures do not affect the response; token counts are not estimated and cache creation/read counts are not included separately.

## Prometheus Scraping

Point your Prometheus instance at the gateway's `/metrics` endpoint:

```yaml
# prometheus.yml
scrape_configs:
  - job_name: llm-gateway
    scrape_interval: 15s
    static_configs:
      - targets: ["localhost:8080"]
```

## Useful Queries

Requests per minute by provider:

```promql
rate(llm_gateway_requests_total[5m]) * 60
```

Average request duration by model:

```promql
rate(llm_gateway_request_duration_seconds_sum[5m]) / rate(llm_gateway_request_duration_seconds_count[5m])
```

Token throughput (tokens per second):

```promql
rate(llm_gateway_tokens_prompt_total[5m]) + rate(llm_gateway_tokens_completion_total[5m])
```

Reported tokens per key over the last hour:

```promql
sum by (key) (increase(llm_gateway_tokens_prompt_total[1h])) + sum by (key) (increase(llm_gateway_tokens_completion_total[1h]))
```

Error rate as a percentage of total requests:

```promql
rate(llm_gateway_provider_errors_total[5m]) / rate(llm_gateway_requests_total[5m]) * 100
```

Routing method distribution:

```promql
rate(llm_gateway_routing_decisions_total[5m])
```

Current in-flight requests:

```promql
llm_gateway_requests_inflight
```
