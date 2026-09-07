package rc

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"
)

// The structured-signal watchers are the source that OVERRIDES the pane stability engine
// for the kinds that have one: instead of inferring activity from whether the tmux pane
// keeps redrawing, they read the agent's own turn/tool structure directly.
//
// opencode is the one such kind today: its sessionWatcher (opencodeWatcher,
// watch_opencode_transport.go) subscribes to the agent's embedded HTTP+SSE server (see
// watchableKind below). The hub merges a session's watcher with pane stability per
// session (see hub_reconcile.go): a fresh, correlated watcher wins; a broken/absent one
// falls back to stability so activity never goes dark.
//
// The codex JSONL tail, the cursor hook-ingest push lane and the shared line tailer they
// both sat on were removed with A6 (charliek/shed#322) — roost is the status authority
// for those kinds now, and they remain launchable, attachable TUI kinds with no derived
// lane.
//
// Layout of the watcher stack:
//   - activityFold (below): a per-kind fold of the parsed event stream into an
//     activity verdict + last-message preview (opencodeFold).
//   - opencodeWatcher (watch_opencode_transport.go): SSE/REST client + fold + a
//     freshness-annotated snapshot (opencode's sessionWatcher).
//   - fsNudger (below): the fsnotify layer that wakes reconcile sub-tick on a write. It
//     has no roots left now that no kind tails a file; it is retained as the seam the
//     hub still constructs (opencode's SSE stream is its own arrival signal).

// watcherFreshWindow bounds how long a correlated watcher's non-settled, non-working
// activity is trusted after its last folded event. A settled verdict (needs_input/
// idle) stays authoritative indefinitely — a quiet file is exactly what a waiting
// agent produces — so in practice this window governs only transitional verdicts.
const watcherFreshWindow = 30 * time.Second

// watcherWorkingGrace is the DELIBERATELY LONGER quiet tolerance for a working
// verdict: a long tool call or model turn can legitimately produce no event for tens
// of seconds, and dropping the verdict at 30s would flap a mid-turn session. The
// asymmetry with watcherFreshWindow is intentional: needs_input/idle keep the 30s rule
// (they are settled anyway), working gets 120s. Past 120s the verdict simply stops
// being fresh and the session's activity goes to *unknown* — S2 (charliek/shed#324)
// removed the pane-stability fallback that used to catch it (see mergedActivity).
const watcherWorkingGrace = 120 * time.Second

// activityFold folds a kind's parsed line stream into a live activity verdict.
// Implementations hold cumulative state across applyLine calls (turn boundaries,
// pending tool calls, the last message) and are NOT safe for concurrent use — the
// owning watcher serializes access.
type activityFold interface {
	// applyLine folds one raw JSONL line, returning true when it advanced meaningful
	// state (an activity-relevant event). Irrelevant/unparseable lines return false
	// and leave state untouched (tolerant parsing).
	applyLine(line []byte) bool
	// reset clears all state (the source reported a truncation/restart).
	reset()
	// noteGap tells the fold a record was LOST mid-stream (the source skipped an
	// oversized record). Any state that depends on having seen every record — pending
	// tool-call ids awaiting their output — must be dropped, leaving the verdict to
	// coarser signals (turn boundaries) until the next turn re-establishes it.
	noteGap()
	// activity is the current verdict: ActivityUnknown until a confirming event.
	activity() Activity
	// lastMessage is a sanitized preview of the most recent agent message ("" if none).
	lastMessage() string
	// settled reports the verdict is a terminal waiting state (needs_input/idle) —
	// authoritative even when the file has gone quiet.
	settled() bool
}

// messageProducer is a fold that ALSO produces a normalized message feed (opencode
// today). Every watcher drains it on each refresh; a fold that does not implement it
// contributes no feed messages. It is declared separately from activityFold, and
// asserted separately, so a fold can produce a feed without being an activityFold.
type messageProducer interface {
	drainMessages() []feedMessage
}

// sessionWatcher is the narrow surface the reconcile loop needs from a per-session
// watcher: refresh it, read its current verdict, drain any feed messages it produced,
// and check whether it has ever folded an event. *opencodeWatcher
// (watch_opencode_transport.go) satisfies it structurally. The seam is transport-
// agnostic on purpose — it outlived the tailed-JSONL watchers it was introduced
// alongside (A6, charliek/shed#322) and is what a future lane plugs into.
type sessionWatcher interface {
	// refresh polls for new state and updates the watcher's current verdict. now
	// stamps the last-event time used by the freshness decision (see snapshot).
	refresh(now time.Time)
	// snapshot reports the watcher's activity + message and its authority at now; see
	// watcherFreshness for the fresh/expiredWorking contract reconcile relies on.
	snapshot(now time.Time) (activity Activity, message string, fresh, expiredWorking bool)
	// drainPending returns and clears the feed messages produced since the last drain.
	drainPending() []feedMessage
	// hadEvent reports whether the watcher has folded at least one activity-relevant
	// event since it was created (used to confirm an ambiguous correlation).
	hadEvent() bool
	// close releases the watcher's resources and marks it terminally closed.
	close()
}

// watcherFreshness is THE quiet-source freshness rule, shared verbatim by every watcher
// that has one (opencodeWatcher once its transport is healthy). Given a verdict,
// whether it is settled, and when the source last produced an event, it reports the
// verdict's authority at now:
//
//   - fresh: authoritative outright — settled (needs_input/idle; trusted indefinitely,
//     the 30s/quiet rule is theirs by construction), recent (last event within
//     watcherFreshWindow), or working within watcherWorkingGrace.
//   - expiredWorking: a working verdict whose source has been quiet past the grace.
//     Since S2 (charliek/shed#324) the merge treats it exactly like any other
//     non-fresh verdict — there is no pane-stability fallback left for it to be
//     weighed against — so it is reported for the watchers' own bookkeeping and
//     asserted by their tests, not consulted by mergedActivity.
//
// An empty/unknown verdict is never fresh. A zero lastEventAt means "nothing folded
// yet", which is neither recent nor within the grace.
func watcherFreshness(activity Activity, settled bool, lastEventAt, now time.Time) (fresh, expiredWorking bool) {
	if activity == "" || activity == ActivityUnknown {
		return false, false
	}
	sinceEvent := time.Duration(-1)
	if !lastEventAt.IsZero() {
		sinceEvent = now.Sub(lastEventAt)
	}
	recent := sinceEvent >= 0 && sinceEvent < watcherFreshWindow
	workingGrace := activity == ActivityWorking && sinceEvent >= 0 && sinceEvent < watcherWorkingGrace
	fresh = settled || recent || workingGrace
	expiredWorking = activity == ActivityWorking && !fresh
	return fresh, expiredWorking
}

// mergedActivity resolves the reconcile precedence, which S2 (charliek/shed#324)
// reduced to two arms:
//
//   - a FRESH watcher verdict (and its last-message) wins;
//   - EVERYTHING ELSE — no watcher, a closed or unhealthy transport, a stale verdict,
//     an EXPIRED-WORKING one — yields ("", ""), i.e. NO activity, which the DTO omits.
//
// The expired-working arm went with the pane-stability engine it consulted (its
// expiry clock WAS that engine's quiet period). The consequence is deliberate: an
// opencode row whose SSE feed dies mid-turn goes to *unknown* rather than sitting at
// `working` forever, because nothing is left that can observe the turn end.
//
// Returned activity is still subject to DisplayActivity (lifecycle-trumps) by the
// caller.
func mergedActivity(watcherActivity Activity, watcherMessage string, watcherFresh bool) (activity Activity, message string) {
	if watcherFresh {
		return watcherActivity, watcherMessage
	}
	return "", ""
}

// watchableKind reports whether a kind has a structured-signal watcher. opencode is the
// only one: it subscribes to its embedded HTTP+SSE server
// (watch_opencode_transport.go). Every other kind derives activity from pane stability
// alone — A6 (charliek/shed#322) retired the codex rollout tail and the cursor
// hook-ingest lane, and A5 (charliek/shed#321) the claude transcript tail before it.
func watchableKind(k Kind) bool {
	return k == KindOpencode
}

// agentSessionEnv reads the back-written SHED_RC_AGENT_SESSION for a tmux session
// ("" when absent). It rides showEnvironment's SHED_RC_ filter.
func agentSessionEnv(r Runner, tmuxName string) string {
	return parseEnv(showEnvironment(r, tmuxName))[envAgentSession]
}

// opencodePortEnv reads the create-time SHED_RC_OPENCODE_PORT for a tmux session
// (stamped by BuildEnvArgs, meta.go) and range-validates it: a missing key, a value
// that doesn't parse as an integer, or one outside 1..65535 all report ok=false — the
// session is unwatchable over the opencode SSE transport (a pre-upgrade session
// created before this port plumbing shipped simply never had the key stamped, which
// is exactly this "missing" case; see the design doc's "pre-upgrade sessions" note).
// Mirrors agentSessionEnv's shape (same showEnvironment/parseEnv path) but returns an
// (int, bool) instead of a "" sentinel since 0 is not itself an invalid port value in
// general — the explicit bool avoids overloading a magic int.
func opencodePortEnv(r Runner, tmuxName string) (int, bool) {
	raw := parseEnv(showEnvironment(r, tmuxName))[envOpencodePort]
	port, err := strconv.Atoi(raw)
	if err != nil || port < 1 || port > 65535 {
		return 0, false
	}
	return port, true
}

// backWriteAgentSession stamps SHED_RC_AGENT_SESSION into the tmux session env so a
// hub restart re-correlates exactly. Best-effort: a set-environment failure is
// swallowed (the window heuristic re-runs next time). Control-char-guarded like every
// other SHED_RC_ value.
func backWriteAgentSession(r Runner, tmuxName, id string) {
	if id == "" || HasControlChars(id) {
		return
	}
	_ = r.Run("set-environment", "-t", tmuxName, envAgentSession, id)
}

// ---- fsnotify nudge layer ----

// fsNudger watches a set of root trees and pings a channel whenever a file changes, so
// the hub can run a reconcile sub-tick (activity surfaces promptly instead of waiting
// up to the active interval). It is a best-effort LATENCY optimization: the reconcile
// tick already refreshes every watcher, so a missed notification only delays a
// transition to the next tick. fsnotify is non-recursive, so directories are added as
// they appear.
//
// DORMANT BY DESIGN, NOT AN OVERSIGHT: no kind tails a file since A6
// (charliek/shed#322), so the hub builds it over an EMPTY root set and the tick is the
// sole driver — the tests are its only live exercise. It is kept for the next
// file-backed lane and retires with the hub in S6 if none arrives first (see
// startFSNudger).
type fsNudger struct {
	w     *fsnotify.Watcher
	nudge chan struct{}
	logf  func(string, ...any)
	roots []string

	mu    sync.Mutex
	added map[string]bool
}

// newFSNudger builds a nudger over the given roots. It never fails the caller: if
// fsnotify is unavailable, run() returns immediately and the reconcile tick is the
// sole driver.
func newFSNudger(roots []string, logf func(string, ...any)) (*fsNudger, error) {
	w, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, err
	}
	return &fsNudger{
		w:     w,
		nudge: make(chan struct{}, 1),
		logf:  logf,
		roots: roots,
		added: map[string]bool{},
	}, nil
}

// addTree adds a watch on dir and every existing subdirectory (fsnotify is
// non-recursive). Missing dirs and permission errors are ignored — a dir that appears
// later is picked up by the Create handler in run().
func (n *fsNudger) addTree(dir string) {
	_ = filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			n.addDir(path)
		}
		return nil
	})
}

func (n *fsNudger) addDir(path string) {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.added[path] {
		return
	}
	// Record only on a SUCCESSFUL add — a failed add must stay forgettable so a later
	// retry (e.g. after the dir becomes readable) can go through.
	if err := n.w.Add(path); err != nil {
		return
	}
	n.added[path] = true
}

// forgetDir drops path (and everything under it) from the added set when the dir is
// removed or renamed away — fsnotify silently drops the kernel watch for a deleted
// dir, so without this a recreated dir at the same path would be skipped by addDir's
// dedupe and its writes would nudge nothing until the next full tick.
func (n *fsNudger) forgetDir(path string) {
	n.mu.Lock()
	defer n.mu.Unlock()
	delete(n.added, path)
	prefix := path + string(filepath.Separator)
	for p := range n.added {
		if strings.HasPrefix(p, prefix) {
			delete(n.added, p)
		}
	}
}

// run watches until ctx is canceled, sending a (coalesced) nudge on any event and
// adding watches on newly-created directories. Always closes the fsnotify watcher.
func (n *fsNudger) run(ctx context.Context) {
	defer n.w.Close()
	for _, r := range n.roots {
		n.addTree(r)
	}
	for {
		select {
		case <-ctx.Done():
			return
		case ev, ok := <-n.w.Events:
			if !ok {
				return
			}
			if ev.Op&fsnotify.Create != 0 {
				// A new subdirectory under a watched root — start watching it so its
				// files' writes are seen.
				if fi, err := os.Stat(ev.Name); err == nil && fi.IsDir() {
					n.addTree(ev.Name)
				}
			}
			if ev.Op&(fsnotify.Remove|fsnotify.Rename) != 0 {
				// The path is gone: forget it so a recreation at the same path can be
				// re-added (addDir dedupes on the added set).
				n.forgetDir(ev.Name)
			}
			n.signal()
		case err, ok := <-n.w.Errors:
			if !ok {
				return
			}
			if n.logf != nil {
				n.logf("rc hub: fsnotify error: %v", err)
			}
		}
	}
}

// signal delivers a non-blocking nudge (coalesced: a pending nudge absorbs bursts).
func (n *fsNudger) signal() {
	select {
	case n.nudge <- struct{}{}:
	default:
	}
}
