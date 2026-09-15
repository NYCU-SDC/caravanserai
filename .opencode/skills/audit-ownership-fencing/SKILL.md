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

Adding a fence is the easy half. The hard half is that code written before the fence existed decided ownership some other way, and that code does not announce itself. This skill is how you find it.

Run the audit on the **whole repository**, not on your diff. The defect you are looking for is in code you did not touch.

## Why this exists

Caravanserai gained Project UID and then assignment generation so that a Project deleted and recreated under the same name, or reassigned A → B → A, could be told apart from the one before it. Both landed on the container layer: containers carry `cara.uid` and `cara.generation` labels, and every adoption or destructive operation checks them.

The data layer got neither.

`internal/agent/restore` decides whether to restore a Project's Managed volumes from a marker file keyed by `(namespace, project)` alone. Its own doc comment states the assumption plainly — *"Once this node is serving a Project, the local volumes are authoritative"* — and that assumption was true when it was written, because a Project stayed on one Node. Once Projects could move, the marker went on proving only that this Node **had once** served the Project, which is a historical fact and says nothing about whether its copy is still current.

The result was silent: a Project that moved away and came back skipped its restore, started against whatever the old Node still had, reported Running, and then had that stale content backed up over the good generation in the object store. No error, no warning — the skip is logged at Debug.

Nobody had to misunderstand ownership fencing for this to happen. The same people applied it correctly one package over. What was missing was the step of asking which existing logic had assumed its absence.

That step is this skill. The cost of skipping it is not a bug report; it is data loss that nothing reports.

## The audit

Five searches. Each one is looking for a specific shape of assumption. Run all five — they overlap only partly, and the one you skip is the one that hides the defect.

### 1. State addressed without the fence

```bash
grep -rn --include='*.go' 'func .*namespace, project\(Name\)\? string' internal/ | grep -v _test.go
```

Anything addressed by `(namespace, project)` and nothing else cannot distinguish one lifetime or one assignment from the next. Widen the pattern to whatever your fence is named.

This search partitions the codebase usefully: functions that already take `uid` and `generation` do not match it. What is left is the unfenced half.

### 2. In-memory and on-disk keys

```bash
grep -rn --include='*.go' -A4 'type [A-Za-z]*Key struct' internal/ | grep -v _test.go
grep -rn --include='*.go' 'map\[.*Key\]' internal/ | grep -v _test.go
```

A key type that carries only name and namespace makes two different lifetimes collide in one map entry. Check every field of the struct, not the name of the type.

### 3. Assumptions stated in comments

```bash
grep -rniE --include='*.go' 'once this (node|agent)|while this node|this node (has|is) |authoritative|belongs to' internal/ | grep -v _test.go
```

The most direct search of the five, because a careful author usually writes the precondition down. Phrases in the present or perfect tense — *once this node is serving*, *this node has established*, *the local volumes are authoritative* — are claims about a moment. Ask what happens when that moment passes.

Treat a comment that states a precondition the code does not check as a finding, even when nothing is broken today.

### 4. Writers to shared external storage

```bash
grep -rn --include='*.go' 'store\.Put\|\.Put(ctx' internal/ | grep -v _test.go
```

State inside the server's database can be guarded by a fenced mutation. State in an object store cannot — whatever writes it is trusted by construction. Every such writer needs to establish its own authority before writing, and a mutable pointer (a `latest` marker, a HEAD reference) needs it more than an immutable object does: an immutable object written by a stale writer is ignorable, a pointer moved by one is not.

### 5. Node-local state that outlives a placement

```bash
grep -rn --include='*.go' 'os.WriteFile\|os.Rename\|os.MkdirAll' internal/agent/ | grep -v _test.go
```

Anything a Node writes to its own disk survives the Project leaving that Node — deliberately so for data, incidentally so for everything stored beside it. Ask of each: if this Project comes back to this Node after living somewhere else, does this file still mean what it meant when it was written?

Note that orphan cleanup cannot help here. It works from Docker labels, so it reaches containers, networks and named volumes, and by construction cannot reach a host directory. Whatever you leave in one stays there.

## What to do with each hit

| Finding | Action |
|---|---|
| The fence can be threaded through with no semantic change | Do it in this change |
| The state genuinely needs lineage, not just an identifier | Open a ticket, and **fail closed until it lands** |
| Correct today, will break when a planned feature arrives | Comment stating the precondition, plus a ticket |
| Unreachable from any current path | Comment saying why, so the next audit does not re-examine it |

The second row is the one that matters. When you cannot prove which copy of some state is current, the answer is to block and say so — never to guess from a fixed precedence like "local wins" or "remote wins". Both of those lose data in one direction, and picking either silently is worse than refusing to start.

Fail closed means: a condition on the resource, a log line above Debug, and no write to any shared store from the unproven state. A failure that is visible costs an operator an hour. A wrong guess that is invisible costs whatever the data was worth.

## A finding is not a boolean

The marker above was a file whose *presence* meant "trusted". That is a boolean standing in for a question that needs a history: which lifetime produced this data, under which assignment, and has anything else written since.

When an audit turns up state like that, adding the fence as a field is usually not enough. Ask whether the thing being recorded is an identity (an identifier suffices) or a lineage (it does not). Deleting the marker, for the case above, would not have fixed anything — it would have moved the same stale data from one branch of the decision to another.

## Done when

- All five searches run against the whole repository, not the diff
- Every hit is resolved into one of the four rows above
- Anything left unfenced fails closed, and the failure is visible in resource status
- The audit's findings are recorded on the ticket that introduced the fence, so the next person can see what was checked and what was deferred
