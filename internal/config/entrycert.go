package config

// entrycert.go is the one check of the user a server entry's client
// certificate names, for every Go reader of a stored certificate (the CLI's
// API client and the roost provider's inventory) and for the CLI's check of a
// newly issued one.
//
// The rule (plan 037 §3.1): an entry's own `user` is the only user it may use.
// A stored certificate is presented only when it names that user, or the
// implicit user of a server with no user list. One that names no user (issued
// by a shed-server older than 0.10.0), or names a user the entry does not
// record, is refused with a message that names the one command that fixes it.
// A newly issued certificate that names no user, or whose user cannot be read,
// is refused too, before anything is written.

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"

	"github.com/charliek/shed/internal/servertls"
)

// RefusalKind says why an entry was refused.
type RefusalKind int

const (
	// RefusedStoredNoUser: the stored certificate names no user.
	RefusedStoredNoUser RefusalKind = iota + 1
	// RefusedStoredUserMismatch: the stored certificate names CertUser, and the
	// entry records EntryUser ("" when it records none).
	RefusedStoredUserMismatch
	// RefusedIssuedNoUser: the server issued a certificate that names no user.
	RefusedIssuedNoUser
	// RefusedIssuedUnreadable: the user of the issued certificate cannot be
	// read (it does not parse, or its user attribute is malformed).
	RefusedIssuedUnreadable
	// RefusedIssuedUserChange: the server issued a certificate for CertUser,
	// and the entry is held to EntryUser.
	RefusedIssuedUserChange
	// RefusedEntryChanged: the entry's row changed on disk while the command
	// ran (another process changed its user), so nothing was saved.
	RefusedEntryChanged
	// RefusedEntryUserChanged: a re-enrolment found, under the lock, that the
	// entry now records EntryUser while this process is held to CertUser
	// (another process re-enrolled the entry for another user).
	// The held certificate is withdrawn too (InvalidatesHeld): it is never
	// sent again.
	RefusedEntryUserChanged
	// RefusedRestoreFailed: a certificate write, or the config save after it,
	// failed and the previous files could not be put back (Cause), so the
	// entry's files and its recorded user may now disagree. It withdraws what
	// the process holds.
	RefusedRestoreFailed
	// RefusedEntryUnreadable: a re-enrolment could not read the entry's row
	// under the server's credential lock (Cause: the lock, the config read, or
	// a row that no longer names the endpoint), so it cannot positively check
	// that the row still records the user this process holds. It fails closed:
	// it withdraws what the process holds, and nothing is saved.
	RefusedEntryUnreadable
	// RefusedUserNotRecorded: the server issued a certificate for CertUser to
	// an entry that records no user, and the write that would record it (the
	// read of the previous files, the file write, or the config save: Cause)
	// failed. The certificate is not used: nothing is sent as a user the row
	// does not record. It ends the process's use of the entry too, so the
	// command fails now and says why, rather than carry on with what it held
	// until that is rejected.
	RefusedUserNotRecorded
	// RefusedMintOnlyUnrecorded: a long-running command that never writes the
	// entry (a tunnel daemon) was issued a certificate for CertUser while the
	// entry's row records no user. It never establishes a user in memory: the
	// certificate is not used, nothing is sent as that user, and the command's
	// use of the entry ends. Recording the user is `shed server update
	// <name> --refetch`; the command is then restarted.
	RefusedMintOnlyUnrecorded
)

// EntryRefusedError refuses the use of a server entry, or a certificate issued
// for it. It is terminal for the process: a client built on a refused entry
// sends no request at all (clienttoken.Refusal).
type EntryRefusedError struct {
	// Entry is the config name of the entry ("" when the caller has none).
	Entry string
	Kind  RefusalKind
	// CertUser is the named user the certificate is for ("" for none).
	CertUser string
	// EntryUser is the user the entry records, or is held to ("" for none).
	EntryUser string
	// Cause is the parse error behind RefusedIssuedUnreadable.
	Cause error
}

// EntryRefusal marks the error as a clienttoken.Refusal.
func (e *EntryRefusedError) EntryRefusal() {}

// InvalidatesHeld reports whether the refusal withdraws the credential the
// process already holds for the entry, not only the replacement it was
// minting: the entry now records another user, or its files and its recorded
// user may disagree. A clienttoken.Source ends at once on such a refusal,
// however valid its certificate still is.
func (e *EntryRefusedError) InvalidatesHeld() bool {
	switch e.Kind {
	case RefusedEntryUserChanged, RefusedRestoreFailed, RefusedEntryUnreadable, RefusedUserNotRecorded,
		RefusedMintOnlyUnrecorded:
		return true
	}
	return false
}

// Unwrap returns the parse error behind an unreadable certificate.
func (e *EntryRefusedError) Unwrap() error { return e.Cause }

func (e *EntryRefusedError) Error() string {
	switch e.Kind {
	case RefusedStoredNoUser:
		return fmt.Sprintf("entry %q cannot be used: its certificate names no user "+
			"(issued by a shed-server older than 0.10.0); no request was sent. "+
			"Enrol it again: shed server update %s --refetch", e.Entry, e.Entry)
	case RefusedStoredUserMismatch:
		// The causes include a certificate write that was interrupted (a crash
		// between writing the files and saving config.yaml) or raced by another
		// process: the CLI keeps no journal across the two files, and a pair
		// left beside a row that does not record its user is refused here.
		records := "records no user (written by a shed CLI older than 0.10.0, edited by hand, " +
			"or an interrupted or concurrent enrolment)"
		if e.EntryUser != "" {
			records = fmt.Sprintf("records user %q (edited by hand, or an interrupted or concurrent enrolment)", e.EntryUser)
		}
		return fmt.Sprintf("entry %q cannot be used: its certificate is for user %q but the entry %s; "+
			"no request was sent. Enrol it again: shed server update %s --refetch",
			e.Entry, e.CertUser, records, e.Entry)
	case RefusedIssuedNoUser:
		return "the server issued a certificate that names no user: it runs a shed-server older than 0.10.0; " +
			"nothing was saved. Upgrade the server to 0.10.0, then retry."
	case RefusedIssuedUnreadable:
		return fmt.Sprintf("the server issued a certificate whose user cannot be read (%v); nothing was saved", e.Cause)
	case RefusedIssuedUserChange:
		return fmt.Sprintf("the server issued a certificate for user %q, but entry %q is held to user %q; nothing was saved.\n"+
			"ssh offered a key that belongs to %q. To stay %q, let this entry offer only %s's key "+
			"(shed server update %s --identity-file <%s's private key>) and retry.\n"+
			"To make this entry %q: shed server update %s --refetch --allow-user-change",
			e.CertUser, e.Entry, e.EntryUser,
			e.CertUser, e.EntryUser, e.EntryUser, e.Entry, e.EntryUser,
			e.CertUser, e.Entry)
	case RefusedEntryChanged:
		return fmt.Sprintf("entry %q changed while this command ran; nothing was saved. Run it again.", e.Entry)
	case RefusedEntryUserChanged:
		records := "now records no user"
		if e.EntryUser != "" {
			records = fmt.Sprintf("now records user %q", e.EntryUser)
		}
		return fmt.Sprintf("entry %q %s, but this command was using it as user %q "+
			"(another command changed the entry while this one ran); nothing was saved and this command's "+
			"certificate is no longer used. Run the command again.", e.Entry, records, e.CertUser)
	case RefusedRestoreFailed:
		return fmt.Sprintf("entry %q: a client certificate update did not complete and the previous certificate could not be put back (%v); "+
			"its certificate files and its recorded user may now disagree. Enrol it again: shed server update %s --refetch",
			e.Entry, e.Cause, e.Entry)
	case RefusedEntryUnreadable:
		return fmt.Sprintf("entry %q could not be read to check the user it is held to (%v); "+
			"nothing was saved and this command's certificate is no longer used. "+
			"Make config.yaml and the entry's credential files readable (or, if the entry was removed or changed, "+
			"see shed server list), then run the command again.", e.Entry, e.Cause)
	case RefusedUserNotRecorded:
		return fmt.Sprintf("the server issued a certificate for user %q, but entry %q could not be updated to record that user (%v); "+
			"the certificate was not used and nothing was kept. "+
			"Make config.yaml and the entry's credential files writable, then run the command again.",
			e.CertUser, e.Entry, e.Cause)
	case RefusedMintOnlyUnrecorded:
		return fmt.Sprintf("the server issued a certificate for user %q, but entry %q records no user, "+
			"and this long-running command (a tunnel) never records one; the certificate was not used and "+
			"nothing was sent as that user. Record the user: shed server update %s --refetch, "+
			"then restart the tunnel (shed tunnels stop, then shed tunnels start).",
			e.CertUser, e.Entry, e.Entry)
	default:
		return fmt.Sprintf("entry %q cannot be used", e.Entry)
	}
}

// AsEntryRefused returns err's *EntryRefusedError, if it is or wraps one.
func AsEntryRefused(err error) (*EntryRefusedError, bool) {
	var r *EntryRefusedError
	if errors.As(err, &r) {
		return r, true
	}
	return nil, false
}

// LoadEntryCertificate loads a server entry's stored client certificate and
// key, as LoadClientCredentials does (under the credential lock keyed on name;
// "" loads unlocked), and returns it only when it may be presented for this
// entry: it names the entry's User, or the implicit user.
//
// A certificate that names no user, or names a user the entry does not record,
// returns *EntryRefusedError (with Entry set to name): the entry must not be
// used until it is enrolled again. Every other failure — no files, a pair that
// does not load, a user attribute that is malformed — returns an ordinary
// error, which callers treat as "no credential on file" (the CLI re-enrols).
func LoadEntryCertificate(name string, e *ServerEntry) (*tls.Certificate, error) {
	if e.ClientCertFile == "" || e.ClientKeyFile == "" {
		return nil, errors.New("no client certificate on file")
	}
	cert, err := LoadClientCredentials(name, e.ClientCertFile, e.ClientKeyFile)
	if err != nil {
		return nil, err
	}
	leaf := cert.Leaf
	if leaf == nil {
		if len(cert.Certificate) == 0 {
			return nil, errors.New("the stored client certificate is empty")
		}
		if leaf, err = x509.ParseCertificate(cert.Certificate[0]); err != nil {
			return nil, fmt.Errorf("parse the stored client certificate: %w", err)
		}
	}
	user, bound, err := servertls.ClientCertUser(leaf)
	switch {
	case err != nil:
		return nil, fmt.Errorf("read the user of the stored client certificate: %w", err)
	case !bound:
		return nil, &EntryRefusedError{Entry: name, Kind: RefusedStoredNoUser, EntryUser: e.User}
	case user == "":
		// The implicit user: a server with no user list. It never names
		// another user, so it is presented whatever the entry records.
		return cert, nil
	case user != e.User:
		return nil, &EntryRefusedError{Entry: name, Kind: RefusedStoredUserMismatch, CertUser: user, EntryUser: e.User}
	}
	return cert, nil
}

// IssuedCertificateUser reads the user a newly issued client certificate (PEM)
// names: user is the named user ("" with implicit true for the implicit user).
// A certificate that names no user returns RefusedIssuedNoUser, and one whose
// user cannot be read (it does not parse, or the attribute is malformed)
// returns RefusedIssuedUnreadable: both are refused, before anything is
// written. The returned error's Entry is entry.
func IssuedCertificateUser(entry string, certPEM []byte) (user string, implicit bool, refusal *EntryRefusedError) {
	block, _ := pem.Decode(certPEM)
	if block == nil || block.Type != "CERTIFICATE" {
		return "", false, &EntryRefusedError{Entry: entry, Kind: RefusedIssuedUnreadable,
			Cause: errors.New("not a PEM CERTIFICATE block")}
	}
	leaf, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return "", false, &EntryRefusedError{Entry: entry, Kind: RefusedIssuedUnreadable, Cause: err}
	}
	user, bound, err := servertls.ClientCertUser(leaf)
	switch {
	case err != nil:
		return "", false, &EntryRefusedError{Entry: entry, Kind: RefusedIssuedUnreadable, Cause: err}
	case !bound:
		return "", false, &EntryRefusedError{Entry: entry, Kind: RefusedIssuedNoUser}
	case user == "":
		return "", true, nil
	}
	return user, false, nil
}

// HeldUserAccepts reports whether an entry held to held may adopt a newly
// issued certificate for user (implicit for the implicit user): no held user,
// the implicit user, or the same name. Otherwise it is a user change, which
// only an explicit --allow-user-change makes.
func HeldUserAccepts(held, user string, implicit bool) bool {
	return held == "" || implicit || held == user
}
