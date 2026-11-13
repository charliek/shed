package fake

import (
	"testing"

	"github.com/charliek/shed/internal/remote/provider"
	"github.com/charliek/shed/internal/remote/provider/providertest"
)

func TestConformance(t *testing.T) {
	const env = "conform"
	providertest.Run(t, providertest.Harness{
		Env:    env,
		Size:   provider.DefaultSizes()[0],
		Image:  "shed-extensions-amd64-v1",
		Family: provider.ImagePrefixFamily + "shed-extensions-amd64",
		New: func(t *testing.T) (provider.Provider, providertest.Controls) {
			p := New(Options{Env: env})
			return p, providertest.Controls{
				Seed: func(t *testing.T, name string, labels map[string]string) provider.Ref {
					return p.Seed(provider.Instance{Name: name, State: provider.StateRunning, Labels: labels})
				},
				PublishHostKey: func(t *testing.T, ref provider.Ref, key string) {
					p.PublishHostKey(ref, key)
				},
				RefuseNextCreate: func(t *testing.T, kind error) {
					p.FailNext(OpCreate, provider.Errorf(kind, string(OpCreate), "injected"))
				},
				FailNextCreateOperation: func(t *testing.T, kind error) {
					p.FailOperation(OpCreate, provider.Errorf(kind, string(OpWait), "injected"))
				},
			}
		},
	})
}
