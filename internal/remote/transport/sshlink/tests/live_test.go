// Package tests holds the SSH link's live check: the transport conformance
// suite pointed at a real machine, by hand. It is how the link is proven
// against a cloud VM before anything else is built on it.
//
// It runs only when SHED_LINK_HOST is set, so "go test ./..." skips it.
//
//	SHED_LINK_HOST=<address or address:port> \
//	SHED_LINK_KEY_FILE=<path to the OpenSSH ed25519 private key> \
//	SHED_LINK_HOST_KEY='ssh-ed25519 AAAA...' \
//	go test ./internal/remote/transport/sshlink/tests -run TestLive -count=1 -v
//
// Optional settings:
//
//	SHED_LINK_USER         login name (default shed-link)
//	SHED_LINK_SOCKET_DIR   the agent's --listen unix:<dir> (default /run/shed-agent)
//	SHED_LINK_HTTP_PORT    the agent's --http-port (default 498)
//	SHED_LINK_STREAM_MIB   size of the large stream check (default 16)
//	SHED_LINK_RESTART_CMD  a local shell command that drops the machine's
//	                       connections and brings sshd and the agent back, for
//	                       the "dial after peer restart" check; without it
//	                       that check is skipped
//
// The host key is required and is never learned: get it over a path you
// trust (the machine's console output, its provider's API), not from the
// connection being tested. The machine needs a running
// "shed-agent --listen unix:<dir> --socket-group <the login user's group>",
// an sshd that lets the user forward to the agent's sockets, /bin/sh, cat and
// sleep. Nothing on the machine is changed; a few sleep commands are left to
// expire within five minutes.
package tests

import (
	"context"
	"io"
	"os"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/charliek/shed/internal/backend"
	"github.com/charliek/shed/internal/remote/transport"
	"github.com/charliek/shed/internal/remote/transport/conformance"
	"github.com/charliek/shed/internal/remote/transport/sshlink"
	"github.com/charliek/shed/internal/vmutil"
)

const usage = `set SHED_LINK_HOST, SHED_LINK_KEY_FILE and SHED_LINK_HOST_KEY to run the live check:

  SHED_LINK_HOST=<address or address:port> \
  SHED_LINK_KEY_FILE=<OpenSSH ed25519 private key> \
  SHED_LINK_HOST_KEY='ssh-ed25519 AAAA...' \
  go test ./internal/remote/transport/sshlink/tests -run TestLive -count=1 -v

optional: SHED_LINK_USER (shed-link), SHED_LINK_SOCKET_DIR (/run/shed-agent),
SHED_LINK_HTTP_PORT (498), SHED_LINK_STREAM_MIB (16), SHED_LINK_RESTART_CMD`

type liveConfig struct {
	host       string
	hostKey    string
	user       string
	socketDir  string
	httpPort   uint16
	streamMiB  int
	restartCmd string
	key        ssh.Signer
}

func loadLiveConfig(t *testing.T) liveConfig {
	t.Helper()
	cfg := liveConfig{
		host:       strings.TrimSpace(os.Getenv("SHED_LINK_HOST")),
		hostKey:    strings.TrimSpace(os.Getenv("SHED_LINK_HOST_KEY")),
		user:       os.Getenv("SHED_LINK_USER"),
		socketDir:  os.Getenv("SHED_LINK_SOCKET_DIR"),
		restartCmd: os.Getenv("SHED_LINK_RESTART_CMD"),
		httpPort:   conformance.DefaultAgentHTTPPort,
		streamMiB:  16,
	}
	if cfg.host == "" {
		t.Skip(usage)
	}
	// From here the caller meant to run it: a bad setting is a failure.
	if cfg.hostKey == "" {
		t.Fatal("SHED_LINK_HOST_KEY is not set. The live check pins the host key; it never learns one.\n" + usage)
	}
	if _, err := sshlink.ParseHostKey(cfg.hostKey); err != nil {
		t.Fatalf("SHED_LINK_HOST_KEY: %v", err)
	}
	keyFile := os.Getenv("SHED_LINK_KEY_FILE")
	if keyFile == "" {
		t.Fatal("SHED_LINK_KEY_FILE is not set.\n" + usage)
	}
	key, err := sshlink.ReadClientKeyFile(keyFile)
	if err != nil {
		t.Fatalf("SHED_LINK_KEY_FILE: %v", err)
	}
	cfg.key = key

	if v := os.Getenv("SHED_LINK_HTTP_PORT"); v != "" {
		port, err := strconv.ParseUint(v, 10, 16)
		if err != nil || port == 0 {
			t.Fatalf("SHED_LINK_HTTP_PORT=%q is not a port", v)
		}
		cfg.httpPort = uint16(port)
	}
	if v := os.Getenv("SHED_LINK_STREAM_MIB"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 4096 {
			t.Fatalf("SHED_LINK_STREAM_MIB=%q: want 1 to 4096", v)
		}
		cfg.streamMiB = n
	}
	return cfg
}

// TestLive runs the transport conformance suite against the machine named by
// the environment, then measures it.
func TestLive(t *testing.T) {
	cfg := loadLiveConfig(t)

	factory, err := sshlink.New(sshlink.Config{
		Keys:      []ssh.Signer{cfg.key},
		User:      cfg.user,
		SocketDir: cfg.socketDir,
		Logf:      t.Logf,
	})
	if err != nil {
		t.Fatal(err)
	}
	open := func(t *testing.T) transport.Link {
		link, err := factory.Open(context.Background(), transport.Target{
			ShedName: "live",
			Addr:     func(context.Context) (string, error) { return cfg.host, nil },
			HostKeys: sshlink.PinnedHostKey(cfg.hostKey),
		})
		if err != nil {
			t.Fatal(err)
		}
		return link
	}

	harness := conformance.Harness{
		Open:        open,
		HTTPPort:    func(transport.Link) uint16 { return cfg.httpPort },
		LargeStream: cfg.streamMiB << 20,
		// A machine across the internet that reboots takes its time.
		DropDetect: 45 * time.Second,
		Recover:    5 * time.Minute,
	}
	if cfg.restartCmd != "" {
		harness.Restart = func(t *testing.T, _ transport.Link) {
			t.Logf("running SHED_LINK_RESTART_CMD")
			cmd := exec.Command("/bin/sh", "-c", cfg.restartCmd)
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("SHED_LINK_RESTART_CMD: %v\n%s", err, out)
			}
		}
	}
	t.Run("conformance", func(t *testing.T) { conformance.Run(t, harness) })

	t.Run("measurements", func(t *testing.T) {
		link := open(t)
		defer link.Close()
		client := vmutil.NewAgentClient(link, transport.PortConsole, transport.PortNotify)
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()

		start := time.Now()
		info, err := client.WaitForProbe(ctx, time.Minute)
		if err != nil {
			t.Fatalf("no probe succeeded: %v", err)
		}
		t.Logf("connect and first probe: %v (agent %q, protocol %d, layout %d)",
			time.Since(start).Round(time.Millisecond), info.Version, info.Protocol, info.Layout)

		const n = 21
		median := func(fn func() error) (lo, mid, hi time.Duration) {
			times := make([]time.Duration, 0, n)
			for range n {
				start := time.Now()
				if err := fn(); err != nil {
					t.Fatal(err)
				}
				times = append(times, time.Since(start))
			}
			slices.Sort(times)
			return times[0], times[n/2], times[n-1]
		}
		lo, mid, hi := median(func() error { _, err := client.Probe(ctx); return err })
		t.Logf("probe over %d: min %v, median %v, max %v", n, lo.Round(100*time.Microsecond), mid.Round(100*time.Microsecond), hi.Round(100*time.Microsecond))
		lo, mid, hi = median(func() error {
			return client.Exec(ctx, backend.ExecOptions{
				Cmd:    []string{"/bin/sh", "-c", "echo hi"},
				Stdout: vmutil.NopWriteCloser(io.Discard),
			})
		})
		t.Logf("exec of \"echo hi\" over %d: min %v, median %v, max %v", n, lo.Round(100*time.Microsecond), mid.Round(100*time.Microsecond), hi.Round(100*time.Microsecond))
	})
}
