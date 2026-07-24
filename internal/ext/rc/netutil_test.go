package rc

import "testing"

func TestFreeLoopbackPort(t *testing.T) {
	port, err := freeLoopbackPort()
	if err != nil {
		t.Fatalf("freeLoopbackPort() error = %v", err)
	}
	if port < 1 || port > 65535 {
		t.Fatalf("freeLoopbackPort() = %d, want 1..65535", port)
	}
}
