package roostprovider

import (
	"fmt"
	"net/url"
	"strings"
)

// Row-id token keys (plan 019 §3.2, last bullets). Two host shapes —
// `shed`+`server`, or `machine` — and then the wizard's accumulated choices.
const (
	keyShed    = "shed"
	keyServer  = "server"
	keyMachine = "machine"
	keyAgent   = "agent"
	keyHome    = "home"
	keyCwd     = "cwd"
	keyProject = "project"
)

// field maps a row-id key onto the Token field that holds it, or nil for a key
// this build does not know.
//
// ONE enumeration of the key set, not two. The obvious spelling is a
// `map[string]bool` of known keys beside a switch that assigns them, and that
// pair fails OPEN: a key added to the map but not to the switch validates and
// is then silently dropped, which is exactly the half-decoded token — a tab
// opened somewhere other than where the user pointed — that ParseToken's
// strictness exists to prevent. Returning the destination makes "is this key
// known" and "where does it go" the same question, so they cannot disagree.
func (t *Token) field(key string) *string {
	switch key {
	case keyShed:
		return &t.Shed
	case keyServer:
		return &t.Server
	case keyMachine:
		return &t.Machine
	case keyAgent:
		return &t.Agent
	case keyHome:
		return &t.Home
	case keyCwd:
		return &t.Cwd
	case keyProject:
		return &t.Project
	}
	return nil
}

// NoneID is the id every non-actionable row carries (plan 019 §3.2). roost
// never activates a row with `actionable:false`, so the id is only ever seen
// by a human reading debug output — but it has to be SOMETHING, and one
// reserved value that can never parse as a Token is what keeps a stale or
// hand-typed `_none` from being mistaken for a host.
const NoneID = "_none"

// Token is a row id: the wizard's whole state, carried in the one string roost
// hands back as `ROOST_SELECTED_ID`.
//
// There is no server-side session — roost runs `shed roost-provider activate`
// as a fresh process per step (activate is recursive; each sub-menu's rows are
// activated in turn), so every step has to reconstruct where it is from the id
// alone. That is the whole reason this type exists.
//
// Exactly one host shape is set: Shed+Server, or Machine. The remaining fields
// accumulate as the wizard descends — Agent and Home at step 2, Cwd (and
// Project, when the workdir came from a far-side project) at step 3.
type Token struct {
	// Shed and Server name a running shed: the shed's own name, and the
	// `servers:` entry name it was found on (the config.yaml map key, which is
	// commonly not the host).
	Shed   string
	Server string
	// Machine names a `machines:` entry.
	Machine string
	// Agent is an RC kind string (Agent.Kind), e.g. `claude-rc`.
	Agent string
	// Home is the far side's ABSOLUTE $HOME, as the probe reported it. It
	// travels in the id because step 3 builds the "Home" workdir candidate
	// from it without re-probing, and because `tab.open` does not expand `~`
	// (verified in plan 019 §2) — so the absolute string is the only usable
	// form.
	Home string
	// Cwd is the chosen workdir, always absolute. Empty means "not chosen
	// yet", which is exactly what NextStep branches on.
	Cwd string
	// Project is a far-side project id when the workdir came from one, empty
	// when it did not (Home, or a shed's landing dir). Empty becomes `"0"` on
	// the `tab.open` wire, which reuses the first project or creates
	// `Default` — either is correct because Cwd is explicit.
	Project string
}

// Encode renders the token as a row id.
//
// url.Values.Encode, which percent-encodes every value and SORTS the keys. The
// sort is why plan 019 §3.2's illustrative ids (`shed=<name>&server=<server>`,
// `<host>&agent=<kind>&home=<abs home>`) do not appear literally: the plan pins
// the CODEC ("row ids are url.Values-encoded tokens") and the key set, and a
// sorted encoding is what makes an id stable — the same token always renders to
// the same bytes, whatever order the fields were filled in.
//
// Empty fields are omitted rather than encoded as `key=`, so a token's id names
// only the choices actually made.
func (t Token) Encode() string {
	v := url.Values{}
	set := func(key, val string) {
		if val != "" {
			v.Set(key, val)
		}
	}
	set(keyShed, t.Shed)
	set(keyServer, t.Server)
	set(keyMachine, t.Machine)
	set(keyAgent, t.Agent)
	set(keyHome, t.Home)
	set(keyCwd, t.Cwd)
	set(keyProject, t.Project)
	return v.Encode()
}

// ParseToken decodes a row id back into a Token.
//
// Strict in all three directions a malformed id can be wrong: an unknown key,
// a repeated key, and an empty value are each an error rather than a silently
// dropped field. A row id is never typed by a human — it is one this process
// emitted and roost handed straight back — so anything unexpected means the
// contract broke, and continuing with a half-decoded token would open a tab
// somewhere other than where the user pointed.
//
// NoneID fails here by construction (`_none` parses as an unknown key), which
// is the belt to roost's braces: roost will not activate a non-actionable row,
// and if it ever did, this refuses rather than guessing.
func ParseToken(id string) (Token, error) {
	values, err := url.ParseQuery(id)
	if err != nil {
		return Token{}, fmt.Errorf("row id %q is not a valid token: %w", id, err)
	}
	var t Token
	for key, vals := range values {
		// The key is judged BEFORE its value, so an id carrying a key this
		// build does not know says so — rather than reporting whatever
		// happens to be wrong with its value. `_none` is the case that makes
		// the order visible: `url.ParseQuery` reads it as the key `_none` with
		// an empty value, and "unknown key" is the true diagnosis.
		dest := t.field(key)
		if dest == nil {
			return Token{}, fmt.Errorf("row id %q carries an unknown key %q", id, key)
		}
		if len(vals) != 1 {
			return Token{}, fmt.Errorf("row id %q repeats the %q key", id, key)
		}
		if vals[0] == "" {
			return Token{}, fmt.Errorf("row id %q has an empty %q value", id, key)
		}
		*dest = vals[0]
	}
	if err := t.validateHost(); err != nil {
		return Token{}, fmt.Errorf("row id %q: %w", id, err)
	}
	return t, nil
}

// validateHost enforces the one invariant every token shape shares: exactly one
// host, fully named. A `shed=` with no `server=` is not "a shed on the default
// server" — there is no default, the same shed name can exist on two servers,
// and guessing would ssh to the wrong machine.
func (t Token) validateHost() error {
	isShed := t.Shed != "" || t.Server != ""
	switch {
	case isShed && t.Machine != "":
		return fmt.Errorf("names both a shed and a machine")
	case isShed && (t.Shed == "" || t.Server == ""):
		return fmt.Errorf("a shed needs both %q and %q", keyShed, keyServer)
	case !isShed && t.Machine == "":
		return fmt.Errorf("names neither a shed nor a machine")
	}
	return nil
}

// IsShed reports whether this token names a shed (rather than a `machines:`
// entry). Only meaningful on a token that came through ParseToken or one of the
// builders — both of which have already enforced validateHost.
func (t Token) IsShed() bool { return t.Shed != "" }

// Host returns just the host keys, dropping every wizard choice.
//
// This is what a step's rows are built FROM: step 2's agent rows each extend
// the host token, so the id the user came in on cannot smuggle a stale `cwd=`
// into the next step.
func (t Token) Host() Token {
	return Token{Shed: t.Shed, Server: t.Server, Machine: t.Machine}
}

// HostLabel is the human name for this host in the menu's non-actionable copy
// ("`<host>` is unreachable", "roost-session is not installed on `<host>`", …).
//
// `<server>/<shed>` and the bare machine name, NOT plan 019 §0's target grammar
// (`roost:<server>/<shed>` / `machine:<name>`). The grammar is pinned for
// machine-readable targets — IPC ops, status rows, capabilities keys, logs —
// and reads badly in the one sentence it would land in here ("roost-session on
// roost:srv/dev is not installed"). The two are never confused because nothing
// in this package emits an id a human reads; §0's grammar has no consumer in
// the provider.
func (t Token) HostLabel() string {
	if t.IsShed() {
		return t.Server + "/" + t.Shed
	}
	return t.Machine
}

// joinNames renders a name list for a subtitle. Empty renders as "none", not as
// the empty string, because a blank subtitle under "no shed-server answered"
// reads as a rendering bug rather than as "you have no servers configured".
func joinNames(names []string) string {
	if len(names) == 0 {
		return "none"
	}
	return strings.Join(names, ", ")
}
