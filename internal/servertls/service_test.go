package servertls

import (
	"crypto/elliptic"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"net"
	"net/url"
	"testing"
	"time"
)

func newServiceTestCA(t *testing.T) CA {
	t.Helper()
	_, certPath, keyPath := caPaths(t)
	ca, err := LoadOrGenerateCA(certPath, keyPath)
	if err != nil {
		t.Fatalf("generate CA: %v", err)
	}
	return ca
}

func signService(t *testing.T, ca CA, client string, ttl time.Duration) *x509.Certificate {
	t.Helper()
	key := mustECDSAKey(t, elliptic.P256())
	der, err := ca.SignServiceCert(&key.PublicKey, client, ttl)
	if err != nil {
		t.Fatalf("SignServiceCert: %v", err)
	}
	return leaf(t, der)
}

func TestSignServiceCertShape(t *testing.T) {
	ca := newServiceTestCA(t)
	before := time.Now()
	cert := signService(t, ca, "nocturn", 2*time.Hour)

	if cert.Subject.CommonName != "nocturn" {
		t.Errorf("CN = %q, want nocturn", cert.Subject.CommonName)
	}
	if ou := cert.Subject.OrganizationalUnit; len(ou) != 1 || ou[0] != ServiceOU {
		t.Errorf("OU = %v, want [service]", ou)
	}
	if len(cert.URIs) != 1 || cert.URIs[0].String() != "shed-client://nocturn" {
		t.Errorf("URIs = %v, want [shed-client://nocturn]", cert.URIs)
	}
	if len(cert.DNSNames)+len(cert.IPAddresses)+len(cert.EmailAddresses) != 0 {
		t.Errorf("unexpected SANs: dns=%v ip=%v email=%v", cert.DNSNames, cert.IPAddresses, cert.EmailAddresses)
	}
	if len(cert.ExtKeyUsage) != 1 || cert.ExtKeyUsage[0] != x509.ExtKeyUsageClientAuth {
		t.Errorf("ExtKeyUsage = %v, want [clientAuth]", cert.ExtKeyUsage)
	}
	if cert.KeyUsage != x509.KeyUsageDigitalSignature {
		t.Errorf("KeyUsage = %v, want digitalSignature", cert.KeyUsage)
	}
	if cert.IsCA {
		t.Error("service certificate is a CA")
	}
	if len(cert.SubjectKeyId) == 0 {
		t.Error("no subjectKeyIdentifier")
	}
	if got := cert.NotAfter.Sub(before); got < 2*time.Hour-time.Minute || got > 2*time.Hour+time.Minute {
		t.Errorf("NotAfter is %s after issuance, want about 2h", got)
	}
	// It chains to the CA as a client certificate.
	if _, err := cert.Verify(x509.VerifyOptions{Roots: ca.Pool(), KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}}); err != nil {
		t.Errorf("service cert does not verify against the CA: %v", err)
	}
	name, isService, err := ServiceClientName(cert)
	if !isService || err != nil || name != "nocturn" {
		t.Errorf("ServiceClientName = %q, %v, %v; want nocturn, true, nil", name, isService, err)
	}
}

func TestSignServiceCertClampsToCA(t *testing.T) {
	ca := newServiceTestCA(t)
	cert := signService(t, ca, "nocturn", 100*365*24*time.Hour)
	if !cert.NotAfter.Equal(ca.NotAfter()) {
		t.Errorf("NotAfter = %s, want clamped to the CA's %s", cert.NotAfter, ca.NotAfter())
	}
}

func TestSignServiceCertRefusals(t *testing.T) {
	ca := newServiceTestCA(t)
	key := mustECDSAKey(t, elliptic.P256())
	for _, name := range []string{"", "Nocturn", "1abc", "no:colon", "SHA256:abc", "a_b", "nocturn/x"} {
		if _, err := ca.SignServiceCert(&key.PublicKey, name, time.Hour); !errors.Is(err, ErrInvalidServiceName) {
			t.Errorf("SignServiceCert(%q) = %v, want ErrInvalidServiceName", name, err)
		}
	}
	if _, err := ca.SignServiceCert(&key.PublicKey, "nocturn", 0); !errors.Is(err, ErrCAInvalidTTL) {
		t.Errorf("zero ttl = %v, want ErrCAInvalidTTL", err)
	}
	p384 := mustECDSAKey(t, elliptic.P384())
	if _, err := ca.SignServiceCert(&p384.PublicKey, "nocturn", time.Hour); !errors.Is(err, ErrCSRUnsupportedKey) {
		t.Errorf("P-384 key = %v, want ErrCSRUnsupportedKey", err)
	}
	if _, err := (CA{}).SignServiceCert(&key.PublicKey, "nocturn", time.Hour); !errors.Is(err, ErrCANotInitialized) {
		t.Errorf("zero CA = %v, want ErrCANotInitialized", err)
	}
}

// serviceLeaf builds an unsigned leaf for classification tests: the
// classifier reads only subject and SANs.
func serviceLeaf(cn string, ous []string, uris []string) *x509.Certificate {
	c := &x509.Certificate{Subject: pkix.Name{CommonName: cn, OrganizationalUnit: ous}}
	for _, u := range uris {
		parsed, err := url.Parse(u)
		if err != nil {
			// A host that url.Parse refuses (an SSH fingerprint has a ':');
			// a certificate can still carry it.
			parsed = &url.URL{Scheme: ServiceURIScheme, Host: u[len(ServiceURIScheme)+3:]}
		}
		c.URIs = append(c.URIs, parsed)
	}
	return c
}

func TestServiceClientNameRejectsMalformed(t *testing.T) {
	withDNS := serviceLeaf("nocturn", []string{"service"}, []string{"shed-client://nocturn"})
	withDNS.DNSNames = []string{"nocturn.example"}
	withIP := serviceLeaf("nocturn", []string{"service"}, []string{"shed-client://nocturn"})
	withIP.IPAddresses = []net.IP{net.IPv4(10, 0, 0, 1)}
	withEmail := serviceLeaf("nocturn", []string{"service"}, []string{"shed-client://nocturn"})
	withEmail.EmailAddresses = []string{"a@b.c"}

	cases := map[string]*x509.Certificate{
		"two OUs":                    serviceLeaf("nocturn", []string{"service", "control"}, []string{"shed-client://nocturn"}),
		"OU control with URI SAN":    serviceLeaf("nocturn", []string{"control"}, []string{"shed-client://nocturn"}),
		"OU service, no URI SAN":     serviceLeaf("nocturn", []string{"service"}, nil),
		"mismatched URI SAN":         serviceLeaf("nocturn", []string{"service"}, []string{"shed-client://other"}),
		"two URI SANs":               serviceLeaf("nocturn", []string{"service"}, []string{"shed-client://nocturn", "shed-client://nocturn"}),
		"URI SAN with path":          serviceLeaf("nocturn", []string{"service"}, []string{"shed-client://nocturn/x"}),
		"extra DNS SAN":              withDNS,
		"extra IP SAN":               withIP,
		"extra email SAN":            withEmail,
		"CN is an SSH fingerprint":   serviceLeaf(testFingerprint, []string{"service"}, []string{"shed-client://" + testFingerprint}),
		"CN uppercase":               serviceLeaf("Nocturn", []string{"service"}, []string{"shed-client://Nocturn"}),
		"URI SAN only, no OU":        serviceLeaf("nocturn", nil, []string{"shed-client://nocturn"}),
		"URI SAN and a user CN + OU": serviceLeaf(testFingerprint, []string{"control"}, []string{"shed-client://nocturn"}),
	}
	for name, leaf := range cases {
		t.Run(name, func(t *testing.T) {
			got, isService, err := ServiceClientName(leaf)
			if !isService {
				t.Fatalf("not classified as service-shaped; a malformed service cert must never fall through to the user path")
			}
			if err == nil {
				t.Fatalf("accepted as client %q, want ErrMalformedServiceCert", got)
			}
		})
	}
}

func TestUserCertIsNeverServiceShaped(t *testing.T) {
	ca := newServiceTestCA(t)
	csr, _ := p256CSR(t)
	for _, scope := range []string{"control", "credentials", ""} {
		der, err := ca.SignClientCSR(csr, testFingerprint, scope, "cli", time.Hour)
		if err != nil {
			t.Fatalf("SignClientCSR: %v", err)
		}
		if name, isService, err := ServiceClientName(leaf(t, der)); isService || err != nil || name != "" {
			t.Errorf("user cert (scope %q) classified as service: %q, %v, %v", scope, name, isService, err)
		}
	}
	// A hostile CSR asking for the service OU and a shed-client URI SAN gets
	// neither: issuance ignores everything but the key.
	key := mustECDSAKey(t, elliptic.P256())
	u, _ := url.Parse("shed-client://nocturn")
	hostile := makeCSR(t, &x509.CertificateRequest{
		Subject:            pkix.Name{CommonName: "nocturn", OrganizationalUnit: []string{"service"}},
		URIs:               []*url.URL{u},
		SignatureAlgorithm: x509.ECDSAWithSHA256,
	}, key)
	der, err := ca.SignClientCSR(hostile, testFingerprint, "control", "cli", time.Hour)
	if err != nil {
		t.Fatalf("SignClientCSR(hostile): %v", err)
	}
	if _, isService, _ := ServiceClientName(leaf(t, der)); isService {
		t.Error("a user CSR asking for the service identity got it")
	}
}

func TestClientConnectionVerifier(t *testing.T) {
	ca := newServiceTestCA(t)
	svc := signService(t, ca, "nocturn", time.Hour)
	csr, _ := p256CSR(t)
	userDER, err := ca.SignClientCSR(csr, testFingerprint, "control", "cli", time.Hour)
	if err != nil {
		t.Fatalf("SignClientCSR: %v", err)
	}
	user := leaf(t, userDER)
	state := func(c *x509.Certificate) tls.ConnectionState {
		return tls.ConnectionState{PeerCertificates: []*x509.Certificate{c}}
	}
	known := func(name string) bool { return name == "nocturn" }

	t.Run("known client accepted without consulting the allowlist", func(t *testing.T) {
		v := ClientConnectionVerifier(func(string) bool {
			t.Fatal("the SSH allowlist was consulted for a service certificate")
			return true
		}, known)
		if err := v(state(svc)); err != nil {
			t.Fatalf("known client refused: %v", err)
		}
	})
	t.Run("unknown client refused without consulting the allowlist", func(t *testing.T) {
		v := ClientConnectionVerifier(func(string) bool {
			t.Fatal("the SSH allowlist was consulted for a service certificate")
			return true
		}, func(string) bool { return false })
		if err := v(state(svc)); !errors.Is(err, ErrUnknownServiceClient) {
			t.Fatalf("unknown client = %v, want ErrUnknownServiceClient", err)
		}
	})
	t.Run("nil clientKnown refuses every service cert", func(t *testing.T) {
		v := ClientConnectionVerifier(func(string) bool { return true }, nil)
		if err := v(state(svc)); !errors.Is(err, ErrUnknownServiceClient) {
			t.Fatalf("service cert with no clients = %v, want ErrUnknownServiceClient", err)
		}
	})
	t.Run("malformed service cert refused", func(t *testing.T) {
		v := ClientConnectionVerifier(func(string) bool { return true }, func(string) bool { return true })
		bad := serviceLeaf("nocturn", []string{"control"}, []string{"shed-client://nocturn"})
		if err := v(state(bad)); !errors.Is(err, ErrMalformedServiceCert) {
			t.Fatalf("malformed = %v, want ErrMalformedServiceCert", err)
		}
	})
	t.Run("user cert goes through the allowlist and never clientKnown", func(t *testing.T) {
		noClients := func(string) bool {
			t.Fatal("clientKnown was consulted for a user certificate")
			return true
		}
		allow := ClientConnectionVerifier(func(fp string) bool { return fp == testFingerprint }, noClients)
		if err := allow(state(user)); err != nil {
			t.Fatalf("allowlisted user refused: %v", err)
		}
		deny := ClientConnectionVerifier(func(string) bool { return false }, noClients)
		if err := deny(state(user)); !errors.Is(err, ErrClientCertNotAuthorized) {
			t.Fatalf("de-allowlisted user = %v, want ErrClientCertNotAuthorized", err)
		}
	})
	t.Run("no peer certificate", func(t *testing.T) {
		v := ClientConnectionVerifier(func(string) bool { return true }, known)
		if err := v(tls.ConnectionState{}); !errors.Is(err, ErrClientCertMissing) {
			t.Fatalf("no cert = %v, want ErrClientCertMissing", err)
		}
	})
}
