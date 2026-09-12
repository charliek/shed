package roostprovider

import (
	"strings"
	"testing"
)

func TestTokenRoundTrip(t *testing.T) {
	tests := []struct {
		name string
		tok  Token
	}{
		{"shed host only", Token{Shed: "dev", Server: "my-server"}},
		{"machine host only", Token{Machine: "mini2"}},
		{
			"shed with an agent and a home",
			Token{Shed: "dev", Server: "my-server", Agent: "claude-rc", Home: "/home/shed"},
		},
		{
			"machine fully descended",
			Token{Machine: "mini2", Agent: "cursor", Home: "/Users/me", Cwd: "/Users/me/src", Project: "7"},
		},
		// The two shapes a naive `k=v&k=v` encoder gets wrong. A `$HOME` with
		// spaces is ordinary on macOS ("/Users/First Last"), and a cwd carrying
		// `&` or `=` is ordinary anywhere — both would otherwise split the
		// token into fields that were never there.
		{
			"a home path with spaces",
			Token{Machine: "laptop", Agent: "codex", Home: "/Users/First Last"},
		},
		{
			"a cwd with ampersands and equals signs",
			Token{
				Shed: "dev", Server: "srv", Agent: "gx",
				Home: "/home/shed",
				Cwd:  "/home/shed/a&b=c/d e",
			},
		},
		{
			"a cwd with the token's own separators and a percent sign",
			Token{
				Machine: "mini2", Agent: "grok",
				Home: "/root",
				Cwd:  "/root/weird?agent=claude-rc&home=%2Fnope",
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			id := tc.tok.Encode()
			got, err := ParseToken(id)
			if err != nil {
				t.Fatalf("ParseToken(%q): %v", id, err)
			}
			if got != tc.tok {
				t.Fatalf("round trip lost data:\n got %+v\nwant %+v\n  id %q", got, tc.tok, id)
			}
		})
	}
}

// TestTokenEncodeIsSortedAndStable documents the one visible consequence of
// using url.Values: keys come out alphabetically, so plan 019 §3.2's
// illustrative `shed=<name>&server=<server>` is not what the bytes look like.
// The contract is the codec and the key set, and a stable encoding is what
// makes an id comparable at all.
func TestTokenEncodeIsSortedAndStable(t *testing.T) {
	tok := Token{Shed: "dev", Server: "srv", Agent: "codex", Home: "/home/shed", Cwd: "/tmp", Project: "3"}
	want := "agent=codex&cwd=%2Ftmp&home=%2Fhome%2Fshed&project=3&server=srv&shed=dev"
	if got := tok.Encode(); got != want {
		t.Fatalf("Encode() = %q, want %q", got, want)
	}
	// Same token, fields assigned in a different order — same bytes.
	other := Token{}
	other.Project = "3"
	other.Cwd = "/tmp"
	other.Home = "/home/shed"
	other.Agent = "codex"
	other.Server = "srv"
	other.Shed = "dev"
	if got := other.Encode(); got != want {
		t.Fatalf("Encode() is not field-order independent: %q", got)
	}
}

func TestTokenEncodeOmitsEmptyFields(t *testing.T) {
	if got := (Token{Machine: "mini2"}).Encode(); got != "machine=mini2" {
		t.Fatalf("Encode() = %q, want %q", got, "machine=mini2")
	}
}

func TestParseTokenRejectsMalformedIds(t *testing.T) {
	tests := []struct {
		name string
		id   string
		want string
	}{
		{"the non-actionable sentinel", NoneID, "unknown key"},
		{"an unknown key", "machine=mini2&colour=red", "unknown key"},
		{"a repeated key", "machine=mini2&machine=mini3", "repeats"},
		{"an empty value", "machine=", "empty"},
		{"a shed with no server", "shed=dev", "needs both"},
		{"a server with no shed", "server=srv", "needs both"},
		{"both a shed and a machine", "shed=dev&server=srv&machine=mini2", "both a shed and a machine"},
		{"no host at all", "agent=codex", "neither a shed nor a machine"},
		{"an invalid percent escape", "machine=%zz", "not a valid token"},
		{"the empty string", "", "neither a shed nor a machine"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParseToken(tc.id)
			if err == nil {
				t.Fatalf("ParseToken(%q) accepted a malformed id", tc.id)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error %q does not mention %q", err, tc.want)
			}
		})
	}
}

func TestTokenHostDropsWizardState(t *testing.T) {
	full := Token{Shed: "dev", Server: "srv", Agent: "codex", Home: "/home/shed", Cwd: "/tmp", Project: "3"}
	want := Token{Shed: "dev", Server: "srv"}
	if got := full.Host(); got != want {
		t.Fatalf("Host() = %+v, want %+v", got, want)
	}
}

func TestTokenHostLabel(t *testing.T) {
	if got := (Token{Shed: "dev", Server: "my-server"}).HostLabel(); got != "my-server/dev" {
		t.Errorf("shed label = %q", got)
	}
	if got := (Token{Machine: "mini2"}).HostLabel(); got != "mini2" {
		t.Errorf("machine label = %q", got)
	}
}

func TestJoinNamesRendersEmptyAsNone(t *testing.T) {
	if got := joinNames(nil); got != "none" {
		t.Errorf("joinNames(nil) = %q, want %q", got, "none")
	}
	if got := joinNames([]string{"a", "b"}); got != "a, b" {
		t.Errorf("joinNames = %q", got)
	}
}
