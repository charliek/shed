package provider

import (
	"fmt"
	"regexp"
	"slices"
	"strconv"
)

// Working labels: what the server writes on a machine to remember where its
// shed is in its life. Who the machine is (its shed id, name and owner) is
// not a label: those are the machine's own members, fixed at create.
const (
	// LabelState is the lifecycle marker; see ShedState.
	LabelState = "shed_state"
	// LabelLayout is the guest layout the agent reported, as a decimal
	// integer. The server writes it in the same SetLabels call that sets
	// shed_state=ready; before that the label is absent.
	LabelLayout = "shed_layout"
)

// ReservedLabels are labels no server writes and no provider returns: their
// facts are the machine's own members (its shed id, name, server and owner).
var ReservedLabels = []string{"shed_schema", "shed_env", "shed_id", "shed_name", "shed_owner", "shed_user", "shed_user_origin"}

// MaxLabels is the most labels a machine holds.
const MaxLabels = 32

// ShedState is the value of LabelState.
type ShedState string

const (
	// ShedStateCreating is set by Create. The machine is not a usable shed
	// yet: the host key is not pinned or the agent has not answered. The
	// start-up and periodic passes destroy a creating machine that is over
	// 15 minutes old and has no local record (remote.StaleCreateAge).
	ShedStateCreating ShedState = "creating"
	// ShedStateReady is set by the backend once the host key is pinned and
	// the agent has answered a probe. It is never set back to creating.
	// Reconcile never deletes a ready machine.
	ShedStateReady ShedState = "ready"
	// ShedStateDeleting is set by the backend before a delete is
	// acknowledged to the client. Reconcile destroys a deleting machine
	// whenever it sees one.
	ShedStateDeleting ShedState = "deleting"
)

var (
	shedIDRegex   = regexp.MustCompile(`^[a-z2-7]{26}$`)
	shedNameRegex = regexp.MustCompile(`^[a-z]([a-z0-9-]{0,61}[a-z0-9])?$`)
	handleRegex   = regexp.MustCompile(`^[a-z][a-z0-9-]{0,62}$`)
	labelKeyRegex = regexp.MustCompile(`^[a-z][a-z0-9_]{0,62}$`)
	labelValRegex = regexp.MustCompile(`^[a-z0-9_-]{0,63}$`)
)

// ValidShedID reports whether s is a shed id as the server mints it and the
// compute contract names a machine: 26 lower-case base32 characters.
func ValidShedID(s string) bool { return shedIDRegex.MatchString(s) }

// ValidShedName reports whether s may be a machine's name.
func ValidShedName(s string) bool { return shedNameRegex.MatchString(s) }

// ValidHandle reports whether s may be a machine's user or client.
func ValidHandle(s string) bool { return handleRegex.MatchString(s) }

// ValidLabelKey reports whether s may be a label key (reserved or not).
func ValidLabelKey(s string) bool { return labelKeyRegex.MatchString(s) }

// ValidLabelValue reports whether s may be a label value.
func ValidLabelValue(s string) bool { return labelValRegex.MatchString(s) }

// LayoutLabel formats a guest layout number as a LabelLayout value.
func LayoutLabel(layout int) string { return strconv.Itoa(layout) }

// Str returns a pointer to s, for a SetLabels value.
func Str(s string) *string { return &s }

func parseLayout(s string) int {
	n, err := strconv.Atoi(s)
	if err != nil || n < 0 {
		return 0
	}
	return n
}

// CheckLabels verifies that every key and value of a label write is legal
// and that no key is reserved. A nil value (a removal, or "must be absent")
// is legal.
func CheckLabels(labels map[string]*string) error {
	for k, v := range labels {
		if err := checkLabel(k, v); err != nil {
			return err
		}
	}
	return nil
}

func checkLabel(k string, v *string) error {
	if slices.Contains(ReservedLabels, k) {
		return fmt.Errorf("label %s is reserved", k)
	}
	if !ValidLabelKey(k) {
		return fmt.Errorf("label key %q is not valid", k)
	}
	if v != nil && !ValidLabelValue(*v) {
		return fmt.Errorf("label %s has an invalid value %q", k, *v)
	}
	return nil
}

// checkLabelMap is CheckLabels for a map of values.
func checkLabelMap(labels map[string]string) error {
	if len(labels) > MaxLabels {
		return fmt.Errorf("%d labels, over %d", len(labels), MaxLabels)
	}
	for k, v := range labels {
		if err := checkLabel(k, &v); err != nil {
			return err
		}
	}
	return nil
}
