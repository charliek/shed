package api

// The credential source routes (`shed source ls|use|unbind|status`): a user
// reads which of their machines answers their credential requests on this
// server, moves that to another machine of theirs, or clears it. The pin
// itself, its rules and its locks are the bus's (internal/plugin/pin.go and
// the note at the top of internal/plugin/registry.go); these routes only
// decide who may read, move and clear whose binding, and call
// Registry.Sources for the read, Registry.Move for the move and
// Registry.Unbind for the clearing (each under the user's gate, so each
// answer is one moment's state).
//
// Who may:
//   - A user reads, moves and clears their own binding, from any of their
//     keys that is not a script key.
//   - An admin may also name another configured user (?user= on the read,
//     "user" in the body of the move and of the unbind), and can then only
//     choose among the machines that user's own keys have presented: machine
//     records are kept per user.
//   - A script key is refused every route. It may not become, move or clear
//     a source, and the read is refused too: the bound machine's id is the
//     whole of what admission matches on, so the routes hand it to nobody
//     who may not hold the source.
//   - A service client is refused (the routes are not on its allowlist, and
//     the handlers refuse any caller that is not a user), and is never a
//     target.
//   - A user who is not an admin naming anyone but themselves gets the same
//     403 whether or not that user exists.
//
// The routes are outside /api/plugins, so they need the control scope: a
// credentials certificate (what a machine's broker holds) can neither read,
// move nor clear a binding.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"time"

	"github.com/charliek/shed/internal/config"
	"github.com/charliek/shed/internal/plugin"
	"github.com/charliek/shed/internal/source"
)

// sourcesBodyLimit bounds the body of POST /api/sources/use.
const sourcesBodyLimit = 4 << 10

// sourceCaller returns the caller's own user on the source routes, and
// whether the caller may name another user (an admin, or the implicit user
// of a server with no user list, who is one). It answers 403 and reports
// ok=false for a caller with no place on them: a service client, a request
// with no caller, and a script key.
func sourceCaller(w http.ResponseWriter, r *http.Request) (own string, admin, ok bool) {
	own, admin, ok = busCaller(r)
	if !ok {
		writeError(w, http.StatusForbidden, config.ErrForbidden, "the credential source routes are for users")
		return "", false, false
	}
	if callerFrom(r.Context()).Script {
		writeError(w, http.StatusForbidden, config.ErrForbidden, "a script key cannot see or move a credential source")
		return "", false, false
	}
	return own, admin, true
}

// sourceTarget returns the user a source request acts on: the caller's own
// (named is empty or names the caller), or, for an admin, the configured
// user named. A caller who is not an admin naming anyone else gets the same
// 403 whether or not that user exists; an admin naming a user who is not
// configured gets 400. ok=false means a refusal was written.
func (s *Server) sourceTarget(w http.ResponseWriter, own string, admin bool, named string) (string, bool) {
	if named == "" || named == own {
		return own, true
	}
	if !admin {
		writeError(w, http.StatusForbidden, config.ErrForbidden, "only an admin can see or move another user's credential source")
		return "", false
	}
	if !s.configuredUser(named) {
		writeError(w, http.StatusBadRequest, config.ErrInvalidRequest,
			fmt.Sprintf("no user named %q is configured on this server (auth.users)", named))
		return "", false
	}
	return named, true
}

// sourceBindings returns the bus's source bindings, answering 503 and
// returning nil when the server has none (no bus, or not pinned).
func (s *Server) sourceBindings(w http.ResponseWriter) source.Bindings {
	if s.plugins != nil {
		if b := s.plugins.Bindings(); b != nil {
			return b
		}
	}
	writeError(w, http.StatusServiceUnavailable, "PLUGINS_DISABLED", "credential source bindings not initialized")
	return nil
}

// sourcesReadWait bounds how long GET /api/sources waits for the user's gate
// (held by a move or an admission that may be waiting for the store's disk)
// before it answers 503 SOURCE_BUSY. A var so tests can shorten it.
var sourcesReadWait = 2 * time.Second

// sourceMovedHook runs in POST /api/sources/use after the move returned and
// before the answer is written (nil in production): a test moves the binding
// again there, to show the answer is the state the request's own move left.
var sourceMovedHook func()

// sourcesResponse is the wire form of a view of one user's sources, read
// under that user's gate (plugin.Registry.Sources, or a move's result): each
// machine with whether it holds a subscription and to which namespaces. It
// holds only that user's records.
func sourcesResponse(v plugin.SourcesView) config.SourcesResponse {
	resp := config.SourcesResponse{User: v.User, Machines: []config.SourceMachine{}}
	if bd := v.Binding; bd != nil {
		resp.Binding = &config.SourceBinding{
			Delivery:   sourceRefs(bd.Delivery),
			Signing:    sourceRefs(bd.Signing),
			BoundAt:    bd.BoundAt,
			BoundBy:    bd.BoundBy,
			Generation: bd.Generation,
		}
	}
	for _, m := range v.Machines {
		ns, ok := v.Connected[m.ID]
		if ns == nil {
			ns = []string{}
		}
		resp.Machines = append(resp.Machines, config.SourceMachine{
			ID:             m.ID,
			Name:           m.Name,
			KeyFingerprint: m.KeyFingerprint,
			FirstSeen:      m.FirstSeen,
			Connected:      ok,
			Namespaces:     ns,
		})
	}
	return resp
}

func sourceRefs(refs []source.Ref) []config.SourceRef {
	out := make([]config.SourceRef, 0, len(refs))
	for _, r := range refs {
		out = append(out, config.SourceRef{Kind: r.Kind, ID: r.ID})
	}
	return out
}

// handleListSources is GET /api/sources (`shed source ls` and `status`): the
// caller's binding and machines, or, for an admin, those of the user named
// by ?user=.
//
// The answer is read under the user's gate, the critical section moves and
// admissions run in, so it is one moment's state: never the binding of one
// side of a move with the connections of the other. It waits for the gate no
// longer than sourcesReadWait (and no longer than the request lives): a move
// or an admission holding it may be waiting for the store's disk, and then
// the answer is 503 SOURCE_BUSY with Retry-After rather than a view that
// could mix the two sides. A client that goes away while waiting is
// answered nothing.
//
//	403 FORBIDDEN         a script key, a service client, or a user who is not
//	                      an admin naming another user
//	400 INVALID_REQUEST   an admin naming a user who is not configured, or
//	                      ?user given twice
//	503 SOURCE_BUSY       the user's records stayed busy past the bounded wait
//	503 PLUGINS_DISABLED  the server has no credential bus
func (s *Server) handleListSources(w http.ResponseWriter, r *http.Request) {
	own, admin, ok := sourceCaller(w, r)
	if !ok {
		return
	}
	named := r.URL.Query()["user"]
	if len(named) > 1 {
		writeError(w, http.StatusBadRequest, config.ErrInvalidRequest, "name at most one user")
		return
	}
	var want string
	if len(named) == 1 {
		want = named[0]
	}
	user, ok := s.sourceTarget(w, own, admin, want)
	if !ok {
		return
	}
	if s.sourceBindings(w) == nil {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), sourcesReadWait)
	defer cancel()
	v, err := s.plugins.Sources(ctx, user)
	if err != nil {
		switch {
		case errors.Is(err, plugin.ErrNotPinned):
			writeError(w, http.StatusServiceUnavailable, "PLUGINS_DISABLED", "credential source bindings not initialized")
		case r.Context().Err() != nil:
			// The client went away while waiting for the user's gate.
		case errors.Is(err, context.DeadlineExceeded):
			w.Header().Set("Retry-After", "1")
			writeError(w, http.StatusServiceUnavailable, config.ErrSourceBusy,
				fmt.Sprintf("the credential source of %s is being changed or saved right now; retry in a moment", whoseSources(user, user != own)))
		default:
			writeError(w, http.StatusInternalServerError, config.ErrInternalError, "the credential sources could not be read")
		}
		return
	}
	writeJSON(w, http.StatusOK, sourcesResponse(v))
}

// handleUseSource is POST /api/sources/use (`shed source use`): make a
// machine that has presented itself for the user their credential source.
// Body {"machine_id":"<32 hex>"} and, for an admin, "user". The move is
// Registry.Move: the store is changed first, then, under the user's gate,
// the old machine's streams are closed and the requests sent to it dropped.
// The new machine is not called: it connects on its next retry. Moving to the
// machine already bound changes nothing. The answer is the user's sources as
// this move left them, read inside the move's own hold of the user's gate
// (never a later move's). The wait for the gate ends with the request.
//
// The body is exactly one JSON object whose only fields are "machine_id"
// and "user", each at most once, spelled exactly so, with a string value
// (null is taken as absent), and nothing after it but white space.
//
//	400 INVALID_REQUEST           the body, a malformed machine id, or an
//	                              admin naming a user who is not configured
//	403 FORBIDDEN                 as for GET /api/sources
//	404 SOURCE_MACHINE_NOT_FOUND  no machine with that id has presented
//	                              itself for the user
//	500 INTERNAL_ERROR            the move was not confirmed: the store could
//	                              not save it. The source in use is unchanged
//	                              for now, but a save that failed late may
//	                              still take effect when the server restarts;
//	                              'shed source ls' shows which is true
//	503 PLUGINS_DISABLED          the server has no credential bus
func (s *Server) handleUseSource(w http.ResponseWriter, r *http.Request) {
	own, admin, ok := sourceCaller(w, r)
	if !ok {
		return
	}
	req, err := decodeSourceUse(http.MaxBytesReader(w, r.Body, sourcesBodyLimit))
	if err != nil {
		writeError(w, http.StatusBadRequest, config.ErrInvalidRequest, `the body must be {"machine_id":"<32 lowercase hex digits>"}: `+err.Error())
		return
	}
	user, ok := s.sourceTarget(w, own, admin, req.User)
	if !ok {
		return
	}
	if !source.ValidMachineID(req.MachineID) {
		writeError(w, http.StatusBadRequest, config.ErrInvalidRequest,
			"machine_id must be a machine's full id, 32 lowercase hex digits ('shed source ls --json' lists them)")
		return
	}
	if s.sourceBindings(w) == nil {
		return
	}
	c := callerFrom(r.Context())
	by := moverFor(c, user != own)
	res, err := s.plugins.Move(r.Context(), user, req.MachineID, by)
	if err != nil {
		switch {
		case errors.Is(err, source.ErrUnknownMachine):
			writeError(w, http.StatusNotFound, config.ErrSourceMachineNotFound,
				fmt.Sprintf("no machine with id %s has presented itself for %s on this server; 'shed source ls' lists those that have",
					req.MachineID, whoseSources(user, user != own)))
		case errors.Is(err, plugin.ErrNotPinned):
			writeError(w, http.StatusServiceUnavailable, "PLUGINS_DISABLED", "credential source bindings not initialized")
		case r.Context().Err() != nil && errors.Is(err, r.Context().Err()):
			// The client went away while waiting for the user's gate:
			// nothing moved.
		default:
			// The store reports a save it could not confirm. A save whose
			// rename landed but whose directory sync failed is not live (the
			// bus keeps the old binding) but may be what the server reads
			// at its next start, so neither "moved" nor "nothing changed" is
			// certain.
			log.Printf("credential source%s: moving it to machine %s for %q was not confirmed (the current source stays in use; the saved file may hold the move after a restart): %v", forBusUser(user), req.MachineID, by, err)
			writeError(w, http.StatusInternalServerError, config.ErrInternalError,
				"the move was not confirmed: the server could not save it. The current credential source stays in use for now, but the move may still take effect when the server restarts; check with 'shed source ls' before retrying")
		}
		return
	}
	if sourceMovedHook != nil {
		sourceMovedHook()
	}
	writeJSON(w, http.StatusOK, sourcesResponse(res.View))
}

// handleUnbindSource is POST /api/sources/unbind (`shed source unbind`):
// clear the user's credential source. Body {} (or none) and, for an admin,
// "user", under the rules of POST /api/sources/use. The unbind is
// Registry.Unbind: the store forgets the user's binding and remembered
// machines, then, under the user's gate, the user's streams are closed and
// their requests dropped. Idempotent: a user with no binding is answered 200
// with "unbound": false. The first of the user's machines to connect next is
// bound (first-connect), so a machine still running reconnects and becomes
// the source again.
//
//	400 INVALID_REQUEST   the body, or an admin naming a user who is not
//	                      configured
//	403 FORBIDDEN         as for GET /api/sources
//	500 INTERNAL_ERROR    the store could not save it: the source in use is
//	                      unchanged for now, but a save that failed late may
//	                      still take effect when the server restarts
//	503 PLUGINS_DISABLED  the server has no credential bus
func (s *Server) handleUnbindSource(w http.ResponseWriter, r *http.Request) {
	own, admin, ok := sourceCaller(w, r)
	if !ok {
		return
	}
	req, err := decodeSourceUnbind(http.MaxBytesReader(w, r.Body, sourcesBodyLimit))
	if err != nil {
		writeError(w, http.StatusBadRequest, config.ErrInvalidRequest, `the body must be {} or {"user":"<name>"}: `+err.Error())
		return
	}
	user, ok := s.sourceTarget(w, own, admin, req.User)
	if !ok {
		return
	}
	if s.sourceBindings(w) == nil {
		return
	}
	by := moverFor(callerFrom(r.Context()), user != own)
	res, err := s.plugins.Unbind(r.Context(), user, by)
	if err != nil {
		switch {
		case errors.Is(err, plugin.ErrNotPinned):
			writeError(w, http.StatusServiceUnavailable, "PLUGINS_DISABLED", "credential source bindings not initialized")
		case r.Context().Err() != nil && errors.Is(err, r.Context().Err()):
			// The client went away while waiting for the user's gate:
			// nothing changed.
		default:
			log.Printf("credential source%s: clearing it by %q was not confirmed (the current source stays in use; the saved file may hold the change after a restart): %v", forBusUser(user), by, err)
			writeError(w, http.StatusInternalServerError, config.ErrInternalError,
				"the unbind was not confirmed: the server could not save it. The current credential source stays in use for now, but the change may still take effect when the server restarts; check with 'shed source status' before retrying")
		}
		return
	}
	resp := config.SourceUnbindResponse{User: user, Unbound: res.Unbound, ForgottenMachines: res.Forgotten}
	if res.Unbound {
		resp.Previous = &config.SourceMachineRef{ID: res.Previous.ID, Name: res.Previous.Name}
	}
	writeJSON(w, http.StatusOK, resp)
}

// decodeSourceUse reads the body of POST /api/sources/use strictly: one JSON
// object, its fields only "machine_id" and "user" (decodeSourceFields).
func decodeSourceUse(body io.Reader) (config.SourceUseRequest, error) {
	var req config.SourceUseRequest
	err := decodeSourceFields(body, false, map[string]*string{"machine_id": &req.MachineID, "user": &req.User})
	return req, err
}

// decodeSourceUnbind reads the body of POST /api/sources/unbind strictly:
// one JSON object whose only field is "user", or no body at all (the
// caller's own).
func decodeSourceUnbind(body io.Reader) (config.SourceUnbindRequest, error) {
	var req config.SourceUnbindRequest
	err := decodeSourceFields(body, true, map[string]*string{"user": &req.User})
	return req, err
}

// decodeSourceFields reads a source route's body strictly: one JSON object,
// its fields only those named in fields, each at most once and spelled
// exactly (the standard decoder matches field names without regard to case
// and lets the last of two equal names win), each a string or null (taken as
// absent), and nothing after the object but white space. With emptyOK, a
// body with nothing in it but white space is an empty object.
func decodeSourceFields(body io.Reader, emptyOK bool, fields map[string]*string) error {
	dec := json.NewDecoder(body)
	tok, err := dec.Token()
	if emptyOK && errors.Is(err, io.EOF) {
		return nil
	}
	if err != nil {
		return err
	}
	if d, ok := tok.(json.Delim); !ok || d != '{' {
		return errors.New("it is not a JSON object")
	}
	seen := map[string]bool{}
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			return err
		}
		key, _ := tok.(string)
		dst, ok := fields[key]
		if !ok {
			return fmt.Errorf("unknown field %q", key)
		}
		if seen[key] {
			return fmt.Errorf("field %q given twice", key)
		}
		seen[key] = true
		if err := dec.Decode(dst); err != nil {
			return fmt.Errorf("field %q: %v", key, err)
		}
	}
	if _, err := dec.Token(); err != nil { // the closing '}'
		return err
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return errors.New("data after the JSON object")
	}
	return nil
}

// whoseSources names the user a source route acted on, in a message.
func whoseSources(user string, other bool) string {
	if !other {
		return "your user"
	}
	return fmt.Sprintf("user %q", user)
}

// moverFor is what a move records as its mover (the binding's bound_by, and
// the move's log line): who moved it, and with which key, in a form a person
// can read later. other is set when an admin moved another user's binding.
// The key's label is quoted (it is configuration or an authorized_keys
// comment); a mover too long for the store keeps only the fingerprint.
func moverFor(c Caller, other bool) string {
	who := "user " + c.User
	switch {
	case other:
		who = "admin " + c.User
	case c.Implicit:
		who = "the implicit user"
	}
	if c.Name == "" {
		return who + " (no key: open mode)"
	}
	if c.KeyLabel != "" {
		if by := fmt.Sprintf("%s with key %q (%s)", who, c.KeyLabel, c.Name); len(by) <= source.MaxTextBytes {
			return by
		}
	}
	return who + " with key " + c.Name
}
