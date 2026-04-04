---
name: create-task
description: Create a Jira issue in the CARA project for another LLM agent to pick up and execute autonomously
---

## When to use this skill

Use this when you need to delegate work to another LLM agent session. This means creating a Jira issue with enough context and acceptance criteria that the receiving agent can execute the task without asking clarifying questions.

## Jira project details

- Cloud ID: `clustron.atlassian.net`
- Project key: `CARA`
- Issue types: `Task`, `Bug`, `Story`, `Epic`, `Subtask`
- Workflow: `Inbox` → `To Do` → `In Progress` → `Review` → `Done` (all global transitions)
- Transition IDs: `33` (Inbox), `11` (To Do), `21` (In Progress), `32` (Review), `31` (Done)
- Link types: `Blocks`, `Relates`, `Duplicate`, `Cloners`

## Choosing the issue type

- **Task** — a discrete, bounded piece of work (implement a function, write tests, refactor a module)
- **Bug** — a defect in existing behavior that needs fixing
- **Story** — a user-facing feature that may span multiple tasks; use as a parent for subtasks
- **Subtask** — a smaller piece of work under a Story or Task; requires `parent` field

Use `Task` as the default. Only use `Story` when the work is large enough to decompose into subtasks.

## Description template

All issues must use this template. Write everything in English. The description is the primary input for the receiving LLM agent — it must be self-contained.

```
## Type

<one of: feat | fix | refactor | test | docs | chore>

## Context

<Why does this work need to happen? What is the current state? Link to related
issues, Notion pages, or PRD sections if relevant. Give the receiving agent
enough background to understand the problem without reading the entire codebase.>

## Scope

<What exactly should be done? Be specific about which files, packages, or
components are involved. If the task involves adding a new resource kind, mention
that the `add-resource-kind` skill should be loaded.>

## Acceptance criteria

- [ ] <criterion 1 — a concrete, verifiable condition>
- [ ] <criterion 2>
- [ ] <criterion N>

## Technical notes

<Optional. Include implementation hints, edge cases to watch for, or constraints
the receiving agent should know about. Reference specific files with
`path/to/file.go:lineNumber` format when possible.>

## Verification

<How should the receiving agent verify the work is complete? List the exact
commands to run.>

```bash
go build ./...
make test
make test-integration
```
```

## Creating an issue

Use the `jira_createJiraIssue` tool:

```
cloudId: clustron.atlassian.net
projectKey: CARA
issueTypeName: Task    (or Bug, Story)
summary: <imperative mood, max 80 chars, e.g. "Add Secret resource kind with CRUD API and CLI support">
description: <filled-in template above>
contentFormat: markdown
```

Do not set assignee, labels, sprint, priority, or story points — leave them at defaults.

## Creating subtasks under a parent

When decomposing a Story into subtasks:

```
cloudId: clustron.atlassian.net
projectKey: CARA
issueTypeName: Subtask
summary: <specific piece of work>
description: <same template, can be shorter since parent provides context>
parent: CARA-<N>    (the parent Story's key)
contentFormat: markdown
```

## Linking related issues

Use the `jira_createIssueLink` tool when issues have dependencies:

- `CARA-2` blocks `CARA-3`: `inwardIssue: CARA-2, outwardIssue: CARA-3, type: Blocks`
- Two issues are related: `inwardIssue: CARA-2, outwardIssue: CARA-3, type: Relates`

## Executing an issue — full workflow

When you pick up an issue to work on, follow this sequence:

### 1. Transition to In Progress

```
jira_transitionJiraIssue:
  cloudId: clustron.atlassian.net
  issueIdOrKey: CARA-<N>
  transition: {id: "21"}
```

### 2. Create a branch

Branch naming convention: `<type>/<CARA-key>-<short-kebab-description>`

- `type` matches the `## Type` field in the description: `feat`, `fix`, `refactor`, `test`, `docs`, `chore`
- `CARA-key` is the Jira issue key (e.g. `CARA-42`)
- `short-kebab-description` is a 2-5 word kebab-case summary

Examples:

- `feat/CARA-42-add-secret-resource-kind`
- `fix/CARA-57-heartbeat-timeout-transition`
- `refactor/CARA-63-batch-store-operations`

```bash
git checkout main && git pull origin main
git checkout -b feat/CARA-42-add-secret-resource-kind
```

### 3. Implement, commit, push

Commit messages use `<type>: <description>` format (matching the project's existing convention):

```bash
git add -A && git commit -m "feat: add Secret resource kind with CRUD API and CLI"
git push -u origin feat/CARA-42-add-secret-resource-kind
```

### 4. Create a PR

Use the `gh` CLI. The PR body follows `.github/PULL_REQUEST_TEMPLATE.md`:

```bash
gh pr create --title "[CARA-42] feat: Add Secret resource kind with CRUD API and CLI" --body "$(cat <<'EOF'
## Type of changes
- Feature

## Purpose
- Add Secret resource kind with full CRUD API, CLI support, and integration tests
- Resolve issue CARA-42

## Additional Information
- Loaded `add-resource-kind` skill and followed all 9 steps
- Secrets have no lifecycle phases — purely CRUD
EOF
)"
```

PR title uses the format `[CARA-<N>] <type>: <description>` (e.g. `[CARA-42] feat: Add Secret resource kind`).

### 5. Transition to Review

After the PR is created (not after merge — merge is handled by reviewers):

```
jira_transitionJiraIssue:
  cloudId: clustron.atlassian.net
  issueIdOrKey: CARA-<N>
  transition: {id: "32"}
```

## Transition ID reference

- `33` → `Inbox`
- `11` → `To Do`
- `21` → `In Progress`
- `32` → `Review`
- `31` → `Done`

## Summary conventions

Write summaries in imperative mood, in English, max 80 characters:

- "Add Secret resource kind with CRUD API and CLI support"
- "Fix node heartbeat timeout not transitioning state to NotReady"
- "Refactor store interface to support batch operations"
- "Add integration tests for Project scheduling lifecycle"

Do not prefix with type (no "feat:", "fix:" etc.) — the type goes in the description template.

## Example: creating a task for another agent

```
summary: Add Secret resource kind with CRUD API and CLI support

description:
## Type

feat

## Context

The Caravanserai PRD defines a Secret resource kind for managing sensitive
configuration data (database passwords, API keys). Currently only Node and
Project kinds are implemented.

See Notion page "Caravanserai Project" for the full Secret spec definition.

## Scope

Full implementation of the Secret resource kind. Load the `add-resource-kind`
skill and follow all 9 steps:

1. API types in `api/v1/secret_types.go`
2. Store interface in `internal/store/interface.go`
3. Postgres implementation in `internal/store/postgres/store.go`
4. HTTP handler in `internal/server/handler/secret/`
5. CLI support in `internal/cli/`
6. Wiring in `cmd/cara-server/main.go`
7. No controllers needed for MVP — Secrets are purely CRUD
8. Integration test in `test/integration/secret_test.go`
9. Example manifest in `examples/secret.yaml`

## Acceptance criteria

- [ ] `POST /api/v1/secrets` creates a Secret and returns 201
- [ ] `GET /api/v1/secrets` lists all Secrets
- [ ] `GET /api/v1/secrets/{name}` returns a single Secret or 404
- [ ] `DELETE /api/v1/secrets/{name}` deletes a Secret and returns 204
- [ ] `caractl apply -f examples/secret.yaml` creates a Secret
- [ ] `caractl get secrets` displays Secrets in table format
- [ ] `caractl delete secret <name>` deletes a Secret
- [ ] Integration test `TestSecretCRUD` passes

## Technical notes

- Secrets have no lifecycle phases — store empty string in the `phase` column
- Spec should contain `data map[string]string` (key-value pairs)
- No status fields needed for MVP — use an empty `SecretStatus` struct
- No controllers needed — skip step 7

## Verification

```bash
go build ./...
make test
make test-integration
```
```
