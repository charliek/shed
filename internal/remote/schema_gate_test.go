package remote

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/charliek/shed/internal/remote/provider"
	"github.com/charliek/shed/internal/remote/provider/fake"
)

// schemaProvider is the fake reporting every instance it reads under another
// label schema, once schema is set. (The fake itself refuses to change that
// label, as the cloud's provider does.)
type schemaProvider struct {
	*fake.Provider
	schema atomic.Value
}

func (s *schemaProvider) Get(ctx context.Context, ref provider.Ref) (provider.Instance, error) {
	inst, err := s.Provider.Get(ctx, ref)
	if v, ok := s.schema.Load().(string); ok && err == nil {
		inst.Labels[provider.LabelSchema] = v
	}
	return inst, err
}

// An instance whose labels are another version's is never touched: not
// stopped, and not destroyed by a delete, whose marker stands for a server
// that can read the labels.
func TestUnknownSchemaInstanceIsNeverMutated(t *testing.T) {
	sp := &schemaProvider{}
	e := newEnv(t, func(e *env, _ *fake.Options) {
		e.wrap = func(p provider.Provider) provider.Provider {
			sp.Provider = p.(*fake.Provider)
			return sp
		}
	})
	const name = "newer"
	e.running(name)
	sp.schema.Store("2")

	if _, err := e.b.StopShed(ctxT(t), name); err != nil {
		t.Fatal(err)
	}
	e.idle()
	if n := e.prov.Count(fake.OpStop); n != 0 {
		t.Fatalf("%d stop calls on an instance of another label schema, want none", n)
	}

	if err := e.b.DeleteShed(ctxT(t), name); err != nil {
		t.Fatal(err)
	}
	e.idle()
	if n := e.prov.Count(fake.OpDestroy); n != 0 {
		t.Fatalf("%d destroy calls on an instance of another label schema, want none", n)
	}
	if e.instances() != 1 {
		t.Fatal("the instance is gone")
	}
	if markerCount(t, e.dir) != 1 {
		t.Fatalf("%d delete markers, want 1: the delete is still owed", markerCount(t, e.dir))
	}
	found := false
	for _, l := range e.logLines() {
		if strings.Contains(l, provider.LabelSchema) {
			found = true
		}
	}
	if !found {
		t.Fatal("nothing was logged about the label schema")
	}
}
