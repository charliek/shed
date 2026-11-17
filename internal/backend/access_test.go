package backend

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/charliek/shed/internal/config"
)

// The outcomes of the access rule.
const (
	allow    = "allow"
	notFound = "not found"
	denied   = "forbidden"
	noID     = "no principal"
)

func outcome(err error) string {
	switch {
	case err == nil:
		return allow
	case errors.Is(err, ErrNoPrincipal):
		return noID
	case errors.Is(err, config.ErrForbiddenSentinel):
		return denied
	case errors.Is(err, config.ErrShedNotFoundSentinel), errors.Is(err, config.ErrSnapshotNotFoundSentinel):
		return notFound
	}
	return "unexpected: " + err.Error()
}

// TestAuthorizeTable is the access rule, every kind of principal against every
// kind of shed, for each action.
func TestAuthorizeTable(t *testing.T) {
	alice := UserPrincipal("alice", false)
	bob := UserPrincipal("bob", false)
	admin := UserPrincipal("root", true)
	client := ClientPrincipal("nocturn")
	other := ClientPrincipal("other")

	type shedKind struct {
		name, owner, user string
	}
	alicesShed := shedKind{"alice's", "", "alice"}
	usersless := shedKind{"user-less", "", ""}
	clients := shedKind{"nocturn's", "nocturn", ""}
	unresolved := shedKind{"unresolved", "", config.UserUnresolved}
	admins := shedKind{"root's", "", "root"}

	// want[i] is the outcome for View, Manage, Enter, Admin.
	type row struct {
		p    Principal
		pn   string
		shed shedKind
		want [4]string
	}
	rows := []row{
		// The implicit user and the server pass everything, whatever the
		// record says (a server with no user list routes every shed as "").
		{ImplicitPrincipal(), "implicit", alicesShed, [4]string{allow, allow, allow, allow}},
		{ImplicitPrincipal(), "implicit", unresolved, [4]string{allow, allow, allow, allow}},
		{ImplicitPrincipal(), "implicit", clients, [4]string{allow, allow, allow, allow}},
		{SystemPrincipal(), "system", alicesShed, [4]string{allow, allow, allow, allow}},
		{SystemPrincipal(), "system", unresolved, [4]string{allow, allow, allow, allow}},

		// A user reaches their own shed and nothing else.
		{alice, "alice", alicesShed, [4]string{allow, allow, allow, denied}},
		{alice, "alice", usersless, [4]string{notFound, notFound, notFound, denied}},
		{alice, "alice", clients, [4]string{notFound, notFound, notFound, denied}},
		{alice, "alice", unresolved, [4]string{notFound, notFound, notFound, denied}},
		{alice, "alice", admins, [4]string{notFound, notFound, notFound, denied}},
		{bob, "bob", alicesShed, [4]string{notFound, notFound, notFound, denied}},

		// An admin lists, stops and deletes any shed, enters a service
		// client's shed and their own, and enters no other.
		{admin, "admin", admins, [4]string{allow, allow, allow, allow}},
		{admin, "admin", alicesShed, [4]string{allow, allow, denied, allow}},
		{admin, "admin", usersless, [4]string{allow, allow, denied, allow}},
		{admin, "admin", unresolved, [4]string{allow, allow, denied, allow}},
		{admin, "admin", clients, [4]string{allow, allow, allow, allow}},

		// A service client reaches its own sheds only, and nothing admin.
		{client, "client", clients, [4]string{allow, allow, allow, denied}},
		{other, "other client", clients, [4]string{notFound, notFound, notFound, notFound}},
		{client, "client", alicesShed, [4]string{notFound, notFound, notFound, notFound}},
		{client, "client", usersless, [4]string{notFound, notFound, notFound, notFound}},
		{client, "client", unresolved, [4]string{notFound, notFound, notFound, notFound}},
	}
	actions := []Action{ActionView, ActionManage, ActionEnter, ActionAdmin}
	for _, r := range rows {
		for i, a := range actions {
			got := outcome(Authorize(r.p, r.shed.owner, r.shed.user, a))
			if got != r.want[i] {
				t.Errorf("%s, %s shed, %s: %s, want %s", r.pn, r.shed.name, a, got, r.want[i])
			}
		}
	}
}

// A missing or malformed principal is refused for every action, never
// treated as anyone.
func TestAuthorizeRefusesMalformedPrincipals(t *testing.T) {
	for _, tc := range []struct {
		name string
		p    Principal
	}{
		{"zero", Principal{}},
		{"unknown kind", Principal{Kind: "robot", Admin: true}},
		{"client with no name", Principal{Kind: PrincipalClient}},
		{"named user spelled as the implicit one", Principal{Kind: PrincipalUser, Admin: true}},
		{"implicit with a name", Principal{Kind: PrincipalUser, User: "alice", Implicit: true}},
		{"invalid user name", Principal{Kind: PrincipalUser, User: config.UserUnresolved, Admin: true}},
	} {
		for _, a := range []Action{ActionView, ActionManage, ActionEnter, ActionAdmin} {
			if got := outcome(Authorize(tc.p, "", "", a)); got != noID {
				t.Errorf("%s, %s: %s, want %s", tc.name, a, got, noID)
			}
		}
		if got := outcome(AuthorizeSnapshot(tc.p, "s", "", ActionView)); got != noID {
			t.Errorf("%s, snapshot: %s, want %s", tc.name, got, noID)
		}
		if got := outcome(AuthorizeAdmin(tc.p, "x")); got != noID {
			t.Errorf("%s, admin: %s, want %s", tc.name, got, noID)
		}
	}
}

// Snapshots belong to their owner. An admin may view and delete any, and use
// one with no user, never another user's. A service client reaches none.
func TestAuthorizeSnapshotTable(t *testing.T) {
	alice := UserPrincipal("alice", false)
	admin := UserPrincipal("root", true)
	client := ClientPrincipal("nocturn")
	for _, tc := range []struct {
		pn   string
		p    Principal
		user string
		want [3]string // View, Manage, Enter (use)
	}{
		{"implicit", ImplicitPrincipal(), "alice", [3]string{allow, allow, allow}},
		{"alice", alice, "alice", [3]string{allow, allow, allow}},
		{"alice", alice, "bob", [3]string{notFound, notFound, notFound}},
		{"alice", alice, "", [3]string{notFound, notFound, notFound}},
		{"admin", admin, "alice", [3]string{allow, allow, denied}},
		{"admin", admin, "", [3]string{allow, allow, allow}},
		{"admin", admin, "root", [3]string{allow, allow, allow}},
		{"client", client, "", [3]string{notFound, notFound, notFound}},
	} {
		for i, a := range []Action{ActionView, ActionManage, ActionEnter} {
			if got := outcome(AuthorizeSnapshot(tc.p, "s", tc.user, a)); got != tc.want[i] {
				t.Errorf("%s, snapshot of %q, %s: %s, want %s", tc.pn, tc.user, a, got, tc.want[i])
			}
		}
	}
}

func TestAuthorizeAdmin(t *testing.T) {
	for _, tc := range []struct {
		pn   string
		p    Principal
		want string
	}{
		{"implicit", ImplicitPrincipal(), allow},
		{"system", SystemPrincipal(), allow},
		{"admin", UserPrincipal("root", true), allow},
		{"user", UserPrincipal("alice", false), denied},
		{"client", ClientPrincipal("nocturn"), denied},
	} {
		if got := outcome(AuthorizeAdmin(tc.p, "image pull")); got != tc.want {
			t.Errorf("%s: %s, want %s", tc.pn, got, tc.want)
		}
	}
}

func TestPrincipalContext(t *testing.T) {
	if _, ok := PrincipalFrom(context.Background()); ok {
		t.Fatal("a bare context carries a principal")
	}
	p := UserPrincipal("alice", true)
	got, ok := PrincipalFrom(WithPrincipal(context.Background(), p))
	if !ok || got != p {
		t.Fatalf("PrincipalFrom = %+v, %v", got, ok)
	}
}

func TestCheckSnapshot(t *testing.T) {
	at := time.Date(2026, 11, 14, 9, 0, 0, 0, time.UTC)
	snap := &config.Snapshot{Name: "s", CreatedAt: at, User: "alice"}
	if err := CheckSnapshot(context.Background(), "s", snap); err != nil {
		t.Errorf("no requirement: %v", err)
	}
	ctx := WithRequiredSnapshot(context.Background(), at, "alice")
	if err := CheckSnapshot(ctx, "s", snap); err != nil {
		t.Errorf("the required snapshot: %v", err)
	}
	for _, other := range []*config.Snapshot{
		{Name: "s", CreatedAt: at.Add(time.Second), User: "alice"}, // taken again
		{Name: "s", CreatedAt: at, User: "bob"},                    // another user's
		nil,
	} {
		if err := CheckSnapshot(ctx, "s", other); !errors.Is(err, config.ErrSnapshotNotFoundSentinel) {
			t.Errorf("CheckSnapshot(%+v) = %v, want not found", other, err)
		}
	}
}
