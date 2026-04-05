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
