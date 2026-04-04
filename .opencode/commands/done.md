---
description: Merge a reviewed PR, clean up branch, and close the Jira issue
---

Merge the PR for Jira issue `$1` and complete all cleanup. Execute all steps without asking — this is a post-review finishing command.

## Steps

1. Find the open PR for `$1` by running `gh pr list --search "$1" --state open --json number,headRefName --limit 1`. If no PR is found, stop and tell the user.

2. Merge the PR with squash merge and delete the remote branch:
   ```
   gh pr merge <number> --squash --delete-branch
   ```

3. Switch back to `main` and pull:
   ```bash
   git checkout main && git pull
   ```

4. Delete the local branch if it still exists:
   ```bash
   git branch -d <headRefName>
   ```
   Use `-D` if `-d` fails (the remote is already gone so this is safe).

5. Transition the Jira issue to Done:
   ```
   jira_transitionJiraIssue:
     cloudId: clustron.atlassian.net
     issueIdOrKey: $1
     transition: {id: "31"}
   ```

6. Confirm to the user that everything is done: PR merged, branch cleaned up, and Jira issue closed.
