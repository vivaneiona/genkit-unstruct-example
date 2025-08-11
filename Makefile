# Telegram Bot Makefile

# Variables
BINARY_NAME=telegram-bot
DOCKER_IMAGE=telegram-bot
DOCKER_TAG=latest

# Default target
.PHONY: help
help: ## Show this help message
	@echo "Available targets:"
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | sort | awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-15s\033[0m %s\n", $$1, $$2}'

# Build the binary
.PHONY: build
build: ## Build the binary
	go build -o $(BINARY_NAME)

# Run the bot (build and run)
.PHONY: run
run: build ## Build and run the bot
	@echo "Starting telegram bot..."
	@if [ ! -f .env ]; then echo "Warning: .env file not found. Copy .env.example to .env and configure it."; fi
	./$(BINARY_NAME)

# Clean build artifacts
.PHONY: clean
clean: ## Clean build artifacts
	rm -f $(BINARY_NAME)
	go clean

# Docker targets
.PHONY: docker-build
docker-build: ## Build Docker image
	docker build -t $(DOCKER_IMAGE):$(DOCKER_TAG) .

.PHONY: run-docker
run-docker: docker-build ## Build and run Docker container
	@echo "Starting telegram bot in Docker..."
	@if [ ! -f .env ]; then echo "Error: .env file required. Copy .env.example to .env and configure it."; exit 1; fi
	docker run --rm \
		--env-file .env \
		-v $(PWD)/spends.db:/app/spends.db \
		$(DOCKER_IMAGE):$(DOCKER_TAG)

# Development targets
.PHONY: dev
dev: ## Run with hot reload (requires .env file)
	go run main.go

.PHONY: test
test: ## Run tests
	go test ./...

.PHONY: deps
deps: ## Download and tidy dependencies
	go mod tidy
	go mod vendor
