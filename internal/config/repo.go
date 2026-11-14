package config

import (
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strings"

	"github.com/charliek/shed/internal/redact"
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

// RepoDirName returns the directory name `git clone <repo>` would create: the
// last path segment of the URL with a trailing ".git" removed. It mirrors git's
// own default-directory logic for the URL forms shed accepts (https://, git://,
// ssh://, git@host:path, and expanded owner/repo shorthand).
func RepoDirName(repo string) (string, error) {
	s := strings.TrimSpace(repo)
	if s == "" {
		return "", fmt.Errorf("empty repository URL")
	}
	s = strings.TrimRight(s, "/")
	// The dir name is whatever follows the last '/' (path separator) or ':'
	// (the SCP-like git@host:owner/repo separator when there is no path slash).
	if i := strings.LastIndexAny(s, "/:"); i >= 0 {
		s = s[i+1:]
	}
	s = strings.TrimSuffix(s, ".git")
	if s == "" || s == "." || s == ".." || strings.ContainsAny(s, "/\x00") {
		// The message reaches logs and API replies: never with the URL's
		// credentials.
		return "", fmt.Errorf("cannot derive a directory name from repository URL %q", SanitizeRepoURL(repo))
	}
	return s, nil
}

// SanitizeRepoURL returns repo with the whole userinfo of a URL replaced by
// redact.Marker, for logging. The user name goes too: a token is often passed
// as the user alone (https://TOKEN@host/repo.git). SSH-form URLs
// (git@host:path) and shorthand pass through unchanged, and nothing else in
// the URL is re-encoded.
func SanitizeRepoURL(repo string) string {
	return redact.String(repo)
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
		return fmt.Errorf("invalid repository URL: %s", parseFailure(err))
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

// parseFailure says why url.Parse refused a repository URL without repeating
// any of it. The message reaches API replies and logs, a URL that does not
// parse cannot be trusted to redact (its userinfo is often the part that is
// wrong), and net/url quotes the whole URL, or the piece it choked on, in its
// own text: that piece can be the password.
func parseFailure(err error) string {
	var uerr *url.Error
	if errors.As(err, &uerr) && uerr.Err != nil {
		if why := uerr.Err.Error(); !strings.ContainsAny(why, "\"'`") {
			return strings.TrimPrefix(why, "net/url: ")
		}
	}
	return "it does not parse as a URL"
}
