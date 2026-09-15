---
name: audit-ownership-fencing
description: Audit what already assumed the old ownership rules before changing how Caravanserai decides who owns a resource or who may write it — a new or changed fence (Project UID, assignment generation, nodeRef, volume provenance, backup authority, condition writability), or making a resource movable between Nodes
---

## When to use this skill

Before merging any change that alters **who owns something, or who is allowed to write it**. Concretely:

- adding or changing an ownership fence — Project UID, assignment generation, `nodeRef`, volume provenance, backup authority, which component may write a condition
- making a resource movable between Nodes, or changing when a move is allowed
- adding a new writer to state that another component already reads
- letting a resource be deleted and recreated under the same name

Adding a fence is the easy half. The hard half is that code written before the fence existed decided ownership some other way, and that code does not announce itself. This skill is how you go looking for it.

Audit the **whole repository**, not your diff. The defect is in code you did not touch.

## Why this exists

Caravanserai gained Project UID and then assignment generation so that a Project deleted and recreated under the same name, or reassigned A → B → A, could be told apart from the one before it. Both landed on the container layer: containers carry `cara.uid` and `cara.generation` labels, and every adoption or destructive operation checks them.

The data layer got neither.

`internal/agent/restore` decides whether to restore a Project's Managed volumes from a marker keyed by `(namespace, project)` alone. Its own doc comment states the assumption plainly — *"Once this node is serving a Project, the local volumes are authoritative"* — and that was true when it was written, because a Project stayed on one Node. Once Projects could move, the marker went on proving only that this Node **had once** served the Project, which is a historical fact and says nothing about whether its copy is still current.

The result was silent: a Project that moved away and came back skipped its restore, started against whatever the old Node still had, reported Running, and had that stale content backed up over the good generation in the object store. No error, no warning — the skip is logged at Debug.

Nobody had to misunderstand ownership fencing for this to happen. The same work applied it correctly one package over. What was missing was the step of asking which existing logic had assumed its absence.

## What this does not cover

Ownership and authority only. It is not a general correctness audit, and widening it until it is will make it something nobody runs.

In particular it will not find bugs about **when** something happened rather than **whose** it is — a timer that measures one incident and is never reset, two controllers disagreeing about whether they are looking at the same event. Those are lifecycle and incident-identity problems and want their own audit. If a finding here turns out to be one of those, record it and take it out of scope.

## Discovery searches

These are **required starting points, not a proof of completeness.** Running all of them and finding nothing does not mean the change is safe — it means the cheap searches came up empty and the reading has to continue by hand, through the call chains that touch the state you are fencing.

Commands are written in `grep` so they run anywhere; use `rg` if you have it. Each is an example of the shape, not the exact query for your change — substitute the name of your own fence.

**Never treat a hit as understood from the matched line.** Open the whole function, the whole struct, and its callers. `-A4` truncates a struct with five fields, and the field that matters is usually not the one on the matched line.

### Tier 1 — production Go

```bash
# 1. State addressed without the fence.
grep -rn --include='*.go' 'func .*namespace, project\(Name\)\? string' \
  internal/ cmd/ api/ | grep -v _test.go

# 2. Keys and maps that cannot tell two lifetimes apart.
grep -rn --include='*.go' -A6 'type [A-Za-z]*Key struct' internal/ cmd/ api/ | grep -v _test.go
grep -rn --include='*.go' 'map\[[A-Za-z]*Key\]' internal/ cmd/ api/ | grep -v _test.go

# 3. Assumptions the author wrote down.
grep -rniE --include='*.go' \
  'once this (node|agent)|while this node|this node (has|is) |authoritative|belongs to' \
  internal/ cmd/ api/ | grep -v _test.go

# 4. Writers to shared external storage.
grep -rn --include='*.go' 'store\.Put\|\.Put(ctx\|putJSON' internal/ cmd/ | grep -v _test.go

# 5. Node-local state that outlives a placement.
grep -rn --include='*.go' 'os.WriteFile\|os.Rename\|os.MkdirAll' internal/agent/ | grep -v _test.go

# 6. Handed a whole resource, never reads its identity.
grep -rl --include='*.go' 'func .*\*v1\.Project' internal/ cmd/ \
  | grep -v _test.go \
  | xargs grep -L 'AssignmentGeneration\|ObjectMeta\.UID\|\.UID\b'
```

Search 6 exists because the others key on parameter *names*, and the common Go idiom is to pass the whole struct. `Run(ctx context.Context, project *v1.Project)` matches none of searches 1–5 and is exactly where authority has to be checked. Sixty functions take a `*v1.Project` today.

Searches 1 and 6 are useful in a second way: the container path already threads `uid` and `generation`, so it does not match either. What is left is the unfenced half of the codebase, not just a list of files.

Search 3 has the highest yield of the six, and it searches prose rather than code. A careful author usually writes the precondition down; phrases in the present or perfect tense — *once this node is serving*, *this node has established* — are claims about a moment. Ask what happens when that moment passes. A comment stating a precondition the code does not check is a finding even when nothing is broken today.

### Tier 2 — everything else that encodes identity

Go is not the only place ownership is decided.

```bash
# Identity fields in the API types and the schemas generated from them.
grep -rn 'UID\|Generation\|NodeRef' api/v1/*.go schemas/*.json | grep -v _test.go

# Migrations, and queries that address a row by name alone.
grep -rn 'uid\|generation\|node_ref' internal/store/postgres/migrations/
grep -rniE 'where .*name|delete from' internal/store/postgres/ --include='*.go' --include='*.sql' \
  | grep -v _test.go

# Destructive operations outside the Go code.
grep -rnE 'rm -rf|docker rm|VolumeRemove|DROP |TRUNCATE' scripts/ deploy/
```

Read these for absence rather than presence: an identity field the API type has but the schema does not, a query that deletes by name where the Go caller believed it was fenced, a script that removes state the fence was supposed to protect. Also check `cmd/` by hand for wiring that constructs a client or runtime without the fence — it is short, and there is no useful pattern for "the argument nobody passed".

## Lifecycle scenarios

The searches find where identity is missing. They cannot tell you whether the result is correct over time, and this class of bug only appears over time — every component behaves correctly in isolation and the invariant breaks in combination. The marker defect passed every unit test in its package.

So pick the scenarios your change makes reachable and **actually exercise them**, in a test or on real infrastructure:

- delete, then recreate under the same name
- A → B
- **A → B → A**, with data written on B in between
- a request from an old Agent arriving late
- containers, markers or local data left over from a previous assignment
- a writer that has lost ownership still trying to move a mutable pointer
- cleanup for an old assignment running while a new one is being established
- a process restart — does the fence still hold from persisted state alone?

A → B → A is the one that catches the most, because it is the only one where a Node legitimately held authority, lost it, and sees the same resource again.

## Recording findings

Put the table on the ticket that introduces the fence, so a reviewer can confirm each hit was handled rather than skimmed:

| Surface | Persistent key | Writer | Authority required | Checked today | Disposition |
|---|---|---|---|---|---|
| e.g. restore marker | `{ns}/{project}` on Node disk | Agent | UID + generation + last writer | presence only | fail closed, CARA-87 |

Note separately, in prose, anything the table cannot hold: which mutable shared pointers exist, which operations are destructive, and what must stay blocked until the work lands.

## Disposition

| Finding | Action |
|---|---|
| Unsafe path this change creates or enables | Fix in this change |
| Pre-existing, but this change exposes it | Fix, or gate the feature off until it is fixed |
| Needs lineage, too large to do safely here | Its own ticket — and **fail closed in this change** |
| Unrelated, and does not gate this change | Record as follow-up; do not grow the PR |

Row three is the one that matters. When you cannot prove which copy of some state is current, the answer is to block and say so — never to guess from a fixed precedence like "local wins" or "remote wins". Both lose data in one direction, and picking either silently is worse than refusing to start.

## Failing closed

Fail closed means three things, in whatever form the layer allows:

1. **Perform no mutation** — nothing started, nothing overwritten, nothing published to a shared store.
2. **Return a typed error** the caller can match on. The layer that holds status authority converts it into a resource condition; a package without that authority must not write status itself. `internal/agent/docker` returning `ErrRecoveryVolumeUnavailable` for the agent loop to turn into `RecoveryBlocked` is the shape.
3. **Leave an observable signal at Warn or above.** Debug is where the marker defect hid for a week.

A visible failure costs an operator an hour. A wrong guess that is invisible costs whatever the data was worth.

## A finding is not always a missing field

The marker was a file whose *presence* meant "trusted" — a boolean standing in for a question that needs a history: which lifetime produced this data, under which assignment, and whether anything has written since.

When an audit turns up state like that, threading the fence through as a field is not the fix. Ask whether what is being recorded is an identity, where an identifier suffices, or a lineage, where it does not. For the marker, deleting it would not have helped either: the same stale data would have been adopted through a different branch of the same decision.

## Done when

- Tier 1 run over all production Go; Tier 2 run over api, SQL, scripts, deployment config and E2E
- Every hit opened and read in full — function, struct, callers — not judged from the matched line
- Every hit placed in one of the four disposition rows, in the table on the ticket
- The applicable lifecycle scenarios exercised, not just reasoned about
- Anything left unfenced fails closed, with a signal above Debug
- Reading that went beyond the searches recorded on the ticket, so the next audit starts where this one stopped
