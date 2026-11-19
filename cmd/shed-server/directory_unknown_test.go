package main

import (
	"net/http"
	"strings"
	"testing"
	"time"

	gossh "golang.org/x/crypto/ssh"

	"github.com/charliek/shed/internal/directory"
)

// TestNewPersonLogsInThroughTheUnknownKeyPull: a person added to the list the
// service serves, after the server's last pull, logs in over SSH (_bootstrap)
// with a key the server has never seen, with no refresh run by anyone: the
// login itself pulls the list. While that pull is held by the service, the
// local admin logs in at once, and a second unknown key is refused at once
// without a pull. The certificate the person's _bootstrap gives then works on
// the API.
func TestNewPersonLogsInThroughTheUnknownKeyPull(t *testing.T) {
	logs := captureEnrolLog(t)
	opsSigner, opsLine := e2eKey(t)
	adaSigner, adaLine := e2eKey(t)
	bobSigner, bobLine := e2eKey(t)
	ops := &e2eIdentity{name: "ops", signer: opsSigner, line: opsLine}
	ada := &e2eIdentity{name: "ada", signer: adaSigner, line: adaLine}
	bob := &e2eIdentity{name: "bob", signer: bobSigner, line: bobLine}

	s, _ := startE2EServer(t, ops)
	s.listUsers(map[*e2eIdentity]bool{})
	// The schedule's first pull, at start, has ended (the server's timers
	// never fire in this test, so no other pull is scheduled).
	deadline := time.After(10 * time.Second)
	for s.rt.Status().LastAttempt.IsZero() {
		select {
		case <-deadline:
			t.Fatal("the first pull did not end")
		case <-time.After(10 * time.Millisecond):
		}
	}
	// Unlisted, her key pulls and is refused at her deadline (8 s, a timer
	// the test fires), not when the pull ends; that takes the unknown-key
	// slot for a minute of the server's clock, which then passes.
	pulls, firstAttempt := s.recorder.pullCount(), s.rt.Status().LastAttempt
	unlisted := make(chan error, 1)
	go func() {
		c, err := s.dial(ada, "_bootstrap")
		if err == nil {
			_ = c.Close()
		}
		unlisted <- err
	}()
	deadline = time.After(10 * time.Second)
	// Her pull has ended (its attempt recorded) and her deadline is set.
	for s.recorder.pullCount() != pulls+1 || s.timers.armed(8*time.Second) != 1 || s.rt.Status().LastAttempt.Equal(firstAttempt) {
		select {
		case err := <-unlisted:
			t.Fatalf("ada's unlisted login answered before its pull and its deadline: %v", err)
		case <-deadline:
			t.Fatal("an unknown key's login did not pull the list and wait for its deadline")
		case <-time.After(10 * time.Millisecond):
		}
	}
	select {
	case err := <-unlisted:
		t.Fatalf("ada's unlisted login answered (%v) once her pull had ended, before her deadline", err)
	case <-time.After(300 * time.Millisecond):
	}
	if n := s.timers.fire(8 * time.Second); n != 1 {
		t.Fatalf("%d deadline timers fired, want 1", n)
	}
	select {
	case err := <-unlisted:
		if err == nil {
			t.Fatal("ada logged in before anyone listed her")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("ada's unlisted login was not answered at her deadline")
	}
	s.offset.Add(int64(directory.UnknownKeyEvery))

	// Ada is listed now; nobody refreshes. Her login pulls, and the service
	// holds that pull's answer.
	s.listUsers(map[*e2eIdentity]bool{ada: false})
	hold, held := make(chan struct{}), make(chan struct{}, 8)
	s.recorder.setHold(hold, held)
	released := false
	release := func() {
		if !released {
			released = true
			s.recorder.setHold(nil, nil)
			close(hold)
		}
	}
	defer release()
	type dialed struct {
		client *gossh.Client
		err    error
	}
	adaLogin := make(chan dialed, 1)
	go func() {
		c, err := s.dial(ada, "_bootstrap")
		adaLogin <- dialed{c, err}
	}()
	select {
	case <-held:
	case <-time.After(10 * time.Second):
		t.Fatal("ada's login did not pull the list")
	}
	pulls = s.recorder.pullCount()

	// The local admin, while that pull is held: in at once.
	quick := func(who *e2eIdentity, wantIn bool) {
		t.Helper()
		got := make(chan error, 1)
		go func() {
			c, err := s.dial(who, "_bootstrap")
			if err == nil {
				_ = c.Close()
			}
			got <- err
		}()
		select {
		case err := <-got:
			if (err == nil) != wantIn {
				t.Fatalf("%s's login while the pull is held: %v (want in: %v)", who.name, err, wantIn)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("%s's login waited on the held pull", who.name)
		}
	}
	asked := s.rt.PullsAsked()
	quick(ops, true)
	// Another unknown key: refused at once, with no pull of its own, asked
	// for or sent.
	quick(bob, false)
	if n := s.rt.PullsAsked() - asked; n != 0 {
		t.Fatalf("%d pulls asked for while the unknown-key slot was taken", n)
	}
	if n := s.recorder.pullCount(); n != pulls {
		t.Fatalf("%d more pulls while the unknown-key slot was taken", n-pulls)
	}
	select {
	case d := <-adaLogin:
		t.Fatalf("ada's login ended before her pull was answered: %v", d.err)
	default:
	}

	release()
	select {
	case d := <-adaLogin:
		if d.err != nil {
			t.Fatalf("ada's first login, through the unknown-key pull: %v", d.err)
		}
		_ = d.client.Close()
	case <-time.After(10 * time.Second):
		t.Fatal("ada's login did not end once her pull was answered")
	}
	// Her pull has ended (her key resolved through its list). No pull was
	// queued for bob behind hers: none was asked for since (every pull is
	// counted when it is made, so none is left to send), and the service
	// had her pull's one directory request and no other.
	if n := s.rt.PullsAsked() - asked; n != 0 {
		t.Fatalf("%d pulls asked for after ada's", n)
	}
	if n := s.recorder.pullCount(); n != pulls {
		t.Fatalf("%d directory requests after ada's, want none", n-pulls)
	}

	// She enrols a certificate at _bootstrap and the API answers her.
	held2 := s.enrol(ada)
	defer held2.Close()
	if code := s.api(ada, http.MethodGet, "/api/ssh-host-key"); code != http.StatusOK {
		t.Fatalf("ada's API request: HTTP %d", code)
	}
	if st := s.rt.Status(); st.LastError != "" || st.Version == "" {
		t.Fatalf("after the unknown-key pull: %+v", st)
	}
	if !strings.Contains(logs.String(), "unknown key "+gossh.FingerprintSHA256(adaSigner.PublicKey())) {
		t.Error("the unknown-key pull was not logged")
	}
}
