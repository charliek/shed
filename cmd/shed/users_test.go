package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/charliek/shed/internal/config"
)

func TestRenderUsersRefresh(t *testing.T) {
	var buf bytes.Buffer
	renderUsersRefresh(&buf, config.UsersRefreshResponse{Logins: []config.GitHubLoginRefresh{
		{Login: "octocat", Status: "ok", GitHubID: 583231, Keys: 2, Added: []string{"SHA256:aaa"}},
		{User: "alice", Login: "hubot", Status: "mismatch", GitHubID: 1, SeenID: 2},
		{Login: "newbie", Status: "unreachable", Keys: 1, Error: "GitHub API returned 503", Removed: []string{"SHA256:bbb"}},
	}})
	got := buf.String()
	lines := strings.Split(got, "\n")
	if !strings.HasPrefix(lines[0], "USER") || !strings.Contains(lines[0], "ACCOUNT ID") {
		t.Errorf("header = %q", lines[0])
	}
	for _, want := range []string{
		"-      octocat  583231      ok           2     1      0",
		"alice  hubot",
		"-      newbie   -           unreachable  1     0      1",
		"octocat: added SHA256:aaa",
		"hubot: pinned to account 1 but now names account 2; its keys are dropped",
		"newbie: removed SHA256:bbb",
		"newbie: GitHub API returned 503",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("output lacks %q:\n%s", want, got)
		}
	}

	buf.Reset()
	renderUsersRefresh(&buf, config.UsersRefreshResponse{})
	if got := buf.String(); got != "This server follows no GitHub logins.\n" {
		t.Errorf("empty = %q", got)
	}
}
