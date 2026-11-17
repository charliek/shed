package refserver

import (
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
)

// maxJWKBytes is the most a client's public key may take, as sent.
const maxJWKBytes = 2048

// maxJWKDepth is the deepest a client's public key may nest.
const maxJWKDepth = 8

// clientKey is an enrolled server's public key.
type clientKey struct {
	alg      string // the one algorithm its assertions are signed with
	ed       ed25519.PublicKey
	ec       *ecdsa.PublicKey
	kid      string
	hasKid   bool
	identity string // the key type and its bytes, for "already enrolled"
}

// verify checks a JWS signature made with the key over the signing input.
func (k *clientKey) verify(signingInput string, sig []byte) bool {
	switch k.alg {
	case "EdDSA":
		return len(sig) == ed25519.SignatureSize && ed25519.Verify(k.ed, []byte(signingInput), sig)
	case "ES256":
		if len(sig) != 64 {
			return false
		}
		sum := sha256.Sum256([]byte(signingInput))
		r, s := new(big.Int).SetBytes(sig[:32]), new(big.Int).SetBytes(sig[32:])
		return ecdsa.Verify(k.ec, sum[:], r, s)
	}
	return false
}

// coordinate decodes a JWK coordinate: unpadded base64url of exactly 32
// bytes, with no stray bits.
func coordinate(raw json.RawMessage) ([]byte, error) {
	s, ok := jsonString(raw)
	if !ok {
		return nil, errors.New("a coordinate is not a string")
	}
	b, err := decodeB64URL(s)
	if err != nil || len(b) != 32 {
		return nil, errors.New("a coordinate is not unpadded base64url of 32 bytes")
	}
	return b, nil
}

// parseJWK reads a client's public key as the enrolment request sent it,
// refusing everything section 1 of the contract refuses.
func parseJWK(raw json.RawMessage) (*clientKey, error) {
	if len(raw) > maxJWKBytes {
		return nil, fmt.Errorf("the key takes %d bytes, more than %d", len(raw), maxJWKBytes)
	}
	if err := checkStrictJSON(raw, maxJWKDepth); err != nil {
		return nil, err
	}
	m, err := objectMembers(raw)
	if err != nil {
		return nil, err
	}
	kty, _ := jsonString(m["kty"])
	crv, _ := jsonString(m["crv"])
	allowed := map[string]bool{"kty": true, "crv": true, "x": true, "kid": true, "alg": true, "use": true, "key_ops": true}
	k := &clientKey{}
	switch {
	case kty == "OKP" && crv == "Ed25519":
		k.alg = "EdDSA"
		x, err := coordinate(m["x"])
		if err != nil {
			return nil, err
		}
		if err := checkEd25519Point(x); err != nil {
			return nil, err
		}
		k.ed = ed25519.PublicKey(x)
		k.identity = "Ed25519:" + string(x)
	case kty == "EC" && crv == "P-256":
		k.alg = "ES256"
		allowed["y"] = true
		x, err := coordinate(m["x"])
		if err != nil {
			return nil, err
		}
		y, err := coordinate(m["y"])
		if err != nil {
			return nil, err
		}
		point := append(append([]byte{4}, x...), y...)
		pub, err := ecdsa.ParseUncompressedPublicKey(elliptic.P256(), point)
		if err != nil {
			return nil, errors.New("the point is not on P-256")
		}
		k.ec = pub
		k.identity = "P-256:" + string(point)
	default:
		return nil, errors.New("the key is neither an Ed25519 OKP key nor a P-256 EC key")
	}
	for name := range m {
		if !allowed[name] {
			// The private members (d, p, q, dp, dq, qi, k) fall here too:
			// a client that sends one has leaked its key.
			return nil, fmt.Errorf("the key has the member %q", name)
		}
	}
	if raw, ok := m["kid"]; ok {
		if k.kid, ok = jsonString(raw); !ok {
			return nil, errors.New("kid is not a string")
		}
		k.hasKid = true
	}
	if raw, ok := m["alg"]; ok {
		if alg, ok := jsonString(raw); !ok || alg != k.alg {
			return nil, errors.New("alg is not the key's own")
		}
	}
	if raw, ok := m["use"]; ok {
		if use, ok := jsonString(raw); !ok || use != "sig" {
			return nil, errors.New("use is not sig")
		}
	}
	if raw, ok := m["key_ops"]; ok {
		var ops []json.RawMessage
		if err := json.Unmarshal(raw, &ops); err != nil || len(raw) == 0 || raw[0] != '[' {
			return nil, errors.New("key_ops is not an array")
		}
		seen := map[string]bool{}
		for _, o := range ops {
			op, ok := jsonString(o)
			if !ok || (op != "verify" && op != "sign") || seen[op] {
				return nil, errors.New("key_ops holds something other than verify and sign, once each")
			}
			seen[op] = true
		}
		if !seen["verify"] {
			return nil, errors.New("key_ops does not hold verify")
		}
	}
	return k, nil
}

// The field and the small-order points of edwards25519. A point's encoding
// is its y coordinate, little-endian, with x's sign in the top bit.
var (
	ed25519P = new(big.Int).Sub(new(big.Int).Lsh(big.NewInt(1), 255), big.NewInt(19))
	// ed25519Order8Y is the y coordinate of two of the four points of order
	// 8; p minus it is the other two's. It is a root of d*y^4 + 2*y^2 - 1
	// (a point that doubles to y = 0), which the tests check.
	ed25519Order8Y, _ = new(big.Int).SetString("2707385501144840649318225287225658788936804267575313519463743609750303402022", 10)
	// ed25519SmallOrderY are the y coordinates of every point of order 1, 2,
	// 4 or 8: the identity (1), order 2 (p-1), order 4 (0) and order 8.
	ed25519SmallOrderY = []*big.Int{
		big.NewInt(0),
		big.NewInt(1),
		new(big.Int).Sub(ed25519P, big.NewInt(1)),
		ed25519Order8Y,
		new(big.Int).Sub(ed25519P, ed25519Order8Y),
	}
)

// checkEd25519Point refuses an Ed25519 public key whose encoding is not
// canonical (y not below p) or that is a point of small order. Whether y
// has an x on the curve at all is left to signature verification, which
// fails for every assertion of such a key (rule 11: both conform).
func checkEd25519Point(x []byte) error {
	le := append([]byte(nil), x...)
	le[31] &= 0x7f
	be := make([]byte, 32)
	for i := range le {
		be[31-i] = le[i]
	}
	y := new(big.Int).SetBytes(be)
	if y.Cmp(ed25519P) >= 0 {
		return errors.New("the Ed25519 key is not canonically encoded")
	}
	for _, small := range ed25519SmallOrderY {
		if y.Cmp(small) == 0 {
			return errors.New("the Ed25519 key is a point of small order")
		}
	}
	return nil
}
