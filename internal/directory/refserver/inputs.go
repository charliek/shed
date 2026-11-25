package refserver

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

// User is one user as the directory serves them: a name, an admin flag and
// keys (contract rule 8).
type User struct {
	Name  string `json:"name"`
	Admin bool   `json:"admin"`
	Keys  []Key  `json:"keys"`
}

// Key is one of a user's keys: the canonical line, its label and, when it
// expires, the instant (RFC 3339, UTC, whole seconds).
type Key struct {
	Key     string `json:"key"`
	Label   string `json:"label"`
	Expires string `json:"expires,omitempty"`
}

// Code is an enrolment code as the operator issued it.
type Code struct {
	// ServerName is the name the code was issued for: the enrolled server's
	// name in the service.
	ServerName string
	// ExpiresAt is when the code stops being valid; zero, never.
	ExpiresAt time.Time
	// Cancelled says the operator cancelled the code.
	Cancelled bool
}

// Inputs is what the operator controls: the users, the codes and the
// disabled servers. The server reads it on each request that needs it and
// never writes it. Any error is a lookup that failed, as in a storage
// outage: /v1 answers 503 UNAVAILABLE and the token endpoint 503
// server_error.
type Inputs interface {
	// Users returns the list to serve. The server checks it against the
	// contract's rules for a service before serving it.
	Users() ([]User, error)
	// Code looks up an enrolment code: found is false for a code the
	// operator never issued.
	Code(code string) (c Code, found bool, err error)
	// Disabled reports whether the operator disabled the server with this
	// client id.
	Disabled(clientID string) (bool, error)
}

// ErrOutage is the error every lookup of a control directory returns while
// the directory holds the file "outage".
var ErrOutage = errors.New("the control directory holds an outage file: every lookup fails")

// Size limits on the control directory's files.
const (
	maxUsersFile      = 8 << 20
	maxDeliveriesFile = 16 << 20
	maxControlLines   = 1 << 20
)

// ControlDir is the inputs as files in one directory, read on every request
// that needs them:
//
//	users.json      the list as served: [{"name", "admin", "keys": [{"key", "label", "expires"?}]}]
//	codes           one "<server name> <code>" per line; each code is spent once, in memory
//	disabled        one client id per line
//	no-deliveries   one client id per line: servers that may not ask for deliveries (every other may)
//	deliveries.json each person's source: {"<user>": "NO_SOURCE" | Delivery}; a person not in it has none
//	outage          present: every lookup fails, as in a storage outage
//
// The directory and each file must not be writable by group or others, and
// neither may be a symbolic link. users.json missing is a failed lookup
// (never an empty list); codes or disabled missing hold nothing.
//
// The directory is opened once, by OpenControlDir, without following a
// symbolic link, and every file is opened relative to that open directory,
// again without following one; each check is made on the object opened, so
// nothing renamed or replaced between a check and a read can be read.
type ControlDir struct {
	path string
	dir  *os.File
}

// OpenControlDir opens a control directory and checks it. The path is
// cleaned first, so a trailing slash cannot make a symbolic link to a
// directory pass for one.
func OpenControlDir(path string) (*ControlDir, error) {
	if path == "" {
		return nil, errors.New("no control directory")
	}
	clean := filepath.Clean(path)
	fd, err := unix.Open(clean, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		if errors.Is(err, unix.ELOOP) || errors.Is(err, unix.ENOTDIR) {
			return nil, fmt.Errorf("%s is not a directory (a symbolic link is not followed)", path)
		}
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	c := &ControlDir{path: clean, dir: os.NewFile(uintptr(fd), clean)}
	if err := c.Check(); err != nil {
		c.dir.Close()
		return nil, err
	}
	return c, nil
}

// Close closes the directory.
func (c *ControlDir) Close() error { return c.dir.Close() }

// Check checks the open directory itself: a directory, not writable by
// group or others.
func (c *ControlDir) Check() error {
	fi, err := c.dir.Stat()
	if err != nil {
		return err
	}
	if !fi.IsDir() {
		return fmt.Errorf("%s is not a directory", c.path)
	}
	if fi.Mode().Perm()&0o022 != 0 {
		return fmt.Errorf("%s is writable by group or others (mode %04o)", c.path, fi.Mode().Perm())
	}
	return nil
}

// read reads one file of the directory, bounded; exists is false when it is
// not there.
func (c *ControlDir) read(name string, limit int64) (b []byte, exists bool, err error) {
	if err := c.Check(); err != nil {
		return nil, false, err
	}
	dirfd := int(c.dir.Fd())
	var st unix.Stat_t
	if err := unix.Fstatat(dirfd, "outage", &st, unix.AT_SYMLINK_NOFOLLOW); err == nil {
		return nil, false, ErrOutage
	} else if !errors.Is(err, unix.ENOENT) {
		return nil, false, fmt.Errorf("outage: %w", err)
	}
	// O_NONBLOCK: a FIFO in the directory must not hang the read.
	fd, err := unix.Openat(dirfd, name, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_CLOEXEC|unix.O_NONBLOCK, 0)
	switch {
	case errors.Is(err, unix.ENOENT):
		return nil, false, nil
	case errors.Is(err, unix.ELOOP):
		return nil, false, fmt.Errorf("%s is a symbolic link", name)
	case err != nil:
		return nil, false, fmt.Errorf("%s: %w", name, err)
	}
	f := os.NewFile(uintptr(fd), name)
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return nil, false, err
	}
	if !fi.Mode().IsRegular() {
		return nil, false, fmt.Errorf("%s is not a regular file", name)
	}
	if fi.Mode().Perm()&0o022 != 0 {
		return nil, false, fmt.Errorf("%s is writable by group or others (mode %04o)", name, fi.Mode().Perm())
	}
	b, err = io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil {
		return nil, false, err
	}
	if int64(len(b)) > limit {
		return nil, false, fmt.Errorf("%s is larger than %d bytes", name, limit)
	}
	return b, true, nil
}

// lines reads a file of one record per line, each split into fields; blank
// lines are skipped.
func (c *ControlDir) lines(name string) ([][]string, error) {
	b, _, err := c.read(name, maxControlLines)
	if err != nil {
		return nil, err
	}
	var out [][]string
	sc := bufio.NewScanner(bytes.NewReader(b))
	sc.Buffer(make([]byte, 0, 4096), maxControlLines)
	for sc.Scan() {
		if f := strings.Fields(sc.Text()); len(f) > 0 {
			out = append(out, f)
		}
	}
	return out, sc.Err()
}

// Users reads users.json, strictly: one JSON array of users, each member
// named once, no member the file format does not have, none missing.
func (c *ControlDir) Users() ([]User, error) {
	b, exists, err := c.read("users.json", maxUsersFile)
	if err != nil {
		return nil, err
	}
	if !exists {
		return nil, errors.New("users.json is missing")
	}
	return decodeUsersFile(b)
}

// decodeUsersFile decodes the users.json format strictly.
func decodeUsersFile(b []byte) ([]User, error) {
	if err := checkStrictJSON(b, 16); err != nil {
		return nil, fmt.Errorf("users.json: %w", err)
	}
	type fileKey struct {
		Key   *string `json:"key"`
		Label *string `json:"label"`
		// Expires is kept raw: an explicit null must be told apart from an
		// absent member, and refused, never read as "never expires".
		Expires json.RawMessage `json:"expires"`
	}
	type fileUser struct {
		Name  *string    `json:"name"`
		Admin *bool      `json:"admin"`
		Keys  *[]fileKey `json:"keys"`
	}
	var raw []fileUser
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&raw); err != nil {
		return nil, fmt.Errorf("users.json: %w", err)
	}
	if raw == nil {
		return nil, errors.New("users.json: not an array")
	}
	users := make([]User, 0, len(raw))
	for i, u := range raw {
		if u.Name == nil || u.Admin == nil || u.Keys == nil {
			return nil, fmt.Errorf("users.json: user %d lacks name, admin or keys", i)
		}
		out := User{Name: *u.Name, Admin: *u.Admin, Keys: []Key{}}
		for j, k := range *u.Keys {
			if k.Key == nil || k.Label == nil {
				return nil, fmt.Errorf("users.json: user %d, key %d lacks key or label", i, j)
			}
			key := Key{Key: *k.Key, Label: *k.Label}
			if k.Expires != nil {
				exp, ok := jsonString(k.Expires)
				if !ok || exp == "" {
					return nil, fmt.Errorf("users.json: user %d, key %d has an expires that is not a non-empty string", i, j)
				}
				key.Expires = exp
			}
			out.Keys = append(out.Keys, key)
		}
		users = append(users, out)
	}
	return users, nil
}

// Code looks a code up in the file codes.
func (c *ControlDir) Code(code string) (Code, bool, error) {
	recs, err := c.lines("codes")
	if err != nil {
		return Code{}, false, err
	}
	for i, f := range recs {
		if len(f) != 2 {
			return Code{}, false, fmt.Errorf("codes: record %d (blank lines not counted) is not \"<server name> <code>\"", i+1)
		}
	}
	for _, f := range recs {
		if f[1] == code {
			return Code{ServerName: f[0]}, true, nil
		}
	}
	return Code{}, false, nil
}

// Disabled looks a client id up in the file disabled.
func (c *ControlDir) Disabled(clientID string) (bool, error) {
	recs, err := c.lines("disabled")
	if err != nil {
		return false, err
	}
	for i, f := range recs {
		if len(f) != 1 {
			return false, fmt.Errorf("disabled: record %d (blank lines not counted) is not one client id", i+1)
		}
		if f[0] == clientID {
			return true, nil
		}
	}
	return false, nil
}

// DeliveriesDenied looks a client id up in the file no-deliveries: every
// server may ask for deliveries until it is listed there.
func (c *ControlDir) DeliveriesDenied(clientID string) (bool, error) {
	recs, err := c.lines("no-deliveries")
	if err != nil {
		return false, err
	}
	for i, f := range recs {
		if len(f) != 1 {
			return false, fmt.Errorf("no-deliveries: record %d (blank lines not counted) is not one client id", i+1)
		}
		if f[0] == clientID {
			return true, nil
		}
	}
	return false, nil
}

// Delivery looks a person up in deliveries.json: one JSON object from user
// name to "NO_SOURCE" or a Delivery, each member named once. The file
// missing, or a person not in it, is no source.
func (c *ControlDir) Delivery(user string) (Delivery, bool, error) {
	b, exists, err := c.read("deliveries.json", maxDeliveriesFile)
	if err != nil || !exists {
		return Delivery{}, false, err
	}
	if err := checkStrictJSON(b, 16); err != nil {
		return Delivery{}, false, fmt.Errorf("deliveries.json: %w", err)
	}
	var all map[string]json.RawMessage
	if err := json.Unmarshal(b, &all); err != nil || all == nil {
		return Delivery{}, false, errors.New("deliveries.json: not one JSON object")
	}
	raw, ok := all[user]
	if !ok {
		return Delivery{}, false, nil
	}
	d, found, err := decodeDelivery(raw)
	if err != nil {
		return Delivery{}, false, fmt.Errorf("deliveries.json: %s: %w", user, err)
	}
	return d, found, nil
}
