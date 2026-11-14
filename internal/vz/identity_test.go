//go:build darwin

package vz

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/charliek/shed/internal/backend"
	"github.com/charliek/shed/internal/config"
	"github.com/charliek/shed/internal/systemprune"
)

// The VZ twin of internal/firecracker/identity_test.go. These tests force the
// interleaving the access guard is built against, with the guard's own hook
// (not timing): after the guard has loaded and authorized alice's shed (or
// snapshot), it is deleted and replaced under the same name by bob's, and
// only then does the backend run. The backend must check the incarnation the
// guard passed down against what it loads, under its lock, and refuse; bob's
// shed must be untouched.

// newIdentityTestClient is newUserTestClient with egress on and the defaults
// a create's preflight reads.
func newIdentityTestClient(t *testing.T) (*Client, string) {
	t.Helper()
	dir := t.TempDir()
	c := newUserTestClient(t, dir)
	c.cfg.DefaultCPUs = 2
	c.cfg.DefaultMemoryMB = 512
	c.serverCfg = &config.ServerConfig{Egress: &config.EgressConfig{Enabled: true}}
	c.SetEgressManager(newTestEgressManager(t))
	return c, dir
}

// replaceShed deletes the named shed's record and writes bob's in its place:
// a later incarnation, created a minute after.
func replaceShed(t *testing.T, dir, name string) *Metadata {
	t.Helper()
	old, err := LoadMetadata(dir, name)
	if err != nil {
		t.Fatalf("replace %s: %v", name, err)
	}
	if err := old.Delete(dir); err != nil {
		t.Fatalf("delete %s: %v", name, err)
	}
	return saveUserTestShed(t, dir, name, func(m *Metadata) {
		m.User = "bob"
		m.CreatedAt = old.CreatedAt.Add(time.Minute)
		// Egress state of bob's own, so an egress change made to it shows.
		m.EgressProfiles = []string{"bobs"}
		m.EgressPort = 41005
	})
}

// TestGuardedOperationsRefuseAShedReplacedAfterTheLookup: every operation on
// a named shed, through the guard, refuses a shed deleted and created again
// under its name between the guard's lookup and the backend's lock.
func TestGuardedOperationsRefuseAShedReplacedAfterTheLookup(t *testing.T) {
	alice := backend.WithPrincipal(context.Background(), backend.UserPrincipal("alice", false))
	ops := map[string]func(g *backend.Guard) error{
		"StartShed":  func(g *backend.Guard) error { _, err := g.StartShed(alice, "box"); return err },
		"StopShed":   func(g *backend.Guard) error { _, err := g.StopShed(alice, "box"); return err },
		"DeleteShed": func(g *backend.Guard) error { return g.DeleteShed(alice, "box") },
		"ResetShed":  func(g *backend.Guard) error { _, err := g.ResetShed(alice, "box"); return err },
		"Exec": func(g *backend.Guard) error {
			return g.Exec(alice, "box", backend.ExecOptions{Cmd: []string{"true"}})
		},
		"ListSessions": func(g *backend.Guard) error { _, err := g.ListSessions(alice, "box"); return err },
		"KillSession":  func(g *backend.Guard) error { return g.KillSession(alice, "box", "main") },
		"DialService":  func(g *backend.Guard) error { _, err := g.DialService(alice, "box", 22); return err },
		"CreateSnapshot": func(g *backend.Guard) error {
			_, err := g.CreateSnapshot(alice, config.SnapshotCreateRequest{Name: "of-box", SourceShed: "box"})
			return err
		},
		"SetShedEgress": func(g *backend.Guard) error {
			ec, ok := backend.AsEgressController(g)
			if !ok {
				return errors.New("no egress controller through the guard")
			}
			_, err := ec.SetShedEgress(alice, "box", []string{"off"})
			return err
		},
		"ClearShedEgress": func(g *backend.Guard) error {
			ec, ok := backend.AsEgressController(g)
			if !ok {
				return errors.New("no egress controller through the guard")
			}
			_, err := ec.ClearShedEgress(alice, "box")
			return err
		},
	}
	for op, call := range ops {
		t.Run(op, func(t *testing.T) {
			c, dir := newIdentityTestClient(t)
			saveUserTestShed(t, dir, "box", func(m *Metadata) {
				m.User = "alice"
				// As a create for a user who is not an admin stores it.
				m.MountSelection = config.SelectedMounts(nil)
			})
			g := backend.NewGuard(NewBackend(c))
			var bobs *Metadata
			g.SetBeforeAct(func(_, name string) {
				if name == "box" && bobs == nil {
					bobs = replaceShed(t, dir, "box")
				}
			})

			err := call(g)
			if !errors.Is(err, config.ErrShedNotFoundSentinel) {
				t.Fatalf("%s across a replace: %v, want shed not found", op, err)
			}
			if bobs == nil {
				t.Fatal("the guard never reached its hook")
			}
			now, lerr := LoadMetadata(dir, "box")
			if lerr != nil {
				t.Fatalf("bob's shed is gone after %s: %v", op, lerr)
			}
			if now.User != "bob" || !now.CreatedAt.Equal(bobs.CreatedAt) || now.Status != config.StatusStopped || now.UpperPath != bobs.UpperPath || len(now.EgressProfiles) != 1 || now.EgressPort != 41005 {
				t.Errorf("bob's shed changed under %s: %+v", op, now)
			}
			if _, serr := os.Stat(SnapshotDir(c.cfg.SnapshotsDir, "of-box")); !errors.Is(serr, os.ErrNotExist) {
				t.Errorf("%s left a snapshot of bob's shed (%v)", op, serr)
			}
		})
	}
}

// TestUnderLockChecksRefuseAnotherIncarnation is the same check without the
// guard: each operation called with a required creation time that is not the
// shed's (as the guard passes down after a replace) refuses it.
func TestUnderLockChecksRefuseAnotherIncarnation(t *testing.T) {
	c, dir := newIdentityTestClient(t)
	meta := saveUserTestShed(t, dir, "box", func(m *Metadata) { m.User = "alice" })
	stale := backend.WithRequiredCreatedAt(context.Background(), meta.CreatedAt.Add(-time.Hour))
	b := NewBackend(c)
	checks := map[string]error{}
	_, checks["GetShed"] = c.GetShed(stale, "box")
	_, checks["StartShed"] = c.StartShed(stale, "box")
	_, checks["ResetShed"] = c.ResetShed(stale, "box")
	_, checks["DialService"] = c.DialService(stale, "box", 22)
	_, checks["SetShedEgress"] = c.SetShedEgress(stale, "box", []string{"off"})
	_, checks["ClearShedEgress"] = c.ClearShedEgress(stale, "box")
	_, checks["ListSessions"] = b.ListSessions(stale, "box")
	checks["KillSession"] = b.KillSession(stale, "box", "main")
	checks["Exec"] = b.Exec(stale, "box", backend.ExecOptions{Cmd: []string{"true"}})
	_, checks["CreateSnapshot"] = c.CreateSnapshot(stale, config.SnapshotCreateRequest{Name: "of-box", SourceShed: "box"})
	for op, err := range checks {
		if !errors.Is(err, config.ErrShedNotFoundSentinel) {
			t.Errorf("%s with another incarnation required: %v, want shed not found", op, err)
		}
	}
	if _, err := c.GetShed(backend.WithRequiredCreatedAt(context.Background(), meta.CreatedAt), "box"); err != nil {
		t.Errorf("GetShed with the shed's own incarnation: %v", err)
	}
}

// TestGuardedSnapshotOperationsRefuseASnapshotRetakenAfterTheLookup: a
// snapshot deleted and taken again under its name (bob's, from bob's shed)
// between the guard's lookup and the snapshot lock is not deleted, and no
// shed is created from it.
func TestGuardedSnapshotOperationsRefuseASnapshotRetakenAfterTheLookup(t *testing.T) {
	alice := backend.WithPrincipal(context.Background(), backend.UserPrincipal("alice", false))
	setup := func(t *testing.T) (*Client, string, *backend.Guard, func() *config.Snapshot) {
		c, dir := newIdentityTestClient(t)
		saveUserTestShed(t, dir, "alices", func(m *Metadata) { m.User = "alice" })
		saveUserTestShed(t, dir, "bobs", func(m *Metadata) { m.User = "bob" })
		sys := context.Background()
		if _, err := c.CreateSnapshot(sys, config.SnapshotCreateRequest{Name: "snap", SourceShed: "alices"}); err != nil {
			t.Fatal(err)
		}
		g := backend.NewGuard(NewBackend(c))
		var retaken *config.Snapshot
		g.SetBeforeAct(func(_, name string) {
			if retaken != nil || (name != "snap" && name != "spawn") {
				return
			}
			if err := c.DeleteSnapshot(sys, "snap"); err != nil {
				t.Fatalf("delete alice's snapshot: %v", err)
			}
			snap, err := c.CreateSnapshot(sys, config.SnapshotCreateRequest{Name: "snap", SourceShed: "bobs"})
			if err != nil {
				t.Fatalf("retake as bob's: %v", err)
			}
			retaken = snap
		})
		return c, dir, g, func() *config.Snapshot { return retaken }
	}

	t.Run("DeleteSnapshot", func(t *testing.T) {
		c, _, g, retaken := setup(t)
		if err := g.DeleteSnapshot(alice, "snap"); !errors.Is(err, config.ErrSnapshotNotFoundSentinel) {
			t.Fatalf("DeleteSnapshot across a retake: %v, want snapshot not found", err)
		}
		if retaken() == nil {
			t.Fatal("the guard never reached its hook")
		}
		got, err := loadSnapshot(c.cfg.SnapshotsDir, "snap")
		if err != nil || got.User != "bob" || !got.CreatedAt.Equal(retaken().CreatedAt) {
			t.Errorf("bob's snapshot after the refused delete: %+v, %v", got, err)
		}
	})

	t.Run("CreateShedFromSnapshot", func(t *testing.T) {
		_, dir, g, retaken := setup(t)
		_, err := g.CreateShed(alice, config.CreateShedRequest{Name: "spawn", FromSnapshot: "snap", ResolvedMounts: config.SelectedMounts(nil)})
		if !errors.Is(err, config.ErrSnapshotNotFoundSentinel) {
			t.Fatalf("create from a retaken snapshot: %v, want snapshot not found", err)
		}
		if retaken() == nil {
			t.Fatal("the guard never reached its hook")
		}
		if _, err := LoadMetadata(dir, "spawn"); !errors.Is(err, ErrInstanceNotFound) {
			t.Errorf("a shed was made from bob's snapshot (%v)", err)
		}
	})
}

// fakeVMSocket listens on a shed's per-port sockets as its VM would. It
// records every byte the host sends on a connection and answers a CONNECT
// line as the guest's tcp proxy does, so a host that went on would send its
// request too. Each connection's bytes are reported once the host closes it.
type fakeVMSocket struct {
	done chan string
}

func newFakeVMSocket() *fakeVMSocket { return &fakeVMSocket{done: make(chan string, 16)} }

func listenFakeVMSocket(t *testing.T, path string, f *fakeVMSocket) {
	t.Helper()
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatalf("listen %s: %v", path, err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				_ = conn.SetDeadline(time.Now().Add(3 * time.Second))
				var got bytes.Buffer
				r := bufio.NewReader(io.TeeReader(conn, &got))
				for {
					line, err := r.ReadString('\n')
					if err != nil {
						break
					}
					if strings.HasPrefix(line, "CONNECT ") {
						if _, err := conn.Write([]byte("OK\n")); err != nil {
							break
						}
					}
				}
				f.done <- got.String()
			}()
		}
	}()
}

// next waits for the next connection to the fake to be closed and returns
// every byte the host sent on it; ok is false when none was made.
func (f *fakeVMSocket) next() (received string, ok bool) {
	select {
	case received = <-f.done:
		return received, true
	case <-time.After(5 * time.Second):
		return "", false
	}
}

// vmDialOps are the operations that reach into a running VM and hold no lock
// while they connect: exec (which sftp and every SSH session use), the
// session list and kill, and a service dial.
var vmDialOps = map[string]func(b *VZBackend, ctx context.Context) error{
	"Exec": func(b *VZBackend, ctx context.Context) error {
		return b.Exec(ctx, "box", backend.ExecOptions{Cmd: []string{"true"}})
	},
	"ListSessions": func(b *VZBackend, ctx context.Context) error {
		_, err := b.ListSessions(ctx, "box")
		return err
	},
	"KillSession": func(b *VZBackend, ctx context.Context) error { return b.KillSession(ctx, "box", "main") },
	"DialService": func(b *VZBackend, ctx context.Context) error {
		conn, err := b.DialService(ctx, "box", 22)
		if conn != nil {
			conn.Close()
		}
		return err
	},
}

// runningAlice makes alice's running shed "box" and a short socket directory
// (a unix socket path has a small limit).
func runningAlice(t *testing.T) (c *Client, dir, sockets string, alices *Metadata) {
	t.Helper()
	c, dir = newIdentityTestClient(t)
	sockets, err := os.MkdirTemp("", "vzs")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(sockets) })
	c.cfg.SocketDir = sockets
	c.cfg.ConsolePort = 1024
	c.cfg.TCPProxyPort = 1025
	alices = saveUserTestShed(t, dir, "box", func(m *Metadata) {
		m.User = "alice"
		m.Status = config.StatusRunning
		m.MountSelection = config.SelectedMounts(nil)
	})
	return c, dir, sockets, alices
}

// listenVM listens on every per-port socket the operations use for the shed.
func listenVM(t *testing.T, c *Client, sockets, name string) *fakeVMSocket {
	t.Helper()
	vm := newFakeVMSocket()
	for _, port := range []uint32{c.cfg.ConsolePort, c.cfg.TCPProxyPort} {
		listenFakeVMSocket(t, filepath.Join(sockets, fmt.Sprintf("%s-%d.sock", name, port)), vm)
	}
	return vm
}

// TestVMConnectionRefusedAfterAReplaceBeforeTheConnect: the VZ twin of the
// Firecracker test. After the backend has checked alice's record and before
// it connects, her shed is deleted and bob's running shed takes the name, and
// with it the per-port sockets. The backend must recheck the record once
// connected, close the connection before sending anything on it, and report
// the shed not found.
func TestVMConnectionRefusedAfterAReplaceBeforeTheConnect(t *testing.T) {
	for op, call := range vmDialOps {
		t.Run(op, func(t *testing.T) {
			c, dir, sockets, alices := runningAlice(t)
			var bobsVM *fakeVMSocket
			c.beforeVMDial = func(name string) {
				if name != "box" || bobsVM != nil {
					return
				}
				bobs := replaceShed(t, dir, "box")
				bobs.Status = config.StatusRunning
				if err := bobs.Save(dir); err != nil {
					t.Fatal(err)
				}
				bobsVM = listenVM(t, c, sockets, "box")
			}
			// As the guard passes it down: alice's incarnation.
			ctx, cancel := context.WithTimeout(backend.WithRequiredCreatedAt(context.Background(), alices.CreatedAt), 5*time.Second)
			defer cancel()

			err := call(NewBackend(c), ctx)
			if bobsVM == nil {
				t.Fatal("the backend never reached its dial hook")
			}
			if !errors.Is(err, config.ErrShedNotFoundSentinel) {
				t.Fatalf("%s across a replace before the connect: %v, want shed not found", op, err)
			}
			received, ok := bobsVM.next()
			if !ok {
				t.Fatal("the connection never reached bob's VM: the window was not exercised")
			}
			if received != "" {
				t.Errorf("%s sent %q to bob's VM", op, received)
			}
		})
	}
}

// TestVMConnectionRefusedWhenTheRecheckCannotRead: a record that cannot be
// read when the connection is rechecked is not trusted. The connection is
// closed with nothing written on it and the operation fails.
func TestVMConnectionRefusedWhenTheRecheckCannotRead(t *testing.T) {
	for op, call := range vmDialOps {
		t.Run(op, func(t *testing.T) {
			c, dir, sockets, alices := runningAlice(t)
			var vm *fakeVMSocket
			c.beforeVMDial = func(name string) {
				if name != "box" || vm != nil {
					return
				}
				// A directory where the record should be: every read fails,
				// and not with "does not exist".
				path := MetadataPath(dir, "box")
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
				if err := os.Mkdir(path, 0o755); err != nil {
					t.Fatal(err)
				}
				vm = listenVM(t, c, sockets, "box")
			}
			ctx, cancel := context.WithTimeout(backend.WithRequiredCreatedAt(context.Background(), alices.CreatedAt), 5*time.Second)
			defer cancel()

			err := call(NewBackend(c), ctx)
			if vm == nil {
				t.Fatal("the backend never reached its dial hook")
			}
			if err == nil || !strings.Contains(err.Error(), "rechecking shed box") {
				t.Fatalf("%s with an unreadable record at the recheck: %v, want the recheck's error", op, err)
			}
			received, ok := vm.next()
			if !ok {
				t.Fatal("the connection never reached the VM: the window was not exercised")
			}
			if received != "" {
				t.Errorf("%s sent %q on a connection it could not recheck", op, received)
			}
		})
	}
}

// TestVMConnectionKeptForTheSameIncarnation: with the shed left alone, the
// recheck passes and the connection is kept, on every dial of the same
// client.
func TestVMConnectionKeptForTheSameIncarnation(t *testing.T) {
	c, _, sockets, alices := runningAlice(t)
	vm := listenVM(t, c, sockets, "box")
	ctx, cancel := context.WithTimeout(backend.WithRequiredCreatedAt(context.Background(), alices.CreatedAt), 5*time.Second)
	defer cancel()
	b := NewBackend(c)
	for i := 0; i < 2; i++ {
		conn, err := b.DialService(ctx, "box", 22)
		if err != nil {
			t.Fatalf("dial %d of the unchanged shed: %v", i, err)
		}
		conn.Close()
		received, ok := vm.next()
		if !ok {
			t.Fatalf("dial %d never reached the VM", i)
		}
		if received != "CONNECT 22\n" {
			t.Errorf("dial %d: the VM received %q, want the handshake %q", i, received, "CONNECT 22\n")
		}
	}
}

// TestSnapshotCreateLeavesAnUnreadableSnapshotAlone: the VZ twin. Only a
// definitive "no such snapshot" frees a name: bob's snapshot whose
// snapshot.json cannot be parsed, or cannot be read, is still his, and
// alice's create under that name is refused with his rootfs untouched.
func TestSnapshotCreateLeavesAnUnreadableSnapshotAlone(t *testing.T) {
	alice := backend.WithPrincipal(context.Background(), backend.UserPrincipal("alice", false))
	for what, spoil := range map[string]func(t *testing.T, path string){
		"malformed": func(t *testing.T, path string) {
			if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
				t.Fatal(err)
			}
		},
		"unreadable": func(t *testing.T, path string) {
			if err := os.Remove(path); err != nil {
				t.Fatal(err)
			}
			if err := os.Mkdir(path, 0o755); err != nil {
				t.Fatal(err)
			}
		},
	} {
		t.Run(what, func(t *testing.T) {
			c, dir := newIdentityTestClient(t)
			saveUserTestShed(t, dir, "alices", func(m *Metadata) {
				m.User = "alice"
				m.MountSelection = config.SelectedMounts(nil)
			})
			saveUserTestShed(t, dir, "bobs", func(m *Metadata) { m.User = "bob" })
			if _, err := c.CreateSnapshot(context.Background(), config.SnapshotCreateRequest{Name: "snap", SourceShed: "bobs"}); err != nil {
				t.Fatal(err)
			}
			rootfs := SnapshotRootfsPath(c.cfg.SnapshotsDir, "snap")
			before, err := os.ReadFile(rootfs)
			if err != nil {
				t.Fatal(err)
			}
			spoil(t, SnapshotMetadataPath(c.cfg.SnapshotsDir, "snap"))

			g := backend.NewGuard(NewBackend(c))
			_, err = g.CreateSnapshot(alice, config.SnapshotCreateRequest{Name: "snap", SourceShed: "alices"})
			if !errors.Is(err, config.ErrSnapshotAlreadyExistsSentinel) {
				t.Fatalf("create over a %s snapshot: %v, want the name in use", what, err)
			}
			after, err := os.ReadFile(rootfs)
			if err != nil || string(after) != string(before) {
				t.Errorf("bob's snapshot rootfs after the refused create: %q (%v), want %q", after, err, before)
			}
		})
	}
}

// TestSnapshotCreateOverAPartialDirectory: the VZ twin. A directory left by a
// create that never finished (no snapshot.json) is retried over; a
// snapshot.json whose rootfs is missing is still a snapshot and is refused.
func TestSnapshotCreateOverAPartialDirectory(t *testing.T) {
	alice := backend.WithPrincipal(context.Background(), backend.UserPrincipal("alice", false))
	setup := func(t *testing.T) (*Client, *backend.Guard) {
		c, dir := newIdentityTestClient(t)
		saveUserTestShed(t, dir, "alices", func(m *Metadata) {
			m.User = "alice"
			m.MountSelection = config.SelectedMounts(nil)
		})
		saveUserTestShed(t, dir, "bobs", func(m *Metadata) { m.User = "bob" })
		return c, backend.NewGuard(NewBackend(c))
	}

	t.Run("unfinished, no metadata", func(t *testing.T) {
		c, g := setup(t)
		dir := SnapshotDir(c.cfg.SnapshotsDir, "snap")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		for name, body := range map[string]string{
			snapshotRootfsFilename:             "unfinished",
			".snapshot-1.json.tmp":             `{"name":"snap","user":"bob"}`,
			systemprune.SnapshotCreatingMarker: "",
		} {
			if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		snap, err := g.CreateSnapshot(alice, config.SnapshotCreateRequest{Name: "snap", SourceShed: "alices"})
		if err != nil {
			t.Fatalf("create over an unfinished snapshot directory: %v", err)
		}
		if snap.User != "alice" {
			t.Errorf("snapshot user %q, want alice", snap.User)
		}
		got, err := c.GetSnapshot(context.Background(), "snap")
		if err != nil || got.User != "alice" {
			t.Errorf("stored snapshot %+v, %v, want alice's", got, err)
		}
		if body, err := os.ReadFile(SnapshotRootfsPath(c.cfg.SnapshotsDir, "snap")); err != nil || string(body) != "upper" {
			t.Errorf("snapshot rootfs %q (%v), want alice's upper", body, err)
		}
	})

	t.Run("metadata, no rootfs", func(t *testing.T) {
		c, g := setup(t)
		if _, err := c.CreateSnapshot(context.Background(), config.SnapshotCreateRequest{Name: "snap", SourceShed: "bobs"}); err != nil {
			t.Fatal(err)
		}
		if err := os.Remove(SnapshotRootfsPath(c.cfg.SnapshotsDir, "snap")); err != nil {
			t.Fatal(err)
		}
		_, err := g.CreateSnapshot(alice, config.SnapshotCreateRequest{Name: "snap", SourceShed: "alices"})
		if !errors.Is(err, config.ErrSnapshotAlreadyExistsSentinel) {
			t.Fatalf("create over a snapshot with no rootfs: %v, want the name in use", err)
		}
		got, err := loadSnapshot(c.cfg.SnapshotsDir, "snap")
		if err != nil || got.User != "bob" {
			t.Errorf("bob's snapshot after the refused create: %+v, %v", got, err)
		}
		if _, err := os.Stat(SnapshotRootfsPath(c.cfg.SnapshotsDir, "snap")); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("the refused create wrote a rootfs (%v)", err)
		}
	})
}

// TestReportedHostResources: a VZ shed reports what it carries from the host
// the way the access rule reads it (config.Shed.HostResources): host
// directories, and a server-mount selection that is not explicitly empty,
// including the {mode: all} of a shed made before selection existed.
func TestReportedHostResources(t *testing.T) {
	for _, tc := range []struct {
		name string
		meta Metadata
		want int
	}{
		{"legacy selection", Metadata{Name: "a"}, 1},
		{"no mounts", Metadata{Name: "a", MountSelection: config.SelectedMounts(nil)}, 0},
		{"named mount", Metadata{Name: "a", MountSelection: config.SelectedMounts([]string{"gh"})}, 1},
		{"host directories", Metadata{Name: "a", MountSelection: config.SelectedMounts(nil), ProjectMounts: []config.MountConfig{{Source: "/src", Target: "/home/shed/src"}}}, 1},
	} {
		if got := metadataToShed(&tc.meta, "").HostResources(); len(got) != tc.want {
			t.Errorf("%s: HostResources = %v", tc.name, got)
		}
	}
}
