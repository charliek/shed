// Package servertls generates and persists the self-signed TLS certificate the
// shed server presents on its HTTPS listener. Clients pin the certificate by
// the SHA-256 fingerprint of its DER encoding at `shed server add` — the same
// trust model as the SSH host key, with no CA, ACME, or domain required.
package servertls

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"sort"
	"time"
)

// certValidity is the lifetime of a generated certificate. The cert is pinned
// by fingerprint rather than chained to a CA, so a long life simply avoids
// forced churn; operators rotate deliberately (delete the files / change
// tls_names) and re-pin with `shed server update --fingerprint`.
const certValidity = 10 * 365 * 24 * time.Hour

// alwaysDNS / alwaysIP are SANs every generated cert carries so a same-host
// `shed server add` or curl over loopback works even when tls_names is empty.
var (
	alwaysDNS = []string{"localhost"}
	alwaysIP  = []net.IP{net.IPv4(127, 0, 0, 1), net.IPv6loopback}
)

// LoadOrGenerate returns the TLS certificate at certPath/keyPath, generating
// and persisting a fresh self-signed one when the files are absent or their
// SANs no longer cover names (plus the loopback defaults). It returns the
// parsed certificate and the leaf DER bytes for Fingerprint.
//
// A cert whose SANs already cover names is reused as-is, so its pinned
// fingerprint is stable across restarts. Changing tls_names regenerates the
// cert (new fingerprint) so an added name takes effect — clients must re-pin.
func LoadOrGenerate(certPath, keyPath string, names []string) (tls.Certificate, []byte, error) {
	dnsNames, ips := splitSANs(names)
	if cert, der, ok := loadIfCovers(certPath, keyPath, dnsNames, ips); ok {
		return cert, der, nil
	}
	return generate(certPath, keyPath, dnsNames, ips)
}

// Fingerprint returns the pin string clients compare against: "sha256:" plus
// the hex-encoded SHA-256 of the certificate's DER encoding.
func Fingerprint(der []byte) string {
	sum := sha256.Sum256(der)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// PinnedClientConfig returns a tls.Config that verifies the server's
// self-signed cert by the pinned fingerprint ("sha256:<hex>") instead of a CA
// chain — the shared trust primitive for every in-repo client (the CLI control
// plane and the Connect tunnel). InsecureSkipVerify disables the default
// chain/hostname check because the self-signed cert is its own anchor;
// VerifyPeerCertificate re-imposes trust by comparing the pin, so a mismatched
// (or absent) cert fails the handshake.
func PinnedClientConfig(fingerprint string) *tls.Config {
	return &tls.Config{
		MinVersion:         tls.VersionTLS12,
		InsecureSkipVerify: true, // not insecure: VerifyPeerCertificate pins the cert
		VerifyPeerCertificate: func(rawCerts [][]byte, _ [][]*x509.Certificate) error {
			if len(rawCerts) == 0 {
				return fmt.Errorf("server presented no TLS certificate")
			}
			if got := Fingerprint(rawCerts[0]); got != fingerprint {
				return fmt.Errorf("TLS cert fingerprint mismatch: server presented %s, pinned %s", got, fingerprint)
			}
			return nil
		},
	}
}

// loadIfCovers loads the persisted cert+key and returns it only when it parses
// and its SANs cover every requested DNS name and IP. Any failure (missing
// files, parse error, stale SANs) returns ok=false so the caller regenerates.
func loadIfCovers(certPath, keyPath string, dnsNames []string, ips []net.IP) (tls.Certificate, []byte, bool) {
	cert, err := tls.LoadX509KeyPair(certPath, keyPath)
	if err != nil || len(cert.Certificate) == 0 {
		return tls.Certificate{}, nil, false
	}
	leaf, err := x509.ParseCertificate(cert.Certificate[0])
	if err != nil || !covers(leaf, dnsNames, ips) {
		return tls.Certificate{}, nil, false
	}
	return cert, cert.Certificate[0], true
}

// covers reports whether leaf's SANs include every requested DNS name and IP.
func covers(leaf *x509.Certificate, dnsNames []string, ips []net.IP) bool {
	haveDNS := make(map[string]bool, len(leaf.DNSNames))
	for _, d := range leaf.DNSNames {
		haveDNS[d] = true
	}
	for _, d := range dnsNames {
		if !haveDNS[d] {
			return false
		}
	}
	haveIP := make(map[string]bool, len(leaf.IPAddresses))
	for _, ip := range leaf.IPAddresses {
		haveIP[ip.String()] = true
	}
	for _, ip := range ips {
		if !haveIP[ip.String()] {
			return false
		}
	}
	return true
}

// splitSANs merges the loopback defaults with names and classifies each as an
// IP or DNS SAN, deduped. DNS names are sorted and IPs kept in encounter order
// (loopback first) so the generated cert is deterministic for a given input.
func splitSANs(names []string) ([]string, []net.IP) {
	dnsSeen := make(map[string]bool)
	var dns []string
	addDNS := func(d string) {
		if d != "" && !dnsSeen[d] {
			dnsSeen[d] = true
			dns = append(dns, d)
		}
	}
	ipSeen := make(map[string]bool)
	var ips []net.IP
	addIP := func(ip net.IP) {
		if k := ip.String(); !ipSeen[k] {
			ipSeen[k] = true
			ips = append(ips, ip)
		}
	}

	for _, d := range alwaysDNS {
		addDNS(d)
	}
	for _, ip := range alwaysIP {
		addIP(ip)
	}
	for _, n := range names {
		if ip := net.ParseIP(n); ip != nil {
			addIP(ip)
		} else {
			addDNS(n)
		}
	}
	sort.Strings(dns)
	return dns, ips
}

// generate creates a self-signed ECDSA P-256 cert with the given SANs, persists
// the cert (0644) and key (0600), and returns the parsed pair + leaf DER.
func generate(certPath, keyPath string, dnsNames []string, ips []net.IP) (tls.Certificate, []byte, error) {
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, nil, fmt.Errorf("generate key: %w", err)
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return tls.Certificate{}, nil, fmt.Errorf("generate serial: %w", err)
	}
	now := time.Now()
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "shed-server"},
		NotBefore:             now.Add(-1 * time.Hour),
		NotAfter:              now.Add(certValidity),
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		DNSNames:              dnsNames,
		IPAddresses:           ips,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &priv.PublicKey, priv)
	if err != nil {
		return tls.Certificate{}, nil, fmt.Errorf("create certificate: %w", err)
	}
	if err := persist(certPath, keyPath, der, priv); err != nil {
		return tls.Certificate{}, nil, err
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: priv}, der, nil
}

// persist writes the cert and key PEM files, creating the parent directory.
func persist(certPath, keyPath string, der []byte, priv *ecdsa.PrivateKey) error {
	if dir := filepath.Dir(certPath); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0700); err != nil {
			return fmt.Errorf("create cert dir: %w", err)
		}
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	if err := os.WriteFile(certPath, certPEM, 0644); err != nil {
		return fmt.Errorf("write cert: %w", err)
	}
	keyDER, err := x509.MarshalECPrivateKey(priv)
	if err != nil {
		return fmt.Errorf("marshal key: %w", err)
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	if err := os.WriteFile(keyPath, keyPEM, 0600); err != nil {
		return fmt.Errorf("write key: %w", err)
	}
	return nil
}
