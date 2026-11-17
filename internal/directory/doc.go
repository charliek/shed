// Package directory is shed-server's side of the directory contract: an
// account service holds people and their SSH login keys, and a server
// enrolled with it learns its users from it.
//
// The contract itself, the source of truth for both sides, is in
// tests/directory-contract/ at the root of this repository: the document
// (v1/CONTRACT.md), its fixtures and its manifest. This package's tests hold
// that folder to its manifest, run its key corpus through shed's own key
// parser, check its literal tokens and check that it names no host or
// address outside example.test.
//
// The client side of the contract:
//
//   - Enroll sends a one-time code and a new Ed25519 public key and checks
//     the 201; the Enrolment it returns is what enrolment.json holds
//     (MarshalFile, ParseEnrolmentFile).
//   - Client logs in (client credentials with a JWT client assertion, a
//     fresh one for every request) and pulls the list (Pull); the access
//     token lives in memory only.
//   - DecodeList applies the contract's rules to a directory answer: the
//     top level (rule 6) and the organization (rule 5) refuse the whole
//     answer; each user is decoded alone (rule 7), skipped with a reason
//     when it fails, the rest standing. A client's further rules (a local
//     admin's name or key, a service client's name) are its lookup's.
//   - Every request goes through the one HTTP client this package builds
//     from the configuration (the issuer, AllowPlainHTTP, and RootCAs for
//     tests): no redirect, HTTPS with certificate checks, plain HTTP only to
//     loopback when asked for, requests only to the issuer. A caller cannot
//     supply its own client or transport. Every failure is a *Failure,
//     classified, with RetryAfter as rule 2 reads it.
//   - The state files (enrolment.json, users.json) are read only after
//     checks of their directory and themselves (ReadStateFile), written
//     durably (WriteStateFile) or once (CreateStateFileOnce), always through
//     the directory checked, opened once.
package directory
