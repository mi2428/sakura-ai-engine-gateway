# Sakura AI Engine Gateway

[![CI](https://github.com/mi2428/sakura-ai-engine-gateway/actions/workflows/ci.yml/badge.svg)](https://github.com/mi2428/sakura-ai-engine-gateway/actions/workflows/ci.yml)

An OpenAI-compatible chat gateway for Sakura AI Engine.

## Getting Started

The Gateway exposes `GET /v1/models` and `POST /v1/chat/completions` without application authentication or client identification. Restrict access with Tailscale ACLs or an equivalent network boundary; never expose it publicly.
There is no fixed concurrency limit. A process-wide memory gate reserves capacity before reading request bodies and queues new work when memory is tight.
Upstream requests rotate across configured Sakura account tokens, with retries and per-account cooldowns for transient failures and rate limits.
For `preview/Kimi-K2.7-Code` chat completions, it supplies `reasoning_effort: max` only when the caller omits that field. Explicit values, including `null`, and other models are passed through unchanged. This centralizes the model default for Open WebUI, OpenCode, and Hermes without rewriting their messages or managing their tools. Clients still own search, verification, and delegation; high effort alone cannot verify current facts and may increase latency and token usage.

### Kubernetes (recommended)

[`k8s/base/`](k8s/base) contains a Deployment and ClusterIP Service.
Create a `sakura-ai-engine-gateway-credentials` Secret with the credentials listed below.
The `:must-pin` image reference is an unpublished placeholder; the consuming overlay must replace it with an immutable image digest and provide an environment-specific ingress.
Keep credentials outside Kustomize, restrict ingress to intended clients, and do not use a public LoadBalancer.

The Gateway requests 2 GiB and is limited to 6 GiB.
The Deployment uses one replica with the `Recreate` strategy because cooldown state is process-local.
In-progress requests can be interrupted during upgrades.

### Docker Compose

Use the `.env` file configured above.

```console
$ docker compose up --build -d gateway
$ curl --fail http://127.0.0.1:38082/health
$ curl --fail http://127.0.0.1:38082/v1/models
$ curl --fail http://127.0.0.1:38082/v1/chat/completions \
    -H 'Content-Type: application/json' \
    -d '{"model":"<model-id>","messages":[{"role":"user","content":"Hello"}]}'
```

Stop the Compose project with `docker compose down`.
The service publishes its host port on loopback only.

## Configuration

Copy `.env.example` to `.env` and replace every `replace-with-*` value.

| Variable | Default / requirement | Description |
| --- | --- | --- |
| `GATEWAY_LISTEN_ADDRESS` | `:8080` | HTTP listen address |
| `GATEWAY_LOCAL_PORT` | `38082` | Docker Compose host port |
| `GATEWAY_MEMORY_CGROUP_PATH` | `/sys/fs/cgroup` | Directory containing this container's cgroup v2 `memory.current` and finite `memory.max` |
| `GATEWAY_MAX_QUEUED_REQUESTS` | `64` | Maximum requests waiting for memory; does not cap active requests |
| `GATEWAY_QUEUE_TIMEOUT` | `60s` | Maximum wait for memory, separate from the five-minute body read timeout |
| `GATEWAY_MAX_REQUEST_BYTES` | `67108864` | Maximum request body (64 MiB) |
| `GOMEMLIMIT` | `4GiB` in Compose/Kubernetes | Go soft memory limit, below the container's 6 GiB hard limit |
| `SAKURA_AI_ENGINE_ACCOUNT_TOKENS` | Required | JSON array of unique Sakura account tokens |
| `SAKURA_AI_ENGINE_API_URL` | `https://api.ai.sakura.ad.jp` | Upstream HTTP(S) origin without credentials, path, query, or fragment |
| `SAKURA_RETRY_BASE_INTERVAL` | `10s` | Initial retry backoff |
| `SAKURA_RETRY_BUDGET` | `3200s` | Retry budget before streaming starts |
| `SAKURA_RETRY_JITTER` | `1s` | Positive retry jitter |
| `SAKURA_RETRY_MAX` | `5` | Retries after the first attempt |
| `SAKURA_RETRY_MAX_INTERVAL` | `120s` | Retry backoff cap; provider `Retry-After` is honored |
| `SAKURA_UPSTREAM_IDLE_TIMEOUT` | `420s` | Maximum upstream I/O inactivity |

The retry budget must cover configured attempts, backoffs, and a 300-second safety margin.
Credentials are read once at startup; restart the Gateway after changing them.
Remove obsolete client-token and concurrency variables from external Secrets and overlays; they are no longer read.

### Memory admission

Run in a cgroup v2 container with a finite memory limit. Missing, invalid or unlimited memory accounting prevents startup; a runtime read failure rejects new API requests with `503` instead of using a stale sample. Restart after changing the container memory limit.

Admission pauses at 75% of `memory.max` and resumes at or below two thirds. Each accepted request reserves four times its declared body size plus 2 MiB until the request finishes; unknown-length uploads reserve against `GATEWAY_MAX_REQUEST_BYTES`. Reservations are conservatively added to measured usage to cover concurrent allocations, buffering and JSON inspection. Known oversized uploads are rejected before queuing. Bodies are not read while queued, and active streams are never paused by the gate.

The queue is bounded by count and time; a full or expired queue returns `503` with `Retry-After: 1`. Observed cancellation and shutdown release queued or active reservations. An HTTP/1 upload disconnected before its body is read may retain a queue slot until the queue timeout. Health and metrics endpoints bypass the gate. Upstream account rotation, 429 cooldowns and retries are unchanged. A supplied client `Authorization` header is ignored and never forwarded upstream.

This is admission control, not an OOM guarantee: accepted requests, connections, kernel buffers and other processes can still consume memory. Keep hard-limit headroom and the Go soft limit; the latter encourages garbage collection but is not a hard cap. Queue time is additional to the existing upstream retry budget.

## HTTP API

| Method | Path | Authentication |
| --- | --- | --- |
| `GET` | `/health`, `/readyz` | None |
| `GET` | `/metrics` | None |
| `GET` | `/v1/models` | None |
| `POST` | `/v1/chat/completions` | None |

Invalid paths return `404`; unsupported methods return `405`.

## Metrics

The Gateway `/metrics` endpoint uses Prometheus text exposition and is unauthenticated.
Restrict it at the proxy or network boundary.

- `sakura_ai_engine_gateway_chat_completion_requests_total{model}` — Accepted chat completion requests.
- `sakura_ai_engine_gateway_upstream_attempts_total{account_slot,model,status_code}` — Requests sent to Sakura; retries count as additional attempts.
- `sakura_ai_engine_gateway_upstream_retries_scheduled_total{account_slot,model,reason}` — Retry decisions before backoff.
- `sakura_ai_engine_gateway_usage_tokens_total{account_slot,model,token_type}` — Provider-reported input, output, or total tokens.
- `sakura_ai_engine_gateway_usage_responses_total{account_slot,model}` — Responses containing recognized usage fields.
- `sakura_ai_engine_gateway_usage_incomplete_total{account_slot,model}` — Successful responses missing input or output usage.
- `sakura_ai_engine_gateway_in_flight_requests` — Requests holding a memory reservation.
- `sakura_ai_engine_gateway_queued_requests` — Requests waiting for memory before body reads.
- `sakura_ai_engine_gateway_memory_usage_bytes` — Last measured cgroup memory usage; check `memory_available` for validity.
- `sakura_ai_engine_gateway_memory_limit_bytes` — Cgroup hard memory limit read at startup.
- `sakura_ai_engine_gateway_memory_reserved_bytes` — Memory reservations held by active requests.
- `sakura_ai_engine_gateway_memory_available` — Whether the latest memory sample succeeded.
- `sakura_ai_engine_gateway_account_cooldown_seconds{account_slot}` — Remaining account cooldown.
- `sakura_ai_engine_gateway_account_rate_limit_streak{account_slot}` — Consecutive rate-limit responses by account.

## Development

Run `make help` to list available checks and build targets.

```console
$ make

Quality
  check        Run Go tests, static checks, container and Kubernetes validation
  fmt          Format Go source

Development
  build        Build the Gateway
  image        Build the Gateway image
  dev          Start the Gateway (requires .env)
  down         Stop local Compose services

Help
  help         Show available commands
```

## License

[MIT](LICENSE).
