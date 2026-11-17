package config

import (
	"fmt"
	"net"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"

	"gopkg.in/yaml.v3"
)

// DirectoryConfigured reports whether the server takes its users from a
// directory (an auth.directory block).
func (c *ServerConfig) DirectoryConfigured() bool {
	return c.Auth != nil && c.Auth.Directory != nil
}

// Issuer is URL with one trailing "/" dropped: the issuer the server checks
// and the audience of its sign-in, byte for byte.
func (d *DirectoryConfig) Issuer() string { return strings.TrimSuffix(d.URL, "/") }

// RefreshInterval is Refresh, or DefaultDirectoryRefresh when omitted. A
// written zero stays zero (and fails validation).
func (d *DirectoryConfig) RefreshInterval() time.Duration {
	if d.Refresh != nil {
		return d.Refresh.Duration()
	}
	return DefaultDirectoryRefresh
}

// TrustPeriod is Trust, or DefaultDirectoryTrust when omitted. A written zero
// stays zero (and fails validation).
func (d *DirectoryConfig) TrustPeriod() time.Duration {
	if d.Trust != nil {
		return d.Trust.Duration()
	}
	return DefaultDirectoryTrust
}

// directoryFieldNames lists auth.directory's keys, for the refusal of any
// other.
const directoryFieldNames = "url, organization, refresh, trust and allow_plain_http"

// UnmarshalYAML decodes auth.directory by walking its mapping, so that:
//   - an omitted refresh or trust (nil, the default) is told apart from one
//     written as zero (out of range, refused by validateDirectory);
//   - every value that does not decode is refused with its field's name
//     (auth.directory.refresh, .trust, .allow_plain_http, .url,
//     .organization);
//   - an unknown key, a key written twice, a merge key (<<) and a key with
//     no value are refused here, since the strict decode (KnownFields) and
//     the library's duplicate-key check do not reach inside a type with its
//     own unmarshaller.
func (d *DirectoryConfig) UnmarshalYAML(n *yaml.Node) error {
	n = resolveYAMLAlias(n)
	if n.Kind != yaml.MappingNode {
		return fmt.Errorf("auth.directory (line %d) must be a mapping of %s", n.Line, directoryFieldNames)
	}
	var out DirectoryConfig
	seen := make(map[string]int, len(n.Content)/2)
	for i := 0; i+1 < len(n.Content); i += 2 {
		k, v := n.Content[i], resolveYAMLAlias(n.Content[i+1])
		if k.Kind != yaml.ScalarNode {
			return fmt.Errorf("auth.directory (line %d): a key must be one of %s", k.Line, directoryFieldNames)
		}
		if k.ShortTag() == "!!merge" {
			return fmt.Errorf("auth.directory (line %d): a merge key (<<) is not accepted here; write each field out", k.Line)
		}
		field := "auth.directory." + k.Value
		switch k.Value {
		case "url", "organization", "refresh", "trust", "allow_plain_http":
		default:
			return fmt.Errorf("auth.directory (line %d): unknown field %q; the fields are %s (keys are case-sensitive)", k.Line, k.Value, directoryFieldNames)
		}
		if first, dup := seen[k.Value]; dup {
			return fmt.Errorf("%s (line %d) is written twice (first at line %d)", field, k.Line, first)
		}
		seen[k.Value] = k.Line
		if v.Kind == yaml.ScalarNode && v.ShortTag() == "!!null" {
			return fmt.Errorf("%s (line %d) has no value; write one or remove the line", field, k.Line)
		}
		var err error
		switch k.Value {
		case "url":
			err = decodeYAMLString(v, &out.URL)
		case "organization":
			err = decodeYAMLString(v, &out.Organization)
		case "refresh":
			out.Refresh = new(Duration)
			err = v.Decode(out.Refresh)
		case "trust":
			out.Trust = new(Duration)
			err = v.Decode(out.Trust)
		case "allow_plain_http":
			err = v.Decode(&out.AllowPlainHTTP)
		}
		if err != nil {
			return fmt.Errorf("%s (line %d): %w", field, k.Line, err)
		}
	}
	*d = out
	return nil
}

// resolveYAMLAlias follows an alias node to the node it names.
func resolveYAMLAlias(n *yaml.Node) *yaml.Node {
	for n.Kind == yaml.AliasNode && n.Alias != nil {
		n = n.Alias
	}
	return n
}

// decodeYAMLString decodes a YAML string scalar into s. Any other scalar
// (a number, a boolean) or a collection is refused, so a value like true or
// 7 is never read as the text of a URL or an organization.
func decodeYAMLString(n *yaml.Node, s *string) error {
	if n.Kind != yaml.ScalarNode {
		return fmt.Errorf("a list or a mapping is not a string")
	}
	if n.ShortTag() != "!!str" {
		return fmt.Errorf("%s is not a string; quote it if it is meant as one", n.Value)
	}
	*s = n.Value
	return nil
}

// directoryOrganizationPattern is the shape of an organization name in the
// directory contract.
var directoryOrganizationPattern = regexp.MustCompile(`^[a-z][a-z0-9-]{0,62}$`)

// validateDirectory checks auth.directory. Called from validateAuth beside
// validateUsers (which has already checked auth.users as a user list), so
// both loaders run it.
//
// Beside a directory, auth.users holds the local admins: at least one, each
// an admin, with keys only (no GitHub login, no script key). The URL is an
// origin; plain HTTP is allowed only when asked for and only to a loopback
// host. Refresh and trust are bounded. Unknown and repeated keys under
// auth.directory, and values that do not decode, are refused earlier, by
// DirectoryConfig.UnmarshalYAML.
func (c *ServerConfig) validateDirectory() error {
	if !c.DirectoryConfigured() {
		return nil
	}
	d := c.Auth.Directory
	if !c.MTLSMode() {
		return fmt.Errorf("auth.directory requires auth.mode: mtls")
	}
	if !c.UsersConfigured() {
		return fmt.Errorf("auth.directory needs at least one local admin in auth.users (admin: true, with a key): the local admins are the users who can log in whatever state the directory is in")
	}
	for _, name := range c.UserNames() {
		u := c.Auth.Users[name]
		at := "auth.users." + name
		if !u.Admin {
			return fmt.Errorf("%s: beside auth.directory every auth.users entry is a local admin; set %s.admin: true (other people come from the directory)", at, at)
		}
		if u.GitHub != "" {
			return fmt.Errorf("%s.github: a local admin beside auth.directory has keys only; remove the github login and list the keys", at)
		}
		if u.GitHubID != 0 {
			return fmt.Errorf("%s.github_id: a local admin beside auth.directory has keys only; remove github_id", at)
		}
		for i, k := range u.Keys {
			if k.Script {
				return fmt.Errorf("%s.keys[%d] (%s).script: a local admin beside auth.directory has no script keys; remove script: true", at, i, k.Label)
			}
		}
	}
	if err := validateDirectoryURL(d); err != nil {
		return err
	}
	if d.Organization != "" && !directoryOrganizationPattern.MatchString(d.Organization) {
		return fmt.Errorf("auth.directory.organization %q is not a valid organization name (must match %s)", d.Organization, directoryOrganizationPattern)
	}
	// Only an omitted refresh or trust takes the default; a written zero is
	// out of range like any other value below the minimum.
	refresh, trust := d.RefreshInterval(), d.TrustPeriod()
	if refresh < MinDirectoryRefresh || refresh > MaxDirectoryRefresh {
		return fmt.Errorf("auth.directory.refresh %s is out of range (from %s to %s; omit it for the default, %s)", refresh, MinDirectoryRefresh, MaxDirectoryRefresh, DefaultDirectoryRefresh)
	}
	if trust < refresh || trust > MaxDirectoryTrust {
		return fmt.Errorf("auth.directory.trust %s is out of range (from auth.directory.refresh, %s, to %s; omitted, it is %s)", trust, refresh, MaxDirectoryTrust, DefaultDirectoryTrust)
	}
	return nil
}

// validateDirectoryURL checks auth.directory.url and allow_plain_http: the
// URL is an origin written plainly, https, or http only with
// allow_plain_http on a loopback host; allow_plain_http itself is refused
// beside a host that is not loopback.
func validateDirectoryURL(d *DirectoryConfig) error {
	raw := d.URL
	if raw == "" {
		return fmt.Errorf("auth.directory.url is required (the service's issuer, for example https://accounts.example.test)")
	}
	if strings.IndexFunc(raw, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) }) >= 0 {
		return fmt.Errorf("auth.directory.url %q holds a space or a control character", raw)
	}
	issuer := d.Issuer()
	var scheme string
	switch {
	case strings.HasPrefix(raw, "https://"):
		scheme = "https"
	case strings.HasPrefix(raw, "http://"):
		scheme = "http"
	default:
		return fmt.Errorf("auth.directory.url %q must start with https:// (or http:// with allow_plain_http, loopback only)", raw)
	}
	if raw == scheme+"://" {
		return fmt.Errorf("auth.directory.url %q has no host", raw)
	}
	u, err := url.Parse(issuer)
	if err != nil {
		return fmt.Errorf("auth.directory.url %q does not parse: %w", raw, err)
	}
	switch {
	case u.User != nil:
		return fmt.Errorf("auth.directory.url %q carries user info; it must be an origin (scheme, host, optional port)", raw)
	case u.Path != "" || u.RawPath != "" || u.Opaque != "":
		return fmt.Errorf("auth.directory.url %q has a path; it must be an origin (scheme, host, optional port)", raw)
	case u.RawQuery != "" || u.ForceQuery || strings.Contains(issuer, "?"):
		return fmt.Errorf("auth.directory.url %q has a query; it must be an origin (scheme, host, optional port)", raw)
	case u.Fragment != "" || strings.Contains(issuer, "#"):
		return fmt.Errorf("auth.directory.url %q has a fragment; it must be an origin (scheme, host, optional port)", raw)
	case u.Hostname() == "":
		return fmt.Errorf("auth.directory.url %q has no host", raw)
	}
	if strings.HasSuffix(u.Host, ":") {
		return fmt.Errorf("auth.directory.url %q has an empty port; remove the ':' or name a port", raw)
	}
	if p := u.Port(); p != "" {
		if n, err := strconv.Atoi(p); err != nil || n < 1 || n > 65535 || strconv.Itoa(n) != p {
			return fmt.Errorf("auth.directory.url %q has an invalid port %q", raw, p)
		}
	}
	// The issuer is compared byte for byte, so its host must have one
	// spelling only: plain ASCII, an IPv4 address or a bracketed IPv6
	// address, never a percent-escape (which the parser decodes, so an
	// invisible or space character could hide in it).
	if err := checkDirectoryHost(raw, strings.TrimPrefix(issuer, scheme+"://")); err != nil {
		return err
	}
	// Belt and braces: with the host checked, the parser's own spelling of
	// the origin is the issuer itself.
	if plain := (&url.URL{Scheme: u.Scheme, Host: u.Host}).String(); plain != issuer {
		return fmt.Errorf("auth.directory.url %q is not written plainly; write it as scheme://host or scheme://host:port, the host in ASCII letters, digits, '-' and '.'", raw)
	}
	loopback := isLoopbackDirectoryHost(u.Hostname())
	allowPlain := bool(d.AllowPlainHTTP)
	if allowPlain && !loopback {
		return fmt.Errorf("auth.directory.allow_plain_http is for a loopback host only (localhost, 127.0.0.0/8, ::1); %q is not one", u.Hostname())
	}
	if scheme == "http" && !allowPlain {
		return fmt.Errorf("auth.directory.url %q is plain HTTP; use https, or set auth.directory.allow_plain_http: true for a loopback host (tests only)", raw)
	}
	return nil
}

// directoryHostRule is how a refusal of the host says what is accepted.
const directoryHostRule = "write the host plainly: ASCII letters, digits, '-' and '.' (an internationalized name in its xn-- form), an IPv4 address, or an IPv6 address in brackets"

// checkDirectoryHost checks the authority (host and optional port) of an
// auth.directory.url that has already parsed as an origin: no percent sign
// anywhere, and the host either a bracketed IPv6 address or made of ASCII
// letters, digits, '-' and '.' only (which an IPv4 address is). The port
// was checked by the caller.
func checkDirectoryHost(raw, authority string) error {
	if strings.Contains(authority, "%") {
		return fmt.Errorf("auth.directory.url %q has a percent sign in its host; %s", raw, directoryHostRule)
	}
	if strings.HasPrefix(authority, "[") {
		end := strings.IndexByte(authority, ']')
		if end < 0 {
			return fmt.Errorf("auth.directory.url %q has an unclosed '[' in its host; %s", raw, directoryHostRule)
		}
		inner := authority[1:end]
		if !strings.Contains(inner, ":") || net.ParseIP(inner) == nil {
			return fmt.Errorf("auth.directory.url %q: %q in brackets is not an IPv6 address; %s", raw, inner, directoryHostRule)
		}
		return nil
	}
	host, _, _ := strings.Cut(authority, ":")
	for i := 0; i < len(host); i++ {
		if !isPlainHostByte(host[i]) {
			return fmt.Errorf("auth.directory.url %q: the host %q holds a character other than ASCII letters, digits, '-' and '.'; %s", raw, host, directoryHostRule)
		}
	}
	return nil
}

// isPlainHostByte reports whether c may appear in a plain host name: an
// ASCII letter, a digit, '-' or '.'.
func isPlainHostByte(c byte) bool {
	switch {
	case 'a' <= c && c <= 'z', 'A' <= c && c <= 'Z', '0' <= c && c <= '9':
		return true
	}
	return c == '-' || c == '.'
}

// isLoopbackDirectoryHost reports whether host (a URL's Hostname) is
// localhost or a loopback address (127.0.0.0/8, ::1).
func isLoopbackDirectoryHost(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
