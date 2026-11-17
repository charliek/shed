package userstest

import (
	"testing"

	"github.com/charliek/shed/internal/users"
)

func TestFakeImplicit(t *testing.T) {
	f := Implicit("SHA256:a")
	u, k, ok := f.UserForKey("SHA256:a")
	if !ok || u != users.Implicit() || k.Fingerprint != "SHA256:a" || k.Source != users.SourceInline {
		t.Fatalf("UserForKey = %+v, %+v, %v; want the implicit user's inline key", u, k, ok)
	}
	if _, _, ok := f.UserForKey("SHA256:b"); ok {
		t.Error("unknown key resolved")
	}
	if _, _, ok := f.UserForKey(""); ok {
		t.Error("empty fingerprint resolved")
	}
	f.RemoveKey("SHA256:a")
	if _, _, ok := f.UserForKey("SHA256:a"); ok {
		t.Error("removed key still resolves")
	}
}

func TestFakeNamedUsers(t *testing.T) {
	f := New()
	f.AddUser(users.User{Name: "bob"})
	f.AddUser(users.User{Name: "alice", Admin: true})
	f.AddKey("bob", users.Key{Fingerprint: "SHA256:b", Source: users.SourceGitHub, Origin: "bob-gh"})
	u, k, ok := f.UserForKey("SHA256:b")
	if !ok || u.Name != "bob" || k.Origin != "bob-gh" {
		t.Errorf("UserForKey = %+v, %+v, %v", u, k, ok)
	}
	got := f.Users()
	if len(got) != 2 || got[0].Name != "alice" || got[1].Name != "bob" {
		t.Errorf("Users() = %+v, want alice then bob", got)
	}
	if u, ok := f.User("alice"); !ok || !u.Admin {
		t.Errorf("User(alice) = %+v, %v", u, ok)
	}
	defer func() {
		if recover() == nil {
			t.Error("AddKey for an unknown user should panic")
		}
	}()
	f.AddKey("carol", users.Key{Fingerprint: "SHA256:c"})
}

func TestMatching(t *testing.T) {
	m := Matching(func(fp string) bool { return fp == "SHA256:a" })
	if u, _, ok := m.UserForKey("SHA256:a"); !ok || u != users.Implicit() {
		t.Errorf("accepted key: %+v, %v", u, ok)
	}
	if _, _, ok := m.UserForKey("SHA256:b"); ok {
		t.Error("rejected key resolved")
	}
	if _, _, ok := Matching(nil).UserForKey("SHA256:a"); ok {
		t.Error("nil predicate resolved a key")
	}
	if !users.HasKey(m, "SHA256:a") || users.HasKey(nil, "SHA256:a") {
		t.Error("HasKey disagrees with the lookup, or a nil lookup accepted a key")
	}
}
