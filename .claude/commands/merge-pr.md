# Merge PR Command

Merge a pull request after verifying CI checks have passed.

Use `$ARGUMENTS` as an optional PR number. If not provided, use the PR associated with the current branch.

## Steps

1. **Identify the PR**: Determine which PR to merge
   - If `$ARGUMENTS` is provided, use it as the PR number: `gh pr view $ARGUMENTS --json number,state,title,isDraft,headRefName,baseRefName,commits,url`
   - Otherwise, detect from current branch: `gh pr view --json number,state,title,isDraft,headRefName,baseRefName,commits,url`
   - If no PR is found, stop and inform the user
   - If the PR is a draft, stop and inform the user it must be marked ready first
   - If the PR is already merged or closed, stop and inform the user

2. **Verify CI checks have passed**: Ensure all checks are green
   - Run `gh pr checks <number>`
   - All checks must pass; ignore bot review checks (e.g. CodeRabbit) which are informational and not required
   - If any required checks have failed, stop and show which checks failed
   - If checks are still running, suggest the user run `/watch-pr` first and then come back to merge

3. **Determine merge strategy**: Choose between merge commit and squash
   - Look at the commits count from the PR data fetched in step 1
   - If there is only **one commit**, use a merge commit (`--merge`) since squash would be equivalent
   - If there are **multiple commits**, ask the user: "This PR has N commits. Would you like to squash them into a single commit, or preserve the individual commits with a merge commit?"

4. **Merge the PR**: Execute the merge
   - Run `gh pr merge <number> --merge --delete-branch` or `gh pr merge <number> --squash --delete-branch` based on the chosen strategy

5. **Clean up local branch**: Sync local state
   - Switch to the base branch: `git checkout <baseRefName>`
   - Pull latest: `git pull`
   - Delete the local head branch if it still exists: `git branch -d <headRefName>`

6. **Confirm success**: Report the result
   - Show the PR URL and title
   - State which merge strategy was used (merge commit or squash)

## Error Handling

- If no PR is found for the current branch, inform the user and suggest providing a PR number
- If CI checks are still running, suggest `/watch-pr` first
- If CI checks have failed, show which checks failed and suggest fixing them
- If the PR is a draft, tell the user to mark it ready for review first
- If merge conflicts exist, inform the user they need to resolve conflicts before merging
- If permission errors occur, inform the user they may not have merge access
