package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/charliek/shed/internal/config"
)

// rcTmuxPrefix is the session-name prefix that marks a Remote Control session
// under the RC Session Convention (e.g. "rc-abc234").
const rcTmuxPrefix = "rc-"

// rcEnrichTimeout bounds a single `shed-ext-rc list` SSH round-trip so a slow or
// unreachable shed never stalls `shed sessions`.
const rcEnrichTimeout = 6 * time.Second

// rcEnrichConcurrency caps concurrent SSH enrichment calls so `sessions --all`
// across many sheds doesn't fan out unbounded.
const rcEnrichConcurrency = 6

// rcSessionDTO mirrors shed-ext-rc's neutral `list` DTO (one entry of the
// `{"rc_sessions":[...]}` payload). It is decoded verbatim from the binary's
// stdout so the shed CLI stays aligned with the cross-repo golden fixture
// (shed-extensions internal/rc + shed-remote-agent); fields beyond those we
// display are accepted and ignored.
type rcSessionDTO struct {
	Slug        string `json:"slug"`
	TmuxSession string `json:"tmux_session"`
	Kind        string `json:"kind"`
	State       string `json:"state"`
	Managed     bool   `json:"managed"`
	DisplayName string `json:"display_name"`
	URL         string `json:"url"`
	CreatedBy   string `json:"created_by"`
}

// rcListResponse is the `shed-ext-rc list` stdout shape.
type rcListResponse struct {
	RCSessions []rcSessionDTO `json:"rc_sessions"`
}

// toSessionRC projects the decoded DTO onto the display type carried on
// config.Session (the decode type is kept separate so it stays aligned with the
// shed-ext-rc golden contract independent of presentation).
func (d rcSessionDTO) toSessionRC() *config.SessionRC {
	return &config.SessionRC{
		Kind:        d.Kind,
		State:       d.State,
		Managed:     d.Managed,
		DisplayName: d.DisplayName,
		URL:         d.URL,
		CreatedBy:   d.CreatedBy,
	}
}

// parseRcList decodes `shed-ext-rc list` stdout into a map keyed by tmux session
// name (e.g. "rc-abc234") for O(1) merge onto enumerated tmux sessions.
func parseRcList(stdout []byte) (map[string]rcSessionDTO, error) {
	var resp rcListResponse
	if err := json.Unmarshal(stdout, &resp); err != nil {
		return nil, fmt.Errorf("decoding shed-ext-rc list: %w", err)
	}
	out := make(map[string]rcSessionDTO, len(resp.RCSessions))
	for _, s := range resp.RCSessions {
		if s.TmuxSession != "" {
			out[s.TmuxSession] = s
		}
	}
	return out, nil
}

// sshCaptureArgs builds the `ssh` argv for a non-interactive, output-capturing
// command against a shed (no PTY; BatchMode so a key/auth issue fails fast
// instead of hanging). The shed name is the SSH user; the host/port come from
// the server entry; host-key checking uses shed's pinned known_hosts.
func sshCaptureArgs(shedName string, entry *config.ServerEntry, remoteArgv ...string) []string {
	args := []string{
		"-p", strconv.Itoa(entry.SSHPort),
		"-o", "UserKnownHostsFile=" + config.GetKnownHostsPath(),
		"-o", "StrictHostKeyChecking=yes",
		"-o", "BatchMode=yes",
		"-o", "ConnectTimeout=10",
		shedName + "@" + entry.Host,
		"--",
	}
	return append(args, remoteArgv...)
}

// rcListOverSSH runs `shed-ext-rc list` in the shed and returns the parsed
// sessions keyed by tmux name. A missing binary, transport failure, or non-zero
// exit is reported as an error for the caller to treat as "no RC data".
func rcListOverSSH(ctx context.Context, shedName string, entry *config.ServerEntry) (map[string]rcSessionDTO, error) {
	args := sshCaptureArgs(shedName, entry, "shed-ext-rc", "list")
	cmd := exec.CommandContext(ctx, "ssh", args...)
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("shed-ext-rc list on %s: %w", shedName, err)
	}
	return parseRcList(out)
}

// rcShedKey identifies a shed by server + name (a shed name is unique only
// within a server, so the aggregated `--all` listing must key by both).
type rcShedKey struct{ server, shed string }

// enrichSessionsRC fills the RC field of every "rc-*" session in sessions by
// querying the in-shed shed-ext-rc binary. It runs one `shed-ext-rc list` per
// distinct (server, shed) that actually has an rc-* session — bounded by
// rcEnrichConcurrency and time-limited per call — resolving each shed's SSH
// endpoint from the client config by the session's ServerName. It mutates
// sessions in place and degrades silently (rows keep their plain tmux data) when
// a shed is unreachable, lacks the binary, or its server is unknown. Non-RC
// listings touch nothing and never dial. Call once, after the full session list
// (with ServerName populated) is assembled.
func enrichSessionsRC(sessions []config.Session) {
	if clientConfig == nil {
		return // config not loaded (e.g. a direct test caller); nothing to resolve
	}
	idxByShed := make(map[rcShedKey][]int)
	for i := range sessions {
		if strings.HasPrefix(sessions[i].Name, rcTmuxPrefix) {
			k := rcShedKey{sessions[i].ServerName, sessions[i].ShedName}
			idxByShed[k] = append(idxByShed[k], i)
		}
	}
	if len(idxByShed) == 0 {
		return
	}

	sem := make(chan struct{}, rcEnrichConcurrency)
	var wg sync.WaitGroup
	for key, idxs := range idxByShed {
		entry, ok := clientConfig.Servers[key.server]
		if !ok {
			continue // unknown server -> leave rows un-enriched
		}
		wg.Add(1)
		go func(key rcShedKey, idxs []int, entry config.ServerEntry) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			ctx, cancel := context.WithTimeout(context.Background(), rcEnrichTimeout)
			defer cancel()
			byTmux, err := rcListOverSSH(ctx, key.shed, &entry)
			if err != nil {
				if verboseLevel > 0 {
					fmt.Fprintf(os.Stderr, "Warning: RC metadata unavailable for %s: %v\n", key.shed, err)
				}
				return
			}
			// Distinct indices per (server, shed) -> safe concurrent writes.
			for _, i := range idxs {
				if dto, ok := byTmux[sessions[i].Name]; ok {
					sessions[i].RC = dto.toSessionRC()
				}
			}
		}(key, idxs, entry)
	}
	wg.Wait()
}
