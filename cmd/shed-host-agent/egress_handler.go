package main

import (
	"bufio"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"
)

// namespaceEgress is the audit namespace for egress-control decisions. It is
// advertised to shed-desktop in the hello_ack and stamped on every egress
// AuditEntry so the desktop can filter the feed to ns=="egress".
const namespaceEgress = "egress"

// egressStreamPath is shed-server's SSE endpoint; one `data:` frame per decision.
const egressStreamPath = "/api/egress/stream"

// egressDecision mirrors shed-server's egress.AuditRecord JSON. shed-extensions
// is a separate module and cannot import shed's internal/egress, so the (small,
// stable) wire shape is duplicated here.
type egressDecision struct {
	Time       time.Time `json:"ts"`
	Shed       string    `json:"shed"`
	Host       string    `json:"host"`
	Port       int       `json:"port"`
	ResolvedIP string    `json:"resolved_ip"`
	Protocol   string    `json:"protocol"`
	Verdict    string    `json:"verdict"`
	Reason     string    `json:"reason"`
}

// errEgressUnavailable means the stream returned 501/404 — egress control is
// disabled on the server. Run backs off hard on it instead of tight-looping.
var errEgressUnavailable = errors.New("egress stream: unavailable (disabled on server)")

// tokenSource provides — and on a 401 invalidates — the control-scoped bearer
// token for the egress stream on a secure server. *credentialSource satisfies
// it; nil for an open server (which sends no minted token).
type tokenSource interface {
	Token() (string, error)
	Invalidate()
}

// EgressSubscriber consumes a shed-server's egress-audit SSE stream and records
// each decision into the AuditLogger (namespace "egress"), which fans it out to
// shed-desktop. Read-only: it never gates or modifies egress.
type EgressSubscriber struct {
	server     string
	url        string
	token      string
	tokens     tokenSource // control-token source for a secure server; nil = open
	httpClient *http.Client
	audit      *AuditLogger
	logger     *slog.Logger
}

// NewEgressSubscriber builds a subscriber for one server target with its own
// authenticated HTTP client (the SDK HostClient only streams the plugin bus): a
// fingerprint-pinned transport for https, else a plain client. tokens supplies
// the control-scoped token for a secure server (the egress route is
// control-scoped, unlike the credentials-scoped bus); pass nil for an open server.
func NewEgressSubscriber(t ServerTarget, tokens tokenSource, audit *AuditLogger, logger *slog.Logger) *EgressSubscriber {
	return &EgressSubscriber{
		server:     t.Name,
		url:        strings.TrimRight(t.URL, "/"),
		token:      t.Token,
		tokens:     tokens,
		httpClient: egressHTTPClient(t.URL, t.TLSFingerprint),
		audit:      audit,
		logger:     logger,
	}
}

// bearer returns the token to send: a control token for a secure server
// (re-minted near expiry by the source), else the static configured token (open
// servers send their usually-empty token). A mint error sends none — the request
// then 401s and Run retries after Invalidate.
func (s *EgressSubscriber) bearer() string {
	if s.tokens == nil {
		return s.token
	}
	tok, err := s.tokens.Token()
	if err != nil {
		s.logger.Debug("egress: control-token mint failed", "server", s.server, "error", err)
		return ""
	}
	return tok
}

// Run streams egress decisions until ctx is cancelled, reconnecting with
// exponential backoff (an offline server simply retries in the background). When
// the server reports egress disabled (501/404) it backs off hard rather than
// polling every 30s, while still re-checking whether egress gets enabled later.
func (s *EgressSubscriber) Run(ctx context.Context) {
	const base, max, unavailable = time.Second, 30 * time.Second, 5 * time.Minute
	backoff := base
	for ctx.Err() == nil {
		err := s.stream(ctx)
		if ctx.Err() != nil {
			return
		}
		wait := backoff
		if errors.Is(err, errEgressUnavailable) {
			wait, backoff = unavailable, base
			s.logger.Debug("egress disabled on server; backing off", "server", s.server, "backoff", wait)
		} else if err != nil {
			s.logger.Debug("egress stream ended; retrying", "error", err, "backoff", backoff)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
		if !errors.Is(err, errEgressUnavailable) {
			if backoff *= 2; backoff > max {
				backoff = max
			}
		}
	}
}

// stream makes one connection and forwards decisions until it errors or ctx ends.
func (s *EgressSubscriber) stream(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.url+egressStreamPath, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "text/event-stream")
	if tok := s.bearer(); tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	resp, err := s.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		if resp.StatusCode == http.StatusUnauthorized && s.tokens != nil {
			s.tokens.Invalidate() // expired/revoked control token → re-mint on reconnect
		}
		if resp.StatusCode == http.StatusNotImplemented || resp.StatusCode == http.StatusNotFound {
			return errEgressUnavailable
		}
		return fmt.Errorf("egress stream: unexpected status %d", resp.StatusCode)
	}

	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		data, ok := strings.CutPrefix(sc.Text(), "data: ")
		if !ok {
			continue // SSE comments / blank separators / event: lines
		}
		var dec egressDecision
		if err := json.Unmarshal([]byte(data), &dec); err != nil {
			continue // skip a malformed frame, keep streaming
		}
		s.audit.LogEntry(egressAuditEntry(s.server, dec))
	}
	return sc.Err()
}

// egressAuditEntry maps one streamed decision into an AuditEntry for the
// host-agent's audit log + desktop feed.
func egressAuditEntry(server string, d egressDecision) AuditEntry {
	ts := ""
	if !d.Time.IsZero() {
		ts = d.Time.UTC().Format(time.RFC3339)
	}
	detail := fmt.Sprintf("%s:%d", d.Host, d.Port)
	if d.ResolvedIP != "" {
		detail += " (" + d.ResolvedIP + ")"
	}
	return AuditEntry{
		Timestamp: ts,
		Server:    server,
		Shed:      d.Shed,
		Namespace: namespaceEgress,
		Operation: d.Protocol,
		Result:    d.Verdict,
		Detail:    detail,
		Reason:    d.Reason,
	}
}

// egressHTTPClient returns the authenticated-transport client for the stream:
// a fingerprint-pinned transport for an https URL + pin, a fail-closed client
// when a pin is set on a non-https URL, else a plain client. SSE is long-lived,
// so there is no overall request timeout. Mirrors the SDK's TLS pin (the sdk is
// a separate module and cannot be imported here).
func egressHTTPClient(serverURL, fingerprint string) *http.Client {
	if fingerprint == "" {
		return &http.Client{}
	}
	if !strings.HasPrefix(strings.ToLower(serverURL), "https://") {
		return &http.Client{Transport: egressErrorTransport{fmt.Errorf(
			"egress stream: TLS pin set but server URL %q is not https; refusing unpinned plaintext", serverURL)}}
	}
	tr := &http.Transport{}
	if base, ok := http.DefaultTransport.(*http.Transport); ok {
		tr = base.Clone()
	}
	tlsCfg := tr.TLSClientConfig.Clone()
	if tlsCfg == nil {
		tlsCfg = &tls.Config{}
	}
	if tlsCfg.MinVersion < tls.VersionTLS12 {
		tlsCfg.MinVersion = tls.VersionTLS12
	}
	tlsCfg.InsecureSkipVerify = true // verification is done by VerifyPeerCertificate (pin)
	tlsCfg.VerifyPeerCertificate = egressPinVerifier(fingerprint)
	tr.TLSClientConfig = tlsCfg
	return &http.Client{Transport: tr}
}

func egressPinVerifier(fingerprint string) func([][]byte, [][]*x509.Certificate) error {
	fingerprint = strings.ToLower(fingerprint) // hex.EncodeToString is lowercase
	return func(rawCerts [][]byte, _ [][]*x509.Certificate) error {
		if len(rawCerts) == 0 {
			return errors.New("server presented no TLS certificate")
		}
		sum := sha256.Sum256(rawCerts[0])
		if got := "sha256:" + hex.EncodeToString(sum[:]); got != fingerprint {
			return fmt.Errorf("TLS cert fingerprint mismatch: server presented %s, pinned %s", got, fingerprint)
		}
		return nil
	}
}

// egressErrorTransport fails every request with err (fail-closed when a pin is
// set on a non-https endpoint).
type egressErrorTransport struct{ err error }

func (e egressErrorTransport) RoundTrip(*http.Request) (*http.Response, error) { return nil, e.err }
