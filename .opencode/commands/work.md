---
description: Pick up a Jira issue, discuss architecture, then implement
---

Fetch the Jira issue `$1` and prepare to work on it. Follow these phases strictly — do NOT skip the discussion phase.

Use the `question` tool for ALL interactions with the user. Ask ONE question at a time — wait for the answer before asking the next. Earlier answers may change what you need to ask next.

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

First, present a brief summary of the task (2-3 sentences) so the user knows you understood it.

Then use the `question` tool to walk through decisions **one at a time**. Typical questions to ask (adapt based on the task):

1. **Approach confirmation** — Present 2-3 possible approaches with trade-offs. Let the user pick.
2. **Scope clarification** — If the issue has ambiguity, ask about each ambiguous point separately.
3. **Design decisions** — For each non-obvious design choice (data model, API shape, naming, error handling), ask one question with concrete options.
4. **Edge cases** — Ask about any edge cases or constraints you identified.

Rules for questions:
- ONE question per `question` tool call. Never batch multiple decisions.
- Provide concrete options with brief descriptions — do not ask open-ended questions unless necessary.
- After each answer, re-evaluate whether your next planned question is still relevant.
- When all decisions are resolved, use the `question` tool to present the final implementation plan and ask for a go/no-go confirmation.

**Do NOT proceed to Phase 3 until the user explicitly confirms.**

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
