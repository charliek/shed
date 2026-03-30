package plugin

// NamespaceCredentials is the reserved namespace for credential sync messages.
const NamespaceCredentials = "system:credentials"

// CredentialSetupPayload is the payload for a system:credentials setup request.
// Sent host → agent on connection establishment to configure file watchers.
type CredentialSetupPayload struct {
	Credentials map[string]string   `json:"credentials"`        // name -> target path in VM
	Excludes    map[string][]string `json:"excludes,omitempty"` // name -> exclude patterns
}

// CredentialChangedPayload is the payload for a system:credentials change event.
// Sent agent → host when credential files change inside the VM.
type CredentialChangedPayload struct {
	Credential string   `json:"credential"` // credential name
	Files      []string `json:"files"`      // relative paths that changed
}
