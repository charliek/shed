package rc

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// Metadata is the write-once SHED_RC_* set stamped into a managed session at create.
type Metadata struct {
	ID          string
	DisplayName string
	Kind        Kind
	Workdir     string
	CreatedBy   string
	CreatedAt   string // RFC3339 UTC (…Z)
	Target      string // optional advisory label
	// Slug is the session's slug (the tmux name's suffix), stamped into SHED_RC_SLUG for
	// every kind so a process launched INSIDE the session can address it on the hub (see
	// envSlug). Set by Create from the same resolved slug it builds the tmux name from.
	Slug string
	// Port is opencode's allocated loopback SSE/HTTP server port (0 = none / not
	// opencode). Set by Create via freeLoopbackPort (netutil.go) BEFORE Metadata is
	// built, so BuildEnvArgs below can stamp it into the session env for the hub's
	// opencode watcher (opencodePortEnv, watch.go) to read back. Ignored by
	// BuildEnvArgs for non-opencode kinds even if nonzero.
	Port int
}

// envValue validates the single-line / no-control-char grammar and returns the
// value, or an error naming the offending key.
func envValue(key, value string) (string, error) {
	if HasControlChars(value) {
		return "", fmt.Errorf("%s must not contain control characters", key)
	}
	return value, nil
}

// BuildEnvArgs returns the `-e KEY=value …` argv fragment for `tmux new-session`,
// in deterministic order. SHED_RC_SLUG is stamped unconditionally for every kind (it is
// how a process running inside the session addresses that session on the hub — see
// envSlug); SHED_RC_TARGET is included only when set; SHED_RC_OPENCODE_PORT only when the
// kind is opencode and a port was allocated. Values are validated against the single-line
// grammar.
//
// For an opencode session it ALSO appends a bare `OPENCODE_SERVER_PASSWORD=` (empty
// value) launch-env override, regardless of whether a port was allocated — opencode's
// embedded HTTP server always runs when the bare TUI starts, and this neutralizes any
// OPENCODE_SERVER_PASSWORD an inherited shell rc file might otherwise set, so the
// hub's watcher (an unauthenticated second client) never hits a 401. This is
// deliberately NOT a SHED_RC_* key — it's a plain launch-env override for the opencode
// process itself, so it doesn't ride the SHED_RC_ prefix and is never read back by
// parseEnv/showEnvironment (which filter to that prefix): it can't pollute SHED_RC_
// metadata parsing.
func BuildEnvArgs(m Metadata) ([]string, error) {
	pairs := [][2]string{
		{envV, fmt.Sprintf("%d", SchemaVersion)},
		{envID, m.ID},
		{envDisplayName, m.DisplayName},
		{envKind, string(m.Kind)},
		{envWorkdir, m.Workdir},
		{envCreatedBy, m.CreatedBy},
		{envCreatedAt, m.CreatedAt},
		{envSlug, m.Slug},
	}
	if m.Target != "" {
		pairs = append(pairs, [2]string{envTarget, m.Target})
	}
	if m.Kind == KindOpencode && m.Port != 0 {
		pairs = append(pairs, [2]string{envOpencodePort, strconv.Itoa(m.Port)})
	}
	args := make([]string, 0, len(pairs)*2+2)
	for _, p := range pairs {
		v, err := envValue(p[0], p[1])
		if err != nil {
			return nil, err
		}
		args = append(args, "-e", p[0]+"="+v)
	}
	if m.Kind == KindOpencode {
		args = append(args, "-e", "OPENCODE_SERVER_PASSWORD=")
	}
	return args, nil
}

// parseEnv turns a `tmux show-environment` dump into SHED_RC_* key→value. tmux
// prints KEY=value for set vars and a bare -KEY for removed ones (skipped).
func parseEnv(dump string) map[string]string {
	out := map[string]string{}
	for _, line := range strings.Split(dump, "\n") {
		if !strings.HasPrefix(line, envPrefix) {
			continue
		}
		eq := strings.IndexByte(line, '=')
		if eq == -1 {
			continue
		}
		out[line[:eq]] = line[eq+1:]
	}
	return out
}

var rfc3339UTCRe = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(?:\.\d+)?Z$`)

func normalizeCreatedAt(raw string) string {
	if rfc3339UTCRe.MatchString(raw) {
		return raw
	}
	return ""
}

// ParseSession reconstructs one session's DTO from its tmux env dump + pane. State
// and url are derived from the pane (never stored), and lane from the kind's AgentSpec
// (laneForKind) — which is why lane is present on legacy/unmanaged and unknown-kind
// rows too. A session with no valid SHED_RC_V (>= MinManagedVersion) is
// legacy/unmanaged: kind defaults to
// claude-broker, display name to the fallback, stray SHED_RC_* values ignored.
// displayFallback receives the slug (e.g. "<shed>/<slug>").
func ParseSession(tmuxSession, envDump, pane string, displayFallback func(slug string) string) Session {
	env := parseEnv(envDump)
	slug := strings.TrimPrefix(tmuxSession, TmuxPrefix)
	// No fallback → leave display_name empty so it's OMITTED from the DTO and the
	// consuming app applies its own (target-aware) "<shed>/<slug>" fallback. The
	// binary, running inside the shed, doesn't know the orchestrator's shed alias.
	fallbackName := ""
	if displayFallback != nil {
		fallbackName = displayFallback(slug)
	}

	val := func(k string) string { return strings.TrimSpace(env[k]) }

	if !isManagedVersion(val(envV)) {
		kind := KindClaudeBroker
		state, url := ClassifyPane(kind, pane)
		return Session{
			Slug:        slug,
			TmuxSession: tmuxSession,
			Kind:        kind,
			State:       state,
			Lane:        laneForKind(kind),
			URL:         url,
			DisplayName: fallbackName,
			Managed:     false,
		}
	}

	kind := parseKind(val(envKind))
	state, url := ClassifyPane(kind, pane)
	name := val(envDisplayName)
	if name == "" {
		name = fallbackName
	}
	return Session{
		Slug:        slug,
		TmuxSession: tmuxSession,
		Kind:        kind,
		State:       state,
		Lane:        laneForKind(kind),
		URL:         url,
		DisplayName: name,
		Workdir:     val(envWorkdir),
		ID:          val(envID),
		CreatedBy:   val(envCreatedBy),
		CreatedAt:   normalizeCreatedAt(val(envCreatedAt)),
		TargetLabel: val(envTarget),
		Managed:     true,
	}
}
