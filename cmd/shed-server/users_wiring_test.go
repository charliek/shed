package main

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	gossh "golang.org/x/crypto/ssh"

	"github.com/charliek/shed/internal/config"
	"github.com/charliek/shed/internal/plugin"
)

// loadWiringConfig loads body (after a backend block) through the CLI loader.
func loadWiringConfig(t *testing.T, body string) *config.ServerConfig {
	t.Helper()
	path := filepath.Join(t.TempDir(), "server.yaml")
	yaml := "name: test\ndefault_backend: firecracker\nfirecracker:\n  instance_dir: /tmp/ti\n" + body
	if err := os.WriteFile(path, []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.LoadServerConfigForCLI(path)
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

func wiringKey(t *testing.T) (line, fp string) {
	t.Helper()
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	k, err := gossh.NewPublicKey(pub)
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(gossh.MarshalAuthorizedKey(k))), gossh.FingerprintSHA256(k)
}

// TestServeWiresTheUserList: with auth.users the lookup shed-server builds
// gives each key to its user (no implicit user), and the bridge routes a
// user's shed to that user's listener. Without a user list both stay as
// before: the implicit user, and implicit routing.
func TestServeWiresTheUserList(t *testing.T) {
	aliceLine, aliceFP := wiringKey(t)
	cfg := loadWiringConfig(t, "auth:\n  mode: mtls\n  users:\n    alice:\n      admin: true\n      keys:\n        - {key: \""+aliceLine+"\", label: laptop}\n")

	lookup, err := newUserLookup(context.Background(), cfg, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if u, _, ok := lookup.UserForKey(aliceFP); !ok || u.Name != "alice" || !u.Admin || u.Implicit {
		t.Errorf("alice's key resolves to %+v %v", u, ok)
	}
	if _, ok := lookup.User(""); ok {
		t.Error("a server with a user list has the implicit user")
	}
	if got := publishedTo(t, cfg, "alice"); got != "alice" {
		t.Errorf("alice's shed's request reached %q's listener, want alice's", got)
	}

	line, fp := wiringKey(t)
	plain := loadWiringConfig(t, "auth:\n  mode: mtls\n  ssh:\n    authorized_keys: [\""+line+"\"]\n")
	lookup, err = newUserLookup(context.Background(), plain, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if u, _, ok := lookup.UserForKey(fp); !ok || !u.Implicit {
		t.Errorf("with no user list the key resolves to %+v %v, want the implicit user", u, ok)
	}
	if got := publishedTo(t, plain, "alice"); got != "" {
		t.Errorf("with no user list alice's shed's request reached %q's listener, want the implicit user's", got)
	}
}

// publishedTo registers a shed of user with the bridge shed-server builds for
// cfg, a listener for that user and one for the implicit user, publishes a
// request from the shed, and returns whose listener received it.
func publishedTo(t *testing.T, cfg *config.ServerConfig, user string) string {
	t.Helper()
	reg := plugin.NewRegistry()
	bridge := newPluginBridge(cfg, reg)
	gen := bridge.RegisterShed("box", &plugin.ShedConn{User: user, CreatedAt: time.Now(), Send: func(*plugin.Envelope) error { return nil }})
	listeners := map[string]*plugin.Listener{}
	for _, who := range []string{user, ""} {
		l, err := reg.Register(plugin.ListenerKey{User: who, Namespace: "ssh-agent"})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { reg.Unregister(l.Key(), l.Token) })
		listeners[who] = l
	}
	if err := bridge.PublishToHost(context.Background(), "box", gen, plugin.NewEnvelope("ssh-agent", plugin.MessageTypeRequest, nil)); err != nil {
		t.Fatal(err)
	}
	for who, l := range listeners {
		select {
		case <-l.Messages:
			return who
		default:
		}
	}
	t.Fatal("no listener received the request")
	return "?"
}
