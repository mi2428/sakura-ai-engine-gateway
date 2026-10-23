# Sakura AI Engine Gateway

An OpenAI-compatible chat gateway for Sakura AI Engine.

## Getting Started

The Gateway exposes `GET /v1/models` and `POST /v1/chat/completions`, authenticated with client-specific bearer tokens.
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
$ curl --fail -H 'Authorization: Bearer <client-token>' http://127.0.0.1:38082/v1/models
$ curl --fail http://127.0.0.1:38082/v1/chat/completions \
    -H 'Authorization: Bearer <client-token>' \
    -H 'Content-Type: application/json' \
    -d '{"model":"<model-id>","messages":[{"role":"user","content":"Hello"}]}'
```

Stop the Compose project with `docker compose down`.
The service publishes its host port on loopback only.

## Configuration

Copy `.env.example` to `.env` and replace every `replace-with-*` value.

| Variable | Default / requirement | Description |
| --- | --- | --- |
| `GATEWAY_CLIENT_TOKENS` | Required | JSON object of client IDs to unique bearer tokens of at least 32 bytes |
| `GATEWAY_LISTEN_ADDRESS` | `:8080` | HTTP listen address |
| `GATEWAY_LOCAL_PORT` | `38082` | Docker Compose host port |
| `GATEWAY_MAX_CONCURRENT_REQUESTS` | `24` | Maximum requests per process |
| `GATEWAY_MAX_CONCURRENT_REQUESTS_PER_CLIENT` | `24` | Maximum requests per client; must not exceed the global limit |
| `GATEWAY_MAX_REQUEST_BYTES` | `67108864` | Maximum request body (64 MiB) |
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

## HTTP API

| Method | Path | Authentication |
| --- | --- | --- |
| `GET` | `/health`, `/readyz` | None |
| `GET` | `/metrics` | None |
| `GET` | `/v1/models` | Client bearer token |
| `POST` | `/v1/chat/completions` | Client bearer token |

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
- `sakura_ai_engine_gateway_in_flight_requests` — Authenticated requests holding a concurrency permit.
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
