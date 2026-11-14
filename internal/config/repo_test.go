package config

import (
	"strings"
	"testing"
)

func TestExpandRepoShorthand(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{name: "empty", input: "", want: ""},
		{name: "shorthand", input: "charliek/prox", want: "git@github.com:charliek/prox.git"},
		{name: "shorthand with dots", input: "org/my.repo", want: "git@github.com:org/my.repo.git"},
		{name: "shorthand with hyphens", input: "my-org/my-repo", want: "git@github.com:my-org/my-repo.git"},
		{name: "ssh url passthrough", input: "git@github.com:charliek/prox.git", want: "git@github.com:charliek/prox.git"},
		{name: "ssh url no .git", input: "git@github.com:charliek/prox", want: "git@github.com:charliek/prox"},
		{name: "https url passthrough", input: "https://github.com/charliek/prox.git", want: "https://github.com/charliek/prox.git"},
		{name: "http url passthrough", input: "http://github.com/charliek/prox.git", want: "http://github.com/charliek/prox.git"},
		{name: "git scheme passthrough", input: "git://github.com/charliek/prox.git", want: "git://github.com/charliek/prox.git"},
		{name: "ssh scheme passthrough", input: "ssh://git@github.com/charliek/prox.git", want: "ssh://git@github.com/charliek/prox.git"},
		{name: "three segments not shorthand", input: "a/b/c", want: "a/b/c"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ExpandRepoShorthand(tt.input)
			if got != tt.want {
				t.Errorf("ExpandRepoShorthand(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

func TestSanitizeRepoURL(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{name: "empty", input: "", want: ""},
		{name: "git@ unchanged", input: "git@github.com:charliek/prox.git", want: "git@github.com:charliek/prox.git"},
		{name: "ssh:// username redacted", input: "ssh://git@host/path", want: "ssh://REDACTED@host/path"},
		{name: "https no userinfo unchanged", input: "https://github.com/charliek/prox.git", want: "https://github.com/charliek/prox.git"},
		{name: "https token as username redacted", input: "https://ghp_token@host/repo.git", want: "https://REDACTED@host/repo.git"},
		{name: "https with password redacted", input: "https://user:pw@host/repo.git", want: "https://REDACTED@host/repo.git"},
		{name: "https with empty password redacted", input: "https://user:@host/repo.git", want: "https://REDACTED@host/repo.git"},
		{name: "not re-encoded", input: "https://u:p@host/a%2Fb repo", want: "https://REDACTED@host/a%2Fb repo"},
		{name: "malformed url passes through", input: "not a url", want: "not a url"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := SanitizeRepoURL(tt.input)
			if got != tt.want {
				t.Errorf("SanitizeRepoURL(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

func TestIsSSHRepoURL(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  bool
	}{
		{name: "empty", input: "", want: false},
		{name: "git@ form", input: "git@github.com:charliek/prox.git", want: true},
		{name: "git@ no .git", input: "git@github.com:charliek/prox", want: true},
		{name: "ssh:// scheme", input: "ssh://git@github.com/charliek/prox.git", want: true},
		{name: "ssh:// no user", input: "ssh://example.com/repo", want: true},
		{name: "https rejected", input: "https://github.com/charliek/prox.git", want: false},
		{name: "http rejected", input: "http://github.com/charliek/prox.git", want: false},
		{name: "git:// rejected", input: "git://github.com/charliek/prox.git", want: false},
		{name: "shorthand rejected", input: "charliek/prox", want: false},
		{name: "garbage rejected", input: "not a url", want: false},
		{name: "malformed git@ rejected", input: "git@:bad", want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := IsSSHRepoURL(tt.input)
			if got != tt.want {
				t.Errorf("IsSSHRepoURL(%q) = %v, want %v", tt.input, got, tt.want)
			}
		})
	}
}

func TestValidateGitRepoURL(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		wantErr bool
	}{
		{name: "empty", input: "", wantErr: false},
		{name: "valid ssh", input: "git@github.com:charliek/prox.git", wantErr: false},
		{name: "valid ssh no .git", input: "git@github.com:charliek/prox", wantErr: false},
		{name: "valid https", input: "https://github.com/charliek/prox.git", wantErr: false},
		{name: "valid http", input: "http://github.com/charliek/prox.git", wantErr: false},
		{name: "valid git scheme", input: "git://github.com/charliek/prox.git", wantErr: false},
		{name: "valid ssh scheme", input: "ssh://git@github.com/charliek/prox.git", wantErr: false},
		{name: "invalid scheme", input: "ftp://github.com/repo.git", wantErr: true},
		{name: "invalid ssh format", input: "git@:bad", wantErr: true},
		{name: "no host", input: "https:///repo.git", wantErr: true},
		{name: "no path", input: "https://github.com", wantErr: true},
		{name: "bare shorthand rejected", input: "charliek/prox", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateGitRepoURL(tt.input)
			if (err != nil) != tt.wantErr {
				t.Errorf("ValidateGitRepoURL(%q) error = %v, wantErr %v", tt.input, err, tt.wantErr)
			}
		})
	}
}

// TestRepoDirNameErrorHidesCredentials: the refusal quotes the URL, and it
// reaches logs and API replies.
func TestRepoDirNameErrorHidesCredentials(t *testing.T) {
	for _, repo := range []string{"https://user:s3cret@example.com/..", "https://s3cret@example.com/.git"} {
		t.Run(repo, func(t *testing.T) {
			_, err := RepoDirName(repo)
			if err == nil {
				t.Fatal("no error")
			}
			if strings.Contains(err.Error(), "s3cret") || !strings.Contains(err.Error(), "example.com") {
				t.Errorf("error = %q, want the URL without its credentials", err)
			}
		})
	}
}

// TestValidateGitRepoURLErrorHidesCredentials: a URL that does not parse is
// refused without any of its text, because net/url quotes the URL (or the
// piece it could not read) in its error and the refusal is an API reply.
func TestValidateGitRepoURLErrorHidesCredentials(t *testing.T) {
	tests := []struct {
		name string
		repo string
		want string
	}{
		{name: "backslash in the password", repo: `https://alice:pa\$s3cret@example.com/x.git`, want: "invalid userinfo"},
		{name: "space in the password", repo: "https://alice:my s3cret@example.com/x.git", want: "invalid userinfo"},
		{name: "bad escape in the password", repo: "https://alice:%zzs3cret@example.com/x.git", want: "does not parse"},
		{name: "password read as a port", repo: "https://alice:s3cret/x.git", want: "does not parse"},
		{name: "control character", repo: "https://alice:s3cret@example.com/x\x7f.git", want: "invalid control character"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateGitRepoURL(tt.repo)
			if err == nil {
				t.Fatal("no error")
			}
			if strings.Contains(err.Error(), "s3cret") || strings.Contains(err.Error(), "alice") {
				t.Errorf("error = %q, want none of the URL", err)
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("error = %q, want it to say %q", err, tt.want)
			}
		})
	}
}
