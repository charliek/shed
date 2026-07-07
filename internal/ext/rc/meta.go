package rc

import (
	"fmt"
	"regexp"
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
// in deterministic order. SHED_RC_TARGET is included only when set. Values are
// validated against the single-line grammar.
func BuildEnvArgs(m Metadata) ([]string, error) {
	pairs := [][2]string{
		{envV, fmt.Sprintf("%d", SchemaVersion)},
		{envID, m.ID},
		{envDisplayName, m.DisplayName},
		{envKind, string(m.Kind)},
		{envWorkdir, m.Workdir},
		{envCreatedBy, m.CreatedBy},
		{envCreatedAt, m.CreatedAt},
	}
	if m.Target != "" {
		pairs = append(pairs, [2]string{envTarget, m.Target})
	}
	args := make([]string, 0, len(pairs)*2)
	for _, p := range pairs {
		v, err := envValue(p[0], p[1])
		if err != nil {
			return nil, err
		}
		args = append(args, "-e", p[0]+"="+v)
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
// and url are derived from the pane (never stored). A session with no valid
// SHED_RC_V (>= MinManagedVersion) is legacy/unmanaged: kind defaults to
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
