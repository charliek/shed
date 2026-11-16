package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/url"
	"testing"

	"github.com/charliek/shed/internal/plugin"
	"github.com/charliek/shed/internal/source"
	"github.com/charliek/shed/internal/source/fake"
)

// The credential bus pin in the API tests: every test server with a bus has
// source bindings (fake.Bindings), and every key stands for one machine of
// its own (testMachineFor), which the request helpers (do, sseServer)
// name in the machine headers unless a test sets them itself.

// testMachineFor is the machine a test request made with the key whose
// fingerprint is fp comes from ("" for open mode, which has no key).
func testMachineFor(fp string) source.Machine {
	sum := sha256.Sum256([]byte("machine:" + fp))
	id := hex.EncodeToString(sum[:])[:32]
	return source.Machine{ID: id, Name: "machine-" + id[:6], KeyFingerprint: fp}
}

// setMachineHeaders names machine m on req, the name percent-encoded as a
// broker sends it.
func setMachineHeaders(req *http.Request, m source.Machine) {
	req.Header.Set(HeaderMachineID, m.ID)
	req.Header.Set(HeaderMachineName, url.PathEscape(m.Name))
}

// withTestMachine names fp's machine on req unless the request names one.
func withTestMachine(req *http.Request, fp string) {
	if _, ok := req.Header[http.CanonicalHeaderKey(HeaderMachineID)]; ok {
		return
	}
	setMachineHeaders(req, testMachineFor(fp))
}

// pinBus gives s's bus fresh in-memory source bindings, and returns them.
func pinBus(s *Server) *fake.Bindings {
	b := fake.New(source.Options{})
	s.SetSourceBindings(b)
	return b
}

// listen admits fp's machine for user and registers it as user's listener
// for namespace, as a subscribe from that machine does (without a stream).
func listen(t *testing.T, s *Server, user, namespace, fp string) *plugin.Listener {
	t.Helper()
	l, err := s.plugins.Subscribe(context.Background(), plugin.ListenerKey{User: user, Namespace: namespace}, testMachineFor(fp))
	if err != nil {
		t.Fatalf("subscribing %s's machine %s to %s: %v", user, testMachineFor(fp).ID, namespace, err)
	}
	return l
}
