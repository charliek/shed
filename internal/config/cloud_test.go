package config

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestFormatLeft(t *testing.T) {
	for _, tc := range []struct {
		d    time.Duration
		want string
	}{
		{-time.Minute, "0m"},
		{0, "0m"},
		{59 * time.Second, "0m"},
		{20 * time.Minute, "20m"},
		{80 * time.Minute, "1h 20m"},
		{5*time.Hour + 10*time.Minute, "5h 10m"},
		{26*time.Hour + 30*time.Minute, "1d 2h"},
	} {
		if got := FormatLeft(tc.d); got != tc.want {
			t.Errorf("FormatLeft(%v) = %q, want %q", tc.d, got, tc.want)
		}
	}
}

func TestLeaseBanner(t *testing.T) {
	now := time.Date(2026, 11, 26, 13, 10, 0, 0, time.UTC)
	// warnS < 0: absent.
	shed := func(left time.Duration, extendable bool, warnS int64) *Shed {
		l := &CloudLease{ExpiresAt: now.Add(left), MaxExpiresAt: now.Add(100 * time.Hour), Extendable: extendable}
		if warnS >= 0 {
			l.WarnS = &warnS
		}
		return &Shed{Name: "widgets", Cloud: &ShedCloud{Lease: l}}
	}
	// The plan's words exactly.
	line, ok := LeaseBanner(shed(80*time.Minute, true, -1), now)
	want := "shed: this cloud shed is deleted at 14:30 UTC (in 1h 20m); extend with: shed extend widgets --hours N"
	if !ok || line != want {
		t.Fatalf("banner = %q, %v\nwant %q", line, ok, want)
	}
	// Not extendable (a client's machine, one at its cap): no advice.
	if line, ok := LeaseBanner(shed(80*time.Minute, false, -1), now); !ok || strings.Contains(line, "extend") {
		t.Fatalf("a lease that cannot be extended: %q, %v", line, ok)
	}
	// Outside the warning time, none: the default 2 h when the record
	// names none, the service's, and none at all for an explicit 0.
	for _, tc := range []struct {
		left  time.Duration
		warnS int64
		shown bool
	}{
		{2*time.Hour + time.Minute, -1, false},
		{2*time.Hour - time.Minute, -1, true},
		{50 * time.Minute, 1800, false},
		{20 * time.Minute, 1800, true},
		{-time.Minute, -1, true},
		{90 * time.Minute, 0, false},
		{time.Minute, 0, false},
	} {
		if _, ok := LeaseBanner(shed(tc.left, true, tc.warnS), now); ok != tc.shown {
			t.Errorf("%v left (warn %ds): shown %v, want %v", tc.left, tc.warnS, ok, tc.shown)
		}
	}
	// Another day carries the date.
	if line, _ := LeaseBanner(shed(11*time.Hour, true, 86400), now); !strings.Contains(line, "deleted at 2026-11-27 00:10 UTC (in 11h 0m)") {
		t.Fatalf("a lease ending tomorrow: %q", line)
	}
	// Not a cloud shed, or no lease: none.
	for _, s := range []*Shed{nil, {Name: "local"}, {Name: "x", Cloud: &ShedCloud{}}, {Name: "x", Cloud: &ShedCloud{Lease: &CloudLease{}}}} {
		if _, ok := LeaseBanner(s, now); ok {
			t.Errorf("a banner for %+v", s)
		}
	}
}

// A local shed's record carries no cloud member at all; a cloud shed's
// carries the plan's shape.
func TestShedCloudJSON(t *testing.T) {
	raw, _ := json.Marshal(Shed{Name: "local"})
	if strings.Contains(string(raw), "cloud") {
		t.Fatalf("a local record: %s", raw)
	}
	at := time.Date(2026, 11, 26, 9, 0, 0, 0, time.UTC)
	warn := int64(7200)
	raw, _ = json.Marshal(ShedCloud{Provider: "gce", Size: CloudSize{Name: "standard", CPUs: 2, MemoryMB: 8192}, DiskGB: 50, Image: "img",
		CreatedAt: at, Lease: &CloudLease{ExpiresAt: at, MaxExpiresAt: at, Extendable: true, PresetsH: []int{6}, WarnS: &warn}})
	want := `{"provider":"gce","size":{"name":"standard","cpus":2,"memory_mb":8192},"disk_gb":50,"image":"img","created_at":"2026-11-26T09:00:00Z",` +
		`"lease":{"expires_at":"2026-11-26T09:00:00Z","max_expires_at":"2026-11-26T09:00:00Z","extendable":true,"presets_h":[6],"warn_s":7200}}`
	if string(raw) != want {
		t.Fatalf("cloud = %s\nwant  %s", raw, want)
	}
	raw, _ = json.Marshal(CloudUsage{Org: CloudCounts{Machines: 1, Max: 10}})
	if string(raw) != `{"person":null,"org":{"machines":1,"running":0,"stopped":0,"max":10}}` {
		t.Fatalf("usage = %s", raw)
	}
}

// warn_s: absent is the default, an explicit 0 is no warning, on the wire.
func TestLeaseWarnPresence(t *testing.T) {
	var absent, zero CloudLease
	if err := json.Unmarshal([]byte(`{"expires_at":"2026-11-26T09:00:00Z"}`), &absent); err != nil || absent.Warn() != DefaultLeaseWarn {
		t.Fatalf("absent: %v %v", absent.Warn(), err)
	}
	if err := json.Unmarshal([]byte(`{"expires_at":"2026-11-26T09:00:00Z","warn_s":0}`), &zero); err != nil || zero.Warn() != 0 {
		t.Fatalf("zero: %v %v", zero.Warn(), err)
	}
	if raw, _ := json.Marshal(zero); !strings.Contains(string(raw), `"warn_s":0`) {
		t.Fatalf("an explicit 0 is not sent: %s", raw)
	}
}

func TestValidUUID(t *testing.T) {
	for s, want := range map[string]bool{
		"6f9619ff-8b86-4d01-b42d-00cf4fc964ff": true, "6F9619FF-8B86-4D01-B42D-00CF4FC964FF": true,
		"": false, "x": false, "6f9619ff8b864d01b42d00cf4fc964ff": false, "6f9619ff-8b86-4d01-b42d-00cf4fc964fg": false,
	} {
		if ValidUUID(s) != want {
			t.Errorf("ValidUUID(%q) = %v", s, !want)
		}
	}
}
