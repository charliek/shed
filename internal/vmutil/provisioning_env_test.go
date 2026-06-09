package vmutil

import (
	"slices"
	"strings"
	"testing"

	"github.com/charliek/shed/internal/provision"
)

func TestBuildEnvProjectVars(t *testing.T) {
	t.Run("workspace + add-dirs", func(t *testing.T) {
		p := &Provisioner{shedName: "demo", workDir: "/home/shed/proj"}
		p.SetAddDirs([]string{"/home/shed/sibling", "/home/shed/ref"})
		env := p.buildEnv(&provision.Config{})
		for _, want := range []string{
			"SHED_CONTAINER=true",
			"SHED_NAME=demo",
			"SHED_WORKSPACE=/home/shed/proj",
			"SHED_ADD_DIRS=/home/shed/sibling:/home/shed/ref",
		} {
			if !slices.Contains(env, want) {
				t.Errorf("buildEnv() missing %q; got %v", want, env)
			}
		}
	})

	t.Run("no add-dirs omits SHED_ADD_DIRS", func(t *testing.T) {
		p := &Provisioner{shedName: "bare", workDir: "/home/shed"}
		for _, e := range p.buildEnv(&provision.Config{}) {
			if strings.HasPrefix(e, "SHED_ADD_DIRS=") {
				t.Errorf("expected no SHED_ADD_DIRS without add-dirs; got %q", e)
			}
		}
	})
}
