package protocol

const (
	// NamespaceAWSCredentials is the plugin namespace for AWS credential operations.
	NamespaceAWSCredentials = "aws-credentials"

	// AWS operations
	AWSOpGetCredentials = "get_credentials"
	AWSOpPing           = "ping"
	AWSOpStatus         = "status"

	// AWS error codes
	AWSCodeRoleNotFound     = "ROLE_NOT_FOUND"
	AWSCodeAssumeRoleFailed = "ASSUME_ROLE_FAILED"
	AWSCodeInternal         = "INTERNAL_ERROR"
)

// AWSCredentialsRequest is the payload for a get_credentials operation.
type AWSCredentialsRequest struct {
	Operation string `json:"operation"`
}

// AWSCredentialsResponse is the payload returned by the host handler.
// Field names use snake_case for the internal protocol; the guest proxy
// translates to the PascalCase format the AWS SDK expects.
type AWSCredentialsResponse struct {
	AccessKeyID     string `json:"access_key_id"`
	SecretAccessKey string `json:"secret_access_key"`
	SessionToken    string `json:"session_token"`
	// Expiration is RFC3339; an empty/omitted value means the credentials are
	// non-expiring as far as the guest is concerned (it discovers expiry on a
	// 403). Passthrough mode omits it when the source profile has no expiry hint.
	Expiration string `json:"expiration,omitempty"`
}

// AWSPingRequest is the payload for a health check ping.
type AWSPingRequest struct {
	Operation string `json:"operation"`
}

// AWSPingResponse is the payload for a health check pong.
type AWSPingResponse struct {
	Status string `json:"status"`
}

// AWSStatusRequest is the payload for a status query.
type AWSStatusRequest struct {
	Operation string `json:"operation"`
}

// AWSStatusResponse is the payload for a status response with detail.
type AWSStatusResponse struct {
	Connected   bool   `json:"connected"`
	Role        string `json:"role"`
	CachedUntil string `json:"cached_until,omitempty"` // RFC3339
}

// AWSErrorResponse is the payload for an error response.
type AWSErrorResponse struct {
	Error string `json:"error"`
	Code  string `json:"code"`
}
