package rc

import "testing"

// The registry tests iterate rc.allKinds directly — the same list IsValidKind
// uses — so adding a Kind automatically extends coverage; a kind without a
// spec fails TestSpecForKindResolvesEveryKind.

func TestSpecForKindResolvesEveryKind(t *testing.T) {
	for _, k := range allKinds {
		spec, ok := specForKind(k)
		if !ok {
			t.Errorf("specForKind(%q) not found", k)
			continue
		}
		if spec.InnerCommand == nil {
			t.Errorf("spec for %q has nil InnerCommand", k)
		}
		if spec.Classify == nil {
			t.Errorf("spec for %q has nil Classify", k)
		}
		if spec.Tool == "" {
			t.Errorf("spec for %q has empty Tool", k)
		}
	}
}

func TestSpecForKindUnknown(t *testing.T) {
	if spec, ok := specForKind(Kind("nope")); ok || spec != nil {
		t.Errorf("specForKind(unknown) = (%v, %v), want (nil, false)", spec, ok)
	}
}

func TestRegistryKindsDisjointAndComplete(t *testing.T) {
	seen := map[Kind]*AgentSpec{}
	for _, spec := range agentRegistry {
		if len(spec.Kinds) == 0 {
			t.Errorf("spec %q declares no kinds", spec.Tool)
		}
		for _, k := range spec.Kinds {
			if prev, dup := seen[k]; dup {
				t.Errorf("kind %q claimed by both %q and %q specs", k, prev.Tool, spec.Tool)
			}
			seen[k] = spec
			if !IsValidKind(k) {
				t.Errorf("spec %q declares invalid kind %q", spec.Tool, k)
			}
		}
	}
	// Union of every spec's Kinds must be exactly allKinds.
	if len(seen) != len(allKinds) {
		t.Errorf("registry covers %d kinds, want %d", len(seen), len(allKinds))
	}
	for _, k := range allKinds {
		if _, ok := seen[k]; !ok {
			t.Errorf("kind %q is not covered by any spec", k)
		}
	}
}

func TestDefaultKindResolves(t *testing.T) {
	if spec, ok := specForKind(DefaultKind); !ok || spec == nil {
		t.Fatalf("DefaultKind %q does not resolve to a spec", DefaultKind)
	}
}
