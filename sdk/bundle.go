package sdk

import "time"

// Bundle is the result of an SSH bootstrap exchange: a freshly-minted HTTP
// bearer token plus the metadata a client needs to reach the HTTP API. The
// exchange itself lives in the sdk/bootstrap sub-package (it shells out to the
// system ssh client); this type is kept here in the root package as the shared
// wire type both that helper and HTTP clients return.
//
// The json tags MUST stay in sync with the server's bootstrapBundle in
// internal/sshd/bootstrap.go. The server returns ports (not a URL) because it
// can't reliably know its own external hostname — the caller builds the base
// URL from the host it dialed plus HTTPSPort (preferred, pinned via
// TLSCertFingerprint) or HTTPPort.
type Bundle struct {
	HTTPPort           int       `json:"http_port,omitempty"`
	HTTPSPort          int       `json:"https_port"`
	TLSCertFingerprint string    `json:"tls_cert_fingerprint"`
	Token              string    `json:"token"`
	Scope              string    `json:"scope"`
	TokenID            string    `json:"token_id"`
	ExpiresAt          time.Time `json:"expires_at"`
}
