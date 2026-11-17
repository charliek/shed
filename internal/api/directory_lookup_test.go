package api

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/charliek/shed/internal/config"
	"github.com/charliek/shed/internal/directory"
	"github.com/charliek/shed/internal/servertls"
	gossh "golang.org/x/crypto/ssh"
)

// directoryTestKey is a fresh Ed25519 key's authorized_keys line and
// fingerprint.
func directoryTestKey(t *testing.T) (line, fp string) {
	t.Helper()
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	pk, err := gossh.NewPublicKey(pub)
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(gossh.MarshalAuthorizedKey(pk))), gossh.FingerprintSHA256(pk)
}

// boundLeaf is a control certificate for the key with this fingerprint,
// bound to user name, as the bootstrap issues one.
func boundLeaf(fp, name string) *x509.Certificate {
	leaf := testClientCert(fp, "control", time.Hour)
	leaf.Subject.Names = []pkix.AttributeTypeAndValue{{Type: servertls.ClientUserOID, Value: name}}
	return leaf
}

// TestDirectoryServiceClientUnchanged checks a list entry named like an
// auth.clients entry does not touch that service client: with the directory
// lookup the client's certificate is answered exactly as with the lookup it
// had, while the entry's own key opens nothing. The directory's users and
// its local admin are users through CertificateUser, each bound to their
// own name.
func TestDirectoryServiceClientUnchanged(t *testing.T) {
	sheds := []config.Shed{
		{Name: "client-box", Status: config.StatusRunning, Owner: testClientName},
		{Name: "admin-box", Status: config.StatusRunning},
	}
	_, before := clientTestServer(t, newClientFakeBackend(sheds...), defaultTestPolicy())
	want := do(before, serviceLeaf(testClientName), "GET", "/api/sheds", "")
	if want.code != http.StatusOK {
		t.Fatalf("the service client's list = %d %s", want.code, want.body)
	}

	rootLine, rootFP := directoryTestKey(t)
	adaLine, adaFP := directoryTestKey(t)
	clientEntryLine, clientEntryFP := directoryTestKey(t)
	entry := func(name string, admin bool, line string) string {
		return `{"name":` + strconv.Quote(name) + `,"admin":` + strconv.FormatBool(admin) +
			`,"keys":[{"key":` + strconv.Quote(line) + `,"label":"l"}]}`
	}
	body := `{"contract":1,"organization":"example","version":"` + strings.Repeat("ab", 32) + `","users":[` +
		entry(testClientName, true, clientEntryLine) + "," + entry("ada", false, adaLine) + `]}`
	list, err := directory.DecodeList([]byte(body), "example")
	if err != nil {
		t.Fatal(err)
	}
	lookup, err := directory.NewLookup(directory.LookupOptions{
		LocalAdmins:    map[string]config.UserConfig{"root": {Admin: true, Keys: []config.UserKeyConfig{{Key: rootLine, Label: "laptop"}}}},
		ServiceClients: []string{testClientName},
	}, list)
	if err != nil {
		t.Fatal(err)
	}
	s, after := clientTestServer(t, newClientFakeBackend(sheds...), defaultTestPolicy())
	s.SetUserLookup(lookup)

	got := do(after, serviceLeaf(testClientName), "GET", "/api/sheds", "")
	if got.code != want.code || !bytes.Equal(got.body, want.body) {
		t.Errorf("the service client's list with the directory lookup = %d %s; want %d %s", got.code, got.body, want.code, want.body)
	}

	for _, tc := range []struct {
		name string
		leaf *x509.Certificate
		code int
	}{
		{"local admin", boundLeaf(rootFP, "root"), http.StatusOK},
		{"listed user", boundLeaf(adaFP, "ada"), http.StatusOK},
		{"the client-named entry's key, bound to that name", boundLeaf(clientEntryFP, testClientName), http.StatusUnauthorized},
		{"the client-named entry's key, bound to ada", boundLeaf(clientEntryFP, "ada"), http.StatusUnauthorized},
		{"ada's key bound to root", boundLeaf(adaFP, "root"), http.StatusUnauthorized},
	} {
		if r := do(after, tc.leaf, "GET", "/api/sheds", ""); r.code != tc.code {
			t.Errorf("%s: GET /api/sheds = %d %s, want %d", tc.name, r.code, r.body, tc.code)
		}
	}
	if _, _, ok := CertificateUser(lookup, boundLeaf(clientEntryFP, testClientName), time.Now()); ok {
		t.Error("CertificateUser accepted the client-named entry's key")
	}
	if u, k, ok := CertificateUser(lookup, boundLeaf(adaFP, "ada"), time.Now()); !ok || u.Name != "ada" || u.Admin || k.Script {
		t.Errorf("CertificateUser(ada) = %+v %+v %v", u, k, ok)
	}
}
