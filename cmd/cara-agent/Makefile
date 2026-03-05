GREEN = \033[0;32m
BLUE = \033[0;34m
RED = \033[0;31m
NC = \033[0m

ROOT_DIR = $(abspath ../..)
BIN_DIR = $(ROOT_DIR)/bin
BINARY = $(BIN_DIR)/cara-agent
MAIN = cmd/cara-agent/main.go

all: build

build:
	@echo -e ":: $(GREEN)Building cara-agent...$(NC)"
	@mkdir -p $(BIN_DIR)
	@cd $(ROOT_DIR) && go build -o $(BINARY) $(MAIN) \
		&& echo -e "==> $(BLUE)Build completed successfully$(NC)" \
		|| (echo -e "==> $(RED)Build failed$(NC)" && exit 1)

run:
	@echo -e ":: $(GREEN)Starting cara-agent...$(NC)"
	@cd $(ROOT_DIR) && go build -o $(BINARY) $(MAIN) && \
		$(BINARY) \
		&& echo -e "==> $(BLUE)cara-agent shut down$(NC)" \
		|| (echo -e "==> $(RED)cara-agent failed to start$(NC)" && exit 1)

.PHONY: all build run
