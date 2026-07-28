package sdk

import (
	"encoding/json"
	"fmt"
	"time"
)

// Credential shapes a bootstrap can return. These mirror the server's
// config.AuthMode* constants for the two modes that issue a client credential;
// they are duplicated here because the sdk is a separate module and cannot
// import internal/.
//
// ABSENT MEANS TOKEN. A server built before client-certificate support omits
// auth_mode entirely, so an empty AuthMode must decode as the bearer-token
// shape — see Bundle.Mode.
const (
	AuthModeToken = "token"
	AuthModeMTLS  = "mtls"
)

// Bundle is the result of an SSH bootstrap exchange: a freshly-minted HTTP
// credential plus the metadata a client needs to reach the HTTP API. The
// exchange itself lives in the sdk/bootstrap sub-package (it shells out to the
// system ssh client); this type is kept here in the root package as the shared
// wire type both that helper and HTTP clients return.
//
// The json tags MUST stay in sync with the server's bootstrapBundle in
// internal/sshd/bootstrap.go. The server returns ports (not a URL) because it
// can't reliably know its own external hostname — the caller builds the base
// URL from the host it dialed plus HTTPSPort (preferred, pinned via
// TLSCertFingerprint) or HTTPPort.
//
// One struct carries both credential shapes, selected by AuthMode:
//
//	token: {auth_mode:"token", http_port?, https_port, tls_cert_fingerprint,
//	        token, scope, token_id, expires_at}
//	mtls:  {auth_mode:"mtls", https_port, tls_cert_fingerprint, client_cert,
//	        scope, cert_serial, expires_at}
//
// A token bundle never carries client_cert/cert_serial; an mtls bundle never
// carries token/token_id/http_port. The mode-specific fields are omitempty so
// a token bundle marshals byte-identically to the pre-mtls shape apart from the
// added auth_mode key.
type Bundle struct {
	// AuthMode names the credential shape carried by this bundle. Empty means
	// token — a pre-mtls server does not emit the key at all.
	AuthMode           string `json:"auth_mode,omitempty"`
	HTTPPort           int    `json:"http_port,omitempty"`
	HTTPSPort          int    `json:"https_port"`
	TLSCertFingerprint string `json:"tls_cert_fingerprint"`
	Token              string `json:"token,omitempty"`
	// ClientCert is the PEM-encoded leaf the server's internal CA issued for the
	// CSR this client submitted. Set in mtls mode only.
	ClientCert string `json:"client_cert,omitempty"`
	Scope      string `json:"scope"`
	TokenID    string `json:"token_id,omitempty"`
	// CertSerial is the issued certificate's serial in lower-case hex. Opaque to
	// the client — it exists for logs and revocation bookkeeping.
	CertSerial string    `json:"cert_serial,omitempty"`
	ExpiresAt  time.Time `json:"expires_at"`
}

// Mode returns the credential shape this bundle carries, normalizing the
// legacy absent-auth_mode case to AuthModeToken.
//
// This is the single place the "absent means token" rule is decided. Anything
// that is neither the mtls literal nor a recognized token literal also falls
// through to token: an unknown future mode is not something this client can
// act on, and treating it as mtls (the branch that expects a certificate) would
// fail more confusingly than treating it as the shape whose fields are actually
// populated.
func (b Bundle) Mode() string {
	if b.AuthMode == AuthModeMTLS {
		return AuthModeMTLS
	}
	return AuthModeToken
}

// String renders the bundle WITHOUT its secret material, so a stray %v or
// %+v in a log line can never print a bearer token. The certificate is public
// by nature but is elided too — it is long, and its serial identifies it.
//
// Bundle deliberately has NO redacting MarshalJSON, unlike Credential: it is
// the wire type in both directions, so a marshaler that dropped the token would
// break the very exchange it describes. Anything that json-encodes a raw Bundle
// is by definition producing the wire form and must carry the credential —
// which is also why a Bundle should not be handed to a structured logger as a
// value. Log the enclosing Credential (which redacts through every channel) or
// Bundle.String().
func (b Bundle) String() string {
	cred := "token=<redacted>"
	if b.Mode() == AuthModeMTLS {
		cred = "client_cert=<redacted> serial=" + b.CertSerial
	}
	return fmt.Sprintf("sdk.Bundle{auth_mode:%s scope:%s https_port:%d http_port:%d pin:%s %s expires_at:%s}",
		b.Mode(), b.Scope, b.HTTPSPort, b.HTTPPort, b.TLSCertFingerprint, cred, b.ExpiresAt.UTC().Format(time.RFC3339))
}

// GoString renders the bundle redacted under %#v.
//
// String covers %v, %+v, and %s, but %#v asks for the Go-syntax representation
// and bypasses Stringer entirely, printing every field including the token and
// the certificate. A GoStringer is the only way to close that channel — and it
// is the right one to close here, because %#v is a debugging verb whose output
// is never parsed, unlike the JSON encoding this type must keep producing
// verbatim.
func (b Bundle) GoString() string { return b.String() }

// Credential is a bootstrap result paired with the locally-generated private
// key that matches the bundle's client certificate. It is the union a client
// stores: EITHER a bearer token (Bundle.Token, KeyPEM nil) OR a certificate +
// key pair (Bundle.ClientCert + KeyPEM).
//
// The key is generated in-process by sdk/bootstrap and never leaves it except
// to the caller's credential store — the CSR carries only the public half over
// the SSH channel.
type Credential struct {
	Bundle Bundle
	// KeyPEM is the PEM-encoded EC private key matching Bundle.ClientCert.
	// Non-nil only in mtls mode.
	//
	// `json:"-"` because this key must never leave the process except into the
	// caller's credential store. Credential is a purely local type — it is
	// never decoded from the server (that is Bundle) and never encoded onto the
	// wire — so excluding the field costs nothing and removes it from every
	// accidental serialization: a debug dump, an error payload, a
	// slog.JSONHandler attribute.
	KeyPEM []byte `json:"-"`
}

// Mode reports the credential shape (see Bundle.Mode).
func (c Credential) Mode() string { return c.Bundle.Mode() }

// String renders the credential without its secret material.
func (c Credential) String() string {
	return "sdk.Credential{" + c.Bundle.String() + " key=<redacted>}"
}

// GoString renders the credential redacted under %#v.
//
// String already covers %v, %+v, and %s, but %#v asks for the Go-syntax
// representation and bypasses Stringer entirely, printing every field of every
// nested struct — the bearer token and the private key included. A GoStringer
// is the only way to close that channel.
func (c Credential) GoString() string { return c.String() }

// MarshalJSON renders the credential redacted.
//
// String and GoString cover the fmt verbs, but a structured logger
// (slog.JSONHandler) and anything that json-encodes a value reach for
// encoding/json instead — and `json:"-"` on KeyPEM alone would not save the
// bearer token nested inside Bundle, which has no redacting marshaler of its
// own because it IS the wire type.
//
// A redacting marshaler is safe here in a way it would not be on Bundle:
// Credential is never decoded from anything, so there is no round trip to
// break. The metadata is preserved so a log line stays useful — what mode, what
// scope, which certificate, when it expires — and only the two secret-bearing
// fields are replaced.
func (c Credential) MarshalJSON() ([]byte, error) {
	const redacted = "<redacted>"
	out := struct {
		AuthMode           string    `json:"auth_mode"`
		Scope              string    `json:"scope"`
		HTTPPort           int       `json:"http_port,omitempty"`
		HTTPSPort          int       `json:"https_port,omitempty"`
		TLSCertFingerprint string    `json:"tls_cert_fingerprint,omitempty"`
		TokenID            string    `json:"token_id,omitempty"`
		CertSerial         string    `json:"cert_serial,omitempty"`
		ExpiresAt          time.Time `json:"expires_at"`
		Token              string    `json:"token,omitempty"`
		ClientCert         string    `json:"client_cert,omitempty"`
		KeyPEM             string    `json:"key,omitempty"`
	}{
		AuthMode:           c.Bundle.Mode(),
		Scope:              c.Bundle.Scope,
		HTTPPort:           c.Bundle.HTTPPort,
		HTTPSPort:          c.Bundle.HTTPSPort,
		TLSCertFingerprint: c.Bundle.TLSCertFingerprint,
		TokenID:            c.Bundle.TokenID,
		CertSerial:         c.Bundle.CertSerial,
		ExpiresAt:          c.Bundle.ExpiresAt,
	}
	if c.Bundle.Token != "" {
		out.Token = redacted
	}
	if c.Bundle.ClientCert != "" {
		out.ClientCert = redacted
	}
	if len(c.KeyPEM) > 0 {
		out.KeyPEM = redacted
	}
	return json.Marshal(out)
}
