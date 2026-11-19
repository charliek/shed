package config

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/charliek/shed/internal/clienttoken"
	"github.com/charliek/shed/internal/servertls"
)

// clientCertPEM makes a client certificate and key whose subject carries the
// given userId values (none: a certificate that names no user).
func clientCertPEM(t *testing.T, users ...string) (certPEM, keyPEM []byte) {
	t.Helper()
	return clientCertPEMFor(t, "SHA256:test", users...)
}

// clientCertPEMFor is clientCertPEM with the subject CN given: the SSH key
// fingerprint a server writes on a certificate it issues to that key.
func clientCertPEMFor(t *testing.T, cn string, users ...string) (certPEM, keyPEM []byte) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	subject := pkix.Name{CommonName: cn}
	for _, u := range users {
		subject.ExtraNames = append(subject.ExtraNames, pkix.AttributeTypeAndValue{Type: servertls.ClientUserOID, Value: u})
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()),
		Subject:      subject,
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, key.Public(), key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
}

func writePair(t *testing.T, certPEM, keyPEM []byte) (certPath, keyPath string) {
	t.Helper()
	dir := t.TempDir()
	certPath, keyPath = filepath.Join(dir, "client.pem"), filepath.Join(dir, "client.key")
	if err := os.WriteFile(certPath, certPEM, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyPath, keyPEM, 0o600); err != nil {
		t.Fatal(err)
	}
	return certPath, keyPath
}

// TestLoadEntryCertificate is the one stored-certificate rule: a certificate
// is presented only when it names the entry's user or the implicit user; one
// that names no user, or a user the entry does not record, is refused; a
// malformed user attribute or a pair that does not load is an ordinary error
// (the caller re-enrols).
func TestLoadEntryCertificate(t *testing.T) {
	cases := []struct {
		name      string
		certUsers []string
		entryUser string
		want      string // "ok", "refused:<kind>", "error"
		kind      RefusalKind
	}{
		{"names the entry's user", []string{"pat"}, "pat", "ok", 0},
		{"the implicit user, entry records one", []string{servertls.ImplicitUserValue}, "pat", "ok", 0},
		{"the implicit user, entry records none", []string{servertls.ImplicitUserValue}, "", "ok", 0},
		{"names no user", nil, "", "refused", RefusedStoredNoUser},
		{"names no user, entry records one", nil, "pat", "refused", RefusedStoredNoUser},
		{"names a user the entry does not record", []string{"pat"}, "", "refused", RefusedStoredUserMismatch},
		{"names another user", []string{"admin"}, "pat", "refused", RefusedStoredUserMismatch},
		{"a malformed value", []string{"Pat"}, "pat", "error", 0},
		{"two attributes", []string{"pat", "pat"}, "pat", "error", 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			certPEM, keyPEM := clientCertPEM(t, tc.certUsers...)
			certPath, keyPath := writePair(t, certPEM, keyPEM)
			e := &ServerEntry{AuthMode: AuthModeMTLS, ClientCertFile: certPath, ClientKeyFile: keyPath, User: tc.entryUser}
			cert, err := LoadEntryCertificate("", e)
			r, refused := AsEntryRefused(err)
			switch tc.want {
			case "ok":
				if err != nil || cert == nil {
					t.Fatalf("got (%v, %v), want the certificate", cert, err)
				}
			case "refused":
				if !refused || r.Kind != tc.kind || cert != nil {
					t.Fatalf("got (%v, %v), want refusal kind %d", cert, err, tc.kind)
				}
				if !clienttoken.IsRefusal(err) {
					t.Error("the refusal does not end a credential source (clienttoken.Refusal)")
				}
			case "error":
				if err == nil || refused || cert != nil {
					t.Fatalf("got (%v, %v), want an ordinary (re-enrol) error", cert, err)
				}
			}
		})
	}

	// A mixed pair — the certificate of one enrolment beside the key of
	// another, as an interrupted or concurrent write can leave it, or as an
	// unlocked reader can see one mid-write — is never presented, whatever
	// user either half is for: it is an ordinary error, and the entry re-enrols
	// under the held-user rule.
	t.Run("a mixed pair", func(t *testing.T) {
		t.Setenv("HOME", t.TempDir()) // the named load takes the credential lock under ~/.shed/creds
		certPEM, _ := clientCertPEM(t, "pat")
		_, otherKey := clientCertPEM(t, "pat")
		certPath, keyPath := writePair(t, certPEM, otherKey)
		for _, name := range []string{"", "srv"} {
			cert, err := LoadEntryCertificate(name, &ServerEntry{AuthMode: AuthModeMTLS,
				ClientCertFile: certPath, ClientKeyFile: keyPath, User: "pat"})
			if _, refused := AsEntryRefused(err); cert != nil || err == nil || refused {
				t.Fatalf("name %q: got (%v, %v), want an ordinary error and nothing to present", name, cert, err)
			}
		}
	})

	t.Run("no files on record", func(t *testing.T) {
		_, err := LoadEntryCertificate("", &ServerEntry{AuthMode: AuthModeMTLS})
		if _, refused := AsEntryRefused(err); err == nil || refused {
			t.Fatalf("err = %v, want an ordinary error", err)
		}
	})
}

// TestReadEntryCertificateIsTheSameCheck: the unlocked reader that `shed
// server list` and `shed server add` use for display gives, for every stored
// state, the answer LoadEntryCertificate gives (presented, refused with the
// same kind and entry name, or an ordinary error), and takes no lock: the
// credential store under HOME is never created.
func TestReadEntryCertificateIsTheSameCheck(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	_, mixedKey := clientCertPEM(t, "pat")
	cases := []struct {
		name      string
		certUsers []string
		mixed     bool
		entryUser string
	}{
		{"names the entry's user", []string{"pat"}, false, "pat"},
		{"the implicit user, entry records none", []string{servertls.ImplicitUserValue}, false, ""},
		{"the implicit user, entry records one", []string{servertls.ImplicitUserValue}, false, "pat"},
		{"names no user", nil, false, ""},
		{"names a user the entry does not record", []string{"pat"}, false, ""},
		{"names another user", []string{"admin"}, false, "pat"},
		{"a malformed value", []string{"Pat"}, false, "pat"},
		{"a mixed pair", []string{"pat"}, true, "pat"},
	}
	type answer struct {
		presented bool
		refused   RefusalKind
		entry     string
		failed    bool
	}
	answerOf := func(cert any, err error) answer {
		a := answer{presented: err == nil && cert != nil, failed: err != nil}
		if r, ok := AsEntryRefused(err); ok {
			a.refused, a.entry = r.Kind, r.Entry
		}
		return a
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			certPEM, keyPEM := clientCertPEM(t, tc.certUsers...)
			if tc.mixed {
				keyPEM = mixedKey
			}
			certPath, keyPath := writePair(t, certPEM, keyPEM)
			e := &ServerEntry{AuthMode: AuthModeMTLS, ClientCertFile: certPath, ClientKeyFile: keyPath, User: tc.entryUser}
			readCert, readErr := ReadEntryCertificate("srv", e)
			if _, err := os.Stat(GetCredsDir()); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("the unlocked read touched the credential store (%v)", err)
			}
			loadCert, loadErr := LoadEntryCertificate("srv", e)
			if got, want := answerOf(readCert, readErr), answerOf(loadCert, loadErr); got != want {
				t.Errorf("ReadEntryCertificate = %+v (%v), LoadEntryCertificate = %+v (%v)", got, readErr, want, loadErr)
			}
			// The locked load made the store; drop it for the next case.
			if err := os.RemoveAll(GetCredsDir()); err != nil {
				t.Fatal(err)
			}
		})
	}
}

// TestStoredCertificateIsHeldToTheIdentityFile (plan 037 P-a, held wherever a
// stored pair is loaded for use): for an entry with an identity_file, a stored
// certificate is presented only when it was made for that key (its subject is
// the key's fingerprint). One made for another key is the ordinary,
// re-enrol error, never a refusal and never presented; an identity file whose
// public key cannot be learned is its typed *IdentityFileError, nothing
// presented. Both readers (the locked load and the display read) answer
// alike. An entry with no identity_file reads no identity file and keeps
// today's answer for a certificate of any subject.
func TestStoredCertificateIsHeldToTheIdentityFile(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	dir := t.TempDir()
	k1, k2 := identityKey(t, dir, "id_old"), identityKey(t, dir, "id_work")
	fp := func(path string) string {
		t.Helper()
		k, err := IdentityFileKey("", path)
		if err != nil {
			t.Fatal(err)
		}
		return k
	}
	var reads int
	orig := storedCertKeyFn
	t.Cleanup(func() { storedCertKeyFn = orig })
	storedCertKeyFn = func(entry, path string) (string, error) {
		reads++
		return orig(entry, path)
	}
	entryFor := func(cn, identity string) *ServerEntry {
		certPEM, keyPEM := clientCertPEMFor(t, cn, "pat")
		certPath, keyPath := writePair(t, certPEM, keyPEM)
		return &ServerEntry{AuthMode: AuthModeMTLS, ClientCertFile: certPath, ClientKeyFile: keyPath,
			User: "pat", IdentityFile: identity}
	}
	readers := map[string]func(string, *ServerEntry) (*tls.Certificate, error){
		"LoadEntryCertificate": LoadEntryCertificate,
		"ReadEntryCertificate": ReadEntryCertificate,
	}
	for name, read := range readers {
		t.Run(name, func(t *testing.T) {
			// Made for the identity file's key: presented.
			if cert, err := read("srv", entryFor(fp(k2), k2)); err != nil || cert == nil {
				t.Fatalf("a certificate for the entry's key: presented=%t, err=%v; want it presented", cert != nil, err)
			}

			// Made for another key: not presented, an ordinary error.
			cert, err := read("srv", entryFor(fp(k1), k2))
			var ie *IdentityFileError
			if _, refused := AsEntryRefused(err); cert != nil || err == nil || refused || errors.As(err, &ie) {
				t.Fatalf("a certificate for another key: presented=%t, err=%v; want an ordinary (re-enrol) error", cert != nil, err)
			}
			if !strings.Contains(err.Error(), fp(k1)) || !strings.Contains(err.Error(), fp(k2)) {
				t.Errorf("message %q names neither key", err)
			}

			// The identity file is gone: its typed refusal, nothing presented.
			gone := filepath.Join(dir, "removed")
			cert, err = read("srv", entryFor(fp(k2), gone))
			if cert != nil || !errors.As(err, &ie) || ie.Entry != "srv" || !strings.Contains(ie.Reason, "does not exist") {
				t.Fatalf("an identity file that is gone: presented=%t, err=%v; want its typed refusal", cert != nil, err)
			}

			// No identity_file: no identity file is read, and a certificate
			// of any subject is presented as before.
			reads = 0
			if cert, err := read("srv", entryFor(fp(k1), "")); err != nil || cert == nil {
				t.Fatalf("no identity_file: presented=%t, err=%v; want the certificate as before", cert != nil, err)
			}
			if reads != 0 {
				t.Errorf("no identity_file: %d identity-file read(s), want none", reads)
			}
		})
	}
}

// TestIssuedCertificateUser: a newly issued certificate is read fail closed.
func TestIssuedCertificateUser(t *testing.T) {
	cases := []struct {
		name     string
		certPEM  []byte
		user     string
		implicit bool
		kind     RefusalKind
	}{
		{"named", mustCert(t, "pat"), "pat", false, 0},
		{"implicit", mustCert(t, servertls.ImplicitUserValue), "", true, 0},
		{"names no user", mustCert(t), "", false, RefusedIssuedNoUser},
		{"malformed", mustCert(t, "Pat"), "", false, RefusedIssuedUnreadable},
		{"two attributes", mustCert(t, "pat", "admin"), "", false, RefusedIssuedUnreadable},
		{"not a certificate", []byte("garbage"), "", false, RefusedIssuedUnreadable},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			user, implicit, refusal := IssuedCertificateUser("srv", tc.certPEM)
			if tc.kind != 0 {
				if refusal == nil || refusal.Kind != tc.kind {
					t.Fatalf("refusal = %v, want kind %d", refusal, tc.kind)
				}
				return
			}
			if refusal != nil || user != tc.user || implicit != tc.implicit {
				t.Fatalf("got (%q, %v, %v), want (%q, %v)", user, implicit, refusal, tc.user, tc.implicit)
			}
		})
	}
}

func mustCert(t *testing.T, users ...string) []byte {
	t.Helper()
	c, _ := clientCertPEM(t, users...)
	return c
}

func TestHeldUserAccepts(t *testing.T) {
	cases := []struct {
		held, user string
		implicit   bool
		want       bool
	}{
		{"", "pat", false, true},
		{"pat", "pat", false, true},
		{"pat", "", true, true},
		{"", "", true, true},
		{"pat", "admin", false, false},
	}
	for _, tc := range cases {
		if got := HeldUserAccepts(tc.held, tc.user, tc.implicit); got != tc.want {
			t.Errorf("HeldUserAccepts(%q, %q, %v) = %v, want %v", tc.held, tc.user, tc.implicit, got, tc.want)
		}
	}
}

// TestEntryRefusedMessages pins the texts the plan's upgrade steps quote.
func TestEntryRefusedMessages(t *testing.T) {
	cases := []struct {
		err  *EntryRefusedError
		want string
	}{
		{&EntryRefusedError{Entry: "shed", Kind: RefusedStoredNoUser},
			`entry "shed" cannot be used: its certificate names no user (issued by a shed-server older than 0.10.0); ` +
				`no request was sent. Enrol it again: shed server update shed --refetch`},
		{&EntryRefusedError{Entry: "shed", Kind: RefusedStoredUserMismatch, CertUser: "pat"},
			`entry "shed" cannot be used: its certificate is for user "pat" but the entry records no user ` +
				`(written by a shed CLI older than 0.10.0, edited by hand, or an interrupted or concurrent enrolment); ` +
				`no request was sent. Enrol it again: shed server update shed --refetch`},
		{&EntryRefusedError{Entry: "shed", Kind: RefusedStoredUserMismatch, CertUser: "pat", EntryUser: "admin"},
			`entry "shed" cannot be used: its certificate is for user "pat" but the entry records user "admin" ` +
				`(edited by hand, or an interrupted or concurrent enrolment); ` +
				`no request was sent. Enrol it again: shed server update shed --refetch`},
		{&EntryRefusedError{Kind: RefusedIssuedNoUser},
			`the server issued a certificate that names no user: it runs a shed-server older than 0.10.0; ` +
				`nothing was saved. Upgrade the server to 0.10.0, then retry.`},
		{&EntryRefusedError{Entry: "shed", Kind: RefusedIssuedUserChange, CertUser: "admin", EntryUser: "pat"},
			"the server issued a certificate for user \"admin\", but entry \"shed\" is held to user \"pat\"; nothing was saved.\n" +
				"ssh offered a key that belongs to \"admin\". To stay \"pat\", let this entry offer only pat's key " +
				"(shed server update shed --identity-file <pat's private key>) and retry.\n" +
				"To make this entry \"admin\": shed server update shed --refetch --allow-user-change"},
		{&EntryRefusedError{Entry: "shed", Kind: RefusedEntryChanged},
			`entry "shed" changed while this command ran; nothing was saved. Run it again.`},
		{&EntryRefusedError{Entry: "shed", Kind: RefusedEntryUserChanged, CertUser: "pat", EntryUser: "admin"},
			`entry "shed" now records user "admin", but this command was using it as user "pat" ` +
				`(another command changed the entry while this one ran); nothing was saved and this command's ` +
				`certificate is no longer used. Run the command again.`},
		{&EntryRefusedError{Entry: "shed", Kind: RefusedRestoreFailed, Cause: errors.New("disk full")},
			`entry "shed": a client certificate update did not complete and the previous certificate could not be put back (disk full); ` +
				`its certificate files and its recorded user may now disagree. Enrol it again: shed server update shed --refetch`},
		{&EntryRefusedError{Entry: "shed", Kind: RefusedEntryUnreadable, Cause: errors.New("permission denied")},
			`entry "shed" could not be verified against its recorded user and identity (permission denied); ` +
				`nothing was saved and this command's certificate is no longer used. ` +
				`Check config.yaml, the entry's credential files and identity_file, then run the command again.`},
		{&EntryRefusedError{Entry: "shed", Kind: RefusedUserNotRecorded, CertUser: "pat", Cause: errors.New("read-only file system")},
			`the server issued a certificate for user "pat", but entry "shed" could not be updated to record that user ` +
				`(read-only file system); the certificate was not used and nothing was kept. ` +
				`Make config.yaml and the entry's credential files writable, then run the command again.`},
	}
	for _, tc := range cases {
		if got := tc.err.Error(); got != tc.want {
			t.Errorf("message:\n got %q\nwant %q", got, tc.want)
		}
	}
	// Only the refusals that withdraw what a process holds end its source at
	// once, however valid its certificate. A row that cannot be read fails
	// closed; a first user that could not be recorded ends the command, which
	// then says why.
	for kind := RefusedStoredNoUser; kind <= RefusedUserNotRecorded; kind++ {
		want := kind == RefusedEntryUserChanged || kind == RefusedRestoreFailed ||
			kind == RefusedEntryUnreadable || kind == RefusedUserNotRecorded
		if got := (&EntryRefusedError{Kind: kind}).InvalidatesHeld(); got != want {
			t.Errorf("kind %d: InvalidatesHeld = %v, want %v", kind, got, want)
		}
	}
	unreadable := &EntryRefusedError{Kind: RefusedIssuedUnreadable, Cause: errors.New("bad DER")}
	if !strings.Contains(unreadable.Error(), "bad DER") || !errors.Is(unreadable, unreadable.Cause) {
		t.Errorf("the unreadable refusal does not carry its cause: %v", unreadable)
	}
}

// TestStoredTokenRefusalEscapesTheEntryName: an entry name edited by hand into
// config.yaml can hold an escape sequence; the stored-token refusal shows it
// escaped everywhere, the suggested command included, never raw.
func TestStoredTokenRefusalEscapesTheEntryName(t *testing.T) {
	name := "srv\x1b[31m"
	msg := StoredTokenRefusal(name, &ServerEntry{IdentityFile: "/home/pat/.ssh/id_work"}).Error()
	if strings.ContainsRune(msg, '\x1b') {
		t.Errorf("the refusal carries a raw ESC: %q", msg)
	}
	if !strings.Contains(msg, `shed server update srv\x1b[31m --refetch`) {
		t.Errorf("the suggested command does not show the name escaped: %q", msg)
	}
}
