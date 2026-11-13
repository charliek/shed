package config

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func TestShedIDAndStatusReasonJSON(t *testing.T) {
	tests := []struct {
		name     string
		shed     Shed
		contains []string
		absent   []string
	}{
		{"local shed has neither", Shed{Name: "a", Status: StatusRunning, Backend: BackendFirecracker},
			nil, []string{`"id"`, `"status_reason"`}},
		{"remote shed has an id", Shed{Name: "a", ID: "abcdefghijklmnopqrstuvwxyz", Status: StatusStopping, Backend: BackendRemote},
			[]string{`"id":"abcdefghijklmnopqrstuvwxyz"`, `"status":"stopping"`, `"backend":"remote"`}, []string{`"status_reason"`}},
		{"failed create has a reason", Shed{Name: "a", ID: "abcdefghijklmnopqrstuvwxyz", Status: StatusError, StatusReason: StatusReasonCapacity},
			[]string{`"status":"error"`, `"status_reason":"capacity"`}, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			data, err := json.Marshal(tt.shed)
			if err != nil {
				t.Fatal(err)
			}
			for _, want := range tt.contains {
				if !strings.Contains(string(data), want) {
					t.Errorf("JSON %s lacks %s", data, want)
				}
			}
			for _, not := range tt.absent {
				if strings.Contains(string(data), not) {
					t.Errorf("JSON %s has %s", data, not)
				}
			}
			var back Shed
			if err := json.Unmarshal(data, &back); err != nil {
				t.Fatal(err)
			}
			if back.ID != tt.shed.ID || back.StatusReason != tt.shed.StatusReason {
				t.Errorf("round trip lost id or reason: %+v", back)
			}
		})
	}
}

// TestCreateShedRequestIDIsServerSet checks that a client cannot choose a
// shed's ID: the field is neither read from nor written to JSON.
func TestCreateShedRequestIDIsServerSet(t *testing.T) {
	var req CreateShedRequest
	if err := json.Unmarshal([]byte(`{"name":"a","id":"abcdefghijklmnopqrstuvwxyz","ID":"x"}`), &req); err != nil {
		t.Fatal(err)
	}
	if req.ID != "" {
		t.Errorf("request body set ID to %q", req.ID)
	}
	req.ID = "abcdefghijklmnopqrstuvwxyz"
	data, err := json.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "abcdefghijklmnopqrstuvwxyz") {
		t.Errorf("ID leaked into JSON: %s", data)
	}
}

func TestRemoteErrorCodes(t *testing.T) {
	tests := []struct {
		code     string
		want     string
		sentinel error
	}{
		{ErrServerLimitReached, "SERVER_LIMIT_REACHED", ErrServerLimitReachedSentinel},
		{ErrCloudQuotaExceeded, "CLOUD_QUOTA_EXCEEDED", ErrCloudQuotaExceededSentinel},
		{ErrZoneCapacityUnavailable, "ZONE_CAPACITY_UNAVAILABLE", ErrZoneCapacityUnavailableSentinel},
		{ErrFeatureUnsupported, "FEATURE_UNSUPPORTED", ErrFeatureUnsupportedSentinel},
	}
	sentinels := make([]error, 0, len(tests))
	for _, tt := range tests {
		t.Run(tt.want, func(t *testing.T) {
			if tt.code != tt.want {
				t.Errorf("code = %q, want %q", tt.code, tt.want)
			}
			if tt.sentinel == nil || tt.sentinel.Error() == "" {
				t.Fatal("sentinel is empty")
			}
			// Distinct from every other sentinel, including the older
			// "not supported" one that maps to 501 BACKEND_ERROR.
			for _, other := range append(sentinels, ErrNotSupportedSentinel) {
				if errors.Is(tt.sentinel, other) {
					t.Errorf("%v matches %v", tt.sentinel, other)
				}
			}
		})
		sentinels = append(sentinels, tt.sentinel)
	}
}
