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

# The dev stack reads MinIO's credentials from .env, which is gitignored so the
# compose file carries no credentials at all. Making it a file target means make
# creates it on first use and never mentions it again — the alternative is every
# clone failing on a password that is not a secret, which teaches people that
# credential handling is noise.
.env:
	@cp .env.example .env
	@echo -e "  -> $(BLUE)created .env from .env.example$(NC)"

dev-up: .env
	@echo -e ":: $(GREEN)Starting development services (PostgreSQL + Headscale)...$(NC)"
	@docker compose up -d --wait \
		&& echo -e "==> $(BLUE)Development services are ready$(NC)" \
		|| (echo -e "==> $(RED)Failed to start development services$(NC)" && exit 1)

dev-down:
	@echo -e ":: $(GREEN)Stopping development services...$(NC)"
	@docker compose down \
		&& echo -e "==> $(BLUE)Services stopped$(NC)"

dev-reset: .env
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

# All three depend on .env for the same reason dev-up does: every docker compose
# call below resolves MinIO's credentials through it, and the compose file uses
# the ':?' form, so a clone that has never run dev-up fails inside Compose rather
# than at a step that could have created the file. It only guarantees the file
# exists — a .env that predates a new required variable is still the operator's
# to reconcile, and the ':?' message names the variable when that happens.
walg-backup: .env
	@echo -e ":: $(GREEN)Taking a control-plane base backup...$(NC)"
	@./scripts/walg-backup.sh \
		&& echo -e "==> $(BLUE)Backup complete$(NC)" \
		|| (echo -e "==> $(RED)Backup failed$(NC)" && exit 1)

walg-restore: .env
	@echo -e ":: $(GREEN)Restoring the control-plane database...$(NC)"
	@./scripts/walg-restore.sh $(if $(MODE),$(MODE),verify) $(ARGS) \
		&& echo -e "==> $(BLUE)Restore complete$(NC)" \
		|| (echo -e "==> $(RED)Restore failed$(NC)" && exit 1)

walg-verify: .env
	@echo -e ":: $(GREEN)Verifying the control-plane backups...$(NC)"
	@./scripts/walg-verify.sh $(ARGS) \
		&& echo -e "==> $(BLUE)Verification complete$(NC)" \
		|| (echo -e "==> $(RED)Verification failed$(NC)" && exit 1)

install-hooks:
	@echo -e ":: $(GREEN)Installing git hooks...$(NC)"
	@git config core.hooksPath .githooks \
		&& echo -e "==> $(BLUE)Git hooks installed (using .githooks/)$(NC)" \
		|| (echo -e "==> $(RED)Failed to configure git hooks$(NC)" && exit 1)

.PHONY: all prepare build run-server run-agent run-cli test test-integration schemas
.PHONY: dev-up dev-down dev-reset dev-server dev-logs install-hooks
.PHONY: walg-backup walg-restore walg-verify
