package main

// Tests for a server entry's identity_file (plan 037 §3.3, A7): the field, the
// --identity-file flag on `server add` and `server update`, the one rule its
// value is held to wherever it comes from, the ssh options it adds to every
// ssh the CLI runs for the entry, and the three properties the docs state:
// the entry's API identity is that key (P-a), its ssh never rides a shared
// connection another key opened (P-b), and ssh offers that key first and no
// agent-only key (P-c).

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	gossh "golang.org/x/crypto/ssh"

	"github.com/charliek/shed/internal/config"
	"github.com/charliek/shed/internal/servertls"
	"github.com/charliek/shed/internal/sshconfig"
	"github.com/charliek/shed/internal/tunnels"
	"github.com/charliek/shed/sdk"
	sdkbootstrap "github.com/charliek/shed/sdk/bootstrap"
)

// identityArgs is what an identity file adds to an ssh argv.
func identityArgs(key string) []string {
	return []string{"-i", key, "-o", "IdentitiesOnly=yes", "-o", "ControlMaster=no", "-o", "ControlPath=none"}
}

// mustBaseSSHArgs is baseSSHArgs for an entry whose identity file (if any) can
// be used, named "srv".
func mustBaseSSHArgs(t *testing.T, shed string, entry *config.ServerEntry, extra ...string) []string {
	t.Helper()
	args, err := baseSSHArgs("srv", shed, entry, extra...)
	if err != nil {
		t.Fatalf("baseSSHArgs: %v", err)
	}
	return args
}

// mustSSHSessionArgs is sshSessionArgs for an entry named "srv".
func mustSSHSessionArgs(t *testing.T, flag, shed string, entry *config.ServerEntry, quoted []string) []string {
	t.Helper()
	args, err := sshSessionArgs(flag, "srv", shed, entry, quoted)
	if err != nil {
		t.Fatalf("sshSessionArgs: %v", err)
	}
	return args
}

// writeKey writes a new unencrypted OpenSSH-format ed25519 private key under
// dir and returns its path.
func writeKey(t *testing.T, dir, name string) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, name)
	writeKeyAt(t, path, nil)
	return path
}

// writeKeyAt writes a new ed25519 private key at path in the OpenSSH format,
// encrypted with passphrase when it is not nil.
func writeKeyAt(t *testing.T, path string, passphrase []byte) {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	var block *pem.Block
	if passphrase == nil {
		block, err = gossh.MarshalPrivateKey(priv, "")
	} else {
		block, err = gossh.MarshalPrivateKeyWithPassphrase(priv, "", passphrase)
	}
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, pem.EncodeToMemory(block), 0o600); err != nil {
		t.Fatal(err)
	}
}

// keyFingerprint is the SHA-256 fingerprint of the identity file at path: the
// subject a server writes on a certificate issued to that key.
func keyFingerprint(t *testing.T, path string) string {
	t.Helper()
	fp, err := config.IdentityFileKey("", path)
	if err != nil {
		t.Fatalf("the test key %s: %v", path, err)
	}
	return fp
}

// credentialForKey issues a credential for user whose certificate was made
// for the SSH key fingerprint subject, pointed at m's HTTPS port when m has a
// listener.
func credentialForKey(t *testing.T, m *mtlsServer, subject, user string) sdk.Credential {
	t.Helper()
	certPEM, keyPEM, serial := m.issueForSubject(t, subject, user, farFromExpiry)
	cred := bundleOf(m.pin, certPEM, keyPEM, serial, farFromExpiry)
	if m.srv != nil {
		_, port := hostPort(t, m.srv.URL)
		cred.Bundle.HTTPSPort = port
	}
	return cred
}

// asIdentityRefusal asserts err is an identity_file refusal for entry, with
// reason in its words.
func asIdentityRefusal(t *testing.T, err error, entry, reason string) {
	t.Helper()
	var ie *config.IdentityFileError
	if !errors.As(err, &ie) {
		t.Fatalf("err = %v, want an identity_file refusal", err)
	}
	if ie.Entry != entry || !strings.Contains(ie.Reason, reason) {
		t.Fatalf("err = %v, want entry %q refused for %q", err, entry, reason)
	}
}

// stubBootstrapIdentity installs a bootstrap that records the identity file
// it was handed and returns cred (or err).
func stubBootstrapIdentity(cred sdk.Credential, err error) (calls *atomic.Int32, identity *atomic.Value) {
	calls, identity = &atomic.Int32{}, &atomic.Value{}
	identity.Store("<none>")
	bootstrapCredentialFn = func(_ string, _ int, identityFile, _, _ string) (sdk.Credential, error) {
		calls.Add(1)
		identity.Store(identityFile)
		return cred, err
	}
	return calls, identity
}

// ---------------------------------------------------------------------------
// the argv: console, exec, attach
// ---------------------------------------------------------------------------

// TestShedSSHArgvIdentityFile: console, exec and the plain attach share
// baseSSHArgs. Without an identity file the argv is exactly what it was before
// the field existed; with one it gains `-i <path> -o IdentitiesOnly=yes -o
// ControlMaster=no -o ControlPath=none` before any extra options and the
// destination.
func TestShedSSHArgvIdentityFile(t *testing.T) {
	t.Setenv("HOME", "/home/pat")
	entry := &config.ServerEntry{Host: "shed.example.test", SSHPort: 2222}
	options := []string{"-p", "2222", "-o", "UserKnownHostsFile=/home/pat/.shed/known_hosts", "-o", "StrictHostKeyChecking=yes"}

	if got, want := mustBaseSSHArgs(t, "box", entry), slices.Concat(options, []string{"box@shed.example.test"}); !slices.Equal(got, want) {
		t.Errorf("argv without an identity file = %q, want %q", got, want)
	}

	key := writeKey(t, t.TempDir(), "id work")
	entry.IdentityFile = key
	identity := identityArgs(key)
	want := slices.Concat(options, identity, []string{"-T", "box@shed.example.test"})
	if got := mustBaseSSHArgs(t, "box", entry, "-T"); !slices.Equal(got, want) {
		t.Errorf("argv with an identity file = %q, want %q", got, want)
	}
	session := mustSSHSessionArgs(t, "-T", "box", entry, []string{"'true'"})
	if want := slices.Concat([]string{"ssh", "-T"}, options, identity, []string{"box@shed.example.test", "'true'"}); !slices.Equal(session, want) {
		t.Errorf("exec argv = %q, want %q", session, want)
	}

	// The plain attach, through the process-replacement seam.
	origExec := execSSH
	t.Cleanup(func() { execSSH = origExec })
	var captured []string
	execSSH = func(_ string, argv []string, _ []string) error { captured = argv; return nil }
	if err := attachPlain("box", "srv", entry, &config.Shed{LandingDir: "/home/shed"}); err != nil {
		t.Fatalf("attachPlain: %v", err)
	}
	if !slices.Equal(captured[2:2+len(options)+len(identity)], slices.Concat(options, identity)) {
		t.Errorf("attach argv = %q, want the identity args after the shared options", captured)
	}
}

// sshEffective runs `ssh -G -F cfg <args>`, which prints the options in force
// for those arguments and connects nowhere, and returns them by name. It skips
// the test where no ssh is installed.
func sshEffective(t *testing.T, cfg string, args []string) map[string][]string {
	t.Helper()
	sshBin, err := exec.LookPath("ssh")
	if err != nil {
		t.Skip("no ssh on PATH")
	}
	out, err := exec.Command(sshBin, append([]string{"-G", "-F", cfg}, args...)...).Output()
	if err != nil {
		t.Fatalf("ssh -G %q: %v", args, err)
	}
	got := map[string][]string{}
	for _, line := range strings.Split(string(out), "\n") {
		if k, v, ok := strings.Cut(line, " "); ok {
			got[k] = append(got[k], v)
		}
	}
	return got
}

// TestEntrySSHNeverRidesASharedConnection (P-b): against a person's ssh config
// that sets `ControlMaster auto` and a ControlPath for every host, the
// options in force for the CLI's ssh to an entry with an identity file (the
// console, exec and plain attach argv, and the `_bootstrap` enrolment) use no
// shared connection at all, so none can ride a master another key opened;
// and the entry's key is offered first, with IdentitiesOnly. Without an
// identity file nothing changes (the config's ControlPath applies, as
// before). Checked with `ssh -G`; skipped where ssh is not installed.
func TestEntrySSHNeverRidesASharedConnection(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	key := writeKey(t, dir, "id_work")
	cfg := filepath.Join(dir, "ssh_config")
	if err := os.WriteFile(cfg, []byte("Host *\n    ControlMaster auto\n    ControlPath "+dir+"/theirs-%C\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	keyed := &config.ServerEntry{Host: "shed.example.test", SSHPort: 2222, IdentityFile: key}
	bootstrapArgv, err := sdkbootstrap.Args(bootstrapParams(keyed.Host, keyed.SSHPort, key, "control", "cli"))
	if err != nil {
		t.Fatal(err)
	}
	for name, args := range map[string][]string{
		"console/exec/attach": mustBaseSSHArgs(t, "box", keyed),
		"bootstrap":           bootstrapArgv,
	} {
		got := sshEffective(t, cfg, args)
		if m := got["controlmaster"]; len(m) != 1 || (m[0] != "false" && m[0] != "no") {
			t.Errorf("%s: controlmaster = %q, want no", name, m)
		}
		if p := got["controlpath"]; len(p) != 0 && !slices.Equal(p, []string{"none"}) {
			t.Errorf("%s: controlpath = %q, want none: the person's ControlPath must not apply", name, p)
		}
		if got["identitiesonly"][0] != "yes" || len(got["identityfile"]) == 0 || got["identityfile"][0] != key {
			t.Errorf("%s: identitiesonly = %q, identityfile = %q; want yes and the entry's key first", name,
				got["identitiesonly"], got["identityfile"])
		}
	}
	// The negative control: without an identity file the person's config
	// decides, so the check above would see their ControlPath.
	got := sshEffective(t, cfg, mustBaseSSHArgs(t, "box", &config.ServerEntry{Host: "shed.example.test", SSHPort: 2222}))
	if p := got["controlpath"]; len(p) != 1 || !strings.HasPrefix(p[0], dir+"/theirs-") {
		t.Errorf("without an identity file: controlpath = %q, want the config's own", p)
	}
}

// handEditedRefusals is each refused identity_file value, as a person would
// write it in config.yaml, with the reason the refusal must give.
func handEditedRefusals(t *testing.T) []struct{ name, yaml, reason string } {
	t.Helper()
	dir := t.TempDir()
	key := writeKey(t, dir, "id_work")
	noKey := filepath.Join(dir, "not_a_key")
	if err := os.WriteFile(noKey, []byte("not a key"), 0o600); err != nil {
		t.Fatal(err)
	}
	return []struct{ name, yaml, reason string }{
		{"CR", `"` + key + `\r"`, "carriage return"},
		{"LF", `"` + key + `\nx"`, "line feed"},
		{"NUL", `"` + key + `\0"`, "NUL"},
		{"double quote", `'` + key + `"'`, "double quote"},
		{"backslash", `'` + key + `\x'`, "backslash"},
		{"hash", `"` + key + `#x"`, "#"},
		{"percent", `"` + key + `%h"`, "%"},
		{"dollar", `"` + key + `$X"`, "$"},
		{"leading space", `" ` + key + `"`, "begins with whitespace"},
		{"trailing space", `"` + key + ` "`, "ends with whitespace"},
		{"only whitespace", `"  "`, "only whitespace"},
		{"relative", `id_work`, "not an absolute path"},
		{"a directory", `"` + dir + `"`, "directory"},
		{"missing", `"` + filepath.Join(dir, "gone") + `"`, "does not exist"},
		{"no public key to learn", `"` + noKey + `"`, "public key cannot be learned"},
	}
}

// loadHandEdited writes config.yaml by hand with one entry "srv" whose
// identity_file is the YAML value given, and makes it this process's config.
func loadHandEdited(t *testing.T, value string) config.ServerEntry {
	t.Helper()
	cfgPath := testClientConfig(t)
	doc := "default_server: srv\nservers:\n  srv:\n    host: 127.0.0.1\n    ssh_port: 2222\n" +
		"    api_url: https://127.0.0.1:18443\n    tls_cert_fingerprint: sha256:00\n    auth_mode: mtls\n" +
		"    identity_file: " + value + "\n"
	if err := os.MkdirAll(filepath.Dir(cfgPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cfgPath, []byte(doc), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.LoadClientConfigFromPath(cfgPath)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	clientConfig = cfg
	return cfg.Servers["srv"]
}

// TestHandEditedIdentityFileIsRefusedBeforeSSH: a value in config.yaml that
// breaks the rule (or names no usable file, or a key whose public half cannot
// be learned) fails every path that would run ssh for the entry, named for the
// entry, and none of them runs ssh: the bootstrap seam is never called, the
// attach never execs, console and exec get no argv, and the generated ssh
// config is not produced.
func TestHandEditedIdentityFileIsRefusedBeforeSSH(t *testing.T) {
	for _, tc := range handEditedRefusals(t) {
		t.Run(tc.name, func(t *testing.T) {
			entry := loadHandEdited(t, tc.yaml)

			// The bootstrap (an mtls entry with no certificate enrols first).
			calls, _ := stubBootstrapIdentity(sdk.Credential{}, errors.New("test: unreachable"))
			_, err := controlCredentialRefresh(entry, "srv", "srv")()
			asIdentityRefusal(t, err, "srv", tc.reason)
			if calls.Load() != 0 {
				t.Errorf("the bootstrap seam ran %d time(s)", calls.Load())
			}

			// console and exec.
			if _, err := sshSessionArgs("-t", "srv", "box", &entry, nil); err == nil {
				t.Error("console built an argv")
			} else {
				asIdentityRefusal(t, err, "srv", tc.reason)
			}

			// The plain attach never reaches exec.
			origExec := execSSH
			t.Cleanup(func() { execSSH = origExec })
			execSSH = func(string, []string, []string) error {
				t.Error("attach exec'd ssh")
				return nil
			}
			err = attachPlain("box", "srv", &entry, &config.Shed{})
			asIdentityRefusal(t, err, "srv", tc.reason)

			// The generated ssh config.
			_, _, err = generateEntries([]shedInfo{{name: "box", serverName: "srv", server: &entry}})
			asIdentityRefusal(t, err, "srv", tc.reason)
		})
	}
}

// TestRefreshUsesTheEntryIdentityFile: every enrolment of an entry — the
// refresh a request runs, and the tunnel daemon's mint-only one — hands the
// entry's identity file to the bootstrap; a key removed after it was set
// fails with the identity file's own message, and ssh is never run.
func TestRefreshUsesTheEntryIdentityFile(t *testing.T) {
	testClientConfig(t)
	m := newMTLSCredentialIssuer(t)
	key := writeKey(t, t.TempDir(), "id work")
	entry := config.ServerEntry{Host: "127.0.0.1", SSHPort: 2222, APIURL: "https://localhost:18443",
		TLSCertFingerprint: m.pin, IdentityFile: key}
	putServerEntry(t, "srv", entry)

	calls, identity := stubBootstrapIdentity(credentialForKey(t, m, keyFingerprint(t, key), testUser), nil)
	if _, err := controlCredentialRefresh(entry, "srv", "srv")(); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	if got := identity.Load(); got != key {
		t.Errorf("the refresh's bootstrap got identity %q, want %q", got, key)
	}
	identity.Store("<none>")
	if _, err := controlCredentialMintOnly(clientConfig.Servers["srv"], "srv")(); err != nil {
		t.Fatalf("mint-only refresh: %v", err)
	}
	if got := identity.Load(); got != key {
		t.Errorf("the tunnel daemon's bootstrap got identity %q, want %q", got, key)
	}

	if err := os.Remove(key); err != nil {
		t.Fatal(err)
	}
	before := calls.Load()
	_, err := controlCredentialRefresh(entry, "srv", "srv")()
	asIdentityRefusal(t, err, "srv", "does not exist")
	if calls.Load() != before {
		t.Error("the bootstrap ran for a key that was removed")
	}
}

// ---------------------------------------------------------------------------
// P-a: the entry's API identity is its key
// ---------------------------------------------------------------------------

// assertOtherKeyRefusal checks a refusal of a certificate made for another
// key: its kind, unwrapped, and the words that say what happened and why.
func assertOtherKeyRefusal(t *testing.T, err error, entry, key, otherFP string) {
	t.Helper()
	r := asRefusal(t, err, config.RefusedIssuedOtherKey)
	if r.Entry != entry || r.IdentityFile != key || r.IssuedKey != otherFP {
		t.Errorf("refusal = %+v, want entry %q, key %q, issued %q", r, entry, key, otherFP)
	}
	for _, want := range []string{fmt.Sprintf("entry %q", entry), "ssh authenticated with a different key than the entry's identity file",
		"IdentityFile your ssh config names for this host", "nothing was saved"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("message lacks %q:\n%s", want, err)
		}
	}
}

// TestIssuedForAnotherKeyIsRefusedOnEveryPath (P-a): with an identity file,
// the server's certificate is accepted only when it was made for that key.
// When ssh authenticated with another key (one the person's ssh config names
// for the host), the certificate the server issues names that key, and each
// of the four ways the CLI enrols — `server add`, `server update --refetch`,
// the refresh a request runs, and a tunnel daemon's mint — refuses it with
// nothing written: no certificate, no key file, no config change, and, for
// the add, no entry at all.
func TestIssuedForAnotherKeyIsRefusedOnEveryPath(t *testing.T) {
	otherFP := sshFingerprint("a key from ~/.ssh/config")

	t.Run("server add", func(t *testing.T) {
		cfgPath := testClientConfig(t)
		withCleanAddFlags(t)
		serverAddTrustTOFU = true
		key := writeKey(t, t.TempDir(), "id_work")
		serverAddIdentityFile = key
		m := newMTLSServer(t)
		host, _ := hostPort(t, m.srv.URL)
		hostKey, _ := testHostKey(t)
		stubHostKeyScan(t, hostKey, nil)
		stubBootstrapIdentity(credentialForKey(t, m, otherFP, testUser), nil)
		freezeCreds(t, "srv")

		assertOtherKeyRefusal(t, runServerAdd(nil, []string{host}), "", key, otherFP)
		if _, err := os.Stat(cfgPath); !os.IsNotExist(err) {
			t.Errorf("config.yaml was written (err=%v)", err)
		}
		if certPath, _ := config.ClientCredentialPaths("srv"); fileExists(certPath) {
			t.Error("a certificate was stored")
		}
		if got := m.requests.Load(); got != 0 {
			t.Errorf("the server got %d request(s), want 0", got)
		}
	})

	t.Run("server update --refetch", func(t *testing.T) {
		testClientConfig(t)
		withCleanAddFlags(t)
		serverAddTrustTOFU = true
		m := newMTLSServer(t)
		cfgPath, _, _ := addedMTLSServer(t, m)
		serverUpdateRefetch, serverUpdateTrustTOFU = true, true
		key := writeKey(t, t.TempDir(), "id_work")
		serverUpdateIdentityFile = optionalString{value: key, set: true}
		certPath, keyPath := config.ClientCredentialPaths("srv")
		before := snapshotFiles(t, cfgPath, certPath, keyPath)
		stubBootstrapIdentity(credentialForKey(t, m, otherFP, testUser), nil)
		freezeCreds(t, "srv")

		assertOtherKeyRefusal(t, runServerUpdate(nil, []string{"srv"}), "srv", key, otherFP)
		before.assertUnchanged(t)
	})

	// The refresh and the tunnel daemon's mint: an entry holding an expired
	// certificate for its key re-enrols, and is issued one for another key.
	refreshSetup := func(t *testing.T) (*mtlsServer, string, config.ServerEntry, fileSnapshot) {
		t.Helper()
		cfgPath := testClientConfig(t)
		m := newMTLSServer(t)
		key := writeKey(t, t.TempDir(), "id_work")
		certPEM, keyPEM, serial := m.issueForSubject(t, keyFingerprint(t, key), testUser, alreadyExpired)
		entry := expiredEntry(t, m, "srv", bundleOf(m.pin, certPEM, keyPEM, serial, alreadyExpired))
		entry.IdentityFile = key
		putServerEntry(t, "srv", entry)
		certPath, keyPath := config.ClientCredentialPaths("srv")
		before := snapshotFiles(t, cfgPath, certPath, keyPath)
		freezeCreds(t, "srv")
		stubBootstrapIdentity(credentialForKey(t, m, otherFP, testUser), nil)
		return m, key, entry, before
	}

	t.Run("the refresh a request runs", func(t *testing.T) {
		m, key, entry, before := refreshSetup(t)
		_, err := NewAPIClientFromNamedEntry("srv", &entry, DefaultTimeout).GetInfo()
		assertOtherKeyRefusal(t, err, "srv", key, otherFP)
		before.assertUnchanged(t)
		if got := m.accepted.Load(); got != 0 {
			t.Errorf("the server accepted %d connection(s), want 0", got)
		}
	})

	t.Run("a tunnel daemon's mint", func(t *testing.T) {
		_, key, entry, before := refreshSetup(t)
		_, err := controlCredentialMintOnly(entry, "srv")()
		assertOtherKeyRefusal(t, err, "srv", key, otherFP)
		before.assertUnchanged(t)
	})
}

// fileExists reports whether path exists.
func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// TestIdentityEntryRefusesAControlToken (P-a): a control token (only a
// shed-server older than 0.10.0 issues one) names no key, so an entry with an
// identity file never takes one, at the add or at a refresh; nothing is
// written.
func TestIdentityEntryRefusesAControlToken(t *testing.T) {
	token := sdk.Credential{Bundle: sdk.Bundle{AuthMode: "token", Token: "tok", HTTPSPort: 443, Scope: "control"}}

	cfgPath := testClientConfig(t)
	withCleanAddFlags(t)
	serverAddTrustTOFU, serverAddName = true, "srv"
	key := writeKey(t, t.TempDir(), "id_work")
	serverAddIdentityFile = key
	hostKey, _ := testHostKey(t)
	stubHostKeyScan(t, hostKey, nil)
	stubBootstrapIdentity(token, nil)
	asRefusal(t, runServerAdd(nil, []string{"shed.example.test"}), config.RefusedIssuedTokenForKey)
	if _, err := os.Stat(cfgPath); !os.IsNotExist(err) {
		t.Errorf("config.yaml was written (err=%v)", err)
	}

	entry := config.ServerEntry{Host: "shed.example.test", SSHPort: 2222, APIURL: "https://shed.example.test:8443", IdentityFile: key}
	putServerEntry(t, "srv", entry)
	before := snapshotFiles(t, cfgPath)
	_, err := controlCredentialRefresh(entry, "srv", "srv")()
	asRefusal(t, err, config.RefusedIssuedTokenForKey)
	before.assertUnchanged(t)
}

// ---------------------------------------------------------------------------
// P-a on a stored certificate: held wherever one is loaded for use
// ---------------------------------------------------------------------------

// storeCertForKey stores, as the entry "srv"'s credential files, a valid
// certificate for testUser made for the SSH key at storedKey, and records the
// entry (on disk and in memory) with identity as its identity_file.
func storeCertForKey(t *testing.T, m *mtlsServer, storedKey, identity string) config.ServerEntry {
	t.Helper()
	certPEM, keyPEM, serial := m.issueForSubject(t, keyFingerprint(t, storedKey), testUser, farFromExpiry)
	entry := mtlsEntry(t, m, "srv", bundleOf(m.pin, certPEM, keyPEM, serial, farFromExpiry))
	entry.IdentityFile = identity
	putServerEntry(t, "srv", entry)
	return entry
}

// storedSubject is the subject CN (the SSH key fingerprint) of the certificate
// stored for the entry "srv".
func storedSubject(t *testing.T) string {
	t.Helper()
	certPath, _ := config.ClientCredentialPaths("srv")
	data, err := os.ReadFile(certPath)
	if err != nil {
		t.Fatal(err)
	}
	block, _ := pem.Decode(data)
	if block == nil {
		t.Fatal("the stored certificate is not PEM")
	}
	leaf, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	return leaf.Subject.CommonName
}

// TestStoredCertificateForAnotherKeyIsNeverPresented (P-a, at load): the
// entry's identity_file is K2 (set by `server update --identity-file` without
// --refetch, or by hand) and its stored certificate, still valid, was made for
// K1. The next command never presents it: the server sees no certificate for
// K1, the entry enrols again with K2, and the certificate stored after is for
// K2.
func TestStoredCertificateForAnotherKeyIsNeverPresented(t *testing.T) {
	testClientConfig(t)
	m := newMTLSServer(t)
	dir := t.TempDir()
	k1, k2 := writeKey(t, dir, "id_old"), writeKey(t, dir, "id_work")
	fp1, fp2 := keyFingerprint(t, k1), keyFingerprint(t, k2)
	entry := storeCertForKey(t, m, k1, k2)
	calls, identity := stubBootstrapIdentity(credentialForKey(t, m, fp2, testUser), nil)

	if _, err := NewAPIClientFromNamedEntry("srv", &entry, DefaultTimeout).GetInfo(); err != nil {
		t.Fatalf("GetInfo: %v", err)
	}
	shown := m.shownCNs()
	if slices.Contains(shown, fp1) {
		t.Errorf("the certificate made for K1 was presented (shown %q)", shown)
	}
	if !slices.Contains(shown, fp2) {
		t.Errorf("no certificate for K2 was presented (shown %q): the check above proves nothing", shown)
	}
	if calls.Load() != 1 || identity.Load() != k2 {
		t.Errorf("bootstrap ran %d time(s) with identity %v, want once with K2", calls.Load(), identity.Load())
	}
	if got := storedSubject(t); got != fp2 {
		t.Errorf("the stored certificate is for %q after the command, want K2's %q", got, fp2)
	}
	if got := clientConfig.Servers["srv"].User; got != testUser {
		t.Errorf("entry user = %q, want %q", got, testUser)
	}
}

// TestStoredCertificateForAnotherKeyReenrolsUnderTheHeldUser: the same, but the
// enrolment with K2 comes back for another user. The held-user rule refuses
// it: nothing is written, and the K1 certificate is still never presented (no
// connection is made at all).
func TestStoredCertificateForAnotherKeyReenrolsUnderTheHeldUser(t *testing.T) {
	cfgPath := testClientConfig(t)
	m := newMTLSServer(t)
	dir := t.TempDir()
	k1, k2 := writeKey(t, dir, "id_old"), writeKey(t, dir, "id_work")
	entry := storeCertForKey(t, m, k1, k2)
	certPath, keyPath := config.ClientCredentialPaths("srv")
	before := snapshotFiles(t, cfgPath, certPath, keyPath)
	freezeCreds(t, "srv")
	stubBootstrapIdentity(credentialForKey(t, m, keyFingerprint(t, k2), "admin"), nil)

	_, err := NewAPIClientFromNamedEntry("srv", &entry, DefaultTimeout).GetInfo()
	r := asRefusal(t, err, config.RefusedIssuedUserChange)
	if r.CertUser != "admin" || r.EntryUser != testUser {
		t.Errorf("refusal names %q and %q, want admin and %s", r.CertUser, r.EntryUser, testUser)
	}
	before.assertUnchanged(t)
	if got := m.accepted.Load(); got != 0 {
		t.Errorf("the server accepted %d connection(s), want 0 (shown %q)", got, m.shownCNs())
	}
}

// TestIdentityFileGoneAtLoadIsRefused: the entry's stored certificate was made
// for its identity file, and the file is gone at the next use. The command
// fails with the identity file's typed refusal, before anything is presented
// or dialled, and ssh is never run.
func TestIdentityFileGoneAtLoadIsRefused(t *testing.T) {
	testClientConfig(t)
	m := newMTLSServer(t)
	key := writeKey(t, t.TempDir(), "id_work")
	entry := storeCertForKey(t, m, key, key)
	if err := os.Remove(key); err != nil {
		t.Fatal(err)
	}
	calls, _ := stubBootstrapIdentity(sdk.Credential{}, errors.New("test: the bootstrap must not run"))

	c := NewAPIClientFromNamedEntry("srv", &entry, DefaultTimeout)
	_, err := c.GetInfo()
	asIdentityRefusal(t, err, "srv", "does not exist")
	if status, serr := c.Status(); status != serverStatusRefused || serr == nil {
		t.Errorf("Status = %q, %v; want refused with the identity refusal", status, serr)
	}
	if calls.Load() != 0 {
		t.Errorf("the bootstrap ran %d time(s), want 0", calls.Load())
	}
	if got := m.accepted.Load(); got != 0 {
		t.Errorf("the server accepted %d connection(s), want 0", got)
	}
}

// TestTunnelHandOffNeverAdoptsAStoredCertificateForAnotherKey: a tunnel daemon
// builds its client from the entry as it is on disk, and the credential it
// hands to its Connect source comes from that client. Another process stored a
// certificate made for K1 while the entry's identity_file is K2: the daemon's
// source never holds it; it holds the K2 certificate its enrolment got.
func TestTunnelHandOffNeverAdoptsAStoredCertificateForAnotherKey(t *testing.T) {
	testClientConfig(t)
	m := newMTLSServer(t)
	dir := t.TempDir()
	k1, k2 := writeKey(t, dir, "id_old"), writeKey(t, dir, "id_work")
	fp1, fp2 := keyFingerprint(t, k1), keyFingerprint(t, k2)
	entry := storeCertForKey(t, m, k2, k2)
	// Another process stores a certificate for K1 under the same entry.
	certPEM, keyPEM, _ := m.issueForSubject(t, fp1, testUser, farFromExpiry)
	if _, _, err := config.WriteClientCredentials("srv", certPEM, keyPEM); err != nil {
		t.Fatal(err)
	}
	calls, _ := stubBootstrapIdentity(credentialForKey(t, m, fp2, testUser), nil)

	client := NewAPIClientFromNamedEntry("srv", &entry, DefaultTimeout)
	src := tunnelCredentialSource(client, "srv", &entry, tunnels.ConnectTarget{TLSPin: m.pin})
	if src == nil {
		t.Fatal("no tunnel credential source")
	}
	cred, _ := src.Current()
	cert := cred.ClientCertificate()
	if cert == nil {
		t.Fatal("the tunnel source holds no certificate")
	}
	leaf, err := x509.ParseCertificate(cert.Certificate[0])
	if err != nil {
		t.Fatal(err)
	}
	if got := leaf.Subject.CommonName; got != fp2 {
		t.Errorf("the tunnel source holds a certificate for %q, want K2's %q (K1 is %q)", got, fp2, fp1)
	}
	if calls.Load() != 1 {
		t.Errorf("bootstrap ran %d time(s), want 1 (the re-enrolment with K2)", calls.Load())
	}
}

// TestNoIdentityFileStoredCertificateUnchanged: an entry with no identity_file
// presents its stored certificate whatever key it was made for, exactly as
// before the field: no enrolment, no ssh.
func TestNoIdentityFileStoredCertificateUnchanged(t *testing.T) {
	testClientConfig(t)
	m := newMTLSServer(t)
	k1 := writeKey(t, t.TempDir(), "id_old")
	entry := storeCertForKey(t, m, k1, "")
	calls, _ := stubBootstrapIdentity(sdk.Credential{}, errors.New("test: the bootstrap must not run"))

	if _, err := NewAPIClientFromNamedEntry("srv", &entry, DefaultTimeout).GetInfo(); err != nil {
		t.Fatalf("GetInfo: %v", err)
	}
	if shown := m.shownCNs(); len(shown) == 0 || shown[0] != keyFingerprint(t, k1) {
		t.Errorf("shown %q, want the stored certificate", shown)
	}
	if calls.Load() != 0 {
		t.Errorf("the bootstrap ran %d time(s), want 0", calls.Load())
	}
}

// TestSDKIdentityErrorIsTheTypedRefusal: the SDK checks the identity file
// again at its own time of use; when the file is gone by then, its error comes
// back as the CLI's one typed refusal, named for the entry, never classified by
// its words. Here the path holds words the classifier looks for ("key not
// authorized", "connection refused"): the add must not read them as a refused
// key or an unreachable server (whose fallback could save the entry over
// plain HTTP), and nothing is written.
func TestSDKIdentityErrorIsTheTypedRefusal(t *testing.T) {
	cfgPath := testClientConfig(t)
	withCleanAddFlags(t)
	serverAddTrustTOFU, serverAddName = true, "srv"
	dir := filepath.Join(t.TempDir(), "key not authorized: connection refused")
	key := writeKey(t, dir, "id_work")
	serverAddIdentityFile = key
	stubHostKeyScan(t, "", errors.New("test: connection refused")) // nothing scanned: an unreachable bootstrap would fall back
	sdkErr := fmt.Errorf("sdk/bootstrap: %w", &sdkbootstrap.IdentityFileError{Path: key, Reason: "the file does not exist"})
	stubBootstrapIdentity(sdk.Credential{}, sdkErr)
	// The plain-HTTP fallback would probe this port: it must never be asked.
	var probes atomic.Int32
	probe := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { probes.Add(1) }))
	t.Cleanup(probe.Close)
	_, serverAddPort = hostPort(t, probe.URL)

	err := runServerAdd(nil, []string{"127.0.0.1"})
	asIdentityRefusal(t, err, "srv", "does not exist")
	if _, statErr := os.Stat(cfgPath); !os.IsNotExist(statErr) {
		t.Errorf("config.yaml was written (err=%v)", statErr)
	}
	if got := probes.Load(); got != 0 {
		t.Errorf("the HTTP fallback ran (%d probe(s))", got)
	}

	// The refresh hands it back typed and named the same way.
	entry := config.ServerEntry{Host: "127.0.0.1", SSHPort: 2222, APIURL: "https://127.0.0.1:18443", IdentityFile: key}
	putServerEntry(t, "srv", entry)
	_, err = controlCredentialRefresh(entry, "srv", "srv")()
	asIdentityRefusal(t, err, "srv", "does not exist")
}

// ---------------------------------------------------------------------------
// shed server add --identity-file
// ---------------------------------------------------------------------------

// TestServerAddIdentityFile: the add enrols with the key (the bootstrap gets
// it), stores it as an absolute path with ~ expanded, and says so; the key's
// public half is learned without its passphrase — an encrypted OpenSSH key
// works as it is, and a legacy encrypted key works with its .pub beside it.
func TestServerAddIdentityFile(t *testing.T) {
	for _, kind := range []string{"unencrypted", "encrypted OpenSSH", "legacy PEM with its .pub"} {
		t.Run(kind, func(t *testing.T) {
			cfgPath := testClientConfig(t)
			withCleanAddFlags(t)
			serverAddTrustTOFU = true
			home, _ := os.UserHomeDir()
			key := filepath.Join(home, ".ssh", "id work")
			if err := os.MkdirAll(filepath.Dir(key), 0o700); err != nil {
				t.Fatal(err)
			}
			var fp string
			switch kind {
			case "unencrypted":
				writeKeyAt(t, key, nil)
				fp = keyFingerprint(t, key)
			case "encrypted OpenSSH":
				writeKeyAt(t, key, []byte("correct horse"))
				fp = keyFingerprint(t, key)
			default:
				legacy := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY",
					Headers: map[string]string{"Proc-Type": "4,ENCRYPTED", "DEK-Info": "AES-128-CBC,00112233445566778899AABBCCDDEEFF"},
					Bytes:   []byte("only the passphrase opens this")})
				if err := os.WriteFile(key, legacy, 0o600); err != nil {
					t.Fatal(err)
				}
				pub := writeKey(t, t.TempDir(), "source")
				fp = keyFingerprint(t, pub)
				signer, err := gossh.ParsePrivateKey(must(os.ReadFile(pub)))
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(key+".pub", gossh.MarshalAuthorizedKey(signer.PublicKey()), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			serverAddIdentityFile = "~/.ssh/id work"

			m := newMTLSServer(t)
			host, _ := hostPort(t, m.srv.URL)
			hostKey, _ := testHostKey(t)
			stubHostKeyScan(t, hostKey, nil)
			_, identity := stubBootstrapIdentity(credentialForKey(t, m, fp, testUser), nil)

			var err error
			out := captureStdout(t, func() { err = runServerAdd(nil, []string{host}) })
			if err != nil {
				t.Fatalf("add: %v", err)
			}
			if got := identity.Load(); got != key {
				t.Errorf("the bootstrap got identity %q, want %q", got, key)
			}
			for _, cfg := range loadedAndInMemory(t, cfgPath) {
				if got := cfg.Servers["srv"].IdentityFile; got != key {
					t.Errorf("identity_file = %q, want %q", got, key)
				}
			}
			if !strings.Contains(out, "  Identity file: "+key+"\n") {
				t.Errorf("the add did not print the identity file:\n%s", out)
			}
		})
	}
}

// must returns v, failing the process on err (test setup only).
func must[T any](v T, err error) T {
	if err != nil {
		panic(err)
	}
	return v
}

// TestServerAddRefusesAnIdentityFileFirst: each refused value at the flag fails
// the add before anything happens: no host-key scan, no bootstrap, nothing
// written. That includes a legacy encrypted key with no .pub beside it, whose
// public key cannot be learned without its passphrase.
func TestServerAddRefusesAnIdentityFileFirst(t *testing.T) {
	dir := t.TempDir()
	key := writeKey(t, dir, "id_work")
	legacy := filepath.Join(dir, "id_rsa")
	block := &pem.Block{Type: "RSA PRIVATE KEY",
		Headers: map[string]string{"Proc-Type": "4,ENCRYPTED", "DEK-Info": "AES-128-CBC,00112233445566778899AABBCCDDEEFF"},
		Bytes:   []byte("only the passphrase opens this")}
	if err := os.WriteFile(legacy, pem.EncodeToMemory(block), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ name, value, reason string }{
		{"CR", key + "\r", "carriage return"},
		{"LF", key + "\nx", "line feed"},
		{"NUL", key + "\x00", "NUL"},
		{"double quote", key + `"`, "double quote"},
		{"backslash", key + `\x`, "backslash"},
		{"hash", key + "#x", "#"},
		{"percent", key + "%h", "%"},
		{"dollar", key + "$X", "$"},
		{"leading space", " " + key, "begins with whitespace"},
		{"trailing space", key + " ", "ends with whitespace"},
		{"only whitespace", "   ", "only whitespace"},
		{"relative", "id_work", "not an absolute path"},
		{"a directory", dir, "directory"},
		{"missing", filepath.Join(dir, "gone"), "does not exist"},
		{"invalid UTF-8", key + "\xff", "not valid UTF-8"},
		{"a legacy encrypted key with no .pub", legacy, "public key cannot be learned without a passphrase"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfgPath := testClientConfig(t)
			withCleanAddFlags(t)
			serverAddTrustTOFU = true
			serverAddName = "srv"
			serverAddIdentityFile = tc.value
			scans := stubHostKeyScan(t, "", errors.New("test: unreachable"))
			calls, _ := stubBootstrapIdentity(sdk.Credential{}, errors.New("test: unreachable"))

			err := runServerAdd(nil, []string{"shed.example.test"})
			asIdentityRefusal(t, err, "srv", tc.reason)
			if scans.calls.Load() != 0 || calls.Load() != 0 {
				t.Errorf("scans = %d, bootstraps = %d; want none", scans.calls.Load(), calls.Load())
			}
			if _, err := os.Stat(cfgPath); !os.IsNotExist(err) {
				t.Errorf("config.yaml was written (err=%v)", err)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// shed server update --identity-file
// ---------------------------------------------------------------------------

// TestServerUpdateIdentityFile: on its own the flag changes only identity_file,
// on any entry (a plain-HTTP one has no https api_url, and that is not this
// flag's business); "" clears it; a value that is only whitespace is refused,
// never read as "clear", and nothing is written.
func TestServerUpdateIdentityFile(t *testing.T) {
	cfgPath := testClientConfig(t)
	withCleanAddFlags(t)
	plain := config.ServerEntry{Host: "shed.example.test", HTTPPort: 8080, SSHPort: 2222}
	putServerEntry(t, "plain", plain)
	key := writeKey(t, t.TempDir(), "id work")

	serverUpdateIdentityFile = optionalString{value: key, set: true}
	var err error
	out := captureStdout(t, func() { err = runServerUpdate(nil, []string{"plain"}) })
	if err != nil {
		t.Fatalf("update --identity-file on a plain-HTTP entry: %v", err)
	}
	if !strings.Contains(out, "identity file: "+key) {
		t.Errorf("output = %q", out)
	}
	for _, cfg := range loadedAndInMemory(t, cfgPath) {
		got := cfg.Servers["plain"]
		if got.IdentityFile != key {
			t.Errorf("identity_file = %q, want %q", got.IdentityFile, key)
		}
		got.IdentityFile = ""
		if got != plain {
			t.Errorf("the update touched more than identity_file: %+v", got)
		}
	}

	before := snapshotFiles(t, cfgPath)
	serverUpdateIdentityFile = optionalString{value: "  ", set: true}
	asIdentityRefusal(t, runServerUpdate(nil, []string{"plain"}), "plain", "only whitespace")
	before.assertUnchanged(t)

	serverUpdateIdentityFile = optionalString{value: "", set: true}
	if err := runServerUpdate(nil, []string{"plain"}); err != nil {
		t.Fatalf(`update --identity-file "": %v`, err)
	}
	if got := onDisk(t, cfgPath).Servers["plain"].IdentityFile; got != "" {
		t.Errorf(`identity_file after --identity-file "" = %q, want cleared`, got)
	}

	// Without the flag the usage error names it.
	serverUpdateIdentityFile = optionalString{}
	putServerEntry(t, "tls", config.ServerEntry{Host: "shed.example.test", SSHPort: 2222, APIURL: "https://shed.example.test:8443"})
	if err := runServerUpdate(nil, []string{"tls"}); err == nil || !strings.Contains(err.Error(), "--identity-file") {
		t.Errorf("usage error = %v, want it to name --identity-file", err)
	}
}

// TestRefetchWithIdentityFile: with --refetch the re-enrolment uses the new
// key, and the key is saved with the rest; a refetch that is refused saves
// nothing, the new key included.
func TestRefetchWithIdentityFile(t *testing.T) {
	setup := func(t *testing.T) (*mtlsServer, string, string) {
		t.Helper()
		testClientConfig(t)
		withCleanAddFlags(t)
		serverAddTrustTOFU = true
		m := newMTLSServer(t)
		cfgPath, _, _ := addedMTLSServer(t, m)
		serverUpdateRefetch = true
		serverUpdateTrustTOFU = true
		key := writeKey(t, t.TempDir(), "pat's key")
		serverUpdateIdentityFile = optionalString{value: key, set: true}
		return m, cfgPath, key
	}

	t.Run("the bootstrap uses the new key, which is saved", func(t *testing.T) {
		m, cfgPath, key := setup(t)
		_, identity := stubBootstrapIdentity(credentialForKey(t, m, keyFingerprint(t, key), testUser), nil)
		jsonFlag = true
		var err error
		out := captureStdout(t, func() { err = runServerUpdate(nil, []string{"srv"}) })
		if err != nil {
			t.Fatalf("refetch: %v", err)
		}
		if got := identity.Load(); got != key {
			t.Errorf("the refetch's bootstrap got identity %q, want the new %q", got, key)
		}
		for _, cfg := range loadedAndInMemory(t, cfgPath) {
			if got := cfg.Servers["srv"].IdentityFile; got != key {
				t.Errorf("identity_file = %q, want %q", got, key)
			}
		}
		var got map[string]any
		if err := json.Unmarshal([]byte(out), &got); err != nil || got["identity_file"] != key {
			t.Errorf("JSON = %s (%v), want identity_file", out, err)
		}
	})

	t.Run("a refused refetch saves nothing, the key included", func(t *testing.T) {
		m, cfgPath, key := setup(t)
		certPath, keyPath := config.ClientCredentialPaths("srv")
		before := snapshotFiles(t, cfgPath, certPath, keyPath)
		// The key's own certificate, for another user: the user change is
		// refused.
		stubBootstrapIdentity(credentialForKey(t, m, keyFingerprint(t, key), "admin"), nil)
		freezeCreds(t, "srv")

		asRefusal(t, runServerUpdate(nil, []string{"srv"}), config.RefusedIssuedUserChange)
		before.assertUnchanged(t)
	})

	t.Run("a removed key fails the refetch before anything is pinned or run", func(t *testing.T) {
		for _, viaFlag := range []bool{true, false} {
			_, cfgPath, key := setup(t)
			if !viaFlag {
				// The key is the entry's own (set earlier), and no flag is given.
				serverUpdateIdentityFile = optionalString{}
				asAnotherProcess(t, cfgPath, func(c *config.ClientConfig) {
					e := c.Servers["srv"]
					e.IdentityFile = key
					c.Servers["srv"] = e
				})
				reloaded, err := config.LoadClientConfigFromPath(cfgPath)
				if err != nil {
					t.Fatal(err)
				}
				clientConfig = reloaded
			}
			if err := os.Remove(key); err != nil {
				t.Fatal(err)
			}
			before := snapshotFiles(t, cfgPath)
			scans := stubHostKeyScan(t, "", errors.New("test: unreachable"))
			calls, _ := stubBootstrapIdentity(sdk.Credential{}, errors.New("test: unreachable"))
			asIdentityRefusal(t, runServerUpdate(nil, []string{"srv"}), "srv", "does not exist")
			if scans.calls.Load() != 0 || calls.Load() != 0 {
				t.Errorf("via flag %v: scans = %d, bootstraps = %d; want none", viaFlag, scans.calls.Load(), calls.Load())
			}
			before.assertUnchanged(t)
		}
	})
}

// TestLegacyRefetchReportsTheIdentityFile: the legacy re-pin (an entry with no
// SSH endpoint, read over its https api_url) saves a changed identity file and
// reports it in --json as the SSH-first refetch does.
func TestLegacyRefetchReportsTheIdentityFile(t *testing.T) {
	cfgPath := testClientConfig(t)
	withCleanAddFlags(t)
	tlsSrv := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	t.Cleanup(tlsSrv.Close)
	putServerEntry(t, "legacy", config.ServerEntry{Host: "127.0.0.1", APIURL: tlsSrv.URL,
		TLSCertFingerprint: servertls.Fingerprint(tlsSrv.Certificate().Raw)})
	key := writeKey(t, t.TempDir(), "id_work")
	serverUpdateRefetch = true
	serverUpdateIdentityFile = optionalString{value: key, set: true}
	jsonFlag = true

	var err error
	out := captureStdout(t, func() { err = runServerUpdate(nil, []string{"legacy"}) })
	if err != nil {
		t.Fatalf("legacy refetch: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal([]byte(out), &got); err != nil || got["identity_file"] != key || got["name"] != "legacy" {
		t.Errorf("JSON = %s (%v), want name and identity_file", out, err)
	}
	if got := onDisk(t, cfgPath).Servers["legacy"].IdentityFile; got != key {
		t.Errorf("identity_file on disk = %q, want %q", got, key)
	}
}

// TestRefetchRefusedAfterAnotherProcessChangedTheIdentityFile is A5's
// interleaving for the second field a --refetch decides against: when another
// process sets the entry's identity_file while this refetch runs (during its
// bootstrap, after its network check, or after its certificate files were
// committed and before its row is saved), the refetch is refused, the files
// it committed are put back, and the other process's key stays.
func TestRefetchRefusedAfterAnotherProcessChangedTheIdentityFile(t *testing.T) {
	setup := func(t *testing.T) (*mtlsServer, string, string) {
		t.Helper()
		testClientConfig(t)
		withCleanAddFlags(t)
		serverAddTrustTOFU = true
		m := newMTLSServer(t)
		cfgPath, _, _ := addedMTLSServer(t, m)
		serverUpdateRefetch = true
		serverUpdateTrustTOFU = true
		return m, cfgPath, writeKey(t, t.TempDir(), "other key")
	}
	setIdentity := func(t *testing.T, cfgPath, key string) {
		t.Helper()
		asAnotherProcess(t, cfgPath, func(c *config.ClientConfig) {
			e := c.Servers["srv"]
			e.IdentityFile = key
			c.Servers["srv"] = e
		})
	}

	t.Run("during the bootstrap", func(t *testing.T) {
		m, cfgPath, other := setup(t)
		certPath, keyPath := config.ClientCredentialPaths("srv")
		issued := mtlsCredentialFor(t, m)
		var before fileSnapshot
		bootstrapCredentialFn = func(string, int, string, string, string) (sdk.Credential, error) {
			setIdentity(t, cfgPath, other)
			before = snapshotFiles(t, cfgPath, certPath, keyPath)
			freezeCreds(t, "srv")
			return issued, nil
		}
		asRefusal(t, runServerUpdate(nil, []string{"srv"}), config.RefusedEntryChanged)
		before.assertUnchanged(t)
		if got := onDisk(t, cfgPath).Servers["srv"].IdentityFile; got != other {
			t.Errorf("identity_file on disk = %q, want the other process's %q", got, other)
		}
	})

	t.Run("after the network check, at the commit", func(t *testing.T) {
		m, cfgPath, other := setup(t)
		certPath, keyPath := config.ClientCredentialPaths("srv")
		stubBootstrapIdentity(mtlsCredentialFor(t, m), nil)
		var before fileSnapshot
		origStage := stageCredentialsFn
		t.Cleanup(func() { stageCredentialsFn = origStage })
		stageCredentialsFn = func(name string, certPEM, keyPEM []byte) (*config.StagedClientCredentials, error) {
			setIdentity(t, cfgPath, other)
			before = snapshotFiles(t, cfgPath, certPath, keyPath)
			return origStage(name, certPEM, keyPEM)
		}
		asRefusal(t, runServerUpdate(nil, []string{"srv"}), config.RefusedEntryChanged)
		before.assertUnchanged(t)
		if got := onDisk(t, cfgPath).Servers["srv"].IdentityFile; got != other {
			t.Errorf("identity_file on disk = %q, want the other process's %q", got, other)
		}
	})

	t.Run("after the files are committed, before the row is saved", func(t *testing.T) {
		m, cfgPath, other := setup(t)
		certPath, keyPath := config.ClientCredentialPaths("srv")
		oldCert, oldKey := must(os.ReadFile(certPath)), must(os.ReadFile(keyPath))
		stubBootstrapIdentity(mtlsCredentialFor(t, m), nil)
		origCommit := commitStagedHeldFn
		t.Cleanup(func() { commitStagedHeldFn = origCommit })
		var committed bool
		commitStagedHeldFn = func(s *config.StagedClientCredentials) error {
			if err := origCommit(s); err != nil {
				return err
			}
			committed = true
			// The new pair is in place; the other process changes the key now,
			// before this refetch's save re-checks the row.
			setIdentity(t, cfgPath, other)
			return nil
		}
		asRefusal(t, runServerUpdate(nil, []string{"srv"}), config.RefusedEntryChanged)
		if !committed {
			t.Fatal("the commit never ran: the test proves nothing about this window")
		}
		// The committed pair was put back: the same bytes as before.
		if !bytes.Equal(must(os.ReadFile(certPath)), oldCert) || !bytes.Equal(must(os.ReadFile(keyPath)), oldKey) {
			t.Error("the refused refetch left its certificate files in place")
		}
		if got := onDisk(t, cfgPath).Servers["srv"]; got.IdentityFile != other || got.User != testUser {
			t.Errorf("row on disk = identity %q, user %q; want the other process's key and the user unchanged", got.IdentityFile, got.User)
		}
	})
}

// ---------------------------------------------------------------------------
// the generated ssh config and the roost alias
// ---------------------------------------------------------------------------

// TestGenerateEntriesIdentityFile: a shed on an entry with an identity file
// gets its four identity lines; one on an entry without gets the stanza it
// always had.
func TestGenerateEntriesIdentityFile(t *testing.T) {
	t.Setenv("HOME", "/home/pat")
	key := writeKey(t, t.TempDir(), "id work")
	keyed := &config.ServerEntry{Host: "shed.example.test", SSHPort: 2222, IdentityFile: key}
	plain := &config.ServerEntry{Host: "shed.example.test", SSHPort: 2222}
	entries, _, err := generateEntries([]shedInfo{
		{name: "a", serverName: "work", server: keyed},
		{name: "b", serverName: "home", server: plain},
	})
	if err != nil {
		t.Fatal(err)
	}
	stanza := "Host shed-a\n    HostName shed.example.test\n    Port 2222\n    User a\n" +
		"    UserKnownHostsFile /home/pat/.shed/known_hosts\n" +
		"    IdentityFile \"" + key + "\"\n    IdentitiesOnly yes\n    ControlMaster no\n    ControlPath none\n"
	if got := sshconfig.GenerateEntry(entries[0]); got != stanza {
		t.Errorf("stanza =\n%s\nwant\n%s", got, stanza)
	}
	if got := sshconfig.GenerateEntry(entries[1]); strings.Contains(got, "Identit") || strings.Contains(got, "Control") {
		t.Errorf("an entry without an identity file got identity lines:\n%s", got)
	}
}

// TestRoostAliasIdentityFile: the Host alias `shed attach` writes for roost
// carries the entry's identity lines; an alias shed wrote earlier with other
// lines is rewritten before use; an alias of that name shed did not write
// refuses the attach, naming the alias and the identity file, and changes
// nothing; without an identity file every existing alias is left alone, as
// before; and a key that cannot be used writes nothing.
func TestRoostAliasIdentityFile(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	key := writeKey(t, t.TempDir(), "id_work")
	lines := "    IdentityFile \"" + key + "\"\n    IdentitiesOnly yes\n    ControlMaster no\n    ControlPath none\n"
	newAttach := func(t *testing.T, content string) (*roostAttach, *bytes.Buffer) {
		t.Helper()
		path := filepath.Join(t.TempDir(), "config")
		if content != "" {
			if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		var out bytes.Buffer
		return &roostAttach{sshConfigPath: path, out: &out, shedServer: "srv"}, &out
	}
	read := func(t *testing.T, a *roostAttach) string {
		t.Helper()
		return string(must(os.ReadFile(a.sshConfigPath)))
	}
	keyed := &config.ServerEntry{Host: "shed.example.test", SSHPort: 2222, IdentityFile: key}
	plain := &config.ServerEntry{Host: "shed.example.test", SSHPort: 2222}
	stale := sshconfig.GenerateManagedBlock([]sshconfig.Entry{{Name: "shed-srv-box", Host: "shed.example.test", Port: 2222,
		User: "box", KnownHostsFile: config.GetKnownHostsPath()}})
	userWritten := "Host shed-srv-box\n    HostName shed.example.test\n    IdentityFile ~/.ssh/id_other\n"

	t.Run("a new alias carries the identity lines", func(t *testing.T) {
		a, _ := newAttach(t, "")
		if err := a.ensureSSHAlias("shed-srv-box", "box", keyed); err != nil {
			t.Fatal(err)
		}
		if got := read(t, a); !strings.Contains(got, lines) {
			t.Errorf("the alias does not carry the identity file:\n%s", got)
		}
	})

	t.Run("an alias shed wrote without the key is rewritten", func(t *testing.T) {
		a, out := newAttach(t, stale)
		if err := a.ensureSSHAlias("shed-srv-box", "box", keyed); err != nil {
			t.Fatal(err)
		}
		if got := read(t, a); !strings.Contains(got, lines) {
			t.Errorf("the stale alias was not rewritten:\n%s", got)
		}
		if !strings.Contains(out.String(), "rewrote Host shed-srv-box") {
			t.Errorf("output = %q, want it to say the alias was rewritten", out.String())
		}
	})

	t.Run("an alias shed did not write refuses the attach", func(t *testing.T) {
		a, _ := newAttach(t, userWritten)
		err := a.ensureSSHAlias("shed-srv-box", "box", keyed)
		var unmanaged *sshconfig.UnmanagedAliasError
		if !errors.As(err, &unmanaged) {
			t.Fatalf("err = %v, want the alias refused", err)
		}
		for _, want := range []string{"Host shed-srv-box", fmt.Sprintf("%q", key), `server "srv"`} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("message lacks %q: %v", want, err)
			}
		}
		if got := read(t, a); got != userWritten {
			t.Errorf("the file changed:\n%s", got)
		}
	})

	t.Run("without an identity file existing aliases are left alone", func(t *testing.T) {
		for _, content := range []string{stale, userWritten} {
			a, _ := newAttach(t, content)
			if err := a.ensureSSHAlias("shed-srv-box", "box", plain); err != nil {
				t.Fatalf("ensureSSHAlias: %v", err)
			}
			if got := read(t, a); got != content {
				t.Errorf("the file changed without an identity file:\n%s", got)
			}
		}
	})

	t.Run("a key that cannot be used writes nothing", func(t *testing.T) {
		a, _ := newAttach(t, "")
		gone := &config.ServerEntry{Host: "shed.example.test", SSHPort: 2222, IdentityFile: filepath.Join(t.TempDir(), "gone")}
		asIdentityRefusal(t, a.ensureSSHAlias("shed-srv-box", "box", gone), "srv", "does not exist")
		if _, err := os.Stat(a.sshConfigPath); !os.IsNotExist(err) {
			t.Errorf("an alias was written for a key that cannot be used (err=%v)", err)
		}
	})
}

// TestSSHConfigInstallIdentityLines: `shed ssh-config --install` rewrites a
// managed stanza whose identity lines are not its entry's — the short alias
// and the qualified roost alias `shed attach` wrote — and lists it under
// changed; with no identity file on the entry the comparison is by name
// only, as before the field existed, so a hand-edited stanza is never
// rewritten for that and a qualified roost alias is not kept.
func TestSSHConfigInstallIdentityLines(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	key := writeKey(t, t.TempDir(), "id_work")
	sshConfigPath := filepath.Join(home, ".ssh", "config")
	knownHosts := config.GetKnownHostsPath()
	stanza := func(name, user string) sshconfig.Entry {
		return sshconfig.Entry{Name: name, Host: "shed.example.test", Port: 2222, User: user, KnownHostsFile: knownHosts}
	}
	install := func(t *testing.T, server *config.ServerEntry, current []sshconfig.Entry, edit string) (string, sshConfigInstallResult) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(sshConfigPath), 0o700); err != nil {
			t.Fatal(err)
		}
		content := sshconfig.GenerateManagedBlock(current)
		content = strings.Replace(content, "    User box\n", "    User box\n"+edit, 1)
		if err := os.WriteFile(sshConfigPath, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		origJSON := jsonFlag
		t.Cleanup(func() { jsonFlag = origJSON })
		jsonFlag = true
		sheds := []shedInfo{{name: "box", serverName: "srv", server: server}}
		entries, _, err := generateEntries(sheds)
		if err != nil {
			t.Fatal(err)
		}
		var runErr error
		out := captureStdout(t, func() { runErr = runSSHConfigInstall(entries, sheds) })
		if runErr != nil {
			t.Fatalf("install: %v", runErr)
		}
		var result struct {
			Details sshConfigInstallResult `json:"details"`
		}
		if err := json.Unmarshal([]byte(out), &result); err != nil {
			t.Fatalf("JSON %s: %v", out, err)
		}
		return string(must(os.ReadFile(sshConfigPath))), result.Details
	}

	t.Run("with an identity file both aliases are rewritten", func(t *testing.T) {
		keyed := &config.ServerEntry{Host: "shed.example.test", SSHPort: 2222, IdentityFile: key}
		got, result := install(t, keyed, []sshconfig.Entry{stanza("shed-box", "box"), stanza("shed-srv-box", "box")}, "")
		if !slices.Equal(result.Changed, []string{"shed-box", "shed-srv-box"}) || len(result.Removed) != 0 {
			t.Errorf("result = %+v, want both aliases changed and none removed", result)
		}
		parsed := sshconfig.Parse(got)
		for _, name := range []string{"shed-box", "shed-srv-box"} {
			e := parsed.FindEntry(name)
			if e == nil || !strings.Contains(e.RawContent, `IdentityFile "`+key+`"`) || !strings.Contains(e.RawContent, "ControlPath none") {
				t.Errorf("%s does not carry the identity lines:\n%s", name, got)
			}
		}
	})

	t.Run("without an identity file a hand edit is not a change", func(t *testing.T) {
		plain := &config.ServerEntry{Host: "shed.example.test", SSHPort: 2222}
		got, result := install(t, plain, []sshconfig.Entry{stanza("shed-box", "box")}, "    ProxyJump bastion\n")
		if len(result.Changed) != 0 || len(result.Added) != 0 || len(result.Removed) != 0 {
			t.Errorf("result = %+v, want nothing to do", result)
		}
		if !strings.Contains(got, "ProxyJump bastion") {
			t.Errorf("the hand edit was lost:\n%s", got)
		}
	})

	t.Run("without an identity file a qualified roost alias is handled as before", func(t *testing.T) {
		plain := &config.ServerEntry{Host: "shed.example.test", SSHPort: 2222}
		_, result := install(t, plain, []sshconfig.Entry{stanza("shed-box", "box"), stanza("shed-srv-box", "box")}, "")
		if !slices.Equal(result.Removed, []string{"shed-srv-box"}) {
			t.Errorf("result = %+v, want the qualified alias removed as before", result)
		}
	})
}

// TestRoostRemoteForCarriesTheIdentityFile: the roost path's own ssh (the
// provider transport `shed attach` and `shed sessions` use) is built with the
// entry's identity file and its name.
func TestRoostRemoteForCarriesTheIdentityFile(t *testing.T) {
	a := &roostAttach{sshBin: "/usr/bin/ssh", shedServer: "srv"}
	_, target, err := a.remoteFor("box", &config.ServerEntry{Host: "shed.example.test", SSHPort: 2222, IdentityFile: "/home/pat/.ssh/id_work"})
	if err != nil {
		t.Fatal(err)
	}
	if target.IdentityFile != "/home/pat/.ssh/id_work" || target.Entry != "srv" {
		t.Errorf("target = %+v, want the identity file and its entry", target)
	}
}
