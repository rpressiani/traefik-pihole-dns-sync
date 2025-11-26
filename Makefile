.PHONY: help build run test clean docker-build docker-run docker-test

help: ## Show this help message
	@echo 'Usage: make [target]'
	@echo ''
	@echo 'Available targets:'
	@awk 'BEGIN {FS = ":.*?## "} /^[a-zA-Z_-]+:.*?## / {printf "  %-15s %s\n", $$1, $$2}' $(MAKEFILE_LIST)

build: ## Build the Go binary locally (for development only)
	go build -o sync .

clean: ## Remove built binary
	rm -f sync

docker-build: ## Build Docker image
	docker build -t traefik-pihole-dns-sync:latest .

docker-run: ## Run Docker container (requires .env)
	docker-compose up -d

docker-stop: ## Stop Docker container
	docker-compose down

docker-logs: ## Show Docker container logs
	docker-compose logs -f

docker-test: ## Run Docker container in test mode (dry-run, once)
	docker-compose run --rm traefik-pihole-dns-sync /app/sync --dry-run --once

docker-rebuild: ## Rebuild and restart Docker container
	docker-compose down
	docker-compose build
	docker-compose up -d

tidy: ## Tidy Go modules
	go mod tidy

fmt: ## Format Go code
	go fmt ./...
