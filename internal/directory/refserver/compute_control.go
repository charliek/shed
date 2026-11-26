package refserver

import (
	"bytes"
	"encoding/base32"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"
)

// The compute service's set-up steps (the contract's named additions to
// the directory exchange language) and its control file. Each step is one
// JSON object with one member, the step's name. The tests apply them as
// hooks; the command reads them from compute.json in the control
// directory, each applied once (by its id) when the file is next read.

// ComputeReply is a reply the service sends to every request of one call
// while the control file names it, whatever the request: a status with
// headers and a body (JSON) or text, a generated body, or silence.
type ComputeReply struct {
	Status   int               `json:"status,omitempty"`
	Headers  map[string]string `json:"headers,omitempty"`
	Body     json.RawMessage   `json:"body,omitempty"`
	Text     *string           `json:"text,omitempty"`
	Generate string            `json:"generate,omitempty"`
	Silent   bool              `json:"silent,omitempty"`
}

// The generated reply bodies.
const (
	// GenOversize is a 200 holding a valid machine and a pad, 64 KiB and
	// one byte in all.
	GenOversize = "oversize"
	// GenOversizeList is a machine list of 4 MiB and one byte.
	GenOversizeList = "oversize_list"
	// GenList501 is a complete list of 501 machines.
	GenList501 = "list_501"
	// GenTruncated declares a valid machine reply's length, sends half of
	// it and closes the connection.
	GenTruncated = "truncated"
	// GenEchoBearer is a valid machine reply whose spec is the bearer
	// token presented.
	GenEchoBearer = "echo_bearer"
	// GenOversizeError is a 409 error body of more than 4 KiB naming BUSY.
	GenOversizeError = "oversize_error"
	// GenEchoBearerEscaped is GenEchoBearer with the bearer's first
	// character written as a \u escape, so the bearer is in the decoded
	// spec but not in the body's bytes.
	GenEchoBearerEscaped = "echo_bearer_escaped"
	// GenOversizeErrorEchoBearer is a 409 naming BUSY whose message holds
	// the bearer presented, padded past 4 KiB.
	GenOversizeErrorEchoBearer = "oversize_error_echo_bearer"
)

// computeCalls are the calls a reply may be forced on.
var computeCalls = []string{"catalog", "create", "start", "stop", "delete", "operation", "get", "list", "labels", "extend", "usage", "ingress_get", "ingress_put"}

// ComputeControlInputs is what an Inputs offers when the command's control
// directory steers the compute service: the bytes of compute.json, or
// exists false when there is none.
type ComputeControlInputs interface {
	ComputeControl() (b []byte, exists bool, err error)
}

// computeFile is compute.json.
type computeFile struct {
	// Steps are set-up steps, each applied once, in order, by its id.
	Steps []struct {
		ID   string                     `json:"id"`
		Step map[string]json.RawMessage `json:"step"`
	} `json:"steps"`
	// Replies are forced replies by call; a call not named answers by the
	// contract's rules.
	Replies map[string]ComputeReply `json:"replies"`
	// AutoSettleS settles every cloud operation this many seconds after it
	// starts (0: only a cloud.settle step settles one).
	AutoSettleS int `json:"auto_settle_s"`
	// HostKey is published for every machine when it first runs, unless a
	// guest.publish_key step published another first.
	HostKey string `json:"host_key"`
	// Access is where running machines are reached (default the world's,
	// 127.0.0.1 port 22).
	Access *ComputeAccess `json:"access"`
}

// applyComputeControl reads compute.json, when the inputs have one, and
// applies what is new in it.
func (s *Server) applyComputeControl() error {
	ci, ok := s.inputs.(ComputeControlInputs)
	if !ok {
		return nil
	}
	b, exists, err := ci.ComputeControl()
	if err != nil || !exists {
		return err
	}
	if err := checkStrictJSON(b, 16); err != nil {
		return fmt.Errorf("compute.json: %w", err)
	}
	var f computeFile
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&f); err != nil {
		return fmt.Errorf("compute.json: %w", err)
	}
	for call, rep := range f.Replies {
		if !containsString(computeCalls, call) {
			return fmt.Errorf("compute.json: replies: %q is not a call", call)
		}
		if err := checkComputeReply(rep); err != nil {
			return fmt.Errorf("compute.json: replies: %s: %w", call, err)
		}
	}
	if f.HostKey != "" && !validHostKey(f.HostKey) {
		return errors.New("compute.json: host_key is not one ssh-ed25519 key")
	}
	c := s.compute
	c.mu.Lock()
	c.replies = map[string]ComputeReply{}
	for k, v := range f.Replies {
		c.replies[k] = v
	}
	c.setSettingsLocked(time.Duration(f.AutoSettleS)*time.Second, f.HostKey, f.Access)
	var todo []map[string]json.RawMessage
	for _, st := range f.Steps {
		if st.ID == "" {
			c.mu.Unlock()
			return errors.New("compute.json: a step has no id")
		}
		if !c.applied[st.ID] {
			c.applied[st.ID] = true
			todo = append(todo, st.Step)
		}
	}
	c.mu.Unlock()
	for _, st := range todo {
		if len(st) != 1 {
			return errors.New("compute.json: a step is not an object with one member")
		}
		for name, raw := range st {
			err := s.applyComputeStep(name, raw, c.advance)
			if err != nil {
				return fmt.Errorf("compute.json: %s: %w", name, err)
			}
		}
	}
	return nil
}

// ComputeTick does what the compute service does on its own: reads the
// control file, settles the operations due under auto_settle_s, and sweeps
// the leases that have ended. The command calls it every second.
func (s *Server) ComputeTick() error {
	err := s.applyComputeControl()
	now := s.computeNow()
	c := s.compute
	c.mu.Lock()
	defer c.mu.Unlock()
	c.settleDue(now)
	c.sweep(now)
	return err
}

func checkComputeReply(r ComputeReply) error {
	forms := 0
	if len(r.Body) > 0 {
		forms++
	}
	if r.Text != nil {
		forms++
	}
	if r.Generate != "" {
		forms++
		switch r.Generate {
		case GenOversize, GenOversizeList, GenList501, GenTruncated, GenEchoBearer, GenOversizeError, GenEchoBearerEscaped, GenOversizeErrorEchoBearer:
		default:
			return fmt.Errorf("generate %q is not one this service knows", r.Generate)
		}
	}
	if r.Silent {
		forms++
	}
	if forms > 1 {
		return errors.New("more than one body form")
	}
	if !r.Silent && r.Generate == "" && (r.Status < 100 || r.Status > 599) {
		return errors.New("a status from 100 to 599 is needed")
	}
	return nil
}

// serverNamed is the live servers with a name.
func (s *Server) serverNamed(name string) ([]string, error) {
	s.mu.Lock()
	var ids []string
	for id, sv := range s.servers {
		if sv.name == name {
			ids = append(ids, id)
		}
	}
	s.mu.Unlock()
	var live []string
	for _, id := range ids {
		disabled, err := s.inputs.Disabled(id)
		if err != nil {
			return nil, err
		}
		if !disabled {
			live = append(live, id)
		}
	}
	return live, nil
}

// decodeStep decodes a step's value strictly.
func decodeStep(raw json.RawMessage, v any) error {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return err
	}
	if _, err := dec.Token(); err != io.EOF {
		return errors.New("data after the value")
	}
	return nil
}

// stringPair decodes a step's value of two strings.
func stringPair(raw json.RawMessage) (string, string, error) {
	var p []string
	if err := decodeStep(raw, &p); err != nil || len(p) != 2 {
		return "", "", errors.New("the value is not two strings")
	}
	return p[0], p[1], nil
}

// applyComputeStep applies one set-up step. advance moves the compute
// clock (the tests move the scenario's clock; the command, its offset);
// after it the lease sweep runs.
func (s *Server) applyComputeStep(name string, raw json.RawMessage, advance func(time.Duration)) error {
	c := s.compute
	switch name {
	case "compute.allow", "compute.deny":
		var server string
		if err := decodeStep(raw, &server); err != nil {
			return err
		}
		ids, err := s.serverNamed(server)
		if err != nil {
			return err
		}
		if len(ids) == 0 {
			return fmt.Errorf("no live server %q", server)
		}
		c.mu.Lock()
		for _, id := range ids {
			c.grants[id] = name == "compute.allow"
		}
		c.mu.Unlock()
	case "client.register":
		server, client, err := stringPair(raw)
		if err != nil {
			return err
		}
		if !handleRx.MatchString(client) {
			return fmt.Errorf("the client name %q breaks the grammar", client)
		}
		c.mu.Lock()
		if c.clients[server] == nil {
			c.clients[server] = map[string]bool{}
		}
		c.clients[server][client] = true
		c.mu.Unlock()
	case "person.optout":
		handle, server, err := stringPair(raw)
		if err != nil {
			return err
		}
		c.mu.Lock()
		c.optouts[[2]string{handle, server}] = true
		c.mu.Unlock()
	case "policy.set":
		return s.setPolicy(raw)
	case "quota.fill":
		var args []string
		if err := decodeStep(raw, &args); err != nil {
			return err
		}
		c.mu.Lock()
		defer c.mu.Unlock()
		switch {
		case len(args) == 2 && args[0] == "person":
			if n := c.world.Policy.PersonMax - c.personCounts(args[1]).machines; n > 0 {
				c.fillPerson[args[1]] += n
			}
		case len(args) == 1 && args[0] == "org":
			if n := c.world.Policy.OrgMax - c.orgCounts().machines; n > 0 {
				c.fillOrg += n
			}
		default:
			return errors.New(`the value is ["person", "<handle>"] or ["org"]`)
		}
	case "cloud.fail":
		call, mode, err := stringPair(raw)
		if err != nil {
			return err
		}
		ok := false
		switch mode {
		case "quota", "capacity":
			ok = call == "create" || call == "start"
		case "busy":
			ok = call == "start" || call == "stop"
		case "lost_reply":
			ok = call == "create" || call == "start" || call == "stop" || call == "delete" || call == "extend" || call == "labels"
		}
		if !ok {
			return fmt.Errorf("cloud.fail %s %s is not one the contract names", call, mode)
		}
		c.mu.Lock()
		c.fails[call] = mode
		c.mu.Unlock()
	case "cloud.settle":
		server, shedID, err := stringPair(raw)
		if err != nil {
			return err
		}
		now := s.computeNow()
		c.mu.Lock()
		defer c.mu.Unlock()
		m := c.machines[mkey{server, shedID}]
		if m == nil {
			return fmt.Errorf("no machine %s of %s", shedID, server)
		}
		c.settle(m, now)
	case "guest.publish_key":
		var args []string
		if err := decodeStep(raw, &args); err != nil || len(args) != 3 {
			return errors.New(`the value is ["<server>", "<shed id>", "<key>"]`)
		}
		server, shedID, key := args[0], args[1], args[2]
		if !validHostKey(key) {
			return errors.New("the key is not one ssh-ed25519 key")
		}
		c.mu.Lock()
		defer c.mu.Unlock()
		m := c.machines[mkey{server, shedID}]
		if m == nil {
			return fmt.Errorf("no machine %s of %s", shedID, server)
		}
		switch {
		case m.hostKey == "":
			m.hostKey = key // the first key is kept
		case m.hostKey != key:
			// A later, different key is not used; the mismatch is recorded,
			// with no key in the line.
			c.logf("%s: %s", HostKeyMismatchLine, shedID)
		}
	case "ingress.break":
		var clause string
		if err := decodeStep(raw, &clause); err != nil {
			return err
		}
		switch clause {
		case breakExtraRule, breakMachineToMachine, breakSharedNetwork, breakUnreadable:
		default:
			return fmt.Errorf("ingress.break %q is not one the contract names", clause)
		}
		c.mu.Lock()
		c.breaks[clause] = true
		c.mu.Unlock()
	case "machine.delete":
		var args []string
		if err := decodeStep(raw, &args); err != nil || len(args) != 3 {
			return errors.New(`the value is ["<server>", "<shed id>", "page" | "external"]`)
		}
		server, shedID, cause := args[0], args[1], args[2]
		if cause != causePage && cause != causeExternal {
			return fmt.Errorf("machine.delete %q: the cause is page or external", cause)
		}
		now := s.computeNow()
		c.mu.Lock()
		defer c.mu.Unlock()
		m := c.machines[mkey{server, shedID}]
		if m == nil {
			return fmt.Errorf("no machine %s of %s", shedID, server)
		}
		if m.pending != nil {
			c.finish(m.pending, "DELETING", now)
			m.pending = nil
		}
		c.remove(m, cause, now)
	case "clock.advance":
		var secs int64
		if err := decodeStep(raw, &secs); err != nil || secs < 0 {
			return errors.New("the value is a non-negative number of seconds")
		}
		advance(time.Duration(secs) * time.Second)
		now := s.computeNow()
		c.mu.Lock()
		c.settleDue(now)
		c.sweep(now)
		c.mu.Unlock()
	default:
		return fmt.Errorf("the set-up step %q is not one the compute contract names", name)
	}
	return nil
}

// setPolicy applies policy.set: an object of the policy's members to set.
func (s *Server) setPolicy(raw json.RawMessage) error {
	var p struct {
		InitialLeaseS   *int64 `json:"initial_lease_s"`
		MaxAgeS         *int64 `json:"max_age_s"`
		ExtendAllowed   *bool  `json:"extend_allowed"`
		PresetsH        []int  `json:"presets_h"`
		ClientMaxLeaseS *int64 `json:"client_max_lease_s"`
		WarnS           *int64 `json:"warn_s"`
		PersonMax       *int   `json:"person_max"`
		OrgMax          *int   `json:"org_max"`
		IngressWritable *bool  `json:"ingress_writable"`
	}
	if err := decodeStep(raw, &p); err != nil {
		return err
	}
	c := s.compute
	c.mu.Lock()
	defer c.mu.Unlock()
	w := &c.world.Policy
	setI := func(dst *int64, v *int64) {
		if v != nil {
			*dst = *v
		}
	}
	setI(&w.InitialLeaseS, p.InitialLeaseS)
	setI(&w.MaxAgeS, p.MaxAgeS)
	setI(&w.ClientMaxLeaseS, p.ClientMaxLeaseS)
	setI(&w.WarnS, p.WarnS)
	if p.ExtendAllowed != nil {
		w.ExtendAllowed = *p.ExtendAllowed
	}
	if p.PresetsH != nil {
		w.PresetsH = p.PresetsH
	}
	if p.PersonMax != nil {
		w.PersonMax = *p.PersonMax
	}
	if p.OrgMax != nil {
		w.OrgMax = *p.OrgMax
	}
	if p.IngressWritable != nil {
		w.IngressWritable = *p.IngressWritable
	}
	return nil
}

// sendComputeReply sends a forced reply.
func (s *Server) sendComputeReply(w http.ResponseWriter, r *http.Request, call *computeCall, rep ComputeReply) {
	switch {
	case rep.Silent:
		// Read the request whole first, so the server notices the client
		// giving up.
		_, _ = io.Copy(io.Discard, io.LimitReader(r.Body, maxComputeRequest+1))
		select {
		case <-r.Context().Done():
		case <-time.After(silentHold):
		}
		panic(http.ErrAbortHandler)
	case rep.Generate != "":
		s.generateComputeReply(w, r, call, rep.Generate)
		return
	}
	h := w.Header()
	h.Set("Cache-Control", "no-store")
	for k, v := range rep.Headers {
		h.Set(k, v)
	}
	switch {
	case rep.Text != nil:
		w.WriteHeader(rep.Status)
		_, _ = io.WriteString(w, *rep.Text)
	case len(rep.Body) > 0:
		if h.Get("Content-Type") == "" {
			h.Set("Content-Type", "application/json")
		}
		w.WriteHeader(rep.Status)
		w.Write(rep.Body)
	default:
		w.WriteHeader(rep.Status)
	}
}

// sampleMachine is a valid running machine of the calling server, for the
// generated replies: the path's shed id, or the n-th of a made-up series.
func (s *Server) sampleMachine(call *computeCall, shedID, spec string) map[string]any {
	c := s.compute
	c.mu.Lock()
	defer c.mu.Unlock()
	now := call.now.Truncate(time.Second)
	m := &cmachine{
		server: call.server, shedID: shedID, name: "sample", user: "ada", state: stateRunning,
		size: c.world.Catalog.Sizes[0], diskGB: c.world.Catalog.Sizes[0].DiskGBDefault,
		imageReq: "default", imageRes: c.world.Catalog.Images.Default,
		created: now, started: now, expires: now.Add(24 * time.Hour), maxExpires: now.Add(120 * time.Hour),
		labels: map[string]string{}, spec: spec,
	}
	return c.machineJSON(m, now)
}

// seriesID is the n-th shed id of a made-up series: 26 base32 characters.
func seriesID(n int) string {
	var b [16]byte
	binary.BigEndian.PutUint64(b[8:], uint64(n))
	copy(b[:8], "listfill")
	return strings.ToLower(base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(b[:]))
}

// generateComputeReply writes one generated reply.
func (s *Server) generateComputeReply(w http.ResponseWriter, r *http.Request, call *computeCall, gen string) {
	shedID := call.route.id
	if !computeIDRx.MatchString(shedID) {
		shedID = seriesID(0)
	}
	switch gen {
	case GenOversize:
		head := encode(map[string]any{"machine": s.sampleMachine(call, shedID, "")})
		// {"machine":{...},"pad":"aaa…"} of exactly the limit and one.
		head = head[:len(head)-1]
		head = append(head, []byte(`,"pad":"`)...)
		pad := maxComputeReply + 1 - len(head) - 2
		body := append(append(head, bytes.Repeat([]byte("a"), pad)...), '"', '}')
		writeBody(w, http.StatusOK, body, true)
	case GenOversizeList:
		head := []byte(`{"machines":[],"deleted":[],"complete":true,"pad":"`)
		pad := maxComputeList + 1 - len(head) - 2
		body := append(append(head, bytes.Repeat([]byte("a"), pad)...), '"', '}')
		writeBody(w, http.StatusOK, body, true)
	case GenList501:
		ms := make([]any, 0, maxComputeListMachines+1)
		for i := 0; i <= maxComputeListMachines; i++ {
			ms = append(ms, s.sampleMachine(call, seriesID(i), ""))
		}
		writeBody(w, http.StatusOK, encode(map[string]any{"machines": ms, "deleted": []any{}, "complete": true}), true)
	case GenTruncated:
		body := encode(map[string]any{"machine": s.sampleMachine(call, shedID, "")})
		h := w.Header()
		h.Set("Content-Type", "application/json")
		h.Set("Cache-Control", "no-store")
		h.Set("Content-Length", fmt.Sprint(len(body)))
		w.WriteHeader(http.StatusOK)
		w.Write(body[:len(body)/2])
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		panic(http.ErrAbortHandler)
	case GenEchoBearer:
		_, tok, _ := strings.Cut(r.Header.Get("Authorization"), " ")
		writeBody(w, http.StatusOK, encode(map[string]any{"machine": s.sampleMachine(call, shedID, tok)}), true)
	case GenEchoBearerEscaped:
		_, tok, _ := strings.Cut(r.Header.Get("Authorization"), " ")
		body := encode(map[string]any{"machine": s.sampleMachine(call, shedID, "SPEC-HERE")})
		if tok != "" {
			body = bytes.Replace(body, []byte(`"SPEC-HERE"`), []byte(fmt.Sprintf(`"\u%04x%s"`, tok[0], tok[1:])), 1)
		}
		writeBody(w, http.StatusOK, body, true)
	case GenOversizeErrorEchoBearer:
		_, tok, _ := strings.Cut(r.Header.Get("Authorization"), " ")
		head := encode(map[string]any{"error": map[string]string{"code": "BUSY", "message": "token " + tok}})
		writeBody(w, http.StatusConflict, append(head, bytes.Repeat([]byte(" "), 4<<10)...), true)
	case GenOversizeError:
		body := encode(map[string]any{"error": map[string]string{"code": "BUSY", "message": strings.Repeat("busy ", 1000)}})
		writeBody(w, http.StatusConflict, body, true)
	}
}

// ComputeInventory is every machine the compute service holds, by the name
// of the server it belongs to: shed ids, sorted (a machine being deleted
// counts until it is gone). The command writes it to its --inventory file,
// so a test can confirm the machines it made are gone by their ids.
func (s *Server) ComputeInventory() map[string][]string {
	c := s.compute
	c.mu.Lock()
	defer c.mu.Unlock()
	out := map[string][]string{}
	for k := range c.machines {
		out[k.server] = append(out[k.server], k.shedID)
	}
	for _, ids := range out {
		sort.Strings(ids)
	}
	return out
}
