package main

import (
	"errors"
	"fmt"
	"strings"

	"github.com/charliek/shed/internal/config"
)

// errDirectoryUnavailable is serve's refusal of an auth.directory block:
// config load accepts and checks the block (so config-validate can), but this
// build has no directory runtime to answer logins from it.
var errDirectoryUnavailable = errors.New("auth.directory: the directory is not available in this build; remove the auth.directory block to start the server")

// loadServeConfig is serve's config load: loadConfig, then the refusal of an
// auth.directory block, which this build cannot serve. It runs before
// anything binds.
func loadServeConfig() (*config.ServerConfig, error) {
	cfg, err := loadConfig()
	if err != nil {
		return nil, err
	}
	if cfg.DirectoryConfigured() {
		return nil, errDirectoryUnavailable
	}
	return cfg, nil
}

// directorySummary is what config-validate prints about auth.directory: the
// service, the organization, refresh and trust, the local admins, and that
// serve refuses the block in this build. Nil without a directory.
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
		fmt.Sprintf("note: %v", errDirectoryUnavailable),
	}
}
