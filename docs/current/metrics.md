# Metrics

The gateway exposes OpenTelemetry metrics via a Prometheus exporter. When enabled, metrics are available at `GET /metrics` in the standard Prometheus text format.

## Enabling Metrics

```yaml
metrics:
  enabled: true
  stream_usage: true   # optional; capture token usage on the streaming path
```

## Instruments

All metric names are prefixed with `llm_gateway.`.

### Request Metrics

**`llm_gateway.requests`** (counter) -- total chat completion requests.

| Attribute | Values | Description |
|---|---|---|
| `provider` | `openai`, `anthropic`, `ollama` | which provider handled the request |
| `model` | model name | the model used |
| `streaming` | `true`, `false` | whether the request was streaming |
| `key` | key name or empty | the API key name (when [virtual API keys](api-keys.md) are enabled) |

**`llm_gateway.request.duration`** (histogram, seconds) -- end-to-end request duration including upstream provider latency.

| Attribute | Values | Description |
|---|---|---|
| `provider` | `openai`, `anthropic`, `ollama` | which provider handled the request |
| `model` | model name | the model used |
| `key` | key name or empty | the API key name (when [virtual API keys](api-keys.md) are enabled) |

**`llm_gateway.requests.inflight`** (up-down counter) -- number of requests currently being processed. Incremented when a request enters the handler, decremented when it completes. Useful for understanding concurrency and detecting request pileups.

### Token Metrics

**`llm_gateway.tokens.prompt`** (counter) -- total prompt (input) tokens across all requests.

| Attribute | Values | Description |
|---|---|---|
| `provider` | provider name | which provider reported the usage |
| `model` | model name | the model used |
| `key` | key name or empty | the API key name (when [virtual API keys](api-keys.md) are enabled) |

**`llm_gateway.tokens.completion`** (counter) -- total completion (output) tokens across all requests.

| Attribute | Values | Description |
|---|---|---|
| `provider` | provider name | which provider reported the usage |
| `model` | model name | the model used |
| `key` | key name or empty | the API key name (when [virtual API keys](api-keys.md) are enabled) |

Non-streaming responses are always metered, from the `usage` field of the completion response.

Streaming responses are metered only when `metrics.stream_usage` is enabled, because asking for streamed usage changes what the gateway sends upstream:

- **OpenAI and local backends** report streamed usage only when the request carries `stream_options.include_usage`, which the gateway sets on the upstream request when the toggle is on. Local backends vary in whether they honor it; one that ignores it simply reports no usage, and the request is still counted by `llm_gateway.requests`.
- **Anthropic** always streams usage and needs no opt-in, but honors the toggle anyway so token semantics stay consistent across providers.

A stream that ends without a usage event -- an upstream that omits it, or a client that disconnects mid-stream -- records no token sample rather than a partial count.

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
