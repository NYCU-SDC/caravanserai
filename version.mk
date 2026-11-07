# Build metadata stamped into every binary through -ldflags (CARA-143). The
# version comes from scripts/release/version.sh, the same script the release
# packages use, so `make build` and a package built from the same commit agree.
# Computed once and exported, so the per-command Makefiles reuse it.
CARA_ROOT := $(abspath $(dir $(lastword $(MAKEFILE_LIST))))

ifndef CARA_VERSION
CARA_VERSION := $(shell $(CARA_ROOT)/scripts/release/version.sh)
endif
ifndef CARA_COMMIT
CARA_COMMIT := $(shell git -C $(CARA_ROOT) rev-parse --short HEAD 2>/dev/null || echo unknown)
endif
ifndef CARA_BUILD_TIME
CARA_BUILD_TIME := $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
endif
export CARA_VERSION CARA_COMMIT CARA_BUILD_TIME

GO_LDFLAGS := -X main.Version=$(CARA_VERSION) -X main.CommitHash=$(CARA_COMMIT) -X main.BuildTime=$(CARA_BUILD_TIME)
