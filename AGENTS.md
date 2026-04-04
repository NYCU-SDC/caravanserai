# AGENTS.md — Caravanserai

## Project Overview

Caravanserai is a lightweight container orchestration platform (PaaS) for Docker
Compose workloads. Kubernetes-inspired but designed for heterogeneous,
cross-network environments with no shared storage. Three Go binaries from one
module (`NYCU-SDC/caravanserai`): `cara-server` (control-plane API + controller
manager), `cara-agent` (per-node Docker reconciler), `caractl` (CLI client).

## Build & Run

```bash
make build                    # Build all 3 binaries to bin/
make run-server               # Build + run cara-server
make run-agent                # Build + run cara-agent
make dev-server               # Start PostgreSQL + build + run cara-server
make prepare                  # go mod download
```

## Testing

```bash
# Unit tests
make test                     # go test -cover ./...

# Integration tests (requires Docker — spins up a disposable PostgreSQL)
make test-integration         # go test -v -tags e2e -timeout 120s ./test/integration/...
make test-integration VERBOSE=1  # With verbose infrastructure logging

# Run a single test by name
go test -v -run TestNodeCRUD ./test/integration/... -tags e2e -timeout 120s

# Run tests for a specific package
go test -v ./internal/store/postgres/...
```

Integration tests use `//go:build e2e` and require `-tags e2e` to compile.

## Dev Environment

```bash
make dev-up                   # Start PostgreSQL 16 via docker compose
make dev-down                 # Stop PostgreSQL (preserves data)
make dev-reset                # Wipe data volume + restart PostgreSQL
make dev-logs                 # Tail PostgreSQL logs
```

The `.env` file at the root configures `DATABASE_URL` and `DEBUG=true`.

## Where to Find Context

### Notion (via MCP)

When a Notion MCP server is available, search for **"Caravanserai"** to find
project documentation. Key pages:

| Page | Content |
|------|---------|
| **Caravanserai Project** | PRD — full product vision, resource YAML specs (Node, Project, DeploymentGroup, Secret, NotificationChannel), design constraints, and long-term roadmap |
| **Caravanserai 專案結構 & MVP 規格** | MVP scope — directory layout, initial Node/Project YAML specs, what is deferred vs. included, and the difference from the PRD |
| **Control Loop / Reconciliation 模型** | Tech spec — how the Controller/Manager/Reconcile pattern works, comparison with Kubernetes controller-runtime, data flow diagrams |
| **Caravanserai 啟用流程** | Tech spec — how nodes join the cluster via Headscale/tsnet, zero-trust registration, Master-side identity verification |
| **Caravanserai Master 災難復原機制** | Tech spec — cold-standby resurrection via Object Storage, backup strategy (WAL-G), split-brain prevention, network re-convergence |

Use these pages to understand **why** design decisions were made before
proposing changes.

### In-repo Resources

| Location | What you'll find |
|----------|-----------------|
| `README.md` | Architecture diagram, API reference (all REST endpoints), project lifecycle, Docker naming conventions, configuration tables |
| `examples/` | Sample YAML manifests (`nginx-project.yaml`, `multi-service.yaml`) for testing deployments |
| `.github/PULL_REQUEST_TEMPLATE.md` | PR format — types: Feature / Fix / Docs / Refactor / CI / Test / Chore |
| `.opencode/skills/dev-environment/` | Step-by-step guide to start the full dev stack (PostgreSQL + cara-server + cara-agent), including background-process mode for LLM agents |
| `.opencode/skills/e2e-testing/` | How to run integration tests and manual full-stack E2E verification with real Docker containers |

### Key Source Files

| File | Purpose |
|------|---------|
| `api/v1/` | Shared API types (Kubernetes-style: TypeMeta, ObjectMeta, Spec, Status) |
| `internal/store/interface.go` | Store contract + sentinel errors (`ErrNotFound`, `ErrAlreadyExists`) |
| `internal/server/controller/controller.go` | `Controller` and `Seeder` interfaces — the core reconciliation contract |
| `internal/server/controller/manager.go` | Controller Manager — work-queue, worker pools, error-backoff requeue |
| `internal/event/bus.go` | In-process event bus for inter-component signaling |

## Code Style

Follow standard Go best practices. The project-specific conventions worth noting:

### Imports

Three groups separated by blank lines: (1) stdlib, (2) internal module, (3) external.

```go
import (
    "encoding/json"
    "net/http"

    v1 "NYCU-SDC/caravanserai/api/v1"
    "NYCU-SDC/caravanserai/internal/store"

    "go.uber.org/zap"
)
```

Standard aliases: `v1` for `api/v1`, `pgstore` for `store/postgres`,
`nodehandler`/`projecthandler` for handler packages.

### Error Handling

- Wrap errors with `fmt.Errorf("context: %w", err)` using lowercase, colon-separated prefixes
- Define sentinel errors as `var ErrXxx = errors.New(...)` in the package that owns them
- Check sentinels with `errors.Is(err, store.ErrNotFound)`
- HTTP responses: descriptive messages for 4xx, generic `"internal server error"` for 5xx
- Explicitly discard errors with `_ =` (e.g., `_ = resp.Body.Close()`)
- No custom error types — use sentinel vars + `fmt.Errorf` wrapping only

```go
return nil, fmt.Errorf("postgres: get node %q: %w", name, err)
```

### Logging

- Use `*zap.Logger` exclusively (never SugaredLogger)
- Logger is the first parameter in constructors: `NewHandler(logger *zap.Logger, ...)`
- Use typed field constructors: `zap.String()`, `zap.Error()`, `zap.Duration()`, etc.
- Create sub-loggers with `.With()` for scoped context
- Levels: Debug (routine), Info (state changes), Warn (recoverable), Error (retryable failures), Fatal (startup only)

### Interfaces

- **Consumer-side** narrow interfaces: each controller defines its own store interface
  in the file where it is consumed
- **Provider-side** broad interface in `store/interface.go`
- Compile-time checks: `var _ store.Store = (*Store)(nil)`
- Adapter structs in `cmd/` bridge broad implementations to narrow controller interfaces

### Handler Pattern

Each handler package (`handler/node/`, `handler/project/`) follows: package doc
listing all routes, `Handler` struct with `logger` + narrow store interface,
`NewHandler` constructor, `RegisterRoutes(mux, middleware)` method, private
handler methods + `writeJSON`/`writeError` helpers.

### Controller Pattern

Kubernetes-inspired reconcile loop: struct with `logger`, narrow store
interface(s), optional `*event.Bus`. `Name() string` returns kebab-case ID.
`Reconcile(ctx, name) (Result, error)` is idempotent core logic. Optional
`Seed(ctx, enqueue)` implements `Seeder` via event bus + periodic ticker.

### Testing Conventions

- Integration tests use `//go:build e2e` tag and `TestMain` with `dockertest`
- Use `testify` — `require` for fatal, `assert` for non-fatal
- Test helpers call `t.Helper()`; `zap.NewNop()` by default
- Tests run against `httptest.NewServer` with real handlers and real PostgreSQL

## PR Convention

PR types: Feature / Fix / Docs / Refactor / CI / Test / Chore. Include purpose and
link to issues. See `.github/PULL_REQUEST_TEMPLATE.md`.
