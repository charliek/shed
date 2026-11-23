package delivery

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"time"

	"github.com/charliek/shed/internal/remote/guest"
)

// Writing an accepted bundle into a shed: one stage, one exec of the agent's
// writer ("shed-agent apply-items"), whose output the server trusts no
// further than it must. The shed user has sudo and can replace the agent, so
// the worst a hostile shed can do is misreport its own delivery; the server
// reads a bounded amount, keeps only well-formed lines about items it sent,
// and counts everything else as io.

// Agent is a shed's agent as Write needs it: the probe that reports its
// protocol, and a command run as the shed user with stdin and stdout, which
// returns the command's exit code (a non-zero exit is not an error; an error
// is a command that did not run or did not finish). Run must stop the
// command once it has sent more than maxOutput bytes, and must not touch
// stdin or stdout once it has returned. *vmutil.AgentClient satisfies it on
// every backend.
//
// Nothing an Agent returns is trusted or repeated: its errors may quote
// what the guest sent, which may be a delivered value, so Write logs and
// returns only fixed classifications of them.
type Agent interface {
	Probe(ctx context.Context) (guest.AgentInfo, error)
	Run(ctx context.Context, argv []string, stdin io.Reader, stdout io.Writer, maxOutput int) (exitCode int, err error)
}

// The writer's bounds: one exec per stage, at most writerTimeout, and at most
// maxWriterOutput bytes of its output read. Vars so tests can shrink them.
var (
	writerTimeout   = 60 * time.Second
	probeTimeout    = 10 * time.Second
	maxWriterOutput = 64 << 10
)

// WriterArgv is the command a stage runs: the agent binary, by the absolute
// path every image installs it at, and its writer subcommand. Nothing
// secret is in it; the items go on stdin.
var WriterArgv = []string{guest.AgentBinaryPath, WriterCommand}

// ItemOutcome is what became of one item of a stage, by its name.
type ItemOutcome struct {
	Name   string
	Result ItemResult
}

// StageResult is what one stage's write did, ready for the record: the
// step's state and reason, each item of the stage with its result (in the
// order they were sent: files, then env items), and, when the stage failed,
// why. A failure with no item (Failure.Item empty) is one the writer did not
// put on an item: the env file could not be replaced or removed, the agent
// did not answer, or its reply could not be read.
type StageResult struct {
	Stage   int
	State   StepState
	Reason  StepReason
	Items   []ItemOutcome
	Failure *Failure
}

// Write writes stage (1 or 2) of bundle into the shed whose agent is agent,
// with projectDir the shed's project directory (used only when a project
// item is in the stage).
//
//   - Stage 2 with no item is skipped (no_items) without asking the agent
//     anything.
//   - Otherwise the agent's protocol is probed first: an agent below
//     guest.ApplyItemsProtocol cannot take items, and the stage fails as
//     image_too_old with nothing sent (a stage 1 with no item is then
//     skipped, no_items: there is nothing to write). The writer is never
//     tried to find out.
//   - The writer runs once, within 60 seconds, as the shed user, with the
//     stage's items on stdin. A stage 1 runs even with no item, so that a
//     re-delivery without env items removes the env file.
//   - At most 64 KiB of its output is read. A line counts only when it is
//     a JSON object holding exactly a name the stage sent and a result in
//     the writer's closed list; the first such line for a name is its
//     result. An item with none is io.
//   - The stage is done when the writer exited 0 and every item is ok
//     (skipped, no_items, for a stage 1 with no item). Otherwise it failed,
//     write_failed, at the first item whose result is neither ok nor
//     not_written, or, when there is none, with result io and no item.
func (s *Service) Write(ctx context.Context, agent Agent, stage int, bundle *Bundle, projectDir string) StageResult {
	items := bundle.StageItems(stage)
	in := NewWriterInput(stage, projectDir, items)
	// Files first, then env items: the order the writer reports them in.
	sent := make([]ItemOutcome, 0, len(items))
	for _, f := range in.Files {
		sent = append(sent, ItemOutcome{Name: f.Name})
	}
	for _, e := range in.Env {
		sent = append(sent, ItemOutcome{Name: e.Name})
	}
	res := StageResult{Stage: stage, Items: sent}
	notWritten := func() {
		for i := range res.Items {
			res.Items[i].Result = ResultNotWritten
		}
	}
	if stage != 1 && stage != 2 {
		notWritten()
		res.State, res.Reason = StepFailed, ReasonWriteFailed
		res.Failure = &Failure{Reason: ReasonWriteFailed, Result: ResultIO}
		return res
	}
	if stage == 2 && len(items) == 0 {
		res.State, res.Reason = StepSkipped, ReasonNoItems
		return res
	}

	// One bound for the stage, probe and writer both; the probe has a
	// shorter one of its own inside it.
	stageCtx, cancel := context.WithTimeout(ctx, writerTimeout)
	defer cancel()
	probeCtx, cancelProbe := context.WithTimeout(stageCtx, probeTimeout)
	info, err := agent.Probe(probeCtx)
	cancelProbe()
	if err != nil {
		// The error is not repeated: the guest may have written it.
		log.Printf("delivery: stage %d: the shed's agent did not answer its probe (probe failed)", stage)
		notWritten()
		res.State, res.Reason = StepFailed, ReasonWriteFailed
		res.Failure = &Failure{Reason: ReasonWriteFailed, Result: ResultIO}
		return res
	}
	if info.Protocol < guest.ApplyItemsProtocol {
		if len(items) == 0 {
			res.State, res.Reason = StepSkipped, ReasonNoItems
			return res
		}
		// The number is the guest's to choose: never repeated.
		log.Printf("delivery: stage %d: the shed's agent speaks a protocol below %d: its image cannot take items",
			stage, guest.ApplyItemsProtocol)
		notWritten()
		res.State, res.Reason = StepFailed, ReasonImageTooOld
		res.Failure = &Failure{Reason: ReasonImageTooOld}
		return res
	}

	input, err := in.Marshal()
	if err != nil {
		// Unreachable for an accepted answer; nothing of it is logged.
		log.Printf("delivery: stage %d: the writer's input could not be encoded", stage)
		notWritten()
		res.State, res.Reason = StepFailed, ReasonWriteFailed
		res.Failure = &Failure{Reason: ReasonWriteFailed, Result: ResultIO}
		return res
	}
	out := &boundedBuffer{max: maxWriterOutput}
	code, runErr := agent.Run(stageCtx, WriterArgv, bytes.NewReader(input), out, maxWriterOutput)
	clear(input)
	switch {
	case out.over:
		log.Printf("delivery: stage %d: the writer's output passed %d bytes; it was stopped and none of it is used (writer output too large)", stage, maxWriterOutput)
		code = -1
	case runErr != nil && stageCtx.Err() != nil:
		log.Printf("delivery: stage %d: the writer did not finish in time (writer timed out)", stage)
		code = -1
	case runErr != nil:
		// The error is not repeated: the guest may have written it.
		log.Printf("delivery: stage %d: the writer did not run or did not finish (exec failed)", stage)
		code = -1
	case code == WriterExitFailed:
		log.Printf("delivery: stage %d: the writer stopped at a failed item (writer exit 1)", stage)
	case code == WriterExitRefused:
		log.Printf("delivery: stage %d: the writer refused its input (writer exit 2)", stage)
	case code != WriterExitOK:
		// The number is the guest's to choose: never repeated.
		log.Printf("delivery: stage %d: the writer exited with an unexpected status", stage)
	}
	output := out.Bytes()
	if out.over {
		output = nil // past the limit nothing of it counts: every item is io
	}
	return judgeWriter(res, code, output, len(items) == 0)
}

// judgeWriter fills res from the writer's exit code and output, trusting
// neither further than the rules on Write say.
func judgeWriter(res StageResult, code int, output []byte, empty bool) StageResult {
	results := writerResults(output, res.Items)
	allOK := true
	for i := range res.Items {
		r, ok := results[res.Items[i].Name]
		if !ok {
			r = ResultIO
		}
		res.Items[i].Result = r
		allOK = allOK && r == ResultOK
	}
	if code == WriterExitOK && allOK {
		res.State = StepDone
		if empty {
			res.State, res.Reason = StepSkipped, ReasonNoItems
		}
		return res
	}
	res.State, res.Reason = StepFailed, ReasonWriteFailed
	res.Failure = &Failure{Reason: ReasonWriteFailed, Result: ResultIO}
	for _, it := range res.Items {
		if it.Result != ResultOK && it.Result != ResultNotWritten {
			res.Failure.Item, res.Failure.Result = it.Name, it.Result
			break
		}
	}
	return res
}

// writerResults reads the writer's lines: the first well-formed line for
// each name sent, whose result is in the writer's closed list. Every other
// line is ignored.
func writerResults(output []byte, sent []ItemOutcome) map[string]ItemResult {
	names := make(map[string]bool, len(sent))
	for _, it := range sent {
		names[it.Name] = true
	}
	results := map[string]ItemResult{}
	for _, line := range bytes.Split(output, []byte("\n")) {
		var l struct {
			Name   *string `json:"name"`
			Result *string `json:"result"`
		}
		dec := json.NewDecoder(bytes.NewReader(line))
		dec.DisallowUnknownFields()
		if dec.Decode(&l) != nil || dec.More() || l.Name == nil || l.Result == nil {
			continue
		}
		if _, err := dec.Token(); err != io.EOF {
			continue
		}
		r := ItemResult(*l.Result)
		if !names[*l.Name] || !containsT(WriterLineResults, r) {
			continue
		}
		if _, seen := results[*l.Name]; !seen {
			results[*l.Name] = r
		}
	}
	return results
}

// boundedBuffer keeps what is written to it up to max bytes; a write past
// it is refused (which stops the exec), and over is set.
type boundedBuffer struct {
	max  int
	buf  []byte
	over bool
}

// errOutputTooLarge is boundedBuffer's refusal of a write past its limit.
var errOutputTooLarge = errors.New("delivery: the writer's output passed its limit")

func (b *boundedBuffer) Write(p []byte) (int, error) {
	if b.over || len(b.buf)+len(p) > b.max {
		b.over = true
		return 0, errOutputTooLarge
	}
	b.buf = append(b.buf, p...)
	return len(p), nil
}

func (b *boundedBuffer) Bytes() []byte { return b.buf }
