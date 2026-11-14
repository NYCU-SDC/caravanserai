# Driving caractl from an LLM agent

This guide is for an agent (or a script) that operates cara through `caractl`. It lists what the agent can rely on, then the common workflows. Every command below was run as written against a local cara-server and agent.

## What to rely on

**Auth.** Put a token in `CARA_TOKEN`. There is no flag for it, so it stays out of process listings. Point `CARA_SERVER` at the control plane.

```bash
export CARA_SERVER=http://localhost:8080
export CARA_TOKEN="$(cat break-glass-token)"
```

Use a token of its own for the agent rather than the break-glass token, so it can be revoked alone. Every user is an admin until roles land (ADR-0031). Revoking takes effect on the next request:

```bash
(umask 077 && caractl token create user ops-agent > ops-agent.token)
caractl token list -o json
```

**Output.** Parse `-o json`; the table is for people and may change. `-o json` works on `get`, `apply`, `explain` and `token list`; `describe` is for people only. Data goes to stdout. Errors go to stderr, prefixed with `Error: `. Progress lines from `wait` and `apply --wait` also go to stderr.

**Exit codes.**

| Code | Meaning |
|---|---|
| 0 | Success |
| 1 | Any other failure: a refused request, a validation error, a Project that reached Failed while being waited for, a mistyped command |
| 2 | `wait` timed out. The message gives the last state seen and the last error |
| 3 | `wait` was about a resource that does not exist |

**Mistakes fail.** A mistyped subcommand at any depth exits 1 and names the valid choices, with a suggestion when one is close:

```console
$ caractl get projetcs
Error: unknown subcommand "projetcs" for "caractl get"; valid: nodes, projects, secrets (did you mean "projects"?)
```

**Field reference without a server.** `caractl explain` reads the schemas built into the binary:

```bash
caractl explain project.spec.services
caractl explain project.status.phase -o json
```

A wrong path exits 1 and lists the valid fields at the deepest part that exists.

## Deploy and wait

```bash
cat > web.yaml <<'EOF'
apiVersion: caravanserai/v1
kind: Project
metadata:
  name: web
spec:
  services:
    - name: web
      image: nginx:alpine
EOF
caractl apply -f web.yaml --wait --timeout 3m
```

`--wait` returns 0 once the Project is Running and 1 if it reaches Failed. While it waits, stderr shows each phase and any blocking reason, such as `SchedulingBlocked: node-b lacks cpu`.

Applying a changed manifest to a Project that already runs updates it in place. `--wait` then waits until the agent runs the new spec, not only until the phase is Running. If the update cannot run, stderr says why, for example `spec revision 2 not running yet (running 1): ImagePullFailed: ...`. The old containers keep running, and the wait ends with exit 2 at the timeout. `caractl rollout restart web` recreates every service without changing the spec.

To wait without applying, or for something other than Running:

```bash
caractl wait project web --for=phase=Running --timeout 2m
caractl wait project web --for=condition=Phase
```

`--for` takes `phase=<Phase>`, `condition=<Type>` (status True), `delete`, or, for a Node, `heartbeat`. An unreachable server or a 5xx answer is retried until the timeout.

## Inspect a Project

```bash
caractl get project web -o json | jq '{phase: .status.phase, node: .status.nodeRef}'
caractl get project web -o json | jq '.status.conditions[] | select(.type == "Phase") | {reason, message}'
```

When a Project is Failed, the `Phase` condition carries the reason. `wait` and `apply --wait` print it as well:

```console
$ caractl apply -f broken.yaml --wait
project/broken created
project/broken Pending
project/broken Scheduled
Error: project "broken" is Failed: ReconcileError: ensure container "web": pull image "registry.invalid/no-such-image:1": ... no such host
```

`caractl describe project web` prints the same information for a person.

## Logs

```bash
caractl logs web/web --tail 20
```

The argument is `<project>/<service>`. Until CARA-139, caractl reads logs straight from the agent, so the machine running caractl must be able to reach the agent's port.

## Delete

```bash
caractl delete project web
caractl wait project web --for=delete --timeout 2m
```

`delete` returns as soon as the server accepts it. The agent then removes the containers, and `wait --for=delete` returns once the record is gone. If the Project is already gone, the wait returns 0 at once.

## Secrets

```bash
cat > db-secret.yaml <<'EOF'
apiVersion: caravanserai/v1
kind: Secret
metadata:
  name: db
spec:
  data:
    - key: password
      value: change-me
EOF
caractl apply -f db-secret.yaml
caractl get secret db -o json | jq '.spec.data[].key'
caractl delete secret db
```

Values are never returned: `get` shows `<redacted>`. A Project uses a Secret through `env[].valueFrom.secretKeyRef` (`caractl explain project.spec.services.env.valueFrom.secretKeyRef`).

## Enrol a Node

```bash
(umask 077 && caractl node token create node-a > node-a.join-token)
```

Copy the file to the machine, set `JOIN_TOKEN_FILE` to its path and `NODE_NAME=node-a`, and start the agent (`systemctl enable --now cara-agent` with the packages, or see the README's Quick Start). The join token works once, for an hour.

The Node exists only once the agent has registered, so `wait` exits 3 until then. Wait for the record, then for the Node to be Ready:

```bash
for i in $(seq 30); do caractl get node node-a >/dev/null 2>&1 && break; sleep 2; done
caractl wait node node-a --for=phase=Ready --timeout 2m
caractl wait node node-a --for=heartbeat --timeout 1m
```

`--for=heartbeat` waits for a heartbeat newer than the one the Node had when the wait started, compared on the server's clock.

## JSON shapes

These are trimmed. Run `caractl explain <kind>` for every field.

`caractl get node node-a -o json`:

```json
{
  "apiVersion": "caravanserai/v1",
  "kind": "Node",
  "metadata": { "name": "node-a", "uid": "d8629c69-...", "namespace": "default" },
  "spec": { "hostname": "node-a" },
  "status": {
    "state": "Ready",
    "network": { "overlayIP": "127.0.0.1", "overlayStatus": "Online", "agentPort": 19090 },
    "capacity": { "cpu": "20000m", "memory": "127600812Ki" },
    "allocatable": { "cpu": "19500m", "memory": "127076524Ki" },
    "lastHeartbeat": "2026-11-06T08:21:10.182792451Z"
  }
}
```

`caractl get nodes -o json` and `caractl get projects -o json` wrap the same objects in `{"kind": "NodeList" | "ProjectList", "items": [...]}`. An empty list has `"items": []`.

`caractl get project web -o json`:

```json
{
  "apiVersion": "caravanserai/v1",
  "kind": "Project",
  "metadata": { "name": "web", "uid": "e759ed94-...", "namespace": "default" },
  "spec": { "services": [ { "name": "web", "image": "nginx:alpine" } ] },
  "status": {
    "phase": "Running",
    "nodeRef": "node-a",
    "assignmentGeneration": 1,
    "conditions": [
      { "type": "Phase", "status": "True", "reason": "ContainersRunning", "message": "All containers running",
        "lastTransitionTime": "2026-11-06T08:21:28.265925093Z" }
    ]
  }
}
```

`caractl get secret db -o json` has the same `metadata`, and `spec.data` lists each key with `"value": "<redacted>"`.

`caractl apply -f <file> -o json` prints the object the server stored, in the same shape as `get`.
