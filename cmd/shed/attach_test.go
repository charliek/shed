package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/charliek/shed/internal/config"
)

// TestRunAttachValidatesSessionNameBeforeNetwork pins the ORDER, not just the
// validation: a bad session name must fail in validateAttachFlags before
// runAttach reaches findShedServer, so a typo never auto-starts a stopped shed.
//
// Testing the order is the whole point — asserting config.ValidateSessionName
// rejects a bad name would pass even if runAttach stopped calling it. The two
// failure modes are told apart by the error text: the validation error names
// the session, a server-resolution error names the server.
func TestRunAttachValidatesSessionNameBeforeNetwork(t *testing.T) {
	origSession, origNew, origConfig := attachSessionFlag, attachNewFlag, clientConfig
	t.Cleanup(func() {
		attachSessionFlag, attachNewFlag, clientConfig = origSession, origNew, origConfig
	})
	attachNewFlag = false
	// No servers configured: if validation were skipped, runAttach would get as
	// far as findShedServer and fail with a server error instead.
	clientConfig = &config.ClientConfig{Servers: map[string]config.ServerEntry{}}

	attachSessionFlag = "not a valid name!!"
	err := runAttach(attachCmd, []string{"myshed"})
	if err == nil {
		t.Fatal("invalid session name accepted")
	}
	if !strings.Contains(err.Error(), "invalid session name") {
		t.Fatalf("expected the session-name validation error (proving it ran before\n"+
			"server resolution), got: %v", err)
	}
}

// TestAttachNewRefusesExistingSession covers --new's existing-session check.
// The golden's --new scenario is backed by a server reporting NO sessions, so
// it pins the create argv but says nothing about the refusal — without this,
// a change that stopped checking would leave every golden scenario green while
// `--new` silently attached to (or clobbered) a live session.
func TestAttachNewRefusesExistingSession(t *testing.T) {
	origSession, origNew, origExec, origConfig := attachSessionFlag, attachNewFlag, execSSH, clientConfig
	t.Cleanup(func() {
		attachSessionFlag, attachNewFlag, execSSH, clientConfig = origSession, origNew, origExec, origConfig
	})

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(config.SessionsResponse{
			Sessions: []config.Session{{Name: "default"}},
		})
	}))
	t.Cleanup(srv.Close)

	entry := &config.ServerEntry{Host: "mini3", APIURL: srv.URL, SSHPort: 2222}
	clientConfig = &config.ClientConfig{Servers: map[string]config.ServerEntry{"myserver": *entry}}

	execSSHCalled := false
	execSSH = func(string, []string, []string) error {
		execSSHCalled = true
		return nil
	}

	attachSessionFlag = "default"
	attachNewFlag = true
	err := attachPlain("myshed", "myserver", entry, &config.Shed{LandingDir: "/home/shed/p"})

	if err == nil {
		t.Fatal("--new against an existing session must refuse")
	}
	if !strings.Contains(err.Error(), "already exists") {
		t.Errorf("error should name the collision, got: %v", err)
	}
	// The refusal must happen BEFORE the exec — an error returned after
	// replacing the process would be unreachable in production.
	if execSSHCalled {
		t.Error("--new refused the session but had already exec'd ssh")
	}
}

// See attach_golden_test.go for the tmux-floor argv golden (default session,
// the empty-LandingDir fallback, a named session, --new, and shell-quoting)
// that is the load-bearing coverage of the plain attach path.
