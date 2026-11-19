package roostprovider

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"encoding/pem"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/charliek/shed/internal/config"
	"github.com/charliek/shed/internal/servertls"
)

// writeTestKey writes a new unencrypted OpenSSH-format ed25519 private key at
// path: an identity file whose public key can be learned.
func writeTestKey(t *testing.T, path string) {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	block, err := ssh.MarshalPrivateKey(priv, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, pem.EncodeToMemory(block), 0o600); err != nil {
		t.Fatal(err)
	}
}

// keyOf is the fingerprint of the identity file at path, as Argv learns it
// for the mux key.
func keyOf(t *testing.T, path string) string {
	t.Helper()
	k, err := config.IdentityFileKey("", path)
	if err != nil {
		t.Fatal(err)
	}
	return k
}

// TestShedTargetArgvWithIdentityFile: a shed whose server entry has an
// identity file is reached with `-i <path> -o IdentitiesOnly=yes`, right after
// the host-key options, and with no shared connection (`-o ControlMaster=no -o
// ControlPath=none`) when the Remote keeps none of its own; with a ControlDir
// the transport's own ControlMaster and ControlPath, keyed by the identity
// file, are the ones given. The argv is otherwise the one TestShedTargetArgv
// pins (which still passes unchanged, with no identity file).
func TestShedTargetArgvWithIdentityFile(t *testing.T) {
	key := filepath.Join(t.TempDir(), "id work")
	writeTestKey(t, key)
	target := ShedTarget(
		RunningShed{Name: "dev", Server: "my-server", ServerHost: "localhost", ServerSSHPort: 2222, IdentityFile: key},
		"/home/me/.shed/known_hosts",
	)
	if target.IdentityFile != key || target.Entry != "my-server" {
		t.Fatalf("target = %+v, want the identity file and its entry carried", target)
	}
	head := []string{
		"/usr/bin/ssh",
		"-o", "BatchMode=yes",
		"-o", "StrictHostKeyChecking=yes",
		"-o", "UserKnownHostsFile=/home/me/.shed/known_hosts",
		"-i", key,
		"-o", "IdentitiesOnly=yes",
	}
	tail := []string{"-T", "-p", "2222", "dev@localhost", "--", "echo hi"}

	got := mustArgv(t, &Remote{SSHBin: "/usr/bin/ssh"}, target, "echo hi")
	want := append(append(append([]string{}, head...),
		"-o", "ControlMaster=no", "-o", "ControlPath=none",
		"-o", "ConnectTimeout=5"), tail...)
	assertSeq(t, "argv without a ControlDir", got, want)

	r := &Remote{SSHBin: "/usr/bin/ssh", ControlDir: "/run/user/1000/shed-roost-provider"}
	got = mustArgv(t, r, target, "echo hi")
	want = append(append(append([]string{}, head...),
		"-o", "ConnectTimeout=5",
		"-o", "ControlMaster=auto",
		"-o", "ControlPersist="+controlPersistArg,
		"-o", "ControlPath="+controlPath(r.ControlDir, target.muxKey(keyOf(t, key)))), tail...)
	assertSeq(t, "argv with a ControlDir", got, want)
}

// TestShedTargetSharedConnectionInForce runs `ssh -G` (which prints the
// options in force and connects nowhere) on the provider's argv against an ssh
// config that sets `ControlMaster auto` and a ControlPath of its own for every
// host, as a person's might: without a ControlDir no shared connection is
// used at all, and with one the provider's own ControlPath, keyed by the
// identity file, is the one in force, never the person's. Skipped where no
// ssh is installed.
func TestShedTargetSharedConnectionInForce(t *testing.T) {
	sshBin, err := exec.LookPath("ssh")
	if err != nil {
		t.Skip("no ssh on PATH")
	}
	dir := t.TempDir()
	key := filepath.Join(dir, "id_work")
	writeTestKey(t, key)
	cfg := filepath.Join(dir, "ssh_config")
	if err := os.WriteFile(cfg, []byte("Host *\n    ControlMaster auto\n    ControlPath "+dir+"/theirs-%C\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	target := ShedTarget(RunningShed{Name: "dev", Server: "srv", ServerHost: "shed.example.test", ServerSSHPort: 2222, IdentityFile: key},
		filepath.Join(dir, "known_hosts"))
	effective := func(t *testing.T, r *Remote) map[string]string {
		t.Helper()
		argv := mustArgv(t, r, target, "cmd")
		// `ssh -G -F <cfg>` with every option of the argv, up to the
		// destination (the `--` and the command are not options).
		args := append([]string{"-G", "-F", cfg}, argv[1:len(argv)-2]...)
		out, err := exec.Command(sshBin, args...).Output()
		if err != nil {
			t.Fatalf("ssh -G: %v", err)
		}
		got := map[string]string{}
		for _, line := range strings.Split(string(out), "\n") {
			if k, v, ok := strings.Cut(line, " "); ok {
				got[k] = v
			}
		}
		return got
	}

	got := effective(t, &Remote{SSHBin: sshBin})
	if m := got["controlmaster"]; m != "false" && m != "no" {
		t.Errorf("without a ControlDir: controlmaster = %q, want no", m)
	}
	if p, ok := got["controlpath"]; ok && p != "none" {
		t.Errorf("without a ControlDir: controlpath = %q, want none (the person's config must not apply)", p)
	}

	r := &Remote{SSHBin: sshBin, ControlDir: filepath.Join(dir, "mux")}
	got = effective(t, r)
	if want := controlPath(r.ControlDir, target.muxKey(keyOf(t, key))); got["controlpath"] != want {
		t.Errorf("with a ControlDir: controlpath = %q, want the provider's own %q", got["controlpath"], want)
	}
	if got["identitiesonly"] != "yes" {
		t.Errorf("identitiesonly = %q, want yes", got["identitiesonly"])
	}
}

// TestArgvRefusesAnIdentityFileItCannotUse: Argv is the gate every exec goes
// through, so an identity file whose value breaks the rule, or whose file is
// gone, is an error there, named for its entry, and no ssh is spawned: the ssh
// here is a fake that leaves a mark when it runs.
func TestArgvRefusesAnIdentityFileItCannotUse(t *testing.T) {
	dir := t.TempDir()
	mark := filepath.Join(dir, "ssh-ran")
	fake := filepath.Join(dir, "ssh")
	if err := os.WriteFile(fake, []byte("#!/bin/sh\n: > '"+mark+"'\nexit 255\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	r := &Remote{SSHBin: fake}

	for _, tc := range []struct{ name, identity, reason string }{
		{"a removed key", filepath.Join(dir, "removed"), "does not exist"},
		{"a percent token", "/home/pat/.ssh/id_%h", "%"},
		{"a relative path", "id_work", "not an absolute path"},
		{"a line feed", "/home/pat/.ssh/id\nwork", "line feed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			target := ShedTarget(RunningShed{Name: "dev", Server: "my-server", ServerHost: "localhost",
				ServerSSHPort: 2222, IdentityFile: tc.identity}, "/home/me/.shed/known_hosts")
			if argv, err := r.Argv(target, "cmd"); err == nil || argv != nil {
				t.Fatalf("Argv = %q, %v; want a refusal and no argv", argv, err)
			}
			_, err := r.run(context.Background(), target, "cmd", nil, 1024)
			var ie *config.IdentityFileError
			if !errors.As(err, &ie) || ie.Entry != "my-server" || !strings.Contains(ie.Reason, tc.reason) {
				t.Fatalf("run err = %v, want my-server's identity file refused for %q", err, tc.reason)
			}
			if _, err := os.Stat(mark); err == nil {
				t.Fatal("ssh was spawned for an identity file that cannot be used")
			}
		})
	}

	// The negative control: the same fake does run for a target with no
	// identity file, so the mark above would have been seen.
	target := ShedTarget(RunningShed{Name: "dev", Server: "my-server", ServerHost: "localhost", ServerSSHPort: 2222},
		"/home/me/.shed/known_hosts")
	_, _ = r.run(context.Background(), target, "cmd", nil, 1024)
	if _, err := os.Stat(mark); err != nil {
		t.Fatalf("the fake ssh did not run (%v): the test proves nothing", err)
	}
}

// TestInventoryCarriesTheIdentityFile: a running shed found on an entry with
// an identity file carries it, so the provider's ssh to that shed offers it.
func TestInventoryCarriesTheIdentityFile(t *testing.T) {
	handler, _ := shedsHandler(t, config.Shed{Name: "a", Status: config.StatusRunning})
	ts := httptest.NewServer(handler)
	defer ts.Close()
	entry := openEntry(ts)
	entry.IdentityFile = "/home/pat/.ssh/id_work"

	got := Inventory(context.Background(), map[string]config.ServerEntry{"srv": entry}, 0).Sheds
	if len(got) != 1 || got[0].IdentityFile != "/home/pat/.ssh/id_work" {
		t.Fatalf("sheds = %#v, want one carrying the entry's identity file", got)
	}
	if target := ShedTarget(got[0], "/home/pat/.shed/known_hosts"); target.IdentityFile != entry.IdentityFile || target.Entry != "srv" {
		t.Errorf("target = %+v", target)
	}
}

// TestInventoryHoldsTheStoredCertificateToTheIdentityFile (plan 037 P-a, at
// every load): the provider reads an entry's stored certificate from disk at
// each inventory. For an entry with an identity_file, a certificate made for
// another key (another process stored it, or the identity_file changed since)
// is never presented: no TCP connection is made, and the server is skipped
// like any entry this package cannot use (it is no refusal: the CLI re-enrols
// such an entry). One whose identity file is gone presents nothing either,
// and is the entry's refusal (its *config.IdentityFileError, named for the
// entry), as it is to the CLI. A certificate made for the identity file's key
// is presented as before.
func TestInventoryHoldsTheStoredCertificateToTheIdentityFile(t *testing.T) {
	dir := t.TempDir()
	k1, k2 := filepath.Join(dir, "id_old"), filepath.Join(dir, "id_work")
	writeTestKey(t, k1)
	writeTestKey(t, k2)
	fp := func(path string) string { return keyOf(t, path) }
	pat := "pat"
	for _, tc := range []struct {
		name, storedFor, identity string
		wantShown, wantRefused    bool
	}{
		{"made for the identity file's key", fp(k2), k2, true, false},
		{"made for another key", fp(k1), k2, false, false},
		{"the identity file is gone", fp(k2), filepath.Join(dir, "removed"), false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			handler, hits := shedsHandler(t, config.Shed{Name: "a", Status: config.StatusRunning})
			ts := httptest.NewUnstartedServer(http.HandlerFunc(handler))
			ts.TLS = &tls.Config{ClientAuth: tls.RequireAnyClientCert}
			counter := &countingListener{Listener: ts.Listener}
			ts.Listener = counter
			ts.StartTLS()
			defer ts.Close()

			certPEM, keyPEM := generateClientCert(t, tc.storedFor, &pat)
			certDir := t.TempDir()
			certPath, keyPath := filepath.Join(certDir, "client.crt"), filepath.Join(certDir, "client.key")
			if err := os.WriteFile(certPath, certPEM, 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(keyPath, keyPEM, 0o600); err != nil {
				t.Fatal(err)
			}
			entry := config.ServerEntry{
				Host: "127.0.0.1", APIURL: ts.URL, SSHPort: 2222, AuthMode: config.AuthModeMTLS,
				TLSCertFingerprint: servertls.Fingerprint(ts.Certificate().Raw),
				ClientCertFile:     certPath, ClientKeyFile: keyPath, User: pat, IdentityFile: tc.identity,
			}

			out := Inventory(context.Background(), map[string]config.ServerEntry{"srv": entry}, 0)
			if tc.wantShown {
				if len(out.Sheds) != 1 || atomic.LoadInt32(hits) != 1 {
					t.Fatalf("the certificate for the entry's key was not presented: %#v hits=%d", out, atomic.LoadInt32(hits))
				}
				return
			}
			if len(out.Answered) != 0 {
				t.Fatalf("the entry was used: %#v", out)
			}
			var ie *config.IdentityFileError
			gotRefused := errors.As(out.Refused["srv"], &ie)
			if gotRefused != tc.wantRefused || len(out.Refused) != map[bool]int{true: 1}[tc.wantRefused] {
				t.Fatalf("refused = %#v, want an identity-file refusal: %v", out.Refused, tc.wantRefused)
			}
			if gotRefused && ie.Entry != "srv" {
				t.Errorf("the refusal names entry %q, want srv", ie.Entry)
			}
			if n := counter.accepted.Load(); n != 0 {
				t.Fatalf("the entry made %d TCP connection(s) to its server, want 0", n)
			}
		})
	}
}

// TestInventoryNeverPresentsAStoredTokenForAnIdentityEntry (plan 037 P-a): a
// control token names no SSH key, so an entry with an identity_file that
// holds one (static or refreshable, written before the identity_file was set)
// presents nothing: the token never reaches the server (a recording server
// sees no request), and the entry is refused with the message naming the
// refetch. Without an identity_file the same entry presents its token as
// before (the negative control: the recording server sees it).
func TestInventoryNeverPresentsAStoredTokenForAnIdentityEntry(t *testing.T) {
	key := filepath.Join(t.TempDir(), "id_work")
	writeTestKey(t, key)
	for _, tc := range []struct {
		name    string
		expires time.Time
	}{
		{"a static token", time.Time{}},
		{"a refreshable token", time.Now().Add(time.Hour)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var seen atomic.Int32
			var gotToken atomic.Value
			ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				seen.Add(1)
				gotToken.Store(r.Header.Get("Authorization"))
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"sheds":[{"name":"a","status":"running"}]}`))
			}))
			defer ts.Close()
			entry := openEntry(ts)
			entry.ControlToken = "stored-token-T"
			entry.ControlTokenExpiresAt = tc.expires
			entry.IdentityFile = key

			out := Inventory(context.Background(), map[string]config.ServerEntry{"srv": entry}, 0)
			r, refused := config.AsEntryRefused(out.Refused["srv"])
			if !refused || r.Kind != config.RefusedStoredTokenForKey || r.Entry != "srv" {
				t.Fatalf("refused = %#v, want srv refused for its stored token", out.Refused)
			}
			if !strings.Contains(r.Error(), "shed server update srv --refetch") {
				t.Errorf("message %q does not name the refetch", r.Error())
			}
			if n := seen.Load(); n != 0 || len(out.Sheds) != 0 {
				t.Fatalf("the server saw %d request(s) with the token; want none", n)
			}

			entry.IdentityFile = ""
			out = Inventory(context.Background(), map[string]config.ServerEntry{"srv": entry}, 0)
			if seen.Load() != 1 || gotToken.Load() != "Bearer stored-token-T" || len(out.Sheds) != 1 {
				t.Fatalf("without an identity file the token was not presented (seen %d, %v): the test proves nothing",
					seen.Load(), gotToken.Load())
			}
		})
	}
}

// TestInventoryReportsAnUnusableKeyWithNoStoredCredential (the third review's
// P3): an entry with nothing stored to present (an mtls entry with no
// certificate files, a secure entry with no token) whose identity file cannot
// be used is the entry's identity-file refusal, named for the entry, not a
// silent skip; nothing reaches its server. The negative control is the same
// entry with a usable key, which is skipped with no refusal (it needs an
// enrolment this package never makes).
func TestInventoryReportsAnUnusableKeyWithNoStoredCredential(t *testing.T) {
	usable := filepath.Join(t.TempDir(), "id_work")
	writeTestKey(t, usable)
	for _, tc := range []struct {
		name string
		mode string
	}{
		{"an mtls entry with no certificate files", config.AuthModeMTLS},
		{"a secure entry with no token", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var seen atomic.Int32
			ts := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { seen.Add(1) }))
			defer ts.Close()
			entry := config.ServerEntry{Host: "127.0.0.1", APIURL: "https://127.0.0.1:1", SSHPort: 2222,
				AuthMode: tc.mode, IdentityFile: filepath.Join(t.TempDir(), "removed")}
			if tc.mode == "" {
				entry.APIURL = ts.URL
				entry.TLSCertFingerprint = "sha256:00" // a secure entry: it needs a credential
			}

			out := Inventory(context.Background(), map[string]config.ServerEntry{"srv": entry}, 0)
			var ie *config.IdentityFileError
			if !errors.As(out.Refused["srv"], &ie) || ie.Entry != "srv" || !strings.Contains(ie.Reason, "does not exist") {
				t.Fatalf("refused = %#v, want srv's identity-file refusal", out.Refused)
			}
			if seen.Load() != 0 || len(out.Answered) != 0 {
				t.Fatalf("the entry was used (%d request(s)): %#v", seen.Load(), out)
			}

			entry.IdentityFile = usable
			out = Inventory(context.Background(), map[string]config.ServerEntry{"srv": entry}, 0)
			if len(out.Refused) != 0 || len(out.Answered) != 0 || seen.Load() != 0 {
				t.Fatalf("with a usable key = %#v (%d request(s)), want a silent skip", out, seen.Load())
			}
		})
	}
}

// TestMuxKeyIncludesTheIdentityFile: two targets that differ only in their
// identity file (two server entries for one server, each with its own key)
// never share a ControlMaster, or the second would ride a connection
// authenticated with the first's key. A target with no identity file keeps the
// v1 key exactly, so its ControlPath is what it was before the field existed.
func TestMuxKeyIncludesTheIdentityFile(t *testing.T) {
	dir := t.TempDir()
	keyA, keyB := filepath.Join(dir, "a"), filepath.Join(dir, "b")
	for _, k := range []string{keyA, keyB} {
		writeTestKey(t, k)
	}
	shed := RunningShed{Name: "dev", Server: "srv", ServerHost: "shed.example.test", ServerSSHPort: 2222}
	plain := ShedTarget(shed, "/home/pat/.shed/known_hosts")
	shed.IdentityFile = keyA
	withA := ShedTarget(shed, "/home/pat/.shed/known_hosts")
	shed.IdentityFile = keyB
	withB := ShedTarget(shed, "/home/pat/.shed/known_hosts")

	fpA, fpB := keyOf(t, keyA), keyOf(t, keyB)
	if got, want := plain.muxKey(""), "v1\x00dev@shed.example.test\x002222\x00true\x00/home/pat/.shed/known_hosts"; got != want {
		t.Errorf("muxKey without an identity file = %q, want the v1 key %q", got, want)
	}
	if got := withA.muxKey(fpA); !strings.HasPrefix(got, "v2\x00") || !strings.HasSuffix(got, "\x00"+keyA+"\x00"+fpA) {
		t.Errorf("muxKey with an identity file = %q, want the v2 key ending in the file and its key", got)
	}
	if withA.muxKey(fpA) == withB.muxKey(fpB) {
		t.Error("two identity files share a mux key")
	}
	if withA.muxKey(fpA) == plain.muxKey("") {
		t.Error("a target with an identity file shares a mux key with one without")
	}

	r := &Remote{SSHBin: "ssh", ControlDir: "/run/user/1000/shed-roost-provider"}
	pathOf := func(target Target) string {
		t.Helper()
		for _, arg := range mustArgv(t, r, target, "cmd") {
			if path, ok := strings.CutPrefix(arg, "ControlPath="); ok {
				return path
			}
		}
		t.Fatalf("no ControlPath in argv for %+v", target)
		return ""
	}
	if pathOf(withA) == pathOf(withB) {
		t.Error("two entries with different keys share a ControlPath")
	}
	if pathOf(withA) == pathOf(plain) {
		t.Error("an entry with a key and one without share a ControlPath")
	}
}

// TestMuxKeyFollowsTheKeyNotTheFileName: the shared connection is keyed by the
// key, not by the file's name. A key replaced at the same path, or a symlink
// at that path retargeted to another key, gives another ControlPath, so ssh
// never reuses a master the old key authenticated for the new one.
func TestMuxKeyFollowsTheKeyNotTheFileName(t *testing.T) {
	dir := t.TempDir()
	r := &Remote{SSHBin: "ssh", ControlDir: "/run/user/1000/shed-roost-provider"}
	pathOf := func(target Target) string {
		t.Helper()
		for _, arg := range mustArgv(t, r, target, "cmd") {
			if path, ok := strings.CutPrefix(arg, "ControlPath="); ok {
				return path
			}
		}
		t.Fatalf("no ControlPath in argv for %+v", target)
		return ""
	}

	t.Run("a key replaced at the same path", func(t *testing.T) {
		key := filepath.Join(dir, "id_work")
		writeTestKey(t, key)
		target := ShedTarget(RunningShed{Name: "dev", Server: "srv", ServerHost: "shed.example.test",
			ServerSSHPort: 2222, IdentityFile: key}, "/home/pat/.shed/known_hosts")
		withK1 := pathOf(target)
		if again := pathOf(target); again != withK1 {
			t.Fatalf("the same key gave two ControlPaths: %q, %q", withK1, again)
		}
		writeTestKey(t, key) // K2, at the same path
		if withK2 := pathOf(target); withK2 == withK1 {
			t.Errorf("K1 and K2 at the same path share ControlPath %q", withK1)
		}
	})

	t.Run("a symlink retargeted", func(t *testing.T) {
		k1, k2 := filepath.Join(dir, "k1"), filepath.Join(dir, "k2")
		writeTestKey(t, k1)
		writeTestKey(t, k2)
		link := filepath.Join(dir, "id_link")
		if err := os.Symlink(k1, link); err != nil {
			t.Fatal(err)
		}
		target := ShedTarget(RunningShed{Name: "dev", Server: "srv", ServerHost: "shed.example.test",
			ServerSSHPort: 2222, IdentityFile: link}, "/home/pat/.shed/known_hosts")
		withK1 := pathOf(target)
		if err := os.Remove(link); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(k2, link); err != nil {
			t.Fatal(err)
		}
		if withK2 := pathOf(target); withK2 == withK1 {
			t.Errorf("a symlink retargeted from K1 to K2 keeps ControlPath %q", withK1)
		}
	})
}
