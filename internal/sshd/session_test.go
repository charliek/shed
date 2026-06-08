package sshd

import (
	"testing"

	"github.com/charliek/shed/internal/config"
)

func TestProjectSessionEnv(t *testing.T) {
	tests := []struct {
		name string
		shed *config.Shed
		want []string
	}{
		{
			name: "older shed without a landing dir gets no project vars",
			shed: &config.Shed{},
			want: nil,
		},
		{
			name: "bare shed lands in home",
			shed: &config.Shed{LandingDir: config.HomePath},
			want: []string{"SHED_WORKSPACE=" + config.HomePath},
		},
		{
			name: "--local-dir / --repo sets workspace only",
			shed: &config.Shed{
				LandingDir:    config.HomePath + "/proj",
				ProjectMounts: []config.MountConfig{{Target: config.HomePath + "/proj"}},
			},
			want: []string{"SHED_WORKSPACE=" + config.HomePath + "/proj"},
		},
		{
			name: "--local-dir + --add-dir sets workspace + colon-joined add-dirs",
			shed: &config.Shed{
				LandingDir: config.HomePath + "/proj",
				ProjectMounts: []config.MountConfig{
					{Target: config.HomePath + "/proj"}, // --local-dir (landing)
					{Target: config.HomePath + "/sibling"},
					{Target: config.HomePath + "/ref"},
				},
			},
			want: []string{
				"SHED_WORKSPACE=" + config.HomePath + "/proj",
				"SHED_ADD_DIRS=" + config.HomePath + "/sibling:" + config.HomePath + "/ref",
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := projectSessionEnv(tt.shed)
			if len(got) != len(tt.want) {
				t.Fatalf("projectSessionEnv() = %v, want %v", got, tt.want)
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Errorf("index %d: got %q, want %q", i, got[i], tt.want[i])
				}
			}
		})
	}
}
