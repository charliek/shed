package api

import (
	"errors"
	"fmt"
	"net/http"
	"testing"

	"github.com/charliek/shed/internal/config"
)

// A refusal that names a feature the backend lacks is 501
// FEATURE_UNSUPPORTED, even when it also carries the generic "not
// supported" sentinel (the remote backend's assign on a cloud shed wraps
// both); the generic one alone stays 501 BACKEND_ERROR.
func TestFeatureUnsupportedWinsOverNotSupported(t *testing.T) {
	both := fmt.Errorf("%w: %w: assign on the remote backend", config.ErrFeatureUnsupportedSentinel, config.ErrNotSupportedSentinel)
	cases := []struct {
		err  error
		code string
	}{
		{both, config.ErrFeatureUnsupported},
		{fmt.Errorf("%w: x", config.ErrFeatureUnsupportedSentinel), config.ErrFeatureUnsupported},
		{fmt.Errorf("%w: x", config.ErrNotSupportedSentinel), config.ErrBackendError},
	}
	for _, c := range cases {
		status, code, _ := mapBackendError(c.err)
		if status != http.StatusNotImplemented || code != c.code {
			t.Errorf("%v: %d %s, want 501 %s", c.err, status, code, c.code)
		}
	}
	if !errors.Is(both, config.ErrNotSupportedSentinel) {
		t.Fatal("the test error lost a sentinel")
	}
}
