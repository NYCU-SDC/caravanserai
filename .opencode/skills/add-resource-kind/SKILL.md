---
name: add-resource-kind
description: Add a new Kubernetes-style resource kind to Caravanserai — API types, store, handler, CLI, controller, wiring, tests, and example manifest
---

## When to use this skill

Use this when adding a new resource kind (e.g. DeploymentGroup, Secret, NotificationChannel) to Caravanserai. This is a 9-step procedure that touches 8+ directories. Follow every step in order. Do not skip steps.

## Overview of steps

1. Define API types in `api/v1/`
2. Add store interface methods in `internal/store/interface.go`
3. Implement store methods in `internal/store/postgres/store.go`
4. Create HTTP handler in `internal/server/handler/<kind>/`
5. Add CLI support in `internal/cli/`
6. Wire handler + CLI into entry points (`cmd/cara-server/main.go`, `cmd/caractl/main.go`)
7. (Optional) Add controller(s) in `internal/server/controller/`
8. Add integration test in `test/integration/`
9. Add example manifest in `examples/`

No database migration is needed — the single `resources` table stores all kinds via `(kind, name)` primary key with JSONB columns for spec/status.

---

## Step 1: API types — `api/v1/<kind>_types.go`

Create `api/v1/<kind>_types.go`. Follow the exact pattern of `node_types.go` / `project_types.go`:

```go
package v1

// <Kind>Spec defines the desired state of a <Kind>.
type <Kind>Spec struct {
    // Fields with both json and yaml tags
    FieldName string `json:"fieldName" yaml:"fieldName"`
}

// <Kind>Status defines the observed state of a <Kind>.
type <Kind>Status struct {
    Phase      <Kind>Phase `json:"phase" yaml:"phase"`
    Conditions []Condition `json:"conditions,omitempty" yaml:"conditions,omitempty"`
}

// <Kind> is the top-level resource.
type <Kind> struct {
    TypeMeta `json:",inline" yaml:",inline"`
    Metadata ObjectMeta   `json:"metadata" yaml:"metadata"`
    Spec     <Kind>Spec   `json:"spec" yaml:"spec"`
    Status   <Kind>Status `json:"status" yaml:"status"`
}

// <Kind>List wraps a slice of <Kind>.
type <Kind>List struct {
    TypeMeta `json:",inline" yaml:",inline"`
    Items    []<Kind> `json:"items" yaml:"items"`
}
```

Rules:
- Package is `v1`, import only `"time"` if needed
- Every field has both `json` and `yaml` struct tags
- `TypeMeta` is embedded with `,inline` tags
- `ObjectMeta` is a named field `Metadata` with tag `json:"metadata" yaml:"metadata"`
- If the kind has lifecycle phases, define a `type <Kind>Phase string` with typed constants
- Slices use `omitempty` in tags
- Only define types — no methods, no constructors

## Step 2: Store interface — `internal/store/interface.go`

Add a new `<Kind>Store` interface and compose it into `Store`:

```go
// <Kind>Store defines persistence operations for <Kind> resources.
type <Kind>Store interface {
    Create<Kind>(ctx context.Context, obj *v1.<Kind>) error
    Get<Kind>(ctx context.Context, name string) (*v1.<Kind>, error)
    List<Kind>s(ctx context.Context) ([]v1.<Kind>, error)
    Update<Kind>(ctx context.Context, obj *v1.<Kind>) error
    Delete<Kind>(ctx context.Context, name string) error
    Update<Kind>Status(ctx context.Context, obj *v1.<Kind>) error
}
```

Then add the new interface to the `Store` composition:

```go
type Store interface {
    NodeStore
    ProjectStore
    <Kind>Store   // ← add this line
    Close()
}
```

Rules:
- Methods accept `context.Context` as first param
- Use `*v1.<Kind>` for single objects, `[]v1.<Kind>` for lists
- Return `error` from all methods
- Add phase-filtered list methods only if the kind has lifecycle phases (e.g. `List<Kind>sByPhase`)
- `ErrNotFound` and `ErrAlreadyExists` are already defined — reuse them

## Step 3: Postgres store — `internal/store/postgres/store.go`

Implement every method from the new interface. Follow the exact pattern of existing Node/Project methods:

```go
func (s *Store) Create<Kind>(ctx context.Context, obj *v1.<Kind>) error {
    specJSON, statusJSON, labelsJSON, annotationsJSON, err := marshalFields(obj.Spec, obj.Status, obj.Metadata.Labels, obj.Metadata.Annotations)
    if err != nil {
        return fmt.Errorf("postgres: marshal <kind> %q: %w", obj.Metadata.Name, err)
    }

    _, err = s.pool.Exec(ctx,
        `INSERT INTO resources (kind, name, phase, spec, status, labels, annotations, created_at, updated_at)
         VALUES ($1, $2, $3, $4, $5, $6, $7, NOW(), NOW())`,
        "<Kind>", obj.Metadata.Name, string(obj.Status.Phase),
        specJSON, statusJSON, labelsJSON, annotationsJSON,
    )
    if err != nil {
        if isUniqueViolation(err) {
            return store.ErrAlreadyExists
        }
        return fmt.Errorf("postgres: create <kind> %q: %w", obj.Metadata.Name, err)
    }

    s.publish("<kind>.created", obj.Metadata.Name)
    return nil
}
```

Rules:
- Kind string in SQL is PascalCase matching the Go type name (e.g. `"DeploymentGroup"`)
- Error messages use pattern `"postgres: <verb> <kind> %q: %w"` with lowercase kind
- Use `marshalFields()` helper (already exists) for JSONB serialization
- Use `unmarshalFields()` helper (already exists) for JSONB deserialization
- Publish events on create and status-update only (not on full update or delete)
- Event topics follow pattern `Topic<Kind>Created`, `Topic<Kind>Updated` — define these as constants in the event bus package if controllers will use them
- If the kind has phases, set the `phase` column from `obj.Status.Phase`
- If the kind has no phase, store `""` in the phase column

After implementing, verify the compile-time check still passes. The existing line is:

```go
var _ store.Store = (*Store)(nil)
```

This will fail to compile if any new interface method is missing — that is the intended verification.

## Step 4: HTTP handler — `internal/server/handler/<kind>/handler.go`

Create directory `internal/server/handler/<kind>/` with a single `handler.go` file.

```go
// Package <kind> provides HTTP handlers for <Kind> resources.
//
// Routes:
//
//	POST   /api/v1/<kind>s          Create a <Kind>
//	GET    /api/v1/<kind>s          List <Kind>s
//	GET    /api/v1/<kind>s/{name}   Get a <Kind>
//	DELETE /api/v1/<kind>s/{name}   Delete a <Kind>
package <kind>
```

Structure:
- `Handler` struct: `logger *zap.Logger` + narrow store interface (use `store.<Kind>Store` or define a local interface if only a subset of methods is needed)
- `NewHandler(logger *zap.Logger, s <StoreInterface>) *Handler` constructor
- `RegisterRoutes(mux *http.ServeMux, basicMiddleware *middleware.Set)` — mounts all routes
- Private handler methods: `create<Kind>`, `list<Kind>s`, `get<Kind>`, `delete<Kind>`
- `writeJSON(w, status, data)` and `writeError(w, status, msg)` helpers (copy from existing handlers)
- `errorResponse` struct: `Error string \`json:"error"\``

Rules:
- Route prefix is `/api/v1/<kind>s` (plural, lowercase)
- Path parameter for single resources: `{name}`
- `create` returns 201 with the created object
- `list` returns 200 with `<Kind>List{TypeMeta: v1.TypeMeta{APIVersion: v1.APIVersion, Kind: "<Kind>"}, Items: items}`
- `get` returns 200 or 404 (check `errors.Is(err, store.ErrNotFound)`)
- `delete` returns 204 on success, 404 if not found
- `create` checks `errors.Is(err, store.ErrAlreadyExists)` and returns 409
- Set `TypeMeta` on the resource in `create` before storing: `obj.APIVersion = v1.APIVersion; obj.Kind = "<Kind>"`
- Validate required fields in `create` and return 400 with descriptive message
- For 5xx errors, always return generic `"internal server error"` message
- Import grouping: stdlib, internal module (`v1`, `store`), external (`zap`, `middleware`)

## Step 5: CLI support — `internal/cli/`

Three files need changes:

### `client.go` — Add HTTP client methods

```go
func (c *Client) Get<Kind>s() (*v1.<Kind>List, error) { ... }
func (c *Client) Get<Kind>(name string) (*v1.<Kind>, error) { ... }
func (c *Client) Delete<Kind>(name string) error { ... }
```

In `ApplyResource`, add a case to the Kind switch:

```go
case "<Kind>":
    return c.apply<Kind>(resource)
```

Add the private `apply<Kind>` method (POST to `/api/v1/<kind>s`).

### `cmd_get.go` — Add get subcommand

Add `newGet<Kind>sCmd()` following the pattern of `newGetNodesCmd()` / `newGetProjectsCmd()`. Register it in `NewGetCmd()`:

```go
cmd.AddCommand(newGet<Kind>sCmd())
```

### `cmd_delete.go` — Add delete subcommand

Add `newDelete<Kind>Cmd()` following existing pattern. Register it in `NewDeleteCmd()`.

### `output.go` — Add table formatting

Add `Print<Kind>(obj)` and `Print<Kind>List(list)` methods to `Printer`. Table columns follow the pattern:

```
NAME    <PHASE-OR-KEY-STATUS>    AGE
```

Use `humanAge()` for the AGE column. Use `latestConditionReason()` if the kind has conditions.

### `cmd_apply.go` — No changes needed

`ApplyResource` dispatches by Kind, so the only change is in `client.go` (the switch case).

## Step 6: Wiring

### `cmd/cara-server/main.go`

1. Import the new handler package with a descriptive alias:

```go
<kind>handler "NYCU-SDC/caravanserai/internal/server/handler/<kind>"
```

2. Register the handler (after existing `apiSrv.Register` calls):

```go
apiSrv.Register(<kind>handler.NewHandler(logger, pgStore))
```

If the handler needs multiple store interfaces, pass `pgStore` multiple times (it implements `store.Store` which composes all sub-interfaces).

3. If controllers are needed, add adapter structs and register them (see Step 7).

### `cmd/caractl/main.go`

No changes needed — `NewGetCmd()`, `NewDeleteCmd()`, `NewApplyCmd()` are already registered. The new subcommands are added inside `internal/cli/` in Step 5.

## Step 7: (Optional) Controllers — `internal/server/controller/`

Only add controllers if the kind needs automated reconciliation (phase transitions, cleanup, scheduling, etc.). Skip this step if the kind is purely CRUD.

Create `internal/server/controller/<kind>_<purpose>.go`:

```go
type <Purpose><Kind>Store interface {
    // Only the methods this controller needs — consumer-side narrow interface
}

type <Kind><Purpose>Controller struct {
    logger *zap.Logger
    store  <Purpose><Kind>Store
    bus    *event.Bus
}

func New<Kind><Purpose>Controller(logger *zap.Logger, store <Purpose><Kind>Store, bus *event.Bus) *<Kind><Purpose>Controller { ... }
func (c *<Kind><Purpose>Controller) Name() string { return "<kind>-<purpose>" }  // kebab-case
func (c *<Kind><Purpose>Controller) Reconcile(ctx context.Context, name string) (Result, error) { ... }
```

Rules:
- Define a narrow store interface in the same file — only the methods this controller calls
- Use controller-local type aliases for phases/states (e.g. `type <Kind>Phase string` with constants) to avoid importing `v1` in controllers
- `Name()` returns kebab-case (e.g. `"secret-rotation"`)
- `Reconcile` must be idempotent — safe to call multiple times for the same key
- Implement `Seeder` interface if the controller needs to self-schedule work (event bus subscription + periodic ticker)
- Seed interval constants follow pattern `<kind><purpose>ResyncInterval`

### Wiring controllers in `cmd/cara-server/main.go`

1. Define adapter struct(s) that bridge `*pgstore.Store` to the controller's narrow interface:

```go
type <kind><Purpose>StoreAdapter struct{ s *pgstore.Store }
func (a *<kind><Purpose>StoreAdapter) MethodName(ctx context.Context, ...) (...) {
    // Call a.s.<StoreMethod>(...) and convert v1 types ↔ controller types
}
```

2. Register with the controller manager:

```go
ctrlManager.Add(controller.New<Kind><Purpose>Controller(logger, &<kind><Purpose>StoreAdapter{pgStore}, eventBus))
```

### Event topics

If controllers need to react to this kind's lifecycle events, add topic constants in `internal/event/bus.go`:

```go
const (
    Topic<Kind>Created = "<kind>.created"
    Topic<Kind>Updated = "<kind>.updated"
)
```

## Step 8: Integration test — `test/integration/<kind>_test.go`

Create `test/integration/<kind>_test.go`:

```go
//go:build e2e

package integration

// Test<Kind>CRUD tests the full lifecycle of a <Kind> resource.
func Test<Kind>CRUD(t *testing.T) {
    // 1. POST /api/v1/<kind>s → 201
    // 2. Duplicate POST → 409
    // 3. GET /api/v1/<kind>s (list) → 200, verify count
    // 4. GET /api/v1/<kind>s/{name} → 200, verify fields
    // 5. DELETE /api/v1/<kind>s/{name} → 204
    // 6. GET after delete → 404
}
```

Rules:
- Must start with `//go:build e2e`
- Use shared `suite.serverURL` and `suite.client` from `TestMain` in `node_test.go`
- Use existing helpers: `doRequest()`, `mustMarshal()`, `mustDecodeBody()`, `drainBody()`
- Use `require` for fatal assertions, `assert` for non-fatal
- Clean up created resources at the end (or use `t.Cleanup`)

The handler must also be registered in `TestMain`. Edit `node_test.go` (or whatever file contains `TestMain`) to add:

```go
apiSrv.Register(<kind>handler.NewHandler(testLogger, pgStore))
```

## Step 9: Example manifest — `examples/<kind>.yaml`

Create a minimal example manifest:

```yaml
apiVersion: caravanserai/v1
kind: <Kind>
metadata:
  name: example-<kind>
spec:
  # minimal valid spec fields
```

Use this to test `caractl apply -f examples/<kind>.yaml`.

---

## Verification checklist

After completing all steps, run in order:

```bash
# 1. Compile check — catches missing interface implementations
go build ./...

# 2. Unit tests
make test

# 3. Integration tests (requires Docker)
make test-integration
```

If `go build` fails with a missing method error on `var _ store.Store = (*Store)(nil)`, a store method is not yet implemented. If a controller adapter fails to compile, a narrow interface method is missing from the adapter struct.
