package protocol

const (
	// NamespaceSSHAgent is the plugin namespace for SSH agent operations.
	NamespaceSSHAgent = "ssh-agent"

	// SSH operations
	SSHOpList   = "list"
	SSHOpSign   = "sign"
	SSHOpPing   = "ping"
	SSHOpStatus = "status"

	// SSH error codes
	SSHCodeKeyNotFound = "KEY_NOT_FOUND"
	SSHCodeSignFailed  = "SIGN_FAILED"
	SSHCodeInternal    = "INTERNAL_ERROR"
)

// SSHSignRequest is the payload for a sign operation.
type SSHSignRequest struct {
	Operation string `json:"operation"`
	PublicKey string `json:"public_key"`
	Data      string `json:"data"` // base64-encoded challenge
	Flags     uint32 `json:"flags"`
}

// SSHListRequest is the payload for a list operation.
type SSHListRequest struct {
	Operation string `json:"operation"`
}

// SSHPingRequest is the payload for a health check ping.
type SSHPingRequest struct {
	Operation string `json:"operation"`
}

// SSHSignResponse is the payload for a successful sign response.
type SSHSignResponse struct {
	Format string `json:"format"`
	Blob   string `json:"blob"` // base64-encoded signature
	Rest   string `json:"rest"`
}

// SSHListResponse is the payload for a successful list response.
type SSHListResponse struct {
	Keys []SSHKeyInfo `json:"keys"`
}

// SSHKeyInfo describes a single SSH public key.
type SSHKeyInfo struct {
	Format  string `json:"format"`
	Blob    string `json:"blob"` // base64-encoded marshaled public key
	Comment string `json:"comment"`
}

// SSHPingResponse is the payload for a health check pong.
type SSHPingResponse struct {
	Status string `json:"status"`
}

// SSHStatusRequest is the payload for a status query.
type SSHStatusRequest struct {
	Operation string `json:"operation"`
}

// SSHStatusResponse is the payload for a status response with detail.
type SSHStatusResponse struct {
	Connected bool   `json:"connected"`
	Mode      string `json:"mode"` // "agent-forward" or "local-keys"
	KeyCount  int    `json:"key_count"`
}

// SSHErrorResponse is the payload for an error response.
type SSHErrorResponse struct {
	Error string `json:"error"`
	Code  string `json:"code"`
}
