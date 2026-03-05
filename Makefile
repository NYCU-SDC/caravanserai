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

.PHONY: all prepare build run-server run-agent run-cli test test-integration
