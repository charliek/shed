package refserver

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"mime"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"
)

// Token endpoint limits (contract section 2).
const (
	maxTokenBody     = 64 << 10
	maxAssertion     = 4096
	maxAssertionLife = 60 // seconds, exp - iat
	maxJTIBytes      = 256
)

// tokenParams are the parameters that may each be sent once only.
var tokenParams = []string{"grant_type", "client_assertion", "client_assertion_type", "client_id", "scope"}

// forbiddenHeaderMembers may not appear in an assertion's header, not even
// as null.
var forbiddenHeaderMembers = []string{"jwk", "jku", "x5u", "x5c", "crit", "b64"}

// errUnavailable is a lookup that failed, as in a storage outage.
var errUnavailable = errors.New("a lookup failed")

// handleToken answers POST /oauth2/token: OAuth 2.0 client credentials with
// a JWT client assertion.
func (s *Server) handleToken(w http.ResponseWriter, r *http.Request) {
	if ok, ra := s.allow(limitToken, callerOf(r)); !ok {
		writeOAuthError(w, http.StatusTooManyRequests, "temporarily_unavailable", "too many token requests", ra)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, maxTokenBody+1))
	if err != nil {
		writeOAuthError(w, http.StatusBadRequest, "invalid_request", "the body did not arrive whole", 0)
		return
	}
	if len(body) > maxTokenBody {
		// A bare 413, not the OAuth shape.
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(http.StatusRequestEntityTooLarge)
		return
	}
	if mt, _, err := mime.ParseMediaType(r.Header.Get("Content-Type")); err != nil || mt != "application/x-www-form-urlencoded" {
		writeOAuthError(w, http.StatusBadRequest, "invalid_request", "the body is not a form", 0)
		return
	}
	form, err := url.ParseQuery(string(body))
	if err != nil {
		writeOAuthError(w, http.StatusBadRequest, "invalid_request", "the body is not a form", 0)
		return
	}
	s.logf("%s", tokenRequestLine(form["scope"]))
	if !slices.Contains(form["grant_type"], grantClientCreds) {
		writeOAuthError(w, http.StatusBadRequest, "unsupported_grant_type", "the grant is client_credentials", 0)
		return
	}
	for _, p := range tokenParams {
		// Sent twice is twice, whatever the values, an empty one included.
		if len(form[p]) > 1 {
			writeOAuthError(w, http.StatusBadRequest, "invalid_request", p+" is sent more than once", 0)
			return
		}
	}
	if len(r.Header.Values("Authorization")) > 0 || anyNonEmpty(form["client_secret"]) {
		writeOAuthError(w, http.StatusUnauthorized, "invalid_client", "a client authenticates by its assertion only", 0)
		return
	}
	if anyNonEmpty(form["resource"]) || anyNonEmpty(form["audience"]) {
		writeOAuthError(w, http.StatusBadRequest, "invalid_target", "resource and audience are not accepted", 0)
		return
	}
	assertionType, assertion := form.Get("client_assertion_type"), form.Get("client_assertion")
	if (assertionType == "") != (assertion == "") {
		writeOAuthError(w, http.StatusBadRequest, "invalid_request", "client_assertion and client_assertion_type come together", 0)
		return
	}
	if assertionType != assertionTypeJWT {
		writeOAuthError(w, http.StatusUnauthorized, "invalid_client", "the client did not authenticate with a JWT assertion", 0)
		return
	}
	scopes, ok := parseScope(form.Get("scope"), s.offersDeliveries(), true)
	if !ok {
		writeOAuthError(w, http.StatusBadRequest, "invalid_scope", "the scope is malformed or names a scope this service lacks", 0)
		return
	}

	now := s.now()
	a, err := s.verifyAssertion(assertion, now)
	if errors.Is(err, errUnavailable) {
		s.logf("token: the server lookup failed")
		writeOAuthError(w, http.StatusServiceUnavailable, "server_error", "the service could not look the server up", 0)
		return
	}
	if err != nil {
		s.logf("token refused: %v", err)
		writeOAuthError(w, http.StatusUnauthorized, "invalid_client", "the client could not be authenticated", 0)
		return
	}
	if id := form.Get("client_id"); id != "" && id != a.clientID {
		s.logf("token refused: client_id is not the assertion's")
		writeOAuthError(w, http.StatusUnauthorized, "invalid_client", "the client could not be authenticated", 0)
		return
	}
	if containsString(scopes, scopeDeliveries) {
		// The grant is read on every request for the scope; a server that
		// may not ask gets invalid_scope, and its assertion is not spent.
		denied, err := s.inputs.(DeliveryInputs).DeliveriesDenied(a.clientID)
		if err != nil {
			s.logf("token: the delivery grant lookup failed")
			writeOAuthError(w, http.StatusServiceUnavailable, "server_error", "the service could not look the server up", 0)
			return
		}
		if denied {
			writeOAuthError(w, http.StatusBadRequest, "invalid_scope", "the scope is malformed or names a scope this service lacks", 0)
			return
		}
	}
	if containsString(scopes, scopeCompute) {
		// The compute grant, like the delivery grant, is read on every
		// request for the scope: a server without it gets invalid_scope,
		// and its assertion is not spent.
		s.compute.mu.Lock()
		granted := s.compute.grants[a.clientID]
		s.compute.mu.Unlock()
		if !granted {
			writeOAuthError(w, http.StatusBadRequest, "invalid_scope", "the scope is malformed or names a scope this service lacks", 0)
			return
		}
	}
	if !s.ledger.spend(a.clientID, a.jti, a.exp, now) {
		s.logf("token refused: the assertion of %s was presented before", a.clientID)
		writeOAuthError(w, http.StatusUnauthorized, "invalid_client", "the client could not be authenticated", 0)
		return
	}

	tok, err := s.issueAccessToken(a.clientID, scopes, now)
	if err != nil {
		writeOAuthError(w, http.StatusInternalServerError, "server_error", "the token could not be signed", 0)
		return
	}
	w.Header().Set("Pragma", "no-cache")
	writeJSON(w, http.StatusOK, map[string]any{
		"access_token": tok,
		"token_type":   "bearer",
		"expires_in":   accessTokenTTL,
		"scope":        strings.Join(scopes, " "),
	}, true)
}

// tokenRequestLine is the line logged for each token request whose form was
// read: the scope parameter exactly as received (quoted), and nothing else
// from the request, so a test can see what a client asks for. The scope is
// not a secret. A request with no scope parameter, and one that sends it
// more than once, each have a form of their own.
func tokenRequestLine(scope []string) string {
	switch len(scope) {
	case 0:
		return "token request: no scope"
	case 1:
		return fmt.Sprintf("token request: scope=%q", scope[0])
	default:
		return fmt.Sprintf("token request: scope sent %d times", len(scope))
	}
}

func anyNonEmpty(vals []string) bool {
	for _, v := range vals {
		if v != "" {
			return true
		}
	}
	return false
}

// offersDeliveries reports whether the service delivers credentials: its
// inputs say who may ask and what each person's source holds.
func (s *Server) offersDeliveries() bool {
	_, ok := s.inputs.(DeliveryInputs)
	return ok
}

// parseScope reads the scope parameter: absent (or sent empty) is
// directory:read alone, never the delivery or compute scope, whatever the
// server may ask for; sent, scope tokens separated by single spaces, each
// one the service has (deliveries:request only when it offers delivery,
// compute:manage only when it offers compute).
func parseScope(scope string, deliveries, compute bool) ([]string, bool) {
	if scope == "" {
		return []string{scopeDirectory}, true
	}
	toks := strings.Split(scope, " ")
	seen := map[string]bool{}
	var out []string
	for _, t := range toks {
		if t != scopeDirectory && (t != scopeDeliveries || !deliveries) && (t != scopeCompute || !compute) {
			return nil, false
		}
		if !seen[t] {
			seen[t] = true
			out = append(out, t)
		}
	}
	return out, true
}

// assertionClaims are what a verified assertion tells the service.
type assertionClaims struct {
	clientID string
	jti      string
	exp      int64
}

// splitJWS splits a compact JWS into its decoded parts and its signing
// input. Each part is unpadded base64url of the alphabet alone, in its one
// canonical form (decodeB64URL).
func splitJWS(tok string) (header, claims, sig []byte, signingInput string, err error) {
	parts := strings.Split(tok, ".")
	if len(parts) != 3 {
		return nil, nil, nil, "", errors.New("not a compact JWS")
	}
	if header, err = decodeB64URL(parts[0]); err != nil {
		return nil, nil, nil, "", errors.New("the header is not base64url")
	}
	if claims, err = decodeB64URL(parts[1]); err != nil {
		return nil, nil, nil, "", errors.New("the claims are not base64url")
	}
	if sig, err = decodeB64URL(parts[2]); err != nil {
		return nil, nil, nil, "", errors.New("the signature is not base64url")
	}
	return header, claims, sig, parts[0] + "." + parts[1], nil
}

// jwsObject reads a JWS header or claims set: one JSON object, no member
// named twice.
func jwsObject(raw []byte) (map[string]json.RawMessage, error) {
	if err := checkStrictJSON(raw, 16); err != nil {
		return nil, err
	}
	return objectMembers(raw)
}

// acceptedAssertionType applies rule 10 to a typ: JWT or
// client-authentication+jwt, in any case, with an optional application/.
func acceptedAssertionType(typ string) bool {
	t := strings.ToLower(typ)
	t = strings.TrimPrefix(t, "application/")
	return t == "jwt" || t == "client-authentication+jwt"
}

// verifyAssertion checks a client assertion completely, short of spending
// it: its size and form, its header, the server it names (a failed lookup
// is errUnavailable), its signature with that server's one key, and its
// claims at the instant now.
func (s *Server) verifyAssertion(tok string, now time.Time) (assertionClaims, error) {
	var none assertionClaims
	if len(tok) > maxAssertion {
		return none, fmt.Errorf("the assertion is over %d bytes", maxAssertion)
	}
	rawHeader, rawClaims, sig, signingInput, err := splitJWS(tok)
	if err != nil {
		return none, err
	}
	header, err := jwsObject(rawHeader)
	if err != nil {
		return none, fmt.Errorf("header: %w", err)
	}
	claims, err := jwsObject(rawClaims)
	if err != nil {
		return none, fmt.Errorf("claims: %w", err)
	}
	for _, m := range forbiddenHeaderMembers {
		if _, ok := header[m]; ok {
			return none, fmt.Errorf("the header has %s", m)
		}
	}
	alg, ok := jsonString(header["alg"])
	if !ok {
		return none, errors.New("the header has no alg")
	}
	if raw, ok := header["typ"]; ok {
		if typ, ok := jsonString(raw); !ok || !acceptedAssertionType(typ) {
			return none, errors.New("the header's typ is refused (rule 10)")
		}
	}
	kid, hasKid := "", false
	if raw, ok := header["kid"]; ok {
		if kid, ok = jsonString(raw); !ok {
			return none, errors.New("the header's kid is not a string")
		}
		hasKid = true
	}

	iss, issOK := jsonString(claims["iss"])
	sub, subOK := jsonString(claims["sub"])
	if !issOK || !subOK || iss == "" || iss != sub {
		return none, errors.New("iss and sub are not both the client id")
	}

	// The lookup: the operator's record of the server first, so that an
	// outage answers 503 whoever is asking.
	disabled, err := s.inputs.Disabled(iss)
	if err != nil {
		return none, errUnavailable
	}
	s.mu.Lock()
	srv := s.servers[iss]
	s.mu.Unlock()
	if srv == nil {
		return none, errors.New("no server has this client id")
	}
	if disabled {
		return none, fmt.Errorf("%s is disabled", iss)
	}

	if alg != srv.key.alg {
		return none, fmt.Errorf("alg %q is not the key's %s", alg, srv.key.alg)
	}
	if hasKid && srv.key.hasKid && kid != srv.key.kid {
		return none, errors.New("the header's kid is not the key's")
	}
	if !srv.key.verify(signingInput, sig) {
		return none, errors.New("the signature does not verify")
	}

	if !acceptedAudience(claims["aud"], s.issuer) {
		return none, errors.New("aud is not the issuer (rule 10)")
	}
	// Every numeric date is range-checked before any arithmetic or
	// conversion (numericDate), so exp - iat and exp + the leeway cannot
	// wrap and time.Unix cannot overflow.
	iat, iatOK := numericDate(claims["iat"])
	exp, expOK := numericDate(claims["exp"])
	if !iatOK || !expOK {
		return none, errors.New("iat and exp are not both integers from 0 to the end of 9999")
	}
	if life := exp - iat; life <= 0 || life > maxAssertionLife {
		return none, fmt.Errorf("exp - iat is %d, not 1 to %d", life, maxAssertionLife)
	}
	if !now.Before(time.Unix(exp, 0).Add(leeway)) {
		return none, errors.New("the assertion has expired")
	}
	if time.Unix(iat, 0).After(now.Add(leeway)) {
		return none, errors.New("iat is in the future")
	}
	if time.Unix(iat, 0).Before(now.Add(-maxAssertionLife*time.Second - leeway)) {
		return none, errors.New("iat is too far in the past")
	}
	if raw, ok := claims["nbf"]; ok {
		nbf, ok := numericDate(raw)
		if !ok {
			return none, errors.New("nbf is not an integer from 0 to the end of 9999")
		}
		if time.Unix(nbf, 0).After(now.Add(leeway)) {
			return none, errors.New("nbf is in the future")
		}
	}
	jti, ok := jsonString(claims["jti"])
	if !ok || len(jti) == 0 || len(jti) > maxJTIBytes {
		return none, errors.New("jti is not a string of 1 to 256 bytes")
	}
	return assertionClaims{clientID: iss, jti: jti, exp: exp}, nil
}

// acceptedAudience applies rule 10 to an assertion's aud: the issuer
// string, or a one-element array holding it, byte for byte.
func acceptedAudience(raw json.RawMessage, issuer string) bool {
	if s, ok := jsonString(raw); ok {
		return s == issuer
	}
	if len(raw) == 0 || raw[0] != '[' {
		return false
	}
	var arr []json.RawMessage
	if err := json.Unmarshal(raw, &arr); err != nil || len(arr) != 1 {
		return false
	}
	s, ok := jsonString(arr[0])
	return ok && s == issuer
}

// The replay ledger: each spent assertion, keyed on its client and jti,
// kept until its exp plus the leeway, the whole time in which the window
// would still accept it.

type ledgerKey struct {
	clientID, jti string
}

type ledger struct {
	mu    sync.Mutex
	spent map[ledgerKey]int64 // the Unix second at which it may be forgotten
	// floor is the latest instant (Unix seconds) the ledger was pruned at:
	// every entry whose time to forget was at or before it may be gone, so
	// an assertion whose time to forget is at or before it is refused even
	// if the clock has since moved back.
	//
	// This is deliberate, and it refuses fresh assertions too: after the
	// clock moves back, an assertion whose acceptance window (exp plus the
	// leeway) ended at or before the latest time the server has seen is
	// refused until the clock catches up, because a never-seen assertion and
	// one spent and then pruned cannot be told apart. Refusing is the safe
	// side; an assertion whose window ends after the floor is accepted as
	// usual. TestLedgerFloorAfterClockMovesBack pins both.
	floor int64
}

// spend checks an assertion has not been spent and spends it, in one step.
// It reports false for a replay.
func (l *ledger) spend(clientID, jti string, exp int64, now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	forget := exp + int64(leeway/time.Second)
	if forget <= l.floor {
		return false
	}
	if n := now.Unix(); n > l.floor {
		for k, at := range l.spent {
			if at <= n {
				delete(l.spent, k)
			}
		}
		l.floor = n
		if forget <= l.floor {
			return false
		}
	}
	k := ledgerKey{clientID, jti}
	if _, spent := l.spent[k]; spent {
		return false
	}
	l.spent[k] = forget
	return true
}

// issueAccessToken signs an access token (RFC 9068) for a client.
func (s *Server) issueAccessToken(clientID string, scopes []string, now time.Time) (string, error) {
	iat := now.Unix()
	header := map[string]string{"alg": "RS256", "typ": "at+jwt", "kid": s.kid}
	claims := map[string]any{
		"iss":       s.issuer,
		"sub":       clientID,
		"client_id": clientID,
		"aud":       []string{s.issuer + "/v1"},
		"gty":       grantClientCreds,
		"scp":       scopes,
		"jti":       randomHex(16),
		"iat":       iat,
		"nbf":       iat,
		"exp":       iat + accessTokenTTL,
	}
	h, err := json.Marshal(header)
	if err != nil {
		return "", err
	}
	c, err := json.Marshal(claims)
	if err != nil {
		return "", err
	}
	enc := base64.RawURLEncoding.EncodeToString
	input := enc(h) + "." + enc(c)
	sum := sha256.Sum256([]byte(input))
	sig, err := rsa.SignPKCS1v15(rand.Reader, s.signer, crypto.SHA256, sum[:])
	if err != nil {
		return "", err
	}
	return input + "." + enc(sig), nil
}

// handleJWKS serves the access tokens' verification key.
func (s *Server) handleJWKS(w http.ResponseWriter, _ *http.Request) {
	pub := s.signer.PublicKey
	enc := base64.RawURLEncoding.EncodeToString
	writeJSON(w, http.StatusOK, map[string]any{"keys": []map[string]string{{
		"kty": "RSA",
		"use": "sig",
		"alg": "RS256",
		"kid": s.kid,
		"n":   enc(pub.N.Bytes()),
		"e":   enc(big.NewInt(int64(pub.E)).Bytes()),
	}}}, false)
}

// handleMetadata serves RFC 8414 metadata.
func (s *Server) handleMetadata(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"issuer":                                s.issuer,
		"token_endpoint":                        s.issuer + pathToken,
		"jwks_uri":                              s.issuer + pathJWKS,
		"grant_types_supported":                 []string{grantClientCreds},
		"token_endpoint_auth_methods_supported": []string{"private_key_jwt"},
		"token_endpoint_auth_signing_alg_values_supported": []string{"ES256", "EdDSA"},
		"scopes_supported": []string{scopeDirectory},
	}, false)
}
