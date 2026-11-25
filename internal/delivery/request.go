package delivery

import (
	"encoding/json"
	"strings"
)

// Reason is why a request was made.
type Reason string

const (
	ReasonCreate    Reason = "create"
	ReasonProvision Reason = "provision"
	// ReasonStart and ReasonRefresh are delivery at a shed's start and to a
	// running shed. A source accepts them; this server sends neither yet.
	ReasonStart   Reason = "start"
	ReasonRefresh Reason = "refresh"
)

// Reasons is every Reason, in the contract's order.
var Reasons = []Reason{ReasonCreate, ReasonProvision, ReasonStart, ReasonRefresh}

// ValidReason reports whether r is one of the contract's reasons.
func ValidReason(r Reason) bool { return containsT(Reasons, r) }

// The backends a request's shed may be on.
const (
	BackendFirecracker = "firecracker"
	BackendVZ          = "vz"
	BackendRemote      = "remote"
)

// Request is what the server sends a source: one shed, created or
// provisioned, wanting its items.
type Request struct {
	Version  int         `json:"version"`
	ID       string      `json:"id"`
	Reason   Reason      `json:"reason"`
	Deadline string      `json:"deadline"`
	User     string      `json:"user"`
	Shed     RequestShed `json:"shed"`
}

// RequestShed is the shed a request is for.
type RequestShed struct {
	Name       string       `json:"name"`
	ID         string       `json:"id"`
	Backend    string       `json:"backend"`
	Repo       *RequestRepo `json:"repo,omitempty"`
	LocalDir   string       `json:"local_dir,omitempty"`
	ProjectDir string       `json:"project_dir,omitempty"`
}

// RequestRepo is the shed's repository: its URL and, when the URL parses to
// one, its owner/name.
type RequestRepo struct {
	URL  string `json:"url"`
	Name string `json:"name,omitempty"`
}

// Marshal writes the request's wire form: compact JSON on one line, as a
// stream's data line carries it.
func (r *Request) Marshal() ([]byte, error) { return json.Marshal(r) }

// ruleRequestValue is a request member whose value breaks its rule. Rules
// for requests do not travel (a source does not answer a request it
// refuses), so it is not one of Rules.
const ruleRequestValue Rule = "bad_value"

var (
	requestMembers = []string{"version", "id", "reason", "deadline", "user", "shed"}
	requestKinds   = map[string]nodeKind{"version": kindNumber, "id": kindString, "reason": kindString, "deadline": kindString, "user": kindString, "shed": kindObject}
	shedMembers    = []string{"name", "id", "backend", "repo", "local_dir", "project_dir"}
	shedRequired   = []string{"name", "id", "backend"}
	shedKinds      = map[string]nodeKind{"name": kindString, "id": kindString, "backend": kindString, "repo": kindObject, "local_dir": kindString, "project_dir": kindString}
	repoMembers    = []string{"url", "name"}
	repoKinds      = map[string]nodeKind{"url": kindString, "name": kindString}
)

// ParseRequest decodes body as a request under the contract's conventions
// and rules. A source does not answer a request it cannot parse.
func ParseRequest(body []byte) (*Request, *Refusal) {
	if len(body) > MaxBodyBytes {
		return nil, refuse(RuleBodyTooLarge, "")
	}
	root, r := parseDocument(body)
	if r != nil {
		return nil, r
	}
	if r := checkMembers(root, "", requestMembers, requestMembers); r != nil {
		return nil, r
	}
	if r := checkTypes(root, "", requestMembers, requestKinds); r != nil {
		return nil, r
	}
	if root.get("version").text != "2" {
		return nil, refuse(RuleUnsupportedVer, "version")
	}
	req := &Request{
		Version:  Version,
		ID:       root.get("id").text,
		Reason:   Reason(root.get("reason").text),
		Deadline: root.get("deadline").text,
		User:     root.get("user").text,
	}
	if !ValidID(req.ID) {
		return nil, refuse(RuleBadID, "id")
	}
	if !ValidReason(req.Reason) {
		return nil, refuse(ruleRequestValue, "reason")
	}
	if !ValidTime(req.Deadline) {
		return nil, refuse(RuleBadTime, "deadline")
	}
	if req.User != "" && !boundedText(req.User, MaxNameBytes) {
		return nil, refuse(ruleRequestValue, "user")
	}

	sn := root.get("shed")
	if r := checkMembers(sn, "shed", shedMembers, shedRequired); r != nil {
		return nil, r
	}
	if r := checkTypes(sn, "shed", shedMembers, shedKinds); r != nil {
		return nil, r
	}
	str := func(n *node, name string) string {
		if v := n.get(name); v != nil {
			return v.text
		}
		return ""
	}
	s := &req.Shed
	s.Name, s.ID, s.Backend = str(sn, "name"), str(sn, "id"), str(sn, "backend")
	if !boundedText(s.Name, MaxNameBytes) {
		return nil, refuse(ruleRequestValue, "shed.name")
	}
	if !shedIDText(s.ID) {
		return nil, refuse(ruleRequestValue, "shed.id")
	}
	switch s.Backend {
	case BackendFirecracker, BackendVZ, BackendRemote:
	default:
		return nil, refuse(ruleRequestValue, "shed.backend")
	}
	if rn := sn.get("repo"); rn != nil {
		if r := checkMembers(rn, "shed.repo", repoMembers, []string{"url"}); r != nil {
			return nil, r
		}
		if r := checkTypes(rn, "shed.repo", repoMembers, repoKinds); r != nil {
			return nil, r
		}
		s.Repo = &RequestRepo{URL: str(rn, "url"), Name: str(rn, "name")}
		if !boundedText(s.Repo.URL, MaxURLBytes) {
			return nil, refuse(ruleRequestValue, "shed.repo.url")
		}
		if rn.get("name") != nil && !repoName(s.Repo.Name) {
			return nil, refuse(ruleRequestValue, "shed.repo.name")
		}
	}
	if sn.get("local_dir") != nil {
		s.LocalDir = str(sn, "local_dir")
		if !boundedText(s.LocalDir, MaxLocalDirBytes) || strings.Contains(s.LocalDir, "/") || s.LocalDir == "." || s.LocalDir == ".." {
			return nil, refuse(ruleRequestValue, "shed.local_dir")
		}
	}
	if sn.get("project_dir") != nil {
		s.ProjectDir = str(sn, "project_dir")
		if !boundedText(s.ProjectDir, MaxProjectDirBytes) || !canonicalAbsolute(s.ProjectDir) {
			return nil, refuse(ruleRequestValue, "shed.project_dir")
		}
	}
	return req, nil
}

// boundedText reports whether s is 1 to max bytes with no control
// character.
func boundedText(s string, max int) bool {
	return s != "" && len(s) <= max && !hasControl(s)
}

// canonicalAbsolute reports whether p is an absolute, canonical path: / or
// slash-separated segments after a leading slash, with no empty, . or ..
// segment, no trailing slash and no backslash.
func canonicalAbsolute(p string) bool {
	if p == "/" {
		return true
	}
	if !strings.HasPrefix(p, "/") || strings.ContainsRune(p, '\\') {
		return false
	}
	for _, seg := range strings.Split(p[1:], "/") {
		if seg == "" || seg == "." || seg == ".." {
			return false
		}
	}
	return true
}

// shedIDText reports whether s is 1 to 64 lowercase letters and digits.
func shedIDText(s string) bool {
	if s == "" || len(s) > MaxNameBytes {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c < 'a' || c > 'z') && (c < '0' || c > '9') {
			return false
		}
	}
	return true
}

// repoName reports whether s is owner/name: two non-empty parts, one slash,
// at most MaxRepoNameBytes, no control character.
func repoName(s string) bool {
	owner, name, ok := strings.Cut(s, "/")
	return ok && owner != "" && name != "" && !strings.Contains(name, "/") && boundedText(s, MaxRepoNameBytes)
}
