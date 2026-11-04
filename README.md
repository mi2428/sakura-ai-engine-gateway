# Sakura AI Engine Gateway

[![CI](https://github.com/mi2428/sakura-ai-engine-gateway/actions/workflows/ci.yml/badge.svg)](https://github.com/mi2428/sakura-ai-engine-gateway/actions/workflows/ci.yml)

An OpenAI-compatible chat gateway for Sakura AI Engine, with account rotation and automatic retries.

>[!CAUTION]
> **No built-in authentication.** Restrict access to a private network (for example, Tailscale ACLs); never expose it publicly.

## Getting Started

### Kubernetes (recommended)

Use [`k8s/base/`](k8s/base) with:

- A separately managed `sakura-ai-engine-gateway-credentials` Secret containing `SAKURA_AI_ENGINE_ACCOUNT_TOKENS`.
- An overlay replacing the unpublished `:must-pin` image placeholder with an immutable image digest.
- Private ingress restricted to intended clients, not a public LoadBalancer.

Upgrades can interrupt in-progress requests.

### Docker Compose

Requires Docker with cgroup v2 and a finite container memory limit (set by the supplied Compose file).

```sh
cp .env.example .env
```

Set `SAKURA_AI_ENGINE_ACCOUNT_TOKENS` in `.env` to a JSON array of your Sakura account tokens.
See [`.env.example`](.env.example) for local settings.

```sh
docker compose up --build -d gateway
curl --fail http://127.0.0.1:38082/v1/models
```

Set your OpenAI-compatible client's base URL to `http://127.0.0.1:38082/v1`.
Restart the Gateway after changing credentials or the container memory limit.

## Environment Variables

The retry budget must cover configured attempts, backoffs, and a 300-second safety margin.
Compose forwards only variables listed in [`docker-compose.yml`](docker-compose.yml); add other overrides to its `environment` section.

| Variable | Default / requirement | Purpose |
| --- | --- | --- |
| `SAKURA_AI_ENGINE_ACCOUNT_TOKENS` | Required | JSON array of unique Sakura account tokens |
| `SAKURA_AI_ENGINE_API_URL` | `https://api.ai.sakura.ad.jp` | Upstream HTTP(S) origin; no credentials, path, query, or fragment |
| `GATEWAY_LISTEN_ADDRESS` | `:8080` | HTTP listen address |
| `GATEWAY_LOCAL_PORT` | `38082` | Compose host port (loopback only) |
| `GATEWAY_MEMORY_CGROUP_PATH` | `/sys/fs/cgroup` | Directory containing cgroup v2 `memory.current` and finite `memory.max` |
| `GATEWAY_MAX_REQUEST_BYTES` | `67108864` | Maximum request body (64 MiB) |
| `GATEWAY_MAX_QUEUED_REQUESTS` | `64` | Maximum requests waiting for memory; not an active-request limit |
| `GATEWAY_QUEUE_TIMEOUT` | `60s` | Maximum wait for memory |
| `GOMEMLIMIT` | `4GiB` in Compose/Kubernetes | Go soft memory limit; keep below the container hard limit |
| `SAKURA_RETRY_MAX` | `5` | Retries after the first attempt |
| `SAKURA_RETRY_BASE_INTERVAL` | `10s` | Initial retry backoff |
| `SAKURA_RETRY_MAX_INTERVAL` | `120s` | Backoff cap; provider `Retry-After` is honored |
| `SAKURA_RETRY_JITTER` | `1s` | Maximum random delay added to backoff |
| `SAKURA_RETRY_BUDGET` | `3200s` | Retry budget before streaming starts |
| `SAKURA_UPSTREAM_IDLE_TIMEOUT` | `420s` | Maximum upstream I/O inactivity |

## Metrics

`GET /metrics` exposes Prometheus text metrics without authentication; restrict access at the network boundary.
`account_slot` is one-based; `token_type` is `input`, `output`, or `total`.

| Metric | Labels | Meaning |
| --- | --- | --- |
| `sakura_ai_engine_gateway_chat_completion_requests_total` | `model` | Accepted chat requests, counted once regardless of retries |
| `sakura_ai_engine_gateway_upstream_attempts_total` | `account_slot`, `model`, `status_code` | Upstream attempts including retries; status `0` means no HTTP response |
| `sakura_ai_engine_gateway_upstream_retries_scheduled_total` | `account_slot`, `model`, `reason` | Retries scheduled before backoff |
| `sakura_ai_engine_gateway_usage_tokens_total` | `account_slot`, `model`, `token_type` | Provider-reported token counts |
| `sakura_ai_engine_gateway_usage_responses_total` | `account_slot`, `model` | Responses containing recognized usage data |
| `sakura_ai_engine_gateway_usage_incomplete_total` | `account_slot`, `model` | Successful responses missing input or output token counts |
| `sakura_ai_engine_gateway_in_flight_requests` | — | Requests holding a memory reservation |
| `sakura_ai_engine_gateway_queued_requests` | — | Requests waiting for memory |
| `sakura_ai_engine_gateway_memory_usage_bytes` | — | Last measured cgroup usage; valid when `memory_available` is `1` |
| `sakura_ai_engine_gateway_memory_limit_bytes` | — | Container hard memory limit read at startup |
| `sakura_ai_engine_gateway_memory_reserved_bytes` | — | Memory reservations held by active requests |
| `sakura_ai_engine_gateway_memory_available` | — | Latest memory sample succeeded (`1`) or failed (`0`) |
| `sakura_ai_engine_gateway_account_cooldown_seconds` | `account_slot` | Remaining account cooldown |
| `sakura_ai_engine_gateway_account_rate_limit_streak` | `account_slot` | Consecutive rate-limit responses |

## License

[MIT](LICENSE).
