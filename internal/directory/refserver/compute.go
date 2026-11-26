package refserver

import (
	"encoding/json"
	"net/netip"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

// The reference compute service (tests/compute-contract/v1/CONTRACT.md):
// the compute scope and /v1/compute/*, held to every exchange of that
// contract by this package's tests. Its cloud is in memory: a machine is a
// record that moves between states when a cloud operation it started is
// settled (by the set-up step cloud.settle, or after auto_settle_s in the
// command's control file), and a lease ends when the sweep runs at a clock
// at or past it (the step clock.advance, or the command's timer). Every
// judgement is made before any cloud work, as the contract says; a machine
// counts against the limits from the moment it is reserved. It imports no
// shed package.
//
// Its state, unlike the directory's, changes over HTTP: machines are made,
// started, stopped, extended and deleted by the calls. Grants, registered
// clients, the policy, opt-outs and faults change only through set-up
// steps (the tests' hooks, or the command's control file compute.json).

const (
	scopeCompute = "compute:manage"
	// pathComputePrefix is the compute routes' common prefix.
	pathComputePrefix = "/v1/compute/"
	// maxComputeRequest is the largest request body.
	maxComputeRequest = 64 << 10
	// maxComputeReply and maxComputeList are the reply limits: every reply
	// but the machine list, and the machine list.
	maxComputeReply = 64 << 10
	maxComputeList  = 4 << 20
	// maxComputeListMachines is the most machines one list holds.
	maxComputeListMachines = 500
	// computeKeep is how long a finished operation, a kept answer and a
	// recent deletion are kept.
	computeKeep = 24 * time.Hour
	// computeLinkUser is the only user an ssh access option names.
	computeLinkUser = "shed-link"
	// maxSpec is the largest spec, in bytes.
	maxSpec = 4096
	// maxLabels is the most labels a machine holds.
	maxLabels = 32
	// maxIngressSources is the most sources one ingress write names.
	maxIngressSources = 64
	// minExtend, maxExtend bound an extend's by_s (whole hours).
	minExtend = 3600
	maxExtend = 2592000
	// minLease bounds a client's lease_s from below.
	minLease = 60
)

// The machine states.
const (
	stateCreating = "creating"
	stateRunning  = "running"
	stateStopped  = "stopped"
	stateStarting = "starting"
	stateStopping = "stopping"
	stateDeleting = "deleting"
)

// The deletion causes the list reports.
const (
	causeLeaseEnded = "lease_ended"
	causePage       = "page"
	causeExternal   = "external"
	causeServer     = "server"
)

// The grammar of the contract's names (section "Names and values").
var (
	wordRx       = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,62}$`)
	shedNameRx   = regexp.MustCompile(`^[a-z]([a-z0-9-]{0,61}[a-z0-9])?$`)
	handleRx     = regexp.MustCompile(`^[a-z][a-z0-9-]{0,62}$`)
	computeIDRx  = regexp.MustCompile(`^[a-z2-7]{26}$`)
	requestIDRx4 = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
	operationRx  = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)
	labelKeyRx   = regexp.MustCompile(`^[a-z][a-z0-9_]{0,62}$`)
	labelValRx   = regexp.MustCompile(`^[a-z0-9_-]{0,63}$`)
	// authorizedKeyRx is one public key line: a type, one space, standard
	// base64, and an optional comment of printable ASCII.
	authorizedKeyRx = regexp.MustCompile(`^[a-z0-9@.-]{1,64} [A-Za-z0-9+/]+={0,2}( [\x20-\x7e]*)?$`)
)

// reservedLabels are the labels a machine's members say, never written.
var reservedLabels = map[string]bool{
	"shed_schema": true, "shed_env": true, "shed_id": true, "shed_name": true,
	"shed_owner": true, "shed_user": true, "shed_user_origin": true,
}

// ComputeWorld is the compute contract's world (its fixtures/world.json):
// the state every exchange starts from, beside the directory contract's.
type ComputeWorld struct {
	Contract int            `json:"contract"`
	Provider string         `json:"provider"`
	Catalog  ComputeCatalog `json:"catalog"`
	Policy   ComputePolicy  `json:"policy"`
	Access   ComputeAccess  `json:"access"`
}

// ComputeCatalog is what the service offers.
type ComputeCatalog struct {
	Sizes       []ComputeSize `json:"sizes"`
	DefaultSize string        `json:"default_size"`
	Images      ComputeImages `json:"images"`
	AccessKinds []string      `json:"access_kinds"`
}

// ComputeSize is one size of the catalog.
type ComputeSize struct {
	Name          string `json:"name"`
	CPUs          int    `json:"cpus"`
	MemoryMB      int    `json:"memory_mb"`
	Arch          string `json:"arch"`
	DiskGBDefault int    `json:"disk_gb_default"`
}

// ComputeImages names the image default resolves to and each alias's.
type ComputeImages struct {
	Default string            `json:"default"`
	Aliases map[string]string `json:"aliases"`
}

// ComputePolicy is the organization's policy, changed by policy.set.
type ComputePolicy struct {
	InitialLeaseS    int64               `json:"initial_lease_s"`
	MaxAgeS          int64               `json:"max_age_s"`
	ExtendAllowed    bool                `json:"extend_allowed"`
	PresetsH         []int               `json:"presets_h"`
	ClientMaxLeaseS  int64               `json:"client_max_lease_s"`
	WarnS            int64               `json:"warn_s"`
	PersonMax        int                 `json:"person_max"`
	OrgMax           int                 `json:"org_max"`
	Sizes            map[string][]string `json:"sizes"`
	IngressWritable  bool                `json:"ingress_writable"`
	IngressMinPrefix struct {
		V4 int `json:"v4"`
		V6 int `json:"v6"`
	} `json:"ingress_min_prefix"`
}

// ComputeAccess is where every running machine is reached.
type ComputeAccess struct {
	Host string `json:"host"`
	Port int    `json:"port"`
}

// DefaultComputeWorld is the contract's world, as the command starts from
// (a test holds it equal to fixtures/world.json).
func DefaultComputeWorld() ComputeWorld {
	w := ComputeWorld{
		Contract: 1,
		Provider: "ref",
		Catalog: ComputeCatalog{
			Sizes: []ComputeSize{
				{Name: "standard", CPUs: 2, MemoryMB: 8192, Arch: "amd64", DiskGBDefault: 50},
				{Name: "large", CPUs: 8, MemoryMB: 32768, Arch: "amd64", DiskGBDefault: 100},
			},
			DefaultSize: "standard",
			Images: ComputeImages{
				Default: "shed-remote-full-v1-0-0-20261101-000000",
				Aliases: map[string]string{
					"base":  "shed-remote-base-v1-0-0-20261101-000000",
					"cloud": "shed-remote-full-v1-0-0-20261101-000000",
				},
			},
			AccessKinds: []string{"ssh"},
		},
		Policy: ComputePolicy{
			InitialLeaseS:   86400,
			MaxAgeS:         432000,
			ExtendAllowed:   true,
			PresetsH:        []int{6, 12, 24, 48},
			ClientMaxLeaseS: 432000,
			WarnS:           7200,
			PersonMax:       4,
			OrgMax:          10,
			Sizes:           map[string][]string{"admin": {"standard", "large"}, "member": {"standard"}},
			IngressWritable: true,
		},
		Access: ComputeAccess{Host: "127.0.0.1", Port: 22},
	}
	w.Policy.IngressMinPrefix.V4 = 24
	w.Policy.IngressMinPrefix.V6 = 48
	return w
}

// mkey names a machine: the server's enrolled name and the shed id.
type mkey struct{ server, shedID string }

// cmachine is one machine.
type cmachine struct {
	server, shedID, name string
	// incarnation tells this machine from a later one with the same shed
	// id; an operation is bound to the incarnation it was made for.
	incarnation         string
	user, client        string // exactly one is set
	state               string
	size                ComputeSize
	diskGB              int
	imageReq, imageRes  string
	created             time.Time
	started, stopped    time.Time // zero: null
	expires, maxExpires time.Time
	labels              map[string]string
	spec                string
	hostKey             string
	body                createBody // the create body, for the fingerprint
	pending             *coperation
}

// coperation is one operation.
type coperation struct {
	id, server, shedID, kind string
	incarnation              string
	done                     bool
	errCode                  string
	at, doneAt               time.Time
	// fail is the cloud's refusal when it settles: quota or capacity.
	fail string
}

// canswer is a kept answer to a request id.
type canswer struct {
	route  string
	status int
	body   []byte
	at     time.Time
}

// cdeletion is one recent deletion.
type cdeletion struct {
	server, shedID, cause string
	at                    time.Time
}

// cingress is one server's ingress.
type cingress struct {
	sources []string
	applied time.Time
}

type rkey struct{ server, requestID string }

// computeState is the whole compute service.
type computeState struct {
	mu       sync.Mutex
	world    ComputeWorld
	offset   time.Duration
	machines map[mkey]*cmachine
	ops      map[string]*coperation
	answers  map[rkey]*canswer
	deleted  []cdeletion
	ingress  map[string]*cingress
	grants   map[string]bool            // client ids allowed compute
	clients  map[string]map[string]bool // server name -> registered clients
	optouts  map[[2]string]bool         // {handle, server}
	// fillPerson and fillOrg are machines of other servers, counted
	// against the limits (quota.fill), all running.
	fillPerson map[string]int
	fillOrg    int
	// fails are the one-shot faults (cloud.fail), by call.
	fails map[string]string
	// breaks are the ingress faults (ingress.break): each clause of
	// exclusive that no longer holds, or "unreadable".
	breaks map[string]bool
	// users is the list of people read for the call being answered
	// (whether a machine is extendable depends on its person).
	users []User
	// logf logs the service's audit lines (the server's Logf).
	logf func(string, ...any)
	// Set by the command's control file.
	hostKeyDefault string
	autoSettle     time.Duration
	replies        map[string]ComputeReply
	applied        map[string]bool
}

func newComputeState() *computeState {
	return &computeState{
		world:      DefaultComputeWorld(),
		machines:   map[mkey]*cmachine{},
		ops:        map[string]*coperation{},
		answers:    map[rkey]*canswer{},
		ingress:    map[string]*cingress{},
		grants:     map[string]bool{},
		clients:    map[string]map[string]bool{},
		optouts:    map[[2]string]bool{},
		fillPerson: map[string]int{},
		fails:      map[string]string{},
		breaks:     map[string]bool{},
		logf:       func(string, ...any) {},
		replies:    map[string]ComputeReply{},
		applied:    map[string]bool{},
	}
}

// createBody is what a create's body says; two creates are the same when
// these are equal.
type createBody struct {
	name, user, client, size, image string
	diskGB                          int // 0: absent
	hasDisk                         bool
	labels                          map[string]string
	keys                            []string
	spec                            string
	accessKinds                     []string
	leaseS                          int64
	hasLease                        bool
}

func (a createBody) equal(b createBody) bool {
	eqStrs := func(x, y []string) bool {
		if len(x) != len(y) {
			return false
		}
		for i := range x {
			if x[i] != y[i] {
				return false
			}
		}
		return true
	}
	if a.name != b.name || a.user != b.user || a.client != b.client || a.size != b.size || a.image != b.image ||
		a.hasDisk != b.hasDisk || a.diskGB != b.diskGB || a.spec != b.spec || a.hasLease != b.hasLease || a.leaseS != b.leaseS ||
		!eqStrs(a.keys, b.keys) || !eqStrs(a.accessKinds, b.accessKinds) || len(a.labels) != len(b.labels) {
		return false
	}
	for k, v := range a.labels {
		if w, ok := b.labels[k]; !ok || w != v {
			return false
		}
	}
	return true
}

// computeRefusal is a refusal: a status and a code.
type computeRefusal struct {
	status int
	code   string
	msg    string
}

func (r *computeRefusal) Error() string { return r.code + ": " + r.msg }

func refuse(status int, code, msg string) *computeRefusal {
	return &computeRefusal{status: status, code: code, msg: msg}
}

// invalid is the refusal of a malformed request.
func invalid(msg string) *computeRefusal { return refuse(422, "INVALID", msg) }

// decodeComputeBody reads a request body: one strict JSON object of at
// most 64 KiB, no member named twice, no unpaired surrogate; its members
// by name (unknown members are the caller's to ignore).
func decodeComputeBody(b []byte) (map[string]json.RawMessage, *computeRefusal) {
	if checkStrictJSON(b, 16) != nil || unpairedSurrogate(b) {
		return nil, invalid("the body is not one JSON object")
	}
	m, err := objectMembers(b)
	if err != nil {
		return nil, invalid("the body is not one JSON object")
	}
	// A null is refused where a member is read (every reader refuses it);
	// a member nobody reads is ignored, null or not.
	return m, nil
}

// requestID reads a body's request_id.
func requestID(m map[string]json.RawMessage) (string, *computeRefusal) {
	id, ok := jsonString(m["request_id"])
	if !ok || !requestIDRx4.MatchString(id) {
		return "", invalid("request_id is not a lowercase UUID")
	}
	return id, nil
}

// stringList reads an array of strings.
func stringList(raw json.RawMessage) ([]string, bool) {
	if len(raw) == 0 || raw[0] != '[' {
		return nil, false
	}
	var elems []json.RawMessage
	if json.Unmarshal(raw, &elems) != nil {
		return nil, false
	}
	out := make([]string, 0, len(elems))
	for _, e := range elems {
		s, ok := jsonString(e)
		if !ok {
			return nil, false
		}
		out = append(out, s)
	}
	return out, true
}

// labelSet reads a labels object: each key and value under the grammar,
// none reserved. nullable allows null values (a write's set and expect).
func labelSet(raw json.RawMessage, nullable bool) (map[string]*string, *computeRefusal) {
	if len(raw) == 0 || raw[0] != '{' {
		return nil, invalid("labels are not an object")
	}
	var m map[string]json.RawMessage
	if json.Unmarshal(raw, &m) != nil {
		return nil, invalid("labels are not an object")
	}
	out := map[string]*string{}
	for k := range m {
		if reservedLabels[k] {
			return nil, refuse(422, "RESERVED_LABEL", "the label "+k+" is reserved")
		}
	}
	for k, v := range m {
		if !labelKeyRx.MatchString(k) {
			return nil, invalid("a label key breaks the grammar")
		}
		if string(v) == "null" {
			if !nullable {
				return nil, invalid("a label value is null")
			}
			out[k] = nil
			continue
		}
		s, ok := jsonString(v)
		if !ok || !labelValRx.MatchString(s) {
			return nil, invalid("a label value breaks the grammar")
		}
		out[k] = &s
	}
	return out, nil
}

// parseCreateBody reads a create's body. The checks a body fails by itself
// come before anything is looked up.
func parseCreateBody(b []byte) (string, createBody, *computeRefusal) {
	var body createBody
	m, r := decodeComputeBody(b)
	if r != nil {
		return "", body, r
	}
	rid, r := requestID(m)
	if r != nil {
		return "", body, r
	}
	// Reserved labels first: a server that sends one has a bug the code
	// names.
	if raw, ok := m["labels"]; ok {
		ls, r := labelSet(raw, false)
		if r != nil {
			return "", body, r
		}
		if len(ls) > maxLabels {
			return "", body, invalid("more than 32 labels")
		}
		body.labels = map[string]string{}
		for k, v := range ls {
			body.labels[k] = *v
		}
	} else {
		return "", body, invalid("labels is missing")
	}
	var ok bool
	if body.name, ok = jsonString(m["name"]); !ok || !shedNameRx.MatchString(body.name) {
		return "", body, invalid("name breaks the grammar")
	}
	_, hasUser := m["user"]
	_, hasClient := m["client"]
	switch {
	case hasUser == hasClient:
		return "", body, invalid("exactly one of user and client")
	case hasUser:
		if body.user, ok = jsonString(m["user"]); !ok || !handleRx.MatchString(body.user) {
			return "", body, invalid("user breaks the grammar")
		}
	default:
		if body.client, ok = jsonString(m["client"]); !ok || !handleRx.MatchString(body.client) {
			return "", body, invalid("client breaks the grammar")
		}
	}
	if body.size, ok = jsonString(m["size"]); !ok || !wordRx.MatchString(body.size) {
		return "", body, invalid("size breaks the grammar")
	}
	if body.image, ok = jsonString(m["image"]); !ok || !wordRx.MatchString(body.image) {
		return "", body, invalid("image breaks the grammar")
	}
	if raw, present := m["disk_gb"]; present {
		n, ok := jsonInt(raw)
		if !ok || n < 1 || n > 65536 {
			return "", body, invalid("disk_gb is not an integer from 1 to 65536")
		}
		body.diskGB, body.hasDisk = int(n), true
	}
	keys, ok := stringList(m["authorized_keys"])
	if !ok || len(keys) < 1 || len(keys) > 4 {
		return "", body, invalid("authorized_keys is not 1 to 4 lines")
	}
	for _, k := range keys {
		if len(k) > 8192 || !authorizedKeyRx.MatchString(k) {
			return "", body, invalid("an authorized key is not one public key line")
		}
	}
	body.keys = keys
	if body.spec, ok = jsonString(m["spec"]); !ok || len(body.spec) > maxSpec {
		return "", body, invalid("spec is not a string of at most 4 KiB")
	}
	kinds, ok := stringList(m["access_kinds"])
	if !ok || len(kinds) < 1 || len(kinds) > 8 {
		return "", body, invalid("access_kinds is not 1 to 8 names")
	}
	for _, k := range kinds {
		if !wordRx.MatchString(k) {
			return "", body, invalid("an access kind breaks the grammar")
		}
	}
	body.accessKinds = kinds
	if raw, present := m["lease_s"]; present {
		n, ok := jsonInt(raw)
		if !ok || n < minLease {
			return "", body, invalid("lease_s is not an integer of at least 60")
		}
		body.leaseS, body.hasLease = n, true
	}
	if hasUser && body.hasLease {
		return "", body, invalid("a person's machine carries no lease_s")
	}
	if hasClient && !body.hasLease {
		return "", body, invalid("a client's machine needs lease_s")
	}
	return rid, body, nil
}

// canonicalCIDR reports whether s is a prefix in its canonical form: the
// network address and the prefix length, as Go prints it.
func canonicalCIDR(s string) (netip.Prefix, bool) {
	p, err := netip.ParsePrefix(s)
	if err != nil || p.Masked() != p || p.String() != s || p.Addr().Zone() != "" || p.Addr().Is4In6() {
		return netip.Prefix{}, false
	}
	return p, true
}

// fmtTime writes an instant as the contract does: UTC, Z, whole seconds.
func fmtTime(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return t.UTC().Format(timeLayout)
}

// sizeOf looks a size up in the catalog.
func (c *computeState) sizeOf(name string) (ComputeSize, bool) {
	for _, s := range c.world.Catalog.Sizes {
		if s.Name == name {
			return s, true
		}
	}
	return ComputeSize{}, false
}

// resolveImage resolves default or an alias.
func (c *computeState) resolveImage(name string) (string, bool) {
	if name == "default" {
		return c.world.Catalog.Images.Default, true
	}
	r, ok := c.world.Catalog.Images.Aliases[name]
	return r, ok
}

// machineJSON is a machine as the contract writes it.
func (c *computeState) machineJSON(m *cmachine, now time.Time) map[string]any {
	nilOr := func(s string) any {
		if s == "" {
			return nil
		}
		return s
	}
	presets := []int{}
	extendable := false
	if m.user != "" {
		presets = append(presets, c.world.Policy.PresetsH...)
		// Whether an extend could be accepted now: every check of extend
		// but the request's own hours, and at least an hour left to the cap.
		live, _ := c.personLive(c.users, m.user, m.server)
		extendable = c.world.Policy.ExtendAllowed && m.state != stateDeleting && live &&
			now.Before(m.expires) && !m.expires.Add(time.Hour).After(m.maxExpires)
	}
	access := []any{}
	if m.state == stateRunning && m.hostKey != "" {
		access = append(access, map[string]any{
			"kind": "ssh", "host": c.world.Access.Host, "port": c.world.Access.Port,
			"user": computeLinkUser, "host_key": m.hostKey,
		})
	}
	labels := map[string]string{}
	for k, v := range m.labels {
		labels[k] = v
	}
	return map[string]any{
		"shed_id": m.shedID, "name": m.name, "server": m.server,
		"user": nilOr(m.user), "client": nilOr(m.client), "state": m.state,
		"size":       map[string]any{"name": m.size.Name, "cpus": m.size.CPUs, "memory_mb": m.size.MemoryMB, "arch": m.size.Arch},
		"disk_gb":    m.diskGB,
		"image":      map[string]any{"requested": m.imageReq, "resolved": m.imageRes},
		"provider":   c.world.Provider,
		"created_at": fmtTime(m.created), "started_at": fmtTime(m.started), "stopped_at": fmtTime(m.stopped),
		"lease": map[string]any{
			"expires_at": fmtTime(m.expires), "max_expires_at": fmtTime(m.maxExpires),
			"extendable": extendable, "presets_h": presets,
		},
		"access": access, "labels": labels, "spec": m.spec,
	}
}

// counts are a usage line.
type counts struct{ machines, running, stopped int }

func (n counts) json(max int) map[string]any {
	return map[string]any{"machines": n.machines, "running": n.running, "stopped": n.stopped, "max": max}
}

func (n *counts) add(state string) {
	n.machines++
	switch state {
	case stateCreating, stateStarting, stateRunning:
		n.running++
	case stateStopping, stateStopped:
		n.stopped++
	}
}

// personCounts counts a person's machines on every server, and the
// machines quota.fill gave them.
func (c *computeState) personCounts(handle string) counts {
	var n counts
	for _, m := range c.machines {
		if m.user == handle {
			n.add(m.state)
		}
	}
	for i := 0; i < c.fillPerson[handle]; i++ {
		n.add(stateRunning)
	}
	return n
}

// orgCounts counts every machine of the organization.
func (c *computeState) orgCounts() counts {
	var n counts
	for _, m := range c.machines {
		n.add(m.state)
	}
	for _, f := range c.fillPerson {
		for i := 0; i < f; i++ {
			n.add(stateRunning)
		}
	}
	for i := 0; i < c.fillOrg; i++ {
		n.add(stateRunning)
	}
	return n
}

// newOperation starts an operation on a machine.
func (c *computeState) newOperation(m *cmachine, kind, fail string, now time.Time) *coperation {
	op := &coperation{id: "op-" + randomHex(16), server: m.server, shedID: m.shedID, incarnation: m.incarnation, kind: kind, fail: fail, at: now}
	c.ops[op.id] = op
	m.pending = op
	return op
}

// finish ends an operation.
func (c *computeState) finish(op *coperation, errCode string, now time.Time) {
	op.done, op.errCode, op.doneAt = true, errCode, now
}

// remove forgets a machine; a deletion with a cause is listed for 24 h.
func (c *computeState) remove(m *cmachine, cause string, now time.Time) {
	delete(c.machines, mkey{m.server, m.shedID})
	if cause != "" {
		c.deleted = append(c.deleted, cdeletion{server: m.server, shedID: m.shedID, cause: cause, at: now})
	}
}

// settle finishes the cloud work in progress on a machine.
func (c *computeState) settle(m *cmachine, now time.Time) {
	op := m.pending
	if op == nil {
		return
	}
	m.pending = nil
	switch op.kind {
	case "create":
		if op.fail != "" {
			c.finish(op, cloudCode(op.fail), now)
			c.remove(m, "", now) // a refused create leaves nothing, and is no deletion
			return
		}
		m.state, m.started = stateRunning, now
		if m.hostKey == "" && c.hostKeyDefault != "" {
			m.hostKey = c.hostKeyDefault
		}
	case "start":
		if op.fail != "" {
			c.finish(op, cloudCode(op.fail), now)
			m.state = stateStopped
			return
		}
		m.state, m.started, m.stopped = stateRunning, now, time.Time{}
		if m.hostKey == "" && c.hostKeyDefault != "" {
			m.hostKey = c.hostKeyDefault
		}
	case "stop":
		m.state, m.stopped = stateStopped, now
	case "delete":
		c.finish(op, "", now)
		c.remove(m, causeServer, now)
		return
	}
	c.finish(op, "", now)
}

func cloudCode(fail string) string {
	if fail == "capacity" {
		return "CLOUD_CAPACITY"
	}
	return "CLOUD_QUOTA"
}

// sweep deletes every machine whose lease has ended, whatever its state,
// and ends the cloud work in progress on it.
func (c *computeState) sweep(now time.Time) {
	var due []*cmachine
	for _, m := range c.machines {
		if !now.Before(m.expires) {
			due = append(due, m)
		}
	}
	sort.Slice(due, func(i, j int) bool { return due[i].shedID < due[j].shedID })
	for _, m := range due {
		if m.pending != nil {
			c.finish(m.pending, "LEASE_ENDED", now)
			m.pending = nil
		}
		c.remove(m, causeLeaseEnded, now)
	}
}

// settleDue settles every operation older than the control file's
// auto_settle_s.
func (c *computeState) settleDue(now time.Time) {
	if c.autoSettle <= 0 {
		return
	}
	var due []*cmachine
	for _, m := range c.machines {
		if m.pending != nil && !m.pending.done {
			due = append(due, m)
		}
	}
	for _, m := range due {
		if !now.Before(m.pending.at.Add(c.autoSettle)) {
			c.settle(m, now)
		}
	}
}

// personLive reports whether a person is live on the list (users) and has
// not opted this server out: who may have a machine, start or extend one.
func (c *computeState) personLive(users []User, handle, server string) (live, admin bool) {
	if c.optouts[[2]string{handle, server}] {
		return false, false
	}
	for _, u := range users {
		if u.Name == handle {
			return true, u.Admin
		}
	}
	return false, false
}

// takeFail takes the one-shot fault set for a call, if its mode is one of
// modes.
func (c *computeState) takeFail(call string, modes ...string) string {
	f, ok := c.fails[call]
	if !ok {
		return ""
	}
	for _, m := range modes {
		if f == m {
			delete(c.fails, call)
			return f
		}
	}
	return ""
}

// keep records the answer to a request id.
func (c *computeState) keep(server, rid, route string, status int, body []byte, now time.Time) {
	c.answers[rkey{server, rid}] = &canswer{route: route, status: status, body: body, at: now}
}

// kept looks up the answer to a request id: found, or a refusal when the id
// was used for another call.
func (c *computeState) kept(server, rid, route string, now time.Time) (*canswer, *computeRefusal) {
	a := c.answers[rkey{server, rid}]
	if a == nil || !now.Before(a.at.Add(computeKeep)) {
		return nil, nil
	}
	if a.route != route {
		return nil, invalid("request_id was used for another call")
	}
	return a, nil
}

// sortedKeys returns a map's keys in order.
func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// validHostKey reports whether a line is one ssh-ed25519 key with nothing
// else.
func validHostKey(line string) bool {
	t, rest, ok := strings.Cut(line, " ")
	if !ok || t != "ssh-ed25519" || strings.ContainsAny(rest, " \t\r\n") {
		return false
	}
	_, err := checkServedKey(line)
	return err == nil
}

// The ingress faults (ingress.break), each a clause of exclusive that no
// longer holds, and unreadable: the service cannot read its rules.
const (
	breakExtraRule        = "extra_rule"
	breakMachineToMachine = "machine_to_machine"
	breakSharedNetwork    = "shared_network"
	breakUnreadable       = "unreadable"
)

// exclusive is whether all of the contract's four clauses hold.
func (c *computeState) exclusive() bool {
	return !c.breaks[breakExtraRule] && !c.breaks[breakMachineToMachine] && !c.breaks[breakSharedNetwork]
}

// HostKeyMismatchLine is logged, with the shed id, when a guest publishes a
// host key that differs from the one kept (no key is in the line).
const HostKeyMismatchLine = "compute: host key mismatch"
