package version

import "testing"

func TestBuildToolsRefForTag(t *testing.T) {
	const base = "ghcr.io/charliek/shed-build-tools"
	tests := []struct {
		name string
		in   string
		want string
	}{
		{name: "release no v-prefix (the v0.5.4 bug)", in: "0.5.4", want: base + ":v0.5.4"},
		{name: "release with v-prefix", in: "v0.5.4", want: base + ":v0.5.4"},
		{name: "whitespace tolerated", in: "  0.5.4 ", want: base + ":v0.5.4"},
		{name: "dev", in: "dev", want: ""},
		{name: "empty", in: "", want: ""},
		{name: "ahead-of-tag/dirty", in: "v0.5.4-2-g493976f", want: ""},
		{name: "dirty suffix", in: "0.5.4-dirty", want: ""},
		{name: "custom name", in: "mybuild", want: ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := BuildToolsRefForTag(tc.in); got != tc.want {
				t.Errorf("BuildToolsRefForTag(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestReleaseTag(t *testing.T) {
	tests := []struct {
		name    string
		in      string
		wantTag string
		wantOK  bool
	}{
		{name: "release no v-prefix", in: "0.6.2", wantTag: "v0.6.2", wantOK: true},
		{name: "release with v-prefix", in: "v0.6.2", wantTag: "v0.6.2", wantOK: true},
		{name: "whitespace tolerated", in: "  0.6.2 ", wantTag: "v0.6.2", wantOK: true},
		{name: "dev", in: "dev", wantTag: "", wantOK: false},
		{name: "empty", in: "", wantTag: "", wantOK: false},
		{name: "ahead-of-tag/dirty", in: "v0.6.2-2-g493976f", wantTag: "", wantOK: false},
		{name: "dirty suffix", in: "0.6.2-dirty", wantTag: "", wantOK: false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			gotTag, gotOK := ReleaseTag(tc.in)
			if gotTag != tc.wantTag || gotOK != tc.wantOK {
				t.Errorf("ReleaseTag(%q) = (%q, %v), want (%q, %v)", tc.in, gotTag, gotOK, tc.wantTag, tc.wantOK)
			}
		})
	}
}

func TestRootfsRef(t *testing.T) {
	if got := RootfsRef("vz", "full", "v0.6.2"); got != "ghcr.io/charliek/shed-vz-full:v0.6.2" {
		t.Errorf("RootfsRef(vz, full, v0.6.2) = %q", got)
	}
	if got := RootfsRef("fc", "base", "v0.6.2"); got != "ghcr.io/charliek/shed-fc-base:v0.6.2" {
		t.Errorf("RootfsRef(fc, base, v0.6.2) = %q", got)
	}
}

func TestReleaseBuildToolsRefUsesVersion(t *testing.T) {
	orig := Version
	t.Cleanup(func() { Version = orig })

	Version = "0.5.4" // release binary form (no leading v)
	if got := ReleaseBuildToolsRef(); got != "ghcr.io/charliek/shed-build-tools:v0.5.4" {
		t.Errorf("ReleaseBuildToolsRef() = %q, want the v-prefixed published tag", got)
	}
	Version = "dev"
	if got := ReleaseBuildToolsRef(); got != "" {
		t.Errorf("dev build should yield no ref, got %q", got)
	}
}
