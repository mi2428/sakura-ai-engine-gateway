COMPOSE := docker compose
.DEFAULT_GOAL := help
.PHONY: help check build image dev down fmt

comma := ,
empty :=
space := $(empty) $(empty)
CMD_NAMES := $(notdir $(patsubst %/,%,$(dir $(wildcard cmd/*/main.go))))
CMD ?= $(subst $(space),$(comma),$(CMD_NAMES))
CMD_LIST = $(strip $(subst $(comma),$(space),$(CMD)))
validate-cmds = $(if $(CMD_LIST),$(if $(filter-out $(CMD_NAMES),$(CMD_LIST)),$(error CMD values must match cmd/*/main.go: $(CMD)),:),$(error CMD must select at least one cmd))
COMPOSE_SERVICES = $(CMD_LIST)
SEARCHTOOL_PROFILE = $(if $(filter searchtool,$(CMD_LIST)),--profile searchtool)

##@ Quality

check: ## Run Go tests, static checks, container and Kubernetes validation
	go mod tidy -diff
	golangci-lint fmt --diff
	go vet ./...
	go test -race -count=1 ./...
	golangci-lint run ./...
	hadolint docker/gateway/Dockerfile docker/searchtool/Dockerfile
	yamllint -d '{extends: relaxed, rules: {line-length: disable}}' docker-compose.yml k8s/base
	$(COMPOSE) --env-file .env.example config --quiet
	bash -o pipefail -c 'kubectl kustomize k8s/base | kubeconform -strict -summary'

fmt: ## Format Go source
	golangci-lint fmt

##@ Development

build: ## Build all cmd/*/main.go packages by default; use CMD=searchtool to select
	@$(validate-cmds)
	mkdir -p build
	go build -trimpath -o build/ $(foreach cmd,$(CMD_LIST),./cmd/$(cmd))

image: ## Build all Docker images by default; use CMD=searchtool to select
	@$(validate-cmds)
	$(COMPOSE) --env-file .env.example $(SEARCHTOOL_PROFILE) build $(COMPOSE_SERVICES)

dev: CMD = gateway
dev: ## Start the Gateway by default (requires .env; use CMD=searchtool)
	@$(validate-cmds)
	$(COMPOSE) $(SEARCHTOOL_PROFILE) up --build $(COMPOSE_SERVICES)

down: ## Stop local Compose services
	$(COMPOSE) --profile searchtool down

##@ Help

help: ## Show available commands
	@awk 'BEGIN {FS = ":.*##"; section = ""} \
	/^[a-zA-Z0-9_.-]+:.*##/ { \
		if (section != "") printf "\n\033[1m%s\033[0m\n", section; \
		section = ""; \
		printf "  \033[36m%-11s\033[0m %s\n", $$1, $$2; next \
	} \
	/^##@/ { section = substr($$0, 5); next }' $(MAKEFILE_LIST)
