package sshd

import (
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/gliderlabs/ssh"
	gossh "golang.org/x/crypto/ssh"

	"github.com/charliek/shed/internal/backend"
	"github.com/charliek/shed/internal/config"
)

// probeOutcome is how a probe's request ended: refused as "not found" (what
// a caller who may not reach the shed is told), succeeded, or anything else.
type probeOutcome int

const (
	probeOther probeOutcome = iota
	probeRefused
	probeSucceeded
)

func (o probeOutcome) String() string {
	return [...]string{"neither refused nor succeeded", "refused", "succeeded"}[o]
}

// sshProbe asks for one kind of channel, request or subsystem on a
// connection logged in as box (alice's shed). run reports how it ended; call
// is the backend call the request makes when it is allowed ("exec" or
// "dial"), so a rightful caller's probe can be checked to have reached it.
type sshProbe struct {
	call string
	run  func(t *testing.T, client *gossh.Client) (probeOutcome, string)
}

// sessionOutcome classifies a session or subsystem's exit.
func sessionOutcome(stderr string, status int) probeOutcome {
	switch {
	case status == 0:
		return probeSucceeded
	case strings.Contains(stderr, "not found"):
		return probeRefused
	}
	return probeOther
}

// sshHandlerClasses classifies every handler the SSH server registers: each
// channel type, global request type and subsystem name. Each has a probe that
// asks for it; as a caller who may not reach the shed it must be refused
// without reaching the backend, which is what the handler's per-channel
// principal and the access guard decide, and as the shed's user it must
// succeed through the backend call named. A handler added to the server's
// maps without a class here fails TestSSHHandlersAreClassified.
var sshHandlerClasses = map[string]sshProbe{
	"channel session": {call: "exec", run: func(t *testing.T, c *gossh.Client) (probeOutcome, string) {
		stderr, status := runOn(t, c, "true", "")
		return sessionOutcome(stderr, status), fmt.Sprintf("status %d stderr %q", status, stderr)
	}},
	"channel direct-tcpip": {call: "dial", run: func(t *testing.T, c *gossh.Client) (probeOutcome, string) {
		conn, err := c.Dial("tcp", "localhost:8080")
		if err != nil {
			if strings.Contains(err.Error(), "not found") {
				return probeRefused, err.Error()
			}
			return probeOther, err.Error()
		}
		_ = conn.Close()
		return probeSucceeded, "opened"
	}},
	"subsystem sftp": {call: "exec", run: func(t *testing.T, c *gossh.Client) (probeOutcome, string) {
		stderr, status := runOn(t, c, "", "sftp")
		return sessionOutcome(stderr, status), fmt.Sprintf("status %d stderr %q", status, stderr)
	}},
}

// registeredSSHHandlers lists the handlers s will serve, as gliderlabs
// resolves them when it starts serving: a map left nil serves the package
// defaults.
func registeredSSHHandlers(s *Server) []string {
	srv := s.sshServer
	channels, requests, subsystems := srv.ChannelHandlers, srv.RequestHandlers, srv.SubsystemHandlers
	if channels == nil {
		channels = ssh.DefaultChannelHandlers
	}
	if requests == nil {
		requests = ssh.DefaultRequestHandlers
	}
	if subsystems == nil {
		subsystems = ssh.DefaultSubsystemHandlers
	}
	var out []string
	for k := range channels {
		out = append(out, "channel "+k)
	}
	for k := range requests {
		out = append(out, "request "+k)
	}
	for k := range subsystems {
		out = append(out, "subsystem "+k)
	}
	sort.Strings(out)
	return out
}

// TestSSHHandlersAreClassified walks the SSH server's registered handler maps
// (channel types, global requests, subsystems): every entry must be
// classified with a probe in sshHandlerClasses, and every class must still be
// registered. No forwarding callback is set (a reverse or socket forward would
// be a door of its own). Then each probe runs as bob on alice's shed and must
// be refused without reaching the backend, with alice's own probe of the same
// handler, as the control, reaching it.
func TestSSHHandlersAreClassified(t *testing.T) {
	who := newPeople(t)
	be := &identityBackend{}
	s, err := NewServer(be, filepath.Join(t.TempDir(), "host_key"), "127.0.0.1:0", nil,
		KeyAuth{Mode: config.SSHAuthEnforce, Users: who.lookup})
	if err != nil {
		t.Fatal(err)
	}
	registered := registeredSSHHandlers(s)
	seen := map[string]bool{}
	for _, h := range registered {
		seen[h] = true
		if _, ok := sshHandlerClasses[h]; !ok {
			t.Errorf("the SSH server registers %q with no class and probe in sshHandlerClasses: classify it", h)
		}
	}
	for h := range sshHandlerClasses {
		if !seen[h] {
			t.Errorf("sshHandlerClasses has %q, which the server does not register", h)
		}
	}
	if s.sshServer.LocalPortForwardingCallback != nil || s.sshServer.ReversePortForwardingCallback != nil {
		t.Error("a port forwarding callback is set: classify the door it opens")
	}
	if s.sshServer.Handler == nil {
		t.Fatal("no session handler")
	}

	addr := serveForTest(t, s)
	for h, probe := range sshHandlerClasses {
		t.Run(h, func(t *testing.T) {
			calls := func() (execs, dials []backend.Principal, n int) {
				execs, dials = be.seen()
				return execs, dials, len(execs) + len(dials)
			}
			_, _, before := calls()
			if got, detail := probe.run(t, dialWith(t, addr, "box", who.bob)); got != probeRefused {
				t.Errorf("bob on alice's shed: %s, want refused (%s)", got, detail)
			}
			if execs, dials, n := calls(); n != before {
				t.Errorf("bob's %s reached the backend: execs %v dials %v", h, execs, dials)
			}

			// The control: alice's own request succeeds, through exactly
			// one backend call of the kind named, made as alice.
			execsBefore, dialsBefore, _ := calls()
			if got, detail := probe.run(t, dialWith(t, addr, "box", who.alice)); got != probeSucceeded {
				t.Errorf("alice on her own shed: %s, want succeeded (%s)", got, detail)
			}
			execs, dials, _ := calls()
			made, other := execs[len(execsBefore):], dials[len(dialsBefore):]
			if probe.call == "dial" {
				made, other = other, made
			}
			if len(made) != 1 || made[0] != backend.UserPrincipal("alice", false) || len(other) != 0 {
				t.Errorf("alice's %s made backend %s calls %v (and other calls %v), want one %s as alice", h, probe.call, made, other, probe.call)
			}
		})
	}
}
