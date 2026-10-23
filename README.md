# Sakura AI Engine Gateway

An OpenAI-compatible chat gateway for Sakura AI Engine, with an optional multi-provider web search service.

### Gateway

The Gateway exposes OpenAI-compatible `GET /v1/models` and `POST /v1/chat/completions` endpoints, authenticated with client-specific bearer tokens.
Upstream requests rotate across configured Sakura account tokens, with retries and per-account cooldowns for transient failures and rate limits.
Chat completions can stream; request size, concurrency, retry, and upstream idle limits are configurable, and `/metrics` reports attempts, token usage, retries, cooldowns, and in-flight requests.

### Search Tool

The Search Tool provides authenticated `POST /search` requests and normalizes Brave, Tavily, Exa, Firecrawl, and Parallel results to `title`, `link`, and `snippet`.
Requests rotate through providers and fall back on timeouts, HTTP 408, HTTP 429, or HTTP 5xx; other provider errors fail closed.
HTTP 429 applies a provider cooldown using `Retry-After` (60 seconds by default, capped at one hour), and queries over 500 characters skip Firecrawl.

## Getting Started

### Kubernetes (recommended)

[`k8s/base/`](k8s/base) contains separate Deployments and ClusterIP Services.
Create a `sakura-ai-engine-gateway-credentials` Secret for the Gateway and a `searchtool-credentials` Secret with the Search Tool credential keys from the tables above.
The `:must-pin` image references are unpublished placeholders; the consuming overlay must replace them with immutable image digests and provide an environment-specific ingress.
Keep credentials outside Kustomize, restrict ingress to intended clients, and do not use a public LoadBalancer.

The Gateway requests 2 GiB and is limited to 6 GiB; the Search Tool requests 64 MiB and is limited to 256 MiB.
Each Deployment uses one replica with the `Recreate` strategy because cooldown state is process-local.
In-progress requests can be interrupted during upgrades.

### Docker Compose

Use the `.env` file configured above.
Start the Gateway:

```console
$ docker compose up --build -d gateway
$ curl --fail http://127.0.0.1:38082/health
$ curl --fail -H 'Authorization: Bearer <client-token>' http://127.0.0.1:38082/v1/models
$ curl --fail http://127.0.0.1:38082/v1/chat/completions \
    -H 'Authorization: Bearer <client-token>' \
    -H 'Content-Type: application/json' \
    -d '{"model":"<model-id>","messages":[{"role":"user","content":"Hello"}]}'
```

Start the Search Tool:

```console
$ docker compose --profile searchtool up --build -d searchtool
$ curl --fail http://127.0.0.1:38083/health
$ curl --fail http://127.0.0.1:38083/search \
    -H 'Authorization: Bearer <searchtool-client-token>' \
    -H 'Content-Type: application/json' \
    -d '{"query":"example search","count":5}'
```

Stop the Compose project with `docker compose --profile searchtool down`.
Both services publish host ports on loopback only.
Both services use one image; Compose runs `/gateway` by default and selects `/searchtool` for the Search Tool.

## Configuration

Copy `.env.example` to `.env` and replace every `replace-with-*` value.
Compose interpolates the required Gateway variables even when only the `searchtool` profile is enabled.
Keep the non-empty Gateway placeholders in `.env` when running the Search Tool alone; the Gateway container will not start.

### Gateway

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

### Search Tool

| Variable | Default / requirement | Description |
| --- | --- | --- |
| `SEARCHTOOL_BRAVE_API_KEY` | Required | Brave Search API key |
| `SEARCHTOOL_CLIENT_TOKEN` | Required | Bearer token of at least 32 bytes |
| `SEARCHTOOL_EXA_API_KEY` | Required | Exa API key |
| `SEARCHTOOL_FIRECRAWL_API_KEY` | Required | Firecrawl API key |
| `SEARCHTOOL_LISTEN_ADDRESS` | `:8081` | HTTP listen address |
| `SEARCHTOOL_LOCAL_PORT` | `38083` | Docker Compose host port |
| `SEARCHTOOL_PARALLEL_API_KEY` | Required | Parallel API key |
| `SEARCHTOOL_TAVILY_API_KEY` | Required | Tavily API key |

The retry budget must cover configured attempts, backoffs, and a 300-second safety margin.
Credentials are read once at startup; restart the corresponding service after changing them.

## HTTP API

| Service | Method | Path | Authentication |
| --- | --- | --- | --- |
| Gateway | `GET` | `/health`, `/readyz` | None |
| Gateway | `GET` | `/metrics` | None |
| Gateway | `GET` | `/v1/models` | Client bearer token |
| Gateway | `POST` | `/v1/chat/completions` | Client bearer token |
| Search Tool | `GET` | `/health`, `/readyz` | None |
| Search Tool | `POST` | `/search` | Search Tool bearer token |

`POST /search` accepts `{"query":"...","count":5}`.
Queries are limited to 2048 bytes; count defaults to 5 and is limited to 20.
The response is `{"results":[{"title":"...","link":"https://...","snippet":"..."}]}`.
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
  build        Build all cmd/*/main.go packages by default; use CMD=searchtool to select
  image        Build the shared Gateway and Search Tool image
  dev          Start the Gateway by default (requires .env; use CMD=searchtool)
  down         Stop local Compose services

Help
  help         Show available commands
```

## License

[MIT](LICENSE).
