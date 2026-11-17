package directory

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// CopyFormat is users.json's format number.
const CopyFormat = 1

// copyFile is users.json, the kept copy of the last good pull: the answer's
// bytes as they arrived (Body, in standard base64) and what they were
// fetched under.
type copyFile struct {
	Format int `json:"format"`
	// FetchedAt is when the answer's headers arrived, on the wall clock
	// (Pull.FetchedAt): the instant its trust time is counted from.
	FetchedAt    time.Time `json:"fetched_at"`
	Issuer       string    `json:"issuer"`
	ClientID     string    `json:"client_id"`
	Organization string    `json:"organization"`
	Version      string    `json:"version"`
	Body         string    `json:"body"`
}

var copyFileMembers = []string{"format", "fetched_at", "issuer", "client_id", "organization", "version", "body"}

// marshalCopy is the kept copy of pull p, made under enrolment e.
func marshalCopy(e *Enrolment, p *Pull) ([]byte, error) {
	out, err := json.MarshalIndent(copyFile{
		Format:       CopyFormat,
		FetchedAt:    p.FetchedAt.Round(0).UTC(),
		Issuer:       e.Issuer,
		ClientID:     e.ClientID,
		Organization: p.List.Organization,
		Version:      p.List.Version,
		Body:         base64.StdEncoding.EncodeToString(p.Body),
	}, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(out, '\n'), nil
}

// keptCopy is a kept copy read back and accepted: its list and when it was
// fetched.
type keptCopy struct {
	list      *List
	fetchedAt time.Time
}

// errCopyPastTrust is a kept copy whose trust time has ended; the copy is
// otherwise sound.
var errCopyPastTrust = errors.New("the kept copy is past its trust time")

// parseCopy decodes users.json and checks it may be used at now, under
// enrolment e, with organization the accepted one (rule 5: the configured
// organization or the enrolment's) and trust today's trust time:
//
//   - strictly decoded: one object, no member unknown or named twice,
//     nothing after it, format 1;
//   - its issuer and client id are the enrolment's;
//   - its organization is the accepted one;
//   - its fetched_at is set and not after now (no tolerance);
//   - its body decodes as base64 and passes today's rules (DecodeList), and
//     names the copy's organization and version;
//   - now is before fetched_at + trust: otherwise the copy is returned with
//     errCopyPastTrust, for the caller to report its deadline.
func parseCopy(data []byte, e *Enrolment, organization string, trust time.Duration, now time.Time) (*keptCopy, error) {
	var f copyFile
	if err := decodeStrictObject(data, copyFileMembers, &f); err != nil {
		return nil, fmt.Errorf("not a kept copy: %w", err)
	}
	switch {
	case f.Format != CopyFormat:
		return nil, fmt.Errorf("format %d is not %d", f.Format, CopyFormat)
	case f.Issuer != e.Issuer:
		return nil, fmt.Errorf("it was pulled from the issuer %q, not this server's enrolment's %q", f.Issuer, e.Issuer)
	case f.ClientID != e.ClientID:
		return nil, fmt.Errorf("it was pulled by the client id %q, not this server's enrolment's %q", f.ClientID, e.ClientID)
	case f.Organization != organization:
		return nil, fmt.Errorf("it names the organization %q, not the accepted %q", f.Organization, organization)
	case f.FetchedAt.IsZero():
		return nil, errors.New("it has no fetched_at")
	}
	fetchedAt := f.FetchedAt.Round(0)
	if fetchedAt.After(now) {
		return nil, fmt.Errorf("its fetched_at %s is after this server's clock (%s)",
			fetchedAt.UTC().Format(time.RFC3339Nano), now.UTC().Format(time.RFC3339Nano))
	}
	body, err := base64.StdEncoding.Strict().DecodeString(f.Body)
	if err != nil {
		return nil, fmt.Errorf("its body is not base64: %w", err)
	}
	if len(body) > MaxDirectoryBody {
		return nil, fmt.Errorf("its body is over the %d bytes an answer may be", MaxDirectoryBody)
	}
	list, err := DecodeList(body, organization)
	if err != nil {
		return nil, fmt.Errorf("its body is refused: %w", err)
	}
	if list.Version != f.Version {
		return nil, fmt.Errorf("its body's version is not the copy's %q", f.Version)
	}
	c := &keptCopy{list: list, fetchedAt: fetchedAt}
	if !now.Before(fetchedAt.Add(trust)) {
		return c, errCopyPastTrust
	}
	return c, nil
}
