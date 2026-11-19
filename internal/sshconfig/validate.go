package sshconfig

import (
	"errors"
	"fmt"
	"strings"

	"github.com/charliek/shed/internal/config"
	sdkbootstrap "github.com/charliek/shed/sdk/bootstrap"
)

// EntryError is an Entry the writers refuse to put into an ssh config: one of
// its values could end its line, start another (a `ProxyCommand` or `Match
// exec` the person's ssh would then run), or be read by ssh as a pattern or a
// token. Nothing is written. Field names the value, Value is the value as it
// was, and Reason says why, in words that follow "cannot be written: ".
type EntryError struct {
	Field  string
	Value  string
	Reason string
}

func (e *EntryError) Error() string {
	// %q escapes any control character, so the value never reaches the
	// terminal raw.
	return fmt.Sprintf("ssh config entry: %s %q cannot be written: %s", e.Field, e.Value, e.Reason)
}

// PlainAlias reports whether alias is a Host alias of the shape shed writes
// (`shed-<shed>`, `shed-<server>-<shed>`): not empty, not starting with `-`
// (ssh would read an option), and only ASCII letters, digits, `.`, `_` and
// `-`. Such a name is one shell word as it is, and ssh reads it as one host
// alias: no pattern (`*`, `?`, `!`, `,`), no token (`%`), no user part (`@`),
// no port, no white space and nothing that ends the line.
func PlainAlias(alias string) bool {
	if alias == "" || alias[0] == '-' {
		return false
	}
	for i := 0; i < len(alias); i++ {
		c := alias[i]
		alnum := 'a' <= c && c <= 'z' || 'A' <= c && c <= 'Z' || '0' <= c && c <= '9'
		if !alnum && c != '.' && c != '_' && c != '-' {
			return false
		}
	}
	return true
}

// LiteralHost reports whether host is a literal host name or address of the
// shape an ssh config's HostName may carry as it is: a DNS name or an IPv4
// address (only ASCII letters, digits, `.`, `-` and `_`, the first and last
// character a letter or digit), or an IPv6 literal (only hex digits and `:`,
// with at least two colons; no zone index and no brackets). Anything else is
// refused: above all `%` (ssh expands tokens in HostName), `$`, `=`, a
// leading `-`, white space and every shell operator, since a ProxyCommand in
// the person's own config can hand the value to a shell through `%h`.
func LiteralHost(host string) bool {
	if host == "" {
		return false
	}
	isHex := func(c byte) bool {
		return '0' <= c && c <= '9' || 'a' <= c && c <= 'f' || 'A' <= c && c <= 'F'
	}
	if strings.Count(host, ":") >= 2 {
		for i := 0; i < len(host); i++ {
			if c := host[i]; c != ':' && !isHex(c) {
				return false
			}
		}
		return true
	}
	isAlnum := func(c byte) bool {
		return 'a' <= c && c <= 'z' || 'A' <= c && c <= 'Z' || '0' <= c && c <= '9'
	}
	if !isAlnum(host[0]) || !isAlnum(host[len(host)-1]) {
		return false
	}
	for i := 0; i < len(host); i++ {
		if c := host[i]; !isAlnum(c) && c != '.' && c != '-' && c != '_' {
			return false
		}
	}
	return true
}

// Validate is the check every writer of this package (Write, AddEntryIfAbsent,
// EnsureEntry) makes of each entry before it writes anything, so no caller can
// put a value into an ssh config that changes what the stanza says:
//
//   - Name, the Host alias: PlainAlias.
//   - User, a shed's name (a shed's SSH user is its name):
//     config.ValidateShedName, the shed-name alphabet and nothing else. A
//     server answers a shed list with names the client did not choose, so
//     this is where a hostile one is stopped.
//   - Host, the HostName: LiteralHost, a literal host name or address and
//     nothing else (the config entry's host, which nothing else checks). ssh
//     expands `%` tokens in HostName, and a ProxyCommand in the person's own
//     config may hand it to a shell through `%h`, so quoting alone would not
//     make any other value safe.
//   - Port: 1 to 65535 (written as a number, so it carries no text).
//   - KnownHostsFile: sdkbootstrap.ValidateIdentityFile's rule (it is written
//     inside double quotes, as IdentityFile is, and ssh expands `%` tokens
//     and `$` variables in it).
//   - IdentityFile: sdkbootstrap.ValidateIdentityFile, the rule that makes it
//     safe to write inside double quotes as it is.
func (e Entry) Validate() error {
	refuse := func(field, value, reason string) error {
		return &EntryError{Field: field, Value: value, Reason: reason}
	}
	if !PlainAlias(e.Name) {
		return refuse("Host alias", e.Name, "it is not a plain host alias (ASCII letters, digits, '.', '_' and '-', "+
			"not starting with '-')")
	}
	if err := config.ValidateShedName(e.User); err != nil {
		return refuse("User", e.User, "it is not a shed name: "+err.Error())
	}
	if e.Host == "" {
		return refuse("HostName", e.Host, "it is empty")
	}
	if !LiteralHost(e.Host) {
		return refuse("HostName", e.Host, "it is not a literal host name or address (a DNS name or IPv4 address: "+
			"ASCII letters, digits, '.', '-' and '_', starting and ending with a letter or digit; "+
			"or an IPv6 address: hex digits and ':' only, with no zone and no brackets)")
	}
	if e.Port < 1 || e.Port > 65535 {
		return refuse("Port", fmt.Sprint(e.Port), "it is not a port number (1 to 65535)")
	}
	if e.KnownHostsFile != "" {
		// The rule that makes IdentityFile safe inside double quotes makes
		// this path safe there too: absolute, and no line break, quote,
		// backslash, '#', '%' or '$' (ssh expands tokens and variables in it).
		if err := sdkbootstrap.ValidateIdentityFile(e.KnownHostsFile); err != nil {
			reason := err.Error()
			var ife *sdkbootstrap.IdentityFileError
			if errors.As(err, &ife) {
				reason = ife.Reason
			}
			return refuse("UserKnownHostsFile", e.KnownHostsFile, reason)
		}
	}
	if e.IdentityFile != "" {
		if err := sdkbootstrap.ValidateIdentityFile(e.IdentityFile); err != nil {
			return refuse("IdentityFile", e.IdentityFile, err.Error())
		}
	}
	return nil
}
