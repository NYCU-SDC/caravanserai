GREEN = \033[0;32m
BLUE = \033[0;34m
RED = \033[0;31m
NC = \033[0m

all: build

prepare:
	@echo -e ":: $(GREEN)Preparing environment...$(NC)"
	@echo -e "  -> Downloading go dependencies..."
	@go mod download \
		&& echo -e "==> $(BLUE)Successfully downloaded go dependencies$(NC)" \
		|| (echo -e "==> $(RED)Failed to download go dependencies$(NC)" && exit 1)

build:
	@echo -e ":: $(GREEN)Building all binaries...$(NC)"
	@$(MAKE) -C cmd/cara-server build
	@$(MAKE) -C cmd/cara-agent build
	@$(MAKE) -C cmd/caractl build
	@echo -e "==> $(BLUE)All binaries built successfully$(NC)"

run-server:
	@$(MAKE) -C cmd/cara-server run

run-agent:
	@$(MAKE) -C cmd/cara-agent run

run-cli:
	@$(MAKE) -C cmd/caractl run

test:
	@echo -e ":: $(GREEN)Running tests...$(NC)"
	@go test -cover ./... \
		&& echo -e "==> $(BLUE)All tests passed$(NC)" \
		|| (echo -e "==> $(RED)Tests failed$(NC)" && exit 1)

test-integration:
	@echo -e ":: $(GREEN)Running integration tests (requires Docker)...$(NC)"
	@go test -v -tags e2e -timeout 120s ./test/integration/... $(if $(VERBOSE),-args -verbose) \
		&& echo -e "==> $(BLUE)All integration tests passed$(NC)" \
		|| (echo -e "==> $(RED)Integration tests failed$(NC)" && exit 1)

schemas:
	@echo -e ":: $(GREEN)Generating JSON Schemas...$(NC)"
	@go run ./cmd/schemagen \
		&& echo -e "==> $(BLUE)Schemas generated successfully$(NC)" \
		|| (echo -e "==> $(RED)Schema generation failed$(NC)" && exit 1)

dev-up:
	@echo -e ":: $(GREEN)Starting development PostgreSQL...$(NC)"
	@docker compose up -d --wait \
		&& echo -e "==> $(BLUE)PostgreSQL is ready$(NC)" \
		|| (echo -e "==> $(RED)Failed to start PostgreSQL$(NC)" && exit 1)

dev-down:
	@echo -e ":: $(GREEN)Stopping development services...$(NC)"
	@docker compose down \
		&& echo -e "==> $(BLUE)Services stopped$(NC)"

dev-reset:
	@echo -e ":: $(GREEN)Resetting development environment (wiping data)...$(NC)"
	@docker compose down -v \
		&& docker compose up -d --wait \
		&& echo -e "==> $(BLUE)Development environment reset complete$(NC)" \
		|| (echo -e "==> $(RED)Failed to reset development environment$(NC)" && exit 1)

dev-server: dev-up build
	@echo -e ":: $(GREEN)Starting cara-server...$(NC)"
	@./bin/cara-server

dev-logs:
	@docker compose logs -f

.PHONY: all prepare build run-server run-agent run-cli test test-integration schemas
.PHONY: dev-up dev-down dev-reset dev-server dev-logs
