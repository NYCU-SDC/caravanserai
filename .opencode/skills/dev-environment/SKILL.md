---
name: dev-environment
description: Start the full Caravanserai development environment (PostgreSQL, cara-server, cara-agent) and verify it is healthy
---

## Components

Three processes must start in this order:

1. PostgreSQL 16 — Docker container via `docker-compose.yaml`
2. cara-server — control-plane API (depends on PostgreSQL)
3. cara-agent — Docker reconciler (depends on cara-server)

## Startup procedure

Run all server/agent processes in the background with logs redirected to files.

```bash
# 1. Start PostgreSQL (blocks until healthy)
make dev-up

# 2. Build all binaries
make build

# 3. Start cara-server in background
./bin/cara-server > /tmp/cara-server.log 2>&1 &
echo "cara-server PID: $!"

# 4. Verify server is ready
sleep 3
curl -sf http://localhost:8080/api/healthz
# Success: exit code 0, body contains "OK"
# Failure: retry after 2 more seconds, then check /tmp/cara-server.log

# 5. Start cara-agent in background
NODE_NAME=test-node ./bin/cara-agent > /tmp/cara-agent.log 2>&1 &
echo "cara-agent PID: $!"

# 6. Verify agent HTTP server is ready (port-forward endpoint + healthz)
sleep 2
curl -sf http://localhost:9090/healthz
# Success: exit code 0, body contains "OK"

# 7. Verify agent registered and node is Ready
sleep 5
./bin/caractl get nodes
# Success: output contains "test-node" with state "Ready"
# If state is "NotReady": wait 5 more seconds and retry (heartbeat interval is 30s)
```

The agent logs `Failed to load agent config from file: open config.yaml: no such file or directory` on startup. This is expected — it falls back to `.env` and environment variables. Do not treat this as an error.

## Shutdown procedure

```bash
kill $(pgrep -f "bin/cara-agent") 2>/dev/null
kill $(pgrep -f "bin/cara-server") 2>/dev/null
make dev-down      # stop PostgreSQL, preserve data
# or: make dev-reset  # stop PostgreSQL AND wipe all data
```

## Checking logs

```bash
tail -30 /tmp/cara-server.log
tail -30 /tmp/cara-agent.log
```

## Agent environment variables

- `SERVER_URL` — cara-server address (default: `http://localhost:8080`)
- `NODE_NAME` — node name for registration (default: OS hostname)
- `HEARTBEAT_INTERVAL` — heartbeat frequency (default: `30s`)
- `DOCKER_HOST` — Docker daemon socket (default: `unix:///var/run/docker.sock`)
- `AGENT_LISTEN_PORT` — Agent HTTP server port (default: `9090`); also settable via `--agent-port` flag

## Prerequisites

- Docker daemon running (`docker ps` must succeed)
- Go 1.22+
- Ports 5432, 8080, and 9090 free
- `.env` at project root contains:
  ```
  DEBUG=true
  DATABASE_URL=postgresql://postgres:password@localhost:5432/caravanserai?sslmode=disable
  ```

## Troubleshooting

- Port 5432 occupied → `docker compose down` then retry `make dev-up`
- Port 8080 occupied → `kill $(lsof -ti :8080)` then restart cara-server
- Port 9090 occupied → `kill $(lsof -ti :9090)` then restart cara-agent
- Agent cannot reach Docker → verify `docker ps` succeeds; if non-default socket, set `DOCKER_HOST`
- Agent fails to register → verify cara-server is running (`curl -sf http://localhost:8080/api/healthz`); check `/tmp/cara-server.log`
- Database migration errors after branch switch → `make dev-reset`
