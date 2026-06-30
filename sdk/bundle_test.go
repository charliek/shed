package sdk

import (
	"encoding/json"
	"testing"
)

func TestBundleJSONTagsMatchServer(t *testing.T) {
	// Guards the cross-module contract: the wire tags the server emits must
	// decode here. (The server side lives in internal/sshd/bootstrap.go.)
	const serverJSON = `{"http_port":80,"https_port":443,"tls_cert_fingerprint":"sha256:f","token":"t","scope":"credentials","token_id":"id","expires_at":"2030-01-02T03:04:05Z"}`
	var b Bundle
	if err := json.Unmarshal([]byte(serverJSON), &b); err != nil {
		t.Fatal(err)
	}
	if b.HTTPPort != 80 || b.HTTPSPort != 443 || b.TLSCertFingerprint != "sha256:f" ||
		b.Token != "t" || b.Scope != "credentials" || b.TokenID != "id" || b.ExpiresAt.IsZero() {
		t.Errorf("decoded bundle mismatch: %+v", b)
	}
}
