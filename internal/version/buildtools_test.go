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
