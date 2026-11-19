package directory

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"io"
	"math/big"
	"net"
	"net/url"
	"testing"
	"time"

	"golang.org/x/net/http2"
	"golang.org/x/net/http2/hpack"
)

// loopbackCertificate is a self-signed certificate for 127.0.0.1 and a pool
// that trusts it.
func loopbackCertificate(t *testing.T) (tls.Certificate, *x509.CertPool) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "127.0.0.1"},
		IPAddresses:           []net.IP{net.IPv4(127, 0, 0, 1)},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(cert)
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}, pool
}

// refusingH2 is what refuseEveryStream saw on its one connection.
type refusingH2 struct {
	proto      string   // the protocol the TLS handshake chose
	paths      []string // each stream's :path, in order
	assertions []string // each token request's assertion, read whole
	err        error
}

// refuseEveryStream accepts one connection on ln and speaks HTTP/2 on it
// by hand: it reads each request whole (headers and body), records it, and
// answers it with RST_STREAM REFUSED_STREAM, which tells a client the
// request was not processed and may be retried. It returns when the client
// closes the connection.
func refuseEveryStream(ln net.Listener) refusingH2 {
	var r refusingH2
	conn, err := ln.Accept()
	if err != nil {
		r.err = err
		return r
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(60 * time.Second))
	tc := conn.(*tls.Conn)
	if err := tc.Handshake(); err != nil {
		r.err = err
		return r
	}
	r.proto = tc.ConnectionState().NegotiatedProtocol
	preface := make([]byte, len(http2.ClientPreface))
	if _, err := io.ReadFull(conn, preface); err != nil || string(preface) != http2.ClientPreface {
		r.err = errors.New("no HTTP/2 client preface")
		return r
	}
	fr := http2.NewFramer(conn, conn)
	fr.ReadMetaHeaders = hpack.NewDecoder(4096, nil)
	if err := fr.WriteSettings(); err != nil {
		r.err = err
		return r
	}
	bodies := map[uint32]*bytes.Buffer{}
	refuse := func(id uint32) {
		form, _ := url.ParseQuery(bodies[id].String())
		if a := form.Get("client_assertion"); a != "" {
			r.assertions = append(r.assertions, a)
		}
		_ = fr.WriteRSTStream(id, http2.ErrCodeRefusedStream)
	}
	for {
		f, err := fr.ReadFrame()
		if err != nil {
			return r // the client closed the connection
		}
		switch f := f.(type) {
		case *http2.SettingsFrame:
			if !f.IsAck() {
				_ = fr.WriteSettingsAck()
			}
		case *http2.PingFrame:
			if !f.IsAck() {
				_ = fr.WritePing(true, f.Data)
			}
		case *http2.MetaHeadersFrame:
			r.paths = append(r.paths, f.PseudoValue("path"))
			bodies[f.StreamID] = &bytes.Buffer{}
			if f.StreamEnded() {
				refuse(f.StreamID)
			}
		case *http2.DataFrame:
			if b := bodies[f.StreamID]; b != nil {
				b.Write(f.Data())
			}
			if f.StreamEnded() {
				refuse(f.StreamID)
			}
		case *http2.GoAwayFrame:
			return r
		}
	}
}

// TestRefusedStreamIsNotRetried runs a login over HTTP/2 against a service
// that reads the token request whole and then refuses its stream
// (REFUSED_STREAM, which the HTTP/2 transport retries whenever it can
// rewind the body). The assertion reaches the service once: the transport
// cannot resend the token request, and the login fails.
func TestRefusedStreamIsNotRetried(t *testing.T) {
	cert, pool := loopbackCertificate(t)
	ln, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{Certificates: []tls.Certificate{cert}, NextProtos: []string{"h2"}})
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	done := make(chan refusingH2, 1)
	go func() { done <- refuseEveryStream(ln) }()

	issuer := "https://" + ln.Addr().String()
	c, err := NewClient(ClientConfig{Enrolment: worldEnrolment(t, issuer), Organization: worldOrg, RootCAs: pool})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	_, err = c.Pull(ctx)
	c.hc.CloseIdleConnections()
	if err == nil {
		t.Fatal("a pull succeeded against a service that refuses every stream")
	}
	if f := asFailure(t, err); f.Kind != KindTransport || f.Step != StepToken {
		t.Errorf("%v, want a transport failure of the login", f)
	}

	var r refusingH2
	select {
	case r = <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("the HTTP/2 service did not finish")
	}
	if r.err != nil {
		t.Fatal(r.err)
	}
	if r.proto != "h2" {
		t.Fatalf("the connection spoke %q, not h2: the test proves nothing", r.proto)
	}
	if len(r.paths) != 1 || r.paths[0] != "/oauth2/token" || len(r.assertions) != 1 {
		t.Fatalf("streams %v, %d assertions read: want the one token request, its assertion once", r.paths, len(r.assertions))
	}
	if _, _, _, ok := parseAssertion(t, r.assertions[0], fixtureJWKKey(t, "server-ed25519")); !ok {
		t.Error("the assertion read does not verify")
	}
}
