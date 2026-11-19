package main

import (
	"errors"
	"testing"

	"github.com/charliek/shed/internal/config"
	"github.com/charliek/shed/sdk"
)

// TestServerListJSONCarriesIdentityFile: `shed --json server list` carries an
// entry's identity_file, as add, update and a refetch report it, so a script
// can tell two entries for one host apart; an entry with none omits the
// member.
func TestServerListJSONCarriesIdentityFile(t *testing.T) {
	testClientConfig(t)
	withCleanAddFlags(t)
	stubBootstrapResult(sdk.Credential{}, errors.New("test: ssh is not reachable"))
	key := writeKey(t, t.TempDir(), "id_admin")
	port := freeClosedPort(t)
	putServerEntry(t, "work", config.ServerEntry{Host: "127.0.0.1", HTTPPort: port, SSHPort: 2222})
	putServerEntry(t, "work-admin", config.ServerEntry{Host: "127.0.0.1", HTTPPort: port, SSHPort: 2222, IdentityFile: key})

	rows := listJSON(t)
	if got, ok := rows["work-admin"]["identity_file"]; !ok || got != key {
		t.Errorf("work-admin: identity_file = %v (present %v), want %q", got, ok, key)
	}
	if got, ok := rows["work"]["identity_file"]; ok {
		t.Errorf("work: identity_file = %v, want the member omitted", got)
	}
}
