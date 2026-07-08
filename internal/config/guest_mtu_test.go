package config

import "testing"

func TestVZConfigGuestMTUValidation(t *testing.T) {
	tests := []struct {
		name    string
		mtu     int
		wantErr bool
	}{
		{"zero is auto-detect", 0, false},
		{"floor", MinGuestMTU, false},
		{"in range", 1400, false},
		{"ceiling", MaxGuestMTU, false},
		{"below floor rejected", MinGuestMTU - 1, true},
		{"above ceiling rejected", MaxGuestMTU + 1, true},
		{"jumbo rejected", 9000, true},
		{"negative rejected", -1, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := DefaultVZConfig()
			cfg.GuestMTU = tt.mtu
			err := cfg.Validate()
			if (err != nil) != tt.wantErr {
				t.Fatalf("guest_mtu=%d: Validate() err = %v, wantErr = %v", tt.mtu, err, tt.wantErr)
			}
		})
	}
}

func TestFirecrackerConfigGuestMTUValidation(t *testing.T) {
	tests := []struct {
		name    string
		mtu     int
		wantErr bool
	}{
		{"zero is auto-detect", 0, false},
		{"floor", MinGuestMTU, false},
		{"in range", 1400, false},
		{"ceiling", MaxGuestMTU, false},
		{"below floor rejected", MinGuestMTU - 1, true},
		{"above ceiling rejected", MaxGuestMTU + 1, true},
		{"jumbo rejected", 9000, true},
		{"negative rejected", -1, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := DefaultFirecrackerConfig()
			cfg.GuestMTU = tt.mtu
			err := cfg.Validate()
			if (err != nil) != tt.wantErr {
				t.Fatalf("guest_mtu=%d: Validate() err = %v, wantErr = %v", tt.mtu, err, tt.wantErr)
			}
		})
	}
}
