package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/charliek/shed/internal/backend"
	"github.com/charliek/shed/internal/clienttoken"
	"github.com/charliek/shed/internal/config"
	"github.com/charliek/shed/internal/servertls"
	"github.com/charliek/shed/sdk"
)

// DefaultTimeout for quick API operations (list, stop, delete, etc.)
const DefaultTimeout = 30 * time.Second

// APIClient provides methods for interacting with the shed server API.
type APIClient struct {
	baseURL       string
	httpClient    *http.Client
	transport     http.RoundTripper // non-nil when TLS-pinned; shared by every client below
	createTimeout time.Duration
	// tokens holds the client's credential — a bearer token or a client
	// certificate — and transparently re-mints it (proactively near expiry,
	// reactively on an auth-shaped failure). Static for open servers,
	// plain-HTTP clients, and legacy fixed tokens. Never nil.
	//
	// The transport above is built ONCE from this source and never rebuilt: the
	// certificate is fetched per handshake via GetClientCertificate, so a
	// rotation — or a token↔mtls mode flip — changes what is presented without
	// touching the transport or its connection pool.
	tokens *clienttoken.Source
}

// pinCredential stamps cred onto req as the ONE credential this attempt will
// transmit, through both channels a request can carry one:
//
//   - the Authorization header, in token state (in mtls state it adds nothing —
//     the credential travels in the handshake and the server's mtls middleware
//     never reads the header);
//   - the request context, which the TLS stack hands back to the transport's
//     GetClientCertificate during the handshake (see clienttoken.WithPinned).
//
// Taking BOTH from one captured value is what makes the generation a caller
// records the generation it actually sends. Reading the live Source separately
// in each place leaves a window in which a concurrent re-mint lands between
// them, and the reactive retry then re-sends the credential the server just
// rejected — Refresh treats the recorded generation as already superseded and
// skips the mint that would have fixed it.
//
// It returns the request to use: attaching a context produces a shallow copy,
// so the caller MUST send the returned one.
func pinCredential(req *http.Request, cred clienttoken.Credential) *http.Request {
	req = req.WithContext(clienttoken.WithPinned(req.Context(), cred))
	if tok := cred.BearerToken(); tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	return req
}

// setAuth pins the client's CURRENT credential onto a request that has no
// re-auth retry wrapped around it (the long-timeout and streaming paths that
// build their own client). Those cannot act on a rejection anyway, so capturing
// at send time is all there is to do — but they still go through one atomic
// capture, so header and handshake agree.
func (c *APIClient) setAuth(req *http.Request) *http.Request {
	cred, _ := c.tokens.Current()
	return pinCredential(req, cred)
}

// closeIdleConnections drops pooled keep-alive connections on the pinned
// transport.
//
// It is called after a re-mint, and it is not an optimization. A pooled
// connection was authenticated with the OLD certificate at handshake time;
// reusing it would replay the credential the server just rejected, so the retry
// would fail exactly as the original request did. Forcing a fresh handshake is
// what makes "re-mint, then retry once" actually retry with the new credential.
// (In token state the header is per-request, so this is merely harmless.)
func (c *APIClient) closeIdleConnections() {
	if t, ok := c.transport.(*http.Transport); ok {
		t.CloseIdleConnections()
	}
}

// currentToken returns the client's current in-memory bearer (control) token,
// or "" in mtls state. It may have been re-minted since construction —
// proactively near expiry or reactively on an auth failure — even when
// persisting the refresh back to config was skipped (ambiguous alias) or
// failed. The tunnel path reads it so a forwarded connection dials with the
// freshest token rather than the possibly-stale one in the config entry.
func (c *APIClient) currentToken() string {
	return c.tokens.Token()
}

// CredentialSource returns the client's credential source, so a long-lived
// consumer (the tunnel daemon) can seed its own Source from the
// freshly-refreshed credential this client obtained during setup.
func (c *APIClient) CredentialSource() *clienttoken.Source {
	return c.tokens
}

// newHTTPClient builds an *http.Client carrying the pinning transport (if any),
// so every request path — including the long-running, ad-hoc clients used for
// SSE and timeouts — verifies the pinned TLS cert. timeout 0 means no
// client-level timeout (the caller bounds it with a context).
func (c *APIClient) newHTTPClient(timeout time.Duration) *http.Client {
	return &http.Client{Timeout: timeout, Transport: c.transport}
}

// NewAPIClient creates a plain-HTTP API client for the given host and port
// (the bootstrap path, before any TLS pin is known).
//
// host:port is joined with net.JoinHostPort, not printf: an IPv6 literal has to
// be bracketed or the result is not a parseable URL, and this constructor is on
// the open-mode add path where the host comes straight from the command line.
func NewAPIClient(host string, port int, createTimeout time.Duration) *APIClient {
	return newAPIClient("http://"+net.JoinHostPort(host, strconv.Itoa(port)), "", "", createTimeout)
}

// newAPIClient is the shared constructor for a client with a STATIC credential:
// an explicit base URL, an optional bearer token, and an optional TLS pin.
func newAPIClient(baseURL, token, tlsFingerprint string, createTimeout time.Duration) *APIClient {
	return newAPIClientWithSource(baseURL, tlsFingerprint, clienttoken.Static(token), createTimeout)
}

// newAPIClientWithSource is the real constructor: it builds the pinned
// transport around src (via servertls, so the CLI, the Connect tunnel, and the
// startup probe all verify identically) so the TLS stack pulls the current
// client certificate from it per handshake. An empty fingerprint yields a nil
// transport — the plain-HTTP path.
//
// Order matters here. The source is created BEFORE the transport and never
// swapped afterwards, so the GetClientCertificate closure has a stable target
// and no request can observe a half-installed credential.
func newAPIClientWithSource(baseURL, tlsFingerprint string, src *clienttoken.Source, createTimeout time.Duration) *APIClient {
	c := &APIClient{
		baseURL:       strings.TrimRight(baseURL, "/"),
		transport:     servertls.PinnedTransport(tlsFingerprint, src.CertificateFor),
		createTimeout: createTimeout,
		tokens:        src,
	}
	c.httpClient = c.newHTTPClient(DefaultTimeout)
	return c
}

// NewAPIClientFromEntry creates an API client from a server entry, honoring its
// api_url/TLS pin and whichever credential it last bootstrapped. When the entry
// carries a bootstrap-minted credential (a control token with an expiry, or a
// client certificate), the client transparently re-mints it over SSH —
// proactively before expiry and reactively on an auth-shaped failure —
// persisting it back to the config entry it came from.
func NewAPIClientFromEntry(entry *config.ServerEntry, createTimeout time.Duration) *APIClient {
	// Resolve which config entry this is first: it names both the credential
	// lock the load takes and the entry a refreshed credential is persisted to.
	// "" means no unambiguous match (a one-off entry, or duplicate aliases to
	// the same endpoint) — the refresh still works for this client's lifetime,
	// it just isn't saved.
	name := serverNameForEntry(entry)
	initial, refreshable := entryCredential(entry, name)
	if !refreshable {
		// Static/legacy token or open server — no refresh wiring, exactly as
		// before client certificates existed.
		return newAPIClient(entry.BaseURL(), entry.ControlToken, entry.TLSCertFingerprint, createTimeout)
	}
	src := clienttoken.New(initial, controlCredentialRefresh(entry.Host, entry.SSHPort, name))
	c := newAPIClientWithSource(entry.BaseURL(), entry.TLSCertFingerprint, src, createTimeout)
	// Proactively re-mint a near-expiry credential so a request never races
	// expiry. A mint failure here is non-fatal (EnsureFresh keeps the stale
	// credential); the reactive retry surfaces any error on the next request.
	src.EnsureFresh()
	return c
}

// entryCredential reads a server entry's stored credential into the Source's
// shape and reports whether it is one the client should re-mint.
//
// The mtls branch is checked first and gated on the recorded auth_mode, so an
// entry that still has stale certificate files from before a flip back to token
// mode does not present them. A certificate that fails to load is NOT an error:
// it degrades to "refreshable with no usable credential", which EnsureFresh
// mints for before the first request goes out. Failing hard would leave the user
// with an unusable entry and no way back short of deleting files by hand.
//
// The same reasoning covers an entry with NO credential recorded at all. That is
// not necessarily an open server — it is also what a secure server's entry looks
// like after a pre-mtls client loaded and re-saved config.yaml, silently
// dropping every key its ServerEntry struct predates (auth_mode,
// client_cert_file, client_key_file, client_cert_expires_at). Both binaries
// being present is normal during an upgrade, so this shape is expected, and
// ServerEntry.NeedsEnrollment is what tells the two apart: an https entry with
// nothing to present enrolls, a plain-HTTP (open) one stays static and pays no
// SSH round-trip. Read as "open" the secure case fails forever — the client
// holds no credential to be rejected, so the reactive re-mint has nothing to
// fire on, and every invocation dies at the same TLS `certificate required`.
func entryCredential(entry *config.ServerEntry, name string) (cred clienttoken.Credential, refreshable bool) {
	if entry.IsMTLS() {
		cert, err := loadClientCert(entry, name)
		if err != nil && verboseLevel > 0 {
			fmt.Fprintf(os.Stderr, "warning: could not load client certificate for %s (re-enrolling): %v\n", entry.Host, err)
		}
		if cert != nil {
			return clienttoken.MTLSCredential(cert, entry.ClientCertExpiresAt), true
		}
		// No usable certificate — refreshable, holding nothing. EnsureFresh
		// enrolls on that (Credential.Usable), so no fake expiry is needed.
		return clienttoken.Credential{Mode: clienttoken.ModeMTLS}, true
	}
	if entry.ControlTokenExpiresAt.IsZero() {
		// Either an open server / legacy static token (static, as always), or a
		// secure entry stripped of its credential (enroll — holding nothing, so
		// EnsureFresh mints before the first request).
		return clienttoken.Credential{}, entry.NeedsEnrollment()
	}
	return clienttoken.TokenCredential(entry.ControlToken, entry.ControlTokenExpiresAt), true
}

// loadClientCert reads the entry's client certificate + key off disk, under the
// named server's credential lock so a concurrent rotation is never observed
// half-committed. A missing path or an unreadable/mismatched pair returns
// (nil, err) — see entryCredential for why that is recoverable rather than
// fatal. name may be "" (an ambiguous or one-off entry), which loads unlocked:
// nothing writes that entry either, so there is nothing to serialize against.
func loadClientCert(entry *config.ServerEntry, name string) (*tls.Certificate, error) {
	if entry.ClientCertFile == "" || entry.ClientKeyFile == "" {
		return nil, errors.New("no client certificate on file")
	}
	return config.LoadClientCredentials(name, entry.ClientCertFile, entry.ClientKeyFile)
}

// controlCredentialRefresh returns a refresh callback that mints a control
// credential over the SSH bootstrap channel and, when persistName != "",
// best-effort persists it to that config entry. It is shared by
// NewAPIClientFromEntry (persisting) and the tunnel daemon (persistName "" —
// mint-only, so a long-lived daemon never rewrites the user's config from its
// own stale in-memory copy).
//
// The bootstrap always submits a CSR, so the server answers in whatever mode it
// is actually in. Whichever comes back is adopted here AND written to the
// stored entry — that write is the mode-flip migration, and it runs in both
// directions.
func controlCredentialRefresh(host string, sshPort int, persistName string) func() (clienttoken.Credential, error) {
	return func() (clienttoken.Credential, error) {
		res, err := bootstrapCredentialFn(host, sshPort, "control", "cli")
		if err != nil {
			return clienttoken.Credential{}, err
		}
		if res.Mode() == sdk.AuthModeMTLS {
			return adoptMTLSCredential(res, persistName)
		}
		if persistName != "" {
			// Best-effort: a Save failure never wastes the mint or fails the
			// request (the next process just re-mints).
			persistTokenCredential(persistName, res.Bundle.Token, res.Bundle.ExpiresAt)
		}
		return clienttoken.TokenCredential(res.Bundle.Token, res.Bundle.ExpiresAt), nil
	}
}

// adoptMTLSCredential turns a freshly issued certificate into a usable
// credential and, when the entry is unambiguous, persists the pair to the creds
// dir + the config entry.
//
// A persist failure is NOT fatal to the mint. The in-memory certificate is
// valid and the current command should proceed on it; the next process simply
// re-enrolls. That mirrors the token path exactly — the alternative (failing the
// user's command because a file write failed) trades a working request for a
// tidy disk.
func adoptMTLSCredential(res sdk.Credential, persistName string) (clienttoken.Credential, error) {
	cert, err := tls.X509KeyPair([]byte(res.Bundle.ClientCert), res.KeyPEM)
	if err != nil {
		return clienttoken.Credential{}, fmt.Errorf("assemble issued client certificate: %w", err)
	}
	if persistName != "" {
		persistMTLSCredential(persistName, res)
	}
	return clienttoken.MTLSCredential(&cert, res.Bundle.ExpiresAt), nil
}

// configMu serializes refresh-path access to the shared clientConfig global —
// the serverNameForEntry lookup and the token persist (map write + Save). The
// `--all` fan-out (forEachServer) constructs clients, and thus refreshes,
// concurrently across goroutines; without this, two near-expiry refreshes would
// race on the Servers map (a fatal "concurrent map writes") and clobber each
// other's config.yaml save.
var configMu sync.Mutex

// persistTokenCredential writes a freshly re-minted control token back to the
// named config entry and saves. It is best-effort: name == "" (no unambiguous
// config entry) is a no-op, and a Save failure is warned-but-not-fatal — the
// token is already valid in memory, so the command proceeds and the next
// process re-mints rather than the request failing. The write goes through
// saveServerEntry, which holds configMu against the `--all` fan-out.
//
// It also completes the mtls→token half of a mode flip: an entry that used to
// hold a certificate has its cert fields cleared and its files deleted, so a
// server switched back to token mode leaves no private key sitting on disk.
//
// The deletion is ORDERED AFTER a successful save, and is skipped when the save
// fails. Deleting first (or unconditionally) trades one failure for a worse
// one: the save is best-effort and a full disk or a permission problem leaves
// config.yaml still naming those files, so removing them would strand the entry
// pointing at credentials that no longer exist. Keeping them costs an orphaned
// key only in the case where the config still refers to it — and the next
// successful save cleans it up.
func persistTokenCredential(name, token string, expiresAt time.Time) {
	if name == "" {
		return
	}
	var hadCert bool
	saveErr := saveServerEntry(name, "refreshed token", func(e *config.ServerEntry) {
		hadCert = e.AuthMode == config.AuthModeMTLS
		e.AuthMode = config.AuthModeToken
		e.ControlToken = token
		e.ControlTokenExpiresAt = expiresAt
		e.ClientCertFile, e.ClientKeyFile = "", ""
		e.ClientCertExpiresAt = time.Time{}
	})
	if !hadCert {
		return
	}
	if saveErr != nil {
		fmt.Fprintf(os.Stderr, "warning: keeping the stale client certificate for %q on disk: "+
			"the config entry still points at it because the save above failed\n", name)
		return
	}
	// The config is now saved WITHOUT the paths, so a removal failure can only
	// leave an orphan file — never a config pointing at a deleted one.
	if err := config.RemoveServerCredentials(name); err != nil {
		fmt.Fprintf(os.Stderr, "warning: could not remove stale client certificate for %q: %v\n", name, err)
	}
}

// persistMTLSCredential writes a freshly issued client certificate + key to the
// creds dir and points the named config entry at them, completing the
// token→mtls half of a mode flip (the stale bearer token is cleared — it is
// dead weight the mtls middleware would never read, and a secret with no
// remaining purpose).
//
// Same best-effort contract as persistTokenCredential: every failure is a
// warning, because the credential is already usable in memory.
func persistMTLSCredential(name string, res sdk.Credential) {
	certPath, keyPath, err := config.WriteClientCredentials(name, []byte(res.Bundle.ClientCert), res.KeyPEM)
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: could not store client certificate for %q: %v\n", name, err)
		return
	}
	_ = saveServerEntry(name, "client certificate", func(e *config.ServerEntry) {
		e.AuthMode = config.AuthModeMTLS
		e.ClientCertFile = certPath
		e.ClientKeyFile = keyPath
		e.ClientCertExpiresAt = res.Bundle.ExpiresAt
		e.ControlToken = ""
		e.ControlTokenExpiresAt = time.Time{}
	})
}

// saveServerEntry applies mutate to the named config entry and saves, holding
// configMu across the read-modify-write + Save. Every credential persist goes
// through here, so the locking discipline the `--all` fan-out depends on lives
// in exactly one place. what names the credential for the warning a failed Save
// prints; the failure is never fatal (see persistTokenCredential).
//
// It warns AND returns the save error. The warning is the user-facing half and
// every caller relies on it; the return value exists for the one caller that
// must not proceed on a failed save — deleting superseded credential files is
// only safe once the config that stopped referring to them is durable.
func saveServerEntry(name, what string, mutate func(*config.ServerEntry)) error {
	configMu.Lock()
	defer configMu.Unlock()
	e := clientConfig.Servers[name]
	mutate(&e)
	clientConfig.Servers[name] = e
	if err := clientConfig.Save(); err != nil {
		fmt.Fprintf(os.Stderr, "warning: could not persist %s for %q: %v\n", what, name, err)
		return err
	}
	return nil
}

// serverNameForEntry returns the config name whose stored entry UNIQUELY matches
// e by its stable identity (host + ssh port + api_url), or "" when there is no
// match (a one-off entry) or more than one (duplicate aliases to the same
// endpoint). Returning "" on ambiguity is deliberate: a refreshed token is only
// persisted when the target entry is unambiguous, so it can never be written to
// the wrong alias. Used so a refreshed token can be written back without
// threading the name through every call site.
//
// ControlToken is deliberately NOT part of the key: the refresh path rewrites
// it, so matching on it would make an entry stop matching its own config row
// after the first re-mint.
func serverNameForEntry(e *config.ServerEntry) string {
	configMu.Lock()
	defer configMu.Unlock()
	match := ""
	for n, se := range clientConfig.Servers {
		if se.Host == e.Host && se.SSHPort == e.SSHPort && se.APIURL == e.APIURL {
			if match != "" {
				return "" // ambiguous — refuse to persist to the wrong alias
			}
			match = n
		}
	}
	return match
}

// sendRequest builds and sends a single JSON request with cred as its pinned
// credential. It is the per-attempt work factored out of doRequest so the
// auth-failure path can retry with a refreshed one.
func (c *APIClient) sendRequest(method, path string, body interface{}, cred clienttoken.Credential) (*http.Response, error) {
	var bodyReader io.Reader
	if body != nil {
		bodyData, err := json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("failed to encode request: %w", err)
		}
		bodyReader = bytes.NewReader(bodyData)
	}
	req, err := http.NewRequest(method, c.baseURL+path, bodyReader)
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req = pinCredential(req, cred)
	return c.httpClient.Do(req)
}

// reauthenticated is the shared reactive re-mint for the request paths.
//
// It fires on an AUTH-SHAPED FAILURE, not on a status code: an HTTP 401, or a
// TLS-level rejection that never produced a response at all (see
// clienttoken.IsAuthFailure). The second case is what an mtls server's refusal
// looks like — under TLS 1.3 the server's certificate_required/expired alert
// surfaces as an error out of http.Client.Do — and it is classified regardless
// of the mode this client believes it is in, so a server flipped between token
// and mtls recovers on its own.
//
// sentGen is the credential generation the caller actually TRANSMITTED (see
// pinCredential), so a concurrent refresh isn't double-minted and a rejection
// is never attributed to the wrong generation. On a re-mint it drops pooled
// connections, because a keep-alive conn still carries the handshake identity
// that was just rejected.
//
// It returns the credential the retry must use — the freshly minted one, or the
// current one when a concurrent caller already replaced the rejected
// generation. A static credential (not Refreshable) or a non-auth outcome
// returns retry=false. Only a failed re-mint returns an error. Callers reach it
// through sendWithReauth, which owns the retry-once policy.
func (c *APIClient) reauthenticated(resp *http.Response, reqErr error, sentGen uint64) (fresh clienttoken.Credential, retry bool, err error) {
	status := 0
	if resp != nil {
		status = resp.StatusCode
	}
	if !clienttoken.IsAuthFailure(status, reqErr) || !c.tokens.Refreshable() {
		return clienttoken.Credential{}, false, nil
	}
	if resp != nil {
		_ = resp.Body.Close()
	}
	fresh, rerr := c.tokens.Refresh(sentGen)
	if rerr != nil {
		return clienttoken.Credential{}, false, fmt.Errorf("re-authenticating after %s: %w", authFailureReason(status), rerr)
	}
	c.closeIdleConnections()
	return fresh, true, nil
}

// authFailureReason names what triggered a re-mint, for the error a failed
// re-mint produces. It keeps the long-standing "after 401" wording for the
// token path rather than churning an existing message, and names the TLS case
// distinctly — the two look nothing alike to a user reading the failure.
func authFailureReason(status int) string {
	if status == http.StatusUnauthorized {
		return "401"
	}
	return "a TLS client-certificate rejection"
}

// sendWithReauth runs send and, on an auth-shaped failure, re-mints and runs it
// exactly once more. It is the single place the retry policy lives, shared by
// the doRequest path and the SSE streaming paths that bypass it.
//
// The credential and its generation are captured ONCE, atomically, before the
// first send, and the captured credential is handed to send — which pins it to
// the request so both the Authorization header and the TLS handshake use that
// exact value. That is what makes sentGen provably the generation transmitted,
// and it is load-bearing rather than tidy: if the request could transmit a
// generation newer than the one recorded, Refresh would see the recorded one as
// already superseded, skip the re-mint, and the single retry would re-send the
// credential the server had just rejected.
//
// wrapErr shapes the transport error each caller reports (a streaming caller
// distinguishes a context deadline, for instance). A failed RE-MINT is returned
// unwrapped: it already says what went wrong, and describing it as a connection
// failure would send the reader after the wrong problem.
func (c *APIClient) sendWithReauth(send func(clienttoken.Credential) (*http.Response, error), wrapErr func(error) error) (*http.Response, error) {
	cred, sentGen := c.tokens.Current()
	resp, err := send(cred)
	fresh, retry, rerr := c.reauthenticated(resp, err, sentGen)
	if rerr != nil {
		return nil, rerr
	}
	if retry {
		resp, err = send(fresh)
	}
	if err != nil {
		return nil, wrapErr(err)
	}
	return resp, nil
}

// connectFailure is the long-standing wording for a request that never reached
// the server.
func connectFailure(err error) error {
	return fmt.Errorf("failed to connect to server: %w", err)
}

// doRequest performs an HTTP request with JSON body and response handling. It
// handles connection errors, status validation, and JSON decoding, and
// transparently re-mints + retries once on an auth-shaped failure (an expired
// bootstrap token, or a rejected/expired client certificate).
func (c *APIClient) doRequest(method, path string, body, result interface{}, expectedStatus ...int) error {
	resp, err := c.sendWithReauth(
		func(cred clienttoken.Credential) (*http.Response, error) {
			return c.sendRequest(method, path, body, cred)
		},
		connectFailure)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	// Check for expected status codes
	validStatus := false
	if len(expectedStatus) == 0 {
		validStatus = resp.StatusCode == http.StatusOK
	} else {
		for _, s := range expectedStatus {
			if resp.StatusCode == s {
				validStatus = true
				break
			}
		}
	}
	if !validStatus {
		return c.parseError(resp)
	}

	// Decode result if provided
	if result != nil {
		if err := json.NewDecoder(resp.Body).Decode(result); err != nil {
			return fmt.Errorf("failed to parse response: %w", err)
		}
	}

	return nil
}

// doRequestWithTimeout performs an HTTP request with a custom timeout using context.
// Used for long-running operations like create and start that may need more time.
func (c *APIClient) doRequestWithTimeout(method, path string, body, result interface{}, timeout time.Duration, expectedStatus ...int) error {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	var bodyReader io.Reader
	if body != nil {
		bodyData, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("failed to encode request: %w", err)
		}
		bodyReader = bytes.NewReader(bodyData)
	}

	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, bodyReader)
	if err != nil {
		return fmt.Errorf("failed to create request: %w", err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	req = c.setAuth(req)

	// Create a client without a Timeout for long-running requests.
	// Important: When both http.Client.Timeout and context deadline are set,
	// the shorter one wins. Since c.httpClient has a 30s timeout, we must use
	// a separate client here to allow the context timeout (potentially minutes)
	// to control cancellation. It still carries the pinning transport.
	client := c.newHTTPClient(0)
	resp, err := client.Do(req)
	if err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			return fmt.Errorf("request timed out after %v (use --timeout to increase)", timeout)
		}
		return fmt.Errorf("failed to connect to server: %w", err)
	}
	defer resp.Body.Close()

	// Check for expected status codes
	validStatus := false
	if len(expectedStatus) == 0 {
		validStatus = resp.StatusCode == http.StatusOK
	} else {
		for _, s := range expectedStatus {
			if resp.StatusCode == s {
				validStatus = true
				break
			}
		}
	}
	if !validStatus {
		return c.parseError(resp)
	}

	// Decode result if provided
	if result != nil {
		if err := json.NewDecoder(resp.Body).Decode(result); err != nil {
			return fmt.Errorf("failed to parse response: %w", err)
		}
	}

	return nil
}

// GetInfo retrieves server information.
func (c *APIClient) GetInfo() (*config.ServerInfo, error) {
	var info config.ServerInfo
	if err := c.doRequest(http.MethodGet, "/api/info", nil, &info); err != nil {
		return nil, err
	}
	// Both released servers and current token-mode servers report the legacy
	// "secure" spelling on this wire (config.LegacyWireAuthMode); normalize
	// here so every consumer compares against the canonical constants.
	info.AuthMode = config.NormalizeAuthMode(info.AuthMode)
	return &info, nil
}

// GetSSHHostKey retrieves the server's SSH host key.
func (c *APIClient) GetSSHHostKey() (*config.SSHHostKeyResponse, error) {
	var hostKey config.SSHHostKeyResponse
	if err := c.doRequest(http.MethodGet, "/api/ssh-host-key", nil, &hostKey); err != nil {
		return nil, err
	}
	return &hostKey, nil
}

// ListSheds retrieves all sheds from the server.
func (c *APIClient) ListSheds() (*config.ShedsResponse, error) {
	var sheds config.ShedsResponse
	if err := c.doRequest(http.MethodGet, "/api/sheds", nil, &sheds); err != nil {
		return nil, err
	}
	return &sheds, nil
}

// ListImages returns available image variants from the server.
func (c *APIClient) ListImages() (*config.ImagesResponse, error) {
	var images config.ImagesResponse
	if err := c.doRequest(http.MethodGet, "/api/images", nil, &images); err != nil {
		return nil, err
	}
	return &images, nil
}

// CreateShed creates a new shed.
func (c *APIClient) CreateShed(req *config.CreateShedRequest) (*config.Shed, error) {
	var shed config.Shed
	if err := c.doRequestWithTimeout(http.MethodPost, "/api/sheds", req, &shed, c.createTimeout, http.StatusCreated, http.StatusOK); err != nil {
		return nil, err
	}
	return &shed, nil
}

// CreateShedWithProgress creates a new shed and streams progress events via SSE.
func (c *APIClient) CreateShedWithProgress(req *config.CreateShedRequest, wantBlobProgress bool, onProgress func(backend.ProgressEvent)) (*config.Shed, error) {
	ctx, cancel := context.WithTimeout(context.Background(), c.createTimeout)
	defer cancel()

	bodyData, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("failed to encode request: %w", err)
	}

	// Opt into structured per-blob byte events (the image-pull leg) only when
	// the caller can render them; older servers ignore the param.
	url := c.baseURL + "/api/sheds"
	if wantBlobProgress {
		url += "?progress=blob"
	}
	// Streaming: no client-level timeout, context deadline only — so this
	// bypasses doRequest and wires its own send through the shared retry
	// policy (mirrors DeleteShedWithProgress). The credential is re-read per
	// send so the retry carries the re-minted one; the body is a fresh reader
	// each time because the first send consumed it.
	client := c.newHTTPClient(0)
	send := func(cred clienttoken.Credential) (*http.Response, error) {
		httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(bodyData))
		if err != nil {
			return nil, fmt.Errorf("failed to create request: %w", err)
		}
		httpReq.Header.Set("Content-Type", "application/json")
		httpReq.Header.Set("Accept", "text/event-stream")
		httpReq = pinCredential(httpReq, cred)
		return client.Do(httpReq)
	}
	wrapSendErr := func(err error) error {
		if ctx.Err() == context.DeadlineExceeded {
			return fmt.Errorf("request timed out after %v (use --timeout to increase)", c.createTimeout)
		}
		return connectFailure(err)
	}

	resp, err := c.sendWithReauth(send, wrapSendErr)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, c.parseError(resp)
	}

	shed, err := c.readShedSSEStream(resp.Body, onProgress)
	return shed, err
}

// readShedSSEStream is the create-shed wrapper around readSSEStream: it
// decodes the terminal "complete" payload into a config.Shed.
func (c *APIClient) readShedSSEStream(body io.Reader, onProgress func(backend.ProgressEvent)) (*config.Shed, error) {
	data, err := c.readSSEStream(body, onProgress)
	if err != nil {
		return nil, err
	}
	var shed config.Shed
	if err := json.Unmarshal(data, &shed); err != nil {
		return nil, fmt.Errorf("failed to parse complete event: %w", err)
	}
	return &shed, nil
}

// readSSEStream parses an SSE event stream, calling onProgress for progress
// events and returning the RAW JSON payload of the terminal "complete" event
// (the caller decodes it into the operation-specific type — a Shed for create,
// an ImagePullResponse for pull). An "error" event is surfaced as a Go error.
//
// This implements the key parts of the SSE specification:
//   - "event:" sets the event type for the next dispatch
//   - "data:" lines are concatenated (with newlines) to form the event payload
//   - Lines starting with ":" are comments (used for keep-alive pings)
//   - A blank line dispatches the accumulated event
func (c *APIClient) readSSEStream(body io.Reader, onProgress func(backend.ProgressEvent)) ([]byte, error) {
	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 0, 256*1024), 256*1024)

	var eventType string
	var dataBuf strings.Builder

	dispatch := func(eventType, data string) (done bool, payload []byte, err error) {
		switch eventType {
		case "progress":
			var event backend.ProgressEvent
			if uerr := json.Unmarshal([]byte(data), &event); uerr == nil && onProgress != nil {
				onProgress(event)
			}
		case "complete":
			return true, []byte(data), nil
		case "error":
			var apiErr config.APIError
			if uerr := json.Unmarshal([]byte(data), &apiErr); uerr != nil {
				return true, nil, fmt.Errorf("server error: %s", data)
			}
			return true, nil, fmt.Errorf("%s: %s", apiErr.Error.Code, apiErr.Error.Message)
		}
		return false, nil, nil
	}

	for scanner.Scan() {
		line := scanner.Text()

		// Blank line dispatches the accumulated event
		if line == "" {
			if dataBuf.Len() > 0 {
				data := dataBuf.String()
				dataBuf.Reset()
				if done, payload, err := dispatch(eventType, data); done {
					return payload, err
				}
			}
			eventType = ""
			continue
		}

		// Comments (including keep-alive pings)
		if strings.HasPrefix(line, ":") {
			continue
		}

		if strings.HasPrefix(line, "event:") {
			eventType = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
			continue
		}

		if strings.HasPrefix(line, "data:") {
			value := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
			if dataBuf.Len() > 0 {
				dataBuf.WriteByte('\n')
			}
			dataBuf.WriteString(value)
			continue
		}
	}

	// Handle a final event if EOF occurs before a trailing blank line.
	if dataBuf.Len() > 0 {
		if done, payload, err := dispatch(eventType, dataBuf.String()); done {
			return payload, err
		}
	}

	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("error reading event stream: %w", err)
	}

	return nil, fmt.Errorf("event stream ended without a complete or error event")
}

// GetShed retrieves a specific shed by name.
func (c *APIClient) GetShed(name string) (*config.Shed, error) {
	var shed config.Shed
	if err := c.doRequest(http.MethodGet, "/api/sheds/"+name, nil, &shed); err != nil {
		return nil, err
	}
	return &shed, nil
}

// EgressShow returns a shed's egress status (active profiles + recent decisions).
func (c *APIClient) EgressShow(name string) (*config.EgressStatus, error) {
	var status config.EgressStatus
	if err := c.doRequest(http.MethodGet, "/api/egress/"+name, nil, &status); err != nil {
		return nil, err
	}
	return &status, nil
}

// EgressSet applies a profile selection to a shed (live on a running shed).
func (c *APIClient) EgressSet(name string, profiles []string) (*config.Shed, error) {
	var shed config.Shed
	req := config.EgressSetRequest{Profiles: profiles}
	if err := c.doRequest(http.MethodPost, "/api/egress/"+name, req, &shed); err != nil {
		return nil, err
	}
	return &shed, nil
}

// EgressOff turns egress control off for a shed.
func (c *APIClient) EgressOff(name string) (*config.Shed, error) {
	var shed config.Shed
	if err := c.doRequest(http.MethodDelete, "/api/egress/"+name, nil, &shed); err != nil {
		return nil, err
	}
	return &shed, nil
}

// EgressProfilesList returns all egress profiles (config baseline + user store),
// each tagged with its source.
func (c *APIClient) EgressProfilesList() ([]config.EgressProfileInfo, error) {
	var infos []config.EgressProfileInfo
	if err := c.doRequest(http.MethodGet, "/api/egress/profiles", nil, &infos); err != nil {
		return nil, err
	}
	return infos, nil
}

// EgressProfileGet returns one egress profile by name.
func (c *APIClient) EgressProfileGet(name string) (*config.EgressProfileInfo, error) {
	var info config.EgressProfileInfo
	if err := c.doRequest(http.MethodGet, "/api/egress/profiles/"+name, nil, &info); err != nil {
		return nil, err
	}
	return &info, nil
}

// EgressProfilePut creates or replaces a user profile (whole document).
func (c *APIClient) EgressProfilePut(name string, p config.EgressProfile) (*config.EgressProfileInfo, error) {
	var info config.EgressProfileInfo
	if err := c.doRequest(http.MethodPut, "/api/egress/profiles/"+name, p, &info); err != nil {
		return nil, err
	}
	return &info, nil
}

// EgressProfileDelete removes a user profile.
func (c *APIClient) EgressProfileDelete(name string) error {
	return c.doRequest(http.MethodDelete, "/api/egress/profiles/"+name, nil, nil)
}

// DeleteShedWithProgress deletes a shed and streams teardown progress via SSE.
// It uses no client-level timeout (context deadline only, like create), so an
// active event stream never trips the 30s quick-op timeout while a delete runs.
// It falls back cleanly to a plain delete when the server predates delete-SSE: a
// 204 No Content means the delete succeeded and there is no stream to read.
func (c *APIClient) DeleteShedWithProgress(name string, onProgress func(backend.ProgressEvent)) error {
	ctx, cancel := context.WithTimeout(context.Background(), c.createTimeout)
	defer cancel()

	// The streaming client (no client-level timeout, context deadline only)
	// bypasses doRequest, so replicate its send + reactive re-auth here.
	// Rebuilding per send is fine — a DELETE has no body. (An earlier version of
	// this comment claimed create was covered by its GetInfo pre-flight: it is
	// not. /api/info is bootstrap-EXEMPT in token mode, so it answers 200 with a
	// stale credential and can never trigger a re-mint. Create wires its own
	// send through sendWithReauth for exactly that reason.)
	client := c.newHTTPClient(0)
	send := func(cred clienttoken.Credential) (*http.Response, error) {
		httpReq, err := http.NewRequestWithContext(ctx, http.MethodDelete, c.baseURL+"/api/sheds/"+name, nil)
		if err != nil {
			return nil, fmt.Errorf("failed to create request: %w", err)
		}
		httpReq.Header.Set("Accept", "text/event-stream")
		httpReq = pinCredential(httpReq, cred)
		return client.Do(httpReq)
	}
	// The deadline reads as a timeout, not a connection failure — that wording
	// predates mtls and is kept.
	wrapSendErr := func(err error) error {
		if ctx.Err() == context.DeadlineExceeded {
			return fmt.Errorf("request timed out after %v (use --timeout to increase)", c.createTimeout)
		}
		return connectFailure(err)
	}

	resp, err := c.sendWithReauth(send, wrapSendErr)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	// Old server (no delete-SSE) returns a plain 204 — the delete succeeded and
	// there is no stream to read.
	if resp.StatusCode == http.StatusNoContent {
		return nil
	}
	if resp.StatusCode != http.StatusOK {
		return c.parseError(resp)
	}
	// A 200 without the SSE content type is a non-streaming responder (an old or
	// proxied plain delete that the pre-SSE client also accepted) — the delete
	// succeeded; don't feed a non-SSE body to the stream reader. Otherwise read
	// the SSE stream, ignoring the benign terminal "complete" payload.
	if !strings.Contains(resp.Header.Get("Content-Type"), "text/event-stream") {
		return nil
	}
	_, err = c.readSSEStream(resp.Body, onProgress)
	return err
}

// StartShed starts a stopped shed.
func (c *APIClient) StartShed(name string) (*config.Shed, error) {
	var shed config.Shed
	if err := c.doRequestWithTimeout(http.MethodPost, "/api/sheds/"+name+"/start", nil, &shed, c.createTimeout); err != nil {
		return nil, err
	}
	return &shed, nil
}

// StopShed stops a running shed.
func (c *APIClient) StopShed(name string) (*config.Shed, error) {
	var shed config.Shed
	if err := c.doRequest(http.MethodPost, "/api/sheds/"+name+"/stop", nil, &shed); err != nil {
		return nil, err
	}
	return &shed, nil
}

// ResetShed resets the per-shed writable upper layer.
func (c *APIClient) ResetShed(name string) (*config.Shed, error) {
	var shed config.Shed
	if err := c.doRequest(http.MethodPost, "/api/sheds/"+name+"/reset", nil, &shed); err != nil {
		return nil, err
	}
	return &shed, nil
}

// ListSessions retrieves all tmux sessions in a shed. Returns the full
// SessionsResponse: like ListAllSessions, the warnings field carries per-shed
// rc-enrichment degradations (e.g. a slow guest probe) that callers should
// surface rather than silently rendering un-enriched rows.
func (c *APIClient) ListSessions(shedName string) (*config.SessionsResponse, error) {
	var resp config.SessionsResponse
	if err := c.doRequest(http.MethodGet, "/api/sheds/"+shedName+"/sessions", nil, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

// ListAllSessions retrieves all tmux sessions across all sheds.
// Returns the full SessionsResponse including any warnings about sheds that couldn't be queried.
func (c *APIClient) ListAllSessions() (*config.SessionsResponse, error) {
	var resp config.SessionsResponse
	if err := c.doRequest(http.MethodGet, "/api/sessions", nil, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

// KillSession terminates a tmux session in a shed.
func (c *APIClient) KillSession(shedName, sessionName string) error {
	path := fmt.Sprintf("/api/sheds/%s/sessions/%s", shedName, sessionName)
	return c.doRequest(http.MethodDelete, path, nil, nil, http.StatusNoContent, http.StatusOK)
}

// imageIdentURL builds the request URL for an endpoint that targets a single
// image by identifier (a Docker ref, digest, or cosmetic tag). A Docker ref
// contains slashes, which can't ride in a single URL path segment (the
// server's chi {name} param stops at the first '/'), so slash-bearing
// identifiers are passed as a ?ref= query. Slash-free identifiers (digests,
// tags) keep the path form so a newer CLI still drives an older server.
func imageIdentURL(collection, ident string) string {
	if strings.Contains(ident, "/") {
		return collection + "?ref=" + url.QueryEscape(ident)
	}
	return collection + "/" + ident
}

// DeleteImage removes an image's addressability (Docker model). The blob is
// GC'd by PruneImages. ident may be a Docker ref, a digest, or a tag label.
func (c *APIClient) DeleteImage(ident string) error {
	return c.doRequest(http.MethodDelete, imageIdentURL("/api/images", ident), nil, nil, http.StatusNoContent, http.StatusOK)
}

// InspectImage returns full details for a ref, tag, or digest.
func (c *APIClient) InspectImage(ident string) (*config.ImageInspectResponse, error) {
	var resp config.ImageInspectResponse
	if err := c.doRequest(http.MethodGet, imageIdentURL("/api/images/inspect", ident), nil, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

// TagImage points newTag at the digest currently held by srcTagOrDigest.
func (c *APIClient) TagImage(src, dst string) error {
	body := config.ImageTagRequest{Source: src, Target: dst}
	return c.doRequest(http.MethodPost, "/api/images/tag", body, nil, http.StatusNoContent, http.StatusOK)
}

// PullImage pulls a Docker reference into the blob store under the named tag.
// platform is an optional override (e.g. "linux/arm64"); withLayers pulls the
// full image (false = boot-only, the default).
func (c *APIClient) PullImage(dockerRef, tag, platform string, withLayers bool) (*config.ImagePullResponse, error) {
	body := config.ImagePullRequest{DockerRef: dockerRef, Tag: tag, Platform: platform, WithLayers: withLayers}
	var resp config.ImagePullResponse
	if err := c.doRequestWithTimeout(http.MethodPost, "/api/images/pull", body, &resp, 30*time.Minute); err != nil {
		return nil, err
	}
	return &resp, nil
}

// PullImageWithProgress pulls a Docker reference and streams per-stage
// progress via SSE (mirrors CreateShedWithProgress). Falls back to the
// non-streaming PullImage path only if the server rejects the stream.
func (c *APIClient) PullImageWithProgress(dockerRef, tag, platform string, withLayers, wantBlobProgress bool, onProgress func(backend.ProgressEvent)) (*config.ImagePullResponse, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()

	bodyData, err := json.Marshal(config.ImagePullRequest{DockerRef: dockerRef, Tag: tag, Platform: platform, WithLayers: withLayers})
	if err != nil {
		return nil, fmt.Errorf("failed to encode request: %w", err)
	}
	// Opt into structured per-blob byte events only when the caller can
	// render them (interactive TTY). Older servers ignore the param and a
	// non-opted-in request keeps today's plain line stream.
	url := c.baseURL + "/api/images/pull"
	if wantBlobProgress {
		url += "?progress=blob"
	}
	// Same shape as CreateShedWithProgress: a streaming send routed through the
	// shared re-auth policy rather than a bare client.Do.
	pullClient := c.newHTTPClient(0)
	send := func(cred clienttoken.Credential) (*http.Response, error) {
		httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(bodyData))
		if err != nil {
			return nil, fmt.Errorf("failed to create request: %w", err)
		}
		httpReq.Header.Set("Content-Type", "application/json")
		httpReq.Header.Set("Accept", "text/event-stream")
		httpReq = pinCredential(httpReq, cred)
		return pullClient.Do(httpReq)
	}
	wrapSendErr := func(err error) error {
		if ctx.Err() == context.DeadlineExceeded {
			return fmt.Errorf("pull timed out after 30m")
		}
		return connectFailure(err)
	}

	resp, err := c.sendWithReauth(send, wrapSendErr)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, c.parseError(resp)
	}

	var out config.ImagePullResponse
	// Fall back to the non-streaming path against a server that ignores the
	// Accept header (e.g. a pre-SSE shed-server): it returns a plain JSON
	// ImagePullResponse with Content-Type application/json.
	if !strings.Contains(resp.Header.Get("Content-Type"), "text/event-stream") {
		if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
			return nil, fmt.Errorf("failed to decode pull response: %w", err)
		}
		return &out, nil
	}

	data, err := c.readSSEStream(resp.Body, onProgress)
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, fmt.Errorf("failed to parse complete event: %w", err)
	}
	return &out, nil
}

// PushImage uploads the manifest held by source (tag or digest) to a
// destination registry ref. Byte-perfect.
func (c *APIClient) PushImage(source, destination string) (*config.ImagePushResponse, error) {
	body := config.ImagePushRequest{Source: source, Destination: destination}
	var resp config.ImagePushResponse
	if err := c.doRequestWithTimeout(http.MethodPost, "/api/images/push", body, &resp, 30*time.Minute); err != nil {
		return nil, err
	}
	return &resp, nil
}

// PruneImages removes unused cached images.
// If dryRun is true, returns candidates without deleting.
func (c *APIClient) PruneImages(dryRun bool) (*config.PruneImagesResponse, error) {
	path := "/api/images/prune"
	if dryRun {
		path += "?dry_run=true"
	}
	var resp config.PruneImagesResponse
	if err := c.doRequest(http.MethodPost, path, nil, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

// ListSnapshots returns all snapshots managed by the server.
func (c *APIClient) ListSnapshots() (*config.SnapshotsResponse, error) {
	var resp config.SnapshotsResponse
	if err := c.doRequest(http.MethodGet, "/api/snapshots", nil, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

// GetSnapshot retrieves a snapshot by name.
func (c *APIClient) GetSnapshot(name string) (*config.Snapshot, error) {
	var snap config.Snapshot
	if err := c.doRequest(http.MethodGet, "/api/snapshots/"+name, nil, &snap); err != nil {
		return nil, err
	}
	return &snap, nil
}

// CreateSnapshot creates a new snapshot from a stopped shed.
// Returns the created snapshot and any non-fatal warnings the backend emitted
// during the operation (e.g., "--local-dir not captured").
func (c *APIClient) CreateSnapshot(req *config.SnapshotCreateRequest) (*config.Snapshot, []string, error) {
	var resp config.SnapshotCreateResponse
	if err := c.doRequestWithTimeout(http.MethodPost, "/api/snapshots", req, &resp, c.createTimeout, http.StatusCreated, http.StatusOK); err != nil {
		return nil, nil, err
	}
	return resp.Snapshot, resp.Warnings, nil
}

// DeleteSnapshot removes a snapshot from the server.
func (c *APIClient) DeleteSnapshot(name string) error {
	return c.doRequest(http.MethodDelete, "/api/snapshots/"+name, nil, nil, http.StatusNoContent, http.StatusOK)
}

// GetSystemDF retrieves disk usage information for the server.
func (c *APIClient) GetSystemDF() (*config.DiskUsage, error) {
	var du config.DiskUsage
	if err := c.doRequest(http.MethodGet, "/api/system/df", nil, &du); err != nil {
		return nil, err
	}
	return &du, nil
}

// SystemPruneOptions mirrors backend.PruneOptions for the CLI → API path.
// Using a client-local struct avoids pulling the backend package into the
// CLI (which doesn't need the rest of the Backend interface).
type SystemPruneOptions struct {
	Images       bool
	Instances    bool
	Logs         bool
	Orphans      bool
	DryRun       bool
	Until        time.Duration
	LogTailBytes int64
}

// pruneTimeout bounds prune requests. Large fleets can exceed DefaultTimeout;
// use a generous ceiling modeled on create rather than 30s.
const pruneTimeout = 10 * time.Minute

// SystemPrune triggers a prune pass on the server and returns the report.
// Scope flags are added as repeatable `scope=` query params; an empty scope
// (all flags false) lets the server apply its default (images + instances
// + orphans, no logs).
func (c *APIClient) SystemPrune(opts SystemPruneOptions) (*config.PruneReport, error) {
	path := "/api/system/prune"
	q := make([]string, 0, 6)
	if opts.Images {
		q = append(q, "scope=images")
	}
	if opts.Instances {
		q = append(q, "scope=instances")
	}
	if opts.Logs {
		q = append(q, "scope=logs")
	}
	if opts.Orphans {
		q = append(q, "scope=orphans")
	}
	if opts.DryRun {
		q = append(q, "dry_run=true")
	}
	// Always send Until on the wire — a zero value from the user
	// means "prune any age" and must reach the handler, otherwise
	// the handler's 72h default would override their explicit intent.
	// The CLI's cobra default (72h) is still preserved: flag unset →
	// systemPruneFlagUntil == 72h → we send until=72h0m0s explicitly.
	q = append(q, "until="+opts.Until.String())
	if opts.LogTailBytes > 0 {
		q = append(q, fmt.Sprintf("log_tail_bytes=%d", opts.LogTailBytes))
	}
	if len(q) > 0 {
		path += "?" + strings.Join(q, "&")
	}
	var report config.PruneReport
	if err := c.doRequestWithTimeout(http.MethodPost, path, nil, &report, pruneTimeout); err != nil {
		return nil, err
	}
	return &report, nil
}

// Ping checks if the server is reachable.
func (c *APIClient) Ping() bool {
	resp, err := c.newHTTPClient(2 * time.Second).Get(c.baseURL + "/api/info")
	if err != nil {
		return false
	}
	resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}

// parseError extracts the error message from an API error response.
func (c *APIClient) parseError(resp *http.Response) error {
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("server returned status %d", resp.StatusCode)
	}

	var apiErr config.APIError
	if err := json.Unmarshal(body, &apiErr); err != nil {
		// If not a structured error, return the body as-is
		return fmt.Errorf("server returned status %d: %s", resp.StatusCode, string(body))
	}

	return fmt.Errorf("%s: %s", apiErr.Error.Code, apiErr.Error.Message)
}
