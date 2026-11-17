package refserver

import (
	"crypto"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	gossh "golang.org/x/crypto/ssh"
)

// The list's limits and names (contract section 3).
const (
	maxKeysPerUser = 20
	maxLabelBytes  = 64
	timeLayout     = "2006-01-02T15:04:05Z"
	// maxDirectoryBody is the largest directory answer a service serves
	// (rule 1), counted on the encoded bytes.
	maxDirectoryBody = 8 << 20
)

var (
	userNameRx    = regexp.MustCompile(`^[a-z][a-z0-9-]{0,62}$`)
	reservedNames = map[string]bool{"root": true, "admin": true, "git": true, "shed": true, "system": true, "nobody": true}
	// acceptedKeyTypes are the key types the contract accepts; every
	// certificate type, ssh-dss and every ML-DSA type are outside it.
	acceptedKeyTypes = map[string]bool{
		gossh.KeyAlgoED25519:    true,
		gossh.KeyAlgoSKED25519:  true,
		gossh.KeyAlgoECDSA256:   true,
		gossh.KeyAlgoECDSA384:   true,
		gossh.KeyAlgoECDSA521:   true,
		gossh.KeyAlgoSKECDSA256: true,
		gossh.KeyAlgoRSA:        true,
	}
)

// invalidTokenMessage is every 401's message: every 401 carries the same
// body, whatever failed.
const invalidTokenMessage = "the access token is missing or not valid"

// handleDirectory answers GET /v1/directory. It checks, in order: the
// token, its scope, the rate limit, then the server's own record (gone:
// 401; disabled: 403), then reads the list and the server's record again,
// so a list never holds a change made after the server was disabled.
func (s *Server) handleDirectory(w http.ResponseWriter, r *http.Request) {
	now := s.now()
	clientID, scopes, err := s.verifyBearer(r, now)
	if err != nil {
		s.logf("directory refused: %v", err)
		writeInvalidToken(w)
		return
	}
	if !slices.Contains(scopes, scopeDirectory) {
		writeV1Error(w, http.StatusForbidden, "INSUFFICIENT_SCOPE", "the token lacks directory:read", 0)
		return
	}
	if ok, ra := s.allow(limitDirectory, clientID); !ok {
		writeV1Error(w, http.StatusTooManyRequests, "RATE_LIMITED", "too many directory requests", ra)
		return
	}
	unavailable := func(err error) {
		s.logf("directory: a lookup failed: %v", err)
		writeUnavailable(w)
	}
	disabled, err := s.inputs.Disabled(clientID)
	if err != nil {
		unavailable(err)
		return
	}
	s.mu.Lock()
	srv := s.servers[clientID]
	s.mu.Unlock()
	if srv == nil {
		s.logf("directory refused: no server has the token's client id")
		writeInvalidToken(w)
		return
	}
	if disabled {
		writeV1Error(w, http.StatusForbidden, "SERVER_DISABLED", "the server is disabled", 0)
		return
	}
	users, err := s.inputs.Users()
	if err != nil {
		unavailable(err)
		return
	}
	if disabled, err = s.inputs.Disabled(clientID); err != nil {
		unavailable(err)
		return
	}
	if disabled {
		writeV1Error(w, http.StatusForbidden, "SERVER_DISABLED", "the server is disabled", 0)
		return
	}
	served, err := serveList(users, now)
	if err != nil {
		unavailable(fmt.Errorf("the list breaks the contract's rules for a service: %w", err))
		return
	}
	// The whole answer is encoded and measured before a 200 is committed:
	// one over rule 1's limit is never served, whatever made it so (the
	// encoder escapes some characters to six bytes each).
	body, err := json.Marshal(map[string]any{
		"contract":     contractNumber,
		"organization": s.org,
		"version":      listVersion(served),
		"generated_at": now.UTC().Format(timeLayout),
		"rules":        map[string]int{"version": 0},
		"users":        served,
	})
	if err != nil {
		unavailable(fmt.Errorf("the answer could not be encoded: %w", err))
		return
	}
	if len(body) > maxDirectoryBody {
		unavailable(fmt.Errorf("the answer takes %d bytes, more than rule 1's %d", len(body), maxDirectoryBody))
		return
	}
	writeBody(w, http.StatusOK, body, true)
}

func writeInvalidToken(w http.ResponseWriter) {
	w.Header().Set("WWW-Authenticate", `Bearer error="invalid_token"`)
	writeV1Error(w, http.StatusUnauthorized, "invalid_token", invalidTokenMessage, 0)
}

// verifyBearer checks the request's access token: one Authorization: Bearer
// header; the header's typ at+jwt, alg RS256 and this service's key id; its
// signature; then iss, aud holding <issuer>/v1, exp and nbf with the leeway,
// gty client_credentials and sub equal to client_id. It returns the client
// id and the scopes.
func (s *Server) verifyBearer(r *http.Request, now time.Time) (string, []string, error) {
	vals := r.Header.Values("Authorization")
	if len(vals) != 1 {
		return "", nil, fmt.Errorf("%d Authorization headers, not one", len(vals))
	}
	scheme, tok, ok := strings.Cut(vals[0], " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") || tok == "" || strings.ContainsAny(tok, " \t") {
		return "", nil, errors.New("the Authorization header is not a bearer token")
	}
	rawHeader, rawClaims, sig, signingInput, err := splitJWS(tok)
	if err != nil {
		return "", nil, err
	}
	header, err := jwsObject(rawHeader)
	if err != nil {
		return "", nil, fmt.Errorf("header: %w", err)
	}
	if typ, _ := jsonString(header["typ"]); typ != "at+jwt" {
		return "", nil, errors.New("the header's typ is not at+jwt")
	}
	if alg, _ := jsonString(header["alg"]); alg != "RS256" {
		return "", nil, errors.New("the header's alg is not RS256")
	}
	if kid, _ := jsonString(header["kid"]); kid != s.kid {
		return "", nil, errors.New("the header's kid is not this service's key")
	}
	sum := sha256.Sum256([]byte(signingInput))
	if err := rsa.VerifyPKCS1v15(&s.signer.PublicKey, crypto.SHA256, sum[:], sig); err != nil {
		return "", nil, errors.New("the signature does not verify")
	}
	claims, err := jwsObject(rawClaims)
	if err != nil {
		return "", nil, fmt.Errorf("claims: %w", err)
	}
	if iss, _ := jsonString(claims["iss"]); iss != s.issuer {
		return "", nil, errors.New("iss is not this service")
	}
	var aud []string
	if err := json.Unmarshal(claims["aud"], &aud); err != nil || !slices.Contains(aud, s.issuer+"/v1") {
		return "", nil, errors.New("aud does not hold <issuer>/v1")
	}
	// Range-checked before any conversion (numericDate): time.Unix wraps
	// near the int64 limits.
	exp, expOK := numericDate(claims["exp"])
	nbf, nbfOK := numericDate(claims["nbf"])
	if !expOK || !nbfOK {
		return "", nil, errors.New("exp and nbf are not both integers from 0 to the end of 9999")
	}
	if !now.Before(time.Unix(exp, 0).Add(leeway)) {
		return "", nil, errors.New("the token has expired")
	}
	if time.Unix(nbf, 0).After(now.Add(leeway)) {
		return "", nil, errors.New("the token is not yet valid")
	}
	if gty, _ := jsonString(claims["gty"]); gty != grantClientCreds {
		return "", nil, errors.New("gty is not client_credentials")
	}
	clientID, _ := jsonString(claims["client_id"])
	sub, _ := jsonString(claims["sub"])
	if clientID == "" || sub != clientID {
		return "", nil, errors.New("sub is not client_id")
	}
	var scopes []string
	if err := json.Unmarshal(claims["scp"], &scopes); err != nil {
		return "", nil, errors.New("scp is not an array of strings")
	}
	return clientID, scopes, nil
}

// serveList checks the operator's list against the contract's rules for a
// service and returns what the directory serves at now: keys that have
// expired left out, users with no key left left out, users ordered by name
// and keys by label (bytewise). A list that breaks a rule is refused whole:
// the reference server never serves one.
func serveList(users []User, now time.Time) ([]User, error) {
	names := map[string]bool{}
	blobs := map[string]string{}
	var served []User
	for _, u := range users {
		if !userNameRx.MatchString(u.Name) || reservedNames[u.Name] {
			return nil, fmt.Errorf("the name %q is not one a service may serve", u.Name)
		}
		if names[u.Name] {
			return nil, fmt.Errorf("the name %q is listed twice", u.Name)
		}
		names[u.Name] = true
		if len(u.Keys) > maxKeysPerUser {
			return nil, fmt.Errorf("%s holds %d keys, more than %d", u.Name, len(u.Keys), maxKeysPerUser)
		}
		labels := map[string]bool{}
		var keys []Key
		for _, k := range u.Keys {
			fp, err := checkServedKey(k.Key)
			if err != nil {
				return nil, fmt.Errorf("%s: %w", u.Name, err)
			}
			if other, dup := blobs[fp]; dup {
				return nil, fmt.Errorf("a key of %s is also listed under %s", u.Name, other)
			}
			blobs[fp] = u.Name
			if err := checkLabel(k.Label); err != nil {
				return nil, fmt.Errorf("%s: %w", u.Name, err)
			}
			if labels[k.Label] {
				return nil, fmt.Errorf("%s: the label %q is used twice", u.Name, k.Label)
			}
			labels[k.Label] = true
			if k.Expires != "" {
				at, err := time.Parse(time.RFC3339, k.Expires)
				if err != nil || at.UTC().Format(timeLayout) != k.Expires {
					return nil, fmt.Errorf("%s: expires %q is not an RFC 3339 instant in UTC with whole seconds", u.Name, k.Expires)
				}
				if !now.Before(at) {
					continue // expired: never listed
				}
			}
			keys = append(keys, k)
		}
		if len(keys) == 0 {
			continue // a user with no key is never listed
		}
		sort.Slice(keys, func(i, j int) bool { return keys[i].Label < keys[j].Label })
		served = append(served, User{Name: u.Name, Admin: u.Admin, Keys: keys})
	}
	sort.Slice(served, func(i, j int) bool { return served[i].Name < served[j].Name })
	if served == nil {
		served = []User{}
	}
	return served, nil
}

// checkServedKey checks a key is one the contract accepts, in its one
// canonical form, and returns its fingerprint (the key blob's identity).
func checkServedKey(line string) (string, error) {
	pk, comment, options, rest, err := gossh.ParseAuthorizedKey([]byte(line))
	if err != nil || comment != "" || len(options) != 0 || len(rest) != 0 {
		return "", errors.New("a key is not one SSH public key line with no options or comment")
	}
	if !acceptedKeyTypes[pk.Type()] {
		return "", fmt.Errorf("the key type %q is not accepted", pk.Type())
	}
	if canonical := strings.TrimSuffix(string(gossh.MarshalAuthorizedKey(pk)), "\n"); canonical != line {
		return "", fmt.Errorf("a key of type %s is not in canonical form", pk.Type())
	}
	// The parser has checked an ECDSA point is on the curve its type names;
	// an RSA modulus is held to the contract's sizes here.
	if ck, ok := pk.(gossh.CryptoPublicKey); ok {
		if k, ok := ck.CryptoPublicKey().(*rsa.PublicKey); ok {
			if k.N == nil || k.N.Sign() <= 0 {
				return "", errors.New("an RSA key's modulus is not positive")
			}
			if bits := k.N.BitLen(); bits < 2048 || bits > 16384 {
				return "", fmt.Errorf("an RSA key of %d bits is not accepted", bits)
			}
		}
	}
	// The service's own rules on the key's wire form, read from the blob
	// itself rather than from what the SSH parser kept of it.
	if err := checkKeyBlob(pk.Type(), pk.Marshal()); err != nil {
		return "", err
	}
	return gossh.FingerprintSHA256(pk), nil
}

// checkKeyBlob applies the contract's rules on a key's wire blob that the
// SSH parser does not: an RSA modulus (and exponent) positive, and a
// security key's application string valid UTF-8.
func checkKeyBlob(keyType string, blob []byte) error {
	var fields int
	switch keyType {
	case gossh.KeyAlgoRSA:
		fields = 3 // type, e, n
	case gossh.KeyAlgoSKED25519:
		fields = 3 // type, key, application
	case gossh.KeyAlgoSKECDSA256:
		fields = 4 // type, curve, point, application
	default:
		return nil
	}
	parts, err := sshStrings(blob, fields)
	if err != nil {
		return fmt.Errorf("a key of type %s: %w", keyType, err)
	}
	switch keyType {
	case gossh.KeyAlgoRSA:
		for i, name := range map[int]string{1: "exponent", 2: "modulus"} {
			if m := parts[i]; len(m) == 0 || m[0]&0x80 != 0 || new(big.Int).SetBytes(m).Sign() == 0 {
				return fmt.Errorf("an RSA key's %s is not positive", name)
			}
		}
	default:
		if app := parts[fields-1]; !utf8.Valid(app) {
			return errors.New("a security key's application is not UTF-8")
		}
	}
	return nil
}

// sshStrings reads exactly n SSH wire strings (a 32-bit big-endian length,
// then that many bytes) that fill b.
func sshStrings(b []byte, n int) ([][]byte, error) {
	out := make([][]byte, 0, n)
	for i := 0; i < n; i++ {
		if len(b) < 4 {
			return nil, errors.New("the blob ends inside a length")
		}
		l := uint64(b[0])<<24 | uint64(b[1])<<16 | uint64(b[2])<<8 | uint64(b[3])
		b = b[4:]
		if uint64(len(b)) < l {
			return nil, errors.New("the blob ends inside a field")
		}
		out = append(out, b[:l])
		b = b[l:]
	}
	if len(b) != 0 {
		return nil, errors.New("the blob has bytes after its fields")
	}
	return out, nil
}

// checkLabel applies rule 9's service side: 1 to 64 bytes of UTF-8 with no
// control character.
func checkLabel(label string) error {
	if len(label) == 0 || len(label) > maxLabelBytes || !utf8.ValidString(label) {
		return fmt.Errorf("the label %q is not 1 to %d bytes of UTF-8", label, maxLabelBytes)
	}
	for _, r := range label {
		if unicode.IsControl(r) {
			return fmt.Errorf("the label %q holds a control character", label)
		}
	}
	return nil
}

// listVersion is the SHA-256 of the served list's JSON: it changes whenever
// the list's content does.
func listVersion(served []User) string {
	b, err := json.Marshal(served)
	if err != nil {
		panic(err)
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}
