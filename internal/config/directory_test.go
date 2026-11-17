package config

import (
	"reflect"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

// directoryAdmin is an auth.users block with one local admin, root, for the
// auth.directory tests.
func directoryAdmin(t *testing.T) string {
	t.Helper()
	return "  users:\n    root:\n      admin: true\n      keys:\n        - {key: \"" + userKeyLine(t) + "\", label: laptop}\n"
}

// TestLoadRefusesBadDirectoryBlocks is the config-load contract for
// auth.directory: every shape below fails through both loaders with a nil
// config and an error that names the field.
func TestLoadRefusesBadDirectoryBlocks(t *testing.T) {
	k1 := userKeyLine(t)
	mtls := "auth:\n  mode: mtls\n"
	admin := directoryAdmin(t)
	dir := func(lines string) string { return "  directory:\n" + lines }
	good := "    url: https://accounts.example.test\n"
	flag := "    allow_plain_http: true\n"
	tests := []struct {
		name string
		body string
		want string
		also string // a second substring the error holds, when set
		not  string // a substring the error must not hold, when set
	}{
		// Mode and the local admins.
		{"open mode, no users", "auth:\n" + dir(good), "auth.directory requires auth.mode: mtls", "", ""},
		{"explicit open, no users", "auth:\n  mode: open\n" + dir(good), "auth.directory requires auth.mode: mtls", "", ""},
		{"open mode with a local admin", "auth:\n" + admin + dir(good), "auth.users requires auth.mode: mtls", "", ""},
		{"no local admin", mtls + dir(good), "auth.directory needs at least one local admin in auth.users", "", ""},
		{"an empty auth.users", mtls + "  users: {}\n" + dir(good), "auth.directory needs at least one local admin in auth.users", "", ""},
		{"a user who is not an admin", mtls + admin + "    bob:\n      keys:\n        - {key: \"" + k1 + "\", label: l}\n" + dir(good), "auth.users.bob: beside auth.directory every auth.users entry is a local admin", "", ""},
		{"a user with admin: false", mtls + admin + "    bob:\n      admin: false\n      keys:\n        - {key: \"" + k1 + "\", label: l}\n" + dir(good), "set auth.users.bob.admin: true", "", ""},
		{"a local admin with a GitHub login", mtls + admin + "    gh:\n      admin: true\n      github: octocat\n" + dir(good), "auth.users.gh.github: a local admin beside auth.directory has keys only", "", ""},
		{"a local admin with a GitHub login and a key", mtls + admin + "    gh:\n      admin: true\n      github: octocat\n      github_id: 7\n      keys:\n        - {key: \"" + k1 + "\", label: l}\n" + dir(good), "auth.users.gh.github:", "", ""},
		{"a local admin with a script key", mtls + "  users:\n    root:\n      admin: true\n      keys:\n        - {key: \"" + userKeyLine(t) + "\", label: desk}\n        - {key: \"" + k1 + "\", label: ci, script: true}\n" + dir(good), "auth.users.root.keys[1] (ci).script", "", ""},
		{"beside auth.ssh.github_users", mtls + "  ssh:\n    github_users: [octocat]\n" + admin + dir(good), "auth.users cannot be combined with auth.ssh.github_users", "", ""},

		// url.
		{"no url", mtls + admin + dir("    refresh: 1h\n"), "auth.directory.url is required", "", ""},
		{"an empty block", mtls + admin + "  directory: {}\n", "auth.directory.url is required", "", ""},
		{"a path", mtls + admin + dir("    url: https://accounts.example.test/v1\n"), "auth.directory.url \"https://accounts.example.test/v1\" has a path", "", ""},
		{"two trailing slashes", mtls + admin + dir("    url: https://accounts.example.test//\n"), "has a path", "", ""},
		{"a query", mtls + admin + dir("    url: https://accounts.example.test?a=b\n"), "has a query", "", ""},
		{"an empty query", mtls + admin + dir("    url: https://accounts.example.test?\n"), "has a query", "", ""},
		{"a fragment", mtls + admin + dir("    url: \"https://accounts.example.test#top\"\n"), "has a fragment", "", ""},
		{"an empty fragment", mtls + admin + dir("    url: \"https://accounts.example.test#\"\n"), "has a fragment", "", ""},
		{"user info", mtls + admin + dir("    url: https://ops@accounts.example.test\n"), "carries user info", "", ""},
		{"user info and password", mtls + admin + dir("    url: https://ops:pw@accounts.example.test\n"), "carries user info", "", ""},
		{"no host", mtls + admin + dir("    url: https://\n"), "has no host", "", ""},
		{"a port with no host", mtls + admin + dir("    url: https://:8443\n"), "has no host", "", ""},
		{"an empty port", mtls + admin + dir("    url: \"https://accounts.example.test:\"\n"), "has an empty port", "", ""},
		{"port 0", mtls + admin + dir("    url: https://accounts.example.test:0\n"), "invalid port", "", ""},
		{"port out of range", mtls + admin + dir("    url: https://accounts.example.test:70000\n"), "invalid port", "", ""},
		{"another scheme", mtls + admin + dir("    url: ftp://accounts.example.test\n"), "must start with https://", "", ""},
		{"no scheme", mtls + admin + dir("    url: accounts.example.test\n"), "must start with https://", "", ""},
		{"an upper-case scheme", mtls + admin + dir("    url: HTTPS://accounts.example.test\n"), "must start with https://", "", ""},
		{"a port with a leading zero", mtls + admin + dir("    url: https://accounts.example.test:0443\n"), "invalid port", "", ""},
		{"a port past any integer", mtls + admin + dir("    url: https://accounts.example.test:99999999999999999999\n"), "invalid port", "", ""},
		{"a bad escape in the host", mtls + admin + dir("    url: https://%61ccounts.example.test\n"), "does not parse", "", ""},
		{"a space", mtls + admin + dir("    url: \"https://accounts.example.test \"\n"), "holds a space or a control character", "", ""},
		{"a tab", mtls + admin + dir("    url: \"https://accounts.example.test\\t\"\n"), "holds a space or a control character", "", ""},

		// The host has one spelling: plain ASCII, IPv4, or IPv6 in brackets.
		// No refusal recommends a percent-escaped spelling.
		{"a host with a letter outside ASCII", mtls + admin + dir("    url: https://é.example.test\n"), "auth.directory.url \"https://é.example.test\": the host \"é.example.test\" holds a character other than ASCII letters", "xn--", "%"},
		{"a host with a literal zero-width space", mtls + admin + dir("    url: \"https://accounts"+string(rune(0x200b))+".example.test\"\n"), "holds a character other than ASCII letters", "", "%"},
		{"a host with an underscore", mtls + admin + dir("    url: https://accounts_one.example.test\n"), "holds a character other than ASCII letters", "", ""},
		{"an escaped zero-width space", mtls + admin + dir("    url: https://accounts%E2%80%8B.example.test\n"), "auth.directory.url \"https://accounts%E2%80%8B.example.test\" has a percent sign in its host", "", "write it as"},
		{"an escaped no-break space", mtls + admin + dir("    url: https://accounts%C2%A0.example.test\n"), "has a percent sign in its host", "", "write it as"},
		{"an escaped letter outside ASCII", mtls + admin + dir("    url: https://%C3%A9.example.test\n"), "has a percent sign in its host", "", "write it as"},
		{"an escaped IPv6 zone", mtls + admin + dir("    url: \"http://[::1%25lo]:18080\"\n"+flag), "has a percent sign in its host", "", ""},
		{"an IPv4 address in brackets", mtls + admin + dir("    url: \"https://[127.0.0.1]\"\n"), "auth.directory.url \"https://[127.0.0.1]\" does not parse", "", ""},

		// Plain HTTP.
		{"plain HTTP without the flag", mtls + admin + dir("    url: http://accounts.example.test\n"), "auth.directory.url \"http://accounts.example.test\" is plain HTTP", "", ""},
		{"plain HTTP to loopback without the flag", mtls + admin + dir("    url: http://127.0.0.1:18080\n"), "is plain HTTP", "", ""},
		{"plain HTTP to localhost with the flag false", mtls + admin + dir("    url: http://localhost:18080\n    allow_plain_http: false\n"), "is plain HTTP", "", ""},
		{"the flag with a host that is not loopback", mtls + admin + dir("    url: http://accounts.example.test\n    allow_plain_http: true\n"), "auth.directory.allow_plain_http is for a loopback host only", "", ""},
		{"the flag with a private address", mtls + admin + dir("    url: http://10.0.0.1:8080\n    allow_plain_http: true\n"), "allow_plain_http is for a loopback host only", "", ""},
		{"the flag with an unspecified address", mtls + admin + dir("    url: http://0.0.0.0:8080\n    allow_plain_http: true\n"), "allow_plain_http is for a loopback host only", "", ""},
		{"the flag with a name that resolves nowhere in particular", mtls + admin + dir("    url: http://localhost.example.test\n    allow_plain_http: true\n"), "allow_plain_http is for a loopback host only", "", ""},
		{"the flag with an https host that is not loopback", mtls + admin + dir("    url: https://accounts.example.test\n    allow_plain_http: true\n"), "allow_plain_http is for a loopback host only", "", ""},
		{"the flag as yes", mtls + admin + dir("    url: http://127.0.0.1:1\n    allow_plain_http: yes\n"), "auth.directory.allow_plain_http (line", "not a boolean", ""},
		{"the flag quoted", mtls + admin + dir("    url: http://127.0.0.1:1\n    allow_plain_http: \"true\"\n"), "auth.directory.allow_plain_http (line", "not a boolean", ""},
		{"the flag as a list", mtls + admin + dir("    url: http://127.0.0.1:1\n    allow_plain_http: [true]\n"), "auth.directory.allow_plain_http (line", "not a boolean", ""},

		// Spellings of loopback the flag does not accept (no name
		// resolution: only localhost itself and loopback addresses).
		{"the flag with 127.1", mtls + admin + dir("    url: http://127.1:18080\n"+flag), "allow_plain_http is for a loopback host only", "", ""},
		{"the flag with a hex address", mtls + admin + dir("    url: http://0x7f.0.0.1:18080\n"+flag), "allow_plain_http is for a loopback host only", "", ""},
		{"the flag with LOCALHOST", mtls + admin + dir("    url: http://LOCALHOST:18080\n"+flag), "allow_plain_http is for a loopback host only", "", ""},
		{"the flag with localhost and a final dot", mtls + admin + dir("    url: http://localhost.:18080\n"+flag), "allow_plain_http is for a loopback host only", "", ""},
		{"the flag with the IPv6 unspecified address", mtls + admin + dir("    url: \"http://[::]:18080\"\n"+flag), "allow_plain_http is for a loopback host only", "", ""},

		// organization.
		{"an organization with a capital", mtls + admin + dir(good+"    organization: Example\n"), "auth.directory.organization \"Example\"", "", ""},
		{"an organization starting with a digit", mtls + admin + dir(good+"    organization: 1example\n"), "auth.directory.organization", "", ""},
		{"an organization with an underscore", mtls + admin + dir(good+"    organization: ex_ample\n"), "auth.directory.organization", "", ""},
		{"an organization too long", mtls + admin + dir(good+"    organization: a"+strings.Repeat("b", 63)+"\n"), "auth.directory.organization", "", ""},
		{"an organization ending in a newline", mtls + admin + dir(good+"    organization: |\n      example\n"), "auth.directory.organization \"example\\n\" is not a valid organization name", "", ""},
		{"an organization that is a number", mtls + admin + dir(good+"    organization: 7\n"), "auth.directory.organization (line", "is not a string", ""},
		{"an organization that is a boolean", mtls + admin + dir(good+"    organization: true\n"), "auth.directory.organization (line", "is not a string", ""},
		{"a url that is a list", mtls + admin + dir("    url: [https://accounts.example.test]\n"), "auth.directory.url (line", "is not a string", ""},

		// refresh and trust.
		{"refresh below a minute", mtls + admin + dir(good+"    refresh: 59s\n"), "auth.directory.refresh 59s is out of range", "", ""},
		{"refresh over seven days", mtls + admin + dir(good+"    refresh: 168h1s\n    trust: 720h\n"), "auth.directory.refresh 168h0m1s is out of range", "", ""},
		{"a negative refresh", mtls + admin + dir(good+"    refresh: -1h\n"), "auth.directory.refresh -1h0m0s is out of range", "", ""},
		{"trust below refresh", mtls + admin + dir(good+"    refresh: 2h\n    trust: 1h\n"), "auth.directory.trust 1h0m0s is out of range (from auth.directory.refresh, 2h0m0s", "", ""},
		{"the default trust below refresh", mtls + admin + dir(good+"    refresh: 96h\n"), "auth.directory.trust 72h0m0s is out of range", "", ""},
		{"trust over thirty days", mtls + admin + dir(good+"    trust: 720h1s\n"), "auth.directory.trust 720h0m1s is out of range", "", ""},
		{"a negative trust", mtls + admin + dir(good+"    trust: -72h\n"), "auth.directory.trust -72h0m0s is out of range (from auth.directory.refresh, 24h0m0s, to 720h0m0s", "", ""},

		// A written zero is out of range; only an omitted value is the
		// default.
		{"refresh written as 0s", mtls + admin + dir(good+"    refresh: 0s\n"), "auth.directory.refresh 0s is out of range", "", ""},
		{"refresh written as 0", mtls + admin + dir(good+"    refresh: 0\n"), "auth.directory.refresh 0s is out of range", "", ""},
		{"trust written as 0s", mtls + admin + dir(good+"    trust: 0s\n"), "auth.directory.trust 0s is out of range", "", ""},
		{"trust written as 0", mtls + admin + dir(good+"    trust: 0\n"), "auth.directory.trust 0s is out of range", "", ""},
		{"refresh written as 0s beside a trust", mtls + admin + dir(good+"    refresh: 0s\n    trust: 1h\n"), "auth.directory.refresh 0s is out of range", "", "auth.directory.trust"},

		// A value that does not decode is refused naming its field.
		{"refresh that is not a duration", mtls + admin + dir(good+"    refresh: 1 day\n"), "auth.directory.refresh (line", "1 day", ""},
		{"trust that is not a duration", mtls + admin + dir(good+"    trust: 1 day\n"), "auth.directory.trust (line", "1 day", ""},
		{"refresh that is a list", mtls + admin + dir(good+"    refresh: [1h]\n"), "auth.directory.refresh (line", "", ""},
		{"refresh with no value", mtls + admin + dir(good+"    refresh:\n"), "auth.directory.refresh (line", "has no value", ""},
		{"trust written as null", mtls + admin + dir(good+"    trust: null\n"), "auth.directory.trust (line", "has no value", ""},
		{"the flag with no value", mtls + admin + dir(good+"    allow_plain_http:\n"), "auth.directory.allow_plain_http (line", "has no value", ""},
		{"url with no value", mtls + admin + dir("    url: ~\n"), "auth.directory.url (line", "has no value", ""},

		// Strict decoding.
		{"an unknown key", mtls + admin + dir(good+"    urll: https://accounts.example.test\n"), "unknown field \"urll\"", "", ""},
		{"a re-cased key", mtls + admin + dir("    URL: https://accounts.example.test\n"), "unknown field \"URL\"", "", ""},
		{"a removed-looking key", mtls + admin + dir(good+"    github: true\n"), "unknown field \"github\"", "", ""},
		{"auth.Directory re-cased", mtls + admin + "  Directory:\n" + good, "Directory", "", ""},
		{"url written twice", mtls + admin + dir("    url: https://other.example.test\n"+good), "auth.directory.url (line", "is written twice", ""},
		{"trust written twice", mtls + admin + dir(good+"    trust: 48h\n    trust: 72h\n"), "auth.directory.trust (line", "is written twice", ""},
		{"a merge key", mtls + admin + dir("    <<: {url: https://accounts.example.test}\n"), "a merge key (<<) is not accepted here", "", ""},
		{"a block that is a list", mtls + admin + "  directory: [https://accounts.example.test]\n", "auth.directory (line", "must be a mapping", ""},
		{"a block that is a string", mtls + admin + "  directory: https://accounts.example.test\n", "auth.directory (line", "must be a mapping", ""},
	}
	for _, tt := range tests {
		for _, l := range loaders {
			t.Run(tt.name+"/"+l.name, func(t *testing.T) {
				cfg, err := l.load(writeServerYAML(t, tt.body))
				if err == nil {
					t.Fatalf("loaded (directory %+v); want a refusal", cfg.Auth.Directory)
				}
				if cfg != nil {
					t.Errorf("a refused load returned a config")
				}
				if !strings.Contains(err.Error(), tt.want) {
					t.Errorf("error %q does not contain %q", err, tt.want)
				}
				if tt.also != "" && !strings.Contains(err.Error(), tt.also) {
					t.Errorf("error %q does not contain %q", err, tt.also)
				}
				if tt.not != "" && strings.Contains(err.Error(), tt.not) {
					t.Errorf("error %q contains %q", err, tt.not)
				}
			})
		}
	}
}

// TestLoadAcceptsDirectoryBlocks is the positive half: each block below loads
// through both loaders, with its issuer (one trailing slash dropped), its
// refresh and trust (defaults when unset), and the local admins as the user
// list. PreflightAuth counts the local admins as the key source.
func TestLoadAcceptsDirectoryBlocks(t *testing.T) {
	admin := directoryAdmin(t)
	tests := []struct {
		name    string
		block   string
		issuer  string
		org     string
		refresh time.Duration
		trust   time.Duration
		plain   bool
	}{
		{"defaults", "    url: https://accounts.example.test\n", "https://accounts.example.test", "", 24 * time.Hour, 72 * time.Hour, false},
		{"one trailing slash", "    url: https://accounts.example.test/\n", "https://accounts.example.test", "", 24 * time.Hour, 72 * time.Hour, false},
		{"a port", "    url: https://accounts.example.test:8443\n", "https://accounts.example.test:8443", "", 24 * time.Hour, 72 * time.Hour, false},
		{"every field", "    url: https://accounts.example.test\n    organization: example\n    refresh: 12h\n    trust: 48h\n    allow_plain_http: false\n", "https://accounts.example.test", "example", 12 * time.Hour, 48 * time.Hour, false},
		{"the lowest refresh, trust equal to it", "    url: https://accounts.example.test\n    refresh: 1m\n    trust: 1m\n", "https://accounts.example.test", "", time.Minute, time.Minute, false},
		{"the highest refresh and trust", "    url: https://accounts.example.test\n    refresh: 168h\n    trust: 720h\n", "https://accounts.example.test", "", 168 * time.Hour, 720 * time.Hour, false},
		{"refresh alone, under the default trust", "    url: https://accounts.example.test\n    refresh: 72h\n", "https://accounts.example.test", "", 72 * time.Hour, 72 * time.Hour, false},
		{"an organization of one letter", "    url: https://accounts.example.test\n    organization: e\n", "https://accounts.example.test", "e", 24 * time.Hour, 72 * time.Hour, false},
		{"plain HTTP to 127.0.0.1", "    url: http://127.0.0.1:18080\n    allow_plain_http: true\n    refresh: 1m\n    trust: 2m\n", "http://127.0.0.1:18080", "", time.Minute, 2 * time.Minute, true},
		{"plain HTTP to another 127/8 address", "    url: http://127.4.5.6:18080/\n    allow_plain_http: true\n", "http://127.4.5.6:18080", "", 24 * time.Hour, 72 * time.Hour, true},
		{"plain HTTP to localhost", "    url: http://localhost:18080\n    allow_plain_http: true\n", "http://localhost:18080", "", 24 * time.Hour, 72 * time.Hour, true},
		{"plain HTTP to ::1", "    url: \"http://[::1]:18080\"\n    allow_plain_http: true\n", "http://[::1]:18080", "", 24 * time.Hour, 72 * time.Hour, true},
		{"https to loopback with the flag", "    url: https://127.0.0.1:18443\n    allow_plain_http: true\n", "https://127.0.0.1:18443", "", 24 * time.Hour, 72 * time.Hour, true},
		{"plain HTTP to ::1 written out", "    url: \"http://[0:0:0:0:0:0:0:1]:18080\"\n    allow_plain_http: true\n", "http://[0:0:0:0:0:0:0:1]:18080", "", 24 * time.Hour, 72 * time.Hour, true},
		{"plain HTTP to 127.0.0.1 mapped into IPv6", "    url: \"http://[::ffff:127.0.0.1]:18080\"\n    allow_plain_http: true\n", "http://[::ffff:127.0.0.1]:18080", "", 24 * time.Hour, 72 * time.Hour, true},
		{"an IPv6 host", "    url: \"https://[2001:db8::1]:8443\"\n", "https://[2001:db8::1]:8443", "", 24 * time.Hour, 72 * time.Hour, false},
		{"a host in its xn-- form", "    url: https://xn--9ca.example.test\n", "https://xn--9ca.example.test", "", 24 * time.Hour, 72 * time.Hour, false},
		{"values through an anchor", "    url: &u https://accounts.example.test\n    organization: example\n    refresh: &r 2h\n    trust: *r\n", "https://accounts.example.test", "example", 2 * time.Hour, 2 * time.Hour, false},
	}
	for _, tt := range tests {
		for _, l := range loaders {
			t.Run(tt.name+"/"+l.name, func(t *testing.T) {
				cfg, err := l.load(writeServerYAML(t, "auth:\n  mode: mtls\n"+admin+"  directory:\n"+tt.block))
				if err != nil {
					t.Fatalf("refused: %v", err)
				}
				if !cfg.DirectoryConfigured() {
					t.Fatal("DirectoryConfigured() = false")
				}
				d := cfg.Auth.Directory
				if d.Issuer() != tt.issuer || d.Organization != tt.org || d.RefreshInterval() != tt.refresh || d.TrustPeriod() != tt.trust || bool(d.AllowPlainHTTP) != tt.plain {
					t.Errorf("issuer %q org %q refresh %s trust %s plain %v; want %q %q %s %s %v",
						d.Issuer(), d.Organization, d.RefreshInterval(), d.TrustPeriod(), d.AllowPlainHTTP, tt.issuer, tt.org, tt.refresh, tt.trust, tt.plain)
				}
				if !cfg.UsersConfigured() || strings.Join(cfg.UserNames(), ",") != "root" {
					t.Errorf("local admins %v", cfg.UserNames())
				}
				if err := cfg.PreflightAuth(); err != nil {
					t.Errorf("PreflightAuth with local admins and a directory: %v", err)
				}
			})
		}
	}
}

// TestDirectoryAcceptsSeveralLocalAdminsWithExpiringKeys: several local
// admins load, a key's expiry is kept and not checked at load (the
// auth.users convention), and an empty directory key is no directory.
func TestDirectoryAcceptsSeveralLocalAdminsWithExpiringKeys(t *testing.T) {
	body := "auth:\n  mode: mtls\n  users:\n" +
		"    root:\n      admin: true\n      keys:\n        - {key: \"" + userKeyLine(t) + "\", label: laptop, expires: \"2020-01-01T00:00:00Z\"}\n" +
		"    ops:\n      admin: true\n      keys:\n        - {key: \"" + userKeyLine(t) + "\", label: desk, script: false}\n" +
		"  directory:\n    url: https://accounts.example.test\n"
	for _, l := range loaders {
		cfg, err := l.load(writeServerYAML(t, body))
		if err != nil {
			t.Fatalf("%s: refused: %v", l.name, err)
		}
		if got := strings.Join(cfg.UserNames(), ","); got != "ops,root" {
			t.Errorf("%s: local admins %s", l.name, got)
		}
		if got := cfg.ExpiredUserKeys(time.Now()); strings.Join(got, ";") != "root (laptop)" {
			t.Errorf("%s: ExpiredUserKeys = %v", l.name, got)
		}
	}
	cfg, err := LoadServerConfigForCLI(writeServerYAML(t, "auth:\n  mode: mtls\n"+directoryAdmin(t)+"  directory:\n"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.DirectoryConfigured() {
		t.Error("an empty auth.directory key is a directory")
	}
}

// TestNoDirectoryIsUnchanged: without auth.directory a server is not a
// directory server, with or without a user list, and a user list keeps its
// existing rules (non-admins, GitHub logins and script keys all load).
func TestNoDirectoryIsUnchanged(t *testing.T) {
	for _, body := range []string{
		"",
		"auth:\n  mode: mtls\n  ssh:\n    github_users: [octocat]\n",
		"auth:\n  mode: mtls\n  users:\n    root:\n      admin: true\n      github: octocat\n" +
			"    bob:\n      keys:\n        - {key: \"" + userKeyLine(t) + "\", label: ci, script: true}\n",
	} {
		cfg, err := LoadServerConfigForCLI(writeServerYAML(t, body))
		if err != nil {
			t.Fatalf("refused %q: %v", body, err)
		}
		if cfg.DirectoryConfigured() {
			t.Errorf("%q is a directory server", body)
		}
	}
}

// TestValidateDirectoryOnAHandBuiltConfig covers what config load cannot
// reach, since validateUsers refuses it first: a github_id with no login on a
// local admin. A hand-built config (a test, a tool) is refused all the same.
func TestValidateDirectoryOnAHandBuiltConfig(t *testing.T) {
	cfg := &ServerConfig{Auth: &AuthConfig{
		Mode: AuthModeMTLS,
		Users: map[string]UserConfig{"root": {
			Admin:    true,
			GitHubID: 7,
			Keys:     []UserKeyConfig{{Key: userKeyLine(t), Label: "laptop"}},
		}},
		Directory: &DirectoryConfig{URL: "https://accounts.example.test"},
	}}
	if err := cfg.validateDirectory(); err == nil || !strings.Contains(err.Error(), "auth.users.root.github_id") {
		t.Errorf("validateDirectory = %v; want a refusal naming auth.users.root.github_id", err)
	}
	cfg.Auth.Users["root"] = UserConfig{Admin: true, Keys: cfg.Auth.Users["root"].Keys}
	if err := cfg.validateDirectory(); err != nil {
		t.Errorf("validateDirectory without github_id = %v", err)
	}

	// Nil refresh and trust are the defaults; a zero written into either
	// is out of range, on a hand-built config as in a file.
	d := cfg.Auth.Directory
	if d.RefreshInterval() != DefaultDirectoryRefresh || d.TrustPeriod() != DefaultDirectoryTrust {
		t.Errorf("nil refresh and trust read as %s and %s; want the defaults", d.RefreshInterval(), d.TrustPeriod())
	}
	zero := Duration(0)
	d.Refresh = &zero
	if err := cfg.validateDirectory(); err == nil || !strings.Contains(err.Error(), "auth.directory.refresh 0s is out of range") {
		t.Errorf("validateDirectory with refresh 0 = %v; want it out of range", err)
	}
	d.Refresh = nil
	d.Trust = &zero
	if err := cfg.validateDirectory(); err == nil || !strings.Contains(err.Error(), "auth.directory.trust 0s is out of range") {
		t.Errorf("validateDirectory with trust 0 = %v; want it out of range", err)
	}
}

// TestDirectoryConfigRoundTrips: a loaded auth.directory written back out as
// YAML (the dev-config writers re-encode the config) decodes to the same
// block, and an omitted refresh and trust stay omitted.
func TestDirectoryConfigRoundTrips(t *testing.T) {
	for _, block := range []string{
		"url: https://accounts.example.test\norganization: example\nrefresh: 2h\ntrust: 48h\n",
		"url: http://127.0.0.1:18080\nallow_plain_http: true\n",
	} {
		var first DirectoryConfig
		if err := yaml.Unmarshal([]byte(block), &first); err != nil {
			t.Fatalf("%q: %v", block, err)
		}
		out, err := yaml.Marshal(&first)
		if err != nil {
			t.Fatal(err)
		}
		var second DirectoryConfig
		if err := yaml.Unmarshal(out, &second); err != nil {
			t.Fatalf("re-decoding %q: %v", out, err)
		}
		if !reflect.DeepEqual(first, second) {
			t.Errorf("round trip of %q: %+v, then %+v", block, first, second)
		}
		if (first.Refresh == nil) != !strings.Contains(block, "refresh") || (first.Trust == nil) != !strings.Contains(block, "trust") {
			t.Errorf("%q: refresh %v trust %v; omitted must be nil, written must be set", block, first.Refresh, first.Trust)
		}
	}
}
