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

# install-cli wires up a `cara` shortcut for caractl and a default --server, so
# the everyday command is `cara get nodes` instead of
# `./bin/caractl --server http://... get nodes`.
#
# It picks the rc file from the login shell ($SHELL: zsh vs bash), points the
# alias at the absolute binary path so it works from any working directory, and
# is idempotent — a managed block between markers is replaced on every run
# rather than appended, so re-running never duplicates lines.
#
# The control-plane address is overridable per machine:
#   make install-cli                                     # localhost (server box)
#   make install-cli CARA_SERVER=http://10.1.253.7:8080  # remote (agent box)
CARA_SERVER ?= http://127.0.0.1:8080
CARACTL_BIN := $(abspath bin/caractl)

install-cli: build
	@echo -e ":: $(GREEN)Installing cara shortcut...$(NC)"
	@shell_name=$$(basename "$${SHELL:-/bin/bash}"); \
	case "$$shell_name" in \
	  zsh)  rc="$$HOME/.zshrc" ;; \
	  bash) rc="$$HOME/.bashrc" ;; \
	  *)    rc="$$HOME/.profile" ;; \
	esac; \
	tmp=$$(mktemp); \
	[ -f "$$rc" ] && sed '/# cara-cli-BEGIN/,/# cara-cli-END/d' "$$rc" > "$$tmp" || true; \
	{ \
	  echo "# cara-cli-BEGIN (managed by make install-cli)"; \
	  echo "alias cara='$(CARACTL_BIN)'"; \
	  echo "export CARA_SERVER='$(CARA_SERVER)'"; \
	  echo "# cara-cli-END"; \
	} >> "$$tmp"; \
	mv "$$tmp" "$$rc"; \
	echo -e "==> $(BLUE)wrote 'cara' alias + CARA_SERVER=$(CARA_SERVER) to $$rc$(NC)"; \
	echo -e "  -> run: $(GREEN)source $$rc$(NC)  (or open a new terminal)"

uninstall-cli:
	@shell_name=$$(basename "$${SHELL:-/bin/bash}"); \
	case "$$shell_name" in \
	  zsh)  rc="$$HOME/.zshrc" ;; \
	  bash) rc="$$HOME/.bashrc" ;; \
	  *)    rc="$$HOME/.profile" ;; \
	esac; \
	if [ -f "$$rc" ]; then \
	  tmp=$$(mktemp); \
	  sed '/# cara-cli-BEGIN/,/# cara-cli-END/d' "$$rc" > "$$tmp" && mv "$$tmp" "$$rc"; \
	  echo -e "==> $(BLUE)removed cara shortcut from $$rc$(NC)"; \
	fi

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

test-orphan-e2e:
	@echo -e ":: $(GREEN)Running isolated orphan-cleanup rehearsal (about 4 minutes)...$(NC)"
	@./scripts/e2e/orphan-cleanup.sh \
		&& echo -e "==> $(BLUE)Orphan-cleanup rehearsal passed$(NC)" \
		|| (echo -e "==> $(RED)Orphan-cleanup rehearsal failed$(NC)" && exit 1)

test-recovery-e2e:
	@echo -e ":: $(GREEN)Running isolated local-recovery rehearsal (about 5 minutes)...$(NC)"
	@./scripts/e2e/local-recovery.sh \
		&& echo -e "==> $(BLUE)Local-recovery rehearsal passed$(NC)" \
		|| (echo -e "==> $(RED)Local-recovery rehearsal failed$(NC)" && exit 1)

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

.PHONY: all prepare build run-server run-agent run-cli test test-integration test-orphan-e2e test-recovery-e2e schemas
.PHONY: dev-up dev-down dev-reset dev-server dev-logs install-hooks
.PHONY: walg-backup walg-restore walg-verify
