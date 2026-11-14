package provider

import (
	"fmt"
	"regexp"
	"strconv"
)

// Label keys. Labels are how a server finds its sheds again after a restart
// or a move, so they are the record of which sheds exist. Keys and values are
// at most 63 characters of lowercase letters, digits, "_" and "-".
const (
	// LabelSchema is the version of this label set: SchemaVersion. An
	// instance with another value is reported and never touched.
	LabelSchema = "shed_schema"
	// LabelEnv is the environment (one server's named slice of a project).
	// A server acts only on instances that carry its own.
	LabelEnv = "shed_env"
	// LabelID is the shed's permanent ID.
	LabelID = "shed_id"
	// LabelName is the shed's name.
	LabelName = "shed_name"
	// LabelOwner is the service client that owns the shed, or the empty
	// string for a shed an admin created. The label is always present.
	LabelOwner = "shed_owner"
	// LabelState is the lifecycle marker; see ShedState. It is on the
	// instance only, not its disk.
	LabelState = "shed_state"
	// LabelLayout is the guest layout the agent reported, as a decimal
	// integer. The server writes it in the same SetLabels call that sets
	// shed_state=ready; before that the label is absent. It is on the
	// instance only.
	LabelLayout = "shed_layout"

	// SchemaVersion is the only LabelSchema value this build understands.
	SchemaVersion = "1"
)

// DiskLabelKeys are the labels a provider copies from Spec.Labels to the
// boot disk at create, so a disk that outlives its instance can be traced.
var DiskLabelKeys = []string{LabelSchema, LabelEnv, LabelID, LabelName, LabelOwner}

// ShedState is the value of LabelState.
type ShedState string

const (
	// ShedStateCreating is set by Create. The instance is not a usable
	// shed yet: the host key is not pinned or the agent has not answered.
	// Reconcile destroys a creating instance that is over 15 minutes old
	// and has no work in flight.
	ShedStateCreating ShedState = "creating"
	// ShedStateReady is set by the backend once the host key pin is
	// written and read back and the agent has answered a probe. It is
	// never set back to creating. Reconcile never deletes a ready
	// instance.
	ShedStateReady ShedState = "ready"
	// ShedStateDeleting is set by the backend before a delete is
	// acknowledged to the client. Reconcile destroys a deleting instance
	// whenever it sees one.
	ShedStateDeleting ShedState = "deleting"
)

var (
	envRegex    = regexp.MustCompile(`^[a-z]([a-z0-9-]{0,18}[a-z0-9])?$`)
	shedIDRegex = regexp.MustCompile(`^[a-z0-9]{8,32}$`)
	labelRegex  = regexp.MustCompile(`^[a-z0-9_-]{0,63}$`)
)

// ValidEnv reports whether s is a legal environment name: 1 to 20 characters,
// lowercase letters, digits and hyphens, starting with a letter and not
// ending with a hyphen.
func ValidEnv(s string) bool { return envRegex.MatchString(s) }

// ValidShedID reports whether s is a legal shed ID: 8 to 32 lowercase letters
// and digits. The server mints 26 (16 random bytes in lowercase base32).
func ValidShedID(s string) bool { return shedIDRegex.MatchString(s) }

// ValidLabelValue reports whether s may be a label value.
func ValidLabelValue(s string) bool { return labelRegex.MatchString(s) }

// InstanceName returns the cloud name of a shed's instance:
// "shed-<env>-<id>", at most 58 characters.
func InstanceName(env, id string) string { return "shed-" + env + "-" + id }

// NetworkTag returns the network tag every instance of an environment
// carries: "shed-<env>". The environment's firewall rule targets it.
func NetworkTag(env string) string { return "shed-" + env }

// FirewallRuleName returns the name of an environment's one ingress rule:
// "shed-<env>-ssh". The setup script creates it; the server never creates or
// deletes it.
func FirewallRuleName(env string) string { return "shed-" + env + "-ssh" }

// Identity is who a shed is, as its labels carry it.
type Identity struct {
	// Env is the environment that owns the shed.
	Env string
	// ID is the permanent shed ID.
	ID string
	// Name is the shed name. A name is unique among an environment's
	// sheds; the ID is what tells a recreated shed from the one before.
	Name string
	// Owner is the owning service client, or "" for an admin's shed.
	Owner string
}

// Validate checks that every field can be a label and that the instance name
// fits.
func (i Identity) Validate() error {
	switch {
	case !ValidEnv(i.Env):
		return Errorf(ErrInvalid, "", "environment %q is not a valid name", i.Env)
	case !ValidShedID(i.ID):
		return Errorf(ErrInvalid, "", "shed id %q is not valid", i.ID)
	case i.Name == "" || !ValidLabelValue(i.Name):
		return Errorf(ErrInvalid, "", "shed name %q cannot be a label", i.Name)
	case !ValidLabelValue(i.Owner):
		return Errorf(ErrInvalid, "", "owner %q cannot be a label", i.Owner)
	}
	return nil
}

// InstanceName returns the cloud name for this shed's instance.
func (i Identity) InstanceName() string { return InstanceName(i.Env, i.ID) }

// Labels returns the label set for an instance in the given lifecycle state.
// It holds every key but LabelLayout.
func (i Identity) Labels(state ShedState) map[string]string {
	return map[string]string{
		LabelSchema: SchemaVersion,
		LabelEnv:    i.Env,
		LabelID:     i.ID,
		LabelName:   i.Name,
		LabelOwner:  i.Owner,
		LabelState:  string(state),
	}
}

// IdentityFromLabels reads an Identity back from an instance's labels. It
// fails with ErrInvalid when the schema is missing or not SchemaVersion, or
// when the environment, ID or name is missing or malformed. Reconcile reports
// such an instance and leaves it alone.
func IdentityFromLabels(labels map[string]string) (Identity, error) {
	if got := labels[LabelSchema]; got != SchemaVersion {
		return Identity{}, Errorf(ErrInvalid, "", "label %s is %q, want %q", LabelSchema, got, SchemaVersion)
	}
	id := Identity{
		Env:   labels[LabelEnv],
		ID:    labels[LabelID],
		Name:  labels[LabelName],
		Owner: labels[LabelOwner],
	}
	if err := id.Validate(); err != nil {
		return Identity{}, err
	}
	return id, nil
}

// LayoutLabel formats a guest layout number as a LabelLayout value.
func LayoutLabel(layout int) string { return strconv.Itoa(layout) }

func parseLayout(s string) int {
	n, err := strconv.Atoi(s)
	if err != nil || n < 0 {
		return 0
	}
	return n
}

// checkLabels verifies that every key and value in a label map is legal.
func checkLabels(labels map[string]string) error {
	for k, v := range labels {
		if k == "" || !ValidLabelValue(k) || k[0] < 'a' || k[0] > 'z' {
			return fmt.Errorf("label key %q is not valid", k)
		}
		if !ValidLabelValue(v) {
			return fmt.Errorf("label %s has an invalid value %q", k, v)
		}
	}
	return nil
}
