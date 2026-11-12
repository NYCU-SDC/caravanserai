# Caravanserai

A lightweight container orchestration system built for self-hosted clusters.
Caravanserai schedules Docker workloads across a fleet of nodes through a
central control plane — without requiring Kubernetes.

```
                      ┌─────────────────┐
  caractl ──────────▶│   cara-server   │◀── Scheduler / Controller Manager
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
| `caractl` | CLI for managing Nodes and Projects |

## Concepts

### Node
A physical or virtual machine running `cara-agent`. Nodes self-register with
the control plane on startup. The Scheduler only assigns work to nodes whose
state is `Ready`.

To take a node out of service:

```bash
caractl node cordon pve1     # take no new projects; the ones there keep running
caractl node drain pve1      # cordon, and move every project off it
caractl describe node pve1   # Drain: Draining | Incomplete (lists why) | Drained
caractl node uncordon pve1   # take projects again, calling off any drain
```

A drain moves each project with a planned move: stopped, backed up first if it
has Managed data, and placed on another node, so nothing written before the
move is lost. `Drained` means nothing is assigned to the node and it has
cleaned up after the projects that left; it can be powered off. A project that
cannot leave without help — a Failed one, one with a `PostgresDifferential`
volume, one with nowhere to go — is listed under `Incomplete`; a move that
failed only for want of room is asked for again every 5 minutes.

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

**Running twice:** when a Node is cut off from the server but keeps running, and `GRACE_EXPIRED_RELEASE` moves its Projects elsewhere, both copies run until the Node reconnects. For most Projects that costs only what the old copy writes, which is lost. A Project with side effects outside its own data (scheduled jobs, webhooks, mail, bots) declares `spec.overlap: Forbid`: it is never moved on the grace period alone, and stays on its Node with `TakeoverRequired` until the Node confirms it stopped, the Node returns, or an operator takes it over. A Forbid Project is not moved when its Node loses power either, since the server cannot tell that from a cut network. Omitting `overlap` means `Allow`.

## Prerequisites

- Go 1.22+
- Docker (daemon accessible at `unix:///var/run/docker.sock` or via `DOCKER_HOST`)
- PostgreSQL (for `cara-server`)

## Installing from a release

Each release on the [Releases page](https://github.com/NYCU-SDC/caravanserai/releases) carries Debian/Ubuntu packages for amd64 and arm64. Each package installs a binary in `/usr/bin`, a systemd unit and a settings file in `/etc/cara`, and leaves its service disabled until that file is filled in. `caractl` also comes as a `.tar.gz` for Linux and macOS. `SHA256SUMS` lists the checksums.

On the control-plane machine, with PostgreSQL 16 already running:

```bash
sudo apt install ./cara-server_<version>_amd64.deb ./caractl_<version>_amd64.deb
sudoedit /etc/cara/server.env          # set DATABASE_URL
sudo systemctl enable --now cara-server
export CARA_TOKEN="$(sudo cat /etc/cara/break-glass-token)"   # generated on install
caractl node token create my-node      # prints a one-time join token
```

On each Node, with Docker installed:

```bash
sudo apt install ./cara-agent_<version>_amd64.deb
sudoedit /etc/cara/agent.env           # set SERVER_URL, NODE_NAME, AGENT_ADVERTISE_IP
sudo sh -c 'umask 077; cat > /etc/cara/join-token'   # paste the join token, then Ctrl-D
sudo systemctl enable --now cara-agent
```

What the packages do to a running system:

| Action | Service | Settings and data |
|---|---|---|
| install | installed, disabled, stopped | `/etc/cara/*.env` from the template; cara-server also generates `/etc/cara/break-glass-token` (0600) if none exists |
| upgrade or reinstall | restarted if it was running | edited `.env` files and the break-glass token are kept |
| `apt remove` | stopped and disabled | `.env` files, tokens, and `/var/lib/cara` (the agent's identity and data) are kept; installing again and `systemctl enable --now` brings the Node back with its kept identity |
| `apt purge` | | the `.env` files are removed as well; tokens and `/var/lib/cara` are still kept |

A crashed service is restarted by systemd after 5 s (`Restart=on-failure`). A clean stop through `systemctl stop` is not restarted.

Releases are cut by pushing a `v*` tag (`v0.1.0`, or `v0.1.0-rc.1` for a prerelease). The [Release workflow](.github/workflows/release.yml) builds the packages with `scripts/release/package.sh`, installs them on its runner, and publishes them. `make package` builds the same packages locally into `dist/`.

## Quick Start

The steps below run cara from source, for development. To install it on servers, see [Installing from a release](#installing-from-a-release).

### 1. Build all binaries

```bash
make build
# Outputs: bin/cara-server  bin/cara-agent  bin/caractl
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

Every API request needs a bearer token. The break-glass admin token, read from a file, is how the first tokens are issued:

```bash
(umask 077 && openssl rand -base64 32 > break-glass-token)
BREAK_GLASS_TOKEN_FILE=break-glass-token \
DATABASE_URL="postgresql://postgres:password@localhost:5432/caravanserai?sslmode=disable" \
  ./bin/cara-server
# Listening on 0.0.0.0:8080
```

### 4. Start the agent (on the same or a different machine)

Issue a one-time join token for the Node, then start the agent with it:

```bash
export CARA_TOKEN="$(cat break-glass-token)"
(umask 077 && ./bin/caractl node token create my-node > my-node.join-token)

SERVER_URL=http://localhost:8080 \
NODE_NAME=my-node \
JOIN_TOKEN_FILE=my-node.join-token \
  ./bin/cara-agent
# Agent trades the join token for its Node token, registers itself, and begins
# heartbeating and polling for work
```

The agent keeps its Node token in `<data_root>/identity/` and needs no join token on later starts; the join token file can be deleted once the Node is enrolled. A join token works once, for the Node it names, for an hour unless `--ttl` says otherwise.

`caractl node revoke my-node` stops the Node's token at once; the agent logs that it was refused. To bring the Node back, issue a new join token, put it in the same file and restart the agent.

### 5. Deploy a Project

```bash
./bin/caractl apply -f examples/nginx-project.yaml
# project/nginx-demo created

./bin/caractl get projects
# NAME         PHASE     NODE      CONDITIONS          AGE
# nginx-demo   Running   my-node   ContainersRunning   5s
```

### 6. Inspect and clean up

```bash
# List all nodes
./bin/caractl get nodes

# Get a single project (JSON)
./bin/caractl --output json get projects nginx-demo

# Delete the project (agent tears down all containers)
./bin/caractl delete project nginx-demo
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

### Backup strategy per Managed volume

A Project with a Managed volume and no `spec.backup` is backed up every 24 hours (the server's `default_backup_interval`); the stored spec shows the interval it got. A Project's own `interval` wins. Leaving `spec.backup` out of a later manifest returns it to the default rather than keeping an earlier value. `backup: {disabled: true}` turns backup off; such a Project is never moved off a lost Node automatically, even if it has an older backup, because restoring that would discard everything written since.

`spec.backup` backs up every Managed volume offline by default: the Project's services are stopped and each volume is archived. The newest 7 generations of a Project are kept, along with the one the server committed; a generation from an earlier Project of the same name is left alone. `spec.backup.items` can select `PostgresDifferential` for a PostgreSQL data volume instead (pgBackRest full and differential backups, PostgreSQL keeps running). See [`examples/volume/postgres-differential.yaml`](examples/volume/postgres-differential.yaml).

| Field | Meaning |
|---|---|
| `items[].volume` | A declared Managed volume |
| `items[].strategy` | `Offline` (default for unlisted volumes) or `PostgresDifferential` |
| `items[].retainFull` | `PostgresDifferential` only: full backups kept, 1 to 12, default 4. A count, not a time guarantee |

`PostgresDifferential` requires the volume to be mounted by exactly one service on `ghcr.io/nycu-sdc/cara-postgres:16` (or the image `postgres_differential_image` names) at `/var/lib/postgresql/data`, and every Managed volume of the Project to use it. That service cannot move its data or WAL out of the volume: `PGDATA` must be unset or that path, `POSTGRES_INITDB_WALDIR` is refused, `POSTGRES_INITDB_ARGS` may only hold `-k`, `--data-checksums` or `--NAME=VALUE` for an encoding, locale, authentication or WAL segment size setting written out in full (initdb accepts abbreviations, so anything else is refused), and none of the three may come from a Secret. A tablespace created from SQL outside the volume cannot be checked at apply time and is not supported. `items` is fixed when the Project is created and must be resent exactly as created: an omitted `retainFull` and an explicit `4` are different values, even though both mean four.

Placing such a Project needs the Agent's object store to be reached over HTTPS (`S3_ENDPOINT`, with `S3_CA_FILE` for a private CA): the Agent runs a pgBackRest repo host on the Node, creates the volume's stanza, turns on WAL archiving (restarting PostgreSQL once) and reports `Running` only after `pgbackrest check` passes. The Project's other services start together with the database and may see that restart. Until then the Project stays `Scheduled` with a `RecoveryBlocked` condition and is retried on every poll:

| Reason | Meaning |
|--------|---------|
| `BackupRepositoryUnavailable` | The Node has no object store, or reaches it over plain HTTP |
| `PgBackRestVersionMismatch` | The database image and the repo host run different pgBackRest versions |
| `ArchivingNotReady` | Creating the stanza or the archive check failed |
| `PostgresPlacementPending` | Another placement step failed, for example the server was unreachable |
| `RestoreInProgress` | The data is being restored in the background |
| `RestoreFailed` | The restore failed and will be tried again |
| `NoRestoreSource` | The Project was placed before, its data is not here, and the server has accepted no backup of it |

Once `Running`, the Project's `interval` takes a pgBackRest backup of the database without stopping any service: full when the stanza has none, when the newest full is seven days old, when the database is on a new timeline, or when the Agent cannot prove WAL archiving was continuous since, and differential otherwise. Continuity is read from the database container's Docker log: a `dropped WAL file` warning (the archive queue passed its 4 GiB bound), a replaced container, or a log that was rotated, truncated or cannot be read each make the next backup full. On a Node whose Docker logging driver keeps no readable log, every backup is full. Each backup's manifest is written to `cara/v2/projects/<uid>/generations/<backupID>/manifest.json` and the backup is then reported to the server, which accepts it only from the Project's current assignment. The Project shows `BackupActive=False` (reason `NoAcceptedBackup`) until the first backup is accepted and `True` after; `caractl describe` shows the last accepted backup. A placement that would need this data restored on a Node with no object store configured is refused with `StrategyRestoreUnavailable`.

A new assignment with no local data restores in the background, without holding up the Node's other Projects. It restores the newest backup the server accepted into staging, replays every WAL segment archived after it, and swaps the result into place once PostgreSQL has shut down cleanly, so a failure leaves nothing started. WAL is archived at least every minute while the database is written to (`archive_timeout`), so that is about what a restore loses. The `DataRestored` condition names the last transaction recovered. A failed restore retries the same backup, waiting from one minute up to 30; it never falls back to an older one. `GRACE_EXPIRED_RELEASE` moves these Projects off a lost Node once they have an accepted backup.

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

The file is the one named by `--config` or `CARA_CONFIG`, and a named file that cannot be read stops startup. Otherwise it is `./config.yaml`, then `/etc/cara/config.yaml`. The same applies to cara-agent.

| Key | Env var | Default | Description |
|-----|---------|---------|-------------|
| `debug` | `DEBUG` | `false` | Enable debug logging |
| `host` | `HOST` | `0.0.0.0` | Listen address |
| `port` | `PORT` | `8080` | Listen port |
| `database_url` | `DATABASE_URL` | _(required)_ | PostgreSQL DSN |
| `break_glass_token_file` | `BREAK_GLASS_TOKEN_FILE` | _(none)_ | File holding the break-glass admin token (`user:break-glass`). Issue the first tokens with it, and keep it for when nothing else works. A named file that is unreadable or empty stops startup |
| `otel_collector_url` | `OTEL_COLLECTOR_URL` | _(optional)_ | OTLP gRPC endpoint |
| `headscale_api_url` | `HEADSCALE_API_URL` | _(optional)_ | Headscale management API URL. Enables the `/api/v1/overlay` endpoints when set together with the API key |
| `headscale_api_key` | `HEADSCALE_API_KEY` | _(optional)_ | Headscale management API key (secret, never logged) |
| `running_grace_period` | `RUNNING_GRACE_PERIOD` | `3m` | How long a Running Project may stay on a NotReady node before its release is attempted |
| `grace_expired_release` | `GRACE_EXPIRED_RELEASE` | `false` | Move Projects off a node that stayed NotReady past the grace period, with no proof it stopped; a partitioned node keeps its copy running until it reconnects. A Project with a Managed volume moves only once it has a committed backup, and restores from it; one with `spec.backup.disabled` or `spec.overlap: Forbid` never moves on the grace period |
| `default_backup_interval` | `DEFAULT_BACKUP_INTERVAL` | `24h` | Backup interval a Project with a Managed volume and no `spec.backup` gets when it is created or updated |
| `headscale_user` | `HEADSCALE_USER` | `cara-node` | Headscale user new pre-auth keys are created under |
| `postgres_differential_image` | `POSTGRES_DIFFERENTIAL_IMAGE` | `ghcr.io/nycu-sdc/cara-postgres:16` | Image a `PostgresDifferential` volume's service must use. Set it where the published image cannot be pulled, or to a `16-<run>.<attempt>` tag to keep every Node on one content; manifests then name the same reference |
| `max_inflight_evictions` | `MAX_INFLIGHT_EVICTIONS` | `1` | Planned moves (drain, fail-back) allowed at once across the cluster. Each is downtime plus a backup and a restore |
| `failback_window` | `FAILBACK_WINDOW` | _(disabled)_ | Daily window `HH:MM-HH:MM` (may cross midnight, e.g. `02:00-05:00`) in which Projects on the backup tier are moved back to primary, one at a time. Empty disables fail-back. **Leave it unset until CARA-122 (fail-back anti-flap) is merged** |
| `failback_timezone` | `FAILBACK_TIMEZONE` | `Asia/Taipei` | IANA time zone `failback_window` is read in. The server logs the window in both this zone and UTC at startup |

### cara-agent

| Key | Env var | Default | Description |
|-----|---------|---------|-------------|
| `debug` | `DEBUG` | `false` | Enable debug logging |
| `server_url` | `SERVER_URL` | `http://localhost:8080` | cara-server address |
| `join_token_file` | `JOIN_TOKEN_FILE` | _(none)_ | File holding a one-time join token (`caractl node token create <name>`). On first start the agent trades it for a Node token and keeps that in `<data_root>/identity/`; later starts use the kept token. A different join token in the file enrols the Node again |
| `token_file` | `AGENT_TOKEN_FILE` | _(none)_ | File holding a Node token issued by hand (`caractl token create node <name>`). Takes precedence over the join token and the kept token. With neither, the agent does not start |
| `node_name` | `NODE_NAME` | OS hostname | Name to register with |
| `heartbeat_interval` | `HEARTBEAT_INTERVAL` | `30s` | Heartbeat frequency |
| `docker_host` | `DOCKER_HOST` | `unix:///var/run/docker.sock` | Docker daemon endpoint |

### caractl flags

These flags are global, so they work on either side of the subcommand:

```bash
./bin/caractl [--server <url>] [--output <format>] <command>
./bin/caractl <command> [--server <url>] [--output <format>]
```

| Flag | Short | Env var | Default | Description |
|------|-------|---------|---------|-------------|
| `--server` | | `CARA_SERVER` | `http://localhost:8080` | cara-server URL |
| `--output` | `-o` | | `table` | Output format: `table` \| `json` \| `yaml` |
| | | `CARA_TOKEN` | _(none)_ | Bearer token sent on every request. There is no flag, to keep it out of process listings |

`--server` resolves in this order: the explicit flag, then `$CARA_SERVER`, then
the localhost default. So on the server box it needs no configuration; from a
remote machine, set `CARA_SERVER` once instead of repeating `--server` on every
call.

### The `cara` shortcut

`make install-cli` installs a `cara` wrapper for `caractl` into a PATH directory
(`~/.local/bin` by default), so the everyday command becomes `cara get nodes`
instead of `./bin/caractl --server http://... get nodes`. The wrapper bakes the
control-plane address in as the default; exporting `CARA_SERVER` or passing
`--server` still overrides it.

A wrapper in PATH is used rather than a shell alias so no `source` step is
needed — a new terminal picks it up automatically, and the current shell needs
at most `rehash` (zsh) / `hash -r` (bash).

```bash
# On the server box (defaults to localhost):
make install-cli

# On an agent / remote machine (point it at the control plane):
make install-cli CARA_SERVER=http://10.1.253.7:8080

# If ~/.local/bin is not on your PATH, target another dir that is:
make install-cli CLI_BINDIR=/opt/homebrew/bin

cara get nodes           # new terminal, or `rehash` in the current one
```

Remove it again with `make uninstall-cli`.

---

## API Reference

All endpoints are under `/api/v1/`. Every endpoint except `GET /api/healthz` needs `Authorization: Bearer <token>` and answers 401 without a valid one (ADR-0031). A token identifies a Node (`node:<name>`) or a user (`user:<name>`); every user is an admin for now.

### Tokens

Admin only. Surfaced as `caractl token …`.

| Method | Path | Description |
|--------|------|-------------|
| `POST` | `/api/v1/tokens` | Issue a token (body: `{kind: node\|user, name}`). The response is the only time the token is shown; the server keeps its SHA-256 hash |
| `GET` | `/api/v1/tokens` | List tokens, without the tokens themselves |
| `DELETE` | `/api/v1/tokens/{id}` | Revoke a token; its next request gets 401 |
| `POST` | `/api/v1/nodes/{name}/join-tokens` | Issue a one-time join token for the Node (body: `{ttl}`, a Go duration, default `1h`, at most `168h`). Shown once. `caractl node token create <name> [--ttl]` |
| `DELETE` | `/api/v1/nodes/{name}/tokens` | Revoke the Node's tokens and its unused join tokens. `caractl node revoke <name>` |

The agent calls `POST /api/v1/nodes/{name}/join` with the join token as its bearer token and gets its Node token back. It is the only route that takes a join token, and a join token works nowhere else. An unknown, used, revoked or expired join token gets 401; one issued for another Node gets 403 and is not used up.

### Nodes

| Method | Path | Description |
|--------|------|-------------|
| `POST` | `/api/v1/nodes` | Register a node. A Node token may register only its own name (403 otherwise) |
| `PUT` | `/api/v1/nodes/{name}` | Update a node's spec and labels. `spec.unschedulable` cordons it and `spec.drain` drains it (`caractl node cordon\|drain\|uncordon <name>`). The `cara.io/tier` label must be `primary` or `backup` (an unlabeled node is treated as `backup`); `POST` validates it too. Set it with `caractl node set-tier <name> primary\|backup`. |
| `GET` | `/api/v1/nodes` | List all nodes |
| `GET` | `/api/v1/nodes/{name}` | Get a single node |
| `DELETE` | `/api/v1/nodes/{name}` | Delete a node |
| `POST` | `/api/v1/nodes/{name}/heartbeat` | Send a heartbeat |
| `POST` | `/api/v1/nodes/{name}/probe` | Server-side reachability probe — server dials the agent's `/healthz` via the agent dialer (see `internal/server/agentdialer/`) and reports latency + status. When the server is joined to the overlay (see below) the dialer routes this over the agent's overlay IP via a tsnet-backed transport; otherwise it uses the default transport. Also surfaced as `caractl node probe <name>`. |
| `GET` | `/api/v1/nodes/{name}/cleanup-obligations` | Unresolved cleanup obligations this node owes for assignments it gave up — the cleanup tasks its agent runs (excludes Projects the node is still assigned) |

### Projects

| Method | Path | Description |
|--------|------|-------------|
| `POST` | `/api/v1/projects` | Create a project |
| `GET` | `/api/v1/projects` | List projects (`?phase=`, `?nodeRef=`) |
| `GET` | `/api/v1/projects/{name}` | Get a single project |
| `PUT` | `/api/v1/projects/{name}` | Replace a project's spec; a Running or Scheduled project's agent recreates only the services that changed (ADR-0026). While assigned, `size`, removing a volume, a volume's type and `backup.items` cannot change |
| `DELETE` | `/api/v1/projects/{name}` | Delete a project |
| `PATCH` | `/api/v1/projects/{name}/status` | Update project status (agent only) |
| `PUT` | `/api/v1/projects/{name}/status/spec-applied` | Report whether a spec revision runs (currently assigned agent only; requires the assignment fence) |
| `POST` | `/api/v1/projects/{name}/restart` | Recreate every service by recording `spec.restartedAt` (users only; `caractl rollout restart`) |
| `PUT` | `/api/v1/projects/{name}/recovery/workload-stopped` | Record that every container is stopped (currently assigned agent only; requires the assignment fence) |
| `PUT` | `/api/v1/projects/{name}/recovery/managed-data-quarantined` | Record that local Managed data left the active path (the obligation's agent only) |
| `PUT` | `/api/v1/projects/{name}/recovery/committed-backup` | Make a backup generation the restore source (currently assigned agent only; requires the assignment fence) |
| `PUT` | `/api/v1/projects/{name}/recovery/resources-removed` | Resolve a cleanup obligation after removing its runtime resources (the obligation's agent only; requires the assignment fence) |
| `POST` | `/api/v1/projects/{name}/recovery/takeover` | Override the release gate for a Node that will not report back (admin only; `caractl takeover`) |
| `GET` | `/api/v1/projects/{name}/recovery/takeover-audit` | List recorded takeovers of the Project (admin only; `caractl takeover-audit`) |

### Overlay

Requires cara-server to be configured with Headscale API credentials; otherwise these return a "not configured" error. Surfaced as `caractl overlay …`.

| Method | Path | Description |
|--------|------|-------------|
| `POST` | `/api/v1/overlay/preauth-keys` | Issue a Headscale pre-auth key (body: `{node, ttl}`) |
| `GET` | `/api/v1/overlay/nodes` | List Headscale overlay nodes |
| `DELETE` | `/api/v1/overlay/nodes/{name}` | Revoke a node from Headscale and the Cara node store |

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

#### Overlay administration from caractl

Instead of shelling into the Headscale container, `caractl overlay` drives node
lifecycle through cara-server, which calls the Headscale management API. Enable
it by giving cara-server a Headscale API URL and key (the key is provisioned
once):

```bash
# One-time: mint a Headscale admin API key.
docker compose exec headscale headscale apikeys create --expiration 720h

./bin/cara-server \
  --headscale-api-url http://localhost:8081 \
  --headscale-api-key <api-key>
```

Then:

```bash
# Issue a pre-auth key for a node to join (the key prints to stdout, once).
./bin/caractl overlay create-preauth-key --node agent-a --ttl 24h

# List overlay nodes Headscale knows about.
./bin/caractl overlay nodes list

# Revoke a node from both Headscale and the Cara node store.
./bin/caractl overlay nodes revoke agent-a
```

Revoke removes the node from Headscale first, then from the Cara node store, so
the two views do not drift. If the store deletion fails after Headscale removal,
the command reports the drift and exits non-zero — re-running it is safe and
retries the store cleanup. When cara-server has no Headscale API credentials the
`overlay` endpoints report that overlay administration is not configured.

##### Node identity binding

When `create-preauth-key --node <name>` issues a key, cara-server records a
durable mapping between that key and the target Cara Node in a `preauth_keys`
table. Only a SHA-256 hash of the key and a short prefix are stored; the raw key
is never persisted or logged.

On its first heartbeat the agent sends the same hash as `keyRef`. cara-server
uses it to bind the reported overlay identity to the intended Node: a key issued
for node A cannot be used to claim node B (such a heartbeat is rejected), and the
key is marked `used` once consumed. This composes with overlay-IP reporting
(CARA-53): CARA-53 records `status.network.overlayIP` from the heartbeat, while
this mapping is what proves that IP was reported by the node the key authorised.
Keys created out of band (e.g. operator bootstrap) are unknown to the table and
are allowed through without binding — only wrong-node and expired keys are
rejected.

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
