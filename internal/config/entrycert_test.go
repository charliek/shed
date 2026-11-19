package config

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
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
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	subject := pkix.Name{CommonName: "SHA256:test"}
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
			`entry "shed" could not be read to check the user it is held to (permission denied); ` +
				`nothing was saved and this command's certificate is no longer used. ` +
				`Make config.yaml and the entry's credential files readable (or, if the entry was removed or changed, ` +
				`see shed server list), then run the command again.`},
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
