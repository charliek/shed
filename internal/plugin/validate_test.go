package plugin

import "testing"

func TestValidateNamespace(t *testing.T) {
	tests := []struct {
		name      string
		namespace string
		wantErr   bool
	}{
		{"valid simple", "my-plugin", false},
		{"valid with dots", "com.example.op", false},
		{"valid with colons", "my:plugin", false},
		{"valid single char", "x", false},
		{"valid max length", string(make([]byte, 128)), true}, // all null bytes are non-printable
		{"empty", "", true},
		{"system prefix", "system:test", true},
		{"system exact", "system:", true},
		{"contains space", "my plugin", true},
		{"contains tab", "my\tplugin", true},
		{"non-ascii", "my-plügin", true},
		{"control char", "my\x00plugin", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateNamespace(tt.namespace)
			if (err != nil) != tt.wantErr {
				t.Errorf("ValidateNamespace(%q) error = %v, wantErr = %v", tt.namespace, err, tt.wantErr)
			}
		})
	}
}

func TestValidateNamespaceMaxLength(t *testing.T) {
	// Exactly 128 printable ASCII chars should be fine
	ns := make([]byte, 128)
	for i := range ns {
		ns[i] = 'a'
	}
	if err := ValidateNamespace(string(ns)); err != nil {
		t.Errorf("128 chars should be valid: %v", err)
	}

	// 129 chars should fail
	ns = make([]byte, 129)
	for i := range ns {
		ns[i] = 'a'
	}
	if err := ValidateNamespace(string(ns)); err == nil {
		t.Error("129 chars should be invalid")
	}
}

func TestIsSystemNamespace(t *testing.T) {
	tests := []struct {
		namespace string
		want      bool
	}{
		{"system:credentials", true},
		{"system:health", true},
		{"system:", true},
		{"my-plugin", false},
		{"systematic", false},
		{"", false},
	}

	for _, tt := range tests {
		t.Run(tt.namespace, func(t *testing.T) {
			if got := IsSystemNamespace(tt.namespace); got != tt.want {
				t.Errorf("IsSystemNamespace(%q) = %v, want %v", tt.namespace, got, tt.want)
			}
		})
	}
}
