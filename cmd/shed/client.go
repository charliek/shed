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
	"io/fs"
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
	sdkbootstrap "github.com/charliek/shed/sdk/bootstrap"
)

// DefaultTimeout for quick API operations (list, stop, delete, etc.)
const DefaultTimeout = 30 * time.Second

// APIClient provides methods for interacting with the shed server API.
type APIClient struct {
	baseURL       string
	httpClient    *http.Client
	transport     http.RoundTripper // the refusal gate over the pinned (or default) transport; shared by every client below
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
	// held is the user this client's refresh closure is held to (nil for a
	// client with no refresh). It changes when the closure adopts its first
	// named user, so a consumer that builds its own refresh from this client
	// (the tunnel daemon) reads it here rather than from a stale entry.
	held *heldUser
}

// heldUserName returns the user this client is held to now: what its refresh
// closure holds, or the entry's user (fallback) for a client with no refresh.
func (c *APIClient) heldUserName(fallback string) string {
	if c.held == nil {
		return fallback
	}
	return c.held.current()
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
	if t, ok := c.transport.(interface{ CloseIdleConnections() }); ok {
		t.CloseIdleConnections()
	}
}

// refusalGate is the backstop of the refused-entry rule (plan 037 §3.1): every
// request path checks the credential source's refusal before it sends (see
// sendWithReauth), and this RoundTripper checks it again on every round trip,
// so a path that bypassed that check, or a refusal recorded after that check
// and before this one, still never dials a refused entry's server. Every
// APIClient has one; next is the pinned transport, or the default transport
// for a plain-HTTP client.
//
// THE CONTRACT IS PER ATTEMPT, and no stronger: an attempt that passed this
// check before the refusal was recorded may complete (its dial, or its send
// over a pooled connection already authenticated); no attempt starts after
// the refusal is recorded. Nothing here coordinates admission with a
// concurrent refusal.
type refusalGate struct {
	src  *clienttoken.Source
	next http.RoundTripper
}

func (g *refusalGate) RoundTrip(req *http.Request) (*http.Response, error) {
	if err := g.src.Err(); err != nil {
		if req.Body != nil {
			_ = req.Body.Close()
		}
		return nil, err
	}
	return g.next.RoundTrip(req)
}

// CloseIdleConnections forwards to the pinned transport (see
// APIClient.closeIdleConnections). The process-wide default transport of an
// unpinned client is not this client's own pool, so it is left alone, as it
// was before the gate wrapped it.
func (g *refusalGate) CloseIdleConnections() {
	if g.next == http.DefaultTransport {
		return
	}
	if t, ok := g.next.(interface{ CloseIdleConnections() }); ok {
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
//
// It is the one place an APIClient's HTTP clients are made, and none of them
// follows a redirect (servertls.RefuseRedirects): a 3xx is the answer, a
// non-success status the request path reports, and the request's credential
// never reaches the Location it names.
func (c *APIClient) newHTTPClient(timeout time.Duration) *http.Client {
	return &http.Client{Timeout: timeout, Transport: c.transport, CheckRedirect: servertls.RefuseRedirects}
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
// client certificate from it per handshake. An empty fingerprint yields no
// pinned transport — the plain-HTTP path, on the default transport.
//
// Every client's transport, pinned or not, is the refusal gate (refusalGate):
// a source can become refused at any time (a refresh refused mid-command), and
// a path that bypasses sendWithReauth must still never dial for it.
//
// Order matters here. The source is created BEFORE the transport and never
// swapped afterwards, so the GetClientCertificate closure has a stable target
// and no request can observe a half-installed credential.
func newAPIClientWithSource(baseURL, tlsFingerprint string, src *clienttoken.Source, createTimeout time.Duration) *APIClient {
	next := http.DefaultTransport
	if pinned := servertls.PinnedTransport(tlsFingerprint, src.CertificateFor); pinned != nil {
		next = pinned
	}
	transport := &refusalGate{src: src, next: next}
	c := &APIClient{
		baseURL:       strings.TrimRight(baseURL, "/"),
		transport:     transport,
		createTimeout: createTimeout,
		tokens:        src,
	}
	c.httpClient = c.newHTTPClient(DefaultTimeout)
	return c
}

// NewAPIClientFromEntry creates an API client from an entry whose config name is
// not available to the caller. It preserves the historical single-match
// behavior by inferring the name from the endpoint. When there is no unique
// match, refresh still works for this client's lifetime but is not persisted.
//
// Callers that already resolved a server name must use
// NewAPIClientFromNamedEntry instead. Endpoint matching cannot distinguish two
// aliases for the same server, while the selected/map-key name can.
func NewAPIClientFromEntry(entry *config.ServerEntry, createTimeout time.Duration) *APIClient {
	name, matches := matchingServerName(entry)
	reason := ""
	switch {
	case matches == 0:
		reason = "no configured server matches this entry"
	case matches > 1:
		reason = "multiple configured server aliases match this endpoint"
	}
	display := name
	if display == "" {
		display = entry.Host
	}
	return newAPIClientFromEntry(entry, name, display, reason, createTimeout)
}

// NewAPIClientFromNamedEntry creates an API client for the named config entry.
// name is authoritative: it is the alias selected by --server/default
// resolution or the map key being iterated, so duplicate endpoint aliases still
// persist refreshed credentials to the entry the command is actually using.
//
// The endpoint identity is checked against the current config before name is
// trusted. A stale or incorrect caller must never write a credential to the
// wrong alias; on mismatch the client remains usable in memory and a successful
// mint reports the persistence skip under -v.
func NewAPIClientFromNamedEntry(name string, entry *config.ServerEntry, createTimeout time.Duration) *APIClient {
	persistName, reason := verifiedServerName(name, entry)
	display := name
	if display == "" {
		display = entry.Host
	}
	return newAPIClientFromEntry(entry, persistName, display, reason, createTimeout)
}

// newAPIClientFromEntry builds the client for an entry. displayName is the
// entry's name as the user knows it, for the messages that name it (it may be
// set when persistName is not: a name that no longer verifies).
//
// An entry whose stored certificate is refused (config.LoadEntryCertificate)
// gets a REFUSED client: its credential source is terminal, every request
// returns the refusal before anything is dialled, and nothing runs ssh. The
// ways back (`shed server update <name> --refetch`, `shed server remove`)
// build no client from the entry.
//
// THE USER IS READ FROM THE ROW ON DISK, here, the one point every client is
// built through: a caller's copy of the entry can be older than the row (a
// lookup that built a client which enrolled the entry's first user hands back
// the copy it started from, and console, exec, attach, sync and tunnels then
// build another client from it). Held to that stale user, the new client would
// refuse the very certificate the first one just recorded.
func newAPIClientFromEntry(entry *config.ServerEntry, persistName, displayName, unpersistedReason string, createTimeout time.Duration) *APIClient {
	current := *entry
	current.User = recordedUser(persistName, entry)
	entry = &current
	initial, refreshable, refusal := entryCredential(entry, persistName, displayName)
	if refusal != nil {
		return newAPIClientWithSource(entry.BaseURL(), entry.TLSCertFingerprint, clienttoken.Refused(refusal), createTimeout)
	}
	if !refreshable {
		// Static/legacy token or open server — no refresh wiring, exactly as
		// before client certificates existed.
		return newAPIClient(entry.BaseURL(), entry.ControlToken, entry.TLSCertFingerprint, createTimeout)
	}
	held := &heldUser{name: entry.User}
	refresh := controlCredentialRefreshHeld(*entry, persistName, persistName, displayName, held)
	if unpersistedReason != "" {
		refresh = observableMintOnlyRefresh(entry.Host, unpersistedReason, refresh)
	}
	src := clienttoken.New(initial, refresh)
	c := newAPIClientWithSource(entry.BaseURL(), entry.TLSCertFingerprint, src, createTimeout)
	c.held = held
	// Proactively re-mint a near-expiry credential so a request never races
	// expiry. A mint failure here is non-fatal (EnsureFresh keeps the stale
	// credential); the reactive retry surfaces any error on the next request.
	// A refusal from this mint ends the client unless it holds a certificate
	// still valid for the entry's user, which it keeps using without minting
	// again (clienttoken.Source.EnsureFreshErr).
	src.EnsureFresh()
	return c
}

// recordedUser is the user the named row records on disk now, read under the
// config lock (one small file read): the row's user when it still names
// entry's endpoint, else entry.User (no name, the row is gone or repointed, or
// the file cannot be read — a persisting refresh then finds that under its own
// lock and fails closed).
func recordedUser(name string, entry *config.ServerEntry) string {
	user := entry.User
	if name == "" {
		return user
	}
	_ = readClientConfigLocked(func(c *config.ClientConfig) error {
		if stored, ok := c.Servers[name]; ok && sameServerEndpoint(stored, *entry) {
			user = stored.User
		}
		return nil
	})
	return user
}

// observableMintOnlyRefresh makes an accidental persistence skip visible while
// preserving the best-effort contract: the freshly minted credential is still
// returned and the command still succeeds. The tunnel daemon deliberately uses
// controlCredentialMintOnly directly, without this wrapper, because its
// mint-only policy is intentional rather than a name-resolution failure.
func observableMintOnlyRefresh(host, reason string, refresh func() (clienttoken.Credential, error)) func() (clienttoken.Credential, error) {
	return func() (clienttoken.Credential, error) {
		cred, err := refresh()
		if err == nil && verboseLevel > 0 {
			fmt.Fprintf(os.Stderr, "warning: refreshed credential for %s was not persisted: %s\n", host, reason)
		}
		return cred, err
	}
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
//
// The one stored state that is NOT recoverable is a certificate whose user the
// entry may not use (config.LoadEntryCertificate): it names no user, or a user
// the entry does not record. That returns refusal, and the caller builds a
// refused client that sends nothing. So does an identity_file that cannot be
// used when the stored pair is checked against it (its typed
// *config.IdentityFileError; when the entry holds no certificate, the
// enrolment EnsureFresh runs before the first request meets the same refusal
// and ends the client), and a stored control token on an entry with an
// identity_file
// (config.RefusedStoredTokenForKey: a token names no key, so it is never
// presented for such an entry). A stored certificate made for another SSH key
// than the entry's identity_file is the recoverable case: it is not presented
// and the entry enrols again with its identity file.
func entryCredential(entry *config.ServerEntry, name, displayName string) (cred clienttoken.Credential, refreshable bool, refusal error) {
	if entry.IsMTLS() {
		cert, err := loadClientCert(entry, name)
		if r, ok := config.AsEntryRefused(err); ok {
			r.Entry = displayName
			return clienttoken.Credential{}, false, r
		}
		if ie, ok := config.NameIdentityFileError(displayName, err); ok {
			// The entry's identity_file cannot be used now, so neither its
			// stored certificate (whose key cannot be checked) nor an enrolment
			// (which would fail the same way) may be: fail closed, nothing is
			// presented and nothing is dialled.
			return clienttoken.Credential{}, false, ie
		}
		if err != nil && verboseLevel > 0 {
			fmt.Fprintf(os.Stderr, "warning: could not load client certificate for %s (re-enrolling): %v\n", entry.Host, err)
		}
		if cert != nil {
			return clienttoken.MTLSCredential(cert, entry.ClientCertExpiresAt), true, nil
		}
		// No usable certificate — refreshable, holding nothing. EnsureFresh
		// enrolls on that (Credential.Usable), so no fake expiry is needed.
		// An identity_file that cannot be used fails that enrolment with its
		// *config.IdentityFileError, a refusal, which ends the source before
		// any request: nothing is sent without a certificate first.
		return clienttoken.Credential{Mode: clienttoken.ModeMTLS}, true, nil
	}
	if entry.IdentityFile != "" && entry.ControlToken != "" {
		// A control token names no SSH key, so an entry held to its identity
		// file never presents one, stored or newly issued (plan 037 P-a): the
		// entry is refused and nothing is dialled with the token.
		return clienttoken.Credential{}, false, config.StoredTokenRefusal(displayName, entry)
	}
	if entry.ControlTokenExpiresAt.IsZero() {
		// Either an open server / legacy static token (static, as always), or a
		// secure entry stripped of its credential (enroll — holding nothing, so
		// EnsureFresh mints before the first request).
		return clienttoken.Credential{}, entry.NeedsEnrollment(), nil
	}
	return clienttoken.TokenCredential(entry.ControlToken, entry.ControlTokenExpiresAt), true, nil
}

// loadClientCert reads the entry's client certificate + key off disk, under the
// named server's credential lock so a concurrent rotation is never observed
// half-committed, and checks the user it names against the entry's
// (config.LoadEntryCertificate). A missing path or an unreadable/mismatched
// pair returns (nil, err) — see entryCredential for why that is recoverable
// rather than fatal; a certificate for a user the entry may not use returns a
// *config.EntryRefusedError, which is not. name may be "" (an ambiguous or
// one-off entry), which loads unlocked: nothing writes that entry either, so
// there is nothing to serialize against.
func loadClientCert(entry *config.ServerEntry, name string) (*tls.Certificate, error) {
	return config.LoadEntryCertificate(name, entry)
}

// controlCredentialRefresh returns a refresh callback that mints a control
// credential over the SSH bootstrap channel and, when persistName != "",
// best-effort persists it to that config entry. It is shared by
// the entry-based API clients (persisting) and the tunnel daemon (persistName "" —
// mint-only, so a long-lived daemon never rewrites the user's config from its
// own stale in-memory copy).
//
// The bootstrap always submits a CSR, so the server answers in whatever mode it
// is actually in. Whichever comes back is adopted here AND written to the
// stored entry — that write is the mode-flip migration, and it runs in both
// directions.
//
// endpoint is the entry this client was built from, carried through so the
// persist can re-verify — under the config lock, against the config as it is at
// persist time — that persistName still names THIS endpoint. Host and ssh port
// alone would not be enough for that check, and by the time a mint completes
// the name may have been removed or repointed by another process.
//
// THE HELD USER (plan 037 §3.1). An issued certificate is adopted only when it
// names the user this entry is held to, or the implicit user, or the entry is
// held to no one yet; a certificate that names no user, or whose user cannot
// be read (including one the SDK's bootstrap rejects as unusable), is refused
// too. A refusal writes nothing and adopts nothing, and is returned as a
// *config.EntryRefusedError, which ends the client's credential source
// (clienttoken.Refusal) unless it may keep a certificate it holds. A
// persisting refresh decides against the row on disk, under the locks
// (persistMTLSCredential). displayName names the entry in those messages.
//
// With persistName "" the refresh writes nothing (mint-only), and decides as
// adoptMTLSCredential says: against the entry's row when it has a name
// (controlCredentialMintOnly, the tunnel daemon), else in memory.
func controlCredentialRefresh(endpoint config.ServerEntry, persistName, displayName string) func() (clienttoken.Credential, error) {
	return controlCredentialRefreshHeld(endpoint, persistName, persistName, displayName, &heldUser{name: endpoint.User})
}

// controlCredentialMintOnly is the tunnel daemon's refresh: mint-only (it
// never writes the entry), and decided at every mint against the entry's row
// named rowName, read under the config lock (adoptMTLSCredential).
func controlCredentialMintOnly(endpoint config.ServerEntry, rowName string) func() (clienttoken.Credential, error) {
	return controlCredentialRefreshHeld(endpoint, "", rowName, rowName, &heldUser{name: endpoint.User})
}

// controlCredentialRefreshHeld is controlCredentialRefresh with the held user
// supplied, so the client that owns the closure can read whom it is held to
// at any moment (APIClient.heldUserName: the tunnel daemon's hand-off).
// rowName is the entry whose row decides the held user: persistName for a
// persisting refresh, the entry's name for a named mint-only one, "" for none.
func controlCredentialRefreshHeld(endpoint config.ServerEntry, persistName, rowName, displayName string, held *heldUser) func() (clienttoken.Credential, error) {
	return func() (clienttoken.Credential, error) {
		// The entry's identity_file, when set, is the key ssh offers first; a
		// value or file that cannot be used fails here, before ssh is spawned,
		// and entryKey is its public key, which the issued certificate must
		// have been made for.
		res, entryKey, err := bootstrapEntryCredential(displayName, endpoint.Host, endpoint.SSHPort, endpoint.IdentityFile)
		var unusable *sdkbootstrap.UnusableCertificateError
		if errors.As(err, &unusable) {
			// The server answered with a certificate the SDK cannot use (none,
			// not PEM, DER that does not parse, another key's certificate): an
			// unreadable issuance, decided like any other, under the same rule.
			return adoptMTLSCredential(sdk.Credential{}, unusable, entryKey, persistName, rowName, displayName, endpoint, held)
		}
		if err != nil {
			return clienttoken.Credential{}, err
		}
		if res.Mode() == sdk.AuthModeMTLS {
			return adoptMTLSCredential(res, nil, entryKey, persistName, rowName, displayName, endpoint, held)
		}
		if endpoint.IdentityFile != "" {
			// A control token says nothing of the key it was issued for, so an
			// entry held to its identity file never uses one (plan 037 P-a).
			// Nothing is written or adopted.
			return clienttoken.Credential{}, &config.EntryRefusedError{Entry: displayName,
				Kind: config.RefusedIssuedTokenForKey, IdentityFile: endpoint.IdentityFile}
		}
		if persistName != "" {
			// Best-effort: a Save failure never wastes the mint or fails the
			// request (the next process just re-mints).
			persistTokenCredential(persistName, endpoint, res.Bundle.Token, res.Bundle.ExpiresAt)
		}
		return clienttoken.TokenCredential(res.Bundle.Token, res.Bundle.ExpiresAt), nil
	}
}

// adoptMTLSCredential turns a freshly issued certificate into a usable
// credential and, when the entry is unambiguous, persists the pair to the creds
// dir + the config entry.
//
// A persisting refresh (persistName set) decides everything in ONE transaction
// against the row on disk, under the server's credential lock
// (persistMTLSCredential): every refusal included, so a refusal that lets the
// client keep the certificate it holds is only ever returned after a positive
// check, under that lock, that the row still records the user it holds. A
// certificate that would record the entry's first user is used only once the
// row records it. A persist failure is a warning only when the row already
// records the certificate's user (a renewal): the in-memory certificate is used
// and the next process re-enrols.
//
// A mint-only refresh (persistName "") writes nothing. When it has the entry's
// name (rowName, the tunnel daemon) it reads the row under the config lock at
// every mint and decides exactly as a persisting refresh does
// (decideAgainstRow): a row that no longer names the endpoint or cannot be
// read, or that records another user than the one held, ends the source; a
// certificate for a user the row does not record is refused. It never
// establishes a first named user in memory: a named certificate for an entry
// whose row records none is refused (config.RefusedMintOnlyUnrecorded, whose
// remedy is a refetch and a restart). Only a source with no entry name (an
// ambiguous alias) decides in memory, against the user this closure is held
// to, keeping the first named user it adopts.
//
// unusable, when set, is the SDK's rejection of the issued certificate
// (sdkbootstrap.UnusableCertificateError): res is then empty, and the
// issuance is refused as unreadable through the same decision.
//
// entryKey is the fingerprint of endpoint's identity_file, learned before the
// bootstrap ran ("" without one). With an identity file, a certificate made
// for any other SSH key (config.IssuedCertificateKey) is refused through the
// same decision as every other refused issuance: nothing is adopted or
// written, and the client keeps only a certificate it already holds for the
// user the row still records (plan 037 P-a).
func adoptMTLSCredential(res sdk.Credential, unusable error, entryKey, persistName, rowName, displayName string,
	endpoint config.ServerEntry, held *heldUser) (clienttoken.Credential, error) {
	var (
		user     string
		implicit bool
		issued   *config.EntryRefusedError
	)
	if unusable != nil {
		issued = &config.EntryRefusedError{Entry: displayName, Kind: config.RefusedIssuedUnreadable, Cause: unusable}
	} else {
		user, implicit, issued = config.IssuedCertificateUser(displayName, []byte(res.Bundle.ClientCert))
		if issued == nil {
			issued = config.IssuedCertificateKey(displayName, endpoint.Host, endpoint.IdentityFile, entryKey,
				[]byte(res.Bundle.ClientCert))
		}
	}
	var cert tls.Certificate
	if issued == nil {
		var err error
		if cert, err = tls.X509KeyPair([]byte(res.Bundle.ClientCert), res.KeyPEM); err != nil {
			return clienttoken.Credential{}, fmt.Errorf("assemble issued client certificate: %w", err)
		}
	}
	// Held across the whole adoption, so two refreshes in one process cannot
	// both adopt from an unheld entry with different users.
	held.mu.Lock()
	defer held.mu.Unlock()
	switch {
	case persistName != "":
		recorded, err := persistMTLSCredential(persistName, endpoint, res, entryKey, user, implicit, issued, held.name)
		if err != nil {
			return clienttoken.Credential{}, err
		}
		held.name = recorded
	case rowName != "":
		recorded, err := decideMintOnly(rowName, endpoint, entryKey, user, implicit, issued, held.name)
		if err != nil {
			return clienttoken.Credential{}, err
		}
		held.name = recorded
	default:
		// No row to read: the key is learned again from the entry's
		// identity_file now, and must still be the one the enrolment used.
		if err := enrolledIdentityHolds(displayName, endpoint, endpoint, entryKey); err != nil {
			return clienttoken.Credential{}, err
		}
		if issued != nil {
			return clienttoken.Credential{}, issued
		}
		if !config.HeldUserAccepts(held.name, user, implicit) {
			return clienttoken.Credential{}, &config.EntryRefusedError{
				Entry: displayName, Kind: config.RefusedIssuedUserChange, CertUser: user, EntryUser: held.name}
		}
		if !implicit && held.name == "" {
			held.name = user
		}
	}
	if implicit && held.name != "" && verboseLevel > 0 {
		fmt.Fprintf(os.Stderr, "note: %s issued a certificate for its only user; this entry stays held to %q\n",
			displayName, held.name)
	}
	return clienttoken.MTLSCredential(&cert, res.Bundle.ExpiresAt), nil
}

// heldUser is the user one refresh closure is held to: the entry's `user` when
// the closure was built, then the first named user it adopts (or, for a
// persisting refresh, the user the row on disk records once it has decided).
type heldUser struct {
	mu   sync.Mutex
	name string
}

// current returns the user held now.
func (h *heldUser) current() string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.name
}

// configMu serializes access to the shared clientConfig global WITHIN this
// process — the server-name lookups (matchingServerName, verifiedServerName)
// and every mutation. The `--all` fan-out (forEachServer) constructs clients,
// and thus refreshes, concurrently across goroutines; without this, two
// near-expiry refreshes would race on the Servers map (a fatal "concurrent map
// writes").
//
// It says nothing about other PROCESSES. Cross-process correctness is
// ClientConfig.Update's file lock (internal/config): every write in this
// package goes through updateClientConfig, which takes configMu and then calls
// Update. The two mutexes are layered, not alternatives — configMu makes reads
// and writes mutually exclusive in-process, Update's file lock makes the
// read-modify-write of config.yaml atomic against a second `shed`.
var configMu sync.Mutex

// updateClientConfig is the single write path for the shared clientConfig: it
// holds configMu (against this process's concurrent readers) across
// ClientConfig.Update (which holds the config FILE lock against other
// processes, loads a fresh on-disk snapshot, saves it, and only then re-applies
// the mutation in memory).
//
// Every mutation closure must be a deterministic function of the config it is
// handed — Update runs it twice, once on the fresh snapshot and once on the
// in-memory one — so it must not read the clientConfig global or accumulate
// state. It also cannot fail: preconditions go through
// updateClientConfigChecked, whose check runs once, under the lock, against the
// on-disk snapshot.
//
// LOCK ORDERING: a caller that also needs a server's credential lock
// (config.LockServerCredentials) takes THAT first. Nothing reached from here
// touches the credential store, so the order can never invert.
func updateClientConfig(mutate func(*config.ClientConfig)) error {
	configMu.Lock()
	defer configMu.Unlock()
	return clientConfig.Update(mutate)
}

// updateClientConfigChecked is updateClientConfig with a precondition tested
// against the FRESH on-disk snapshot inside the lock.
//
// Every "does this entry still exist / still mean what I think" question in
// this package goes here rather than being asked before the call. Asked
// earlier, it is answered from a snapshot that another `shed` process can
// invalidate before the write lands — which is how two concurrent adds of one
// name both succeed, and how a set-default survives the removal of its target.
func updateClientConfigChecked(check func(*config.ClientConfig) error, mutate func(*config.ClientConfig)) error {
	configMu.Lock()
	defer configMu.Unlock()
	return clientConfig.UpdateChecked(check, mutate)
}

// cacheShedLocation records where a shed lives, through the locked write path.
// The create/start/stop commands all end with exactly this one-line mutation.
//
// The timestamp is taken ONCE and passed in: Update applies the mutation to the
// fresh on-disk snapshot and then to the in-memory one, so a CacheShed that
// stamped time.Now() itself would leave the file and the running process
// holding rows that differ in updated_at for no reason anyone chose.
func cacheShedLocation(shedName, serverName, status string) error {
	now := time.Now()
	return updateClientConfig(func(c *config.ClientConfig) {
		c.CacheShedAt(shedName, serverName, status, now)
	})
}

// listingCache collects what the servers that answered a shed listing said,
// for one locked cache update after every listing is in. A server that did not
// answer (unreachable, refused) is never recorded, so its cached rows are left
// alone: no answer is not "no sheds".
type listingCache struct {
	// answered maps each server that answered to the names it listed (an
	// empty, non-nil set for a server that answered with no sheds).
	answered map[string]map[string]struct{}
	rows     []listingCacheRow
}

type listingCacheRow struct{ name, server, status string }

// answer records one server's successful listing.
func (l *listingCache) answer(server string, sheds []config.Shed) {
	if l.answered == nil {
		l.answered = make(map[string]map[string]struct{})
	}
	names := make(map[string]struct{}, len(sheds))
	for _, sh := range sheds {
		names[sh.Name] = struct{}{}
		l.rows = append(l.rows, listingCacheRow{name: sh.Name, server: server, status: sh.Status})
	}
	l.answered[server] = names
}

// apply drops the cached rows each answering server no longer lists, then
// caches every listed shed at at. It is a pure function of the collected
// listings and the config it is handed, so it is safe as an Update mutation
// (which runs it against the fresh on-disk snapshot and then the in-memory
// one).
func (l *listingCache) apply(c *config.ClientConfig, at time.Time) {
	for server, names := range l.answered {
		c.PruneShedCache(server, names)
	}
	for _, r := range l.rows {
		c.CacheShedAt(r.name, r.server, r.status, at)
	}
}

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
//
// LOCK ORDERING — the server's credential lock is taken FIRST and held across
// BOTH the config update and the deletion, then the config file lock is taken
// underneath it (inside saveServerEntry). Holding it across the pair is what
// closes the token↔mtls transition race: a concurrent mtls persist cannot slip
// its own config update between this save and this deletion and end up naming
// files this call is about to remove (see persistMTLSCredential, which takes
// the same lock in the same order). No path anywhere takes a credential lock
// while holding the config lock, so the two can never deadlock.
//
// It is taken unconditionally, before anything is read, rather than only when a
// certificate turns out to need deleting: which snapshot holds one is precisely
// what a concurrent flip can change under us, so deciding first and locking
// second would re-open the window. The cost is an empty lock file under
// ~/.shed/creds for token-only servers, which is inert.
func persistTokenCredential(name string, endpoint config.ServerEntry, token string, expiresAt time.Time) {
	if name == "" {
		return
	}
	unlockCreds, err := config.LockServerCredentials(name)
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: could not lock the credential store for %q: %v\n", name, err)
		return
	}
	defer unlockCreds()

	// True if ANY view of this entry held a certificate: the entry this client
	// was built from (which is what the deletion is cleaning up after, and the
	// only view available when the save fails before it can read the file),
	// plus whichever snapshot saveServerEntry's mutation sees.
	hadCert := endpoint.AuthMode == config.AuthModeMTLS
	saveErr := saveServerEntry(name, "refreshed token", endpoint, func(e *config.ServerEntry) {
		if e.AuthMode == config.AuthModeMTLS {
			hadCert = true
		}
		e.AuthMode = config.AuthModeToken
		e.ControlToken = token
		e.ControlTokenExpiresAt = expiresAt
		e.ClientCertFile, e.ClientKeyFile = "", ""
		e.ClientCertExpiresAt = time.Time{}
		// e.User is kept: the held user survives the flip (ServerEntry.User).
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
	// leave an orphan file — never a config pointing at a deleted one. Remove
	// takes no lock of its own, which is exactly why it is safe to call while
	// holding this one.
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
// It is ONE TRANSACTION under the server's credential lock (plan 037 §3.1),
// with the config lock nested inside it (the documented order):
//
//  1. take the credential lock;
//  2. read the row on disk, under the config lock, and decide: it must still
//     name this endpoint, it must still record the user the calling closure
//     holds (held), and only then is the issued certificate judged: issued
//     (a certificate that names no user, or whose user cannot be read) is
//     refused, and the row's `user` must accept the certificate (empty, the
//     same name, or an implicit certificate);
//  3. on a refusal, return it with nothing written;
//  4. write the files with the lock-held writer;
//  5. save the row (recording `user` when it was empty and the certificate is
//     named), re-checking the same condition under the config lock; a failed
//     save puts the previous files back;
//  6. release.
//
// Two processes that both start from `user: ""` and are issued different
// named users therefore cannot both win: the second finds the first's `user`
// at step 2 and is refused with nothing written.
//
// THE RULE IS ABOUT WHAT A PROCESS ADOPTS, not a re-read of the row before
// every request. A process never adopts a certificate for a user other than
// the one its entry's row records when checked under the lock (here, at
// client construction, and at every mint of a named tunnel daemon), or the
// implicit user of a server with no user list. A certificate it already holds
// stays in use until its next refresh or until the certificate ends,
// whichever is first, even if another process meanwhile changed the entry
// with --allow-user-change; at its next refresh it re-reads the row and stops
// if the row names another user (config.RefusedEntryUserChanged). A binding
// (the row's `user`) is established or changed only by a write that read the
// row under the server's credential lock (this transaction for a first user,
// `--refetch --allow-user-change` for a change), and a certificate that
// establishes one is used only once the row recording it is saved: if the
// read of the previous files, the file write or the save fails, the refresh
// fails (config.RefusedUserNotRecorded) and nothing is adopted. A certificate
// already held may be kept after a refused re-enrolment only when, under that
// lock, the row still positively records the user it is for (an unreadable
// row ends the client with config.RefusedEntryUnreadable), and only until its
// own NotAfter.
//
// WHY THERE IS NO JOURNAL. The certificate pair and
// config.yaml are separate files, and nothing here commits them atomically
// together: a crash, or a kill, between the file write (step 4) and the row
// save (step 5) leaves the new pair beside the old row. That is accepted, on
// one condition the readers enforce: no state this transaction can leave on
// disk is ever used to send a request as a user other than the one the row
// records, and none is silently adopted. Every reader of a pair checks it
// against the row it holds (config.LoadEntryCertificate: the CLI with and
// without a name, the roost provider's inventory): a pair whose halves do not
// match is not presented (the entry re-enrols under this same rule), and a
// certificate for a user the row does not record refuses the entry, sending
// nothing, until `shed server update <name> --refetch`. A journal would only
// turn that refusal into a silent repair, which is the one outcome the held
// user exists to prevent.
//
// issued is the refusal config.IssuedCertificateUser gave the certificate (it
// names no user, or its user cannot be read), or nil; it is returned only
// after step 2's check of held, so it never lets a client keep a certificate
// for a user the row no longer records.
//
// It returns recorded, the user the row records once this transaction decided
// against it. err is a refusal (*config.EntryRefusedError):
// RefusedEntryUnreadable when the lock or the read of the row failed (at the
// first check or at the save's re-check), or the row no longer names this
// endpoint (fail closed: nothing is kept written, and the client ends);
// RefusedRestoreFailed when a failed write or save could not put the previous
// files back; RefusedUserNotRecorded when the certificate would record the
// entry's first user and the snapshot read, the file write or the save
// failed. A refusal from the save's re-check is returned as it is, never
// turned into a warning. Only a failure to WRITE a renewal for the user the
// row already records (the snapshot read, the file write, or the config file
// write) is a warning, the persistTokenCredential contract: the credential is
// usable in memory and the next process re-enrolls.
func persistMTLSCredential(name string, endpoint config.ServerEntry, res sdk.Credential, entryKey, user string, implicit bool,
	issued *config.EntryRefusedError, held string) (recorded string, err error) {
	unlockCreds, err := config.LockServerCredentials(name)
	if err != nil {
		return "", entryUnreadable(name, fmt.Errorf("lock its credential files: %w", err))
	}
	defer unlockCreds()

	// establishes: the row records no user and the certificate names one, so
	// the write records the entry's first user and may not be best effort.
	var establishes bool
	// checked: the save's re-check ran, so a save error after it is a failure
	// to write the file, not to read or check the row.
	var checked bool
	decide := func(c *config.ClientConfig) error {
		checked = true
		var err error
		recorded, establishes, err = decideAgainstRow(c, name, endpoint, entryKey, user, implicit, issued, held)
		return err
	}
	if err := readClientConfigLocked(decide); err != nil {
		if clienttoken.IsRefusal(err) {
			return "", err
		}
		return "", entryUnreadable(name, err)
	}
	notRecorded := func(cause error) error {
		return &config.EntryRefusedError{Entry: name, Kind: config.RefusedUserNotRecorded, CertUser: user, Cause: cause}
	}

	prev, err := storedPairOf(name)
	if err != nil {
		// Nothing written: a pair that could not be put back must not be
		// replaced.
		if establishes {
			return "", notRecorded(err)
		}
		fmt.Fprintf(os.Stderr, "warning: not storing the client certificate for %q: %v\n", name, err)
		return recorded, nil
	}
	certPath, keyPath, err := writeCredentialsHeldFn(name, []byte(res.Bundle.ClientCert), res.KeyPEM)
	if err != nil {
		if rerr := prev.restoreHeld(name); rerr != nil {
			return "", restoreFailed(name, rerr)
		}
		if establishes {
			return "", notRecorded(err)
		}
		fmt.Fprintf(os.Stderr, "warning: could not store client certificate for %q: %v\n", name, err)
		return recorded, nil
	}

	checked = false
	err = updateClientConfigChecked(decide, func(c *config.ClientConfig) {
		stored, ok := c.Servers[name]
		if !ok {
			return // in-memory only; see saveServerEntry
		}
		stored.AuthMode = config.AuthModeMTLS
		stored.ClientCertFile = certPath
		stored.ClientKeyFile = keyPath
		stored.ClientCertExpiresAt = res.Bundle.ExpiresAt
		stored.ControlToken = ""
		stored.ControlTokenExpiresAt = time.Time{}
		if !implicit && stored.User == "" {
			stored.User = user
		}
		c.Servers[name] = stored
	})
	if err != nil {
		if rerr := prev.restoreHeld(name); rerr != nil {
			return "", restoreFailed(name, rerr)
		}
		if clienttoken.IsRefusal(err) {
			// The save's re-check refused (the row was removed, repointed or
			// changed its user or its identity since the first check):
			// returned unchanged.
			return "", err
		}
		if !checked {
			// The row could not be locked or read for the re-check: nothing
			// positively says it still records the user held.
			return "", entryUnreadable(name, err)
		}
		if establishes {
			return "", notRecorded(err)
		}
		fmt.Fprintf(os.Stderr, "warning: could not persist client certificate for %q: %v\n", name, err)
	}
	return recorded, nil
}

// entryUnreadable is the refusal of a transaction that could not read the
// entry's row under the lock (cause: the lock, the read, or a row that no
// longer names the endpoint): it cannot positively check the user held, so it
// fails closed and withdraws what the process holds.
func entryUnreadable(name string, cause error) error {
	return &config.EntryRefusedError{Entry: name, Kind: config.RefusedEntryUnreadable, Cause: cause}
}

// decideAgainstRow is the one decision every certificate adoption for a named
// entry makes against the entry's row as it is on disk (c, read under the
// config lock): the persisting refresh (twice: before writing and at the
// save) and the named mint-only refresh (at every mint).
//
// The row must still name this endpoint (else RefusedEntryUnreadable), still
// hold the identity the enrolment was made with (enrolledIdentityHolds: the
// same identity_file, and the same key learned from it now as entryKey, the
// key learned before the bootstrap; else RefusedEntryUnreadable, or the
// file's *config.IdentityFileError) and still record the user the caller
// holds (held; else RefusedEntryUserChanged). Each of those withdraws what
// the caller holds too. Only then is the issued certificate judged: issued (it
// names no user, or its user cannot be read) is returned, and the row's `user`
// must accept the certificate (HeldUserAccepts; else
// RefusedIssuedUserChange). recorded is the user the row records once this
// decision is made; establishes says the certificate would record the entry's
// first named user (the row records none and the certificate is named).
func decideAgainstRow(c *config.ClientConfig, name string, endpoint config.ServerEntry, entryKey, user string, implicit bool,
	issued *config.EntryRefusedError, held string) (recorded string, establishes bool, err error) {
	if err := checkStillNames(c, name, endpoint); err != nil {
		return "", false, entryUnreadable(name, err)
	}
	stored := c.Servers[name]
	if err := enrolledIdentityHolds(name, stored, endpoint, entryKey); err != nil {
		return "", false, err
	}
	if held != "" && stored.User != held {
		return "", false, &config.EntryRefusedError{Entry: name, Kind: config.RefusedEntryUserChanged,
			CertUser: held, EntryUser: stored.User}
	}
	// The row still records the user held: only now may a refusal of the
	// issued certificate let the client keep what it holds.
	if issued != nil {
		return "", false, issued
	}
	if !config.HeldUserAccepts(stored.User, user, implicit) {
		return "", false, &config.EntryRefusedError{Entry: name, Kind: config.RefusedIssuedUserChange,
			CertUser: user, EntryUser: stored.User}
	}
	if stored.User == "" && !implicit {
		return user, true, nil
	}
	return stored.User, false, nil
}

// enrolledIdentityHolds checks, at the moment an enrolment is decided, that the
// entry still holds the identity the enrolment was made with: stored (the row
// now, or for a source with no row, endpoint itself) has the same
// identity_file as endpoint (sameIdentityFile, the comparison a --refetch
// makes), and the public key learned from that file NOW is entryKey, the one
// learned before the bootstrap ran and that the issued certificate was checked
// against. Another process that set another key, or a key replaced at the
// same path (or a retargeted symlink), fails it: nothing is written or
// adopted, no user is recorded, and the refusal withdraws what the caller
// holds (RefusedEntryUnreadable, the refusal of a row that no longer matches
// what the process started with; or the file's *config.IdentityFileError when
// its key cannot be learned now, so it cannot be shown to be the same). An
// entry with no identity_file on either side reads no file.
func enrolledIdentityHolds(name string, stored, endpoint config.ServerEntry, entryKey string) error {
	if !sameIdentityFile(stored, endpoint) {
		return entryUnreadable(name, errors.New("its identity_file changed while this command ran"))
	}
	if endpoint.IdentityFile == "" {
		return nil
	}
	now, err := config.IdentityFileKey(name, stored.IdentityFile)
	if err != nil {
		return err
	}
	if now != entryKey {
		return entryUnreadable(name, errors.New("the key in its identity_file changed while this command ran"))
	}
	return nil
}

// decideMintOnly is the named mint-only refresh's decision (the tunnel
// daemon): decideAgainstRow against the entry's row, read under the config
// lock at this mint. It writes nothing, so a certificate that would establish
// the entry's first named user is refused (config.RefusedMintOnlyUnrecorded):
// a user the row does not record is never adopted, not even in memory.
func decideMintOnly(name string, endpoint config.ServerEntry, entryKey, user string, implicit bool,
	issued *config.EntryRefusedError, held string) (string, error) {
	var (
		recorded    string
		establishes bool
	)
	err := readClientConfigLocked(func(c *config.ClientConfig) error {
		var err error
		recorded, establishes, err = decideAgainstRow(c, name, endpoint, entryKey, user, implicit, issued, held)
		return err
	})
	if err != nil {
		if clienttoken.IsRefusal(err) {
			return "", err
		}
		return "", entryUnreadable(name, err)
	}
	if establishes {
		return "", &config.EntryRefusedError{Entry: name, Kind: config.RefusedMintOnlyUnrecorded, CertUser: user}
	}
	return recorded, nil
}

// restoreFailed is the error of a transaction whose config save failed and
// whose previous credential files could not be put back: never only a
// warning, because the entry's files and its recorded user may now disagree.
// It is a refusal that withdraws what the process holds.
func restoreFailed(name string, cause error) error {
	return &config.EntryRefusedError{Entry: name, Kind: config.RefusedRestoreFailed, Cause: cause}
}

// writeCredentialsHeldFn and commitStagedHeldFn are the only two writes of a
// server's credential files inside the CLI's write transaction (the caller
// holds that server's credential lock); removeCredentialsHeldFn removes them,
// putBackCredentialFileFn restores one file a failed transaction changed, and
// readCredentialFileFn reads the pair a transaction may have to put back.
// Vars so a test can prove a refused transaction writes nothing at all, and
// can stop or fail a transaction at each step.
var (
	writeCredentialsHeldFn  = config.WriteClientCredentialsHeld
	commitStagedHeldFn      = func(s *config.StagedClientCredentials) error { return s.CommitHeld() }
	removeCredentialsHeldFn = config.RemoveServerCredentials
	putBackCredentialFileFn = config.PutBackClientCredentialFile
	readCredentialFileFn    = os.ReadFile
)

// readClientConfigLocked runs read against the config on disk under configMu
// and the config file lock (ClientConfig.ReadLocked). A caller that holds a
// server's credential lock takes it first.
func readClientConfigLocked(read func(*config.ClientConfig) error) error {
	configMu.Lock()
	defer configMu.Unlock()
	return clientConfig.ReadLocked(read)
}

// storedPair is a server's credential files as they were before a write, so a
// write whose config save then failed can put them back.
type storedPair struct{ cert, key []byte }

// storedPairOf reads the pair at the server's store paths. The caller holds
// the server's credential lock, so what it reads is one committed pair. A file
// that does not exist reads as empty; any other read error is returned, and
// the caller writes nothing (a pair it could not read is a pair it could not
// put back).
func storedPairOf(name string) (storedPair, error) {
	certPath, keyPath := config.ClientCredentialPaths(name)
	var p storedPair
	for _, f := range []struct {
		path string
		into *[]byte
	}{{certPath, &p.cert}, {keyPath, &p.key}} {
		b, err := readCredentialFileFn(f.path)
		switch {
		case err == nil:
			*f.into = b
		case errors.Is(err, fs.ErrNotExist):
		default:
			return storedPair{}, fmt.Errorf("read the current client credentials of %q before replacing them: %w", name, err)
		}
	}
	return p, nil
}

// restoreHeld puts back, under the credential lock the caller holds, each
// destination this transaction actually changed, and nothing else. A
// destination whose bytes are still the previous ones is left alone: a write
// that failed before either destination changed (no space for its temporary
// file, say) leaves the previous pair intact, so this verifies that and
// writes nothing, and the caller treats the failure as the write failure it
// is, not a failed restoration. When there were no files at all, the
// directory goes (everything in it is this transaction's). A destination that
// cannot be read is taken as changed. Its error is never only a warning: the
// caller fails with restoreFailed.
func (p storedPair) restoreHeld(name string) error {
	certPath, keyPath := config.ClientCredentialPaths(name)
	files := []struct {
		path, what string
		was        []byte
	}{{keyPath, "key", p.key}, {certPath, "certificate", p.cert}}
	var changed []int
	for i, f := range files {
		now, err := readCredentialFileFn(f.path)
		if errors.Is(err, fs.ErrNotExist) {
			now, err = nil, nil
		}
		if err != nil || !bytes.Equal(now, f.was) {
			changed = append(changed, i)
		}
	}
	if len(changed) == 0 {
		return nil // the previous pair is intact: nothing to put back
	}
	if len(p.cert) == 0 && len(p.key) == 0 {
		return removeCredentialsHeldFn(name)
	}
	for _, i := range changed {
		if err := putBackCredentialFileFn(files[i].path, files[i].was); err != nil {
			return fmt.Errorf("restore the previous client %s: %w", files[i].what, err)
		}
	}
	return nil
}

// saveServerEntry applies mutate to the named config entry through
// updateClientConfig — configMu in-process, the config file lock across
// processes, mutation applied to a fresh on-disk snapshot and then to the
// in-memory one. Every credential persist goes through here, so the locking
// discipline lives in exactly one place. what names the credential for the
// warning a failed save prints; the failure is never fatal (see
// persistTokenCredential).
//
// endpoint is what the credential was minted against, and the entry is
// RE-VERIFIED against it inside the lock — the same test verifiedServerName
// applies before a client is built, applied again at the only moment that
// matters. Minting is an SSH round-trip; a `shed server rm` or a `shed server
// update` in another process can land inside it. Without the re-check, a
// removed entry would be resurrected by the persist (as a bare credential row
// for a server the user deleted) and a repointed one would be handed a private
// key issued by a different server. Both abort instead: the credential stays
// usable in memory for this command and the next process re-enrolls.
//
// It warns AND returns the error. The warning is the user-facing half and every
// caller relies on it; the return value exists for the one caller that must not
// proceed on a failed save — deleting superseded credential files is only safe
// once the config that stopped referring to them is durable.
func saveServerEntry(name, what string, endpoint config.ServerEntry, mutate func(*config.ServerEntry)) error {
	err := updateClientConfigChecked(
		func(c *config.ClientConfig) error { return checkStillNames(c, name, endpoint) },
		func(c *config.ClientConfig) {
			stored, ok := c.Servers[name]
			if !ok {
				// Only reachable for this process's in-memory snapshot, which
				// the check above did not (and must not) cover. Skipping is
				// right: re-creating the row here would resurrect an entry this
				// process has already dropped.
				return
			}
			mutate(&stored)
			c.Servers[name] = stored
		})
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: could not persist %s for %q: %v\n", what, name, err)
		return err
	}
	return nil
}

// checkStillNames is the precondition every credential persist and every
// entry-editing command shares: the named row must still exist AND still be the
// endpoint the caller resolved. It is written as a check rather than folded
// into a mutation so it runs exactly once, on the on-disk snapshot, under the
// lock — the only instant at which the answer is authoritative.
func checkStillNames(c *config.ClientConfig, name string, endpoint config.ServerEntry) error {
	stored, ok := c.Servers[name]
	if !ok {
		return fmt.Errorf("configured server %q no longer exists", name)
	}
	if !sameServerEndpoint(stored, endpoint) {
		return fmt.Errorf("configured server %q no longer names the endpoint this credential was issued for", name)
	}
	return nil
}

// serverNameForEntry returns the config name whose stored entry UNIQUELY matches
// e by its stable identity (host + ssh port + api_url), or "" when there is no
// match (a one-off entry) or more than one (duplicate aliases to the same
// endpoint). It is the compatibility fallback for NewAPIClientFromEntry;
// callers that possess the authoritative alias use NewAPIClientFromNamedEntry.
//
// ControlToken is deliberately NOT part of the key: the refresh path rewrites
// it, so matching on it would make an entry stop matching its own config row
// after the first re-mint.
func serverNameForEntry(e *config.ServerEntry) string {
	name, _ := matchingServerName(e)
	return name
}

// matchingServerName is serverNameForEntry plus the match count needed to make
// a skipped persistence observable without changing the compatibility helper's
// signature.
func matchingServerName(e *config.ServerEntry) (string, int) {
	configMu.Lock()
	defer configMu.Unlock()
	match := ""
	matches := 0
	for n, se := range clientConfig.Servers {
		if sameServerEndpoint(se, *e) {
			matches++
			match = n
		}
	}
	if matches != 1 {
		return "", matches
	}
	return match, matches
}

// verifiedServerName accepts a caller-resolved alias only while its current
// config row still names the same endpoint. This is the guard against a stale
// entry/name pair persisting a private key under an unrelated alias.
func verifiedServerName(name string, e *config.ServerEntry) (string, string) {
	if name == "" {
		return "", "the caller did not provide a server name"
	}
	configMu.Lock()
	defer configMu.Unlock()
	stored, ok := clientConfig.Servers[name]
	if !ok {
		return "", fmt.Sprintf("configured server %q no longer exists", name)
	}
	if !sameServerEndpoint(stored, *e) {
		return "", fmt.Sprintf("configured server %q no longer matches this endpoint", name)
	}
	return name, ""
}

func sameServerEndpoint(a, b config.ServerEntry) bool {
	return a.Host == b.Host && a.SSHPort == b.SSHPort && a.APIURL == b.APIURL
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
		if clienttoken.IsRefusal(rerr) {
			// A refusal says exactly what is wrong and what fixes it; it is not
			// a failure to re-authenticate, and it has ended the client.
			return clienttoken.Credential{}, false, rerr
		}
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
//
// A REFUSED client (its entry may not be used, plan 037 §3.1) returns the
// refusal, unwrapped, before send runs: no request path makes a connection for
// it. Every request path of APIClient reaches the server through here.
func (c *APIClient) sendWithReauth(send func(clienttoken.Credential) (*http.Response, error), wrapErr func(error) error) (*http.Response, error) {
	if err := c.tokens.Err(); err != nil {
		return nil, err
	}
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
		if refused := c.tokens.Err(); refused != nil && clienttoken.IsRefusal(err) {
			return nil, refused
		}
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
// Like doRequest it re-mints and retries once on an auth-shaped failure; the
// timeout context bounds both attempts.
func (c *APIClient) doRequestWithTimeout(method, path string, body, result interface{}, timeout time.Duration, expectedStatus ...int) error {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	var bodyData []byte
	if body != nil {
		var err error
		bodyData, err = json.Marshal(body)
		if err != nil {
			return fmt.Errorf("failed to encode request: %w", err)
		}
	}

	// Create a client without a Timeout for long-running requests.
	// Important: When both http.Client.Timeout and context deadline are set,
	// the shorter one wins. Since c.httpClient has a 30s timeout, we must use
	// a separate client here to allow the context timeout (potentially minutes)
	// to control cancellation. It still carries the pinning transport.
	client := c.newHTTPClient(0)
	send := func(cred clienttoken.Credential) (*http.Response, error) {
		// Fresh request and body reader per attempt: the first send consumed
		// the reader.
		var bodyReader io.Reader
		if bodyData != nil {
			bodyReader = bytes.NewReader(bodyData)
		}
		req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, bodyReader)
		if err != nil {
			return nil, fmt.Errorf("failed to create request: %w", err)
		}
		if bodyData != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		req = pinCredential(req, cred)
		return client.Do(req)
	}
	wrapErr := func(err error) error {
		if ctx.Err() == context.DeadlineExceeded {
			return fmt.Errorf("request timed out after %v (use --timeout to increase)", timeout)
		}
		return connectFailure(err)
	}

	resp, err := c.sendWithReauth(send, wrapErr)
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
			// Server text, escaped as parseError escapes it (SafeText):
			// printable text is unchanged, a control character never reaches
			// the terminal raw.
			// The server sends the API's error envelope,
			// {"error":{"code","message","reason"}}; a bare
			// {"code","message"} is read too.
			var apiErr config.APIError
			if uerr := json.Unmarshal([]byte(data), &apiErr); uerr == nil && apiErr.Error.Code != "" {
				return true, nil, errors.New(apiErrorText(apiErr.Error))
			}
			var bare config.APIErrorDetail
			if uerr := json.Unmarshal([]byte(data), &bare); uerr == nil && bare.Code != "" {
				return true, nil, errors.New(apiErrorText(bare))
			}
			return true, nil, fmt.Errorf("server error: %s", sdkbootstrap.SafeText(data))
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

// errSourcesUnsupported is a server without the credential source routes
// (older than the pin): a 404 that carries no API error code.
var errSourcesUnsupported = errors.New("the server does not support credential sources")

// ListSources returns the caller's credential source and machines (GET
// /api/sources), or user's when user is set (admin only).
func (c *APIClient) ListSources(user string) (*config.SourcesResponse, error) {
	path := "/api/sources"
	if user != "" {
		path += "?user=" + url.QueryEscape(user)
	}
	var resp config.SourcesResponse
	if err := c.doSourcesRequest(http.MethodGet, path, nil, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

// UseSource makes machineID (a full machine id) the credential source of the
// caller, or of user when user is set (admin only), and returns the sources
// after the move (POST /api/sources/use).
func (c *APIClient) UseSource(machineID, user string) (*config.SourcesResponse, error) {
	var resp config.SourcesResponse
	req := config.SourceUseRequest{MachineID: machineID, User: user}
	if err := c.doSourcesRequest(http.MethodPost, "/api/sources/use", req, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

// UnbindSource clears the credential source of the caller, or of user when
// user is set (admin only) (POST /api/sources/unbind). A user with no source
// is answered with Unbound false.
func (c *APIClient) UnbindSource(user string) (*config.SourceUnbindResponse, error) {
	var resp config.SourceUnbindResponse
	req := config.SourceUnbindRequest{User: user}
	if err := c.doSourcesRequest(http.MethodPost, "/api/sources/unbind", req, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

// doSourcesRequest is doRequest for the credential source routes, which also
// tells a server without them (errSourcesUnsupported) from a 404 the routes
// answer themselves (SOURCE_MACHINE_NOT_FOUND).
func (c *APIClient) doSourcesRequest(method, path string, body, result any) error {
	resp, err := c.sendWithReauth(
		func(cred clienttoken.Credential) (*http.Response, error) {
			return c.sendRequest(method, path, body, cred)
		},
		connectFailure)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusNotFound:
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		var apiErr config.APIError
		if json.Unmarshal(data, &apiErr) != nil || apiErr.Error.Code == "" {
			return errSourcesUnsupported
		}
		return fmt.Errorf("%s: %s", apiErr.Error.Code, apiErr.Error.Message)
	default:
		return c.parseError(resp)
	}
	if err := json.NewDecoder(resp.Body).Decode(result); err != nil {
		return fmt.Errorf("failed to parse response: %w", err)
	}
	return nil
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

// ExtendShed moves a cloud shed's lease later by req.Hours, with the
// caller's request id (req.RequestID) when it has one.
func (c *APIClient) ExtendShed(name string, req config.ExtendShedRequest) (*config.Shed, error) {
	var shed config.Shed
	if err := c.doRequestWithTimeout(http.MethodPost, "/api/sheds/"+name+"/extend", req, &shed, c.createTimeout); err != nil {
		return nil, err
	}
	return &shed, nil
}

// CloudUsage reads the machines held against the account service's limits
// (the caller's and the organization's).
func (c *APIClient) CloudUsage() (*config.CloudUsage, error) {
	var u config.CloudUsage
	if err := c.doRequest(http.MethodGet, "/api/cloud/usage", nil, &u); err != nil {
		return nil, err
	}
	return &u, nil
}

// StopShed stops a running shed.
func (c *APIClient) StopShed(name string) (*config.Shed, error) {
	var shed config.Shed
	if err := c.doRequest(http.MethodPost, "/api/sheds/"+name+"/stop", nil, &shed); err != nil {
		return nil, err
	}
	return &shed, nil
}

// ProvisionShedWithProgress runs `shed provision` on a shed, streaming its
// progress via SSE (a failed delivery arrives as a warning), and returns the
// shed record it ends with.
func (c *APIClient) ProvisionShedWithProgress(name string, onProgress func(backend.ProgressEvent)) (*config.Shed, error) {
	ctx, cancel := context.WithTimeout(context.Background(), c.createTimeout)
	defer cancel()
	url := c.baseURL + "/api/sheds/" + name + "/provision"
	client := c.newHTTPClient(0)
	send := func(cred clienttoken.Credential) (*http.Response, error) {
		httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, nil)
		if err != nil {
			return nil, fmt.Errorf("failed to create request: %w", err)
		}
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
	return c.readShedSSEStream(resp.Body, onProgress)
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
// degradations (e.g. tmux unavailable in the guest) that callers should
// surface rather than silently rendering an incomplete list.
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

// RefreshUsers asks the server to fetch its GitHub logins again (admin only).
func (c *APIClient) RefreshUsers() (*config.UsersRefreshResponse, error) {
	var resp config.UsersRefreshResponse
	if err := c.doRequest(http.MethodPost, "/api/users/refresh", nil, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

// errUsersStatusUnsupported is a server older than GET /api/users/status:
// it answers the route with a 404 (the route itself never does).
var errUsersStatusUnsupported = errors.New("this server has no status route")

// UsersStatus asks where the server's users come from and, on a directory
// server, the state of its list (admin only).
func (c *APIClient) UsersStatus() (*config.UsersStatusResponse, error) {
	resp, err := c.sendWithReauth(
		func(cred clienttoken.Credential) (*http.Response, error) {
			return c.sendRequest(http.MethodGet, "/api/users/status", nil, cred)
		},
		connectFailure)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusNotFound:
		return nil, errUsersStatusUnsupported
	default:
		return nil, c.parseError(resp)
	}
	var st config.UsersStatusResponse
	if err := json.NewDecoder(resp.Body).Decode(&st); err != nil {
		return nil, fmt.Errorf("failed to parse response: %w", err)
	}
	return &st, nil
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

// Server statuses `shed server list` shows.
const (
	serverStatusOnline  = "online"
	serverStatusOffline = "offline"
	serverStatusRefused = "refused"
)

// statusProbeTimeout bounds the HTTP leg of Status (the reactive re-mint, an
// ssh round trip, is not inside it).
const statusProbeTimeout = 2 * time.Second

// Status probes the server for `shed server list` through the request path
// (sendWithReauth), so its one reactive re-mint runs as for any request: a
// certificate the server rejects is re-enrolled, or refused when the new one
// is for another user. It returns serverStatusRefused, with the refusal, for a
// refused client (no request is sent) or a refusal from that re-mint;
// serverStatusOnline on a 200; serverStatusOffline otherwise.
func (c *APIClient) Status() (string, error) {
	if err := c.tokens.Err(); err != nil {
		return serverStatusRefused, err
	}
	client := c.newHTTPClient(statusProbeTimeout)
	resp, err := c.sendWithReauth(func(cred clienttoken.Credential) (*http.Response, error) {
		req, err := http.NewRequest(http.MethodGet, c.baseURL+"/api/info", nil)
		if err != nil {
			return nil, err
		}
		return client.Do(pinCredential(req, cred))
	}, connectFailure)
	if err != nil {
		if clienttoken.IsRefusal(err) {
			return serverStatusRefused, err
		}
		return serverStatusOffline, nil
	}
	_ = resp.Body.Close()
	if resp.StatusCode == http.StatusOK {
		return serverStatusOnline, nil
	}
	return serverStatusOffline, nil
}

// apiStatusError is the server's answer with a status the request did not
// expect (parseError): the server was reached and answered. Status is the
// HTTP status; the text is what parseError has always returned.
type apiStatusError struct {
	Status int
	// Code is the structured error's code, "" when the body had none.
	Code string
	text string
}

func (e *apiStatusError) Error() string { return e.text }

// parseError extracts the error message from an API error response, as an
// *apiStatusError carrying the status. What the server sent (the body, or the
// structured error's code and message) is escaped as its neighbours escape
// server text (sdkbootstrap.SafeText): printable text is unchanged, and a
// control character (an ESC sequence, a line break) never reaches the
// terminal raw.
func (c *APIClient) parseError(resp *http.Response) error {
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return &apiStatusError{Status: resp.StatusCode, text: fmt.Sprintf("server returned status %d", resp.StatusCode)}
	}

	var apiErr config.APIError
	if err := json.Unmarshal(body, &apiErr); err != nil {
		// If not a structured error, return the body as it is, escaped (the
		// line break that ends a plain-text body ends the line, as before).
		return &apiStatusError{Status: resp.StatusCode, text: fmt.Sprintf("server returned status %d: %s",
			resp.StatusCode, sdkbootstrap.SafeText(strings.TrimRight(string(body), "\r\n")))}
	}

	return &apiStatusError{Status: resp.StatusCode, Code: apiErr.Error.Code, text: apiErrorText(apiErr.Error)}
}
