package refserver

import (
	"encoding/json"
	"io"
	"mime"
	"net/http"
	"net/netip"
	"sort"
	"strings"
	"time"
)

// computeRoute is one compute call: its name (as the contract's replies
// and the control file name it), the method, and the id in its path.
type computeRoute struct {
	call   string
	method string
	id     string // the shed id or operation id in the path, if any
}

// matchComputeRoute maps a path under /v1/compute/ to its call; ok is false
// for a path that names none. allow is the method the path takes.
func matchComputeRoute(method, path string) (rt computeRoute, allow string, ok bool) {
	rest := strings.TrimPrefix(path, pathComputePrefix)
	parts := strings.Split(rest, "/")
	pick := func(call, m, id string) (computeRoute, string, bool) {
		return computeRoute{call: call, method: m, id: id}, m, true
	}
	switch {
	case rest == "catalog":
		return pick("catalog", http.MethodGet, "")
	case rest == "usage":
		return pick("usage", http.MethodGet, "")
	case rest == "machines":
		return pick("list", http.MethodGet, "")
	case rest == "ingress":
		switch method {
		case http.MethodGet:
			return pick("ingress_get", http.MethodGet, "")
		case http.MethodPut:
			return pick("ingress_put", http.MethodPut, "")
		}
		return computeRoute{}, "GET, PUT", true
	case len(parts) == 2 && parts[0] == "operations" && parts[1] != "":
		return pick("operation", http.MethodGet, parts[1])
	case len(parts) == 2 && parts[0] == "machines" && parts[1] != "":
		switch method {
		case http.MethodPut:
			return pick("create", http.MethodPut, parts[1])
		case http.MethodGet:
			return pick("get", http.MethodGet, parts[1])
		case http.MethodDelete:
			return pick("delete", http.MethodDelete, parts[1])
		}
		return computeRoute{}, "GET, PUT, DELETE", true
	case len(parts) == 3 && parts[0] == "machines" && parts[1] != "":
		switch parts[2] {
		case "start", "stop", "labels", "extend":
			return pick(parts[2], http.MethodPost, parts[1])
		}
	}
	return computeRoute{}, "", false
}

// computeCall is one request, once it has been authenticated.
type computeCall struct {
	route    computeRoute
	server   string // the calling server's enrolled name
	clientID string
	now      time.Time
	body     []byte
	r        *http.Request
}

// handleCompute answers every route under /v1/compute/. It checks, in
// order: the route and its method, the token and its scope, the rate
// limit, the server's record (gone: 401; disabled: 403), the compute grant,
// the control file's faults, then the call's own rules.
func (s *Server) handleCompute(w http.ResponseWriter, r *http.Request) {
	rt, allow, ok := matchComputeRoute(r.Method, r.URL.Path)
	if !ok {
		writeV1Error(w, http.StatusNotFound, "NOT_FOUND", "no such endpoint", 0)
		return
	}
	if rt.call == "" || r.Method != rt.method {
		w.Header().Set("Allow", allow)
		writeV1Error(w, http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED", "use "+allow, 0)
		return
	}
	now := s.now()
	clientID, scopes, err := s.verifyBearer(r, now)
	if err != nil {
		s.logf("compute refused: %v", err)
		writeInvalidToken(w)
		return
	}
	if !containsString(scopes, scopeCompute) {
		writeV1Error(w, http.StatusForbidden, "INSUFFICIENT_SCOPE", "the token lacks compute:manage", 0)
		return
	}
	if ok, ra := s.allow(limitCompute, clientID); !ok {
		writeV1Error(w, http.StatusTooManyRequests, "RATE_LIMITED", "too many compute requests", ra)
		return
	}
	disabled, err := s.inputs.Disabled(clientID)
	if err != nil {
		s.logf("compute: a lookup failed: %v", err)
		writeUnavailable(w)
		return
	}
	s.mu.Lock()
	srv := s.servers[clientID]
	s.mu.Unlock()
	if srv == nil {
		s.logf("compute refused: no server has the token's client id")
		writeInvalidToken(w)
		return
	}
	if disabled {
		writeV1Error(w, http.StatusForbidden, "SERVER_DISABLED", "the server is disabled", 0)
		return
	}
	if err := s.applyComputeControl(); err != nil {
		s.logf("compute: the control file: %v", err)
		writeUnavailable(w)
		return
	}
	c := s.compute
	c.mu.Lock()
	granted := c.grants[clientID]
	forced, isForced := c.replies[rt.call]
	c.mu.Unlock()
	if !granted {
		writeV1Error(w, http.StatusForbidden, "INSUFFICIENT_SCOPE", "this server may not manage compute", 0)
		return
	}
	call := &computeCall{route: rt, server: srv.name, clientID: clientID, now: s.computeNow(), r: r}
	if isForced {
		s.sendComputeReply(w, r, call, forced)
		return
	}
	if r.Method == http.MethodPut || r.Method == http.MethodPost || r.Method == http.MethodDelete {
		body, err := io.ReadAll(io.LimitReader(r.Body, maxComputeRequest+1))
		if err != nil {
			writeV1Error(w, http.StatusUnprocessableEntity, "INVALID", "the body did not arrive whole", 0)
			return
		}
		if len(body) > maxComputeRequest {
			writeV1Error(w, http.StatusRequestEntityTooLarge, "PAYLOAD_TOO_LARGE", "the request is over 64 KiB", 0)
			return
		}
		if mt, _, err := mime.ParseMediaType(r.Header.Get("Content-Type")); err != nil || mt != "application/json" {
			writeV1Error(w, http.StatusUnprocessableEntity, "INVALID", "the body is not application/json", 0)
			return
		}
		call.body = body
	}
	// A path's id is held to its grammar before anything is looked up.
	switch rt.call {
	case "operation":
		if !operationRx.MatchString(rt.id) {
			writeV1Error(w, http.StatusUnprocessableEntity, "INVALID", "the operation id breaks the grammar", 0)
			return
		}
	case "create", "get", "delete", "start", "stop", "labels", "extend":
		if !computeIDRx.MatchString(rt.id) {
			writeV1Error(w, http.StatusUnprocessableEntity, "INVALID", "the shed id breaks the grammar", 0)
			return
		}
	}

	var users []User
	if needsUsers(rt.call) {
		if users, err = s.inputs.Users(); err != nil {
			s.logf("compute: a lookup failed: %v", err)
			writeUnavailable(w)
			return
		}
	}
	c.mu.Lock()
	c.users = users
	status, reply, lost := s.computeAnswer(call, users)
	c.users = nil
	c.mu.Unlock()
	if r, ok := reply.(*computeRefusal); ok {
		writeV1Error(w, r.status, r.code, r.msg, 0)
		return
	}
	if lost {
		// The work is done and its answer kept; the reply is lost.
		writeV1Error(w, http.StatusServiceUnavailable, "UNAVAILABLE", "the reply was lost", 0)
		return
	}
	if status == http.StatusNoContent {
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(http.StatusNoContent)
		return
	}
	writeBody(w, status, reply.([]byte), true)
}

// needsUsers reports whether a call reads the list of people: every call
// that answers a machine (whether it is extendable depends on its person)
// and usage.
func needsUsers(call string) bool {
	switch call {
	case "catalog", "ingress_get", "ingress_put":
		return false
	}
	return true
}

// computeNow is the compute clock: the server's, moved by the control
// file's clock steps.
func (s *Server) computeNow() time.Time {
	s.compute.mu.Lock()
	defer s.compute.mu.Unlock()
	return s.now().Add(s.compute.offset)
}

// encode marshals an answer.
func encode(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}

// computeAnswer answers a call. It holds c.mu. It returns a status and the
// body (or a *computeRefusal), and whether the reply is to be lost
// (cloud.fail lost_reply: the work is done and kept, and the server hears
// 503).
func (s *Server) computeAnswer(call *computeCall, users []User) (int, any, bool) {
	c := s.compute
	now := call.now
	switch call.route.call {
	case "catalog":
		return 200, encode(c.catalogJSON()), false
	case "list":
		body, r := c.listJSON(call.server, now)
		if r != nil {
			return 0, r, false
		}
		return 200, body, false
	case "get":
		m := c.machines[mkey{call.server, call.route.id}]
		if m == nil {
			return 0, refuse(404, "NOT_FOUND", "no such machine"), false
		}
		return 200, encode(map[string]any{"machine": c.machineJSON(m, now)}), false
	case "operation":
		op := c.ops[call.route.id]
		if op == nil || op.server != call.server || (op.done && !now.Before(op.doneAt.Add(computeKeep))) {
			return 0, refuse(404, "NOT_FOUND", "no such operation, or it has expired"), false
		}
		switch {
		case !op.done:
			return 200, encode(map[string]any{"done": false}), false
		case op.errCode != "":
			return 200, encode(map[string]any{"done": true, "error": map[string]string{"code": op.errCode}}), false
		}
		// The machine the operation was made for, as it is now: null once
		// it is gone, even if the shed id has a new machine since.
		var machine any
		if m := c.machines[mkey{op.server, op.shedID}]; m != nil && m.incarnation == op.incarnation {
			machine = c.machineJSON(m, now)
		}
		return 200, encode(map[string]any{"done": true, "machine": machine}), false
	case "usage":
		return s.usage(call, users)
	case "ingress_get":
		if c.breaks[breakUnreadable] {
			return 0, refuse(503, "UNAVAILABLE", "the service cannot read its ingress rules"), false
		}
		return 200, encode(c.ingressJSON(call.server)), false
	}
	return s.mutate(call, users)
}

// mutate answers a mutation: the request id's kept answer, or the call's
// own rules, keeping a 2xx answer for 24 h.
func (s *Server) mutate(call *computeCall, users []User) (int, any, bool) {
	c := s.compute
	route := call.route.call + " " + call.route.id
	var (
		rid    string
		create createBody
		m      map[string]json.RawMessage
		r      *computeRefusal
	)
	if call.route.call == "create" {
		rid, create, r = parseCreateBody(call.body)
	} else {
		if m, r = decodeComputeBody(call.body); r == nil {
			rid, r = requestID(m)
		}
	}
	if r != nil {
		return 0, r, false
	}
	var refusal *computeRefusal
	// The checks a body fails by itself come before the kept answer.
	switch call.route.call {
	case "labels":
		refusal = checkLabelsBody(m)
	case "extend":
		_, refusal = extendBy(m)
	case "ingress_put":
		_, refusal = ingressSources(m)
	}
	if refusal != nil {
		return 0, refusal, false
	}
	if a, r := c.kept(call.server, rid, route, call.now); r != nil {
		return 0, r, false
	} else if a != nil {
		if a.status == 204 {
			return 204, nil, false
		}
		return a.status, a.body, false
	}
	var (
		status int
		reply  any
	)
	switch call.route.call {
	case "create":
		status, reply = s.create(call, create, users)
	case "start":
		status, reply = s.start(call, users)
	case "stop":
		status, reply = s.stop(call)
	case "delete":
		status, reply = s.deleteMachine(call)
	case "labels":
		status, reply = s.labels(call, m)
	case "extend":
		status, reply = s.extend(call, m, users)
	case "ingress_put":
		status, reply = s.putIngress(call, m)
	}
	if _, refused := reply.(*computeRefusal); refused {
		return status, reply, false
	}
	var body []byte
	if b, ok := reply.([]byte); ok {
		body = b
	}
	c.keep(call.server, rid, route, status, body, call.now)
	lost := c.takeFail(call.route.call, "lost_reply") != ""
	return status, reply, lost
}

func (c *computeState) catalogJSON() map[string]any {
	sizes := []any{}
	for _, z := range c.world.Catalog.Sizes {
		sizes = append(sizes, map[string]any{
			"name": z.Name, "cpus": z.CPUs, "memory_mb": z.MemoryMB, "arch": z.Arch, "disk_gb_default": z.DiskGBDefault,
		})
	}
	p := c.world.Policy
	return map[string]any{
		"sizes":        sizes,
		"default_size": c.world.Catalog.DefaultSize,
		"images":       map[string]any{"default": c.world.Catalog.Images.Default, "aliases": sortedKeys(c.world.Catalog.Images.Aliases)},
		"access_kinds": append([]string{}, c.world.Catalog.AccessKinds...),
		"policy": map[string]any{
			"person": map[string]any{
				"initial_lease_s": p.InitialLeaseS, "max_age_s": p.MaxAgeS,
				"extend_allowed": p.ExtendAllowed, "presets_h": append([]int{}, p.PresetsH...),
			},
			"client": map[string]any{"max_lease_s": p.ClientMaxLeaseS},
			"warn_s": p.WarnS,
		},
		"limits":  map[string]any{"person_max": p.PersonMax, "org_max": p.OrgMax},
		"ingress": map[string]any{"writable": p.IngressWritable},
	}
}

// listJSON is the list, or 503 when it cannot be answered whole: more than
// 500 machines, or a body over 4 MiB.
func (c *computeState) listJSON(server string, now time.Time) ([]byte, *computeRefusal) {
	var ms []*cmachine
	for k, m := range c.machines {
		if k.server == server {
			ms = append(ms, m)
		}
	}
	if len(ms) > maxComputeListMachines {
		return nil, refuse(503, "UNAVAILABLE", "more machines than one list holds")
	}
	sort.Slice(ms, func(i, j int) bool { return ms[i].shedID < ms[j].shedID })
	machines := []any{}
	for _, m := range ms {
		machines = append(machines, c.machineJSON(m, now))
	}
	var ds []cdeletion
	for _, d := range c.deleted {
		if d.server == server && now.Before(d.at.Add(computeKeep)) {
			ds = append(ds, d)
		}
	}
	sort.SliceStable(ds, func(i, j int) bool {
		if !ds[i].at.Equal(ds[j].at) {
			return ds[i].at.Before(ds[j].at)
		}
		return ds[i].shedID < ds[j].shedID
	})
	deleted := []any{}
	for _, d := range ds {
		deleted = append(deleted, map[string]any{"shed_id": d.shedID, "at": fmtTime(d.at), "cause": d.cause})
	}
	body := encode(map[string]any{"machines": machines, "deleted": deleted, "complete": true})
	if len(body) > maxComputeList {
		return nil, refuse(503, "UNAVAILABLE", "the list is larger than a reply holds")
	}
	return body, nil
}

func (c *computeState) ingressJSON(server string) map[string]any {
	in := c.ingress[server]
	sources := []string{}
	var applied time.Time
	if in != nil {
		sources = append(sources, in.sources...)
		applied = in.applied
	}
	return map[string]any{"sources": sources, "exclusive": c.exclusive(), "applied_at": fmtTime(applied)}
}

// usage answers GET /v1/compute/usage[?user=<handle>].
func (s *Server) usage(call *computeCall, users []User) (int, any, bool) {
	c := s.compute
	q := call.r.URL.Query()
	var person any
	if vals, ok := q["user"]; ok {
		if len(vals) != 1 {
			return 0, invalid("user is given more than once"), false
		}
		if live, _ := c.personLive(users, vals[0], call.server); live {
			person = c.personCounts(vals[0]).json(c.world.Policy.PersonMax)
		}
	}
	return 200, encode(map[string]any{"person": person, "org": c.orgCounts().json(c.world.Policy.OrgMax)}), false
}

// create answers PUT /v1/compute/machines/{shed_id}, its body read.
func (s *Server) create(call *computeCall, b createBody, users []User) (int, any) {
	c := s.compute
	now := call.now
	if m := c.machines[mkey{call.server, call.route.id}]; m != nil {
		if !m.body.equal(b) {
			return 409, refuse(409, "SPEC_MISMATCH", "this shed id was created with another body")
		}
		if m.pending != nil && m.pending.kind == "create" {
			return 202, encode(map[string]any{"machine": c.machineJSON(m, now), "operation": m.pending.id})
		}
		return 200, encode(map[string]any{"machine": c.machineJSON(m, now)})
	}
	for k, m := range c.machines {
		if k.server == call.server && m.name == b.name {
			return 409, refuse(409, "EXISTS", "another machine of this server has the name")
		}
	}
	// The judgement, before any cloud work.
	p := c.world.Policy
	admin := false
	if b.user != "" {
		live, a := c.personLive(users, b.user, call.server)
		if !live {
			return 403, refuse(403, "PERSON_UNKNOWN", "no such person, or they opted this server out")
		}
		admin = a
	} else if !c.clients[call.server][b.client] {
		return 403, refuse(403, "CLIENT_UNKNOWN", "the client is not registered for this server")
	}
	size, ok := c.sizeOf(b.size)
	if !ok {
		return 422, refuse(422, "SIZE_UNKNOWN", "no such size")
	}
	if b.user != "" {
		role := "member"
		if admin {
			role = "admin"
		}
		if !containsString(p.Sizes[role], b.size) {
			return 403, refuse(403, "SIZE_NOT_ALLOWED", "the size is not allowed for this person")
		}
	}
	resolved, ok := c.resolveImage(b.image)
	if !ok {
		return 422, refuse(422, "IMAGE_UNKNOWN", "no such image")
	}
	if b.client != "" && b.leaseS > p.ClientMaxLeaseS {
		return 422, refuse(422, "LEASE_TOO_LONG", "the lease is longer than the client's maximum")
	}
	supported := false
	for _, k := range b.accessKinds {
		if containsString(c.world.Catalog.AccessKinds, k) {
			supported = true
		}
	}
	if !supported {
		return 422, refuse(422, "ACCESS_UNSUPPORTED", "no access kind the server named is offered")
	}
	if b.user != "" && c.personCounts(b.user).machines >= p.PersonMax {
		return 409, refuse(409, "QUOTA_PERSON", "the person holds as many machines as they may")
	}
	if c.orgCounts().machines >= p.OrgMax {
		return 409, refuse(409, "QUOTA_ORG", "the organization holds as many machines as it may")
	}
	disk := size.DiskGBDefault
	if b.hasDisk {
		disk = b.diskGB
	}
	m := &cmachine{
		server: call.server, shedID: call.route.id, incarnation: randomHex(8), name: b.name, user: b.user, client: b.client,
		state: stateCreating, size: size, diskGB: disk, imageReq: b.image, imageRes: resolved,
		created: now, labels: map[string]string{}, spec: b.spec, body: b,
	}
	for k, v := range b.labels {
		m.labels[k] = v
	}
	if b.user != "" {
		m.expires = now.Add(time.Duration(p.InitialLeaseS) * time.Second)
		m.maxExpires = now.Add(time.Duration(p.MaxAgeS) * time.Second)
		if m.expires.After(m.maxExpires) {
			m.expires = m.maxExpires
		}
	} else {
		m.expires = now.Add(time.Duration(b.leaseS) * time.Second)
		m.maxExpires = m.expires
	}
	c.machines[mkey{m.server, m.shedID}] = m
	op := c.newOperation(m, "create", c.takeFail("create", "quota", "capacity"), now)
	return 202, encode(map[string]any{"machine": c.machineJSON(m, now), "operation": op.id})
}

// start answers POST …/{shed_id}/start.
func (s *Server) start(call *computeCall, users []User) (int, any) {
	c := s.compute
	now := call.now
	m := c.machines[mkey{call.server, call.route.id}]
	switch {
	case m == nil:
		return 404, refuse(404, "NOT_FOUND", "no such machine")
	case m.state == stateDeleting:
		return 409, refuse(409, "DELETING", "the machine is being deleted")
	case !now.Before(m.expires):
		return 409, refuse(409, "LEASE_ENDED", "the machine's lease has ended")
	}
	if m.user != "" {
		if live, _ := c.personLive(users, m.user, call.server); !live {
			return 403, refuse(403, "PERSON_INACTIVE", "the machine's person is not active")
		}
	}
	if c.takeFail("start", "busy") != "" {
		return 409, refuse(409, "BUSY", "the cloud is busy with this machine")
	}
	switch m.state {
	case stateRunning:
		return 200, encode(map[string]any{"machine": c.machineJSON(m, now)})
	case stateStarting:
		return 202, encode(map[string]any{"operation": m.pending.id})
	case stateCreating, stateStopping:
		return 409, refuse(409, "BUSY", "the machine is in another transition")
	}
	m.state = stateStarting
	op := c.newOperation(m, "start", c.takeFail("start", "quota", "capacity"), now)
	return 202, encode(map[string]any{"operation": op.id})
}

// stop answers POST …/{shed_id}/stop.
func (s *Server) stop(call *computeCall) (int, any) {
	c := s.compute
	now := call.now
	m := c.machines[mkey{call.server, call.route.id}]
	switch {
	case m == nil:
		return 404, refuse(404, "NOT_FOUND", "no such machine")
	case m.state == stateDeleting:
		return 409, refuse(409, "DELETING", "the machine is being deleted")
	}
	if c.takeFail("stop", "busy") != "" {
		return 409, refuse(409, "BUSY", "the cloud is busy with this machine")
	}
	switch m.state {
	case stateStopped:
		return 200, encode(map[string]any{"machine": c.machineJSON(m, now)})
	case stateStopping:
		return 202, encode(map[string]any{"operation": m.pending.id})
	case stateCreating, stateStarting:
		return 409, refuse(409, "BUSY", "the machine is in another transition")
	}
	m.state = stateStopping
	op := c.newOperation(m, "stop", "", now)
	return 202, encode(map[string]any{"operation": op.id})
}

// deleteMachine answers DELETE …/{shed_id}.
func (s *Server) deleteMachine(call *computeCall) (int, any) {
	c := s.compute
	now := call.now
	m := c.machines[mkey{call.server, call.route.id}]
	if m == nil {
		return 204, nil
	}
	if m.state == stateDeleting {
		return 202, encode(map[string]any{"operation": m.pending.id})
	}
	if m.pending != nil {
		c.finish(m.pending, "DELETING", now)
	}
	m.state = stateDeleting
	op := c.newOperation(m, "delete", "", now)
	return 202, encode(map[string]any{"operation": op.id})
}

// checkLabelsBody checks a labels write's body by itself: set an object,
// expect an object when present, both under the grammar and naming no
// reserved label.
func checkLabelsBody(m map[string]json.RawMessage) *computeRefusal {
	raw, ok := m["set"]
	if !ok {
		return invalid("set is missing")
	}
	if _, r := labelSet(raw, true); r != nil {
		return r
	}
	if raw, ok := m["expect"]; ok {
		if _, r := labelSet(raw, true); r != nil {
			return r
		}
	}
	return nil
}

// labels answers POST …/{shed_id}/labels: one atomic write.
func (s *Server) labels(call *computeCall, body map[string]json.RawMessage) (int, any) {
	c := s.compute
	m := c.machines[mkey{call.server, call.route.id}]
	switch {
	case m == nil:
		return 404, refuse(404, "NOT_FOUND", "no such machine")
	case m.state == stateDeleting:
		return 409, refuse(409, "DELETING", "the machine is being deleted")
	}
	set, _ := labelSet(body["set"], true)
	if raw, ok := body["expect"]; ok {
		expect, _ := labelSet(raw, true)
		for k, want := range expect {
			got, has := m.labels[k]
			if (want == nil && has) || (want != nil && (!has || got != *want)) {
				return 409, refuse(409, "LABEL_TAKEN", "a label does not hold what the write expects")
			}
		}
	}
	next := map[string]string{}
	for k, v := range m.labels {
		next[k] = v
	}
	for k, v := range set {
		if v == nil {
			delete(next, k)
		} else {
			next[k] = *v
		}
	}
	if len(next) > maxLabels {
		return 422, invalid("the write would leave more than 32 labels")
	}
	m.labels = next
	return 200, encode(map[string]any{"machine": c.machineJSON(m, call.now)})
}

// extendBy reads an extend's by_s: whole hours from 1 to 720.
func extendBy(m map[string]json.RawMessage) (int64, *computeRefusal) {
	n, ok := jsonInt(m["by_s"])
	if !ok || n < minExtend || n > maxExtend || n%3600 != 0 {
		return 0, invalid("by_s is not whole hours from 3600 to 2592000")
	}
	return n, nil
}

// extend answers POST …/{shed_id}/extend.
func (s *Server) extend(call *computeCall, body map[string]json.RawMessage, users []User) (int, any) {
	c := s.compute
	now := call.now
	by, _ := extendBy(body)
	m := c.machines[mkey{call.server, call.route.id}]
	switch {
	case m == nil:
		return 404, refuse(404, "NOT_FOUND", "no such machine")
	case m.state == stateDeleting:
		return 409, refuse(409, "DELETING", "the machine is being deleted")
	case !now.Before(m.expires):
		return 409, refuse(409, "LEASE_ENDED", "the machine's lease has ended")
	case m.client != "" || !c.world.Policy.ExtendAllowed:
		return 403, refuse(403, "EXTEND_NOT_ALLOWED", "this machine's lease cannot be extended")
	}
	if live, _ := c.personLive(users, m.user, call.server); !live {
		return 403, refuse(403, "PERSON_INACTIVE", "the machine's person is not active")
	}
	next := m.expires.Add(time.Duration(by) * time.Second)
	if next.After(m.maxExpires) {
		return 409, refuse(409, "LEASE_AT_CAP", "the extend would pass the machine's cap")
	}
	m.expires = next
	return 200, encode(map[string]any{"machine": c.machineJSON(m, now)})
}

// ingressSources reads an ingress write's sources: 0 to 64 canonical
// prefixes, none twice.
func ingressSources(m map[string]json.RawMessage) ([]netip.Prefix, *computeRefusal) {
	list, ok := stringList(m["sources"])
	if !ok || len(list) > maxIngressSources {
		return nil, invalid("sources is not 0 to 64 prefixes")
	}
	seen := map[string]bool{}
	var out []netip.Prefix
	for _, s := range list {
		p, ok := canonicalCIDR(s)
		if !ok || seen[s] {
			return nil, invalid("a source is not a canonical prefix, or is named twice")
		}
		seen[s] = true
		out = append(out, p)
	}
	return out, nil
}

// putIngress answers PUT /v1/compute/ingress.
func (s *Server) putIngress(call *computeCall, body map[string]json.RawMessage) (int, any) {
	c := s.compute
	p := c.world.Policy
	if !p.IngressWritable {
		return 403, refuse(403, "INGRESS_NOT_WRITABLE", "this service's ingress is managed elsewhere")
	}
	if c.breaks[breakUnreadable] {
		return 503, refuse(503, "UNAVAILABLE", "the service cannot read its ingress rules")
	}
	prefixes, _ := ingressSources(body)
	sources := []string{}
	for _, pf := range prefixes {
		min := p.IngressMinPrefix.V6
		if pf.Addr().Is4() {
			min = p.IngressMinPrefix.V4
		}
		if pf.Bits() < min {
			return 422, refuse(422, "INGRESS_TOO_WIDE", "a source is wider than the service allows")
		}
		sources = append(sources, pf.String())
	}
	c.ingress[call.server] = &cingress{sources: sources, applied: call.now}
	return 200, encode(c.ingressJSON(call.server))
}
