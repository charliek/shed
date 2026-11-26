package backend

import (
	"context"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/charliek/shed/internal/config"
)

// How the guard treats each optional interface a backend may implement
// beside Backend (capabilities.go, guard.go, upgrade.go, assign.go).
const (
	// optionalGuarded: an interface whose methods act on a named shed. The
	// guard returns a guarded implementation of it (IsAsync,
	// AsEgressController), and every method is walked below.
	optionalGuarded = "guarded"
	// optionalQuestion: a read-only capability question, answered from the
	// wrapped backend (unwrap); it acts on no shed.
	optionalQuestion = "question"
	// optionalServer: used only by the server's own start-up work on the raw
	// backend, never through a Guard, which does not implement it.
	optionalServer = "server-only"
)

// optionalInterfaces classifies every exported interface declared in this
// package other than Backend. A new one fails
// TestGuardClassifiesEveryOptionalInterface until it is listed here.
var optionalInterfaces = map[string]string{
	"AsyncLifecycle":   optionalGuarded,
	"AsyncDeleter":     optionalGuarded,
	"EgressController": optionalGuarded,
	"Provisioner":      optionalGuarded,
	// ProvisionRun is what a guarded BeginProvision returns: reached only
	// through it, so its methods are walked through it.
	"ProvisionRun":           optionalGuarded,
	"NetworkProfiles":        optionalQuestion,
	"CapabilityReporter":     optionalQuestion,
	"Cataloger":              optionalQuestion,
	"CountingLister":         optionalServer,
	"CountingSnapshotLister": optionalServer,
	"SnapshotStamper":        optionalServer,
	"OriginStamper":          optionalServer,
}

// optionalTypes maps each classified interface to its type, for the method
// walk.
var optionalTypes = map[string]reflect.Type{
	"AsyncLifecycle":         reflect.TypeOf((*AsyncLifecycle)(nil)).Elem(),
	"AsyncDeleter":           reflect.TypeOf((*AsyncDeleter)(nil)).Elem(),
	"EgressController":       reflect.TypeOf((*EgressController)(nil)).Elem(),
	"Provisioner":            reflect.TypeOf((*Provisioner)(nil)).Elem(),
	"ProvisionRun":           reflect.TypeOf((*ProvisionRun)(nil)).Elem(),
	"NetworkProfiles":        reflect.TypeOf((*NetworkProfiles)(nil)).Elem(),
	"CapabilityReporter":     reflect.TypeOf((*CapabilityReporter)(nil)).Elem(),
	"Cataloger":              reflect.TypeOf((*Cataloger)(nil)).Elem(),
	"CountingLister":         reflect.TypeOf((*CountingLister)(nil)).Elem(),
	"CountingSnapshotLister": reflect.TypeOf((*CountingSnapshotLister)(nil)).Elem(),
	"SnapshotStamper":        reflect.TypeOf((*SnapshotStamper)(nil)).Elem(),
	"OriginStamper":          reflect.TypeOf((*OriginStamper)(nil)).Elem(),
}

// declaredInterfaces returns the exported interface types declared in this
// package's non-test source files.
func declaredInterfaces(t *testing.T) []string {
	t.Helper()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	var out []string
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, name, nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatal(err)
		}
		for _, decl := range f.Decls {
			gd, ok := decl.(*ast.GenDecl)
			if !ok || gd.Tok != token.TYPE {
				continue
			}
			for _, spec := range gd.Specs {
				ts := spec.(*ast.TypeSpec)
				if _, isIface := ts.Type.(*ast.InterfaceType); isIface && ts.Name.IsExported() {
					out = append(out, ts.Name.Name)
				}
			}
		}
	}
	sort.Strings(out)
	return out
}

// optionalCall is one method of a guarded optional interface, called on the
// guarded implementation the guard hands out for the backend b wraps.
type optionalCall func(t *testing.T, g *Guard, ctx context.Context) error

// optionalCases has a case for every method of every guarded optional
// interface, keyed "Interface.Method". Each acts on alice's shed "box".
func optionalCases() map[string]optionalCall {
	async := func(t *testing.T, g *Guard) AsyncLifecycle {
		al, ok := IsAsync(g)
		if !ok {
			t.Fatal("the guard hides the async lifecycle")
		}
		return al
	}
	egress := func(t *testing.T, g *Guard) EgressController {
		ec, ok := AsEgressController(g)
		if !ok {
			t.Fatal("the guard hides the egress capability")
		}
		return ec
	}
	return map[string]optionalCall{
		"AsyncLifecycle.WaitSettled": func(t *testing.T, g *Guard, ctx context.Context) error {
			_, err := async(t, g).WaitSettled(ctx, "box")
			return err
		},
		"AsyncDeleter.DeleteAndWait": func(t *testing.T, g *Guard, ctx context.Context) error {
			d, ok := async(t, g).(AsyncDeleter)
			if !ok {
				t.Fatal("the guard's async lifecycle has no delete-and-wait")
			}
			return d.DeleteAndWait(ctx, ctx, "box")
		},
		"EgressController.SetShedEgress": func(t *testing.T, g *Guard, ctx context.Context) error {
			_, err := egress(t, g).SetShedEgress(ctx, "box", []string{"base"})
			return err
		},
		"EgressController.ClearShedEgress": func(t *testing.T, g *Guard, ctx context.Context) error {
			_, err := egress(t, g).ClearShedEgress(ctx, "box")
			return err
		},
		"ProvisionRun.Run": func(t *testing.T, g *Guard, ctx context.Context) error {
			pv, _ := AsProvisioner(g)
			_, err := Provision(ctx, pv, "box")
			return err
		},
		"ProvisionRun.Release": func(t *testing.T, g *Guard, ctx context.Context) error {
			pv, _ := AsProvisioner(g)
			run, err := pv.BeginProvision(ctx, "box")
			if err != nil {
				return err
			}
			run.Release()
			return nil
		},
		"Provisioner.BeginProvision": func(t *testing.T, g *Guard, ctx context.Context) error {
			pv, ok := AsProvisioner(g)
			if !ok {
				t.Fatal("the guard hides the provision capability")
			}
			_, err := Provision(ctx, pv, "box")
			return err
		},
	}
}

// TestGuardClassifiesEveryOptionalInterface: every exported interface this
// package declares besides Backend is classified (optionalInterfaces), so a
// new optional capability fails here until it is given a rule. Every method
// of a guarded one has a case (optionalCases), so a method added to one
// fails here too. A guarded method refuses a call with no principal and a
// stranger without reaching the backend, and reaches alice's shed bound to
// the incarnation and the user the guard authorized. A server-only interface
// is not implemented by a Guard, so nothing can reach it through one.
func TestGuardClassifiesEveryOptionalInterface(t *testing.T) {
	declared := declaredInterfaces(t)
	seen := map[string]bool{}
	for _, name := range declared {
		seen[name] = true
		if name == "Backend" {
			continue
		}
		if _, ok := optionalInterfaces[name]; !ok {
			t.Errorf("interface %s has no classification in optionalInterfaces: classify it (guarded, question or server-only)", name)
		}
		if _, ok := optionalTypes[name]; !ok {
			t.Errorf("interface %s has no entry in optionalTypes", name)
		}
	}
	for name := range optionalInterfaces {
		if !seen[name] {
			t.Errorf("optionalInterfaces lists %s, which this package does not declare", name)
		}
	}

	cases := optionalCases()
	walked := map[string]bool{}
	for name, class := range optionalInterfaces {
		typ := optionalTypes[name]
		if typ == nil {
			continue
		}
		switch class {
		case optionalGuarded:
			for i := 0; i < typ.NumMethod(); i++ {
				key := name + "." + typ.Method(i).Name
				walked[key] = true
				if _, ok := cases[key]; !ok {
					t.Errorf("%s has no guard case in optionalCases: classify it", key)
				}
			}
		case optionalServer:
			if reflect.TypeOf(&Guard{}).Implements(typ) {
				t.Errorf("a Guard implements the server-only %s: a caller could reach it through the guard", name)
			}
		}
	}
	for key := range cases {
		if !walked[key] {
			t.Errorf("optionalCases has %s, which is not a method of a guarded interface", key)
		}
	}

	newGuarded := func() (*recAsync, *Guard) {
		rb, _ := fixture()
		b := &recAsync{recBackend: rb, tombstones: map[string]time.Time{}}
		return b, NewGuard(b)
	}
	for key, call := range cases {
		t.Run(key, func(t *testing.T) {
			b, g := newGuarded()
			if err := call(t, g, context.Background()); !errors.Is(err, ErrNoPrincipal) {
				t.Errorf("no principal: %v, want ErrNoPrincipal", err)
			}
			if got := b.acted(); len(got) != 0 {
				t.Errorf("no principal reached the backend: %v", got)
			}

			b, g = newGuarded()
			if err := call(t, g, as(bob)); !errors.Is(err, config.ErrShedNotFoundSentinel) {
				t.Errorf("bob: %v, want not found", err)
			}
			if got := b.acted(); len(got) != 0 {
				t.Errorf("bob reached the backend: %v", got)
			}

			b, g = newGuarded()
			if err := call(t, g, as(alice)); err != nil {
				t.Fatalf("alice: %v", err)
			}
			acted := b.acted()
			if len(acted) == 0 {
				t.Fatal("alice's call did not reach the backend")
			}
			ctx := b.ctxs[acted[0]]
			if got, ok := RequiredCreatedAt(ctx); !ok || !got.Equal(t0) {
				t.Errorf("reached the backend without the shed's incarnation: %v %v", got, ok)
			}
			if got, ok := RequiredUser(ctx); !ok || got != "alice" {
				t.Errorf("reached the backend without the user it was authorized against: %q %v", got, ok)
			}
		})
	}
}
