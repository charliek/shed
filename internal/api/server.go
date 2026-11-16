// Package api provides the HTTP API server for shed.
package api

import (
	"sync"
	"time"

	"github.com/charliek/shed/internal/audit"
	"github.com/charliek/shed/internal/backend"
	"github.com/charliek/shed/internal/config"
	"github.com/charliek/shed/internal/egress"
	"github.com/charliek/shed/internal/plugin"
	"github.com/charliek/shed/internal/source"
	"github.com/charliek/shed/internal/users"
	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
)

// Server is the HTTP API server for shed.
type Server struct {
	// backend is always a *backend.Guard (or nil): every call is decided by
	// the access rule for the principal on the request's context.
	backend     backend.Backend
	cfg         *config.ServerConfig
	sshHostKey  string
	plugins     *plugin.Registry
	bridge      *plugin.Bridge
	egressAudit *egress.AuditLog         // nil when egress is disabled
	egressStore *config.UserProfileStore // nil when egress is disabled
	// users answers "which user owns this SSH key fingerprint?" for the mtls
	// middleware, which re-checks a presented client certificate's Subject CN
	// on every request. nil until SetUserLookup; nil authorizes nothing.
	users users.Lookup
	// keyRefresher fetches the GitHub logins again for POST
	// /api/users/refresh. nil until SetKeyRefresher.
	keyRefresher users.Refresher
	// caFingerprint / caNotAfter describe the internal client CA, reported on
	// /api/info in mtls mode so an operator can confirm which CA this server
	// trusts and when it must be rotated. Zero values in every other mode.
	caFingerprint string
	caNotAfter    time.Time

	// mux is the router Router() built, kept so the auth middleware (which
	// runs at the root, before routing) can resolve a service client's
	// request to its route pattern with Find. nil until Router() runs; a
	// client request with no mux is refused.
	mux *chi.Mux

	// clientMu guards clientCreates, the creates each service client has in
	// flight (client -> shed name -> count), counted against policy.max_sheds
	// with the sheds it owns.
	clientMu      sync.Mutex
	clientCreates map[string]map[string]int

	// audit is the audit log (nil: off). See audit.go.
	audit *audit.Log

	// reapSkipped records the owned sheds the reaper has already logged as
	// left alone (owner no longer configured), so each is logged once.
	reapSkipped map[string]bool

	// execMu guards execInFlight, the count of HTTPS execs running per shed
	// (exec.max_concurrent_per_shed). The map is created on first use.
	execMu       sync.Mutex
	execInFlight map[string]int

	// missingID logs a credential-bus request with no valid machine headers
	// once per caller (a released broker retries every few seconds).
	missingID missingIDLog
}

// SetSourceBindings gives the credential bus its source bindings (the pin):
// shorthand for the registry's SetBindings, which shed-server calls itself at
// start-up, before any listener can register. Without them the bus routes
// that subscribe and answer refuse every request (fail closed).
func (s *Server) SetSourceBindings(b source.Bindings) {
	if s.plugins != nil {
		s.plugins.SetBindings(b)
	}
}

// SetEgressAudit attaches the durable egress audit log so `shed egress show`
// can return recent decisions. Called by shed-server at startup when egress is
// enabled; nil leaves the egress routes reporting no recent activity.
func (s *Server) SetEgressAudit(a *egress.AuditLog) { s.egressAudit = a }

// SetEgressUserStore attaches the runtime user-profile store backing the
// `/api/egress/profiles` routes and merged into `shed egress show`. Called at
// startup when egress is enabled; nil leaves the profile routes reporting only
// config profiles (and PUT/DELETE returning 501).
func (s *Server) SetEgressUserStore(st *config.UserProfileStore) { s.egressStore = st }

// SetUserLookup attaches the user lookup the mtls middleware re-checks a
// client certificate's Subject CN (an SSH key fingerprint) against on every
// request. In shed-server this is the same users.ConfigLookup the SSH server
// asks. It MUST read live state — a snapshot would make certificates
// unrevokable. Called once at startup in mtls mode; leaving it nil makes the
// mtls path authorize nothing.
func (s *Server) SetUserLookup(l users.Lookup) { s.users = l }

// SetClientCAInfo records the internal client CA's fingerprint and expiry for
// /api/info reporting in mtls mode. Two scalars rather than the CA itself: the
// API server has no use for issuance.
func (s *Server) SetClientCAInfo(fingerprint string, notAfter time.Time) {
	s.caFingerprint = fingerprint
	s.caNotAfter = notAfter
}

// NewServer creates a new API server. Every backend call it makes goes
// through the access guard: b is wrapped in a backend.Guard unless it is one
// already (shed-server passes the one Guard the SSH server shares).
func NewServer(b backend.Backend, cfg *config.ServerConfig, sshHostKey string, plugins *plugin.Registry, bridge *plugin.Bridge) *Server {
	var guarded backend.Backend
	if b != nil {
		guarded = backend.Guarded(b)
	}
	return &Server{
		backend:    guarded,
		cfg:        cfg,
		sshHostKey: sshHostKey,
		plugins:    plugins,
		bridge:     bridge,
	}
}

// useCommonMiddleware installs the shared middleware stack. RealIP is
// applied only behind a trusted proxy (it trusts client X-Forwarded-For);
// otherwise the real TCP peer address is used so a source IP can't be
// forged to evade per-IP controls or poison audit logs.
func (s *Server) useCommonMiddleware(r chi.Router) {
	r.Use(middleware.RequestID)
	if s.cfg.TrustedProxy {
		// chi v5.5 deprecates RealIP for trusting client-supplied headers.
		// That trust is exactly what trusted_proxy opts into (off by
		// default, documented as safe only behind a proxy that overwrites
		// the headers), and the ClientIP* replacements leave RemoteAddr
		// untouched, which would change behaviour for existing configs.
		r.Use(middleware.RealIP) //nolint:staticcheck // SA1019: deliberate, gated by trusted_proxy
	}
	r.Use(middleware.Logger)
	r.Use(middleware.Recoverer)
	r.Use(ContentTypeJSON)
	r.Use(s.authMiddleware)
}

// Router returns the chi router for the HTTP API — served over pinned TLS in
// mtls mode, plain HTTP in open mode. It registers the full route surface on a
// single listener: the control plane (info, lifecycle, images, system,
// snapshots, sessions, egress) plus the credential bus (/api/plugins/*) and the
// Connect tunnel (/api/sheds/*/connect/*). In mtls mode the bus requires a
// credentials-scoped certificate, the Connect tunnel accepts control or
// credentials, and everything else requires control (see authMiddleware).
func (s *Server) Router() chi.Router {
	r := chi.NewRouter()
	s.mux = r
	s.useCommonMiddleware(r)

	r.Route("/api", func(r chi.Router) {
		// Server info and the SSH host key. Unauthenticated only in open
		// mode, where nothing is; mtls has no exemptions — see
		// authMiddleware.
		r.Get("/info", s.handleGetInfo)
		r.Get("/ssh-host-key", s.handleGetSSHHostKey)

		// Sessions (aggregate across all sheds)
		r.Get("/sessions", s.handleListAllSessions)

		// Single-call host snapshot: server identity + features, df, and every
		// shed with its sessions (control scope, GET-only — the auth
		// middleware's default branch requires control).
		r.Get("/overview", s.handleOverview)

		// Images
		r.Route("/images", func(r chi.Router) {
			r.Get("/", s.handleListImages)
			// Ref-keyed forms (?ref=) for slash-bearing Docker refs; see
			// imageIdent. The legacy /{name} + /inspect/{name} forms below
			// stay for slash-free identifiers from older clients.
			// A backend with no image store (remote) answers the routes
			// below 501 FEATURE_UNSUPPORTED; the list above still works.
			images := r.With(s.requireCapability("image management", capImageManagement))
			images.Delete("/", s.handleDeleteImage)
			images.Get("/inspect", s.handleInspectImage)
			images.Post("/prune", s.handlePruneImages)
			images.Post("/tag", s.handleTagImage)
			images.Post("/pull", s.handlePullImage)
			images.Post("/push", s.handlePushImage)
			images.Get("/inspect/{name}", s.handleInspectImage)
			// Wrap parametric /{name} in a sub-router so it doesn't shadow
			// literal sibling routes like /pull, /push, /prune, /tag at the
			// chi trie level (which would return 405 Method Not Allowed
			// instead of dispatching to the literal POST handler).
			r.Route("/{name}", func(r chi.Router) {
				r.With(s.requireCapability("image management", capImageManagement)).Delete("/", s.handleDeleteImage)
			})
		})

		// System (disk reporting + prune)
		r.Route("/system", func(r chi.Router) {
			r.Get("/df", s.handleSystemDF)
			r.Post("/prune", s.handleSystemPrune)
		})

		// Snapshots
		r.Route("/snapshots", func(r chi.Router) {
			r.Get("/", s.handleListSnapshots)
			snap := s.requireCapability("snapshots", capSnapshots)
			r.With(snap).Post("/", s.handleCreateSnapshot)
			// Same defensive wrap as /images — drift insurance if we ever
			// add literal sibling routes (e.g. /snapshots/prune).
			r.Route("/{name}", func(r chi.Router) {
				r.With(snap).Get("/", s.handleGetSnapshot)
				r.With(snap).Delete("/", s.handleDeleteSnapshot)
			})
		})

		// Egress control: the audit SSE stream (literal), user-profile CRUD
		// (literal /profiles), plus per-shed status + live set/off. The literal
		// /stream and /profiles siblings are registered before the /{name} wrap
		// so they aren't shadowed at the chi trie level — which also makes
		// "stream" and "profiles" reserved shed names on these routes.
		r.Route("/egress", func(r chi.Router) {
			r.Get("/stream", s.handleEgressStream)
			r.Get("/profiles", s.handleListProfiles)
			egressWrite := s.requireCapability("egress control", capEgressProfiles)
			r.Route("/profiles/{name}", func(r chi.Router) {
				r.Get("/", s.handleGetProfile)
				r.With(egressWrite).Put("/", s.handlePutProfile)
				r.With(egressWrite).Delete("/", s.handleDeleteProfile)
			})
			r.Route("/{name}", func(r chi.Router) {
				r.Get("/", s.handleEgressShow)
				r.With(egressWrite).Post("/", s.handleEgressSet)
				r.With(egressWrite).Delete("/", s.handleEgressOff)
			})
		})

		// Users: fetch the GitHub logins again (admin only; control scope).
		r.Post("/users/refresh", s.handleUsersRefresh)

		// Credential sources: which of a user's machines answers their
		// credential requests, and moving that (control scope: outside
		// /api/plugins, so a credentials certificate cannot reach them).
		r.Get("/sources", s.handleListSources)
		r.Post("/sources/use", s.handleUseSource)

		// Plugins / Extensions (the credential bus)
		r.Route("/plugins", func(r chi.Router) {
			r.Get("/listeners", s.handleListListeners)
			r.Get("/listeners/{namespace}/messages", s.handlePluginSubscribe)
			r.Post("/listeners/{namespace}/respond", s.handlePluginRespond)
			r.Get("/sheds", s.handleListPluginSheds)
		})

		// Sheds: lifecycle routes (control scope) plus the Connect tunnel leaf
		// (control or credentials) — all on the single listener.
		r.Route("/sheds", func(r chi.Router) {
			r.Get("/", s.handleListSheds)
			r.Post("/", s.handleCreateShed)
			r.Route("/{name}", func(r chi.Router) {
				r.Get("/", s.handleGetShed)
				r.Delete("/", s.handleDeleteShed)
				r.Post("/start", s.handleStartShed)
				r.Post("/stop", s.handleStopShed)
				// Give a shed with no user to a user (admin only).
				r.Post("/assign", s.handleAssignShed)
				r.With(s.requireCapability("reset", capReset)).Post("/reset", s.handleResetShed)

				// HTTPS exec: one command, NDJSON output (control scope
				// in an enforced mode, via authorizeScope's default).
				r.Post("/exec", s.handleExecShed)

				// Sessions within a shed
				r.Get("/sessions", s.handleListSessions)
				r.Delete("/sessions/{session}", s.handleKillSession)

				// Connect API: TCP tunnel via HTTP upgrade
				r.Get("/connect/{port}", s.handleConnect)
			})
		})
	})

	return r
}
