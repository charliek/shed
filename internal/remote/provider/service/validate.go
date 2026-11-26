package service

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/netip"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"golang.org/x/crypto/ssh"
)

// The validation of the contract's section "Validation": a server refuses
// a reply that breaks any rule, and never shows, logs or dials anything of
// it. Every refusal is an *InvalidError naming the rule.

// The rules a machine object can break.
const (
	RuleNotJSON           = "not_json"
	RuleMissingMember     = "missing_member"
	RuleWrongType         = "wrong_type"
	RuleShedIDMismatch    = "shed_id_mismatch"
	RuleServerMismatch    = "server_mismatch"
	RuleNameInvalid       = "name_invalid"
	RuleOwnerInvalid      = "owner_invalid"
	RuleStateInvalid      = "state_invalid"
	RuleWordInvalid       = "word_invalid"
	RuleNumberOutOfRange  = "number_out_of_range"
	RuleTimeInvalid       = "time_invalid"
	RuleLeaseInvalid      = "lease_invalid"
	RuleLabelsInvalid     = "labels_invalid"
	RuleSpecTooLong       = "spec_too_long"
	RuleAccessHostInvalid = "access_host_invalid"
	RuleAccessHostRefused = "access_host_refused"
	RuleAccessPortInvalid = "access_port_invalid"
	RuleAccessUserInvalid = "access_user_invalid"
	RuleAccessKeyInvalid  = "access_host_key_invalid"
)

// MachineRules lists every rule a machine object can break.
var MachineRules = []string{
	RuleNotJSON, RuleMissingMember, RuleWrongType, RuleShedIDMismatch, RuleServerMismatch, RuleNameInvalid,
	RuleOwnerInvalid, RuleStateInvalid, RuleWordInvalid, RuleNumberOutOfRange, RuleTimeInvalid, RuleLeaseInvalid,
	RuleLabelsInvalid, RuleSpecTooLong, RuleAccessHostInvalid, RuleAccessHostRefused, RuleAccessPortInvalid,
	RuleAccessUserInvalid, RuleAccessKeyInvalid,
}

// InvalidError is a reply the contract refuses: Rule names why.
type InvalidError struct {
	Rule   string
	Detail string
}

func (e *InvalidError) Error() string {
	if e.Detail == "" {
		return "invalid reply: " + e.Rule
	}
	return "invalid reply: " + e.Rule + ": " + e.Detail
}

func bad(rule, format string, args ...any) *InvalidError {
	return &InvalidError{Rule: rule, Detail: fmt.Sprintf(format, args...)}
}

// The contract's grammar ("Names and values").
var (
	wordRx      = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,62}$`)
	shedNameRx  = regexp.MustCompile(`^[a-z]([a-z0-9-]{0,61}[a-z0-9])?$`)
	handleRx    = regexp.MustCompile(`^[a-z][a-z0-9-]{0,62}$`)
	shedIDRx    = regexp.MustCompile(`^[a-z2-7]{26}$`)
	operationRx = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)
	labelKeyRx  = regexp.MustCompile(`^[a-z][a-z0-9_]{0,62}$`)
	labelValRx  = regexp.MustCompile(`^[a-z0-9_-]{0,63}$`)
	codeRx      = regexp.MustCompile(`^[A-Z][A-Z0-9_]{0,63}$`)
	requestIDRx = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
)

// ReservedLabels are the labels whose facts are the machine's members:
// never written, and refused in a reply.
var ReservedLabels = []string{"shed_schema", "shed_env", "shed_id", "shed_name", "shed_owner", "shed_user", "shed_user_origin"}

// ValidShedID, ValidOperation and ValidRequestID check an id's grammar.
func ValidShedID(s string) bool    { return shedIDRx.MatchString(s) }
func ValidOperation(s string) bool { return operationRx.MatchString(s) }
func ValidRequestID(s string) bool { return requestIDRx.MatchString(s) }

const timeLayout = "2006-01-02T15:04:05Z"

// Context is what a reply is judged against: the shed it is about (empty
// for a list), the server's own enrolled name, whether the server runs
// with test drivers (which lets an access host be loopback, link-local,
// unspecified or multicast). The credentials a reply is checked against
// are each attempt's own, on its Reply.
type Context struct {
	ShedID      string
	Server      string
	TestDrivers bool
}

// checkStrict checks raw is one JSON value in valid UTF-8, no member named
// twice in any object, nothing after it but whitespace, no deeper than 16,
// and no \u escape of an unpaired UTF-16 surrogate (which Go's decoder
// would repair).
func checkStrict(raw []byte) error {
	if !utf8.Valid(raw) {
		return errors.New("not valid UTF-8")
	}
	if unpairedSurrogate(raw) {
		return errors.New("an unpaired surrogate escape")
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if err := strictValue(dec, 1); err != nil {
		return err
	}
	if _, err := dec.Token(); err != io.EOF {
		return errors.New("data after the JSON value")
	}
	return nil
}

func strictValue(dec *json.Decoder, depth int) error {
	tok, err := dec.Token()
	if err != nil {
		return err
	}
	d, ok := tok.(json.Delim)
	if !ok {
		return nil
	}
	if depth > 16 {
		return errors.New("nested deeper than 16")
	}
	switch d {
	case '{':
		seen := map[string]bool{}
		for dec.More() {
			k, err := dec.Token()
			if err != nil {
				return err
			}
			name, _ := k.(string)
			if seen[name] {
				return fmt.Errorf("the member %q is named twice", name)
			}
			seen[name] = true
			if err := strictValue(dec, depth+1); err != nil {
				return err
			}
		}
	case '[':
		for dec.More() {
			if err := strictValue(dec, depth+1); err != nil {
				return err
			}
		}
	default:
		return fmt.Errorf("an unexpected %q", d)
	}
	_, err = dec.Token()
	return err
}

// unpairedSurrogate reports whether raw holds a \u escape of a surrogate
// that is not one half of a high-then-low pair.
func unpairedSurrogate(raw []byte) bool {
	hex := func(i int) (rune, bool) {
		if i+6 > len(raw) || raw[i] != '\\' || raw[i+1] != 'u' {
			return 0, false
		}
		var r rune
		for _, c := range raw[i+2 : i+6] {
			switch {
			case c >= '0' && c <= '9':
				r = r<<4 | rune(c-'0')
			case c >= 'a' && c <= 'f':
				r = r<<4 | rune(c-'a'+10)
			case c >= 'A' && c <= 'F':
				r = r<<4 | rune(c-'A'+10)
			default:
				return 0, false
			}
		}
		return r, true
	}
	for i := 0; i < len(raw); {
		if raw[i] != '\\' {
			i++
			continue
		}
		r, ok := hex(i)
		if !ok {
			i += 2
			continue
		}
		switch {
		case r >= 0xD800 && r <= 0xDBFF:
			lo, ok := hex(i + 6)
			if !ok || lo < 0xDC00 || lo > 0xDFFF {
				return true
			}
			i += 12
		case r >= 0xDC00 && r <= 0xDFFF:
			return true
		default:
			i += 6
		}
	}
	return false
}

// object is a JSON object's members, read one by one under the rules.
type object map[string]json.RawMessage

// asObject reads a value that must be an object.
func asObject(raw json.RawMessage, where string) (object, error) {
	raw = trim(raw)
	if len(raw) == 0 || raw[0] != '{' {
		return nil, bad(RuleWrongType, "%s is not an object", where)
	}
	var o object
	if err := json.Unmarshal(raw, &o); err != nil {
		return nil, bad(RuleWrongType, "%s is not an object", where)
	}
	return o, nil
}

// decodeTop reads a reply body as one strict JSON object.
func decodeTop(b []byte) (object, error) {
	if err := checkStrict(b); err != nil {
		return nil, bad(RuleNotJSON, "%v", err)
	}
	return asObject(b, "the body")
}

func (o object) need(name string) (json.RawMessage, error) {
	v, ok := o[name]
	if !ok {
		return nil, bad(RuleMissingMember, "%s is missing", name)
	}
	return trim(v), nil
}

// trim drops the JSON whitespace around a value: a value is judged by what
// it is, never by its first byte.
func trim(v json.RawMessage) json.RawMessage { return bytes.Trim(v, " \t\r\n") }

func isNull(raw json.RawMessage) bool { return string(trim(raw)) == "null" }

func (o object) str(name string) (string, error) {
	v, err := o.need(name)
	if err != nil {
		return "", err
	}
	var s string
	if len(v) == 0 || v[0] != '"' || json.Unmarshal(v, &s) != nil {
		return "", bad(RuleWrongType, "%s is not a string", name)
	}
	return s, nil
}

// optStr reads a member that is a string or null.
func (o object) optStr(name string) (string, bool, error) {
	v, err := o.need(name)
	if err != nil {
		return "", false, err
	}
	if isNull(v) {
		return "", false, nil
	}
	s, err := o.str(name)
	return s, err == nil, err
}

func (o object) boolean(name string) (bool, error) {
	v, err := o.need(name)
	if err != nil {
		return false, err
	}
	switch string(v) {
	case "true":
		return true, nil
	case "false":
		return false, nil
	}
	return false, bad(RuleWrongType, "%s is not a boolean", name)
}

// integer reads a JSON integer (no fraction, no exponent) in [min, max].
func (o object) integer(name string, min, max int64) (int64, error) {
	v, err := o.need(name)
	if err != nil {
		return 0, err
	}
	return intValue(v, name, min, max)
}

func intValue(v json.RawMessage, name string, min, max int64) (int64, error) {
	v = trim(v)
	if len(v) == 0 || (v[0] != '-' && (v[0] < '0' || v[0] > '9')) || bytes.ContainsAny(v, ".eE") {
		return 0, bad(RuleWrongType, "%s is not an integer", name)
	}
	var n json.Number
	if json.Unmarshal(v, &n) != nil {
		return 0, bad(RuleWrongType, "%s is not an integer", name)
	}
	i, err := n.Int64()
	if err != nil || i < min || i > max {
		return 0, bad(RuleNumberOutOfRange, "%s is not from %d to %d", name, min, max)
	}
	return i, nil
}

func (o object) array(name string) ([]json.RawMessage, error) {
	v, err := o.need(name)
	if err != nil {
		return nil, err
	}
	var a []json.RawMessage
	if len(v) == 0 || v[0] != '[' || json.Unmarshal(v, &a) != nil {
		return nil, bad(RuleWrongType, "%s is not an array", name)
	}
	for i := range a {
		a[i] = trim(a[i])
	}
	return a, nil
}

// parseTime reads the contract's time: RFC 3339, UTC, Z, whole seconds, a
// real instant.
func parseTime(s, name string) (time.Time, error) {
	t, err := time.Parse(timeLayout, s)
	if err != nil || t.Format(timeLayout) != s {
		return time.Time{}, bad(RuleTimeInvalid, "%s is not an RFC 3339 UTC time in whole seconds", name)
	}
	return t, nil
}

func (o object) time(name string) (time.Time, error) {
	s, err := o.str(name)
	if err != nil {
		return time.Time{}, err
	}
	return parseTime(s, name)
}

func (o object) optTime(name string) (time.Time, error) {
	s, ok, err := o.optStr(name)
	if err != nil || !ok {
		return time.Time{}, err
	}
	return parseTime(s, name)
}

func (o object) word(name string) (string, error) {
	s, err := o.str(name)
	if err != nil {
		return "", err
	}
	if !wordRx.MatchString(s) {
		return "", bad(RuleWordInvalid, "%s breaks the grammar", name)
	}
	return s, nil
}

// refusedClass names the class an access host is in that only test
// drivers allow, or "".
func refusedClass(a netip.Addr) string {
	switch {
	case a.IsLoopback():
		return "loopback"
	case a.IsLinkLocalUnicast(), a.IsLinkLocalMulticast():
		return "link-local"
	case a.IsUnspecified():
		return "unspecified"
	case a.IsMulticast():
		return "multicast"
	}
	return ""
}

// ParseAccessHost reads an access option's host: an IPv4 or IPv6 literal
// in its canonical text, with no zone and no IPv4-mapped IPv6 form; one in
// a refused class (loopback, link-local, unspecified, multicast) only when
// testDrivers is true.
func ParseAccessHost(s string, testDrivers bool) (netip.Addr, error) {
	a, err := netip.ParseAddr(s)
	if err != nil || a.Zone() != "" || a.Is4In6() || a.String() != s {
		return netip.Addr{}, bad(RuleAccessHostInvalid, "the host is not an IP literal in its canonical form")
	}
	if c := refusedClass(a); c != "" && !testDrivers {
		return netip.Addr{}, bad(RuleAccessHostRefused, "the host is %s", c)
	}
	return a, nil
}

// ValidHostKey reports whether a line is one ssh-ed25519 public key in its
// canonical form: the type, one space, the base64 of the key blob, and
// nothing else.
func ValidHostKey(line string) bool {
	typ, rest, ok := strings.Cut(line, " ")
	if !ok || typ != ssh.KeyAlgoED25519 || strings.ContainsAny(rest, " \t\r\n") {
		return false
	}
	pk, comment, options, tail, err := ssh.ParseAuthorizedKey([]byte(line))
	if err != nil || comment != "" || len(options) != 0 || len(tail) != 0 || pk.Type() != ssh.KeyAlgoED25519 {
		return false
	}
	return strings.TrimSuffix(string(ssh.MarshalAuthorizedKey(pk)), "\n") == line
}

// DecodeMachine reads and validates one machine object, as its own JSON
// document.
func DecodeMachine(raw []byte, c Context) (*Machine, error) {
	if err := checkStrict(raw); err != nil {
		return nil, bad(RuleNotJSON, "%v", err)
	}
	return machineValue(raw, c)
}

// machineValue validates a machine object already held to checkStrict (as
// part of a reply). c.ShedID, when set, is the shed the machine must be.
func machineValue(raw json.RawMessage, c Context) (*Machine, error) {
	o, err := asObject(raw, "the machine")
	if err != nil {
		return nil, err
	}
	m := &Machine{}
	if m.ShedID, err = o.str("shed_id"); err != nil {
		return nil, err
	}
	if !shedIDRx.MatchString(m.ShedID) {
		return nil, bad(RuleShedIDMismatch, "shed_id breaks the grammar")
	}
	if c.ShedID != "" && m.ShedID != c.ShedID {
		return nil, bad(RuleShedIDMismatch, "shed_id is not the one asked for")
	}
	if m.Name, err = o.str("name"); err != nil {
		return nil, err
	}
	if !shedNameRx.MatchString(m.Name) {
		return nil, bad(RuleNameInvalid, "name breaks the grammar")
	}
	if m.Server, err = o.str("server"); err != nil {
		return nil, err
	}
	if m.Server != c.Server {
		return nil, bad(RuleServerMismatch, "server is not this server's name")
	}
	user, hasUser, err := o.optStr("user")
	if err != nil {
		return nil, err
	}
	client, hasClient, err := o.optStr("client")
	if err != nil {
		return nil, err
	}
	switch {
	case hasUser == hasClient:
		return nil, bad(RuleOwnerInvalid, "exactly one of user and client is set")
	case hasUser && !handleRx.MatchString(user), hasClient && !handleRx.MatchString(client):
		return nil, bad(RuleOwnerInvalid, "the owner breaks the grammar")
	}
	m.User, m.Client = user, client
	if m.State, err = o.str("state"); err != nil {
		return nil, err
	}
	switch m.State {
	case StateCreating, StateRunning, StateStopped, StateStarting, StateStopping, StateDeleting:
	default:
		return nil, bad(RuleStateInvalid, "state is not one of the six")
	}
	sraw, err := o.need("size")
	if err != nil {
		return nil, err
	}
	so, err := asObject(sraw, "size")
	if err != nil {
		return nil, err
	}
	if m.Size.Name, err = so.word("name"); err != nil {
		return nil, err
	}
	cpus, err := so.integer("cpus", 1, 1024)
	if err != nil {
		return nil, err
	}
	mem, err := so.integer("memory_mb", 1, 1<<24)
	if err != nil {
		return nil, err
	}
	m.Size.CPUs, m.Size.MemoryMB = int(cpus), int(mem)
	if m.Size.Arch, err = so.word("arch"); err != nil {
		return nil, err
	}
	disk, err := o.integer("disk_gb", 1, 65536)
	if err != nil {
		return nil, err
	}
	m.DiskGB = int(disk)
	iraw, err := o.need("image")
	if err != nil {
		return nil, err
	}
	im, err := asObject(iraw, "image")
	if err != nil {
		return nil, err
	}
	if m.Image.Requested, err = im.word("requested"); err != nil {
		return nil, err
	}
	if m.Image.Resolved, err = im.word("resolved"); err != nil {
		return nil, err
	}
	if m.Provider, err = o.word("provider"); err != nil {
		return nil, err
	}
	if m.CreatedAt, err = o.time("created_at"); err != nil {
		return nil, err
	}
	if m.StartedAt, err = o.optTime("started_at"); err != nil {
		return nil, err
	}
	if m.StoppedAt, err = o.optTime("stopped_at"); err != nil {
		return nil, err
	}
	lraw, err := o.need("lease")
	if err != nil {
		return nil, err
	}
	lo, err := asObject(lraw, "lease")
	if err != nil {
		return nil, err
	}
	if m.Lease.ExpiresAt, err = lo.time("expires_at"); err != nil {
		return nil, err
	}
	if m.Lease.MaxExpiresAt, err = lo.time("max_expires_at"); err != nil {
		return nil, err
	}
	if m.Lease.Extendable, err = lo.boolean("extendable"); err != nil {
		return nil, err
	}
	presets, err := lo.array("presets_h")
	if err != nil {
		return nil, err
	}
	if len(presets) > 16 {
		return nil, bad(RuleLeaseInvalid, "more than 16 presets")
	}
	m.Lease.PresetsH = []int{}
	for _, p := range presets {
		h, err := intValue(p, "presets_h", 1, 720)
		if err != nil {
			return nil, err
		}
		m.Lease.PresetsH = append(m.Lease.PresetsH, int(h))
	}
	if m.Lease.ExpiresAt.Before(m.CreatedAt) || m.Lease.MaxExpiresAt.Before(m.Lease.ExpiresAt) {
		return nil, bad(RuleLeaseInvalid, "the lease does not run created_at ≤ expires_at ≤ max_expires_at")
	}
	if err := m.access(o, c); err != nil {
		return nil, err
	}
	lbl, err := o.need("labels")
	if err != nil {
		return nil, err
	}
	if m.Labels, err = labels(lbl); err != nil {
		return nil, err
	}
	if m.Spec, err = o.str("spec"); err != nil {
		return nil, err
	}
	if len(m.Spec) > MaxSpec {
		return nil, bad(RuleSpecTooLong, "spec is over 4 KiB")
	}
	return m, nil
}

// access reads the machine's access options: an ordered list; an option
// of a kind this version does not define is skipped whatever it holds, and
// a member an ssh option does not define is ignored.
func (m *Machine) access(o object, c Context) error {
	list, err := o.array("access")
	if err != nil {
		return err
	}
	for _, raw := range list {
		ao, err := asObject(raw, "an access option")
		if err != nil {
			return err
		}
		kind, err := ao.str("kind")
		if err != nil {
			return err
		}
		if kind != AccessSSH {
			m.Ignored++
			continue
		}
		var a SSHAccess
		host, err := ao.str("host")
		if err != nil {
			return err
		}
		if a.Host, err = ParseAccessHost(host, c.TestDrivers); err != nil {
			return err
		}
		port, err := ao.integer("port", -1<<31, 1<<31)
		if err != nil {
			return err
		}
		if port < 1 || port > 65535 {
			return bad(RuleAccessPortInvalid, "port is not from 1 to 65535")
		}
		a.Port = int(port)
		if a.User, err = ao.str("user"); err != nil {
			return err
		}
		if a.User != LinkUser {
			return bad(RuleAccessUserInvalid, "user is not %s", LinkUser)
		}
		if a.HostKey, err = ao.str("host_key"); err != nil {
			return err
		}
		if !ValidHostKey(a.HostKey) {
			return bad(RuleAccessKeyInvalid, "host_key is not one ssh-ed25519 key")
		}
		m.Access = append(m.Access, a)
	}
	return nil
}

// labels reads a labels object: at most 32, each key and value under the
// grammar, none reserved.
func labels(raw json.RawMessage) (map[string]string, error) {
	lo, err := asObject(raw, "labels")
	if err != nil {
		return nil, err
	}
	if len(lo) > MaxLabels {
		return nil, bad(RuleLabelsInvalid, "more than 32 labels")
	}
	out := map[string]string{}
	for k := range lo {
		for _, r := range ReservedLabels {
			if k == r {
				return nil, bad(RuleLabelsInvalid, "the reserved label %s", k)
			}
		}
		v, err := lo.str(k)
		if err != nil {
			return nil, bad(RuleLabelsInvalid, "a label value is not a string")
		}
		if !labelKeyRx.MatchString(k) || !labelValRx.MatchString(v) {
			return nil, bad(RuleLabelsInvalid, "a label breaks the grammar")
		}
		out[k] = v
	}
	return out, nil
}

// machineMember reads a reply's member machine.
func machineMember(o object, c Context) (*Machine, error) {
	raw, err := o.need("machine")
	if err != nil {
		return nil, err
	}
	return machineValue(raw, c)
}

// operationMember reads a reply's member operation: an operation id.
func operationMember(o object) (string, error) {
	id, err := o.str("operation")
	if err != nil {
		return "", err
	}
	if !operationRx.MatchString(id) {
		return "", bad(RuleWordInvalid, "operation breaks the grammar")
	}
	return id, nil
}

// decodeOperation reads GET /v1/compute/operations/{op}'s 200.
func decodeOperation(b []byte, c Context) (*Operation, error) {
	o, err := decodeTop(b)
	if err != nil {
		return nil, err
	}
	done, err := o.boolean("done")
	if err != nil {
		return nil, err
	}
	op := &Operation{Done: done}
	if !done {
		return op, nil
	}
	mraw, hasMachine := o["machine"]
	eraw, hasError := o["error"]
	switch {
	case hasMachine == hasError:
		return nil, bad(RuleMissingMember, "a done operation has exactly one of machine and error")
	case hasError:
		eo, err := asObject(eraw, "error")
		if err != nil {
			return nil, err
		}
		code, err := eo.str("code")
		if err != nil {
			return nil, err
		}
		if !codeRx.MatchString(code) {
			return nil, bad(RuleWordInvalid, "the error code breaks the grammar")
		}
		op.ErrorCode = code
	case isNull(mraw):
		op.Gone = true
	default:
		if op.Machine, err = machineValue(mraw, c); err != nil {
			return nil, err
		}
	}
	return op, nil
}

// decodeList reads GET /v1/compute/machines's 200.
func decodeList(b []byte, c Context) (*List, error) {
	o, err := decodeTop(b)
	if err != nil {
		return nil, err
	}
	complete, err := o.boolean("complete")
	if err != nil {
		return nil, err
	}
	if !complete {
		return nil, bad("list_incomplete", "complete is false: a list is never partial")
	}
	ms, err := o.array("machines")
	if err != nil {
		return nil, err
	}
	if len(ms) > MaxListMachines {
		return nil, bad("list_too_long", "more than %d machines", MaxListMachines)
	}
	l := &List{}
	seen := map[string]bool{}
	lc := c
	lc.ShedID = ""
	for _, raw := range ms {
		m, err := machineValue(raw, lc)
		if err != nil {
			return nil, err
		}
		if seen[m.ShedID] {
			return nil, bad(RuleShedIDMismatch, "a shed id is listed twice")
		}
		seen[m.ShedID] = true
		l.Machines = append(l.Machines, *m)
	}
	ds, err := o.array("deleted")
	if err != nil {
		return nil, err
	}
	for _, raw := range ds {
		do, err := asObject(raw, "a deletion")
		if err != nil {
			return nil, err
		}
		var d Deletion
		if d.ShedID, err = do.str("shed_id"); err != nil {
			return nil, err
		}
		if !shedIDRx.MatchString(d.ShedID) {
			return nil, bad(RuleShedIDMismatch, "a deletion's shed_id breaks the grammar")
		}
		if d.At, err = do.time("at"); err != nil {
			return nil, err
		}
		if d.Cause, err = do.str("cause"); err != nil {
			return nil, err
		}
		switch d.Cause {
		case CauseLeaseEnded, CausePage, CauseExternal, CauseServer:
		default:
			return nil, bad(RuleWordInvalid, "a deletion's cause is not one of the four")
		}
		l.Deleted = append(l.Deleted, d)
	}
	return l, nil
}

// decodeUsage reads GET /v1/compute/usage's 200.
func decodeUsage(b []byte) (*Usage, error) {
	o, err := decodeTop(b)
	if err != nil {
		return nil, err
	}
	counts := func(raw json.RawMessage, name string) (*Counts, error) {
		co, err := asObject(raw, name)
		if err != nil {
			return nil, err
		}
		var n Counts
		for _, f := range []struct {
			name string
			dst  *int
		}{{"machines", &n.Machines}, {"running", &n.Running}, {"stopped", &n.Stopped}, {"max", &n.Max}} {
			v, err := co.integer(f.name, 0, MaxCount)
			if err != nil {
				return nil, err
			}
			*f.dst = int(v)
		}
		if n.Running+n.Stopped > n.Machines {
			return nil, bad(RuleNumberOutOfRange, "%s counts more running and stopped than machines", name)
		}
		return &n, nil
	}
	u := &Usage{}
	praw, err := o.need("person")
	if err != nil {
		return nil, err
	}
	if !isNull(praw) {
		if u.Person, err = counts(praw, "person"); err != nil {
			return nil, err
		}
	}
	oraw, err := o.need("org")
	if err != nil {
		return nil, err
	}
	org, err := counts(oraw, "org")
	if err != nil {
		return nil, err
	}
	u.Org = *org
	return u, nil
}

// decodeIngress reads GET and PUT /v1/compute/ingress's 200.
func decodeIngress(b []byte) (*Ingress, error) {
	o, err := decodeTop(b)
	if err != nil {
		return nil, err
	}
	srcs, err := o.array("sources")
	if err != nil {
		return nil, err
	}
	if len(srcs) > MaxIngressSources {
		return nil, bad(RuleNumberOutOfRange, "more than %d sources", MaxIngressSources)
	}
	in := &Ingress{}
	seen := map[string]bool{}
	for _, raw := range srcs {
		var s string
		if len(raw) == 0 || raw[0] != '"' || json.Unmarshal(raw, &s) != nil {
			return nil, bad(RuleWrongType, "a source is not a string")
		}
		p, err := netip.ParsePrefix(s)
		if err != nil || p.Masked() != p || p.String() != s || p.Addr().Is4In6() || seen[s] {
			return nil, bad(RuleWordInvalid, "a source is not a canonical prefix, or is named twice")
		}
		seen[s] = true
		in.Sources = append(in.Sources, p)
	}
	if in.Exclusive, err = o.boolean("exclusive"); err != nil {
		return nil, err
	}
	if in.AppliedAt, err = o.optTime("applied_at"); err != nil {
		return nil, err
	}
	return in, nil
}

// decodeCatalog reads GET /v1/compute/catalog's 200, member by member
// under the strict reader (exact member names, every member required, no
// null).
func decodeCatalog(b []byte) (*Catalog, error) {
	o, err := decodeTop(b)
	if err != nil {
		return nil, err
	}
	var cat Catalog
	sizes, err := o.array("sizes")
	if err != nil {
		return nil, err
	}
	if len(sizes) == 0 || len(sizes) > 64 {
		return nil, bad(RuleNumberOutOfRange, "the catalog has 1 to 64 sizes")
	}
	names := map[string]bool{}
	for _, raw := range sizes {
		so, err := asObject(raw, "a size")
		if err != nil {
			return nil, err
		}
		var z Size
		if z.Name, err = so.word("name"); err != nil {
			return nil, err
		}
		if z.Arch, err = so.word("arch"); err != nil {
			return nil, err
		}
		for _, f := range []struct {
			name     string
			min, max int64
			dst      *int
		}{{"cpus", 1, 1024, &z.CPUs}, {"memory_mb", 1, 1 << 24, &z.MemoryMB}, {"disk_gb_default", 1, 65536, &z.DiskGBDefault}} {
			v, err := so.integer(f.name, f.min, f.max)
			if err != nil {
				return nil, err
			}
			*f.dst = int(v)
		}
		if names[z.Name] {
			return nil, bad(RuleWordInvalid, "a size is named twice")
		}
		names[z.Name] = true
		cat.Sizes = append(cat.Sizes, z)
	}
	if cat.DefaultSize, err = o.word("default_size"); err != nil {
		return nil, err
	}
	if !names[cat.DefaultSize] {
		return nil, bad(RuleWordInvalid, "default_size is not a size")
	}
	iraw, err := o.need("images")
	if err != nil {
		return nil, err
	}
	im, err := asObject(iraw, "images")
	if err != nil {
		return nil, err
	}
	if cat.Images.Default, err = im.word("default"); err != nil {
		return nil, err
	}
	if cat.Images.Aliases, err = words(im, "aliases", 0, 64); err != nil {
		return nil, err
	}
	for _, a := range cat.Images.Aliases {
		if a == "default" {
			return nil, bad(RuleWordInvalid, "default is not an alias")
		}
	}
	if cat.AccessKinds, err = words(o, "access_kinds", 1, 8); err != nil {
		return nil, err
	}
	praw, err := o.need("policy")
	if err != nil {
		return nil, err
	}
	po, err := asObject(praw, "policy")
	if err != nil {
		return nil, err
	}
	perraw, err := po.need("person")
	if err != nil {
		return nil, err
	}
	pp, err := asObject(perraw, "policy.person")
	if err != nil {
		return nil, err
	}
	if cat.Policy.Person.InitialLeaseS, err = pp.integer("initial_lease_s", 1, MaxLeaseS); err != nil {
		return nil, err
	}
	if cat.Policy.Person.MaxAgeS, err = pp.integer("max_age_s", 1, MaxLeaseS); err != nil {
		return nil, err
	}
	if cat.Policy.Person.MaxAgeS < cat.Policy.Person.InitialLeaseS {
		return nil, bad(RuleNumberOutOfRange, "max_age_s is below initial_lease_s")
	}
	if cat.Policy.Person.ExtendAllowed, err = pp.boolean("extend_allowed"); err != nil {
		return nil, err
	}
	presets, err := pp.array("presets_h")
	if err != nil {
		return nil, err
	}
	if len(presets) > 16 {
		return nil, bad(RuleNumberOutOfRange, "more than 16 presets")
	}
	cat.Policy.Person.PresetsH = []int{}
	for _, p := range presets {
		h, err := intValue(p, "presets_h", 1, 720)
		if err != nil {
			return nil, err
		}
		cat.Policy.Person.PresetsH = append(cat.Policy.Person.PresetsH, int(h))
	}
	craw, err := po.need("client")
	if err != nil {
		return nil, err
	}
	pc, err := asObject(craw, "policy.client")
	if err != nil {
		return nil, err
	}
	if cat.Policy.Client.MaxLeaseS, err = pc.integer("max_lease_s", 1, MaxLeaseS); err != nil {
		return nil, err
	}
	if cat.Policy.WarnS, err = po.integer("warn_s", 0, MaxLeaseS); err != nil {
		return nil, err
	}
	lraw, err := o.need("limits")
	if err != nil {
		return nil, err
	}
	lo, err := asObject(lraw, "limits")
	if err != nil {
		return nil, err
	}
	pm, err := lo.integer("person_max", 0, MaxCount)
	if err != nil {
		return nil, err
	}
	om, err := lo.integer("org_max", 0, MaxCount)
	if err != nil {
		return nil, err
	}
	cat.Limits = Limits{PersonMax: int(pm), OrgMax: int(om)}
	inraw, err := o.need("ingress")
	if err != nil {
		return nil, err
	}
	ino, err := asObject(inraw, "ingress")
	if err != nil {
		return nil, err
	}
	if cat.Ingress.Writable, err = ino.boolean("writable"); err != nil {
		return nil, err
	}
	return &cat, nil
}

// words reads an array of min to max words, none twice.
func words(o object, name string, min, max int) ([]string, error) {
	list, err := o.array(name)
	if err != nil {
		return nil, err
	}
	if len(list) < min || len(list) > max {
		return nil, bad(RuleNumberOutOfRange, "%s holds %d to %d names", name, min, max)
	}
	out := []string{}
	seen := map[string]bool{}
	for _, raw := range list {
		var s string
		if len(raw) == 0 || raw[0] != '"' || json.Unmarshal(raw, &s) != nil {
			return nil, bad(RuleWrongType, "%s holds a value that is not a string", name)
		}
		if !wordRx.MatchString(s) || seen[s] {
			return nil, bad(RuleWordInvalid, "%s holds a name outside the grammar, or twice", name)
		}
		seen[s] = true
		out = append(out, s)
	}
	return out, nil
}

// decodeAccepted reads a create, start, stop or delete's 2xx.
func decodeAccepted(call Call, status int, b []byte, c Context) (*Accepted, error) {
	if call == CallDelete && status == 204 {
		return &Accepted{Gone: true}, nil
	}
	o, err := decodeTop(b)
	if err != nil {
		return nil, err
	}
	a := &Accepted{}
	if status == 202 {
		if a.Operation, err = operationMember(o); err != nil {
			return nil, err
		}
		if call != CallCreate {
			return a, nil
		}
	}
	if a.Machine, err = machineMember(o, c); err != nil {
		return nil, err
	}
	return a, nil
}
