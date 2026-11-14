package remote

import (
	"errors"
	"testing"

	"github.com/charliek/shed/internal/config"
)

// An egress selection of exactly ["off"] asks for no profile. The API lets it
// through to a backend without egress profiles, so the backend must too.
func TestCreateEgressSelection(t *testing.T) {
	tests := []struct {
		name    string
		egress  []string
		refused bool
	}{
		{"none", nil, false},
		{"off", []string{"off"}, false},
		{"a profile", []string{"web"}, true},
		{"off and a profile", []string{"off", "web"}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			e := newEnv(t)
			_, err := e.b.CreateShed(ctxT(t), config.CreateShedRequest{Name: "egress", NoProvision: true, Egress: tt.egress})
			if got := errors.Is(err, config.ErrFeatureUnsupportedSentinel); got != tt.refused {
				t.Fatalf("CreateShed(egress=%v) = %v, refused=%v, want refused=%v", tt.egress, err, got, tt.refused)
			}
			if !tt.refused && err != nil {
				t.Fatalf("CreateShed(egress=%v): %v", tt.egress, err)
			}
		})
	}
}
