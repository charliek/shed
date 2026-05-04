package config

import (
	"fmt"
	"net/url"
	"regexp"
	"strings"
)

// gitSSHRegex matches git@host:path format (e.g., git@github.com:user/repo.git)
var gitSSHRegex = regexp.MustCompile(`^git@[a-zA-Z0-9][a-zA-Z0-9.-]*:[a-zA-Z0-9_.-]+/[a-zA-Z0-9_.-]+(?:\.git)?$`)

// repoShorthandRegex matches owner/repo shorthand (e.g., charliek/prox)
var repoShorthandRegex = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]*/[a-zA-Z0-9][a-zA-Z0-9_.-]*$`)

// ExpandRepoShorthand expands owner/repo shorthand to a full git SSH URL.
// Full URLs (with scheme or git@ prefix) pass through unchanged.
// Empty strings pass through unchanged.
func ExpandRepoShorthand(repo string) string {
	if repo == "" {
		return repo
	}

	// Already a full URL — pass through
	if strings.Contains(repo, "://") || strings.HasPrefix(repo, "git@") {
		return repo
	}

	// Match owner/repo shorthand
	if repoShorthandRegex.MatchString(repo) {
		return "git@github.com:" + repo + ".git"
	}

	return repo
}

// SanitizeRepoURL returns repo with the password component removed from the
// URL's userinfo, if any. The username is preserved (it's informational, not
// a secret). SSH-form URLs (git@host:path) and shorthand pass through
// unchanged. Use this when logging URLs to avoid leaking embedded credentials
// from forms like `https://user:password@host/repo.git`.
func SanitizeRepoURL(repo string) string {
	if repo == "" || strings.HasPrefix(repo, "git@") {
		return repo
	}
	u, err := url.Parse(repo)
	if err != nil || u.User == nil {
		return repo
	}
	if _, hasPassword := u.User.Password(); !hasPassword {
		return repo
	}
	u.User = url.User(u.User.Username())
	return u.String()
}

// IsSSHRepoURL reports whether a repo URL uses SSH transport.
// Matches both `git@host:path` (SCP-like) and `ssh://...` schemes.
// HTTPS, git://, http://, and empty strings return false.
func IsSSHRepoURL(repo string) bool {
	if repo == "" {
		return false
	}
	if strings.HasPrefix(repo, "ssh://") {
		return true
	}
	if strings.HasPrefix(repo, "git@") {
		return gitSSHRegex.MatchString(repo)
	}
	return false
}

// ValidateGitRepoURL validates that a git repository URL is well-formed.
// Accepts https://, git://, ssh://, and git@host:path formats.
func ValidateGitRepoURL(repoURL string) error {
	if repoURL == "" {
		return nil // Empty is valid (no repo to clone)
	}

	// Check for git@host:path format first (SCP-like syntax)
	if strings.HasPrefix(repoURL, "git@") {
		if !gitSSHRegex.MatchString(repoURL) {
			return fmt.Errorf("invalid git SSH URL format: %s", repoURL)
		}
		return nil
	}

	// Parse as standard URL
	parsed, err := url.Parse(repoURL)
	if err != nil {
		return fmt.Errorf("invalid repository URL: %w", err)
	}

	// Validate scheme
	validSchemes := map[string]bool{
		"https": true,
		"http":  true,
		"git":   true,
		"ssh":   true,
	}
	if !validSchemes[parsed.Scheme] {
		return fmt.Errorf("unsupported URL scheme %q: must be https, http, git, or ssh", parsed.Scheme)
	}

	// Validate host is present
	if parsed.Host == "" {
		return fmt.Errorf("repository URL must have a host")
	}

	// Validate path is present (should have at least /user/repo or /repo)
	if parsed.Path == "" || parsed.Path == "/" {
		return fmt.Errorf("repository URL must have a path")
	}

	return nil
}
