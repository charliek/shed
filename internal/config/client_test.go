package config

import "testing"

func TestServerEntryBaseURL(t *testing.T) {
	tests := []struct {
		name  string
		entry ServerEntry
		want  string
	}{
		{
			name:  "legacy plain http from host+port",
			entry: ServerEntry{Host: "host.example", HTTPPort: 8080},
			want:  "http://host.example:8080",
		},
		{
			name:  "api_url overrides scheme+host+port",
			entry: ServerEntry{Host: "host.example", HTTPPort: 8080, APIURL: "https://host.example:8443"},
			want:  "https://host.example:8443",
		},
		{
			name:  "api_url trailing slash trimmed",
			entry: ServerEntry{Host: "host.example", HTTPPort: 8080, APIURL: "https://host.example:8443/"},
			want:  "https://host.example:8443",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.entry.BaseURL(); got != tt.want {
				t.Errorf("BaseURL() = %q, want %q", got, tt.want)
			}
		})
	}
}
