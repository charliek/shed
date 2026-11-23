package delivery

import (
	"bytes"
	"encoding/json"
	"os"
	"strings"
)

// The writer is "shed-agent apply-items": the agent subcommand that writes
// one stage of an accepted bundle into a shed. The server runs it through
// the agent's exec path as the shed user, by absolute path, with a
// WriterInput on stdin and nothing secret in its arguments or environment.
// It answers one WriterLine per item on stdout.
//
// These are the documents both sides use. They are not part of the provision
// contract (a source never sees them); they are between this server and the
// agent baked into the shed's image, whose protocol number says it has the
// writer (guest.ApplyItemsProtocol).

// WriterCommand is the agent's subcommand.
const WriterCommand = "apply-items"

// WriterVersion is the version every WriterInput carries.
const WriterVersion = 1

// MaxWriterInputBytes is the most the writer reads from stdin; a longer
// input is refused unread. A stage's input is a re-encoding, with no HTML
// escaping, of a subset of an accepted answer's members (which was at most
// MaxBodyBytes) plus a project directory of at most MaxProjectDirBytes, so
// it is always under MaxBodyBytes + 2 KiB; the cap leaves a margin above
// that.
const MaxWriterInputBytes = 7 << 20

// EnvFile is the file under the shed user's home that a stage 1 writes the
// env items to, replaced whole at each stage 1 (and removed when the stage
// has no env item). Its directory is the user environment directory the
// agent reads after /etc/environment.d.
const (
	UserEnvDir  = refusedHomeTree
	EnvFileName = "50-shed-delivery.conf"
	EnvFile     = UserEnvDir + "/" + EnvFileName
)

// The writer's exit codes.
const (
	// WriterExitOK: every item was written.
	WriterExitOK = 0
	// WriterExitFailed: the stage stopped at a failure. The lines say which
	// item and why; the items after it are not_written. It is also the code
	// when a stage 1 with no env item could not remove the env file (a link
	// or something other than a regular file stands there); there is then
	// no line for it.
	WriterExitFailed = 1
	// WriterExitRefused: the input was refused (or the writer could not
	// start: run as root, no home). Nothing was written and no line is
	// printed.
	WriterExitRefused = 2
)

// WriterInput is one stage for the writer.
//
// Files are written in order, then the env file (stage 1 only); the stage
// stops at the first failure. Env items are written last so that a stage
// that fails on a file leaves the previous env file as it was: a command
// never sees a new value whose file did not land.
type WriterInput struct {
	Version int `json:"version"`
	// Stage is 1 or 2. A stage 1 replaces (or removes) the env file; a
	// stage 2 carries no env item and leaves it alone. A project file is
	// stage 2 only.
	Stage int `json:"stage"`
	// ProjectDir is the shed's project directory, absolute and canonical,
	// strictly under the shed user's home. It is present exactly when a
	// file has base project.
	ProjectDir string       `json:"project_dir,omitempty"`
	Files      []WriterFile `json:"files"`
	Env        []WriterEnv  `json:"env"`
}

// WriterFile is one file item, without its stage.
type WriterFile struct {
	Name    string `json:"name"`
	Base    Base   `json:"base"`
	Path    string `json:"path"`
	Mode    string `json:"mode"`
	Content []byte `json:"content"` // padded standard base64 on the wire
}

// WriterEnv is one env item.
type WriterEnv struct {
	Name     string `json:"name"`
	Variable string `json:"variable"`
	Value    string `json:"value"`
}

// WriterLine is one line of the writer's output: an item's name and its
// result, one of WriterLineResults. It carries nothing else.
type WriterLine struct {
	Name   string     `json:"name"`
	Result ItemResult `json:"result"`
}

// WriterLineResults are the results a writer line may carry: the writer's
// own (WriterResults), and not_written for an item its stage never reached.
var WriterLineResults = append(append([]ItemResult{}, WriterResults...), ResultNotWritten)

// FileMode is a writer file's mode.
func (f WriterFile) FileMode() os.FileMode {
	return Item{Kind: KindFile, Mode: f.Mode}.FileMode()
}

// NewWriterInput is stage's input from an accepted answer's items: the file
// items of that stage in order, and for stage 1 every env item in order.
// projectDir is the shed's project directory; it is set on the input only
// when a project item is in it.
func NewWriterInput(stage int, projectDir string, items []Item) WriterInput {
	in := WriterInput{Version: WriterVersion, Stage: stage, Files: []WriterFile{}, Env: []WriterEnv{}}
	for _, it := range items {
		switch {
		case it.Kind == KindFile && it.Stage == stage:
			in.Files = append(in.Files, WriterFile{Name: it.Name, Base: it.Base, Path: it.Path, Mode: it.Mode, Content: it.Content})
			if it.Base == BaseProject {
				in.ProjectDir = projectDir
			}
		case it.Kind == KindEnv && stage == 1:
			in.Env = append(in.Env, WriterEnv{Name: it.Name, Variable: it.Variable, Value: it.Value})
		}
	}
	return in
}

// Marshal writes the input's wire form: one JSON object, no HTML escaping,
// nil lists as [].
func (in WriterInput) Marshal() ([]byte, error) {
	if in.Files == nil {
		in.Files = []WriterFile{}
	}
	if in.Env == nil {
		in.Env = []WriterEnv{}
	}
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(in); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(b.Bytes(), []byte("\n")), nil
}

var (
	writerMembers   = []string{"version", "stage", "project_dir", "files", "env"}
	writerRequired  = []string{"version", "stage", "files", "env"}
	writerKinds     = map[string]nodeKind{"version": kindNumber, "stage": kindNumber, "project_dir": kindString, "files": kindArray, "env": kindArray}
	writerFileNames = []string{"name", "base", "path", "mode", "content"}
	writerEnvNames  = []string{"name", "variable", "value"}
	writerStrings   = map[string]nodeKind{"name": kindString, "base": kindString, "path": kindString, "mode": kindString, "content": kindString, "variable": kindString, "value": kindString}
)

// ParseWriterInput decodes and checks a writer input, for the writer, which
// trusts nothing the server checked: the document strictly (no duplicate
// member, null or unknown member), then every item rule of the contract
// that applies to it (names, bases and stages, canonical paths, refused
// targets, modes, content and value limits, refused variables, duplicate
// and nested targets), with home the shed user's home (absolute and
// canonical). A project directory must be strictly under home. One bad
// item refuses the whole input.
func ParseWriterInput(body []byte, home string) (*WriterInput, *Refusal) {
	if !canonicalAbsolute(home) || home == "/" {
		return nil, refuse(ruleRequestValue, "home")
	}
	if len(body) > MaxWriterInputBytes {
		return nil, refuse(RuleBodyTooLarge, "")
	}
	root, r := parseDocument(body)
	if r != nil {
		return nil, r
	}
	if r := checkMembers(root, "", writerMembers, writerRequired); r != nil {
		return nil, r
	}
	if r := checkTypes(root, "", writerMembers, writerKinds); r != nil {
		return nil, r
	}
	if root.get("version").text != "1" {
		return nil, refuse(RuleUnsupportedVer, "version")
	}
	in := &WriterInput{Version: WriterVersion, Files: []WriterFile{}, Env: []WriterEnv{}}
	switch root.get("stage").text {
	case "1":
		in.Stage = 1
	case "2":
		in.Stage = 2
	default:
		return nil, refuse(RuleBadStage, "stage")
	}

	// The project directory, placed under the contract's home so the target
	// rules below see where each project file lands.
	virtualProject := ""
	if pd := root.get("project_dir"); pd != nil {
		in.ProjectDir = pd.text
		rel, under := strings.CutPrefix(in.ProjectDir, home+"/")
		if !boundedText(in.ProjectDir, MaxProjectDirBytes) || !canonicalAbsolute(in.ProjectDir) || !under || rel == "" {
			return nil, refuse(ruleRequestValue, "project_dir")
		}
		virtualProject = HomeDir + "/" + rel
	}

	fileNodes, envNodes := root.get("files").elems, root.get("env").elems
	if len(fileNodes)+len(envNodes) > MaxItems {
		return nil, refuse(RuleTooManyItems, "files")
	}
	if in.Stage != 1 && len(envNodes) > 0 {
		return nil, refuse(RuleBadStage, "env")
	}

	var items []Item
	hasProject := false
	for i, n := range fileNodes {
		where := index("files", i)
		if n.kind != kindObject {
			return nil, refuse(RuleWrongType, where)
		}
		if r := checkMembers(n, where, writerFileNames, writerFileNames); r != nil {
			return nil, r
		}
		if r := checkTypes(n, where, writerFileNames, writerStrings); r != nil {
			return nil, r
		}
		f := WriterFile{Name: n.get("name").text, Base: Base(n.get("base").text), Path: n.get("path").text, Mode: n.get("mode").text}
		if !ValidName(f.Name) {
			return nil, refuse(RuleBadName, join(where, "name"))
		}
		switch f.Base {
		case BaseHome:
		case BaseProject:
			if in.Stage != 2 {
				return nil, refuse(RuleProjectBaseStage, join(where, "base"))
			}
			if virtualProject == "" {
				return nil, refuse(RuleBadBase, join(where, "base"))
			}
			hasProject = true
		default:
			return nil, refuse(RuleBadBase, join(where, "base"))
		}
		if rule := CheckPath(f.Path); rule != "" {
			return nil, refuse(rule, join(where, "path"))
		}
		it := Item{Name: f.Name, Kind: KindFile, Stage: in.Stage, Base: f.Base, Path: f.Path}
		if rel, ok := homeRelative(landing(it, virtualProject)); ok && RefusedHomeTarget(rel) {
			return nil, refuse(RuleRefusedTarget, join(where, "path"))
		}
		if !containsT(modes, f.Mode) {
			return nil, refuse(RuleBadMode, join(where, "mode"))
		}
		content, ok := decodeContent(n.get("content").text)
		if !ok {
			return nil, refuse(RuleBadBase64, join(where, "content"))
		}
		if len(content) > MaxFileBytes {
			return nil, refuse(RuleFileTooLarge, join(where, "content"))
		}
		f.Content, it.Content = content, content
		in.Files = append(in.Files, f)
		items = append(items, it)
	}
	if virtualProject != "" && !hasProject {
		// A project directory with no project file is not something the
		// server sends; refuse rather than guess.
		return nil, refuse(ruleRequestValue, "project_dir")
	}
	for i, n := range envNodes {
		where := index("env", i)
		if n.kind != kindObject {
			return nil, refuse(RuleWrongType, where)
		}
		if r := checkMembers(n, where, writerEnvNames, writerEnvNames); r != nil {
			return nil, r
		}
		if r := checkTypes(n, where, writerEnvNames, writerStrings); r != nil {
			return nil, r
		}
		e := WriterEnv{Name: n.get("name").text, Variable: n.get("variable").text, Value: n.get("value").text}
		if !ValidName(e.Name) {
			return nil, refuse(RuleBadName, join(where, "name"))
		}
		if !variableRe.MatchString(e.Variable) {
			return nil, refuse(RuleBadVariable, join(where, "variable"))
		}
		if RefusedVariable(e.Variable) {
			return nil, refuse(RuleRefusedVariable, join(where, "variable"))
		}
		if rule := CheckValue(e.Value); rule != "" {
			return nil, refuse(rule, join(where, "value"))
		}
		in.Env = append(in.Env, e)
		items = append(items, Item{Name: e.Name, Kind: KindEnv, Variable: e.Variable, Value: e.Value})
	}
	if r := checkAcross(items, virtualProject); r != nil {
		return nil, r
	}
	return in, nil
}
