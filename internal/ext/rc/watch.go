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

// The JSONL watchers are the structured-signal source that OVERRIDES the pane
// stability engine for codex and claude sessions: instead of inferring activity from
// whether the tmux pane keeps redrawing, they tail the agent's own append-only log
// (codex rollout / claude transcript) and read the turn/tool structure directly. The
// hub merges the two per session (see hub_reconcile.go): a fresh, correlated watcher
// wins; a broken/absent one falls back to stability so activity never goes dark.
//
// Layout of the watcher stack:
//   - lineTailer (watch_tail.go): resilient byte-level tailing.
//   - activityFold (below): a per-kind fold of the parsed line stream into an
//     activity verdict + last-message preview (codexFold, claudeFold).
//   - fileWatcher (below): tailer + fold + a freshness-annotated snapshot.
//   - correlation (below + the per-kind files): mapping a tmux session to its file.
//   - fsNudger (below): the fsnotify layer that wakes reconcile sub-tick on a write.

// watcherFreshWindow bounds how long a correlated watcher's non-settled, non-working
// activity is trusted after its last folded event. A settled verdict (needs_input/
// idle) stays authoritative indefinitely — a quiet file is exactly what a waiting
// agent produces — so in practice this window governs only transitional verdicts.
const watcherFreshWindow = 30 * time.Second

// watcherWorkingGrace is the DELIBERATELY LONGER quiet tolerance for a working
// verdict: a long tool call or model turn can legitimately write nothing to the JSONL
// for tens of seconds, and flipping to stability at 30s would flap a mid-turn session.
// The asymmetry with watcherFreshWindow is intentional: needs_input/idle keep the 30s
// rule (they are settled anyway), working gets 120s — and even past 120s, working only
// yields to stability when stability itself holds a SETTLED quiet verdict (idle/
// needs_input after its quiet period); if the pane still churns, working is kept (see
// mergedActivity).
const watcherWorkingGrace = 120 * time.Second

// correlateWindow is the ±tolerance around a session's created-at within which a
// candidate JSONL file's own creation time must fall to be a match.
const correlateWindow = 60 * time.Second

// activityFold folds a kind's parsed JSONL line stream into a live activity verdict.
// Implementations hold cumulative state across applyLine calls (turn boundaries,
// pending tool calls, the last message) and are NOT safe for concurrent use — the
// owning fileWatcher serializes access.
type activityFold interface {
	// applyLine folds one raw JSONL line, returning true when it advanced meaningful
	// state (an activity-relevant event). Irrelevant/unparseable lines return false
	// and leave state untouched (tolerant parsing).
	applyLine(line []byte) bool
	// reset clears all state (the tailer reported a truncation/rotation).
	reset()
	// noteGap tells the fold a record was LOST mid-stream (the tailer skipped an
	// oversized line). Any state that depends on having seen every record — pending
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

// messageProducer is an activityFold that ALSO produces a normalized message feed
// (codex only; claude feeds activity only in this phase). The fileWatcher drains it on
// each refresh; a fold that does not implement it contributes no feed messages.
//
// Ambiguous correlation caveat (accepted): a watcher attached on an AMBIGUOUS window
// match is follow-only and its ACTIVITY stays untrusted (unknown) until an in-file
// event confirms the pick — but new appends it folds before that confirmation do
// reach the session's message ring. Worst case the ring briefly carries a few
// messages from the same user's other same-cwd session; the ring is per-session,
// same-trust content, and a confirmed-wrong pick is torn down with the watcher.
type messageProducer interface {
	drainMessages() []feedMessage
}

// sessionWatcher is the narrow surface the reconcile loop and the input handler need
// from a per-session watcher: refresh it, read its current verdict, drain any feed
// messages it produced, and check whether it has ever folded an event. *fileWatcher
// (below) satisfies this interface structurally — no other change is required for it
// to be used through the interface. The seam exists so a second, network/SSE-backed
// watcher (an opencode session's event stream, added later) can plug into the same
// reconcile/input-handler call sites: both hub_reconcile.go and hub.go hold the
// per-session watcher as a sessionWatcher and call only these five methods, so
// reconcile is transport-agnostic between a tailed JSONL file and a live SSE feed.
type sessionWatcher interface {
	// refresh polls for new state and updates the watcher's current verdict. now
	// stamps the last-event time used by the freshness decision (see snapshot).
	refresh(now time.Time)
	// snapshot reports the watcher's activity + message and its authority at now; see
	// (*fileWatcher).snapshot for the fresh/expiredWorking contract reconcile relies on.
	snapshot(now time.Time) (activity Activity, message string, fresh, expiredWorking bool)
	// drainPending returns and clears the feed messages produced since the last drain.
	drainPending() []feedMessage
	// hadEvent reports whether the watcher has folded at least one activity-relevant
	// event since it was created (used to confirm an ambiguous correlation).
	hadEvent() bool
	// close releases the watcher's resources and marks it terminally closed.
	close()
}

// fileWatcher pairs a tailer with a fold and tracks freshness for the reconcile merge.
type fileWatcher struct {
	tailer *lineTailer

	mu          sync.Mutex
	fold        activityFold
	lastEventAt time.Time
	curActivity Activity
	curMessage  string
	curSettled  bool
	pending     []feedMessage // feed messages produced since the last drainPending
	closed      bool          // terminal: refresh no-ops after close (see close)
}

// var _ sessionWatcher = (*fileWatcher)(nil) is a compile-time check that fileWatcher's
// method set has not drifted from the interface reconcile/the input handler depend on.
var _ sessionWatcher = (*fileWatcher)(nil)

func newFileWatcher(path string, catchUp bool, fold activityFold) *fileWatcher {
	return &fileWatcher{
		tailer: &lineTailer{path: path, catchUp: catchUp},
		fold:   fold,
	}
}

// refresh polls the file and folds any new lines. A reset from the tailer clears the
// fold; a poll error (permission/transient) is swallowed so the prior verdict is
// retained. now stamps the last-event time used by the freshness decision. A CLOSED
// watcher no-ops: the tailer released its file handle on close, and a poll would
// silently reopen the path from offset 0 — a full re-read (and a leaked handle) that
// refolds a dead incarnation's history into a watcher that is already discarded.
func (w *fileWatcher) refresh(now time.Time) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return
	}
	lines, didReset, gapped, err := w.tailer.poll()
	if didReset {
		w.fold.reset()
	}
	if gapped {
		// A record was lost (oversized skip): drop record-exact state (pending tool
		// calls) so a swallowed *_output line can't pin the verdict at working forever.
		w.fold.noteGap()
	}
	if err != nil {
		return
	}
	for _, ln := range lines {
		if w.fold.applyLine(ln) {
			w.lastEventAt = now
		}
	}
	w.curActivity = w.fold.activity()
	w.curMessage = w.fold.lastMessage()
	w.curSettled = w.fold.settled()
	// Drain any feed messages the fold produced this poll into the watcher's pending
	// queue; reconcile empties it into the session ring (see drainPending).
	if mp, ok := w.fold.(messageProducer); ok {
		w.pending = append(w.pending, mp.drainMessages()...)
	}
}

// drainPending returns and clears the feed messages produced since the last drain (in
// stream order). reconcile appends these to the session's message ring.
func (w *fileWatcher) drainPending() []feedMessage {
	w.mu.Lock()
	defer w.mu.Unlock()
	if len(w.pending) == 0 {
		return nil
	}
	out := w.pending
	w.pending = nil
	return out
}

// snapshot reports the watcher's activity + message and its authority at now:
//
//   - fresh: the verdict is authoritative outright — settled (needs_input/idle;
//     trusted indefinitely, the 30s/quiet rule is theirs by construction), recent
//     (last event within watcherFreshWindow), or working within watcherWorkingGrace.
//   - expiredWorking: a working verdict whose file has been quiet past the grace —
//     not discarded, but demoted to conditional: the merge lets stability take over
//     only if stability holds a settled quiet verdict (see mergedActivity).
//
// An empty/unknown verdict is never fresh.
func (w *fileWatcher) snapshot(now time.Time) (activity Activity, message string, fresh, expiredWorking bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.curActivity == "" || w.curActivity == ActivityUnknown {
		return w.curActivity, w.curMessage, false, false
	}
	sinceEvent := time.Duration(-1)
	if !w.lastEventAt.IsZero() {
		sinceEvent = now.Sub(w.lastEventAt)
	}
	recent := sinceEvent >= 0 && sinceEvent < watcherFreshWindow
	workingGrace := w.curActivity == ActivityWorking && sinceEvent >= 0 && sinceEvent < watcherWorkingGrace
	fresh = w.curSettled || recent || workingGrace
	expiredWorking = w.curActivity == ActivityWorking && !fresh
	return w.curActivity, w.curMessage, fresh, expiredWorking
}

// hadEvent reports whether the fold has consumed at least one activity-relevant event
// since attach. Used to confirm an AMBIGUOUS correlation before its session id is
// back-written: an in-file event after attach is the plan's "first in-file event
// confirms" signal (the watcher is follow-only on the ambiguous path, so any folded
// event necessarily happened after this session was created).
func (w *fileWatcher) hadEvent() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return !w.lastEventAt.IsZero()
}

// close releases the tailer's file handle and marks the watcher terminally closed —
// any later refresh (e.g. an input handler holding a stale pointer) is a no-op rather
// than a from-zero reopen. Idempotent.
func (w *fileWatcher) close() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.closed = true
	w.tailer.close()
}

// mergedActivity resolves the reconcile precedence:
//
//   - a FRESH watcher verdict (and its last-message) wins outright;
//   - an EXPIRED-WORKING verdict (working, file quiet past the grace) yields to
//     stability only when stability holds a settled quiet verdict (idle/needs_input —
//     the pane genuinely stopped); if the pane still churns (stability=working) or
//     stability has no verdict, working is KEPT — a long silent turn must not flap;
//   - otherwise the pane-stability activity drives and last-message is dropped
//     (stability has no message signal).
//
// Returned activity is still subject to DisplayActivity (lifecycle-trumps) by the
// caller.
func mergedActivity(watcherActivity Activity, watcherMessage string, watcherFresh, watcherExpiredWorking bool, stability Activity) (activity Activity, message string) {
	if watcherFresh {
		return watcherActivity, watcherMessage
	}
	if watcherExpiredWorking {
		if stability == ActivityIdle || stability == ActivityNeedsInput {
			return stability, ""
		}
		return watcherActivity, watcherMessage
	}
	return stability, ""
}

// watchableKind reports whether a kind has a JSONL watcher (codex rollout / claude
// transcript). Other kinds derive activity from pane stability alone.
func watchableKind(k Kind) bool {
	return k == KindCodex || IsClaudeKind(k)
}

// correlation is the outcome of mapping a tmux session to its agent JSONL file.
type correlation struct {
	path      string // the chosen file
	sessionID string // the agent's own session id (back-written into the tmux env)
	ambiguous bool   // >1 candidate in the window → newest chosen, treat history as untrusted
}

// jsonlPeek is the correlation metadata read from an agent JSONL file's early lines
// (codex rollout session_meta / claude transcript header). Both per-kind peek parsers
// return it so the newest-pick + ambiguity logic below is shared.
type jsonlPeek struct {
	sessionID string
	cwd       string
	createdAt time.Time
	hasTime   bool
}

// peekCandidate pairs a JSONL file with its peeked correlation metadata.
type peekCandidate struct {
	path string
	peek jsonlPeek
}

// peekNewer reports whether candidate a is newer than b by peeked created-at (window
// candidates always carry one — no-timestamp files are excluded from window matching
// by the correlate functions). nameTiebreak breaks an exact created-at tie by
// filename; only codex passes true (rollout names are timestamp-prefixed, so lexical
// order is chronological) — claude transcript names are bare UUIDs, where a filename
// comparison would be meaningless.
func peekNewer(a peekCandidate, b peekCandidate, nameTiebreak bool) bool {
	if !a.peek.createdAt.Equal(b.peek.createdAt) {
		return a.peek.createdAt.After(b.peek.createdAt)
	}
	if nameTiebreak {
		return filepath.Base(a.path) > filepath.Base(b.path)
	}
	return false
}

// pickCorrelation returns the correlation for the newest of matches, flagging ambiguity
// when more than one candidate survived the caller's window filter (history untrusted).
// matches must be non-empty; see peekNewer for nameTiebreak.
func pickCorrelation(matches []peekCandidate, nameTiebreak bool) correlation {
	best := 0
	for i := 1; i < len(matches); i++ {
		if peekNewer(matches[i], matches[best], nameTiebreak) {
			best = i
		}
	}
	return correlation{
		path:      matches[best].path,
		sessionID: matches[best].peek.sessionID,
		ambiguous: len(matches) > 1,
	}
}

// withinWindow reports whether a and b are within w of each other.
func withinWindow(a, b time.Time, w time.Duration) bool {
	d := a.Sub(b)
	if d < 0 {
		d = -d
	}
	return d <= w
}

// parseJSONLTime parses an RFC3339(nano) timestamp, ok=false on empty/invalid.
func parseJSONLTime(s string) (time.Time, bool) {
	if s == "" {
		return time.Time{}, false
	}
	if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
		return t, true
	}
	return time.Time{}, false
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

// listJSONLUnder walks root and returns every *.jsonl path, tolerating per-directory
// permission errors (a skipped subdir does not abort the walk). match filters basenames.
func listJSONLUnder(root string, match func(base string) bool) []string {
	var out []string
	_ = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil // permission/transient on this entry → skip it, keep walking
		}
		if d.IsDir() {
			return nil
		}
		base := d.Name()
		if filepath.Ext(base) == ".jsonl" && (match == nil || match(base)) {
			out = append(out, path)
		}
		return nil
	})
	return out
}

// ---- fsnotify nudge layer ----

// fsNudger watches the codex + claude root trees and pings a channel whenever a file
// changes, so the hub can run a reconcile sub-tick (activity surfaces promptly instead
// of waiting up to the active interval). It is a best-effort LATENCY optimization: the
// reconcile tick already refreshes every watcher, so a missed notification only delays
// a transition to the next tick. fsnotify is non-recursive, so directories are added
// as they appear (codex's dated YYYY/MM/DD subdirs, or the whole ~/.codex tree on a
// fresh shed).
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
				// A new dated subdir (or the sessions/projects dir itself) — start
				// watching it so its files' writes are seen.
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
