---
name: e2e-testing
description: Run end-to-end tests for Caravanserai — both automated integration tests and manual full-stack verification with real Docker containers
---

## Automated integration tests

```bash
make test-integration
# With verbose infrastructure logging:
make test-integration VERBOSE=1
```

- Build tag: `//go:build e2e` — only compiled with `-tags e2e`
- Location: `test/integration/`
- `TestMain` uses `dockertest` to start a disposable PostgreSQL 16 container
- Tests run against `httptest.NewServer` with real handlers and real `pgstore`
- Container is automatically removed after tests complete
- No manual setup required — just run the make target

Run a single test by name:

```bash
go test -v -run TestNodeCRUD ./test/integration/... -tags e2e -timeout 120s
```

### Adding new integration tests

New test files in `test/integration/` must:

1. Start with `//go:build e2e`
2. Use the shared `suite.serverURL` and `suite.client` from `TestMain`
3. Use `doRequest()`, `mustMarshal()`, `mustDecodeBody()` helpers
4. Clean up any created resources to avoid test pollution

Example:

```go
//go:build e2e

func TestProjectCRUD(t *testing.T) {
    body := mustMarshal(t, v1.Project{
        ObjectMeta: v1.ObjectMeta{Name: "test-project"},
        Spec: v1.ProjectSpec{
            Services: []v1.ServiceDef{{Name: "web", Image: "nginx:alpine"}},
        },
    })
    resp := doRequest(t, http.MethodPost, "/api/v1/projects", body)
    require.Equal(t, http.StatusCreated, resp.StatusCode)
    // ... continue with get, list, delete, etc.
}
```

## Manual full-stack E2E testing

Use this to verify the complete flow: server scheduling a Project to a node, the agent creating real Docker containers, and cleanup on deletion.

### 1. Start all components

Load the `dev-environment` skill and follow its startup procedure, or run:

```bash
make dev-up && make build
./bin/cara-server > /tmp/cara-server.log 2>&1 &
sleep 3
curl -sf http://localhost:8080/api/healthz  # must exit 0
NODE_NAME=test-node ./bin/cara-agent > /tmp/cara-agent.log 2>&1 &
sleep 5
```

### 2. Verify node registration

```bash
./bin/caractl get nodes
# Success: output contains "test-node" with state "Ready"
# If "NotReady": wait 5 more seconds and retry (heartbeat interval is 30s)
```

### 3. Deploy a test Project

```bash
./bin/caractl apply -f examples/nginx-project.yaml
# Success: output contains "created"
```

### 4. Wait for Project to reach Running phase

Phase progression: `Pending` → `Scheduled` (scheduler assigns node) → `Running` (agent started containers).

Poll every 5 seconds, max 2 minutes:

```bash
for i in $(seq 1 24); do
  OUTPUT=$(./bin/caractl get projects 2>&1)
  echo "[$((i*5))s] $OUTPUT"
  if echo "$OUTPUT" | grep -q "nginx-demo.*Running"; then
    echo "=== Project reached Running ==="
    break
  fi
  if echo "$OUTPUT" | grep -q "nginx-demo.*Failed"; then
    echo "=== Project FAILED ==="
    ./bin/caractl --output yaml get projects nginx-demo
    break
  fi
  sleep 5
done
```

If phase is `Failed`, inspect conditions:

```bash
./bin/caractl --output yaml get projects nginx-demo
```

### 5. Verify Docker containers

```bash
# Container must exist and be running
docker ps --filter "label=cara.project=nginx-demo" | grep -q "nginx-demo-web"

# Network must exist
docker network ls --filter "name=cara-nginx-demo" | grep -q "cara-nginx-demo"
```

### 6. Test cleanup

```bash
./bin/caractl delete project nginx-demo
# Wait for agent to process termination
sleep 15
```

### 7. Verify cleanup

```bash
# Project gone (empty list or 404)
./bin/caractl get projects

# No containers with this label
docker ps --filter "label=cara.project=nginx-demo" --format '{{.Names}}' | grep -qv "nginx-demo"

# No network
docker network ls --filter "name=cara-nginx-demo" --format '{{.Name}}' | grep -qv "cara-nginx-demo"
```

### Multi-service E2E test

```bash
./bin/caractl apply -f examples/multi-service.yaml
# Poll for Running phase (same loop as above, replace "nginx-demo" with "wordpress")

# Verify: two containers running
docker ps --filter "label=cara.project=wordpress" --format '{{.Names}}'
# Expected: wordpress-db, wordpress-app

# Verify: volume created
docker volume ls --filter "name=cara-wordpress-mysql-data" --format '{{.Name}}'

# Clean up
./bin/caractl delete project wordpress
sleep 15
```

### Success criteria

All of these must be true for a passing E2E test:

1. `./bin/caractl get nodes` shows node with state `Ready`
2. `./bin/caractl get projects` shows project with phase `Running`
3. `docker ps --filter "label=cara.project=<name>"` shows expected containers
4. `docker network ls --filter "name=cara-<name>"` shows the project network
5. After `caractl delete project <name>` + 15s wait: containers, network, and volumes are gone
6. `./bin/caractl get projects` no longer lists the deleted project

### Troubleshooting

- Project stuck in `Pending` → no Ready nodes; run `./bin/caractl get nodes` and verify state is `Ready`
- Project stuck in `Scheduled` → agent hasn't polled yet (polls every 10s); check `/tmp/cara-agent.log` for Docker errors, image pull failures, or port conflicts
- Project goes to `Failed` → run `./bin/caractl --output yaml get projects <name>` and inspect `.status.conditions`
- Containers not cleaned up after delete → agent may still be processing; wait 10 more seconds then check `/tmp/cara-agent.log`

## Unit tests

```bash
make test
# Runs: go test -cover ./... (excludes e2e-tagged files)
```
