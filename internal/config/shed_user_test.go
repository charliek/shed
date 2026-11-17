package config

import (
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
)

func TestValidUserName(t *testing.T) {
	tests := []struct {
		name string
		ok   bool
	}{
		{"alice", true},
		{"a", true},
		{"bob-2", true},
		{"a" + strings.Repeat("b", 62), true},
		{"a" + strings.Repeat("b", 63), false},
		{"", false},
		{"Alice", false},
		{"2bob", false},
		{"-bob", false},
		{"bob_smith", false},
		{"bob.smith", false},
		{UserUnresolved, false},
	}
	for _, tt := range tests {
		if got := ValidUserName(tt.name); got != tt.ok {
			t.Errorf("ValidUserName(%q) = %v, want %v", tt.name, got, tt.ok)
		}
	}
}

func TestValidateShedOwnership(t *testing.T) {
	tests := []struct {
		name        string
		owner, user string
		ok          bool
	}{
		{"implicit user", "", "", true},
		{"a user", "", "alice", true},
		{"a service client", "nocturn", "", true},
		{"both", "nocturn", "alice", false},
		{"invalid user name", "", "Alice", false},
		{"the unresolved marker", "", UserUnresolved, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateShedOwnership(tt.owner, tt.user)
			if (err == nil) != tt.ok {
				t.Fatalf("ValidateShedOwnership(%q, %q) = %v, want ok=%v", tt.owner, tt.user, err, tt.ok)
			}
			if err != nil && !errors.Is(err, ErrInvalidShedRequestSentinel) {
				t.Errorf("error %v does not wrap ErrInvalidShedRequestSentinel", err)
			}
		})
	}
}

// A record that names both a service client and a user fails closed: neither
// the client nor any user can match what it is reported with.
func TestShedOwnershipFailsClosedOnBoth(t *testing.T) {
	tests := []struct {
		owner, user         string
		wantOwner, wantUser string
	}{
		{"", "", "", ""},
		{"nocturn", "", "nocturn", ""},
		{"", "alice", "", "alice"},
		{"nocturn", "alice", "", UserUnresolved},
	}
	for _, tt := range tests {
		owner, user := ShedOwnership(tt.owner, tt.user)
		if owner != tt.wantOwner || user != tt.wantUser {
			t.Errorf("ShedOwnership(%q, %q) = (%q, %q), want (%q, %q)", tt.owner, tt.user, owner, user, tt.wantOwner, tt.wantUser)
		}
	}
}

// The user is reported on a shed and a snapshot, and never read from a
// create request on the wire.
func TestUserFieldWireShape(t *testing.T) {
	b, err := json.Marshal(Shed{Name: "a", User: "alice"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"user":"alice"`) {
		t.Errorf("Shed JSON %s has no user", b)
	}
	b, _ = json.Marshal(Shed{Name: "a"})
	if strings.Contains(string(b), `"user"`) {
		t.Errorf("Shed JSON for the implicit user %s carries a user key", b)
	}
	b, _ = json.Marshal(Snapshot{Name: "s", User: "alice"})
	if !strings.Contains(string(b), `"user":"alice"`) {
		t.Errorf("Snapshot JSON %s has no user", b)
	}

	var req CreateShedRequest
	if err := json.Unmarshal([]byte(`{"name":"a","user":"alice"}`), &req); err != nil {
		t.Fatal(err)
	}
	if req.User != "" {
		t.Errorf("CreateShedRequest.User = %q from the wire, want empty", req.User)
	}
	// A snapshot takes its source shed's user, so a snapshot request has
	// no user to carry, from the wire or from a caller.
	if _, ok := reflect.TypeOf(SnapshotCreateRequest{}).FieldByName("User"); ok {
		t.Error("SnapshotCreateRequest has a User field: a snapshot's user is its source's")
	}
}
