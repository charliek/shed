package config

import (
	"testing"
	"time"
)

// TestDeliveryTimeout: delivery.timeout defaults to 30 s, takes a set value,
// and is refused outside 1 s to 10 min.
func TestDeliveryTimeout(t *testing.T) {
	if got := (&ServerConfig{}).DeliveryTimeout(); got != 30*time.Second {
		t.Fatalf("default = %s", got)
	}
	for _, tc := range []struct {
		d  time.Duration
		ok bool
	}{{0, true}, {time.Second, true}, {45 * time.Second, true}, {10 * time.Minute, true},
		{500 * time.Millisecond, false}, {-time.Second, false}, {11 * time.Minute, false}} {
		c := &ServerConfig{Delivery: &DeliveryConfig{Timeout: Duration(tc.d)}}
		if err := c.validateDelivery(); (err == nil) != tc.ok {
			t.Errorf("delivery.timeout %s: %v, want ok %v", tc.d, err, tc.ok)
		}
		if tc.ok && tc.d > 0 && c.DeliveryTimeout() != tc.d {
			t.Errorf("DeliveryTimeout() = %s, want %s", c.DeliveryTimeout(), tc.d)
		}
	}
}
