package delivery

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"os"
	"strings"
)

// Kind is an item's kind.
type Kind string

const (
	KindFile Kind = "file"
	KindEnv  Kind = "env"
)

// Base is the directory a file item's path is relative to.
type Base string

const (
	BaseHome    Base = "home"
	BaseProject Base = "project"
)

// The file modes an item may carry.
var modes = []string{"0400", "0600", "0644"}

// ProblemCodes is the closed list of a problem's code.
var ProblemCodes = []string{
	"missing_input", "unreadable", "signed_out", "passphrase_protected", "conflict", "command_failed",
}

// Answer is what a source sends for one request.
type Answer struct {
	Version  int
	ID       string
	Items    []Item
	Problems []Problem
}

// Item is one thing to write into the shed: a file, or an env variable.
type Item struct {
	Name     string
	Provider string
	Kind     Kind

	// A file item's members.
	Stage   int
	Base    Base
	Path    string
	Mode    string
	Content []byte // decoded

	// An env item's members. An env item is stage 1.
	Variable string
	Value    string

	Fingerprint string
	ExpiresAt   string // "" when absent
}

// Problem is something a source could not deliver, and why.
type Problem struct {
	Provider string `json:"provider"`
	Code     string `json:"code"`
	Message  string `json:"message"`
}

// ItemStage is the stage the item is written at: an env item's is 1.
func (it Item) ItemStage() int {
	if it.Kind == KindEnv {
		return 1
	}
	return it.Stage
}

// Target is what the item writes: a file item's base-relative path, or an
// env item's variable.
func (it Item) Target() string {
	if it.Kind == KindEnv {
		return it.Variable
	}
	return it.Path
}

// FileMode is a file item's mode.
func (it Item) FileMode() os.FileMode {
	switch it.Mode {
	case "0400":
		return 0o400
	case "0644":
		return 0o644
	}
	return 0o600
}

// MarshalJSON writes the item's wire form: the members of its kind only,
// content as padded standard base64.
func (it Item) MarshalJSON() ([]byte, error) {
	var b bytes.Buffer
	b.WriteByte('{')
	first := true
	put := func(name string, v any) error {
		enc, err := json.Marshal(v)
		if err != nil {
			return err
		}
		if !first {
			b.WriteByte(',')
		}
		first = false
		nb, _ := json.Marshal(name)
		b.Write(nb)
		b.WriteByte(':')
		b.Write(enc)
		return nil
	}
	fields := []struct {
		name string
		v    any
		ok   bool
	}{
		{"name", it.Name, true},
		{"provider", it.Provider, true},
		{"kind", it.Kind, true},
		{"stage", it.Stage, it.Kind == KindFile},
		{"base", it.Base, it.Kind == KindFile},
		{"path", it.Path, it.Kind == KindFile},
		{"mode", it.Mode, it.Kind == KindFile},
		{"content", base64.StdEncoding.EncodeToString(it.Content), it.Kind == KindFile},
		{"variable", it.Variable, it.Kind == KindEnv},
		{"value", it.Value, it.Kind == KindEnv},
		{"fingerprint", it.Fingerprint, true},
		{"expires_at", it.ExpiresAt, it.ExpiresAt != ""},
	}
	for _, f := range fields {
		if !f.ok {
			continue
		}
		if err := put(f.name, f.v); err != nil {
			return nil, err
		}
	}
	b.WriteByte('}')
	return b.Bytes(), nil
}

// MarshalJSON writes the answer's wire form. Nil lists are written [].
func (a Answer) MarshalJSON() ([]byte, error) {
	items, problems := a.Items, a.Problems
	if items == nil {
		items = []Item{}
	}
	if problems == nil {
		problems = []Problem{}
	}
	return json.Marshal(struct {
		Version  int       `json:"version"`
		ID       string    `json:"id"`
		Items    []Item    `json:"items"`
		Problems []Problem `json:"problems"`
	}{a.Version, a.ID, items, problems})
}

var (
	answerMembers  = []string{"version", "id", "items", "problems"}
	answerKinds    = map[string]nodeKind{"version": kindNumber, "id": kindString, "items": kindArray, "problems": kindArray}
	itemCommon     = []string{"name", "provider", "kind", "fingerprint", "expires_at"}
	itemFile       = []string{"stage", "base", "path", "mode", "content"}
	itemEnv        = []string{"variable", "value"}
	itemAll        = append(append(append([]string{}, itemCommon...), itemFile...), itemEnv...)
	itemCommonReq  = []string{"name", "provider", "kind", "fingerprint"}
	itemKinds      = map[string]nodeKind{"name": kindString, "provider": kindString, "kind": kindString, "fingerprint": kindString, "expires_at": kindString, "stage": kindNumber, "base": kindString, "path": kindString, "mode": kindString, "content": kindString, "variable": kindString, "value": kindString}
	problemMembers = []string{"provider", "code", "message"}
	problemKinds   = map[string]nodeKind{"provider": kindString, "code": kindString, "message": kindString}
)

// ParseAnswer decodes body as an answer to req and applies every rule of
// the contract, in the contract's order. It returns the answer, or the first
// rule broken. One bad item refuses the whole answer.
//
// The answer is judged against its request: its id, and the shed's project
// directory, which places every base: project item at an absolute path, so
// that the target rules (refused, duplicate and nested targets) see where
// each file really lands, across both bases.
func ParseAnswer(body []byte, req *Request) (*Answer, *Refusal) {
	requestID, projectDir := "", ""
	if req != nil {
		requestID, projectDir = req.ID, req.Shed.ProjectDir
	}
	// The placement below is lexical, so it holds only for a canonical
	// absolute directory. ParseRequest checks that; a request built in
	// code (the server's own) is checked here, never trusted.
	if projectDir != "" && (!boundedText(projectDir, MaxProjectDirBytes) || !canonicalAbsolute(projectDir)) {
		return nil, refuse(ruleRequestValue, "shed.project_dir")
	}
	if len(body) > MaxBodyBytes {
		return nil, refuse(RuleBodyTooLarge, "")
	}
	root, r := parseDocument(body)
	if r != nil {
		return nil, r
	}
	// The top level.
	if r := checkMembers(root, "", answerMembers, answerMembers); r != nil {
		return nil, r
	}
	if r := checkTypes(root, "", answerMembers, answerKinds); r != nil {
		return nil, r
	}
	if root.get("version").text != "1" {
		return nil, refuse(RuleUnsupportedVer, "version")
	}
	id := root.get("id").text
	if !ValidID(id) {
		return nil, refuse(RuleBadID, "id")
	}
	if id != requestID {
		return nil, refuse(RuleIDMismatch, "id")
	}
	itemNodes, problemNodes := root.get("items").elems, root.get("problems").elems
	if len(itemNodes) > MaxItems {
		return nil, refuse(RuleTooManyItems, "items")
	}
	if len(problemNodes) > MaxProblems {
		return nil, refuse(RuleTooManyProblems, "problems")
	}

	a := &Answer{Version: Version, ID: id, Items: []Item{}, Problems: []Problem{}}
	for i, n := range itemNodes {
		it, r := parseItem(n, index("items", i), projectDir)
		if r != nil {
			return nil, r
		}
		a.Items = append(a.Items, it)
	}
	if r := checkAcross(a.Items, projectDir); r != nil {
		return nil, r
	}
	if EnvFileSize(a.Items) > MaxEnvFileBytes {
		return nil, refuse(RuleEnvTooLarge, "items")
	}
	for i, n := range problemNodes {
		p, r := parseProblem(n, index("problems", i))
		if r != nil {
			return nil, r
		}
		a.Problems = append(a.Problems, p)
	}
	return a, nil
}

// parseItem applies the item rules to one item, in order.
func parseItem(n *node, where, projectDir string) (Item, *Refusal) {
	var it Item
	if n.kind != kindObject {
		return it, refuse(RuleWrongType, where)
	}
	if r := checkMembers(n, where, itemAll, nil); r != nil {
		return it, r
	}
	// The kind decides the rest.
	k := n.get("kind")
	switch {
	case k == nil:
		return it, refuse(RuleMissingMember, join(where, "kind"))
	case k.kind != kindString:
		return it, refuse(RuleWrongType, join(where, "kind"))
	case k.text != string(KindFile) && k.text != string(KindEnv):
		return it, refuse(RuleBadKind, join(where, "kind"))
	}
	it.Kind = Kind(k.text)
	own, other := itemFile, itemEnv
	if it.Kind == KindEnv {
		own, other = itemEnv, itemFile
	}
	for _, m := range n.members {
		if containsT(other, m.name) {
			return it, refuse(RuleWrongKindMember, join(where, m.name))
		}
	}
	required := append(append([]string{}, itemCommonReq...), own...)
	if r := checkMembers(n, where, itemAll, required); r != nil {
		return it, r
	}
	if r := checkTypes(n, where, itemAll, itemKinds); r != nil {
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

	switch it.Kind {
	case KindFile:
		switch n.get("stage").text {
		case "1":
			it.Stage = 1
		case "2":
			it.Stage = 2
		default:
			return it, refuse(RuleBadStage, join(where, "stage"))
		}
		it.Base = Base(str("base"))
		if it.Base != BaseHome && it.Base != BaseProject {
			return it, refuse(RuleBadBase, join(where, "base"))
		}
		if it.Base == BaseProject && it.Stage != 2 {
			return it, refuse(RuleProjectBaseStage, join(where, "base"))
		}
		it.Path = str("path")
		if rule := CheckPath(it.Path); rule != "" {
			return it, refuse(rule, join(where, "path"))
		}
		if rel, ok := homeRelative(landing(it, projectDir)); ok && RefusedHomeTarget(rel) {
			return it, refuse(RuleRefusedTarget, join(where, "path"))
		}
		it.Mode = str("mode")
		if !containsT(modes, it.Mode) {
			return it, refuse(RuleBadMode, join(where, "mode"))
		}
		content, ok := decodeContent(str("content"))
		if !ok {
			return it, refuse(RuleBadBase64, join(where, "content"))
		}
		if len(content) > MaxFileBytes {
			return it, refuse(RuleFileTooLarge, join(where, "content"))
		}
		it.Content = content
	case KindEnv:
		it.Variable = str("variable")
		if !variableRe.MatchString(it.Variable) {
			return it, refuse(RuleBadVariable, join(where, "variable"))
		}
		if RefusedVariable(it.Variable) {
			return it, refuse(RuleRefusedVariable, join(where, "variable"))
		}
		it.Value = str("value")
		if rule := CheckValue(it.Value); rule != "" {
			return it, refuse(rule, join(where, "value"))
		}
	}

	it.Fingerprint = str("fingerprint")
	if !fingerprintRe.MatchString(it.Fingerprint) {
		return it, refuse(RuleBadFingerprint, join(where, "fingerprint"))
	}
	if n.get("expires_at") != nil {
		it.ExpiresAt = str("expires_at")
		if !ValidTime(it.ExpiresAt) {
			return it, refuse(RuleBadTime, join(where, "expires_at"))
		}
	}
	return it, nil
}

// decodeContent decodes padded standard base64 with no line break and zero
// padding bits.
func decodeContent(s string) ([]byte, bool) {
	if strings.ContainsAny(s, "\r\n") {
		return nil, false
	}
	b, err := base64.StdEncoding.Strict().DecodeString(s)
	if err != nil {
		return nil, false
	}
	return b, true
}

// checkAcross applies the rules that compare items, in order: a name used
// twice, a target named twice, a file path that is a directory prefix of
// another, and the file contents together. File targets are compared where
// they land (landing), so a home item and a project item that reach the
// same file, or one inside the other, are caught.
func checkAcross(items []Item, projectDir string) *Refusal {
	for j := range items {
		for i := 0; i < j; i++ {
			if items[i].Name == items[j].Name {
				return refuse(RuleDuplicateName, index("items", j)+".name")
			}
		}
	}
	keys := make([]string, len(items))
	for i, it := range items {
		if it.Kind == KindEnv {
			keys[i] = "env:" + it.Variable
		} else {
			keys[i] = landing(it, projectDir)
		}
	}
	for j := range items {
		for i := 0; i < j; i++ {
			if keys[i] == keys[j] {
				return refuse(RuleDuplicateTarget, index("items", j))
			}
		}
	}
	for j := range items {
		for i := 0; i < j; i++ {
			if items[i].Kind == KindFile && items[j].Kind == KindFile &&
				(strings.HasPrefix(keys[i], keys[j]+"/") || strings.HasPrefix(keys[j], keys[i]+"/")) {
				return refuse(RuleNestedTarget, index("items", j))
			}
		}
	}
	total := 0
	for _, it := range items {
		total += len(it.Content)
	}
	if total > MaxTotalFileBytes {
		return refuse(RuleTotalTooLarge, "items")
	}
	return nil
}

// EnvFileSize is the size of the env file the env items among items are
// written as: one VARIABLE=value line each, with its line feed.
func EnvFileSize(items []Item) int {
	n := 0
	for _, it := range items {
		if it.Kind == KindEnv {
			n += len(it.Variable) + 1 + len(it.Value) + 1
		}
	}
	return n
}

// HomeDir is the shed user's home directory, the base home.
const HomeDir = "/home/shed"

// landing is where a file item lands: an absolute path, its base's
// directory joined with its path. A base: project item of a request with
// no project directory has no place; it is keyed in a space of its own
// ("project:<path>"), compared only with other such items, and is never
// written (the server skips stage 2 for a shed with no project directory).
func landing(it Item, projectDir string) string {
	if it.Base == BaseProject {
		if projectDir == "" {
			return "project:" + it.Path
		}
		if projectDir == "/" {
			return "/" + it.Path
		}
		return projectDir + "/" + it.Path
	}
	return HomeDir + "/" + it.Path
}

// homeRelative is an absolute path's path under HomeDir, and whether it is
// under it.
func homeRelative(abs string) (string, bool) {
	return strings.CutPrefix(abs, HomeDir+"/")
}

// parseProblem applies the problem rules to one problem, in order.
func parseProblem(n *node, where string) (Problem, *Refusal) {
	var p Problem
	if n.kind != kindObject {
		return p, refuse(RuleWrongType, where)
	}
	if r := checkMembers(n, where, problemMembers, problemMembers); r != nil {
		return p, r
	}
	if r := checkTypes(n, where, problemMembers, problemKinds); r != nil {
		return p, r
	}
	p = Problem{Provider: n.get("provider").text, Code: n.get("code").text, Message: n.get("message").text}
	if !ValidName(p.Provider) {
		return p, refuse(RuleBadProvider, join(where, "provider"))
	}
	if !containsT(ProblemCodes, p.Code) {
		return p, refuse(RuleBadProblemCode, join(where, "code"))
	}
	if len(p.Message) > MaxMessageBytes || !Printable(p.Message) {
		return p, refuse(RuleBadProblemMessage, join(where, "message"))
	}
	return p, nil
}
