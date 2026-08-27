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
| `PUT` | `/api/v1/nodes/{name}` | Update a node's spec |
| `GET` | `/api/v1/nodes` | List all nodes |
| `GET` | `/api/v1/nodes/{name}` | Get a single node |
| `DELETE` | `/api/v1/nodes/{name}` | Delete a node |
| `POST` | `/api/v1/nodes/{name}/heartbeat` | Send a heartbeat |
| `POST` | `/api/v1/nodes/{name}/probe` | Server-side reachability probe — server dials the agent's `/healthz` via the agent dialer (see `internal/server/agentdialer/`) and reports latency + status. When the server is joined to the overlay (see below) the dialer routes this over the agent's overlay IP via a tsnet-backed transport; otherwise it uses the default transport. Also surfaced as `caractl node probe <name>`. |

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

# Start local PostgreSQL and Headscale
make dev-up

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

### Local Headscale

`make dev-up` starts a development Headscale control plane at
`http://localhost:8082` using the pinned `headscale/headscale:v0.29.2` image.
Its config lives in `deploy/dev/headscale/config.yaml`, and its state is stored
in the `headscale-data` Docker volume. Use `make dev-reset` to wipe that state.

To create a local agent pre-auth key for future overlay work:

```bash
docker compose exec headscale headscale users create cara-node
docker compose exec headscale headscale users list
docker compose exec headscale headscale preauthkeys create \
  --user <cara-node-id> \
  --expiration 24h
```

The `cara-node` user only needs to be created once per Headscale data volume.
If it already exists, use the numeric ID from `users list`; Headscale v0.29
does not accept the username in `preauthkeys create --user`.

#### Joining the overlay from cara-agent

Overlay networking is **opt-in**: `cara-agent` joins the Headscale mesh only
when both a control-plane URL and a pre-auth key file are configured. With
neither set, the agent runs on the underlay as before.

Write the pre-auth key to a file (never commit it), then start the agent
pointing at the dev Headscale:

```bash
docker compose exec headscale headscale preauthkeys create --user 1 --expiration 24h > preauth.key

./bin/cara-agent \
  --headscale-url http://localhost:8082 \
  --preauth-key-file ./preauth.key
```

On startup the agent blocks until Headscale assigns an overlay IP, logs it
(`Joined Headscale overlay … overlay_ip=100.64.0.x`), and registers as a node
you can see with `docker compose exec headscale headscale nodes list`. A join
failure caused by a bad/expired key fails fast with a readable error; the agent
never silently falls back to the underlay once overlay is requested.

| Setting | Flag | Env / YAML | Notes |
|---|---|---|---|
| Control-plane URL | `--headscale-url` | `HEADSCALE_URL` / `headscale_url` | Enables overlay when set |
| Pre-auth key file | `--preauth-key-file` | `HEADSCALE_PREAUTH_KEY_FILE` / `preauth_key_file` | Path to a file holding the key; the key is never logged |
| Overlay hostname | `--overlay-hostname` | `OVERLAY_HOSTNAME` / `overlay_hostname` | Optional; defaults to the node name |

> The dev Headscale listens on host port `8082`; the agent ingress proxy
> defaults to `:8081`. The two no longer clash, so no `--proxy-listen-addr`
> override is needed when running an agent on the same host.

#### Joining the overlay from cara-server

For the server to reach a NAT-ed agent it must be on the overlay too: the
agent's overlay IP lives in the `100.64.0.0/10` CGNAT range, which the host
kernel has no route to. Overlay networking is **opt-in** on the server as well —
it joins Headscale only when both a control-plane URL and a pre-auth key file
are configured. With neither set, `cara-server` uses the default HTTP transport
(existing behaviour, no overlay).

```bash
docker compose exec headscale headscale preauthkeys create --user 1 --expiration 24h > server-preauth.key

./bin/cara-server \
  --headscale-url http://localhost:8081 \
  --preauth-key-file ./server-preauth.key
```

On startup the server joins the mesh, logs its overlay IP
(`Joined Headscale overlay … overlay_ip=100.64.0.x`), and injects a tsnet-backed
transport into the agent dialer so every server→agent call is routed over the
overlay. A join failure fails fast; the server never silently falls back to a
transport that cannot reach the overlay once overlay is requested.

| Setting | Flag | Env / YAML | Notes |
|---|---|---|---|
| Control-plane URL | `--headscale-url` | `HEADSCALE_URL` / `headscale_url` | Enables overlay when set |
| Pre-auth key file | `--preauth-key-file` | `HEADSCALE_PREAUTH_KEY_FILE` / `preauth_key_file` | Path to a file holding the key; the key is never logged |
| Overlay hostname | `--overlay-hostname` | `OVERLAY_HOSTNAME` / `overlay_hostname` | Optional; defaults to `cara-server` |
| Overlay state dir | `--overlay-state-dir` | `OVERLAY_STATE_DIR` / `overlay_state_dir` | Optional; defaults to a `cara-server`-specific dir. Set this when running server and agent on the same host so their tsnet state does not collide |

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
