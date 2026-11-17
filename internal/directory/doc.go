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
package directory
