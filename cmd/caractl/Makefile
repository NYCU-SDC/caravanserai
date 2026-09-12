GREEN = \033[0;32m
BLUE = \033[0;34m
RED = \033[0;31m
NC = \033[0m

ROOT_DIR = $(abspath ../..)
BIN_DIR = $(ROOT_DIR)/bin
BINARY = $(BIN_DIR)/caractl
# The package directory, not a single source file. Naming one file compiles
# exactly that file, so a second file in the same package is invisible to the
# build and it fails on symbols the package itself defines.
PKG = ./cmd/caractl

all: build

build:
	@echo -e ":: $(GREEN)Building caractl...$(NC)"
	@mkdir -p $(BIN_DIR)
	@cd $(ROOT_DIR) && go build -o $(BINARY) $(PKG) \
		&& echo -e "==> $(BLUE)Build completed successfully$(NC)" \
		|| (echo -e "==> $(RED)Build failed$(NC)" && exit 1)

run:
	@echo -e ":: $(GREEN)Running caractl...$(NC)"
	@cd $(ROOT_DIR) && go build -o $(BINARY) $(PKG) && \
		$(BINARY) \
		&& echo -e "==> $(BLUE)caractl exited$(NC)" \
		|| (echo -e "==> $(RED)caractl failed to run$(NC)" && exit 1)

.PHONY: all build run
