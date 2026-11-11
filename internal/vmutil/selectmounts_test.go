package vmutil

import (
	"os"
	"path/filepath"
	"sort"
	"testing"

	"github.com/charliek/shed/internal/config"
)

func TestSelectMounts(t *testing.T) {
	dir := t.TempDir()
	mk := func(name string) string {
		p := filepath.Join(dir, name)
		if err := os.Mkdir(p, 0o755); err != nil {
			t.Fatal(err)
		}
		return p
	}
	file := filepath.Join(dir, "file")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := &config.ServerConfig{Mounts: map[string]config.MountConfig{
		"claude":  {Source: mk("claude"), Target: "/home/shed/.claude"},
		"gh":      {Source: mk("gh"), Target: "/home/shed/.config/gh"},
		"missing": {Source: filepath.Join(dir, "nope"), Target: "/x"},
		"file":    {Source: file, Target: "/y"},
	}}
	keys := func(m map[string]config.MountConfig) []string {
		out := []string{}
		for k := range m {
			out = append(out, k)
		}
		sort.Strings(out)
		return out
	}
	tests := []struct {
		name string
		sel  *config.MountSelection
		want []string
	}{
		{"nil (legacy) is every existing mount", nil, []string{"claude", "gh"}},
		{"all", config.AllMounts(), []string{"claude", "gh"}},
		{"selected empty is none", config.SelectedMounts([]string{}), []string{}},
		{"selected names", config.SelectedMounts([]string{"gh"}), []string{"gh"}},
		{"selected unconfigured name is skipped", config.SelectedMounts([]string{"gh", "ssh"}), []string{"gh"}},
		{"selected missing source is skipped", config.SelectedMounts([]string{"missing"}), []string{}},
		{"unknown mode is none", &config.MountSelection{Mode: "everything"}, []string{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := keys(SelectMounts(cfg, tt.sel))
			if len(got) != len(tt.want) {
				t.Fatalf("SelectMounts = %v, want %v", got, tt.want)
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Fatalf("SelectMounts = %v, want %v", got, tt.want)
				}
			}
		})
	}
	if got := SelectMounts(nil, config.AllMounts()); got != nil {
		t.Errorf("nil config = %v, want nil", got)
	}
}
