package config

import "testing"

// The server's env_file variables (its own credentials) reach the clone of
// an admin's shed only: never a service client's, never one created for a
// user who is not an admin (NoServerEnv, set by the access guard).
func TestServerEnvWithheld(t *testing.T) {
	for _, tc := range []struct {
		owner       string
		noServerEnv bool
		want        bool
	}{
		{"", false, false},
		{"nocturn", false, true},
		{"", true, true},
		{"nocturn", true, true},
	} {
		req := CreateShedRequest{NoServerEnv: tc.noServerEnv}
		if got := req.ServerEnvWithheld(tc.owner); got != tc.want {
			t.Errorf("owner %q noServerEnv %v: withheld %v, want %v", tc.owner, tc.noServerEnv, got, tc.want)
		}
	}
}

// ActiveImageAliases reads the active backend's aliases only.
func TestActiveImageAliases(t *testing.T) {
	cfg := &ServerConfig{
		DefaultBackend: BackendFirecracker,
		Firecracker:    &FirecrackerConfig{ImageAliases: map[string]string{"fc": "ghcr.io/x/fc:v1"}},
		VZ:             &VZConfig{ImageAliases: map[string]string{"vz": "ghcr.io/x/vz:v1"}},
	}
	if a := cfg.ActiveImageAliases(); len(a) != 1 || a["fc"] == "" {
		t.Errorf("firecracker aliases = %v", a)
	}
	cfg.DefaultBackend = BackendVZ
	if a := cfg.ActiveImageAliases(); len(a) != 1 || a["vz"] == "" {
		t.Errorf("vz aliases = %v", a)
	}
	cfg.DefaultBackend = BackendRemote
	if a := cfg.ActiveImageAliases(); a != nil {
		t.Errorf("remote with no remote config = %v", a)
	}
}
