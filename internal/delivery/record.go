package delivery

import (
	"encoding/json"
)

// State is a shed's delivery state.
type State string

const (
	StateNone          State = "none"
	StateNotApplicable State = "not_applicable"
	StateDelivered     State = "delivered"
	StatePartial       State = "partial"
	StateNotDelivered  State = "not_delivered"
)

// States is every State, in the contract's order.
var States = []State{StateNone, StateNotApplicable, StateDelivered, StatePartial, StateNotDelivered}

// StepState is the state of one tracked step.
type StepState string

const (
	StepDone    StepState = "done"
	StepSkipped StepState = "skipped"
	StepFailed  StepState = "failed"
	StepOwed    StepState = "owed"
)

// StepStates is every StepState, in the contract's order.
var StepStates = []StepState{StepDone, StepSkipped, StepFailed, StepOwed}

// The tracked steps, in the one order they run.
const (
	StepStage1  = "stage1"
	StepClone   = "clone"
	StepStage2  = "stage2"
	StepInstall = "install"
)

// StepNames is every step, in the order they run.
var StepNames = []string{StepStage1, StepClone, StepStage2, StepInstall}

// StepReason says why a step was skipped or failed, or why a delivery
// failed.
type StepReason string

const (
	ReasonNotApplicable      StepReason = "not_applicable"
	ReasonFromSnapshot       StepReason = "from_snapshot"
	ReasonAdopted            StepReason = "adopted"
	ReasonNoItems            StepReason = "no_items"
	ReasonNoRepo             StepReason = "no_repo"
	ReasonLocalDir           StepReason = "local_dir"
	ReasonNoProjectDir       StepReason = "no_project_dir"
	ReasonNoHook             StepReason = "no_hook"
	ReasonNoProvision        StepReason = "no_provision"
	ReasonSourceOffline      StepReason = "source_offline"
	ReasonSourceTimeout      StepReason = "source_timeout"
	ReasonSourceDisconnected StepReason = "source_disconnected"
	// ReasonSourceRefused is a hosted source that refused the server or its
	// request (contract section 3b); a machine source never does.
	ReasonSourceRefused    StepReason = "source_refused"
	ReasonAnswerRefused    StepReason = "answer_refused"
	ReasonWriteFailed      StepReason = "write_failed"
	ReasonImageTooOld      StepReason = "image_too_old"
	ReasonCloneFailed      StepReason = "clone_failed"
	ReasonProjectDirExists StepReason = "project_dir_exists"
	ReasonHookFailed       StepReason = "hook_failed"
)

// StepReasons is every StepReason, in the contract's order.
var StepReasons = []StepReason{
	ReasonNotApplicable, ReasonFromSnapshot, ReasonAdopted, ReasonNoItems,
	ReasonNoRepo, ReasonLocalDir, ReasonNoProjectDir, ReasonNoHook, ReasonNoProvision,
	ReasonSourceOffline, ReasonSourceTimeout, ReasonSourceDisconnected, ReasonSourceRefused,
	ReasonAnswerRefused, ReasonWriteFailed, ReasonImageTooOld,
	ReasonCloneFailed, ReasonProjectDirExists, ReasonHookFailed,
}

// skipReasons and failReasons are the reasons each step may carry when
// skipped and when failed.
var (
	skipReasons = map[string][]StepReason{
		StepStage1:  {ReasonNotApplicable, ReasonNoItems, ReasonImageTooOld, ReasonFromSnapshot, ReasonAdopted},
		StepClone:   {ReasonNoRepo, ReasonLocalDir, ReasonFromSnapshot, ReasonAdopted},
		StepStage2:  {ReasonNotApplicable, ReasonNoItems, ReasonNoProjectDir, ReasonLocalDir, ReasonImageTooOld, ReasonFromSnapshot, ReasonAdopted},
		StepInstall: {ReasonNoHook, ReasonNoProvision, ReasonFromSnapshot, ReasonAdopted},
	}
	failReasons = map[string][]StepReason{
		StepStage1:  {ReasonSourceOffline, ReasonSourceTimeout, ReasonSourceDisconnected, ReasonSourceRefused, ReasonAnswerRefused, ReasonWriteFailed},
		StepClone:   {ReasonCloneFailed, ReasonProjectDirExists},
		StepStage2:  {ReasonWriteFailed},
		StepInstall: {ReasonHookFailed},
	}
)

// FailureReasons are the reasons a delivery fails, as the warning names
// them.
var FailureReasons = []StepReason{
	ReasonSourceOffline, ReasonSourceTimeout, ReasonSourceDisconnected, ReasonSourceRefused,
	ReasonAnswerRefused, ReasonWriteFailed, ReasonImageTooOld,
}

// Outcome is what became of the last delivery's request.
type Outcome string

const (
	OutcomeAccepted     Outcome = "accepted"
	OutcomeRefused      Outcome = "refused"
	OutcomeNoAnswer     Outcome = "no_answer"
	OutcomeNotRequested Outcome = "not_requested"
)

// Outcomes is every Outcome, in the contract's order.
var Outcomes = []Outcome{OutcomeAccepted, OutcomeRefused, OutcomeNoAnswer, OutcomeNotRequested}

// ItemResult is what became of one item in the shed. The first seven are
// the writer's (WriterResults); the last two are the record's own.
type ItemResult string

const (
	ResultOK              ItemResult = "ok"
	ResultIsDirectory     ItemResult = "is_directory"
	ResultSymlink         ItemResult = "symlink"
	ResultNotDirectory    ItemResult = "not_directory"
	ResultPermission      ItemResult = "permission"
	ResultOtherFilesystem ItemResult = "other_filesystem"
	ResultIO              ItemResult = "io"
	ResultNotWritten      ItemResult = "not_written"
	ResultNotInLastAnswer ItemResult = "not_in_last_answer"
)

// WriterResults is the closed list the writer reports per item.
var WriterResults = []ItemResult{ResultOK, ResultIsDirectory, ResultSymlink, ResultNotDirectory, ResultPermission, ResultOtherFilesystem, ResultIO}

// ItemResults is every ItemResult, in the contract's order.
var ItemResults = []ItemResult{
	ResultOK, ResultIsDirectory, ResultSymlink, ResultNotDirectory, ResultPermission, ResultOtherFilesystem, ResultIO,
	ResultNotWritten, ResultNotInLastAnswer,
}

// Record is a shed's delivery record. A shed with no record (created before
// delivery existed) is reported as {"state": "none"}.
type Record struct {
	State State        `json:"state"`
	Steps *Steps       `json:"steps,omitempty"`
	Last  *Last        `json:"last,omitempty"`
	Items []RecordItem `json:"items,omitempty"`
}

// Steps is the state of each tracked step.
type Steps struct {
	Stage1  Step `json:"stage1"`
	Clone   Step `json:"clone"`
	Stage2  Step `json:"stage2"`
	Install Step `json:"install"`
}

// Step is one step's state and, when it was skipped or failed, why.
type Step struct {
	State  StepState  `json:"state"`
	Reason StepReason `json:"reason,omitempty"`
}

// Last is the last delivery: when, why, and what became of it.
type Last struct {
	At       string    `json:"at"`
	Reason   Reason    `json:"reason"`
	Outcome  Outcome   `json:"outcome"`
	Failure  *Failure  `json:"failure,omitempty"`
	Source   *Source   `json:"source,omitempty"`
	Problems []Problem `json:"problems,omitempty"`
}

// Failure is why the last delivery failed: the reason, with the rule an
// answer broke, or the item a write failed at and its result. A write
// failure with no item is the env file's own.
type Failure struct {
	Reason StepReason `json:"reason"`
	Rule   Rule       `json:"rule,omitempty"`
	Item   string     `json:"item,omitempty"`
	Result ItemResult `json:"result,omitempty"`
}

// SourceMachine is a machine source's identity: the machine a request
// went to.
type SourceMachine struct {
	MachineID   string `json:"machine_id"`
	MachineName string `json:"machine_name"`
}

// SourceKind is the kind of source a record names.
type SourceKind string

const (
	// SourceKindMachine is a machine source (contract section 3a).
	SourceKindMachine SourceKind = "machine"
	// SourceKindService is a hosted source, the account service (section 3b).
	SourceKindService SourceKind = "service"
)

// Source is the source a record says was asked: its kind and, for a
// machine, the machine's id and name.
type Source struct {
	Kind        SourceKind `json:"kind"`
	MachineID   string     `json:"machine_id,omitempty"`
	MachineName string     `json:"machine_name,omitempty"`
}

// Record is the record's form of a machine source.
func (m SourceMachine) Record() *Source {
	return &Source{Kind: SourceKindMachine, MachineID: m.MachineID, MachineName: m.MachineName}
}

// ServiceSource is the record's form of a hosted source.
func ServiceSource() *Source { return &Source{Kind: SourceKindService} }

// RecordItem is one delivered item as the record keeps it: never its
// content or value.
type RecordItem struct {
	Name        string     `json:"name"`
	Provider    string     `json:"provider"`
	Stage       int        `json:"stage"`
	Kind        Kind       `json:"kind"`
	Base        Base       `json:"base,omitempty"`
	Target      string     `json:"target"`
	Fingerprint string     `json:"fingerprint"`
	ExpiresAt   string     `json:"expires_at,omitempty"`
	WrittenAt   string     `json:"written_at,omitempty"`
	Result      ItemResult `json:"result"`
}

// list returns the steps by name, in order.
func (s *Steps) list() []struct {
	name string
	step Step
} {
	return []struct {
		name string
		step Step
	}{{StepStage1, s.Stage1}, {StepClone, s.Clone}, {StepStage2, s.Stage2}, {StepInstall, s.Install}}
}

// DeriveState is the state the contract gives a record, from its steps, its
// last delivery and its items, in order:
//
//   - none: no steps (the shed has no record), or every step skipped as
//     adopted;
//   - not_applicable: the last delivery made no request and did not fail;
//   - delivered: every step done or skipped, the last delivery recorded no
//     failure, and the last answer reported no problem (an image too old
//     for delivery skips both stages with a failure: not delivered);
//   - partial: an item was written by the last delivery (result ok);
//   - not_delivered: otherwise.
func DeriveState(r *Record) State {
	if r.Steps == nil {
		return StateNone
	}
	adopted, settled := true, true
	for _, s := range r.Steps.list() {
		if s.step.State != StepSkipped || s.step.Reason != ReasonAdopted {
			adopted = false
		}
		if s.step.State != StepDone && s.step.State != StepSkipped {
			settled = false
		}
	}
	if adopted {
		return StateNone
	}
	if r.Last != nil && r.Last.Outcome == OutcomeNotRequested && r.Last.Failure == nil {
		return StateNotApplicable
	}
	if settled && (r.Last == nil || (r.Last.Failure == nil && len(r.Last.Problems) == 0)) {
		return StateDelivered
	}
	for _, it := range r.Items {
		if it.Result == ResultOK {
			return StatePartial
		}
	}
	return StateNotDelivered
}

// The rules a record can break beyond the answer's (which it reuses where
// they fit: the document rules, unknown_member, missing_member, wrong_type,
// bad_time, bad_name, bad_provider, bad_kind, bad_stage, bad_base,
// project_base_stage, the path rules, refused_target, bad_variable,
// refused_variable, bad_fingerprint, too_many_problems, bad_problem_code,
// bad_problem_message, duplicate_target). They do not travel: a record is
// the server's, and these say why one is not a record.
const (
	RuleUnexpectedMember Rule = "unexpected_member"
	RuleEmptyList        Rule = "empty_list"
	RuleBadState         Rule = "bad_state"
	RuleBadStepState     Rule = "bad_step_state"
	RuleBadReason        Rule = "bad_reason"
	RuleBadLastReason    Rule = "bad_last_reason"
	RuleBadOutcome       Rule = "bad_outcome"
	RuleBadFailure       Rule = "bad_failure"
	RuleBadRule          Rule = "bad_rule"
	RuleBadSource        Rule = "bad_source"
	RuleBadResult        Rule = "bad_result"
	RuleStateMismatch    Rule = "state_mismatch"
)

// RecordRules is every rule only a record can break, in the contract's
// order.
var RecordRules = []Rule{
	RuleUnexpectedMember, RuleEmptyList, RuleBadState, RuleBadStepState, RuleBadReason,
	RuleBadLastReason, RuleBadOutcome, RuleBadFailure, RuleBadRule, RuleBadSource,
	RuleBadResult, RuleStateMismatch,
}

var (
	recordMembers  = []string{"state", "steps", "last", "items"}
	recordKinds    = map[string]nodeKind{"state": kindString, "steps": kindObject, "last": kindObject, "items": kindArray}
	stepKinds      = map[string]nodeKind{StepStage1: kindObject, StepClone: kindObject, StepStage2: kindObject, StepInstall: kindObject}
	stepMembers    = []string{"state", "reason"}
	stepKinds2     = map[string]nodeKind{"state": kindString, "reason": kindString}
	lastMembers    = []string{"at", "reason", "outcome", "failure", "source", "problems"}
	lastRequired   = []string{"at", "reason", "outcome"}
	lastKinds      = map[string]nodeKind{"at": kindString, "reason": kindString, "outcome": kindString, "failure": kindObject, "source": kindObject, "problems": kindArray}
	failureMembers = []string{"reason", "rule", "item", "result"}
	failureKinds   = map[string]nodeKind{"reason": kindString, "rule": kindString, "item": kindString, "result": kindString}
	sourceMembers  = []string{"kind", "machine_id", "machine_name"}
	sourceKinds    = map[string]nodeKind{"kind": kindString, "machine_id": kindString, "machine_name": kindString}
	rItemMembers   = []string{"name", "provider", "stage", "kind", "base", "target", "fingerprint", "expires_at", "written_at", "result"}
	rItemRequired  = []string{"name", "provider", "stage", "kind", "target", "fingerprint", "result"}
	rItemKinds     = map[string]nodeKind{"name": kindString, "provider": kindString, "stage": kindNumber, "kind": kindString, "base": kindString, "target": kindString, "fingerprint": kindString, "expires_at": kindString, "written_at": kindString, "result": kindString}
)

// ParseRecord decodes a record and holds it to the contract, on the parsed
// document (so an absent member and an empty one are told apart): the
// document rules, then each object's members (unknown, missing, types),
// then what each member must be and when it must be present, then the state
// against DeriveState. It returns the record or the first rule broken.
func ParseRecord(body []byte) (*Record, *Refusal) {
	root, r := parseDocument(body)
	if r != nil {
		return nil, r
	}
	if r := checkObject(root, "", recordMembers, []string{"state"}, recordKinds); r != nil {
		return nil, r
	}
	rec := &Record{State: State(root.get("state").text)}
	if !containsT(States, rec.State) {
		return nil, refuse(RuleBadState, "state")
	}
	sn, ln, in := root.get("steps"), root.get("last"), root.get("items")
	if sn == nil {
		switch {
		case ln != nil:
			return nil, refuse(RuleUnexpectedMember, "last")
		case in != nil:
			return nil, refuse(RuleUnexpectedMember, "items")
		}
	} else {
		if r := checkObject(sn, "steps", StepNames, StepNames, stepKinds); r != nil {
			return nil, r
		}
		rec.Steps = &Steps{}
		targets := []*Step{&rec.Steps.Stage1, &rec.Steps.Clone, &rec.Steps.Stage2, &rec.Steps.Install}
		for i, name := range StepNames {
			st, r := parseStep(sn.get(name), "steps."+name, name)
			if r != nil {
				return nil, r
			}
			*targets[i] = st
		}
	}
	if ln != nil {
		last, r := parseLast(ln)
		if r != nil {
			return nil, r
		}
		rec.Last = last
	}
	if in != nil {
		if len(in.elems) == 0 {
			return nil, refuse(RuleEmptyList, "items")
		}
		seen := map[string]bool{}
		for i, n := range in.elems {
			where := index("items", i)
			it, r := parseRecordItem(n, where)
			if r != nil {
				return nil, r
			}
			key := string(it.Kind) + "\x00" + string(it.Base) + "\x00" + it.Target
			if seen[key] {
				return nil, refuse(RuleDuplicateTarget, where)
			}
			seen[key] = true
			rec.Items = append(rec.Items, it)
		}
	}
	if rec.State != DeriveState(rec) {
		return nil, refuse(RuleStateMismatch, "state")
	}
	return rec, nil
}

// Validate holds a record the server made to the contract: it is encoded
// and parsed as any reader would parse it.
func (r *Record) Validate() error {
	b, err := json.Marshal(r)
	if err != nil {
		return err
	}
	if _, refusal := ParseRecord(b); refusal != nil {
		return refusal
	}
	return nil
}

// checkObject checks n is an object with no member outside allowed, every
// member of required, and each member of its JSON type.
func checkObject(n *node, where string, allowed, required []string, kinds map[string]nodeKind) *Refusal {
	if n.kind != kindObject {
		return refuse(RuleWrongType, where)
	}
	if r := checkMembers(n, where, allowed, required); r != nil {
		return r
	}
	return checkTypes(n, where, allowed, kinds)
}

// present reports whether n has the member name.
func present(n *node, name string) bool { return n.get(name) != nil }

// presence checks a conditional member: it must be present exactly when
// want holds.
func presence(n *node, where, name string, want bool) *Refusal {
	switch has := present(n, name); {
	case want && !has:
		return refuse(RuleMissingMember, join(where, name))
	case !want && has:
		return refuse(RuleUnexpectedMember, join(where, name))
	}
	return nil
}

func parseStep(n *node, where, name string) (Step, *Refusal) {
	var s Step
	if r := checkObject(n, where, stepMembers, []string{"state"}, stepKinds2); r != nil {
		return s, r
	}
	s.State = StepState(n.get("state").text)
	if !containsT(StepStates, s.State) {
		return s, refuse(RuleBadStepState, join(where, "state"))
	}
	if r := presence(n, where, "reason", s.State == StepSkipped || s.State == StepFailed); r != nil {
		return s, r
	}
	if !present(n, "reason") {
		return s, nil
	}
	s.Reason = StepReason(n.get("reason").text)
	allowed := failReasons[name]
	if s.State == StepSkipped {
		allowed = skipReasons[name]
	}
	if !containsT(allowed, s.Reason) {
		return s, refuse(RuleBadReason, join(where, "reason"))
	}
	return s, nil
}

func parseLast(n *node) (*Last, *Refusal) {
	const where = "last"
	if r := checkObject(n, where, lastMembers, lastRequired, lastKinds); r != nil {
		return nil, r
	}
	l := &Last{At: n.get("at").text, Reason: Reason(n.get("reason").text), Outcome: Outcome(n.get("outcome").text)}
	if !ValidTime(l.At) {
		return nil, refuse(RuleBadTime, "last.at")
	}
	if !ValidReason(l.Reason) {
		return nil, refuse(RuleBadLastReason, "last.reason")
	}
	var allowed []StepReason
	switch l.Outcome {
	case OutcomeAccepted:
		allowed = []StepReason{ReasonWriteFailed, ReasonImageTooOld}
	case OutcomeRefused:
		allowed = []StepReason{ReasonAnswerRefused}
	case OutcomeNoAnswer:
		allowed = []StepReason{ReasonSourceOffline, ReasonSourceTimeout, ReasonSourceDisconnected, ReasonSourceRefused}
	case OutcomeNotRequested:
		allowed = []StepReason{ReasonImageTooOld}
	default:
		return nil, refuse(RuleBadOutcome, "last.outcome")
	}

	// failure: required when the request was refused or not answered.
	if l.Outcome == OutcomeRefused || l.Outcome == OutcomeNoAnswer {
		if r := presence(n, where, "failure", true); r != nil {
			return nil, r
		}
	}
	if fn := n.get("failure"); fn != nil {
		const fw = "last.failure"
		if r := checkObject(fn, fw, failureMembers, []string{"reason"}, failureKinds); r != nil {
			return nil, r
		}
		f := &Failure{Reason: StepReason(fn.get("reason").text)}
		if !containsT(allowed, f.Reason) {
			return nil, refuse(RuleBadFailure, fw+".reason")
		}
		if r := presence(fn, fw, "rule", f.Reason == ReasonAnswerRefused); r != nil {
			return nil, r
		}
		isWrite := f.Reason == ReasonWriteFailed
		// item only with write_failed, and optional then: absent, the
		// failure is the env file's own, no item's.
		if !isWrite {
			if r := presence(fn, fw, "item", false); r != nil {
				return nil, r
			}
		}
		if r := presence(fn, fw, "result", isWrite); r != nil {
			return nil, r
		}
		if rn := fn.get("rule"); rn != nil {
			f.Rule = Rule(rn.text)
			if !ValidRule(f.Rule) {
				return nil, refuse(RuleBadRule, fw+".rule")
			}
		}
		if isWrite {
			f.Result = ItemResult(fn.get("result").text)
			if in := fn.get("item"); in != nil {
				f.Item = in.text
				if !ValidName(f.Item) {
					return nil, refuse(RuleBadName, fw+".item")
				}
			}
			if f.Result == ResultOK || !containsT(WriterResults, f.Result) {
				return nil, refuse(RuleBadResult, fw+".result")
			}
		}
		l.Failure = f
	}

	// source, by the outcome (the contract's table "Outcomes"): required
	// when an answer came, and with source_refused (only a hosted source
	// refuses); absent when no request was made; otherwise optional (a
	// machine source as in version 1, a hosted one whenever it was chosen).
	refusedBySource := l.Failure != nil && l.Failure.Reason == ReasonSourceRefused
	switch {
	case l.Outcome == OutcomeAccepted || l.Outcome == OutcomeRefused || refusedBySource:
		if r := presence(n, where, "source", true); r != nil {
			return nil, r
		}
	case l.Outcome == OutcomeNotRequested:
		if r := presence(n, where, "source", false); r != nil {
			return nil, r
		}
	}
	if sn := n.get("source"); sn != nil {
		const sw = "last.source"
		if r := checkObject(sn, sw, sourceMembers, []string{"kind"}, sourceKinds); r != nil {
			return nil, r
		}
		s := &Source{Kind: SourceKind(sn.get("kind").text)}
		if s.Kind != SourceKindMachine && s.Kind != SourceKindService {
			return nil, refuse(RuleBadSource, sw+".kind")
		}
		if refusedBySource && s.Kind != SourceKindService {
			return nil, refuse(RuleBadSource, sw+".kind")
		}
		isMachine := s.Kind == SourceKindMachine
		if r := presence(sn, sw, "machine_id", isMachine); r != nil {
			return nil, r
		}
		if r := presence(sn, sw, "machine_name", isMachine); r != nil {
			return nil, r
		}
		if isMachine {
			s.MachineID, s.MachineName = sn.get("machine_id").text, sn.get("machine_name").text
			if !ValidID(s.MachineID) {
				return nil, refuse(RuleBadSource, sw+".machine_id")
			}
			if s.MachineName == "" || len(s.MachineName) > 63 || hasControl(s.MachineName) {
				return nil, refuse(RuleBadSource, sw+".machine_name")
			}
		}
		l.Source = s
	}

	// problems: only with an accepted answer, and never an empty list.
	if pn := n.get("problems"); pn != nil {
		if l.Outcome != OutcomeAccepted {
			return nil, refuse(RuleUnexpectedMember, "last.problems")
		}
		if len(pn.elems) == 0 {
			return nil, refuse(RuleEmptyList, "last.problems")
		}
		if len(pn.elems) > MaxProblems {
			return nil, refuse(RuleTooManyProblems, "last.problems")
		}
		for i, e := range pn.elems {
			p, r := parseProblem(e, index("last.problems", i))
			if r != nil {
				return nil, r
			}
			l.Problems = append(l.Problems, p)
		}
	}
	return l, nil
}

func parseRecordItem(n *node, where string) (RecordItem, *Refusal) {
	var it RecordItem
	if r := checkObject(n, where, rItemMembers, rItemRequired, rItemKinds); r != nil {
		return it, r
	}
	str := func(name string) string {
		if v := n.get(name); v != nil {
			return v.text
		}
		return ""
	}
	it.Name, it.Provider = str("name"), str("provider")
	if !ValidName(it.Name) {
		return it, refuse(RuleBadName, join(where, "name"))
	}
	if !ValidName(it.Provider) {
		return it, refuse(RuleBadProvider, join(where, "provider"))
	}
	switch str("stage") {
	case "1":
		it.Stage = 1
	case "2":
		it.Stage = 2
	default:
		return it, refuse(RuleBadStage, join(where, "stage"))
	}
	it.Kind = Kind(str("kind"))
	if it.Kind != KindFile && it.Kind != KindEnv {
		return it, refuse(RuleBadKind, join(where, "kind"))
	}
	if r := presence(n, where, "base", it.Kind == KindFile); r != nil {
		return it, r
	}
	it.Target = str("target")
	switch it.Kind {
	case KindFile:
		it.Base = Base(str("base"))
		if it.Base != BaseHome && it.Base != BaseProject {
			return it, refuse(RuleBadBase, join(where, "base"))
		}
		if it.Base == BaseProject && it.Stage != 2 {
			return it, refuse(RuleProjectBaseStage, join(where, "base"))
		}
		if rule := CheckPath(it.Target); rule != "" {
			return it, refuse(rule, join(where, "target"))
		}
		if it.Base == BaseHome && RefusedHomeTarget(it.Target) {
			return it, refuse(RuleRefusedTarget, join(where, "target"))
		}
	case KindEnv:
		if it.Stage != 1 {
			return it, refuse(RuleBadStage, join(where, "stage"))
		}
		if !variableRe.MatchString(it.Target) {
			return it, refuse(RuleBadVariable, join(where, "target"))
		}
		if RefusedVariable(it.Target) {
			return it, refuse(RuleRefusedVariable, join(where, "target"))
		}
	}
	it.Fingerprint = str("fingerprint")
	if !fingerprintRe.MatchString(it.Fingerprint) {
		return it, refuse(RuleBadFingerprint, join(where, "fingerprint"))
	}
	if present(n, "expires_at") {
		it.ExpiresAt = str("expires_at")
		if !ValidTime(it.ExpiresAt) {
			return it, refuse(RuleBadTime, join(where, "expires_at"))
		}
	}
	it.Result = ItemResult(str("result"))
	if !containsT(ItemResults, it.Result) {
		return it, refuse(RuleBadResult, join(where, "result"))
	}
	if r := presence(n, where, "written_at", it.Result == ResultOK || it.Result == ResultNotInLastAnswer); r != nil {
		return it, r
	}
	if present(n, "written_at") {
		it.WrittenAt = str("written_at")
		if !ValidTime(it.WrittenAt) {
			return it, refuse(RuleBadTime, join(where, "written_at"))
		}
	}
	return it, nil
}

func containsT[T comparable](list []T, v T) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}
