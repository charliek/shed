package protocol

const (
	// NamespaceDockerCredentials is the plugin namespace for Docker credential operations.
	NamespaceDockerCredentials = "docker-credentials"

	// Docker operations
	DockerOpGet    = "get"
	DockerOpList   = "list"
	DockerOpPing   = "ping"
	DockerOpStatus = "status"

	// Docker error codes
	DockerCodeNotFound     = "CREDENTIALS_NOT_FOUND"
	DockerCodeNotAllowed   = "REGISTRY_NOT_ALLOWED"
	DockerCodeReadOnly     = "READ_ONLY"
	DockerCodeHelperFailed = "HELPER_FAILED"
	DockerCodeInternal     = "INTERNAL_ERROR"
)

// DockerGetRequest is the payload for a get operation.
type DockerGetRequest struct {
	Operation string `json:"operation"`
	ServerURL string `json:"server_url"`
}

// DockerGetResponse is the payload returned for a successful credential lookup.
type DockerGetResponse struct {
	ServerURL string `json:"server_url"`
	Username  string `json:"username"`
	Secret    string `json:"secret"`
}

// DockerListRequest is the payload for a list operation.
type DockerListRequest struct {
	Operation string `json:"operation"`
}

// DockerListResponse is the payload returned for a list operation.
type DockerListResponse struct {
	Registries map[string]string `json:"registries"`
}

// DockerPingRequest is the payload for a health check ping.
type DockerPingRequest struct {
	Operation string `json:"operation"`
}

// DockerPingResponse is the payload for a health check pong.
type DockerPingResponse struct {
	Status string `json:"status"`
}

// DockerStatusRequest is the payload for a status query.
type DockerStatusRequest struct {
	Operation string `json:"operation"`
}

// DockerStatusResponse is the payload for a status response with detail.
type DockerStatusResponse struct {
	Connected     bool `json:"connected"`
	AllowAll      bool `json:"allow_all"`
	RegistryCount int  `json:"registry_count"`
}

// DockerErrorResponse is the payload for an error response.
type DockerErrorResponse struct {
	Error string `json:"error"`
	Code  string `json:"code"`
}
