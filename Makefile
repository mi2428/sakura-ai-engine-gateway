COMPOSE := docker compose
.DEFAULT_GOAL := help

.PHONY: help check build image dev down fmt

##@ Quality

check: ## Run Go tests, static checks, container and Kubernetes validation
	go mod tidy -diff
	golangci-lint fmt --diff
	go vet ./...
	go test -race -count=1 ./...
	golangci-lint run ./...
	hadolint docker/gateway/Dockerfile
	yamllint -d '{extends: relaxed, rules: {line-length: disable}}' docker-compose.yml k8s/base
	$(COMPOSE) --env-file .env.example config --quiet
	bash -o pipefail -c 'kubectl kustomize k8s/base | kubeconform -strict -summary'

fmt: ## Format Go source
	golangci-lint fmt

##@ Development

build: ## Build all Go commands into build/
	mkdir -p build
	go build -trimpath -o build/ ./cmd/...

image: ## Build the local Docker image
	$(COMPOSE) --env-file .env.example build

dev: ## Start the local gateway (requires a configured .env)
	$(COMPOSE) up --build

down: ## Stop the local gateway
	$(COMPOSE) down

##@ Help

help: ## Show available commands
	@awk 'BEGIN {FS = ":.*##"; section = ""} \
	/^[a-zA-Z0-9_.-]+:.*##/ { \
		if (section != "") printf "\n\033[1m%s\033[0m\n", section; \
		section = ""; \
		printf "  \033[36m%-11s\033[0m %s\n", $$1, $$2; next \
	} \
	/^##@/ { section = substr($$0, 5); next }' $(MAKEFILE_LIST)
