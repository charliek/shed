package provider

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// MaxSpec is the largest Spec.Spec, in bytes.
const MaxSpec = 4096

// ShedSpecSchema is the current ShedSpec.Schema.
const ShedSpecSchema = 2

// ShedSpec is the document the server stores in a machine's spec, which the
// provider keeps and returns verbatim. Together with the machine's own
// members (name, owner, size, image, state) it is everything adoption needs:
// a record rebuilt from them equals the one that was lost. Readers ignore
// fields they do not know.
type ShedSpec struct {
	// Schema is ShedSpecSchema.
	Schema int `json:"schema"`
	// CreatedAt is when the shed was created, in UTC: the server's own
	// time, which ownership checks compare. The provider's creation time is
	// not used for that.
	CreatedAt time.Time `json:"created_at"`
	// Repo is the repository cloned at create, with any user name and
	// password removed (see StripRepoUserinfo). Empty when none.
	Repo string `json:"repo,omitempty"`
	// Network is the shed's network profile: "open" or "isolated".
	Network string `json:"network"`
	// LandingDir is where interactive logins land. Empty means the home
	// directory.
	LandingDir string `json:"landing_dir,omitempty"`
	// UserOrigin is where the shed's user came from when it was made:
	// "local" or "directory" (plan 040). Empty: not known.
	UserOrigin string `json:"user_origin,omitempty"`
}

// Encode returns the JSON stored as the spec. The schema is set to
// ShedSpecSchema, the time is converted to UTC and the repo is stripped of
// credentials, so a caller cannot store them by mistake.
func (s ShedSpec) Encode() (string, error) {
	s.Schema = ShedSpecSchema
	s.CreatedAt = s.CreatedAt.UTC()
	s.Repo = StripRepoUserinfo(s.Repo)
	data, err := json.Marshal(s)
	if err != nil {
		return "", fmt.Errorf("encode the shed spec: %w", err)
	}
	if len(data) > MaxSpec {
		return "", Errorf(ErrInvalid, "create", "the shed spec is %d bytes, over %d (a repository URL that long cannot be stored)", len(data), MaxSpec)
	}
	return string(data), nil
}

// DecodeShedSpec parses a machine's spec. An empty value, or a schema this
// build does not read, is an error wrapping ErrInvalid.
func DecodeShedSpec(value string) (ShedSpec, error) {
	if strings.TrimSpace(value) == "" {
		return ShedSpec{}, Errorf(ErrInvalid, "", "the machine's spec is empty")
	}
	var s ShedSpec
	if err := json.Unmarshal([]byte(value), &s); err != nil {
		return ShedSpec{}, &Error{Kind: ErrInvalid, Detail: "the machine's spec is not valid JSON", Err: err}
	}
	if s.Schema != ShedSpecSchema {
		return ShedSpec{}, Errorf(ErrInvalid, "", "the machine's spec has schema %d, want %d", s.Schema, ShedSpecSchema)
	}
	return s, nil
}

// StripRepoUserinfo removes the whole user-and-password part from a
// repository URL: "https://token@host/o/r.git" and "https://u:p@host/o/r.git"
// both become "https://host/o/r.git". On an ssh:// URL the user is a login
// name, not a credential, so it is kept and only a password is dropped. A
// value with no scheme, such as "git@github.com:o/r.git" or "o/r", is
// returned unchanged.
//
// The backend stores the stripped form in the shed's record as well, so a
// record rebuilt from ShedSpec equals the original.
func StripRepoUserinfo(repo string) string {
	scheme, rest, ok := strings.Cut(repo, "://")
	if !ok {
		return repo
	}
	authority, path, hasPath := strings.Cut(rest, "/")
	at := strings.LastIndex(authority, "@")
	if at < 0 {
		return repo
	}
	host := authority[at+1:]
	if s := strings.ToLower(scheme); s == "ssh" || s == "git+ssh" || s == "ssh+git" {
		if user, _, _ := strings.Cut(authority[:at], ":"); user != "" {
			host = user + "@" + host
		}
	}
	out := scheme + "://" + host
	if hasPath {
		out += "/" + path
	}
	return out
}

// Limits of a create.
const (
	// MaxAuthorizedKeys bounds Spec.AuthorizedKeys.
	MaxAuthorizedKeys = 4
	// MaxAuthorizedKey bounds one authorized key line.
	MaxAuthorizedKey = 8 << 10
	// MinClientLease bounds a client's lease from below.
	MinClientLease = 60 * time.Second
	// MaxDiskGB bounds Spec.DiskGB.
	MaxDiskGB = 65536
)

// Spec is a create: everything a provider needs to make one machine. Build
// it with NewSpec. The backend stores it in the shed's record and replays it
// verbatim on a resumed create, RequestID included; a provider compares two
// creates of one shed id by every member but RequestID.
type Spec struct {
	// RequestID is the create's request id (NewRequestID).
	RequestID string `json:"request_id"`
	// ShedID is the shed's id: the machine's Ref.
	ShedID string `json:"shed_id"`
	// Name is the shed's name.
	Name string `json:"name"`
	// User or Client is who owns the machine: a person's handle or a
	// client's name, never both. A provider whose catalog says
	// OwnerRequired needs one.
	User   string `json:"user,omitempty"`
	Client string `json:"client,omitempty"`
	// Size is a catalog size's name.
	Size string `json:"size"`
	// Image is ImageDefault or a catalog alias.
	Image string `json:"image"`
	// DiskGB is the disk; zero gives the size's default.
	DiskGB int `json:"disk_gb,omitempty"`
	// Labels are the working labels the machine starts with.
	Labels map[string]string `json:"labels"`
	// AuthorizedKeys are the public keys the guest's link user accepts
	// (one normally, two during a rotation).
	AuthorizedKeys []string `json:"authorized_keys"`
	// Spec is the encoded ShedSpec, kept and returned verbatim.
	Spec string `json:"spec"`
	// AccessKinds are the access kinds the server can use, in its order of
	// preference.
	AccessKinds []string `json:"access_kinds"`
	// LeaseS is a client's lease in seconds (clients only, required there).
	LeaseS int64 `json:"lease_s,omitempty"`
}

// Clone returns a deep copy.
func (s *Spec) Clone() *Spec {
	if s == nil {
		return nil
	}
	out := *s
	if s.Labels != nil {
		out.Labels = make(map[string]string, len(s.Labels))
		for k, v := range s.Labels {
			out.Labels[k] = v
		}
	}
	out.AuthorizedKeys = append([]string(nil), s.AuthorizedKeys...)
	out.AccessKinds = append([]string(nil), s.AccessKinds...)
	return &out
}

// Owner says who a create is for.
type Owner struct {
	// User is a person's handle, Client a client's name; at most one.
	User, Client string
	// Lease is a client's lease (clients only).
	Lease time.Duration
}

// NewSpec builds the create for a shed. authorizedKeys are the server's SSH
// public keys in authorized_keys form (one normally, two during a rotation).
// The labels hold shed_state=creating; the spec is shed, encoded.
func NewSpec(shedID, name string, owner Owner, shed ShedSpec, authorizedKeys []string, size string, diskGB int, image string) (Spec, error) {
	encoded, err := shed.Encode()
	if err != nil {
		return Spec{}, err
	}
	keys := make([]string, 0, len(authorizedKeys))
	for _, k := range authorizedKeys {
		if k = strings.TrimSpace(k); k != "" {
			keys = append(keys, k)
		}
	}
	if image == "" {
		image = ImageDefault
	}
	spec := Spec{
		RequestID:      NewRequestID(),
		ShedID:         shedID,
		Name:           name,
		User:           owner.User,
		Client:         owner.Client,
		Size:           size,
		Image:          image,
		DiskGB:         diskGB,
		Labels:         map[string]string{LabelState: string(ShedStateCreating)},
		AuthorizedKeys: keys,
		Spec:           encoded,
		AccessKinds:    []string{AccessSSH},
	}
	if owner.Client != "" {
		spec.LeaseS = int64(owner.Lease / time.Second)
	}
	if err := spec.Validate(); err != nil {
		return Spec{}, err
	}
	return spec, nil
}

// Validate checks a Spec before anything is sent. Providers call it first in
// Create and return its error, which wraps ErrInvalid.
func (s Spec) Validate() error {
	bad := func(format string, args ...any) error { return Errorf(ErrInvalid, "create", format, args...) }
	switch {
	case s.RequestID == "":
		return bad("no request id")
	case !ValidShedID(s.ShedID):
		return bad("shed id %q is not valid", s.ShedID)
	case !ValidShedName(s.Name):
		return bad("shed name %q cannot name a machine", s.Name)
	case s.User != "" && s.Client != "":
		return bad("a machine belongs to a person or to a client, not both")
	case s.User != "" && !ValidHandle(s.User):
		return bad("user %q is not a valid handle", s.User)
	case s.Client != "" && !ValidHandle(s.Client):
		return bad("client %q is not a valid name", s.Client)
	case s.Client != "" && s.LeaseS < int64(MinClientLease/time.Second):
		return bad("a client's machine needs a lease of at least %s", MinClientLease)
	case s.Client == "" && s.LeaseS != 0:
		return bad("only a client's machine carries a lease")
	case s.Size == "":
		return bad("no size")
	case s.Image == "":
		return bad("no image")
	case s.DiskGB < 0 || s.DiskGB > MaxDiskGB:
		return bad("disk size %d is not 1 to %d GB", s.DiskGB, MaxDiskGB)
	case s.Labels[LabelState] != string(ShedStateCreating):
		return bad("label %s must be %q at create", LabelState, ShedStateCreating)
	case len(s.AuthorizedKeys) == 0:
		return bad("no authorized key: the server could never log in")
	case len(s.AuthorizedKeys) > MaxAuthorizedKeys:
		return bad("%d authorized keys, over %d", len(s.AuthorizedKeys), MaxAuthorizedKeys)
	case len(s.Spec) > MaxSpec:
		return bad("the spec is %d bytes, over %d", len(s.Spec), MaxSpec)
	case len(s.AccessKinds) == 0 || len(s.AccessKinds) > 8:
		return bad("1 to 8 access kinds")
	}
	if err := checkLabelMap(s.Labels); err != nil {
		return bad("%v", err)
	}
	for _, k := range s.AuthorizedKeys {
		if len(k) > MaxAuthorizedKey || strings.ContainsAny(k, "\r\n") {
			return bad("an authorized key is not one line of at most %d bytes", MaxAuthorizedKey)
		}
	}
	if _, err := DecodeShedSpec(s.Spec); err != nil {
		return err
	}
	return nil
}

// SameCreate reports whether two creates are the same body: every member
// but RequestID equal.
func SameCreate(a, b Spec) bool {
	a.RequestID, b.RequestID = "", ""
	ja, err1 := json.Marshal(a)
	jb, err2 := json.Marshal(b)
	return err1 == nil && err2 == nil && string(ja) == string(jb)
}
