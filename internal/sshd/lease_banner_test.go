package sshd

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	gossh "golang.org/x/crypto/ssh"

	"github.com/charliek/shed/internal/backend"
	"github.com/charliek/shed/internal/config"
)

// The lease banner (plan 041 §3.4): a session with a terminal into a cloud
// shed whose lease ends soon is told once, on stderr; a session without one
// (rsync, `ssh host cmd`) never is, and stdout is never touched.

// binaryOut is what the shed's command writes: bytes a terminal's line
// discipline would mangle.
var binaryOut = []byte{0x00, 0x01, '\n', 0xff, '\r', 0x1b, '[', 'm'}

// cloudBackend holds alice's running cloud shed "box", whose lease ends in
// left. Its exec writes binaryOut.
type cloudBackend struct {
	backend.Backend
	left time.Duration
}

func (b *cloudBackend) Type() backend.Type { return backend.TypeRemote }

func (b *cloudBackend) GetShed(_ context.Context, name string) (*config.Shed, error) {
	if name != "box" {
		return nil, config.ErrShedNotFoundSentinel
	}
	return &config.Shed{Name: "box", User: "alice", Status: config.StatusRunning, CreatedAt: entryCreated,
		Cloud: &config.ShedCloud{Lease: &config.CloudLease{ExpiresAt: time.Now().Add(b.left), MaxExpiresAt: time.Now().Add(100 * time.Hour), Extendable: true}}}, nil
}

func (b *cloudBackend) Exec(_ context.Context, _ string, opts backend.ExecOptions) error {
	if opts.OnStarted != nil {
		opts.OnStarted()
	}
	_, _ = opts.Stdout.Write(binaryOut)
	return nil
}

// sessionOn runs one session: a shell (cmd "") or a command, with a
// terminal or without, and with the environment requests env, and returns
// its stdout and stderr.
func sessionOn(t *testing.T, client *gossh.Client, cmd string, pty bool, env ...string) (stdout, stderr []byte) {
	t.Helper()
	sess, err := client.NewSession()
	if err != nil {
		t.Fatalf("session: %v", err)
	}
	defer func() { _ = sess.Close() }()
	var out, errOut bytes.Buffer
	sess.Stdout, sess.Stderr = &out, &errOut
	for _, kv := range env {
		k, v, _ := strings.Cut(kv, "=")
		if err := sess.Setenv(k, v); err != nil {
			t.Fatalf("setenv %s: %v", kv, err)
		}
	}
	if pty {
		if err := sess.RequestPty("xterm", 24, 80, gossh.TerminalModes{}); err != nil {
			t.Fatalf("pty: %v", err)
		}
	}
	if cmd == "" {
		if err := sess.Shell(); err != nil {
			t.Fatalf("shell: %v", err)
		}
		err = sess.Wait()
	} else {
		err = sess.Run(cmd)
	}
	if err != nil {
		t.Fatalf("session (%q, pty %v): %v", cmd, pty, err)
	}
	return out.Bytes(), errOut.Bytes()
}

func TestLeaseBannerOnTerminalSessionsOnly(t *testing.T) {
	who := newPeople(t)
	const words = "shed: this cloud shed is deleted at "
	for _, tc := range []struct {
		name  string
		cmd   string
		pty   bool
		left  time.Duration
		count int
		env   []string
	}{
		{"a login with a terminal (console, raw ssh)", "", true, 80 * time.Minute, 1, nil},
		{"a command with a terminal (an interactive exec)", "ls", true, 80 * time.Minute, 1, nil},
		{"a command without a terminal (ssh host cmd, rsync)", "rsync --server", false, 80 * time.Minute, 0, nil},
		{"a shell without a terminal", "", false, 80 * time.Minute, 0, nil},
		{"a terminal, the lease far off", "", true, 30 * time.Hour, 0, nil},
		// The CLI under --json, or with stderr not a terminal, keeps its
		// terminal and asks for no banner.
		{"a terminal, the banner declined", "ls", true, 80 * time.Minute, 0, []string{"SHED_LEASE_BANNER=0"}},
		{"a login, the banner declined", "", true, 80 * time.Minute, 0, []string{"SHED_LEASE_BANNER=0"}},
		{"a terminal, another value", "", true, 80 * time.Minute, 1, []string{"SHED_LEASE_BANNER=1"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, addr := identityServer(t, &cloudBackend{left: tc.left}, who.lookup)
			client := dialWith(t, addr, "box", who.alice)
			stdout, stderr := sessionOn(t, client, tc.cmd, tc.pty, tc.env...)
			if n := strings.Count(string(stderr), words); n != tc.count {
				t.Fatalf("stderr holds the banner %d times, want %d: %q", n, tc.count, stderr)
			}
			if tc.count == 1 && !strings.Contains(string(stderr), "; extend with: shed extend box --hours N\r\n") {
				t.Fatalf("the banner's words: %q", stderr)
			}
			if !tc.pty && !bytes.Equal(stdout, binaryOut) {
				t.Fatalf("stdout = %q, want the command's bytes unchanged %q", stdout, binaryOut)
			}
			if bytes.Contains(stdout, []byte(words)) {
				t.Fatalf("the banner reached stdout: %q", stdout)
			}
		})
	}
}
