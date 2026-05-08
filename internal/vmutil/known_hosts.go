package vmutil

import (
	"strings"

	"github.com/charliek/shed/internal/config"
)

// defaultKnownHosts is GitHub's published SSH host keys, copy-pasted from
// https://docs.github.com/en/authentication/keeping-your-account-and-data-secure/githubs-ssh-key-fingerprints
//
// These are baked in so a freshly-created shed can clone github.com over SSH
// without any operator configuration. PR #58 removed the bind-mount that
// previously supplied the user's ~/.ssh/known_hosts; this constant replaces
// the GitHub portion of that lost trust state.
//
// If GitHub rotates host keys (last rotation: March 2023), update this
// constant and ship a release. Operators can also add or override entries via
// ServerConfig.Git.ExtraKnownHosts without waiting for a release.
const defaultKnownHosts = `github.com ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIOMqqnkVzrm0SdG6UOoqKLsabgH5C9okWi0dh2l9GKJl
github.com ecdsa-sha2-nistp256 AAAAE2VjZHNhLXNoYTItbmlzdHAyNTYAAAAIbmlzdHAyNTYAAABBBEmKSENjQEezOmxkZMy7opKgwFB9nkt5YRrYMjNuG5N87uRgg6CLrbo5wAdT/y6v0mKV0U2w0WZ2YB/++Tpockg=
github.com ssh-rsa AAAAB3NzaC1yc2EAAAADAQABAAABgQCj7ndNxQowgcQnjshcLrqPEiiphnt+VTTvDP6mHBL9j1aNUkY4Ue1gvwnGLVlOhGeYrnZaMgRK6+PKCUXaDbC7qtbW8gIkhL7aGCsOr/C56SJMy/BCZfxd1nWzAOxSDPgVsmerOBYfNqltV9/hWCqBywINIR+5dIg6JTJ72pcEpEjcYgXkE2YEFXV1JHnsKgbLWNlhScqb2UmyRkQyytRLtL+38TGxkxCflmO+5Z8CSSNY7GidjMIZ7Q4zMjA2n1nGrlTDkzwDCsw+wqFPGQA179cnfGWOWRVruj16z6XyvxvjJwbz0wQZ75XK5tKSb7FNyeIEs4TT4jk+S4dhPeAUC5y+bDYirYgM4GC7uEnztnZyaVWQ7B381AK4Qdrwt51ZqExKbQpTUNn+EjqoTwvqNj4kqx5QUCI0ThS/YkOxJCXmPUWZbhjpCg56i+2aB6CmK2JGhn57K5mj0MNdBXA4/WnwH6XoPWJzK5Nyu2zB3nAZp+S5hpQs+p1vN1/wsjk=
`

// BuildKnownHosts returns the full known_hosts content to seed into a shed
// before `git clone` runs. The output combines built-in defaults with any
// operator-supplied extras and always ends with a trailing newline.
//
// Duplicate lines are removed only on exact match — OpenSSH happily accepts
// multiple entries for the same host (any-match-wins semantics), so we don't
// try to be smart about host or key-type collisions.
func BuildKnownHosts(cfg *config.ServerConfig) string {
	var b strings.Builder
	seen := make(map[string]bool)

	add := func(line string) {
		line = strings.TrimRight(line, "\r\n")
		if line == "" || seen[line] {
			return
		}
		seen[line] = true
		b.WriteString(line)
		b.WriteByte('\n')
	}

	for _, line := range strings.Split(defaultKnownHosts, "\n") {
		add(line)
	}
	if cfg != nil && cfg.Git != nil {
		for _, line := range cfg.Git.ExtraKnownHosts {
			add(line)
		}
	}
	return b.String()
}
