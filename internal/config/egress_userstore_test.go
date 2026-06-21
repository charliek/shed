package config

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func TestUserProfileStoreCRUDAndPersistence(t *testing.T) {
	dir := t.TempDir()
	s, err := OpenUserProfileStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Put("mine", EgressProfile{Allow: []string{"example.com"}}); err != nil {
		t.Fatal(err)
	}
	got, ok := s.Get("mine")
	if !ok || len(got.Allow) != 1 || got.Allow[0] != "example.com" {
		t.Fatalf("get = %v %v", got, ok)
	}
	if names := s.Names(); len(names) != 1 || names[0] != "mine" {
		t.Fatalf("names = %v", names)
	}

	// reopen → persisted
	s2, err := OpenUserProfileStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := s2.Get("mine"); !ok {
		t.Fatal("profile did not persist across reopen")
	}

	// delete removes file + map entry, and is idempotent-erroring
	if err := s.Delete("mine"); err != nil {
		t.Fatal(err)
	}
	if _, ok := s.Get("mine"); ok {
		t.Fatal("profile should be gone after delete")
	}
	if _, err := os.Stat(filepath.Join(dir, "mine.yaml")); !os.IsNotExist(err) {
		t.Fatal("file should be removed on delete")
	}
	if err := s.Delete("mine"); err == nil {
		t.Fatal("delete of a missing profile should error")
	}
}

func TestUserProfileStoreSortedNames(t *testing.T) {
	s, _ := OpenUserProfileStore(t.TempDir())
	for _, n := range []string{"zeta", "alpha", "mike"} {
		if err := s.Put(n, EgressProfile{Allow: []string{"a.com"}}); err != nil {
			t.Fatal(err)
		}
	}
	got := s.Names()
	want := []string{"alpha", "mike", "zeta"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("Names not sorted: %v", got)
		}
	}
}

func TestUserProfileStoreValidation(t *testing.T) {
	s, _ := OpenUserProfileStore(t.TempDir())
	bad := []struct {
		desc string
		name string
		p    EgressProfile
	}{
		{"bad cel", "x", EgressProfile{Rule: "garbage ++"}},
		{"reserved name", "off", EgressProfile{Mode: "audit"}},
		{"uppercase name (apfs collision)", "Foo", EgressProfile{Allow: []string{"a.com"}}},
		{"bad char", "a b", EgressProfile{Allow: []string{"a.com"}}},
		{"empty name", "", EgressProfile{Allow: []string{"a.com"}}},
	}
	for _, b := range bad {
		if err := s.Put(b.name, b.p); err == nil {
			t.Errorf("%s: expected Put error", b.desc)
		}
	}
	// a rejected Put leaves the store unchanged (no map entry, no file)
	if _, ok := s.Get("x"); ok {
		t.Error("failed Put should not persist to the map")
	}
	if _, err := os.Stat(filepath.Join(s.dir, "x.yaml")); !os.IsNotExist(err) {
		t.Error("failed Put should not write a file")
	}
}

func TestUserProfileStoreDeepCopy(t *testing.T) {
	s, _ := OpenUserProfileStore(t.TempDir())
	if err := s.Put("p", EgressProfile{Allow: []string{"a.com"}}); err != nil {
		t.Fatal(err)
	}
	// mutate a Get() copy
	got, _ := s.Get("p")
	got.Allow[0] = "mutated-via-get"
	// mutate a List() copy
	listed := s.List()["p"]
	listed.Allow[0] = "mutated-via-list"

	again, _ := s.Get("p")
	if again.Allow[0] != "a.com" {
		t.Errorf("store mutated through a returned slice: %v", again.Allow)
	}
}

func TestUserProfileStoreFailHardOnBadFile(t *testing.T) {
	// malformed YAML
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "bad.yaml"), []byte("allow: [unclosed"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenUserProfileStore(dir); err == nil {
		t.Fatal("Open must fail hard on a malformed profile file (no silent skip)")
	}
	// valid YAML, invalid CEL
	dir2 := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir2, "badcel.yaml"), []byte("rule: \"garbage ++\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenUserProfileStore(dir2); err == nil {
		t.Fatal("Open must fail hard on a bad-CEL profile file")
	}
	// illegal filename (uppercase)
	dir3 := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir3, "Bad.yaml"), []byte("allow: [a.com]\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenUserProfileStore(dir3); err == nil {
		t.Fatal("Open must reject an illegal profile filename")
	}
}

func TestUserProfileStoreNilSafe(t *testing.T) {
	var s *UserProfileStore
	if s.List() != nil {
		t.Error("nil store List should be nil")
	}
	if _, ok := s.Get("x"); ok {
		t.Error("nil store Get should be false")
	}
	if s.Names() != nil {
		t.Error("nil store Names should be nil")
	}
}

// TestUserProfileStoreConcurrent is the -race guard: this is the first writer
// into the egress resolution read path, so concurrent Put vs List/Get/Names must
// be race-free.
func TestUserProfileStoreConcurrent(t *testing.T) {
	s, _ := OpenUserProfileStore(t.TempDir())
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			name := fmt.Sprintf("p%d", i)
			for j := 0; j < 25; j++ {
				_ = s.Put(name, EgressProfile{Allow: []string{"a.com", "b.com"}})
				_ = s.List()
				_ = s.Names()
				_, _ = s.Get(name)
			}
		}(i)
	}
	wg.Wait()
}
