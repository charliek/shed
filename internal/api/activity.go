package api

import (
	"encoding/json"
	"log"
	"net/http"
	"strconv"

	"github.com/charliek/shed/internal/audit"
	"github.com/charliek/shed/internal/backend"
	"github.com/charliek/shed/internal/config"
)

// GET /api/activity?since=&shed=&limit= (plan 039 §3.6): the server's audit
// log read back, newest first, as the caller may see it.
//
// What a caller sees is decided by each event as it was written, never by
// the sheds as they are now: a person sees the lifecycle and delivery events
// whose shed was theirs when the event was written (its recorded user), so a
// shed deleted and its name taken by someone else keeps its old events with
// the old user, and the new user's events are theirs. A service client sees
// the events of the sheds it owned. An admin (and the implicit user of a
// server with no user list) sees every event, the older kinds (api, exec.end,
// reap, ssh, connect) included; those name no shed id and no shed user, so
// nobody else sees them. A delivery event's detail (its steps, the source,
// items and failure) is shown only to a caller who may enter the shed as it
// was recorded; anyone else gets its state, as the shed document does.
//
// The visibility rule and the shed filter are applied to each record before
// it counts toward the page, and a page reads no more than a bounded number
// of lines and bytes (activityBudget): when the budget runs out first, the
// page holds what was found and next continues from there. A cursor is the
// audit log's (audit.Cursor): a record's seq and its place in the files,
// which rotation does not change.

// Activity paging bounds.
const (
	// activityDefaultLimit is a page's size when limit is not given.
	activityDefaultLimit = 100
	// ActivityMaxLimit is the largest page; a larger limit is taken as it.
	ActivityMaxLimit = 500
)

// activityBudget is what one page reads, at most: lines of every kind, and
// bytes.
var activityBudget = audit.Budget{Lines: 10000, Bytes: 32 << 20}

// ActivityResponse is GET /api/activity's answer: events newest first, and
// next, the cursor to pass as since for the page after ("" when there is
// nothing older to read).
type ActivityResponse struct {
	Events []ActivityEvent `json:"events"`
	Next   string          `json:"next"`
}

// ActivityEvent is one event. ID is opaque and unique on the server (passed
// as since, the page starts with the event just older); At is when it was written (RFC
// 3339, UTC). Kind is the audit kind: shed.create, shed.start, shed.stop,
// shed.delete, shed.reset, shed.assign, shed.provision.start,
// shed.provision.end and delivery for everyone who may see them, and an
// older kind (api, exec.end, reap, ssh, connect) for an admin. Result (ok or
// failed) is a lifecycle event's; Delivery a delivery event's.
type ActivityEvent struct {
	ID       string                 `json:"id"`
	At       string                 `json:"at"`
	Kind     string                 `json:"kind"`
	Shed     ActivityShed           `json:"shed"`
	Actor    ActivityActor          `json:"actor"`
	Result   string                 `json:"result,omitempty"`
	Delivery *audit.DeliveryOutcome `json:"delivery,omitempty"`
}

// ActivityShed is the shed an event is about, as it was then: its name, id,
// user and owning service client (an older kind has only its name and, for
// some, its owner; an api line for a route with no shed has none).
type ActivityShed struct {
	Name  string `json:"name"`
	ID    string `json:"id,omitempty"`
	User  string `json:"user,omitempty"`
	Owner string `json:"owner,omitempty"`
}

// ActivityActor is who acted: kind user (by name, or implicit), client (by
// name), system (the server itself) or unknown. Script is set for a user who
// acted with a script key.
type ActivityActor struct {
	Kind     string `json:"kind"`
	User     string `json:"user,omitempty"`
	Client   string `json:"client,omitempty"`
	Implicit bool   `json:"implicit,omitempty"`
	Script   bool   `json:"script,omitempty"`
}

// activityRecord is the part of an audit line the feed reads.
type activityRecord struct {
	TS        string                 `json:"ts"`
	Event     string                 `json:"event"`
	Identity  audit.Identity         `json:"identity"`
	Shed      string                 `json:"shed"`
	ShedID    string                 `json:"shed_id"`
	ShedUser  string                 `json:"shed_user"`
	ShedOwner string                 `json:"shed_owner"`
	Owner     string                 `json:"owner"`
	Result    string                 `json:"result"`
	Delivery  *audit.DeliveryOutcome `json:"delivery"`
}

// activityVisible reports whether p may see rec: an admin (or the implicit
// user) every record; anyone else only a lifecycle or delivery event, a
// service client of a shed it owned, a person of a shed that was theirs.
func activityVisible(p backend.Principal, rec *activityRecord) bool {
	if p.Kind != backend.PrincipalClient && p.IsAdmin() {
		return true
	}
	if !audit.IsShedEventKind(rec.Event) {
		return false
	}
	switch p.Kind {
	case backend.PrincipalClient:
		return p.Client != "" && rec.ShedOwner == p.Client
	case backend.PrincipalUser:
		return p.User != "" && rec.ShedOwner == "" && rec.ShedUser == p.User
	}
	return false
}

// activityEvent is rec as p sees it.
func activityEvent(p backend.Principal, at audit.Cursor, rec *activityRecord) ActivityEvent {
	ev := ActivityEvent{
		ID:   at.String(),
		At:   rec.TS,
		Kind: rec.Event,
		Shed: ActivityShed{Name: rec.Shed, ID: rec.ShedID, User: rec.ShedUser, Owner: rec.ShedOwner},
		Actor: ActivityActor{
			Kind: rec.Identity.Kind, User: rec.Identity.User, Implicit: rec.Identity.Implicit,
			Script: rec.Identity.Script != nil && *rec.Identity.Script,
		},
	}
	if rec.Identity.Kind == audit.KindClient {
		ev.Actor.Client = rec.Identity.Name
	}
	if !audit.IsShedEventKind(rec.Event) {
		// An older kind: its shed by name, and the owner some carry.
		ev.Shed = ActivityShed{Name: rec.Shed, Owner: rec.Owner}
		return ev
	}
	if rec.Event == audit.EventDelivery {
		d := audit.DeliveryOutcome{}
		if rec.Delivery != nil {
			d = *rec.Delivery
		}
		if backend.Authorize(p, rec.ShedOwner, rec.ShedUser, backend.ActionEnter) != nil {
			d = d.StateOnly()
		}
		ev.Delivery = &d
		return ev
	}
	ev.Result = rec.Result
	return ev
}

// handleActivity serves GET /api/activity.
func (s *Server) handleActivity(w http.ResponseWriter, r *http.Request) {
	p, ok := backend.PrincipalFrom(r.Context())
	if !ok {
		writeError(w, http.StatusForbidden, config.ErrForbidden, "no caller identity on the request")
		return
	}
	q := r.URL.Query()
	limit := activityDefaultLimit
	if v := q.Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			writeError(w, http.StatusBadRequest, config.ErrInvalidRequest, "limit must be a whole number from 1")
			return
		}
		limit = min(n, ActivityMaxLimit)
	}
	var from audit.Cursor
	if v := q.Get("since"); v != "" {
		if from, ok = audit.ParseCursor(v); !ok {
			writeError(w, http.StatusBadRequest, config.ErrInvalidRequest, "since is not a cursor this server gave")
			return
		}
	}
	shed := q.Get("shed")
	if shed != "" && config.ValidateShedName(shed) != nil && !config.ValidShedID(shed) {
		writeError(w, http.StatusBadRequest, config.ErrInvalidRequest, "shed must be a shed name or a shed id")
		return
	}

	resp := ActivityResponse{Events: []ActivityEvent{}}
	budget := activityBudget
	if s.activityScan > 0 {
		budget.Lines = s.activityScan
	}
	next, more, err := s.audit.ReadBack(from, limit, budget, func(at audit.Cursor, _ uint64, line []byte) bool {
		var rec activityRecord
		if json.Unmarshal(line, &rec) != nil {
			return false
		}
		if !activityVisible(p, &rec) {
			return false
		}
		// shed= is a name, or an id; a shed whose name is an id's form
		// matches both ways.
		if shed != "" && rec.Shed != shed && rec.ShedID != shed {
			return false
		}
		resp.Events = append(resp.Events, activityEvent(p, at, &rec))
		return true
	})
	if err != nil {
		log.Printf("activity: reading the audit log: %v", err)
		writeError(w, http.StatusInternalServerError, config.ErrInternalError, "the audit log could not be read")
		return
	}
	if more {
		resp.Next = next.String()
	}
	writeJSON(w, http.StatusOK, resp)
}
