package config

import "testing"

func TestExtensionsConfigValidate(t *testing.T) {
	tests := []struct {
		name    string
		config  *ExtensionsConfig
		wantErr bool
	}{
		{
			name: "valid with multiple namespaces",
			config: &ExtensionsConfig{
				Enabled: []string{"ssh-agent", "aws-credentials", "my-extension"},
			},
			wantErr: false,
		},
		{
			name: "empty namespace rejected",
			config: &ExtensionsConfig{
				Enabled: []string{"ssh-agent", ""},
			},
			wantErr: true,
		},
		{
			name: "duplicate namespace rejected",
			config: &ExtensionsConfig{
				Enabled: []string{"ssh-agent", "ssh-agent"},
			},
			wantErr: true,
		},
		{
			name: "system prefix rejected",
			config: &ExtensionsConfig{
				Enabled: []string{"system:health"},
			},
			wantErr: true,
		},
		{
			name: "spaces in namespace rejected",
			config: &ExtensionsConfig{
				Enabled: []string{"my extension"},
			},
			wantErr: true,
		},
		{
			name: "non-ASCII characters rejected",
			config: &ExtensionsConfig{
				Enabled: []string{"my-plügin"},
			},
			wantErr: true,
		},
		{
			name: "empty enabled list is valid",
			config: &ExtensionsConfig{
				Enabled: []string{},
			},
			wantErr: false,
		},
		{
			name: "nil enabled list is valid",
			config: &ExtensionsConfig{
				Enabled: nil,
			},
			wantErr: false,
		},
		{
			name: "valid with dots and colons",
			config: &ExtensionsConfig{
				Enabled: []string{"com.example.ext", "my:extension"},
			},
			wantErr: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.config.Validate()
			if (err != nil) != tt.wantErr {
				t.Errorf("Validate() error = %v, wantErr = %v", err, tt.wantErr)
			}
		})
	}
}

func TestGitConfigValidate(t *testing.T) {
	tests := []struct {
		name    string
		config  *GitConfig
		wantErr bool
	}{
		{
			name:    "nil extras is valid",
			config:  &GitConfig{ExtraKnownHosts: nil},
			wantErr: false,
		},
		{
			name:    "empty extras is valid",
			config:  &GitConfig{ExtraKnownHosts: []string{}},
			wantErr: false,
		},
		{
			name: "valid ed25519 line",
			config: &GitConfig{ExtraKnownHosts: []string{
				"gitlab.com ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIAfuCHKVTjquxvt6CM6tdG4SLp1Btn/nOeHHE5UOzRdf",
			}},
			wantErr: false,
		},
		{
			name: "valid ecdsa line",
			config: &GitConfig{ExtraKnownHosts: []string{
				"example.com ecdsa-sha2-nistp256 AAAAE2VjZHNhLXNoYTItbmlzdHAyNTYAAAAIbmlzdHAyNTY=",
			}},
			wantErr: false,
		},
		{
			name: "valid rsa line with trailing comment",
			config: &GitConfig{ExtraKnownHosts: []string{
				"my-gitea.internal ssh-rsa AAAAB3NzaC1yc2E= operator@laptop",
			}},
			wantErr: false,
		},
		{
			name:    "empty line rejected",
			config:  &GitConfig{ExtraKnownHosts: []string{""}},
			wantErr: true,
		},
		{
			name:    "whitespace-only line rejected",
			config:  &GitConfig{ExtraKnownHosts: []string{"   "}},
			wantErr: true,
		},
		{
			name:    "single field rejected",
			config:  &GitConfig{ExtraKnownHosts: []string{"github.com"}},
			wantErr: true,
		},
		{
			name:    "two fields rejected",
			config:  &GitConfig{ExtraKnownHosts: []string{"github.com ssh-ed25519"}},
			wantErr: true,
		},
		{
			name: "unknown key type rejected",
			config: &GitConfig{ExtraKnownHosts: []string{
				"github.com ssh-magic AAAAB3NzaC1y",
			}},
			wantErr: true,
		},
		{
			name: "garbage rejected",
			config: &GitConfig{ExtraKnownHosts: []string{
				"this is not a known_hosts line",
			}},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.config.Validate()
			if (err != nil) != tt.wantErr {
				t.Errorf("Validate() error = %v, wantErr = %v", err, tt.wantErr)
			}
		})
	}
}
