package config

import (
	"crypto/ed25519"
	"crypto/rand"
	"strings"
	"testing"
	"time"

	gossh "golang.org/x/crypto/ssh"
)

// userKeyLine returns a fresh ed25519 authorized_keys line (no newline).
func userKeyLine(t *testing.T) string {
	t.Helper()
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	k, err := gossh.NewPublicKey(pub)
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(gossh.MarshalAuthorizedKey(k)))
}

// userCertLine returns, as an authorized_keys line, an SSH user certificate
// for the key in keyLine, signed by a fresh CA, valid forever.
func userCertLine(t *testing.T, keyLine string) string {
	t.Helper()
	key, _, _, _, err := gossh.ParseAuthorizedKey([]byte(keyLine))
	if err != nil {
		t.Fatal(err)
	}
	_, caPriv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	ca, err := gossh.NewSignerFromKey(caPriv)
	if err != nil {
		t.Fatal(err)
	}
	cert := &gossh.Certificate{Key: key, CertType: gossh.UserCert, KeyId: "k", ValidPrincipals: []string{"shed"}, ValidBefore: gossh.CertTimeInfinity}
	if err := cert.SignCert(rand.Reader, ca); err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(gossh.MarshalAuthorizedKey(cert)))
}

// loaders are the two config loaders: serve's and the CLI's.
var loaders = []struct {
	name string
	load func(string) (*ServerConfig, error)
}{{"serve", LoadServerConfigFromPath}, {"cli", LoadServerConfigForCLI}}

// TestLoadRefusesBadUserLists is the config-load contract for auth.users:
// every shape below fails through both loaders with a nil config and an error
// that names the problem.
func TestLoadRefusesBadUserLists(t *testing.T) {
	k1, k2 := userKeyLine(t), userKeyLine(t)
	mtls := "auth:\n  mode: mtls\n"
	admin := "    root:\n      admin: true\n      keys:\n        - {key: \"" + k2 + "\", label: laptop}\n"
	tests := []struct {
		name string
		body string
		want string
	}{
		{"open mode (no auth.mode)", "auth:\n  users:\n" + admin, "auth.users requires auth.mode: mtls"},
		{"explicit open", "auth:\n  mode: open\n  users:\n" + admin, "auth.users requires auth.mode: mtls"},
		{"token mode", "auth:\n  mode: token\n  users:\n" + admin, "mtls"},
		{"beside auth.ssh.authorized_keys", mtls + "  ssh:\n    authorized_keys: [\"" + k1 + "\"]\n  users:\n" + admin, "auth.ssh.authorized_keys"},
		{"beside auth.ssh.authorized_keys_file", mtls + "  ssh:\n    authorized_keys_file: /tmp/keys\n  users:\n" + admin, "auth.ssh.authorized_keys_file"},
		{"beside auth.ssh.github_users", mtls + "  ssh:\n    github_users: [octocat]\n  users:\n" + admin, "auth.ssh.github_users"},
		{"a name with a capital", mtls + "  users:\n" + admin + "    Alice:\n      github: alice\n", `invalid auth.users name "Alice"`},
		{"a name starting with a digit", mtls + "  users:\n" + admin + "    1bob:\n      github: bob\n", `invalid auth.users name "1bob"`},
		{"a name that is also a client", mtls + "  users:\n" + admin + "    ci:\n      github: ci-bot\n  clients:\n    ci:\n      policy: {name_prefix: ci-, max_sheds: 1, max_age: 1h}\n", "also an auth.clients name"},
		{"a user with no login and no key", mtls + "  users:\n" + admin + "    bob:\n      admin: false\n", "auth.users.bob needs a github login or at least one key"},
		{"an invalid GitHub login", mtls + "  users:\n" + admin + "    bob:\n      github: \"bob/../x\"\n", "not a valid GitHub login"},
		{"two users, one login", mtls + "  users:\n" + admin + "    alice:\n      github: Octo\n    bob:\n      github: octo\n", "same GitHub login"},
		{"a negative github_id", mtls + "  users:\n" + admin + "    bob:\n      github: bob\n      github_id: -3\n", "github_id must be a positive"},
		{"github_id without github", mtls + "  users:\n" + admin + "    bob:\n      github_id: 3\n      keys:\n        - {key: \"" + k1 + "\", label: l}\n", "github_id needs"},
		{"a key with no label", mtls + "  users:\n" + admin + "    bob:\n      keys:\n        - key: \"" + k1 + "\"\n", "label is required"},
		{"a label used twice", mtls + "  users:\n" + admin + "    bob:\n      keys:\n        - {key: \"" + k1 + "\", label: l}\n        - {key: \"" + userKeyLine(t) + "\", label: l}\n", "used twice"},
		{"a key that does not parse", mtls + "  users:\n" + admin + "    bob:\n      keys:\n        - {key: \"ssh-ed25519 not-base64\", label: l}\n", "not an OpenSSH public key"},
		{"a key with options", mtls + "  users:\n" + admin + "    bob:\n      keys:\n        - {key: 'from=\"10.0.0.1\" " + k1 + "', label: l}\n", "options"},
		{"two keys on one line", mtls + "  users:\n" + admin + "    bob:\n      keys:\n        - {key: \"" + k1 + "\\n" + userKeyLine(t) + "\", label: l}\n", "one line"},
		{"a key listed twice under one user", mtls + "  users:\n" + admin + "    bob:\n      keys:\n        - {key: \"" + k1 + "\", label: a}\n        - {key: \"" + k1 + "\", label: b}\n", "a key belongs to one user"},
		{"a key under two users", mtls + "  users:\n" + admin + "    bob:\n      keys:\n        - {key: \"" + k2 + "\", label: mine}\n", "a key belongs to one user"},
		{"an expiry that is not a time", mtls + "  users:\n" + admin + "    bob:\n      keys:\n        - {key: \"" + k1 + "\", label: l, expires: soon}\n", "neither an RFC 3339 time nor a date"},
		{"no admin", mtls + "  users:\n    bob:\n      keys:\n        - {key: \"" + k1 + "\", label: l}\n", "at least one admin"},
		{"an SSH certificate as a key", mtls + "  users:\n" + admin + "    bob:\n      keys:\n        - {key: \"" + userCertLine(t, k1) + "\", label: l}\n", "SSH certificate"},
		{"a certificate of another user's key", mtls + "  users:\n" + admin + "    bob:\n      keys:\n        - {key: \"" + userCertLine(t, k2) + "\", label: l}\n", "list the public key itself"},
		{"a certificate of the user's own script key", mtls + "  users:\n" + admin + "    bob:\n      keys:\n        - {key: \"" + k1 + "\", label: ci, script: true}\n        - {key: \"" + userCertLine(t, k1) + "\", label: cert}\n", "SSH certificate"},
		{"two certificates of one key", mtls + "  users:\n" + admin + "    bob:\n      keys:\n        - {key: \"" + userCertLine(t, k1) + "\", label: l}\n    carol:\n      keys:\n        - {key: \"" + userCertLine(t, k1) + "\", label: l}\n", "SSH certificate"},
		{"an admin with only a script key", mtls + "  users:\n    root:\n      admin: true\n      keys:\n        - {key: \"" + k1 + "\", label: ci, script: true}\n", "at least one admin"},
		{"admins with only script keys, beside a user who is not one", mtls + "  users:\n    root:\n      admin: true\n      keys:\n        - {key: \"" + k1 + "\", label: ci, script: true}\n    bob:\n      github: bob\n", "at least one admin"},
		{"admin: yes", mtls + "  users:\n" + admin + "    bob:\n      github: bob\n      admin: yes\n", "not a boolean"},
		{"admin: \"yes\"", mtls + "  users:\n" + admin + "    bob:\n      github: bob\n      admin: \"yes\"\n", "not a boolean"},
		{"admin: \"on\"", mtls + "  users:\n" + admin + "    bob:\n      github: bob\n      admin: \"on\"\n", "not a boolean"},
		{"admin: \"true\"", mtls + "  users:\n" + admin + "    bob:\n      github: bob\n      admin: \"true\"\n", "not a boolean"},
		{"admin: !!bool yes", mtls + "  users:\n" + admin + "    bob:\n      github: bob\n      admin: !!bool yes\n", "yes"},
		{"script: \"on\"", mtls + "  users:\n" + admin + "    bob:\n      keys:\n        - {key: \"" + k1 + "\", label: l, script: \"on\"}\n", "not a boolean"},
		{"script: off", mtls + "  users:\n" + admin + "    bob:\n      keys:\n        - {key: \"" + k1 + "\", label: l, script: off}\n", "not a boolean"},
		{"an unknown key under a user", mtls + "  users:\n" + admin + "    bob:\n      github: bob\n      admn: true\n", "admn"},
		{"a re-cased key under a user", mtls + "  users:\n" + admin + "    bob:\n      GitHub: bob\n", "GitHub"},
		{"an unknown key under a key", mtls + "  users:\n" + admin + "    bob:\n      keys:\n        - {key: \"" + k1 + "\", lable: l}\n", "lable"},
		{"auth.Users re-cased", mtls + "  Users:\n" + admin, "Users"},
	}
	for _, tt := range tests {
		for _, l := range loaders {
			t.Run(tt.name+"/"+l.name, func(t *testing.T) {
				cfg, err := l.load(writeServerYAML(t, tt.body))
				if err == nil {
					t.Fatalf("loaded with users %v; want a refusal", cfg.UserNames())
				}
				if cfg != nil {
					t.Errorf("a refused load returned a config")
				}
				if !strings.Contains(err.Error(), tt.want) {
					t.Errorf("error %q does not contain %q", err, tt.want)
				}
			})
		}
	}
}

// TestLoadAcceptsUserLists is the positive half: a user list in mtls loads
// through both loaders with every field as written, an expired key included
// (it is simply not accepted), and auth.ssh's non-source settings beside it.
// PreflightAuth counts the list as the key source.
func TestLoadAcceptsUserLists(t *testing.T) {
	k1, k2, k3 := userKeyLine(t), userKeyLine(t), userKeyLine(t)
	body := "auth:\n  mode: mtls\n" +
		"  ssh:\n    github_refresh: 30m\n    max_auth_tries: 20\n" +
		"  users:\n" +
		"    alice:\n      github: Alice-GH\n      admin: true\n" +
		"    bob:\n      github: bob-gh\n      github_id: 1234567\n      keys:\n" +
		"        - key: \"" + k1 + " bob@laptop\"\n          label: laptop\n          expires: 2099-06-30\n" +
		"        - key: \"" + k2 + "\"\n          label: ci\n          script: true\n" +
		"        - key: \"" + k3 + "\"\n          label: old\n          expires: \"2020-01-01T00:00:00Z\"\n" +
		"        - key: \"" + userKeyLine(t) + "\"\n          label: ancient\n          expires: \"0001-01-01T00:00:00Z\"\n" +
		"    carol:\n      admin: false\n      keys:\n        - {key: \"" + userKeyLine(t) + "\", label: desk, script: False}\n" +
		"    dave:\n      admin: TRUE\n      keys:\n        - {key: \"" + userKeyLine(t) + "\", label: ci, script: true}\n        - {key: \"" + userKeyLine(t) + "\", label: desk}\n"
	for _, l := range loaders {
		t.Run(l.name, func(t *testing.T) {
			cfg, err := l.load(writeServerYAML(t, body))
			if err != nil {
				t.Fatalf("refused: %v", err)
			}
			if !cfg.UsersConfigured() || strings.Join(cfg.UserNames(), ",") != "alice,bob,carol,dave" {
				t.Fatalf("users %v", cfg.UserNames())
			}
			if err := cfg.PreflightAuth(); err != nil {
				t.Errorf("PreflightAuth with a user list and no auth.ssh source: %v", err)
			}
			alice, bob := cfg.Auth.Users["alice"], cfg.Auth.Users["bob"]
			if !alice.Admin || alice.GitHub != "Alice-GH" || bob.Admin || bob.GitHubID != 1234567 {
				t.Errorf("alice %+v bob %+v", alice, bob)
			}
			if dave := cfg.Auth.Users["dave"]; !dave.Admin || !dave.Keys[0].Script || dave.Keys[1].Script || cfg.Auth.Users["carol"].Admin {
				t.Errorf("dave %+v carol %+v", dave, cfg.Auth.Users["carol"])
			}
			if len(bob.Keys) != 4 || bob.Keys[0].Label != "laptop" || bob.Keys[0].Expires != "2099-06-30" || !bob.Keys[1].Script || bob.Keys[0].Script {
				t.Errorf("bob's keys %+v", bob.Keys)
			}
			if got := cfg.ExpiredUserKeys(time.Now()); strings.Join(got, ";") != "bob (old);bob (ancient)" {
				t.Errorf("ExpiredUserKeys = %v", got)
			}
		})
	}
}

// TestNoUserListIsUnchanged: a config with no auth.users (or an empty one)
// has no user list; an mtls server still needs an auth.ssh key source.
func TestNoUserListIsUnchanged(t *testing.T) {
	for _, body := range []string{
		"auth:\n  mode: mtls\n  ssh:\n    github_users: [octocat]\n",
		"auth:\n  mode: mtls\n  users: {}\n  ssh:\n    github_users: [octocat]\n",
	} {
		cfg, err := LoadServerConfigForCLI(writeServerYAML(t, body))
		if err != nil {
			t.Fatalf("refused %q: %v", body, err)
		}
		if cfg.UsersConfigured() || cfg.UserNames() != nil {
			t.Errorf("%q has a user list", body)
		}
	}
	cfg, err := LoadServerConfigForCLI(writeServerYAML(t, "auth:\n  mode: mtls\n"))
	if err != nil {
		t.Fatal(err)
	}
	if err := cfg.PreflightAuth(); err == nil || !strings.Contains(err.Error(), "auth.users") {
		t.Errorf("mtls with no key source: %v, want a refusal naming auth.users", err)
	}
}

func TestParseKeyExpiry(t *testing.T) {
	at := func(tm time.Time) *time.Time { return &tm }
	for _, tc := range []struct {
		in   string
		want *time.Time // nil: no expiry
		err  bool
	}{
		{"", nil, false},
		{"2027-06-30", at(time.Date(2027, 7, 1, 0, 0, 0, 0, time.UTC)), false},
		{"2027-06-30T12:00:00+02:00", at(time.Date(2027, 6, 30, 10, 0, 0, 0, time.UTC)), false},
		// The earliest time Go holds is the zero time, and still an expiry
		// (long past), never "no expiry".
		{"0001-01-01T00:00:00Z", at(time.Time{}), false},
		{"0001-01-01T01:00:00+01:00", at(time.Time{}), false},
		{"0000-12-31", at(time.Time{}), false},
		{"30/06/2027", nil, true},
		{"tomorrow", nil, true},
	} {
		got, err := ParseKeyExpiry(tc.in)
		same := (got == nil) == (tc.want == nil) && (got == nil || got.Equal(*tc.want))
		if (err != nil) != tc.err || !same {
			t.Errorf("ParseKeyExpiry(%q) = %v, %v; want %v (error %v)", tc.in, got, err, tc.want, tc.err)
		}
	}
}
