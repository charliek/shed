# Release Command

Create a new release by generating a changelog entry, committing it, and pushing a version tag.

## Steps

1. **Verify CI status**: Check that CI has passed for the current commit on main branch
   - Run `gh run list --commit $(git rev-parse HEAD) --status completed --json conclusion,name`
   - All workflows should have conclusion "success"
   - If CI hasn't completed or failed, stop and inform the user

2. **Check branch**: Ensure we're on the main branch
   - Run `git branch --show-current`
   - If not on main, stop and inform the user

3. **Determine version tag**: Ask user for semantic version
   - Prompt: "What version should this release be? (e.g., v1.0.0, v1.1.0, v2.0.0)"
   - Validate format matches `v[0-9]+\.[0-9]+\.[0-9]+(-[a-zA-Z0-9.]+)?`
   - Check tag doesn't already exist: `git tag -l "<version>"`
   - If tag exists, inform user and ask for different version

4. **Generate changelog entry**: Summarize changes since last release
   - Find the previous release tag: `git describe --tags --abbrev=0 --match "v*" 2>/dev/null`
   - Get commits since that tag: `git log <previous-tag>..HEAD --oneline`
   - If no previous tag exists, get all commits: `git log --oneline`
   - Write a concise summary of the changes (group by type: features, fixes, updates)

5. **Update CHANGELOG.md**: Prepend the new entry
   - Read the current CHANGELOG.md
   - Add a new section at the top (after the header) with the version and changes
   - Format:
     ```
     ## vX.Y.Z

     - Change 1
     - Change 2
     ```

6. **Commit the changelog**: Create a release commit
   - Stage CHANGELOG.md: `git add CHANGELOG.md`
   - Commit with message: `Release vX.Y.Z`
   - Include Co-Authored-By line

7. **Create and push tag**: Trigger the release workflow
   - Create tag: `git tag vX.Y.Z`
   - Push commit and tag: `git push && git push --tags`

8. **Confirm success**: Show the release URL
   - Display: `https://github.com/charliek/shed/releases/tag/vX.Y.Z`
   - Inform user that the release workflow has been triggered
   - They can monitor it at the Actions tab

## Error Handling

- If CI hasn't passed, inform the user and suggest running `/release` again after CI completes
- If not on main branch, inform the user to switch branches
- If there are uncommitted changes, warn the user and ask how to proceed
- If the version tag already exists, ask for a different version
- If push fails, provide troubleshooting steps
