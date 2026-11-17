package main

import (
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/charliek/shed/internal/config"
	"github.com/charliek/shed/internal/directory"
)

// Seams for tests of a directory server: the wall clock its lookup and
// runtime run on, the ticks of its trust watch, the timers of its schedule
// and the bound of one pull. nil (and 0) are the real ones.
var (
	directoryNow         func() time.Time
	directoryTrustTicks  func() (<-chan time.Time, func())
	directoryTimer       func(time.Duration) (<-chan time.Time, func())
	directoryPullTimeout time.Duration
)

// newDirectoryUsers builds a directory server's user source
// (auth.directory): the lookup of the local admins (auth.users beside the
// directory) and the list, and the runtime that pulls the list. It reads the
// enrolment and the kept copy in <stateDir>/directory and publishes the
// copy's list when it may be used (directory.Runtime.Load). It sends
// nothing: the first pull starts with StartRefresh.
func newDirectoryUsers(cfg *config.ServerConfig, stateDir string) (*directory.Runtime, error) {
	d := cfg.Auth.Directory
	clients := make([]string, 0, len(cfg.Auth.Clients))
	for name := range cfg.Auth.Clients {
		clients = append(clients, name)
	}
	sort.Strings(clients)
	maxTries := 0
	if s := cfg.SSHAuth(); s != nil {
		maxTries = s.MaxAuthTries
	}
	lookup, err := directory.NewLookup(directory.LookupOptions{
		LocalAdmins:    cfg.Auth.Users,
		ServiceClients: clients,
		MaxAuthTries:   maxTries,
		Now:            directoryNow,
		TrustTicks:     directoryTrustTicks,
	})
	if err != nil {
		return nil, err
	}
	rt, err := directory.NewRuntime(directory.RuntimeOptions{
		Lookup:         lookup,
		StateDir:       filepath.Join(stateDir, directory.StateDirName),
		Issuer:         d.Issuer(),
		Organization:   d.Organization,
		AllowPlainHTTP: bool(d.AllowPlainHTTP),
		Refresh:        d.RefreshInterval(),
		Trust:          d.TrustPeriod(),
		Timer:          directoryTimer,
		PullTimeout:    directoryPullTimeout,
	})
	if err != nil {
		return nil, err
	}
	rt.Load()
	return rt, nil
}

// directoryStartupLines are serve's start-up lines on a directory server
// (plan section 3.10): the source and its state (the runtime's), the local
// admins, and a warning when every local admin key has expired at now.
func directoryStartupLines(cfg *config.ServerConfig, rt *directory.Runtime, now time.Time) []string {
	lines := rt.StartupLines()
	names := cfg.UserNames()
	lines = append(lines, "Local admins: "+strings.Join(names, ", "))
	total := 0
	for _, name := range names {
		total += len(cfg.Auth.Users[name].Keys)
	}
	if expired := cfg.ExpiredUserKeys(now); total > 0 && len(expired) == total {
		lines = append(lines, fmt.Sprintf("WARNING: every local admin key has expired (%s): no local admin can log in, and with the account service unreachable nobody can", strings.Join(expired, ", ")))
	}
	return lines
}

// directorySummary is what config-validate prints about auth.directory: the
// service, the organization, refresh and trust, and the local admins. Nil
// without a directory.
func directorySummary(cfg *config.ServerConfig) []string {
	if !cfg.DirectoryConfigured() {
		return nil
	}
	d := cfg.Auth.Directory
	org := "the organization named at enrolment"
	if d.Organization != "" {
		org = "organization " + d.Organization
	}
	return []string{
		fmt.Sprintf("users: from the directory at %s (%s), refresh %s, trust %s", d.Issuer(), org, d.RefreshInterval(), d.TrustPeriod()),
		fmt.Sprintf("local admins: %s", strings.Join(cfg.UserNames(), ", ")),
	}
}
