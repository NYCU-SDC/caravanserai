# VM tests

Scenarios that need real hosts: systemd, reboots, power loss, networks between machines, root-owned files on disk. The e2e rehearsals under `scripts/e2e/` run on one machine and cannot reach these. Every VM trial so far has found a defect that unit, integration and e2e tests missed. Why this exists and when it must run: ADR-0032 and `docs/plan_docs/vm-test-harness.md` in the docs repo.

## Running

```bash
hangar login --device        # once; a maintainer approves it in the browser
test/vm/run.sh --list
test/vm/run.sh --commit <sha> --out ../test-records/$(date -u +%F)-<short-sha> join-tokens
```

`run.sh`, in order:

1. builds the commit's release packages from a detached worktree with `scripts/release/package.sh`, the script a release uses, so the working tree never leaks in. Without `--commit` it uses HEAD, and refuses if there are uncommitted changes.
2. creates `vt-server`, `vt-node-a` and `vt-node-b`.
3. installs the packages the way an operator would: `cara-server` and `caractl` on the server (with PostgreSQL in Docker), `cara-agent` on the two Nodes, settings appended to the env files the packages install. Then it enrols the agents with join tokens. With `needs: s3` it also deploys MinIO over TLS. With `needs: pgdiff` it builds `cara-postgres:16` from the commit into a registry on the server.
4. runs the scenarios in the order given.
5. collects the server's and agents' journals.
6. writes `README.md` and `results.jsonl` into `--out`, which defaults to `.vmtest/<date>-<sha>/`.
7. deletes the VMs, even when a scenario fails or the run is interrupted.

The exit status is 0 only when no check failed.

The record's "defects found" and "not covered" sections are left for the person who ran it. The tool only records what it measured.

## Rules

| Rule | How it is enforced |
|---|---|
| Delete every VM after the run | `trap` on exit and interrupt, then a check that none is left; any that remain are named |
| Touch only test VMs | VMs carry `purpose: cara-test`; deleting a VM without that label is refused; a run refuses to start if `vt-*` VMs already exist |
| Hard power-offs are rationed | at least 25 min apart within a run (`VMTEST_POWER_OFF_GAP`); a run whose scenarios need more than one refuses to start unless `--allow-wait` |
| Test a commit, not a working tree | see step 1 |
| Write only measured values | checks are `expect` (want / got / verdict) or `observe` (a measured value); durations are single runs |

Hangar is borrowed: keep runs to what a PR needs, and do not schedule them.

## When a PR needs it

A PR that touches any of these runs the related scenarios before it merges, and puts the result in its Evidence. If it needs none, the PR says why:

- agent lifecycle: start, restart, enrolment, systemd
- recovery and relocation across Nodes
- backup and restore
- authentication

Server-only logic, API fields, CLI output and docs do not need it.

## Writing a scenario

`scenarios/<name>.sh` is sourced by `run.sh` with the libraries loaded. Its header declares:

```bash
# vmtest-proves: one line: what passing shows
# vmtest-needs: s3 pgdiff          # or empty
# vmtest-server-env: GRACE_EXPIRED_RELEASE=true
# vmtest-power-offs: 0
# vmtest-minutes: 12
# vmtest-enrol: manual             # the scenario enrols agents itself; default: run.sh does
```

Choose cases from the failure-mode table in the ticket, not from the code. For each state the change touches (a file, a row, a setting, a process), ask what happens when it is missing, empty, half written, left by an older version, changed concurrently, wrongly owned, or deleted by an operator.

Available: `vm`, `srv`, `api`, `ctl`, `wait_for`, `expect`, `observe`, `power_off`, `field`, `cond`, `heartbeat_after`, `vm_for_node`, `vm_now`, `agent_log_since`, `agent_log_count`, `enrol_agent`, `put_join_token`, `install_package`, `install_state`, `install_binary` (see `lib/`). Inside a scenario `$bin` is the build directory: the packages as `$bin/<package>.deb`, the bare binaries in `$bin/bin/`.

| Scenario | Ported from |
|---|---|
| `join-tokens` | the 2026-11-06 CARA-138 trial |
| `packages` | new with CARA-143; run it after `join-tokens` when both are given |
| `node-authz` | new with CARA-140 |
| `spec-update` | new with CARA-134 |
| `overlap-partition` | new with ADR-0036 (`spec.overlap`) |
| `default-backup` | new with the ADR-0029 default backup interval |

Porting the offline-retention and pgdiff-restore trials (2026-11-05) is follow-up work.

## Another IaaS

Only `lib/provider-hangar.sh` knows about Hangar. Write `lib/provider-<name>.sh` with the same functions and run with `VMTEST_PROVIDER=<name>`.
