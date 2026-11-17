package refserver

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"unicode/utf8"
)

// Enrolment limits (contract section 1).
const (
	maxEnrolBody = 8 << 10
	maxCodeBytes = 64
	maxNameBytes = 255
)

// handleEnroll answers POST /v1/servers/enroll. It checks, in order: the
// rate limit (before the body is read), the media type, the size, the JSON,
// each field and the key, and only then the code, so a request refused
// before the code is looked up leaves the code unspent.
func (s *Server) handleEnroll(w http.ResponseWriter, r *http.Request) {
	if ok, ra := s.allow(limitEnroll, callerOf(r)); !ok {
		writeV1Error(w, http.StatusTooManyRequests, "RATE_LIMITED", "too many enrolment requests", ra)
		return
	}
	if mt, _, err := mime.ParseMediaType(r.Header.Get("Content-Type")); err != nil || mt != "application/json" {
		writeV1Error(w, http.StatusUnsupportedMediaType, "UNSUPPORTED_MEDIA_TYPE", "the body must be application/json", 0)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, maxEnrolBody+1))
	if err != nil {
		writeV1Error(w, http.StatusBadRequest, "INVALID_REQUEST", "the body did not arrive whole", 0)
		return
	}
	if len(body) > maxEnrolBody {
		writeV1Error(w, http.StatusRequestEntityTooLarge, "PAYLOAD_TOO_LARGE", "the body is over 8 KiB", 0)
		return
	}
	codeText, keyRaw, err := decodeEnrolRequest(body)
	if err != nil {
		s.logf("enrolment refused: %v", err)
		writeV1Error(w, http.StatusBadRequest, "INVALID_REQUEST", "the body is not an enrolment request", 0)
		return
	}
	key, err := parseJWK(keyRaw)
	if err != nil {
		s.logf("enrolment refused: public_key: %v", err)
		writeV1Error(w, http.StatusBadRequest, "INVALID_PUBLIC_KEY", "public_key is not an accepted key", 0)
		return
	}

	code, found, err := s.inputs.Code(codeText)
	if err != nil {
		s.logf("enrolment: the code lookup failed: %v", err)
		writeUnavailable(w)
		return
	}
	if !found {
		writeV1Error(w, http.StatusUnauthorized, "ENROLMENT_CODE_INVALID", "no such enrolment code", 0)
		return
	}

	s.mu.Lock()
	clientID, status, errCode, err := s.enrolLocked(codeText, code, key)
	s.mu.Unlock()
	if err != nil {
		s.logf("enrolment: a lookup failed: %v", err)
		writeUnavailable(w)
		return
	}
	if status != http.StatusCreated {
		writeV1Error(w, status, errCode, "enrolment refused", 0)
		return
	}
	s.onEnrol(code.ServerName, clientID)
	writeJSON(w, http.StatusCreated, map[string]any{
		"contract":       contractNumber,
		"client_id":      clientID,
		"issuer":         s.issuer,
		"token_endpoint": s.issuer + pathToken,
		"organization":   s.org,
	}, true)
}

// enrolLocked decides an enrolment and, when it succeeds, spends the code
// and enrols the server, all under s.mu: two requests with one code enrol
// once.
func (s *Server) enrolLocked(codeText string, code Code, key *clientKey) (clientID string, status int, errCode string, err error) {
	switch {
	case s.spentCodes[codeText] != "":
		return "", http.StatusConflict, "ENROLMENT_CODE_SPENT", nil
	case code.Cancelled:
		return "", http.StatusGone, "ENROLMENT_CODE_CANCELLED", nil
	case !code.ExpiresAt.IsZero() && !s.now().Before(code.ExpiresAt):
		return "", http.StatusGone, "ENROLMENT_CODE_EXPIRED", nil
	}
	for _, srv := range s.servers {
		if srv.name != code.ServerName {
			continue
		}
		disabled, err := s.inputs.Disabled(srv.clientID)
		if err != nil {
			return "", 0, "", err
		}
		if !disabled {
			return "", http.StatusConflict, "SERVER_NAME_IN_USE", nil
		}
	}
	// A key held by any enrolled server, disabled or not, is in use: a
	// disabled server comes back only with a new key pair.
	for _, srv := range s.servers {
		if srv.key.identity == key.identity {
			return "", http.StatusConflict, "PUBLIC_KEY_IN_USE", nil
		}
	}
	clientID = "srv_" + randomHex(16)
	s.servers[clientID] = &server{name: code.ServerName, clientID: clientID, key: key}
	s.spentCodes[codeText] = clientID
	return clientID, http.StatusCreated, "", nil
}

// decodeEnrolRequest reads the body and returns its code and public key:
// one JSON object, no member named twice (known or not), code and name
// within their limits, and public_key present. The key itself is left to
// parseJWK, so that every fault of the key, its size and its shape
// included, is INVALID_PUBLIC_KEY.
func decodeEnrolRequest(body []byte) (string, json.RawMessage, error) {
	if !utf8.Valid(body) {
		return "", nil, errors.New("the body is not valid UTF-8")
	}
	members, err := topLevelMembers(body)
	if err != nil {
		return "", nil, err
	}
	for name, raw := range members {
		if name == "public_key" {
			continue
		}
		if err := checkStrictJSON(raw, 64); err != nil {
			return "", nil, err
		}
	}
	code, ok := jsonString(members["code"])
	if !ok || len(code) == 0 || len(code) > maxCodeBytes {
		return "", nil, errors.New("code is not a string of 1 to 64 bytes")
	}
	name, ok := jsonString(members["name"])
	if !ok || len(name) == 0 || len(name) > maxNameBytes {
		return "", nil, errors.New("name is not 1 to 255 printable ASCII characters")
	}
	for i := 0; i < len(name); i++ {
		if name[i] < 0x20 || name[i] > 0x7e {
			return "", nil, errors.New("name is not 1 to 255 printable ASCII characters")
		}
	}
	key, ok := members["public_key"]
	if !ok {
		return "", nil, errors.New("no public_key")
	}
	return code, key, nil
}

// topLevelMembers reads a JSON object's members as raw values, refusing a
// member named twice and anything after the object.
func topLevelMembers(body []byte) (map[string]json.RawMessage, error) {
	dec := json.NewDecoder(bytes.NewReader(body))
	tok, err := dec.Token()
	if err != nil {
		return nil, err
	}
	if d, ok := tok.(json.Delim); !ok || d != '{' {
		return nil, errors.New("the body is not a JSON object")
	}
	members := map[string]json.RawMessage{}
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			return nil, err
		}
		name, _ := tok.(string)
		if _, dup := members[name]; dup {
			return nil, errors.New("a member is named twice")
		}
		var raw json.RawMessage
		if err := dec.Decode(&raw); err != nil {
			return nil, err
		}
		members[name] = raw
	}
	if _, err := dec.Token(); err != nil {
		return nil, err
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, errors.New("data after the JSON object")
	}
	return members, nil
}

// addServer enrols a server outside the enrolment endpoint: the tests' way
// of loading the contract's world, never reachable over HTTP.
func (s *Server) addServer(name, clientID string, jwk []byte) error {
	key, err := parseJWK(jwk)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, dup := s.servers[clientID]; dup {
		return errors.New("client id enrolled twice")
	}
	s.servers[clientID] = &server{name: name, clientID: clientID, key: key}
	return nil
}

// spendCode marks a code spent by a server, as addServer does for the
// world's spent codes.
func (s *Server) spendCode(code, clientID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.spentCodes[code] = clientID
}
