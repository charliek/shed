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
//     fresh one for every request, asking for the scope directory:read and
//     nothing else) and pulls the list (Pull); the access token lives in
//     memory only.
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
//   - Lookup is the users.Lookup of a directory server: the local admins
//     (auth.users beside auth.directory) and a decoded list's users, after
//     the server's own rules (an entry with a local admin's name or key, or
//     a service client's name, is skipped). Every reader answers from one
//     snapshot per call, whose users and keys never change once the lookup
//     holds it. A new snapshot is published at one point only: a newly
//     pulled list (Install, numbered so an older pull's late result is
//     discarded) or the end of its trust time (WatchTrust), after which the
//     lookup answers for the local admins only, on the wall clock and
//     latched, until a new list arrives. Each publication runs a hook
//     (SetPublicationHook) through which the server closes the SSH
//     connections of users the new snapshot no longer has.
//   - Runtime is a directory server's user source while it runs: the Lookup
//     every front door asks, and the pulls that change it. It reads the
//     enrolment before each pull (one that is missing, for another issuer or
//     organization, or failing a check means nothing is sent and only the
//     local admins can log in), pulls one list at a time (a caller joins a
//     pull not yet started, or waits behind the one running), writes the
//     kept copy (users.json) durably before it publishes the list (and
//     removes the old copy when the new one cannot be written), starts from
//     that copy (Load) only when every check of it passes, and schedules
//     the next pull: the last good pull plus the refresh, or after a
//     failure 15 minutes or the Retry-After when later (at most an hour).
//     Write, then publish keeps the copy on disk never OLDER than the list
//     published, so no restart brings back a person a published list
//     removed. The other direction is allowed on purpose: a server that
//     stops between the write and the publication leaves a copy of a list
//     it never published, the service's own, fully checked answer, and the
//     next start loads it (under every start-up check); that crash window is
//     not a bug. When the trust time of the list ends (or a copy past it is
//     found at start), the copy is removed durably, outside the publication
//     and every lock readers take: at once when a reader finds the end (it
//     tells the runtime without blocking, taking no lock and doing no disk
//     work), at the trust watch's ticks, at each turn of the schedule (where
//     a removal that failed is tried again), and at a graceful stop
//     (CheckTrustAtStop), so a restart starts with only the local admins
//     whatever its clock says. What remains: a process that dies without a
//     graceful stop in the moment between the end of the trust time and the
//     removal of the copy, followed by a clock set back and a restart while
//     the service cannot be reached, brings the list back for what remains
//     of its trust time on that clock; an accurate clock is a prerequisite.
//     A pull's network work is bounded by
//     PullTimeout; the storage work around it is not interruptible and lies
//     outside that bound.
//     Refresh (`shed users refresh`) pulls at once and reports the pull as
//     a row; Status is what an admin is shown.
//   - Every text a Failure takes from an answer has the credential the
//     request carried (the assertion, the access token) replaced by
//     CredentialMarker, so nothing that logs a failure can show it. A
//     directory 200 whose body holds the access token the request carried
//     (as its bytes stand or inside any JSON string) is not a good pull but
//     a failure of its own kind (KindCredentialEchoed), so the token reaches
//     neither the kept copy nor a skipped entry's log line.
//   - The state files (enrolment.json, users.json) are read only after
//     checks of their directory and themselves (ReadStateFile), written
//     durably (WriteStateFile) or once (CreateStateFileOnce), always through
//     the directory checked, opened once. A caller with several steps to do
//     (a lock, a listing, reads, removals, a create) opens the directory
//     once (OpenStateDir, MakeStateDir) and does each through that StateDir.
package directory
