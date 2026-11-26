package remote

import (
	"context"
	"errors"
	"testing"

	"github.com/charliek/shed/internal/backend"
	"github.com/charliek/shed/internal/config"
	"github.com/charliek/shed/internal/plugin"
	"github.com/charliek/shed/internal/remote/provider/fake"
)

// usersEnv is an environment whose bridge routes by user, as on a server
// with a user list.
func usersEnv(t *testing.T) (*env, *plugin.Bridge) {
	t.Helper()
	bridge := plugin.NewBridge(plugin.NewRegistry(), true)
	e := newEnv(t, func(e *env, _ *fake.Options) { e.tune = func(o *Options) { o.Bridge = bridge } })
	return e, bridge
}

func (e *env) assign(name, user string) (*config.Shed, error) {
	e.t.Helper()
	shed, err := e.b.GetShed(context.Background(), name)
	if err != nil {
		e.t.Fatalf("GetShed(%s): %v", name, err)
	}
	return e.b.AssignUser(backend.WithRequiredCreatedAt(ctxT(e.t), shed.CreatedAt), name, config.AssignShedRequest{User: user})
}

// A cloud shed belongs to the person or client its machine was made for:
// AssignUser answers 501 FEATURE_UNSUPPORTED, writes nothing and changes
// nothing (plan 041 §0.1 item 16).
func TestAssignUserIsUnsupported(t *testing.T) {
	e := newEnv(t)
	e.createFor("legacy", "")
	labels, calls := e.prov.LabelWrites(), len(e.prov.Calls())
	_, err := e.assign("legacy", "alice")
	if !errors.Is(err, config.ErrFeatureUnsupportedSentinel) {
		t.Fatalf("AssignUser = %v, want feature unsupported (501)", err)
	}
	if e.userOf("legacy") != "" || e.prov.LabelWrites() != labels || len(e.prov.Calls()) != calls {
		t.Errorf("a refused assignment changed something")
	}
}

// The start-up pass that gives sheds with no user to the one configured user
// leaves a cloud shed as it is: the assignment is refused.
func TestStampUnassignedOnRemote(t *testing.T) {
	e, _ := usersEnv(t)
	e.createFor("legacy", "")
	backend.StampUnassigned(ctxT(t), e.b, []backend.UpgradeUser{{Name: "root", Admin: true}}, true, backend.UpgradeLog{})
	if got := e.userOf("legacy"); got != "" {
		t.Errorf("legacy's user = %q, want none (a cloud shed is not assigned)", got)
	}
}
