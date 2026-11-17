package servertls

// clientuser.go is the user a client certificate was issued to.
//
// A user's client certificate names the SSH key it was enrolled with (the
// Subject CN, a key fingerprint), and every request resolves that key to a
// user through the live user lookup. The key alone is not enough: a key can
// change hands (an operator moves it to another user and restarts with the
// same CA, or a GitHub refresh finds it under another user's login), and a
// certificate whose TLS private key only the first user holds must not then
// authenticate as the second. So the certificate also names the user it was
// issued to, and the server accepts it only while its key still resolves to
// that same user.
//
// The user is carried in the Subject as one userId attribute (RFC 4519 "uid",
// OID 0.9.2342.19200300.100.1.1): a registered attribute that means exactly
// this, which every X.509 parser reads as an ordinary subject attribute. The
// clients in this repository never look inside a certificate's subject: the
// Go CLI and SDK store the PEM and compare only its public key with their
// private key, the Rust credential stores read only its validity window, and
// rustls presents the DER as it was issued. The attribute travels with the
// certificate untouched.
//
// The value is the user's name, or ImplicitUserValue for the implicit user of
// a server with no user list (whose name, "", cannot be an attribute value).
// A user name is never ImplicitUserValue, so a certificate issued to the
// implicit user never reads as a named user's, nor the reverse.
//
// A certificate issued before this attribute existed has none (Bound false).
// It is accepted under the fingerprint rule alone until it expires; the
// server never issues one again.

import (
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/asn1"
	"errors"
	"regexp"
)

// ClientUserOID is the userId attribute type (RFC 4519) that carries, in a
// user's client certificate subject, the user the certificate was issued to.
var ClientUserOID = asn1.ObjectIdentifier{0, 9, 2342, 19200300, 100, 1, 1}

// ImplicitUserValue is the userId value of a certificate issued to the
// implicit user (the one user of a server with no user list). It is not a
// valid user name.
const ImplicitUserValue = "!implicit"

// userNamePattern mirrors config.ValidUserName (this package does not import
// internal/config).
var userNamePattern = regexp.MustCompile(`^[a-z][a-z0-9-]{0,62}$`)

var (
	// ErrCAInvalidUser rejects issuance for a user name that is not a valid
	// one. Like ErrCAInvalidSubject it is a server-side bug, not client input.
	ErrCAInvalidUser = errors.New("ca: user is not a valid user name")
	// ErrClientUserMalformed is returned for a certificate whose subject
	// carries the user attribute more than once, or with a value that is
	// neither a valid user name nor ImplicitUserValue. It must be refused.
	ErrClientUserMalformed = errors.New("client certificate: malformed user attribute")
)

// clientUserAttribute is the subject attribute naming user ("" for the
// implicit user).
func clientUserAttribute(user string) (pkix.AttributeTypeAndValue, error) {
	value := ImplicitUserValue
	if user != "" {
		if !userNamePattern.MatchString(user) {
			return pkix.AttributeTypeAndValue{}, ErrCAInvalidUser
		}
		value = user
	}
	return pkix.AttributeTypeAndValue{Type: ClientUserOID, Value: value}, nil
}

// ClientCertUser returns the user a client certificate was issued to: user
// is the user's name, "" for the implicit user, and bound is true. A
// certificate with no user attribute (issued before it existed) returns
// bound false and no error. A malformed attribute returns
// ErrClientUserMalformed.
func ClientCertUser(leaf *x509.Certificate) (user string, bound bool, err error) {
	if leaf == nil {
		return "", false, ErrClientUserMalformed
	}
	found := false
	for _, atv := range leaf.Subject.Names {
		if !atv.Type.Equal(ClientUserOID) {
			continue
		}
		if found {
			return "", false, ErrClientUserMalformed
		}
		found = true
		v, ok := atv.Value.(string)
		switch {
		case !ok:
			return "", false, ErrClientUserMalformed
		case v == ImplicitUserValue:
			user = ""
		case userNamePattern.MatchString(v):
			user = v
		default:
			return "", false, ErrClientUserMalformed
		}
	}
	return user, found, nil
}
