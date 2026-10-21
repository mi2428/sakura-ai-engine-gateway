# Sakura AI Engine Gateway

A Sakura AI Engine gateway that exposes OpenAI-compatible `/v1/models` and `/v1/chat/completions` endpoints.
Clients use their own tokens; the gateway rotates configured Sakura account tokens upstream.

## Getting Started

### Kubernetes (recommended)

[`k8s/base/`](k8s/base) provides a Deployment and ClusterIP Service.
Before deployment, provision a Secret named `sakura-ai-engine-gateway-credentials` in the target namespace with the two required credential keys.
The consuming overlay must pin the source commit and image digest, and provide an environment-specific ingress.

> [!IMPORTANT]
> The base's `:must-pin` image tag is not published; replace it with an immutable digest.
> Keep credentials outside Kustomize, and expose the Service only through an ingress that restricts access to intended clients.
> Do not expose the Service as a public LoadBalancer.

The base requests 2 GiB and limits the container to 6 GiB.
The default limits allow up to 1.5 GiB of request bodies across 24 concurrent requests, before runtime overhead.
The Deployment runs one replica with the `Recreate` strategy because cooldown state is process-local.
In-progress streams can be interrupted during upgrades.

### Docker Compose

Requires Docker Compose and a Sakura account token.
Copy `.env.example` to `.env`, set `SAKURA_AI_ENGINE_ACCOUNT_TOKENS` to a JSON array of upstream tokens, and configure `GATEWAY_CLIENT_TOKENS` with a local client token.
The sample `local` client token is public; use it for local testing only.

> [!IMPORTANT]
> Do not commit a populated `.env`.
> For deployed clients, generate a unique random token of at least 32 bytes for each client and store both credential values in a secret manager.

```console
$ cp .env.example .env  # Set the Sakura account token and a local client token in .env.
$ docker compose up --build -d
$ curl --fail http://127.0.0.1:38082/health
$ curl --fail -H 'Authorization: Bearer <client-token>' http://127.0.0.1:38082/v1/models
$ curl --fail http://127.0.0.1:38082/v1/chat/completions \
    -H 'Authorization: Bearer <client-token>' \
    -H 'Content-Type: application/json' \
    -d '{"model":"<model-id>","messages":[{"role":"user","content":"Hello"}]}'
$ make down
```

Replace `<client-token>` with the token configured in `.env` and `<model-id>` with an ID returned by `/v1/models`.
The host port is published on loopback only.

## API and Configuration

| Method | Path | Authentication |
| --- | --- | --- |
| `GET` | `/health`, `/readyz` | None |
| `GET` | `/metrics` | None |
| `GET` | `/v1/models` | Client bearer token |
| `POST` | `/v1/chat/completions` | Client bearer token |

Other paths return `404`; unsupported methods return `405`.
`/metrics` uses Prometheus text exposition and is unauthenticated; restrict access at the proxy or network boundary.
`GATEWAY_CLIENT_TOKENS` maps stable client IDs to unique tokens.
`SAKURA_AI_ENGINE_ACCOUNT_TOKENS` is a JSON array of unique upstream tokens.

| Variable | Default | Description |
| --- | --- | --- |
| `SAKURA_AI_ENGINE_ACCOUNT_TOKENS` | Required | JSON array of Sakura account tokens |
| `GATEWAY_CLIENT_TOKENS` | Required | JSON object of client IDs to unique bearer tokens (at least 32 bytes each) |
| `GATEWAY_LISTEN_ADDRESS` | `:8080` | HTTP listen address |
| `SAKURA_AI_ENGINE_API_URL` | `https://api.ai.sakura.ad.jp` | Upstream HTTP(S) origin; no credentials, path, query, or fragment |
| `GATEWAY_MAX_REQUEST_BYTES` | `67108864` | Maximum request body (64 MiB) |
| `GATEWAY_MAX_CONCURRENT_REQUESTS` | `24` | Maximum requests per process |
| `GATEWAY_MAX_CONCURRENT_REQUESTS_PER_CLIENT` | `24` | Maximum requests per client; cannot exceed the global limit |
| `SAKURA_RETRY_MAX` | `5` | Retries after the first attempt |
| `SAKURA_RETRY_BASE_INTERVAL` | `10s` | Initial backoff |
| `SAKURA_RETRY_MAX_INTERVAL` | `120s` | Backoff cap; provider `Retry-After` is honored |
| `SAKURA_RETRY_JITTER` | `1s` | Positive retry jitter |
| `SAKURA_RETRY_BUDGET` | `3200s` | Retry time before streaming starts; active streams are not limited by this budget |
| `SAKURA_UPSTREAM_IDLE_TIMEOUT` | `420s` | Maximum upstream I/O inactivity |
| `GATEWAY_LOCAL_PORT` | `38082` | Host port for Docker Compose only |

Invalid configuration fails at startup.
The retry budget must cover configured attempts, backoffs, and a 300-second safety margin; restart the gateway after changing credentials.
Credentials and request bodies are not logged.

## Prometheus Metrics

`/metrics` is an unauthenticated Prometheus endpoint; protect it at the front proxy.

- `sakura_ai_engine_gateway_chat_completion_requests_total{model}` — accepted chat completion requests, counted once per caller request.
- `sakura_ai_engine_gateway_upstream_attempts_total{account_slot,model,status_code}` — requests sent to Sakura. Retries count as additional attempts; status `0` means no HTTP response was received. Use this for the console's request count.
- `sakura_ai_engine_gateway_upstream_retries_scheduled_total{account_slot,model,reason}` — retry decisions before backoff. A scheduled retry may be cancelled before the next upstream attempt.
- `sakura_ai_engine_gateway_usage_tokens_total{account_slot,model,token_type}` — provider-reported token totals; `token_type` is `input`, `output`, or `total`.
- `sakura_ai_engine_gateway_usage_responses_total{account_slot,model}` — chat completion responses containing recognized usage fields.
- `sakura_ai_engine_gateway_usage_incomplete_total{account_slot,model}` — successful responses without both input and output token counts.
- `sakura_ai_engine_gateway_in_flight_requests` — current authenticated requests holding a concurrency permit.
- `sakura_ai_engine_gateway_account_cooldown_seconds{account_slot}` — remaining rate-limit cooldown for each upstream account slot.
- `sakura_ai_engine_gateway_account_rate_limit_streak{account_slot}` — consecutive rate-limit responses for each upstream account slot.

Grafana query examples using the selected time range:

```promql
sum by (model) (increase(sakura_ai_engine_gateway_upstream_attempts_total{model!="unknown"}[$__range]))
sum by (model) (increase(sakura_ai_engine_gateway_usage_tokens_total{model!="unknown",token_type="input"}[$__range]))
sum by (model) (increase(sakura_ai_engine_gateway_usage_tokens_total{model!="unknown",token_type="output"}[$__range]))
sum by (account_slot, model, token_type) (increase(sakura_ai_engine_gateway_usage_tokens_total{model!="unknown"}[$__range]))
```

## Development

To run `make check`, install Go, `golangci-lint`, Hadolint, Yamllint, Kubeconform, Docker Compose, and `kubectl`:

```console
$ make check build
```

The default `make` target prints the available targets:

```console
$ make

Quality
  check        Run Go tests, static checks, container and Kubernetes validation
  fmt          Format Go source

Development
  build        Build the local Go binary
  image        Build the local Docker image
  dev          Start the local gateway (requires a configured .env)
  down         Stop the local gateway

Help
  help         Show available commands
```

## License

[MIT](LICENSE).
