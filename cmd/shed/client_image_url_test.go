package main

import "testing"

// TestImageIdentURL locks the client half of the rm/inspect-by-ref fix: a
// slash-bearing Docker ref must go out as a ?ref= query (url-encoded), while
// slash-free identifiers (digests, cosmetic tags) keep the path form.
func TestImageIdentURL(t *testing.T) {
	tests := []struct {
		name       string
		collection string
		ident      string
		want       string
	}{
		{
			name:       "ref with slashes -> query",
			collection: "/api/images",
			ident:      "ghcr.io/charliek/shed-vz-full:v0.6.0",
			want:       "/api/images?ref=ghcr.io%2Fcharliek%2Fshed-vz-full%3Av0.6.0",
		},
		{
			name:       "inspect ref with slashes -> query",
			collection: "/api/images/inspect",
			ident:      "ghcr.io/charliek/shed-vz-full:v0.6.0",
			want:       "/api/images/inspect?ref=ghcr.io%2Fcharliek%2Fshed-vz-full%3Av0.6.0",
		},
		{
			name:       "digest -> path",
			collection: "/api/images",
			ident:      "sha256:2d9669bcf0cd",
			want:       "/api/images/sha256:2d9669bcf0cd",
		},
		{
			name:       "cosmetic tag -> path",
			collection: "/api/images",
			ident:      "full",
			want:       "/api/images/full",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := imageIdentURL(tt.collection, tt.ident); got != tt.want {
				t.Errorf("imageIdentURL(%q, %q) = %q, want %q", tt.collection, tt.ident, got, tt.want)
			}
		})
	}
}
