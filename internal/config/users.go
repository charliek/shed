package config

import (
	"fmt"
	"sort"
	"strings"
	"time"

	gossh "golang.org/x/crypto/ssh"
	"gopkg.in/yaml.v3"
)

// UsersConfigured reports whether the server has a user list (auth.users with
// at least one user). Without one every accepted key belongs to the implicit
// user, an admin.
func (c *ServerConfig) UsersConfigured() bool {
	return c.Auth != nil && len(c.Auth.Users) > 0
}

// UserNames returns the configured user names, sorted.
func (c *ServerConfig) UserNames() []string {
	if !c.UsersConfigured() {
		return nil
	}
	names := make([]string, 0, len(c.Auth.Users))
	for name := range c.Auth.Users {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// ParseUserKey parses one auth.users key line: exactly one OpenSSH
// authorized_keys entry, with no options (from=, command=, ...) and nothing
// after it. Options are refused rather than dropped: the server would not
// apply them, and an operator who wrote one expects it to hold.
func ParseUserKey(line string) (gossh.PublicKey, error) {
	data := []byte(strings.TrimSpace(line))
	if len(data) == 0 {
		return nil, fmt.Errorf("key is empty")
	}
	if strings.ContainsAny(string(data), "\r\n") {
		return nil, fmt.Errorf("key must be one line")
	}
	key, _, options, rest, err := gossh.ParseAuthorizedKey(data)
	if err != nil {
		return nil, fmt.Errorf("key is not an OpenSSH public key line: %w", err)
	}
	if len(options) > 0 {
		return nil, fmt.Errorf("key carries authorized_keys options (%s); they are not supported, remove them", strings.Join(options, ","))
	}
	if len(strings.TrimSpace(string(rest))) > 0 {
		return nil, fmt.Errorf("key line holds more than one key")
	}
	// A certificate is refused: its fingerprint is not its key's, so the
	// key could be listed again, raw or in another certificate, under
	// another user (or without script: true), and whoever holds the private
	// key could pick which of them to be. Nor would the server check its
	// validity, principals or critical options.
	if cert, ok := key.(*gossh.Certificate); ok {
		return nil, fmt.Errorf("key is an SSH certificate (%s), which auth.users does not accept; list the public key itself (%s)", cert.Type(), cert.Key.Type())
	}
	return key, nil
}

// StrictBool is a config flag that takes a YAML boolean only: true or false,
// unquoted (True and TRUE too, as YAML 1.2 has them). The YAML library also
// turns the YAML 1.1 words yes, no, on, off, y and n into booleans, quoted or
// not; for auth.users.admin and script a value like that is refused instead,
// so what grants admin is never a string that reads like a word.
type StrictBool bool

// UnmarshalYAML implements yaml.Unmarshaler.
func (b *StrictBool) UnmarshalYAML(n *yaml.Node) error {
	if n.Kind != yaml.ScalarNode || n.ShortTag() != "!!bool" {
		return fmt.Errorf("line %d: %q is not a boolean; write true or false, unquoted", n.Line, n.Value)
	}
	var v bool
	if err := n.Decode(&v); err != nil {
		return err
	}
	if lower := strings.ToLower(n.Value); lower != "true" && lower != "false" {
		return fmt.Errorf("line %d: %q is not a boolean; write true or false, unquoted", n.Line, n.Value)
	}
	*b = StrictBool(v)
	return nil
}

// validateUsers checks auth.users. Called from validateAuth, so both loaders
// run it.
//
// A user list needs mtls (the only mode in which keys are enforced and each
// request carries its key's identity), and replaces auth.ssh's key sources:
// with both, a key in auth.ssh would belong to nobody, so the combination is
// refused rather than guessed at. auth.ssh.github_refresh and max_auth_tries
// still apply.
//
// Each user needs a GitHub login, a key, or both. A key belongs to one user
// only: a key listed twice, under one user or two, is refused, and so are two
// users naming the same GitHub login (logins are case-insensitive). At least
// one admin must be able to act as a person: one with a GitHub login or a key
// not marked script. A key whose expiry has passed is not an error here (it
// is simply not accepted; the server logs it at start, and warns when no
// admin is left with a credential that works).
func (c *ServerConfig) validateUsers() error {
	if !c.UsersConfigured() {
		return nil
	}
	if !c.MTLSMode() {
		return fmt.Errorf("auth.users requires auth.mode: mtls")
	}
	if ssh := c.SSHAuth(); ssh != nil {
		var sources []string
		if len(ssh.AuthorizedKeys) > 0 {
			sources = append(sources, "auth.ssh.authorized_keys")
		}
		if ssh.AuthorizedKeysFile != "" {
			sources = append(sources, "auth.ssh.authorized_keys_file")
		}
		if len(ssh.GitHubUsers) > 0 {
			sources = append(sources, "auth.ssh.github_users")
		}
		if len(sources) > 0 {
			return fmt.Errorf("auth.users cannot be combined with %s: with a user list every key belongs to a user, so list each key (or GitHub login) under its user in auth.users and remove %s",
				strings.Join(sources, ", "), strings.Join(sources, ", "))
		}
	}

	keyOwner := map[string]string{} // fingerprint -> "user (label)"
	loginOwner := map[string]string{}
	admins := 0
	for _, name := range c.UserNames() {
		u := c.Auth.Users[name]
		at := "auth.users." + name
		if !ValidUserName(name) {
			return fmt.Errorf("invalid auth.users name %q (must match %s)", name, userNameRegex)
		}
		if _, clash := c.Auth.Clients[name]; clash {
			return fmt.Errorf("auth.users name %q is also an auth.clients name; a user and a service client must have different names", name)
		}
		if bool(u.Admin) && u.CanAdminister() {
			admins++
		}
		if u.GitHub == "" && len(u.Keys) == 0 {
			return fmt.Errorf("%s needs a github login or at least one key", at)
		}
		if u.GitHub != "" {
			if !ValidGitHubUsername(u.GitHub) {
				return fmt.Errorf("%s.github %q is not a valid GitHub login", at, u.GitHub)
			}
			lower := strings.ToLower(u.GitHub)
			if other, dup := loginOwner[lower]; dup {
				return fmt.Errorf("auth.users.%s and auth.users.%s name the same GitHub login %q (logins are case-insensitive); a login belongs to one user", other, name, u.GitHub)
			}
			loginOwner[lower] = name
		}
		switch {
		case u.GitHubID < 0:
			return fmt.Errorf("%s.github_id must be a positive GitHub account id", at)
		case u.GitHubID > 0 && u.GitHub == "":
			return fmt.Errorf("%s.github_id needs %s.github, the login it pins", at, at)
		}
		labels := map[string]bool{}
		for i, k := range u.Keys {
			kat := fmt.Sprintf("%s.keys[%d]", at, i)
			if k.Label == "" {
				return fmt.Errorf("%s.label is required", kat)
			}
			if labels[k.Label] {
				return fmt.Errorf("%s.label %q is used twice; labels are unique per user", kat, k.Label)
			}
			labels[k.Label] = true
			key, err := ParseUserKey(k.Key)
			if err != nil {
				return fmt.Errorf("%s (%s): %w", kat, k.Label, err)
			}
			if _, err := ParseKeyExpiry(k.Expires); err != nil {
				return fmt.Errorf("%s (%s): %w", kat, k.Label, err)
			}
			fp := gossh.FingerprintSHA256(key)
			here := fmt.Sprintf("auth.users.%s (%s)", name, k.Label)
			if first, dup := keyOwner[fp]; dup {
				return fmt.Errorf("key %s is listed under %s and %s; a key belongs to one user and is listed once", fp, first, here)
			}
			keyOwner[fp] = here
		}
	}
	if admins == 0 {
		return fmt.Errorf("auth.users needs at least one admin (admin: true) with a github login or a key that is not marked script")
	}
	return nil
}

// CanAdminister reports whether u has a credential a person administers
// with: a GitHub login, or a key not marked script. Expiry is not looked at
// (config load does not depend on the time).
func (u UserConfig) CanAdminister() bool {
	if u.GitHub != "" {
		return true
	}
	for _, k := range u.Keys {
		if !k.Script {
			return true
		}
	}
	return false
}

// ExpiredUserKeys lists the auth.users keys whose expiry has passed at now,
// as "user (label)", for a start-up warning.
func (c *ServerConfig) ExpiredUserKeys(now time.Time) []string {
	var out []string
	for _, name := range c.UserNames() {
		for _, k := range c.Auth.Users[name].Keys {
			if t, err := ParseKeyExpiry(k.Expires); err == nil && t != nil && !now.Before(*t) {
				out = append(out, fmt.Sprintf("%s (%s)", name, k.Label))
			}
		}
	}
	return out
}
