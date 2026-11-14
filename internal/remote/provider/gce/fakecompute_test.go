package gce

import (
	"crypto"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeCompute is an httptest server that speaks enough of the Compute v1 API,
// and the OAuth token endpoint, for the client's tests. Operations finish on
// the first wait unless opDelay says otherwise.
type fakeCompute struct {
	t       *testing.T
	srv     *httptest.Server
	project string
	zone    string

	// pubKey verifies the token endpoint's JWT assertions; nil skips it.
	pubKey *rsa.PublicKey

	mu        sync.Mutex
	instances map[string]*fInst
	ops       map[string]*fOp
	requests  map[string]string // requestId -> op name
	images    map[string]fImage // "project/name"
	disks     map[string]*fDisk
	firewalls []map[string]any
	networks  map[string]map[string]any
	subnets   map[string]map[string]any

	nextOp   int
	nextAddr int
	nextFP   int

	// pageSize caps every list page.
	pageSize int
	// opDelay is how many wait calls an operation answers RUNNING first.
	opDelay int
	// doneAtOnce finishes every operation before the reply that starts it.
	doneAtOnce bool
	// failCalls are injected replies, used first-come.
	failCalls []fFail
	// failOps: the next operation of a type finishes with these codes.
	failOps map[string][]fOpErr

	// tokens: issued access tokens, and the ones the API now rejects.
	tokenTTL  int
	tokens    int
	revoked   map[string]bool
	tokenHits int

	log []fRequest
}

type fRequest struct {
	Method string
	Path   string
	Query  url.Values
	Body   string
	Token  string
}

type fFail struct {
	method string
	match  *regexp.Regexp
	query  *regexp.Regexp // optional, against the raw query
	status int
	reason string
	msg    string
	times  int
}

type fOpErr struct {
	code   string
	msg    string
	status int
}

type fInst struct {
	name, status, machineType string
	labels                    map[string]string
	labelFP                   string
	metadata                  map[string]string
	metaFP                    string
	natIP, internalIP         string
	diskGB                    int
	scheduling                map[string]any
	created, started, stopped time.Time
	guestAttrs                map[string]string
	tags                      []string
}

type fOp struct {
	name, opType, target string
	status               string
	remaining            int
	apply                func() // effect on success, run under mu
	rollback             func() // effect on failure, run under mu
	errs                 []fOpErr
}

type fImage struct {
	name, family, status, deprecated string
}

type fDisk struct {
	name   string
	labels map[string]string
	users  []string
	sizeGB int
}

func newFakeCompute(t *testing.T) *fakeCompute {
	f := &fakeCompute{
		t:         t,
		project:   "test-proj",
		zone:      "us-central1-a",
		instances: map[string]*fInst{},
		ops:       map[string]*fOp{},
		requests:  map[string]string{},
		images:    map[string]fImage{},
		disks:     map[string]*fDisk{},
		networks:  map[string]map[string]any{},
		subnets:   map[string]map[string]any{},
		failOps:   map[string][]fOpErr{},
		revoked:   map[string]bool{},
		pageSize:  500,
		tokenTTL:  3600,
	}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeCompute) baseURL() string  { return f.srv.URL + "/compute/v1/" }
func (f *fakeCompute) tokenURL() string { return f.srv.URL + "/token" }

func (f *fakeCompute) selfLink(path string) string {
	return "https://www.googleapis.com/compute/v1/" + path
}

func (f *fakeCompute) fingerprint() string {
	f.nextFP++
	return base64.StdEncoding.EncodeToString([]byte(fmt.Sprintf("fp-%d", f.nextFP)))
}

// failNext makes the next n calls matching method and path pattern fail.
func (f *fakeCompute) failNext(method, pattern string, status int, reason, msg string, n int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failCalls = append(f.failCalls, fFail{method: method, match: regexp.MustCompile(pattern), status: status, reason: reason, msg: msg, times: n})
}

func (f *fakeCompute) failNextOp(opType, code, msg string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failOps[opType] = append(f.failOps[opType], fOpErr{code: code, msg: msg, status: 503})
}

func (f *fakeCompute) requestsFor(method, pattern string) []fRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	re := regexp.MustCompile(pattern)
	var out []fRequest
	for _, r := range f.log {
		if r.Method == method && re.MatchString(r.Path) {
			out = append(out, r)
		}
	}
	return out
}

func (f *fakeCompute) seed(in *fInst) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if in.status == "" {
		in.status = "RUNNING"
	}
	if in.labels == nil {
		in.labels = map[string]string{}
	}
	if in.metadata == nil {
		in.metadata = map[string]string{}
	}
	if in.machineType == "" {
		in.machineType = "e2-standard-2"
	}
	if in.diskGB == 0 {
		in.diskGB = 20
	}
	if in.created.IsZero() {
		in.created = time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	}
	if in.status == "RUNNING" && in.natIP == "" {
		in.natIP = f.newAddrLocked()
	}
	in.labelFP = f.fingerprint()
	in.metaFP = f.fingerprint()
	f.instances[in.name] = in
}

func (f *fakeCompute) instance(name string) *fInst {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.instances[name]
}

func (f *fakeCompute) newAddrLocked() string {
	f.nextAddr++
	return fmt.Sprintf("198.51.100.%d", f.nextAddr)
}

func (f *fakeCompute) writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func (f *fakeCompute) apiErr(w http.ResponseWriter, status int, reason, msg string) {
	f.writeJSON(w, status, map[string]any{"error": map[string]any{
		"code":    status,
		"message": msg,
		"errors":  []any{map[string]any{"reason": reason, "message": msg, "domain": "global"}},
	}})
}

var (
	reInstances  = regexp.MustCompile(`^projects/([^/]+)/zones/([^/]+)/instances$`)
	reInstance   = regexp.MustCompile(`^projects/([^/]+)/zones/([^/]+)/instances/([^/]+)$`)
	reInstAction = regexp.MustCompile(`^projects/([^/]+)/zones/([^/]+)/instances/([^/]+)/([A-Za-z]+)$`)
	reOpWait     = regexp.MustCompile(`^projects/([^/]+)/zones/([^/]+)/operations/([^/]+)/wait$`)
	reImage      = regexp.MustCompile(`^projects/([^/]+)/global/images/(family/)?([^/]+)$`)
	reDisks      = regexp.MustCompile(`^projects/([^/]+)/zones/([^/]+)/disks$`)
	reDisk       = regexp.MustCompile(`^projects/([^/]+)/zones/([^/]+)/disks/([^/]+)$`)
	reNetwork    = regexp.MustCompile(`^projects/([^/]+)/global/networks/([^/]+)$`)
	reSubnet     = regexp.MustCompile(`^projects/([^/]+)/regions/([^/]+)/subnetworks/([^/]+)$`)
	reFirewalls  = regexp.MustCompile(`^projects/([^/]+)/global/firewalls$`)
	reLabelEq    = regexp.MustCompile(`^labels\.([a-z0-9_-]+) = "([^"]*)"$`)
)

func (f *fakeCompute) serve(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	if r.URL.Path == "/token" {
		f.serveToken(w, r, body)
		return
	}
	path, ok := strings.CutPrefix(r.URL.Path, "/compute/v1/")
	if !ok {
		http.NotFound(w, r)
		return
	}
	tok := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")

	f.mu.Lock()
	defer f.mu.Unlock()
	f.log = append(f.log, fRequest{Method: r.Method, Path: path, Query: r.URL.Query(), Body: string(body), Token: tok})

	if tok == "" || f.revoked[tok] {
		f.apiErr(w, 401, "authError", "Request had invalid authentication credentials.")
		return
	}
	for i := range f.failCalls {
		fc := &f.failCalls[i]
		if fc.times > 0 && fc.method == r.Method && fc.match.MatchString(path) && (fc.query == nil || fc.query.MatchString(r.URL.RawQuery)) {
			fc.times--
			f.apiErr(w, fc.status, fc.reason, fc.msg)
			return
		}
	}
	// A repeated requestId gets the first operation back.
	if rid := r.URL.Query().Get("requestId"); rid != "" && r.Method != http.MethodGet {
		if name, ok := f.requests[rid]; ok {
			f.writeJSON(w, 200, f.opJSON(f.ops[name]))
			return
		}
	}

	switch {
	case reOpWait.MatchString(path):
		m := reOpWait.FindStringSubmatch(path)
		op, ok := f.ops[m[3]]
		if !ok {
			f.apiErr(w, 404, "notFound", "The resource 'operation' was not found")
			return
		}
		if op.status != "DONE" {
			if op.remaining > 0 {
				op.remaining--
			} else {
				f.finishLocked(op)
			}
		}
		f.writeJSON(w, 200, f.opJSON(op))
	case reInstances.MatchString(path):
		if r.Method == http.MethodPost {
			f.insert(w, r, body)
		} else {
			f.listInstances(w, r)
		}
	case reInstance.MatchString(path):
		name := reInstance.FindStringSubmatch(path)[3]
		in, ok := f.instances[name]
		if !ok {
			f.apiErr(w, 404, "notFound", fmt.Sprintf("The resource 'projects/%s/zones/%s/instances/%s' was not found", f.project, f.zone, name))
			return
		}
		switch r.Method {
		case http.MethodGet:
			f.writeJSON(w, 200, f.instJSON(in))
		case http.MethodDelete:
			in.status = "STOPPING"
			in.natIP = ""
			f.startOp(w, r, "delete", name, func() { delete(f.instances, name) }, nil)
		}
	case reInstAction.MatchString(path):
		m := reInstAction.FindStringSubmatch(path)
		f.action(w, r, m[3], m[4], body)
	case reImage.MatchString(path):
		m := reImage.FindStringSubmatch(path)
		f.getImage(w, m[1], m[2] != "", m[3])
	case reDisks.MatchString(path):
		f.listDisks(w, r)
	case reDisk.MatchString(path):
		name := reDisk.FindStringSubmatch(path)[3]
		d, ok := f.disks[name]
		if !ok {
			f.apiErr(w, 404, "notFound", "The resource 'disk' was not found")
			return
		}
		if r.Method == http.MethodDelete {
			f.startOp(w, r, "delete", name, func() { delete(f.disks, name) }, nil)
			return
		}
		f.writeJSON(w, 200, f.diskJSON(d))
	case reNetwork.MatchString(path):
		n, ok := f.networks[reNetwork.FindStringSubmatch(path)[2]]
		if !ok {
			f.apiErr(w, 404, "notFound", "The resource 'network' was not found")
			return
		}
		f.writeJSON(w, 200, n)
	case reSubnet.MatchString(path):
		s, ok := f.subnets[reSubnet.FindStringSubmatch(path)[3]]
		if !ok {
			f.apiErr(w, 404, "notFound", "The resource 'subnetwork' was not found")
			return
		}
		f.writeJSON(w, 200, s)
	case reFirewalls.MatchString(path):
		items := make([]any, 0, len(f.firewalls))
		for _, fw := range f.firewalls {
			items = append(items, fw)
		}
		f.page(w, r, items)
	default:
		f.apiErr(w, 404, "notFound", "no such route "+path)
	}
}

// page answers a list with one page of items.
func (f *fakeCompute) page(w http.ResponseWriter, r *http.Request, items []any) {
	start := 0
	if tok := r.URL.Query().Get("pageToken"); tok != "" {
		n, err := strconv.Atoi(strings.TrimPrefix(tok, "page-"))
		if err != nil {
			f.apiErr(w, 400, "invalid", "bad page token")
			return
		}
		start = n
	}
	size := f.pageSize
	if mr, err := strconv.Atoi(r.URL.Query().Get("maxResults")); err == nil && mr < size {
		size = mr
	}
	end := min(start+size, len(items))
	if start > len(items) {
		start = len(items)
	}
	reply := map[string]any{"kind": "compute#list", "items": items[start:end]}
	if end < len(items) {
		reply["nextPageToken"] = fmt.Sprintf("page-%d", end)
	}
	f.writeJSON(w, 200, reply)
}

func (f *fakeCompute) matchFilter(w http.ResponseWriter, r *http.Request) (func(map[string]string) bool, bool) {
	filter := r.URL.Query().Get("filter")
	if filter == "" {
		return func(map[string]string) bool { return true }, true
	}
	m := reLabelEq.FindStringSubmatch(filter)
	if m == nil {
		f.apiErr(w, 400, "invalid", "Invalid value for field 'filter': '"+filter+"'. Invalid list filter expression.")
		return nil, false
	}
	return func(labels map[string]string) bool {
		v, ok := labels[m[1]]
		return ok && v == m[2]
	}, true
}

func (f *fakeCompute) listInstances(w http.ResponseWriter, r *http.Request) {
	match, ok := f.matchFilter(w, r)
	if !ok {
		return
	}
	names := make([]string, 0, len(f.instances))
	for n := range f.instances {
		names = append(names, n)
	}
	sort.Strings(names)
	var items []any
	for _, n := range names {
		if in := f.instances[n]; match(in.labels) {
			items = append(items, f.instJSON(in))
		}
	}
	f.page(w, r, items)
}

func (f *fakeCompute) listDisks(w http.ResponseWriter, r *http.Request) {
	match, ok := f.matchFilter(w, r)
	if !ok {
		return
	}
	names := make([]string, 0, len(f.disks))
	for n := range f.disks {
		names = append(names, n)
	}
	sort.Strings(names)
	var items []any
	for _, n := range names {
		if d := f.disks[n]; match(d.labels) {
			items = append(items, f.diskJSON(d))
		}
	}
	f.page(w, r, items)
}

func (f *fakeCompute) diskJSON(d *fDisk) map[string]any {
	users := make([]string, 0, len(d.users))
	for _, u := range d.users {
		users = append(users, f.selfLink("projects/"+f.project+"/zones/"+f.zone+"/instances/"+u))
	}
	return map[string]any{
		"name":              d.name,
		"zone":              f.selfLink("projects/" + f.project + "/zones/" + f.zone),
		"labels":            d.labels,
		"sizeGb":            strconv.Itoa(d.sizeGB),
		"status":            "READY",
		"users":             users,
		"creationTimestamp": "2026-11-01T10:00:00.000-07:00",
	}
}

func (f *fakeCompute) getImage(w http.ResponseWriter, project string, family bool, name string) {
	if family {
		var best *fImage
		for k, img := range f.images {
			if strings.HasPrefix(k, project+"/") && img.family == name && img.deprecated == "" {
				if best == nil || img.name > best.name {
					img := img
					best = &img
				}
			}
		}
		if best == nil {
			f.apiErr(w, 404, "notFound", "The resource 'projects/"+project+"/global/images/family/"+name+"' was not found")
			return
		}
		f.writeJSON(w, 200, f.imageJSON(project, *best))
		return
	}
	img, ok := f.images[project+"/"+name]
	if !ok {
		f.apiErr(w, 404, "notFound", "The resource 'projects/"+project+"/global/images/"+name+"' was not found")
		return
	}
	f.writeJSON(w, 200, f.imageJSON(project, img))
}

func (f *fakeCompute) imageJSON(project string, img fImage) map[string]any {
	out := map[string]any{
		"name":         img.name,
		"selfLink":     f.selfLink("projects/" + project + "/global/images/" + img.name),
		"family":       img.family,
		"status":       img.status,
		"architecture": "X86_64",
		"diskSizeGb":   "10",
	}
	if img.deprecated != "" {
		out["deprecated"] = map[string]any{"state": img.deprecated}
	}
	return out
}

func (f *fakeCompute) instJSON(in *fInst) map[string]any {
	items := make([]any, 0, len(in.metadata))
	keys := make([]string, 0, len(in.metadata))
	for k := range in.metadata {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		items = append(items, map[string]any{"key": k, "value": in.metadata[k]})
	}
	ac := map[string]any{"type": "ONE_TO_ONE_NAT", "name": "External NAT", "networkTier": "PREMIUM"}
	if in.natIP != "" {
		ac["natIP"] = in.natIP
	}
	ts := func(t time.Time) string {
		if t.IsZero() {
			return ""
		}
		return t.In(time.FixedZone("PDT", -7*3600)).Format("2006-01-02T15:04:05.000-07:00")
	}
	out := map[string]any{
		"kind":              "compute#instance",
		"id":                "1234567890",
		"name":              in.name,
		"status":            in.status,
		"zone":              f.selfLink("projects/" + f.project + "/zones/" + f.zone),
		"machineType":       f.selfLink("projects/" + f.project + "/zones/" + f.zone + "/machineTypes/" + in.machineType),
		"creationTimestamp": ts(in.created),
		"labels":            in.labels,
		"labelFingerprint":  in.labelFP,
		"metadata":          map[string]any{"kind": "compute#metadata", "fingerprint": in.metaFP, "items": items},
		"networkInterfaces": []any{map[string]any{"networkIP": in.internalIP, "accessConfigs": []any{ac}}},
		"disks": []any{map[string]any{
			"boot": true, "diskSizeGb": strconv.Itoa(in.diskGB), "architecture": "X86_64",
			"source": f.selfLink("projects/" + f.project + "/zones/" + f.zone + "/disks/" + in.name),
		}},
		"scheduling": in.scheduling,
		"tags":       map[string]any{"items": in.tags},
	}
	if s := ts(in.started); s != "" {
		out["lastStartTimestamp"] = s
	}
	if s := ts(in.stopped); s != "" {
		out["lastStopTimestamp"] = s
	}
	if in.status == "RUNNING" {
		if mrd, ok := in.scheduling["maxRunDuration"].(map[string]any); ok {
			secs, _ := strconv.Atoi(fmt.Sprint(mrd["seconds"]))
			out["resourceStatus"] = map[string]any{"scheduling": map[string]any{
				"terminationTimestamp": ts(in.started.Add(time.Duration(secs) * time.Second)),
			}}
		}
	}
	return out
}

func (f *fakeCompute) opJSON(op *fOp) map[string]any {
	out := map[string]any{
		"kind":          "compute#operation",
		"name":          op.name,
		"zone":          f.selfLink("projects/" + f.project + "/zones/" + f.zone),
		"operationType": op.opType,
		"status":        op.status,
		"targetLink":    op.target,
	}
	if op.status == "DONE" && len(op.errs) > 0 {
		var errs []any
		for _, e := range op.errs {
			errs = append(errs, map[string]any{"code": e.code, "message": e.msg})
		}
		out["error"] = map[string]any{"errors": errs}
		out["httpErrorStatusCode"] = op.errs[0].status
		out["httpErrorMessage"] = "SERVICE UNAVAILABLE"
	}
	return out
}

// startOp registers an operation; with opDelay 0 it is still RUNNING in the
// reply and finishes on the first wait.
func (f *fakeCompute) startOp(w http.ResponseWriter, r *http.Request, opType, target string, apply, rollback func()) {
	f.nextOp++
	op := &fOp{
		name:      fmt.Sprintf("operation-%d", f.nextOp),
		opType:    opType,
		target:    target,
		status:    "RUNNING",
		remaining: f.opDelay,
		apply:     apply,
		rollback:  rollback,
	}
	if q := f.failOps[opType]; len(q) > 0 {
		op.errs = []fOpErr{q[0]}
		f.failOps[opType] = q[1:]
	}
	f.ops[op.name] = op
	if rid := r.URL.Query().Get("requestId"); rid != "" {
		f.requests[rid] = op.name
	}
	if f.doneAtOnce {
		f.finishLocked(op)
	}
	f.writeJSON(w, 200, f.opJSON(op))
}

// doneOp registers an operation that finished at once.
func (f *fakeCompute) doneOp(w http.ResponseWriter, r *http.Request, opType, target string, apply func()) {
	f.startOp(w, r, opType, target, apply, nil)
	f.finishLocked(f.ops[fmt.Sprintf("operation-%d", f.nextOp)])
}

func (f *fakeCompute) finishLocked(op *fOp) {
	if op.status == "DONE" {
		return
	}
	op.status = "DONE"
	if len(op.errs) > 0 {
		if op.rollback != nil {
			op.rollback()
		}
		return
	}
	if op.apply != nil {
		op.apply()
	}
}

func (f *fakeCompute) insert(w http.ResponseWriter, r *http.Request, body []byte) {
	var req struct {
		Name        string            `json:"name"`
		MachineType string            `json:"machineType"`
		Labels      map[string]string `json:"labels"`
		Tags        struct {
			Items []string `json:"items"`
		} `json:"tags"`
		Metadata struct {
			Items []struct {
				Key   string `json:"key"`
				Value string `json:"value"`
			} `json:"items"`
		} `json:"metadata"`
		Disks []struct {
			InitializeParams struct {
				SourceImage string `json:"sourceImage"`
				DiskSizeGb  string `json:"diskSizeGb"`
			} `json:"initializeParams"`
		} `json:"disks"`
		Scheduling map[string]any `json:"scheduling"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		f.apiErr(w, 400, "invalid", "bad body: "+err.Error())
		return
	}
	if _, ok := f.instances[req.Name]; ok {
		f.apiErr(w, 409, "alreadyExists", fmt.Sprintf("The resource 'projects/%s/zones/%s/instances/%s' already exists", f.project, f.zone, req.Name))
		return
	}
	md := map[string]string{}
	for _, it := range req.Metadata.Items {
		md[it.Key] = it.Value
	}
	gb := 10
	if len(req.Disks) > 0 {
		gb, _ = strconv.Atoi(req.Disks[0].InitializeParams.DiskSizeGb)
	}
	in := &fInst{
		name:        req.Name,
		status:      "PROVISIONING",
		machineType: lastSegment(req.MachineType),
		labels:      req.Labels,
		labelFP:     f.fingerprint(),
		metadata:    md,
		metaFP:      f.fingerprint(),
		internalIP:  "10.10.0.2",
		diskGB:      gb,
		scheduling:  req.Scheduling,
		created:     time.Date(2026, 11, 13, 9, 0, 0, 0, time.UTC),
		tags:        req.Tags.Items,
	}
	f.instances[req.Name] = in
	name := req.Name
	f.startOp(w, r, "insert", name, func() {
		in.status = "RUNNING"
		in.natIP = f.newAddrLocked()
		in.started = time.Date(2026, 11, 13, 9, 0, 30, 0, time.UTC)
	}, func() { delete(f.instances, name) })
}

func (f *fakeCompute) action(w http.ResponseWriter, r *http.Request, name, verb string, body []byte) {
	in, ok := f.instances[name]
	if !ok {
		f.apiErr(w, 404, "notFound", fmt.Sprintf("The resource 'projects/%s/zones/%s/instances/%s' was not found", f.project, f.zone, name))
		return
	}
	switch verb {
	case "start":
		if in.status == "RUNNING" {
			f.doneOp(w, r, "start", name, nil)
			return
		}
		prev := in.status
		in.status = "STAGING"
		f.startOp(w, r, "start", name, func() {
			in.status = "RUNNING"
			in.natIP = f.newAddrLocked()
			in.started = in.started.Add(time.Hour)
		}, func() { in.status = prev })
	case "stop":
		in.status = "STOPPING"
		f.startOp(w, r, "stop", name, func() {
			in.status = "TERMINATED"
			in.natIP = ""
			in.stopped = time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC)
		}, func() { in.status = "RUNNING" })
	case "setLabels":
		var req struct {
			Labels           map[string]string `json:"labels"`
			LabelFingerprint string            `json:"labelFingerprint"`
		}
		_ = json.Unmarshal(body, &req)
		if req.LabelFingerprint != in.labelFP {
			f.apiErr(w, 412, "conditionNotMet", "Labels fingerprint either invalid or resource labels have changed")
			return
		}
		f.doneOp(w, r, "setLabels", name, func() {
			in.labels = maps.Clone(req.Labels)
			in.labelFP = f.fingerprint()
		})
	case "setMetadata":
		var req struct {
			Fingerprint string `json:"fingerprint"`
			Items       []struct {
				Key   string `json:"key"`
				Value string `json:"value"`
			} `json:"items"`
		}
		_ = json.Unmarshal(body, &req)
		if req.Fingerprint != in.metaFP {
			f.apiErr(w, 412, "conditionNotMet", "Supplied fingerprint does not match current metadata fingerprint.")
			return
		}
		f.doneOp(w, r, "setMetadata", name, func() {
			in.metadata = map[string]string{}
			for _, it := range req.Items {
				in.metadata[it.Key] = it.Value
			}
			in.metaFP = f.fingerprint()
		})
	case "setScheduling":
		if in.status != "TERMINATED" {
			f.apiErr(w, 400, "badRequest", "Instance must be stopped to change maxRunDuration.")
			return
		}
		var sched map[string]any
		_ = json.Unmarshal(body, &sched)
		f.doneOp(w, r, "setScheduling", name, func() { in.scheduling = sched })
	case "getGuestAttributes":
		key := r.URL.Query().Get("variableKey")
		v, ok := in.guestAttrs[key]
		if !ok {
			f.apiErr(w, 404, "notFound", "The resource 'guest attribute "+key+"' was not found")
			return
		}
		f.writeJSON(w, 200, map[string]any{"kind": "compute#guestAttributes", "variableKey": key, "variableValue": v})
	default:
		f.apiErr(w, 400, "badRequest", "unknown action "+verb)
	}
}

func (f *fakeCompute) serveToken(w http.ResponseWriter, r *http.Request, body []byte) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.tokenHits++
	form, err := parseForm(string(body))
	if err != nil || form["grant_type"] != "urn:ietf:params:oauth:grant-type:jwt-bearer" {
		f.writeJSON(w, 400, map[string]any{"error": "unsupported_grant_type"})
		return
	}
	parts := strings.Split(form["assertion"], ".")
	if len(parts) != 3 {
		f.writeJSON(w, 400, map[string]any{"error": "invalid_grant", "error_description": "bad assertion"})
		return
	}
	if f.pubKey != nil {
		sig, _ := base64.RawURLEncoding.DecodeString(parts[2])
		sum := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
		if rsa.VerifyPKCS1v15(f.pubKey, crypto.SHA256, sum[:], sig) != nil {
			f.writeJSON(w, 400, map[string]any{"error": "invalid_grant", "error_description": "Invalid JWT Signature."})
			return
		}
	}
	claims, _ := base64.RawURLEncoding.DecodeString(parts[1])
	var c map[string]any
	if json.Unmarshal(claims, &c) != nil || c["scope"] != Scope || c["aud"] != f.tokenURL() {
		f.writeJSON(w, 400, map[string]any{"error": "invalid_grant", "error_description": "bad claims"})
		return
	}
	f.tokens++
	f.writeJSON(w, 200, map[string]any{"access_token": fmt.Sprintf("tok-%d", f.tokens), "expires_in": f.tokenTTL, "token_type": "Bearer"})
}

func parseForm(s string) (map[string]string, error) {
	v, err := url.ParseQuery(s)
	if err != nil {
		return nil, err
	}
	out := map[string]string{}
	for k := range v {
		out[k] = v.Get(k)
	}
	return out, nil
}
