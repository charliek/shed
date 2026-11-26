package provider

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"testing"
	"time"
)

const (
	testShedID = "abcdefghijklmnopqrstuvwx23"
	testKey    = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIexample shed-server"
)

func testShedSpec() ShedSpec {
	return ShedSpec{CreatedAt: time.Unix(1700000000, 0), Network: "open", Repo: "https://u:p@host/o/r.git", UserOrigin: "directory"}
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
		Network:    "isolated",
		LandingDir: "/home/shed/r",
		UserOrigin: "directory",
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
	if got.Repo != "https://github.com/o/r.git" || got.Network != "isolated" || got.LandingDir != "/home/shed/r" || got.UserOrigin != "directory" {
		t.Errorf("decoded spec = %+v", got)
	}

	for _, bad := range []string{"", "   ", "{", `{"schema":0}`, `{"schema":1}`, `{"schema":99}`} {
		t.Run("rejects "+bad, func(t *testing.T) {
			if _, err := DecodeShedSpec(bad); !errors.Is(err, ErrInvalid) {
				t.Errorf("DecodeShedSpec(%q) = %v, want ErrInvalid", bad, err)
			}
		})
	}
	t.Run("too long to store", func(t *testing.T) {
		long := in
		long.Repo = "https://github.com/o/" + strings.Repeat("r", MaxSpec)
		if _, err := long.Encode(); !errors.Is(err, ErrInvalid) {
			t.Errorf("Encode of an over-long spec = %v, want ErrInvalid", err)
		}
	})
}

func TestNewSpec(t *testing.T) {
	spec, err := NewSpec(testShedID, "my-shed", Owner{User: "ada"}, testShedSpec(), []string{testKey, "  "}, "standard", 0, "")
	if err != nil {
		t.Fatalf("NewSpec: %v", err)
	}
	if !regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`).MatchString(spec.RequestID) {
		t.Errorf("RequestID = %q, want a lower-case random UUID", spec.RequestID)
	}
	if spec.ShedID != testShedID || spec.Name != "my-shed" || spec.User != "ada" || spec.Client != "" {
		t.Errorf("identity = %+v", spec)
	}
	if spec.Image != ImageDefault {
		t.Errorf("Image = %q, want %q for none", spec.Image, ImageDefault)
	}
	if spec.Labels[LabelState] != string(ShedStateCreating) || len(spec.Labels) != 1 {
		t.Errorf("labels = %v, want shed_state=creating alone", spec.Labels)
	}
	if len(spec.AuthorizedKeys) != 1 || spec.AuthorizedKeys[0] != testKey {
		t.Errorf("authorized keys = %q, want the one non-blank key", spec.AuthorizedKeys)
	}
	if spec.LeaseS != 0 {
		t.Errorf("a person's create carries a lease: %d", spec.LeaseS)
	}
	decoded, err := DecodeShedSpec(spec.Spec)
	if err != nil {
		t.Fatalf("DecodeShedSpec: %v", err)
	}
	if decoded.Repo != "https://host/o/r.git" || decoded.UserOrigin != "directory" {
		t.Errorf("spec = %+v", decoded)
	}

	client, err := NewSpec(testShedID, "nocturn-1", Owner{Client: "nocturn", Lease: 30 * time.Hour}, testShedSpec(), []string{testKey}, "standard", 50, "cloud")
	if err != nil {
		t.Fatalf("NewSpec for a client: %v", err)
	}
	if client.LeaseS != 30*3600 || client.Client != "nocturn" || client.DiskGB != 50 {
		t.Errorf("client spec = %+v", client)
	}

	tests := []struct {
		name   string
		mutate func(*Spec)
	}{
		{"no request id", func(s *Spec) { s.RequestID = "" }},
		{"bad shed id", func(s *Spec) { s.ShedID = "abc" }},
		{"bad name", func(s *Spec) { s.Name = "My_Shed" }},
		{"user and client", func(s *Spec) { s.Client = "nocturn" }},
		{"bad user", func(s *Spec) { s.User = "Ada" }},
		{"a person with a lease", func(s *Spec) { s.LeaseS = 3600 }},
		{"no size", func(s *Spec) { s.Size = "" }},
		{"no image", func(s *Spec) { s.Image = "" }},
		{"disk too large", func(s *Spec) { s.DiskGB = MaxDiskGB + 1 }},
		{"state is not creating", func(s *Spec) { s.Labels[LabelState] = string(ShedStateReady) }},
		{"a reserved label", func(s *Spec) { s.Labels["shed_user"] = "ada" }},
		{"an illegal label value", func(s *Spec) { s.Labels["extra"] = "Upper" }},
		{"no authorized key", func(s *Spec) { s.AuthorizedKeys = nil }},
		{"five authorized keys", func(s *Spec) { s.AuthorizedKeys = []string{testKey, testKey, testKey, testKey, testKey} }},
		{"a key over two lines", func(s *Spec) { s.AuthorizedKeys = []string{testKey + "\n" + testKey} }},
		{"no access kind", func(s *Spec) { s.AccessKinds = nil }},
		{"a spec that is not a shed spec", func(s *Spec) { s.Spec = "{" }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			bad := *spec.Clone()
			tt.mutate(&bad)
			if err := bad.Validate(); !errors.Is(err, ErrInvalid) {
				t.Errorf("Validate = %v, want ErrInvalid", err)
			}
		})
	}
	t.Run("a client without a lease", func(t *testing.T) {
		bad := *client.Clone()
		bad.LeaseS = 0
		if err := bad.Validate(); !errors.Is(err, ErrInvalid) {
			t.Errorf("Validate = %v, want ErrInvalid", err)
		}
	})
}

func TestSameCreate(t *testing.T) {
	a, err := NewSpec(testShedID, "my-shed", Owner{User: "ada"}, testShedSpec(), []string{testKey}, "standard", 0, "")
	if err != nil {
		t.Fatal(err)
	}
	b := *a.Clone()
	b.RequestID = NewRequestID()
	if !SameCreate(a, b) {
		t.Errorf("two creates that differ only in request id are not the same")
	}
	b.AuthorizedKeys = append(b.AuthorizedKeys, testKey)
	if SameCreate(a, b) {
		t.Errorf("two creates with different keys are the same")
	}
}

func TestCatalog(t *testing.T) {
	c := Catalog{
		Sizes:   []Size{{Name: "standard", CPUs: 2, MemoryMB: 8192, Arch: ArchAMD64}, {Name: "large", CPUs: 8, MemoryMB: 32768, Arch: ArchAMD64}},
		Aliases: []string{"base", "cloud"},
	}
	if c.GuestArch() != ArchAMD64 {
		t.Errorf("GuestArch = %q", c.GuestArch())
	}
	c.Sizes = append(c.Sizes, Size{Name: "arm", CPUs: 2, MemoryMB: 8192, Arch: ArchARM64})
	if c.GuestArch() != "" {
		t.Errorf("GuestArch of mixed sizes = %q, want none", c.GuestArch())
	}
	for image, want := range map[string]bool{"": true, ImageDefault: true, "cloud": true, "other": false} {
		if got := c.HasImage(image); got != want {
			t.Errorf("HasImage(%q) = %v, want %v", image, got, want)
		}
	}
	if s, ok := c.SizeNamed("large"); !ok || s.CPUs != 8 {
		t.Errorf("SizeNamed(large) = %+v, %v", s, ok)
	}
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
		{ErrComputeLimit, ReasonLimit, true},
		{ErrNotFound, "", false},
		{ErrBusy, "", false},
		{ErrPolicy, "", false},
	}
	for _, tt := range tests {
		t.Run(tt.kind.Error(), func(t *testing.T) {
			err := fmt.Errorf("job: %w", Errorf(tt.kind, "wait", "machine %s", "x"))
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
	t.Run("the code travels", func(t *testing.T) {
		err := fmt.Errorf("job: %w", &Error{Kind: ErrComputeLimit, Op: "create", Code: "QUOTA_PERSON"})
		if CodeOf(err) != "QUOTA_PERSON" || !strings.Contains(err.Error(), "QUOTA_PERSON") {
			t.Errorf("CodeOf = %q, text %q", CodeOf(err), err)
		}
		if CodeOf(errors.New("x")) != "" {
			t.Errorf("CodeOf of a plain error is not empty")
		}
	})
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
		StateStarting: false, StateStopping: false, StateDeleting: false, StateUnknown: false,
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
	spec, err := testShedSpec().Encode()
	if err != nil {
		t.Fatal(err)
	}
	inst := Instance{
		Labels: map[string]string{LabelState: string(ShedStateReady), LabelLayout: LayoutLabel(1)},
		Spec:   spec,
		Access: []Access{{Kind: AccessSSH, Host: "203.0.113.7", Port: 22}},
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
	if got, err := inst.ShedSpec(); err != nil || got.UserOrigin != "directory" {
		t.Errorf("ShedSpec = %+v, %v", got, err)
	}
	cp := inst.Clone()
	cp.Labels[LabelState] = "x"
	cp.Access[0].Host = "x"
	if inst.Labels[LabelState] != string(ShedStateReady) || inst.Access[0].Host != "203.0.113.7" {
		t.Errorf("Clone shares state with its source")
	}
}

func TestCheckLabels(t *testing.T) {
	if err := CheckLabels(map[string]*string{LabelState: Str("ready"), "old": nil}); err != nil {
		t.Errorf("CheckLabels of a legal write: %v", err)
	}
	for _, k := range ReservedLabels {
		if err := CheckLabels(map[string]*string{k: Str("x")}); err == nil {
			t.Errorf("CheckLabels allows the reserved label %s", k)
		}
	}
	if err := CheckLabels(map[string]*string{"Bad": nil}); err == nil {
		t.Errorf("CheckLabels allows a bad key")
	}
}
