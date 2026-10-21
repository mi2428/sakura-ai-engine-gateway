# Sakura AI Engine Gateway

A Sakura AI Engine gateway that exposes OpenAI-compatible `/v1/models` and `/v1/chat/completions` endpoints.
Clients use their own tokens; the gateway rotates configured Sakura account tokens upstream.

## Run Locally

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
| `GET` | `/v1/models` | Client bearer token |
| `POST` | `/v1/chat/completions` | Client bearer token |

Other paths return `404`; unsupported methods return `405`.
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

Retries happen before response headers are sent; failures after streaming starts are not retried.
A retry may trigger another completion if Sakura processed the request but its response was lost.
On timeout retries, `reasoning_effort` values `medium` and `high` are lowered to `low`.
Rate-limit cooldowns are stored per process, so run a single replica.

## Kubernetes Deployment

`k8s/base/` provides a Deployment and ClusterIP Service.
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

## Development

To run `make check`, install Go 1.27, `golangci-lint` 2.x, Hadolint, Yamllint, Kubeconform, Docker Compose, and `kubectl`:

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
