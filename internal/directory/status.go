package directory

import (
	"time"

	"github.com/charliek/shed/internal/config"
)

// UsersStatus is the state of the source as GET /api/users/status answers it
// (`shed users status`): Status, in the API's terms. It holds no key and no
// label. The API asks for it through an interface of its own, so it needs
// nothing of this package.
//
// The local-admin key warning (config.DirectoryStatus.ExpiredLocalAdmins) is
// not set here: it is the server's config, which the API holds.
func (r *Runtime) UsersStatus() config.UsersStatusResponse {
	return usersStatus(r.Status())
}

// usersStatus is st in the API's terms.
func usersStatus(st Status) config.UsersStatusResponse {
	d := &config.DirectoryStatus{
		URL:          st.Issuer,
		Organization: st.Organization,
		ClientID:     st.ClientID,
		State:        string(st.State),
		Reason:       st.Reason,
		LastGoodPull: instant(st.LastGoodPull),
		Version:      st.Version,
		TrustedUntil: instant(st.TrustedUntil),
		LastAttempt:  instant(st.LastAttempt),
		LastError:    st.LastError,
		NextAttempt:  instant(st.NextAttempt),
		Copy:         string(st.Copy),
		CopyError:    st.CopyError,
		Skipped:      make([]config.SkippedEntry, 0, len(st.Skipped)),
	}
	for _, s := range st.Skipped {
		d.Skipped = append(d.Skipped, config.SkippedEntry{Name: s.Name, Reason: string(s.Reason), Description: skipWords(s.Reason)})
	}
	us := make([]config.UserStatus, 0, len(st.Users))
	for _, u := range st.Users {
		source := string(u.Source)
		switch u.Source {
		case UserLocal:
			source = config.UserFromLocal
		case UserDirectory:
			source = config.UserFromDirectory
		}
		us = append(us, config.UserStatus{Name: u.Name, Admin: u.Admin, Source: source})
	}
	return config.UsersStatusResponse{Source: config.UsersSourceDirectory, Directory: d, Users: us}
}

// instant is t in UTC, or nil for the zero time (none).
func instant(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	u := t.UTC()
	return &u
}
