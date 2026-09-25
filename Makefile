.PHONY: all help build test test-race fmt vet lint security run/proxy run/api refresh refresh/once load sys/tag sys/changelog

GOCMD ?= go
BINARY := proxy_manager
BUILD_DIR := bin
LOCAL_ENV_FILE ?= $(CURDIR)/.local_env
LOAD_LOCAL_ENV = if [ -f "$(LOCAL_ENV_FILE)" ]; then set -a; . "$(LOCAL_ENV_FILE)" || exit 1; set +a; fi;
export REDIS_TEST_URL

all: help

## Release:

sys/tag: ## Create and push an annotated version tag
	@printf 'Enter tag version (e.g., 1.0.0): '; \
	read -r version || exit 1; \
	if ! printf '%s\n' "$$version" | grep -Eq '^[0-9]+\.[0-9]+\.[0-9]+$$'; then \
		echo "Use a version in X.Y.Z form." >&2; exit 1; \
	fi; \
	git tag -a "v$$version" -m "v$$version" && git push origin "v$$version"

sys/changelog: ## Generate CHANGELOG.md from version tags
	@set -- $$(git tag --list 'v[0-9]*' --sort=-version:refname); \
	if [ "$$#" -eq 0 ]; then echo "No version tags found." >&2; exit 1; fi; \
	printf '# Changelog\n\n' > CHANGELOG.md; \
	while [ "$$#" -gt 0 ]; do \
		tag=$$1; shift; \
		printf '## %s (%s)\n\n' "$$tag" "$$(git log -1 --format=%as "$$tag")" >> CHANGELOG.md; \
		if [ "$$#" -gt 0 ]; then range="$$1..$$tag"; else range="$$tag"; fi; \
		git log --reverse --format='- %s [%an]' "$$range" >> CHANGELOG.md || exit 1; \
		printf '\n' >> CHANGELOG.md; \
	done

## Development:

build: ## Build the service binary
	$(GOCMD) build -o $(BUILD_DIR)/$(BINARY) ./cmd/proxy_manager

test: ## Run tests
	$(GOCMD) test ./...
	sh scripts/test-release.sh

test-race: ## Run tests with race detection
	$(GOCMD) test -race ./...

fmt: ## Format Go code
	$(GOCMD) fmt ./...

vet: ## Check Go code
	$(GOCMD) vet ./...

lint: ## Check Go format and run vet
	@files=$$(gofmt -l .); test -z "$$files" || { printf '%s\n' "$$files"; exit 1; }
	$(GOCMD) vet ./...

security: ## Scan Go code
	$(GOCMD) run github.com/securego/gosec/v2/cmd/gosec@v2.29.0 ./...

## Services:

run/proxy: ## Run the CONNECT server
	@$(LOAD_LOCAL_ENV) $(GOCMD) run ./cmd/proxy_manager run proxy

run/api: ## Run the statistics API
	@$(LOAD_LOCAL_ENV) $(GOCMD) run ./cmd/proxy_manager run api

refresh: ## Refresh provider lists until the process stops
	@$(LOAD_LOCAL_ENV) $(GOCMD) run ./cmd/proxy_manager refresh

refresh/once: ## Refresh provider lists one time
	@$(LOAD_LOCAL_ENV) $(GOCMD) run ./cmd/proxy_manager refresh --once

## Diagnostics:

load: ## Check load with an isolated Redis database
	@$(GOCMD) run ./cmd/loadprobe $(LOAD_FLAGS)

## Help:

help: ## Show targets
	@printf 'proxy-manager - CONNECT proxy service\n'
	@awk 'BEGIN { FS = ":.*##"; section = "" } \
		/^## / { section = substr($$0, 4); next } \
		/^[a-zA-Z_\/-]+:.*##/ { \
			if (section != "") { printf "\n%s\n", section; section = "" } \
			printf "  %-16s %s\n", $$1, $$2 \
		}' $(MAKEFILE_LIST)
