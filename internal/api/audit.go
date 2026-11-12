package api

import (
	"context"
	"log"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5/middleware"

	"github.com/charliek/shed/internal/audit"
)

// SetAuditLog attaches the audit log (<state>/audit.jsonl). shed-server opens
// it when auth.clients is set or audit.enabled is true; nil (the default)
// records nothing.
func (s *Server) SetAuditLog(l *audit.Log) { s.audit = l }

// recordAudit appends one record, logging (never failing the request on) a
// write error.
func (s *Server) recordAudit(v any) {
	if s.audit == nil {
		return
	}
	if err := s.audit.Record(v); err != nil {
		log.Printf("audit: %v", err)
	}
}

// auditIdentity is the audit form of a caller: a client by name and serial,
// an admin by subject (none in open mode).
func auditIdentity(c Caller) audit.Identity {
	if c.IsClient() {
		return audit.Identity{Kind: audit.KindClient, Name: c.Name, Serial: c.Serial}
	}
	return audit.Identity{Kind: audit.KindAdmin, Subject: c.Name}
}

// auditEntry is the part of a client request's api line that is learned while
// the request is served: the matched route (serveServiceClient) and the shed
// (the URL's {name}, or the create handler's request body). It is only ever
// touched from the request's own goroutine.
type auditEntry struct {
	route string
	shed  string
}

type auditEntryKey struct{}

func auditEntryFrom(ctx context.Context) *auditEntry {
	e, _ := ctx.Value(auditEntryKey{}).(*auditEntry)
	return e
}

// setAuditShed names the shed of the current client request's api line (a
// no-op for an admin request, which has no entry).
func setAuditShed(ctx context.Context, name string) {
	if e := auditEntryFrom(ctx); e != nil {
		e.shed = name
	}
}

// auditClientRequest serves one service client request through serve and
// then writes its api line: exactly one per request, whether it was allowed,
// refused (401, 403, 404, a policy violation) or failed. Admin requests never
// come through here and are not logged.
func (s *Server) auditClientRequest(w http.ResponseWriter, r *http.Request, id audit.Identity, serve func(http.ResponseWriter, *http.Request)) {
	if s.audit == nil {
		serve(w, r)
		return
	}
	start := time.Now()
	entry := &auditEntry{}
	ww := middleware.NewWrapResponseWriter(w, r.ProtoMajor)
	defer func() {
		status := ww.Status()
		if status == 0 {
			status = http.StatusOK // nothing written: net/http sends 200
		}
		s.recordAudit(audit.APIEvent{
			Event:      audit.EventAPI,
			RequestID:  middleware.GetReqID(r.Context()),
			Identity:   id,
			Remote:     r.RemoteAddr,
			Method:     r.Method,
			Route:      entry.route,
			Path:       r.URL.Path,
			Shed:       entry.shed,
			Status:     status,
			DurationMS: time.Since(start).Milliseconds(),
		})
	}()
	serve(ww, r.WithContext(context.WithValue(r.Context(), auditEntryKey{}, entry)))
}

// newExecEndEvent starts the exec.end line for an exec of argv in shed name:
// argv[0] and the count by default, the whole argv with audit.exec_argv: full.
// ExitCode starts at -1 (no exit status) until the handler learns one.
func (s *Server) newExecEndEvent(r *http.Request, name string, argv []string) *audit.ExecEndEvent {
	e := &audit.ExecEndEvent{
		Event:     audit.EventExecEnd,
		RequestID: middleware.GetReqID(r.Context()),
		Identity:  auditIdentity(callerFrom(r.Context())),
		Shed:      name,
		ExitCode:  -1,
	}
	if s.cfg.AuditExecArgvFull() {
		e.Argv = append([]string(nil), argv...)
	} else if len(argv) > 0 {
		e.Argv0 = argv[0]
		e.Argc = len(argv)
	}
	return e
}
