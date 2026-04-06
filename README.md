# Caravanserai

A lightweight container orchestration system built for self-hosted clusters.
Caravanserai schedules Docker workloads across a fleet of nodes through a
central control plane — without requiring Kubernetes.

```
                      ┌─────────────────┐
  caractrl ──────────▶│   cara-server   │◀── Scheduler / Controller Manager
                      │  (control plane) │
                      └────────┬────────┘
                               │ HTTP API
              ┌────────────────┼────────────────┐
              ▼                ▼                ▼
        ┌──────────┐    ┌──────────┐    ┌──────────┐
        │cara-agent│    │cara-agent│    │cara-agent│
        │  node-01 │    │  node-02 │    │  node-03 │
        └────┬─────┘    └────┬─────┘    └────┬─────┘
             │ Docker API    │               │
          [containers]    [containers]    [containers]
```

## Components

| Binary | Role |
|--------|------|
| `cara-server` | Control-plane API server + Controller Manager |
| `cara-agent` | Per-node agent — reconciles containers via Docker |
| `caractrl` | CLI for managing Nodes and Projects |

## Concepts

### Node
A physical or virtual machine running `cara-agent`. Nodes self-register with
the control plane on startup. The Scheduler only assigns work to nodes whose
state is `Ready`.

### Project
A workload definition — a set of containers (services) that must be
co-located on a single node. Services share a Docker bridge network and
resolve each other by service name, exactly like Docker Compose.

**Lifecycle:**

```
Pending ──(scheduler)──▶ Scheduled ──(agent)──▶ Running
                                                    │
                                               (error) ▼
                                                  Failed
```

## Prerequisites

- Go 1.22+
- Docker (daemon accessible at `unix:///var/run/docker.sock` or via `DOCKER_HOST`)
- PostgreSQL (for `cara-server`)

## Quick Start

### 1. Build all binaries

```bash
make build
# Outputs: bin/cara-server  bin/cara-agent  bin/caractrl
```

### 2. Start PostgreSQL

```bash
docker run -d \
  --name caravanserai-db \
  -e POSTGRES_DB=caravanserai \
  -e POSTGRES_USER=postgres \
  -e POSTGRES_PASSWORD=password \
  -p 5432:5432 \
  postgres:16
```

### 3. Start the control-plane server

```bash
DATABASE_URL="postgresql://postgres:password@localhost:5432/caravanserai?sslmode=disable" \
  ./bin/cara-server
# Listening on 0.0.0.0:8080
```

### 4. Start the agent (on the same or a different machine)

```bash
SERVER_URL=http://localhost:8080 \
NODE_NAME=my-node \
  ./bin/cara-agent
# Agent registers itself, begins heartbeating and polling for work
```

### 5. Deploy a Project

```bash
./bin/caractrl apply -f examples/nginx-project.yaml
# project/nginx-demo created

./bin/caractrl get projects
# NAME         PHASE     NODE      CONDITIONS          AGE
# nginx-demo   Running   my-node   ContainersRunning   5s
```

### 6. Inspect and clean up

```bash
# List all nodes
./bin/caractrl get nodes

# Get a single project (JSON)
./bin/caractrl --output json get projects nginx-demo

# Delete the project (agent tears down all containers)
./bin/caractrl delete project nginx-demo
```

---

## Example Manifests

### Minimal nginx project

```yaml
# examples/nginx-project.yaml
apiVersion: caravanserai/v1
kind: Project
metadata:
  name: nginx-demo
spec:
  services:
    - name: web
      image: nginx:alpine
```

### Multi-service app with a volume

```yaml
apiVersion: caravanserai/v1
kind: Project
metadata:
  name: wordpress
spec:
  services:
    - name: db
      image: mysql:8
      env:
        - name: MYSQL_ROOT_PASSWORD
          value: "secret"
        - name: MYSQL_DATABASE
          value: "wp"
      volumeMounts:
        - name: mysql-data
          mountPath: /var/lib/mysql
    - name: app
      image: wordpress:latest
      env:
        - name: WORDPRESS_DB_HOST
          value: db          # resolves via the shared bridge network
        - name: WORDPRESS_DB_PASSWORD
          value: "secret"
        - name: WORDPRESS_DB_NAME
          value: "wp"
  volumes:
    - name: mysql-data
      type: Ephemeral
```

### Node manifest (manual registration)

```yaml
apiVersion: caravanserai/v1
kind: Node
metadata:
  name: edge-node-01
  labels:
    zone: hsinchu
spec:
  hostname: edge-01.local
  unschedulable: false
```

---

## Configuration

### cara-server

Configuration is read in order: `config.yaml` → `.env` → environment variables → CLI flags.

| Key | Env var | Default | Description |
|-----|---------|---------|-------------|
| `debug` | `DEBUG` | `false` | Enable debug logging |
| `host` | `HOST` | `0.0.0.0` | Listen address |
| `port` | `PORT` | `8080` | Listen port |
| `database_url` | `DATABASE_URL` | _(required)_ | PostgreSQL DSN |
| `otel_collector_url` | `OTEL_COLLECTOR_URL` | _(optional)_ | OTLP gRPC endpoint |

### cara-agent

| Key | Env var | Default | Description |
|-----|---------|---------|-------------|
| `debug` | `DEBUG` | `false` | Enable debug logging |
| `server_url` | `SERVER_URL` | `http://localhost:8080` | cara-server address |
| `node_name` | `NODE_NAME` | OS hostname | Name to register with |
| `heartbeat_interval` | `HEARTBEAT_INTERVAL` | `30s` | Heartbeat frequency |
| `docker_host` | `DOCKER_HOST` | `unix:///var/run/docker.sock` | Docker daemon endpoint |

### caractrl flags

Flags must appear **before** the subcommand:

```bash
./bin/caractrl [--server <url>] [--output <format>] <command>
```

| Flag | Default | Description |
|------|---------|-------------|
| `--server` | `http://localhost:8080` | cara-server URL |
| `--output` | `table` | Output format: `table` \| `json` \| `yaml` |

---

## API Reference

All endpoints are under `/api/v1/`.

### Nodes

| Method | Path | Description |
|--------|------|-------------|
| `POST` | `/api/v1/nodes` | Register a node |
| `GET` | `/api/v1/nodes` | List all nodes |
| `GET` | `/api/v1/nodes/{name}` | Get a single node |
| `DELETE` | `/api/v1/nodes/{name}` | Delete a node |
| `POST` | `/api/v1/nodes/{name}/heartbeat` | Send a heartbeat |

### Projects

| Method | Path | Description |
|--------|------|-------------|
| `POST` | `/api/v1/projects` | Create a project |
| `GET` | `/api/v1/projects` | List projects (`?phase=`, `?nodeRef=`) |
| `GET` | `/api/v1/projects/{name}` | Get a single project |
| `DELETE` | `/api/v1/projects/{name}` | Delete a project |
| `PATCH` | `/api/v1/projects/{name}/status` | Update project status (agent only) |

---

## Development

```bash
# Set up git hooks (run once after cloning)
make install-hooks

# Run all unit tests
make test

# Run integration tests (requires Docker)
make test-integration

# Regenerate JSON Schemas from Go types
make schemas

# Build a single binary
make -C cmd/cara-server build
make -C cmd/cara-agent  build
make -C cmd/caractl    build
```

The pre-commit hook automatically regenerates `schemas/` when `api/v1/` or
`cmd/schemagen/` files are staged, so schema files stay in sync with Go types.

### Docker resource naming

The agent uses deterministic names so reconciliation is stateless:

| Resource | Pattern |
|----------|---------|
| Network | `cara-{projectName}` |
| Container | `{projectName}-{serviceName}` |
| Volume | `cara-{projectName}-{volumeName}` |

Labels attached to every container:

```
cara.project = <projectName>
cara.service  = <serviceName>
```

---

## Project Phases

| Phase | Set by | Meaning |
|-------|--------|---------|
| `Pending` | Server | Accepted; awaiting scheduler |
| `Scheduled` | Scheduler | Node assigned; agent not yet confirmed |
| `Running` | Agent | All containers up |
| `Failed` | Agent | Terminal error (see Conditions) |
| `Terminating` | Server | Deletion in progress |
