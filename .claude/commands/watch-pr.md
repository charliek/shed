# Watch PR Command

Watch a pull request's CI checks and bot reviews, fixing failures along the way.

Use `$ARGUMENTS` as an optional PR number. If not provided, use the PR associated with the current branch.

## Steps

1. **Identify the PR**: Determine which PR to watch
   - If `$ARGUMENTS` is provided, use it as the PR number: `gh pr view $ARGUMENTS --json number,headRefName,baseRefName,url`
   - Otherwise, detect from current branch: `gh pr view --json number,headRefName,baseRefName,url`
   - If no PR is found, stop and inform the user

2. **Wait for CI checks**: Watch until all checks complete
   - Run `gh pr checks <number> --watch --fail-fast`
   - If `--watch` is not supported, fall back to polling `gh pr checks <number>` every 30 seconds
   - Ignore bot review checks (e.g. CodeRabbit) when evaluating pass/fail — these are informational

3. **Evaluate CI results**: Categorize the outcomes
   - If all required checks passed, move to step 5 (bot review)
   - If any required checks failed, proceed to step 4

4. **Handle CI failures**: Diagnose and fix
   - Identify the failed check from `gh pr checks <number>` — the output includes a link column with the run URL containing the run ID
   - Fetch failure logs using that run ID: `gh run view <run-id> --log-failed`
   - Examine the repo's CI config to understand what the check does
   - Determine the fix from the logs and CI config — do not hardcode any CI job names or language-specific commands
   - Run equivalent local checks to verify the fix before pushing
   - Commit the fix, push, and go back to step 2 to re-watch
   - **Circuit breaker**: If the same check fails a second time after a fix attempt, stop and ask the user for guidance instead of continuing to retry

5. **Wait for bot review**: Check for bot review tools
   - Look at the PR checks for any bot review tool (e.g. CodeRabbit)
   - If no bot review tool is detected, skip to step 7
   - Poll for review comments up to 15 minutes using `gh pr view <number> --json reviews,comments` every 60 seconds
   - If no review arrives within 15 minutes, move to step 7

6. **Handle review comments**: Evaluate and address feedback
   - Read the review comments from the PR
   - Evaluate each comment: fix comments that point out real improvements (bugs, correctness, meaningful quality issues)
   - Skip nitpicks, style-only suggestions, and comments that don't improve the code
   - If changes were made, commit, push, and go back to step 2 to re-watch
   - If no changes were needed, move to step 7

7. **Report results**: Summarize the outcome
   - State whether all CI checks passed
   - Summarize any fixes that were made
   - Summarize any review comments that were addressed or skipped
   - If everything is green, suggest the user can now run `/merge-pr` to merge
