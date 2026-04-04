---
description: Pick up a Jira issue, discuss architecture, then implement
---

Fetch the Jira issue `$1` and prepare to work on it. Follow these phases strictly — do NOT skip the discussion phase.

## Phase 1 — Understand the task

1. Fetch the issue using `jira_getJiraIssue` with cloudId `clustron.atlassian.net` and issueIdOrKey `$1`. Use `responseContentFormat: markdown`.
2. Read the description carefully. Identify:
   - The **type** (feat/fix/refactor/test/docs/chore)
   - The **scope** (which files/packages/components)
   - The **acceptance criteria** (every checkbox)
   - Any **skills** referenced (e.g. `add-resource-kind`, `create-doc`)
3. If the description references skills, load them now with the `skill` tool to understand the full workflow.
4. Explore the codebase to understand the current state of the areas that will be changed.

## Phase 2 — Discuss with the user (MANDATORY)

Present a structured implementation plan to the user. Include:

- **Summary**: One paragraph of what the task asks for and why.
- **Proposed approach**: File-by-file list of changes, in execution order. For each file, briefly describe what will be added or modified.
- **Open questions**: Anything ambiguous in the issue, design trade-offs, or decisions that need user input.
- **Risk areas**: Parts of the implementation that might be tricky or have non-obvious side effects.

**STOP HERE and wait for the user to confirm or adjust the plan.** Do not proceed to Phase 3 until the user explicitly says to go ahead.

## Phase 3 — Implement

Once the user confirms:

1. Transition the Jira issue to In Progress:
   ```
   jira_transitionJiraIssue:
     cloudId: clustron.atlassian.net
     issueIdOrKey: $1
     transition: {id: "21"}
   ```
2. Create a branch following the naming convention `<type>/$1-<short-kebab-description>` off of `main`.
3. Implement the changes following the agreed plan. Use TodoWrite to track progress through each step.
4. Run verification commands from the issue's Verification section (typically `go build ./...`, `make test`, `make test-integration`).
5. Commit with `<type>: <description>` format.
6. Push and create a PR using `gh pr create`. PR title format: `[$1] <type>: <description>` (e.g. `[CARA-42] feat: Add Secret resource kind`). The PR body follows `.github/PULL_REQUEST_TEMPLATE.md`.
7. Transition to Review:
   ```
   jira_transitionJiraIssue:
     cloudId: clustron.atlassian.net
     issueIdOrKey: $1
     transition: {id: "32"}
   ```
