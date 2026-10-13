# CHANGELOG

## Unreleased

FEATURE: Prompt and completion token metrics now carry `key` and `streaming` labels on both completion paths. OpenAI and local streaming requests ask upstream for usage and consume it without exposing usage chunks to clients; Anthropic assembles cumulative usage totals. Each request meters the latest reported total once, and backends that report no usage increment no token counters. See [docs/current/metrics.md](docs/current/metrics.md).

FEATURE: Every dispatched completion now writes one structured usage record on the `usage` log channel, including when metrics are disabled. An optional `X-LLM-Gateway-Run` header enables downstream totals grouped by key and run; invalid or repeated values are ignored. Records omit token fields when unmeasured and carry the completion id and client-facing error type. See [docs/current/metrics.md](docs/current/metrics.md#usage-records).

CHANGE: An upstream error envelope carrying a message but no `type` now reaches the client as a `server_error` instead of an error with an empty type. The OpenAI-compatible providers pass such an envelope through verbatim, so the gateway previously emitted a type no client could act on; the same value is what the new usage record and the provider-error metric report.

## v0.1.7

FEATURE: Virtual API keys can now be composed from boot-resident config keys, reloadable strict-YAML files, and a versioned HTTP key API. Sources refresh without restarting, converge through polling plus optional file watches and Unix `SIGHUP`, hold last-known-good on reload failure, and support optional fail-closed exclusion through `max_staleness`. The resident store keeps only SHA-256 digests, source records may publish `key_sha256` and exact-boundary `expires_at`, HTTP refreshes support ETag/conditional `304`, and four OpenTelemetry instruments expose refresh results, freshness, exclusion, and resident cardinality. See [docs/current/key-sources.md](docs/current/key-sources.md).

CHANGE: Virtual API-key configuration and source documents now decode strictly. Unknown or duplicate fields, type coercions, invalid model globs, malformed key material, and unsupported schema versions fail loudly rather than silently widening access; plaintext keys must fit the HTTP bearer-token grammar. `api_keys.enabled: true` may contain only external sources, while a declared source requires authentication to be enabled. Config keys remain first in precedence so they can serve as break-glass credentials. See [docs/current/api-keys.md](docs/current/api-keys.md).

FIX: Operator-supplied zrok share tokens no longer appear in access, provider, endpoint, key-source, persistent-share, or cleanup logs. Lifecycle messages use non-secret owner labels instead. Tokens generated for ephemeral gateway shares remain startup output because that is how the operator learns the new address.

FIX: Errors from an OpenAI-compatible local backend keep their own type and status instead of becoming a gateway `server_error` with HTTP 500. The local provider read only Ollama's native `{"error": "message"}` form, so a standard `{"error": {"message": ..., "type": ...}}` envelope from vLLM, SGLang, or llama-server fell through to a generic server error -- reporting a client's bad request as a gateway failure. The OpenAI envelope is now read first and the Ollama form remains a fallback. A response body matching neither envelope no longer appears in the client-visible message, since the local provider fronts arbitrary operator-configured backends whose bodies are not the gateway's to forward.

FIX: A provider `base_url` that is not an absolute HTTP(S) URL is now a directed startup error naming the field, rather than a gateway that starts healthy and fails every affected request. This covers the OpenAI, Anthropic, and local blocks and each multi-endpoint entry; an omitted `base_url` still means "use the default" and is unaffected.

FEATURE: New `dummy-keys` binary serves the published `GET /v1/keys` contract from a local file, so an HTTP key source can be exercised without standing up a management plane. The file is re-read per request and carries an `ETag`, so editing it and watching the gateway converge is the demo, and `--fault` injects the failure modes the design is built around -- an error status, a `count` that disagrees with `keys`, pagination headers, an unsolicited `304`, a stalled response, a malformed envelope, an unknown record field -- none of which are observable against a server that always behaves. See [docs/current/dummy-keys.md](docs/current/dummy-keys.md).

FIX: Startup errors about the OpenAI provider now name `providers.open_ai.*`, the configuration key the gateway actually reads. They previously named `providers.openai.*`, which does not exist -- an operator following the message would search their config for a key that is not there and change nothing.

## v0.1.6

FEATURE: Sterling capability coordinates can be carried as strict virtual model aliases on the existing OpenAI chat surface. The gateway resolves `sterling-capability:sterling-classes/v1/<class>` to the configured route model before provider dispatch while applying explicit-model policy plus route and concrete-model API-key restrictions. The v1 vocabulary currently contains only `frontier-coding`.

CHANGE: Every chat response now reports the concrete model selected by the gateway in its `model` field, for both non-streaming responses and streaming chunks. The gateway no longer passes through an upstream provider's differing model identifier; this makes the reported binding authoritative for all clients, not only Sterling capability runs. When the upstream reports a different identifier (a dated snapshot or server-side alias), the gateway logs it -- the tripwire that surfaces provider-side aliasing that would otherwise be normalized away.

FIX: Multi-endpoint failover corrupted retried requests. A request that failed over to another endpoint reused an already-consumed request body, so the surviving endpoint received an empty or truncated payload -- most damaging for the embedding and classifier calls that ride the same round-robin transport, where a blank body could return a plausible-but-wrong result. Failover now installs a fresh body on every attempt (and refuses to replay a body it cannot recreate rather than send a drained one). It also treats a dropped connection (`io.EOF`/`io.ErrUnexpectedEOF`) as an endpoint failure worth retrying, while a client-cancelled request (`context.Canceled`) is no longer mistaken for one.

FIX: Streaming responses now surface upstream errors instead of swallowing them. A mid-stream error from any provider -- an Anthropic `error` event, or an OpenAI/local `{"error": ...}` envelope that previously decoded into an empty chunk -- is delivered to the client as an SSE error and the stream is closed; a stream that ends without its terminal event no longer reads as a successful completion. Intermediate streaming chunks now emit `finish_reason: null` per the OpenAI streaming contract rather than omitting the field.

FIX: OpenAI-backed semantic routing (embeddings and the classifier) now rides a configured Agora tunnel or zrok share when the OpenAI provider is reached over one, instead of dialing the API directly. A credential-firewall deployment no longer leaks direct egress on its routing calls.

FIX: Virtual API keys defined as environment references (`key: "${VAR}"`) are now expanded, matching the documented behavior and the way upstream provider keys already resolved; previously the literal `${VAR}` placeholder was stored and used as the bearer token.

FIX: A virtual API key with `allowed_routes` restrictions is no longer wrongly denied on explicit-model requests, which select no semantic route. Route restrictions apply only to routed decisions; the resolved-model check still guards every request.

CHANGE: Configuration the gateway cannot honor now fails fast at startup with a directed error instead of degrading silently. This covers `api_keys.enabled` with no keys configured (which previously started with open access), a `zrok.share.mode` that is unrecognized or conflicts with a persistent share token, a provider `agora_tunnel` or `zrok_share_token` set without the API key that provider needs to initialize, a routing provider other than `local` or `openai`, an unresolved `${VAR}` in any secret or base-URL field, a top-level `local` overlay set alongside `endpoints`, and a routing config with duplicate, empty, or unknown route references, a matching layer enabled with no routes, or two API keys sharing a value. An explicit `advertisement.publish: false` on a dial-only gateway is no longer rejected.

CHANGE: Unknown paths and unsupported methods now return OpenAI-shaped JSON errors (404 and 405) instead of `net/http`'s plain-text responses, keeping every client-facing error in the OpenAI envelope. The `dummy-model` backend does the same.

CHANGE: Removed the unused `metrics.listen` config field. It promised a separate metrics listener that was never implemented -- metrics are served on the gateway's own handler at `/metrics`.

## v0.1.5

FEATURE: Agora overlay transport, a peer to zrok built on the SDK's Layer 1 `Listen`/`Dial` primitives. The gateway can serve its handler over an operator-provisioned Agora tunnel (the credential-firewall front door) and dial providers or local endpoints that live behind Agora tunnels, set per-site with `agora_tunnel` alongside a top-level `agora:` block. Serving is **bind-only** -- the gateway binds a tunnel its account owns and never creates or deletes one. Every enabled listener (local, zrok, Agora) serves the same handler at once; the local TCP port is opt-in or fallback so a credential-firewall deployment stays private-only. When a provider sets both `agora_tunnel` and `zrok_share_token`, Agora wins, and a cloud-egress provider keeps its real `https://` base URL so TLS rides the tunnel end-to-end. See [docs/current/agora.md](docs/current/agora.md).

FEATURE: Full bidirectional tool-calling translation for the Anthropic provider, including streaming. OpenAI `tools` and `tool_choice` now map to Anthropic `tools`/`input_schema` and `tool_choice`; Anthropic `tool_use` responses map back to OpenAI `tool_calls` (with `finish_reason: "tool_calls"`); multi-turn `tool` messages round-trip as coalesced Anthropic `tool_result` blocks; and streamed tool calls are assembled from `content_block_start`/`input_json_delta` events into OpenAI-shaped tool-call deltas.

FIX: The Anthropic provider previously dropped `tools` and `tool_choice` entirely and flattened assistant `tool_calls` and `tool` results into plain text, so MCP/tool-calling clients never received their tools and Claude replied with prose instead of tool calls.

FEATURE: New `dummy-model` standalone binary serves an OpenAI-compatible endpoint backed by a fake model, so tests and demos no longer need a real backend (Ollama, vLLM, OpenAI, etc.) or network egress. It echoes the last user message (or a canned `--response`), serves `/v1/models` and `/health`, and supports streaming with a configurable per-chunk `--stream-delay`, deterministic tool-call simulation when a request carries `tools`, and `--error-rate`/`--error-type` error injection. Point the gateway's `local.base_url` at it (default `:8081`) to use it as a drop-in backend.

CHANGE: Refreshed dependencies across the tree to pick up upstream fixes, most notably `github.com/openziti/sdk-golang` to `v1.5.4`, along with the OpenTelemetry, go-openapi, and `zitadel/oidc` modules. The `go` directive also moves to `1.25.7`, raising the minimum Go required to build from source.

## v0.1.4

CHANGE: Removed specificity in docs and implementation tying local instances to Ollama; we really support any local OpenAI-compatible inference. Works with Ollama, llama-server, vLLM, SGLang, or anything that exposes `/v1/chat/completions`.

FIX: Improved ziti context handling to eliminate an issue with leaks on long-running instances. (https://github.com/openziti/llm-gateway/issues/7)

## v0.1.3

FIX: Release attestation changes.

## v0.1.2

FIX: Fix for `draft-release` action; unauthenticated `curl` error.

## v0.1.1

Initial public release.
