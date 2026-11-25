// Package delivery is shed-server's side of the provision contract: the
// documents a server and a person's credential source exchange when a shed
// is created or provisioned, and the rules an answer must meet.
//
// The contract itself, the source of truth for every implementation, is in
// tests/provision-contract/ at the root of this repository: the document
// (v2/CONTRACT.md), its fixtures and its manifest. This package's tests hold
// that folder to its manifest, run every answer fixture through
// ParseAnswer and check the rule it is refused by, decode every request,
// record and stream fixture, check that the rule names, reasons and results
// the contract lists are the ones this package knows, and check that the
// folder names no host or address outside example.test and 127.0.0.1.
//
// What is here:
//
//   - Request: what the server sends a source (ParseRequest, Marshal).
//   - Answer, Item, Problem: what a source sends back. ParseAnswer decodes
//     an answer strictly (a duplicate member, a null or an unknown member
//     refuses it) and applies every rule of the contract, in the contract's
//     order, against the request it answers (whose project directory
//     places the project items, so the target rules see where each file
//     lands); a refusal is a *Refusal naming the rule, which is what the
//     server returns as answer_refused. A Refusal never holds a delivered
//     value: it names the rule and where (items[3].path), nothing else.
//   - Record: the per-shed delivery record and its enums (State, StepState,
//     Reason, Outcome, ItemResult), with ParseRecord (on the parsed
//     document, refusing under a rule name) and DeriveState.
//   - The transport's names: the routes, the stream header, the error codes
//     and the limits.
//   - The writer's documents (writer.go), between the server and
//     "shed-agent apply-items" in the shed, not part of the contract:
//     WriterInput (one stage, with NewWriterInput, Marshal and the
//     writer's own strict ParseWriterInput), WriterLine, the exit codes
//     and the env file's name.
//   - Service: the server's delivery. Request asks a user's source for a
//     shed's bundle over the pin (Pin, Stream: internal/plugin's registry
//     admits the machine and holds its stream) and waits for the answer;
//     Claim judges an answer's binding and body for the transport (the
//     pending-request table, table.go); Write writes one stage of an
//     accepted bundle into a shed through its agent (Agent). A bundle is
//     held in memory only and prints as its item count.
//
// It has no HTTP handler and stores nothing: the routes are internal/api's,
// and the record is its caller's to keep.
package delivery
