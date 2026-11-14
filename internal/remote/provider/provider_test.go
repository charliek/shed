package provider

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

func testIdentity() Identity {
	return Identity{Env: "dev-home", ID: "abcdefghijklmnopqrstuvwx23", Name: "my-shed", Owner: "nocturn"}
}

func TestIdentityLabelsRoundTrip(t *testing.T) {
	tests := []struct {
		name string
		id   Identity
	}{
		{"owned", testIdentity()},
		{"admin shed has an empty owner", Identity{Env: "prod", ID: "a1b2c3d4", Name: "x"}},
		{"longest names", Identity{
			Env:   "e" + strings.Repeat("n", 19),
			ID:    strings.Repeat("7", 32),
			Name:  "s" + strings.Repeat("h", 62),
			Owner: strings.Repeat("o", 63),
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := tt.id.Validate(); err != nil {
				t.Fatalf("Validate: %v", err)
			}
			labels := tt.id.Labels(ShedStateCreating)
			if err := checkLabels(labels); err != nil {
				t.Fatalf("labels are not legal: %v", err)
			}
			if _, ok := labels[LabelOwner]; !ok {
				t.Errorf("label %s must be present even when empty", LabelOwner)
			}
			if _, ok := labels[LabelLayout]; ok {
				t.Errorf("label %s must not be set at create", LabelLayout)
			}
			got, err := IdentityFromLabels(labels)
			if err != nil {
				t.Fatalf("IdentityFromLabels: %v", err)
			}
			if got != tt.id {
				t.Errorf("round trip = %+v, want %+v", got, tt.id)
			}
			if n := len(tt.id.InstanceName()); n > 63 {
				t.Errorf("instance name is %d characters, over 63", n)
			}
		})
	}
}

func TestIdentityRejects(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*Identity)
	}{
		{"empty env", func(i *Identity) { i.Env = "" }},
		{"env starts with a digit", func(i *Identity) { i.Env = "1dev" }},
		{"env ends with a hyphen", func(i *Identity) { i.Env = "dev-" }},
		{"env too long", func(i *Identity) { i.Env = strings.Repeat("e", 21) }},
		{"env upper case", func(i *Identity) { i.Env = "Dev" }},
		{"id too short", func(i *Identity) { i.ID = "abc" }},
		{"id with a hyphen", func(i *Identity) { i.ID = "abcd-efgh" }},
		{"empty name", func(i *Identity) { i.Name = "" }},
		{"name with a dot", func(i *Identity) { i.Name = "a.b" }},
		{"owner with a space", func(i *Identity) { i.Owner = "a b" }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			id := testIdentity()
			tt.mutate(&id)
			if err := id.Validate(); !errors.Is(err, ErrInvalid) {
				t.Errorf("Validate = %v, want ErrInvalid", err)
			}
		})
	}
}

func TestIdentityFromLabelsSchema(t *testing.T) {
	tests := []struct {
		name   string
		schema string
		set    bool
	}{
		{"missing", "", false},
		{"unknown", "2", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			labels := testIdentity().Labels(ShedStateReady)
			delete(labels, LabelSchema)
			if tt.set {
				labels[LabelSchema] = tt.schema
			}
			if _, err := IdentityFromLabels(labels); !errors.Is(err, ErrInvalid) {
				t.Errorf("IdentityFromLabels = %v, want ErrInvalid", err)
			}
		})
	}
}

func TestNames(t *testing.T) {
	if got := InstanceName("dev-home", "abcd1234"); got != "shed-dev-home-abcd1234" {
		t.Errorf("InstanceName = %q", got)
	}
	if got := NetworkTag("dev-home"); got != "shed-dev-home" {
		t.Errorf("NetworkTag = %q", got)
	}
	if got := FirewallRuleName("dev-home"); got != "shed-dev-home-ssh" {
		t.Errorf("FirewallRuleName = %q", got)
	}
}

func TestStripRepoUserinfo(t *testing.T) {
	tests := []struct {
		in, want string
	}{
		{"https://token@host/o/r.git", "https://host/o/r.git"},
		{"https://u:p@host/o/r.git", "https://host/o/r.git"},
		{"https://u:p@ss@host:8443/o/r.git", "https://host:8443/o/r.git"},
		{"https://host/o/r.git", "https://host/o/r.git"},
		{"https://token@host", "https://host"},
		{"ssh://git@host/o/r.git", "ssh://git@host/o/r.git"},
		{"ssh://git:pw@host/o/r.git", "ssh://git@host/o/r.git"},
		{"git@github.com:o/r.git", "git@github.com:o/r.git"},
		{"o/r", "o/r"},
		{"", ""},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			if got := StripRepoUserinfo(tt.in); got != tt.want {
				t.Errorf("StripRepoUserinfo(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestShedSpecRoundTrip(t *testing.T) {
	created := time.Date(2026, 11, 13, 4, 5, 6, 0, time.FixedZone("x", -6*3600))
	in := ShedSpec{
		CreatedAt:  created,
		Repo:       "https://secret@github.com/o/r.git",
		Image:      "shed-extensions-v1",
		Size:       SizeStandard,
		Network:    "isolated",
		LandingDir: "/home/shed/r",
	}
	encoded, err := in.Encode()
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	if strings.Contains(encoded, "secret") {
		t.Fatalf("encoded spec keeps the credential: %s", encoded)
	}
	got, err := DecodeShedSpec(encoded)
	if err != nil {
		t.Fatalf("DecodeShedSpec: %v", err)
	}
	if got.Schema != ShedSpecSchema {
		t.Errorf("Schema = %d, want %d", got.Schema, ShedSpecSchema)
	}
	if !got.CreatedAt.Equal(created) || got.CreatedAt.Location() != time.UTC {
		t.Errorf("CreatedAt = %v, want %v in UTC", got.CreatedAt, created)
	}
	if got.Repo != "https://github.com/o/r.git" || got.Network != "isolated" || got.LandingDir != "/home/shed/r" {
		t.Errorf("decoded spec = %+v", got)
	}

	for _, bad := range []string{"", "   ", "{", `{"schema":0}`, `{"schema":99}`} {
		t.Run("rejects "+bad, func(t *testing.T) {
			if _, err := DecodeShedSpec(bad); !errors.Is(err, ErrInvalid) {
				t.Errorf("DecodeShedSpec(%q) = %v, want ErrInvalid", bad, err)
			}
		})
	}
}

func TestNewSpec(t *testing.T) {
	id := testIdentity()
	size := DefaultSizes()[0]
	shed := ShedSpec{CreatedAt: time.Unix(1700000000, 0), Network: "open", Repo: "https://u:p@host/o/r.git"}
	const key = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIexample shed-server"

	spec, err := NewSpec(id, shed, []string{key, "  "}, size, DefaultDiskGB, "shed-extensions-v1", 24*time.Hour)
	if err != nil {
		t.Fatalf("NewSpec: %v", err)
	}
	if spec.Name != "shed-dev-home-"+id.ID {
		t.Errorf("Name = %q", spec.Name)
	}
	if spec.Labels[LabelState] != string(ShedStateCreating) {
		t.Errorf("shed_state = %q, want creating", spec.Labels[LabelState])
	}
	if spec.Metadata[MetadataShedName] != id.Name || spec.Metadata[MetadataShedID] != id.ID {
		t.Errorf("metadata identity = %v", spec.Metadata)
	}
	if spec.Metadata[MetadataAuthorizedKeys] != key {
		t.Errorf("authorized keys = %q, want the one non-blank key", spec.Metadata[MetadataAuthorizedKeys])
	}
	if _, ok := spec.Metadata[MetadataHostKeyPin]; ok {
		t.Errorf("the host key pin must not be set at create")
	}
	decoded, err := DecodeShedSpec(spec.Metadata[MetadataShedSpec])
	if err != nil {
		t.Fatalf("DecodeShedSpec: %v", err)
	}
	if decoded.Image != "shed-extensions-v1" || decoded.Size != SizeStandard || decoded.Repo != "https://host/o/r.git" {
		t.Errorf("shed-spec = %+v", decoded)
	}

	tests := []struct {
		name   string
		mutate func(*Spec)
	}{
		{"name does not match labels", func(s *Spec) { s.Name = "other" }},
		{"state is not creating", func(s *Spec) { s.Labels[LabelState] = string(ShedStateReady) }},
		{"no authorized key", func(s *Spec) { s.Metadata[MetadataAuthorizedKeys] = "" }},
		{"no machine type", func(s *Spec) { s.Size.MachineType = "" }},
		{"no disk", func(s *Spec) { s.DiskGB = 0 }},
		{"no image", func(s *Spec) { s.Image = "" }},
		{"negative run limit", func(s *Spec) { s.MaxRun = -1 }},
		{"foreign metadata key", func(s *Spec) { s.Metadata["ssh-keys"] = "x" }},
		{"metadata too large", func(s *Spec) { s.Metadata[MetadataAuthorizedKeys] = strings.Repeat("k", MaxMetadataBytes) }},
		{"bad shed-spec", func(s *Spec) { s.Metadata[MetadataShedSpec] = "{" }},
		{"illegal label value", func(s *Spec) { s.Labels["extra"] = "Upper" }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			bad, err := NewSpec(id, shed, []string{key}, size, DefaultDiskGB, "shed-extensions-v1", 0)
			if err != nil {
				t.Fatalf("NewSpec: %v", err)
			}
			tt.mutate(&bad)
			if err := bad.Validate(); !errors.Is(err, ErrInvalid) {
				t.Errorf("Validate = %v, want ErrInvalid", err)
			}
		})
	}
}

func TestResolveSize(t *testing.T) {
	sizes := DefaultSizes()
	tests := []struct {
		name     string
		cpus, mb int
		want     string
		wantErr  bool
	}{
		{"nothing asked gives the default", 0, 0, SizeStandard, false},
		{"exact standard", 2, 8192, SizeStandard, false},
		{"smaller rounds up to standard", 1, 1024, SizeStandard, false},
		{"cpus only", 4, 0, SizeLarge, false},
		{"memory only", 0, 16384, SizeLarge, false},
		{"exact large", 8, 32768, SizeLarge, false},
		{"too many cpus", 16, 0, "", true},
		{"too much memory", 2, 65536, "", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ResolveSize(sizes, SizeStandard, tt.cpus, tt.mb)
			if tt.wantErr {
				if !errors.Is(err, ErrInvalid) {
					t.Fatalf("err = %v, want ErrInvalid", err)
				}
				if !strings.Contains(err.Error(), SizeLarge) {
					t.Errorf("error should name the sizes: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("ResolveSize: %v", err)
			}
			if got.Name != tt.want {
				t.Errorf("size = %q, want %q", got.Name, tt.want)
			}
		})
	}
	t.Run("unknown default", func(t *testing.T) {
		if _, err := ResolveSize(sizes, "huge", 0, 0); !errors.Is(err, ErrInvalid) {
			t.Errorf("err = %v, want ErrInvalid", err)
		}
	})
}

func TestErrorKindsAndReasons(t *testing.T) {
	tests := []struct {
		kind    error
		reason  string
		refusal bool
	}{
		{ErrQuota, ReasonQuota, true},
		{ErrCapacity, ReasonCapacity, true},
		{ErrLimit, ReasonLimit, true},
		{ErrNotFound, "", false},
		{ErrBusy, "", false},
		{ErrForeign, "", false},
	}
	for _, tt := range tests {
		t.Run(tt.kind.Error(), func(t *testing.T) {
			err := fmt.Errorf("job: %w", Errorf(tt.kind, "wait", "zone %s", "z"))
			if !errors.Is(err, tt.kind) {
				t.Errorf("errors.Is lost the kind: %v", err)
			}
			if got := Reason(err); got != tt.reason {
				t.Errorf("Reason = %q, want %q", got, tt.reason)
			}
			if got := IsRefusal(err); got != tt.refusal {
				t.Errorf("IsRefusal = %v, want %v", got, tt.refusal)
			}
			var pe *Error
			if !errors.As(err, &pe) || pe.Op != "wait" {
				t.Errorf("errors.As did not find the *Error: %v", err)
			}
		})
	}
	t.Run("context error stays visible", func(t *testing.T) {
		err := &Error{Op: "wait", Err: context.DeadlineExceeded}
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("errors.Is(DeadlineExceeded) = false for %v", err)
		}
		if Reason(err) != "" {
			t.Errorf("a timeout is not a refusal")
		}
	})
}

func TestStateSettled(t *testing.T) {
	settled := map[State]bool{
		StateRunning: true, StateStopped: true, StateAbsent: true,
		StateStarting: false, StateStopping: false, StateUnknown: false,
	}
	for state, want := range settled {
		t.Run(string(state), func(t *testing.T) {
			if got := state.Settled(); got != want {
				t.Errorf("Settled = %v, want %v", got, want)
			}
		})
	}
}

func TestInstanceAccessors(t *testing.T) {
	id := testIdentity()
	labels := id.Labels(ShedStateReady)
	labels[LabelLayout] = LayoutLabel(1)
	spec, err := ShedSpec{CreatedAt: time.Unix(1700000000, 0), Image: "img", Size: SizeLarge, Network: "open"}.Encode()
	if err != nil {
		t.Fatal(err)
	}
	inst := Instance{Labels: labels, Metadata: map[string]string{
		MetadataShedSpec:   spec,
		MetadataHostKeyPin: "ssh-ed25519 AAAA",
	}}
	if got, err := inst.Identity(); err != nil || got != id {
		t.Errorf("Identity = %+v, %v", got, err)
	}
	if inst.ShedState() != ShedStateReady {
		t.Errorf("ShedState = %q", inst.ShedState())
	}
	if inst.Layout() != 1 {
		t.Errorf("Layout = %d", inst.Layout())
	}
	if (Instance{}).Layout() != 0 {
		t.Errorf("Layout with no label should be 0")
	}
	if inst.HostKeyPin() != "ssh-ed25519 AAAA" {
		t.Errorf("HostKeyPin = %q", inst.HostKeyPin())
	}
	if got, err := inst.ShedSpec(); err != nil || got.Size != SizeLarge {
		t.Errorf("ShedSpec = %+v, %v", got, err)
	}
}
