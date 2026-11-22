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
		{
			"shed with an agent and a home",
			Token{Shed: "dev", Server: "my-server", Agent: "claude-rc", Home: "/home/shed"},
		},
		{
			"shed fully descended",
			Token{Shed: "dev", Server: "my-server", Agent: "claude-rc", Home: "/home/shed", Cwd: "/home/shed/src", Project: "7"},
		},
		// The two shapes a naive `k=v&k=v` encoder gets wrong. A `$HOME` with
		// spaces is ordinary on macOS ("/Users/First Last"), and a cwd carrying
		// `&` or `=` is ordinary anywhere — both would otherwise split the
		// token into fields that were never there.
		{
			"a home path with spaces",
			Token{Shed: "box", Server: "laptop", Agent: "opencode", Home: "/Users/First Last"},
		},
		{
			"a cwd with ampersands and equals signs",
			Token{
				Shed: "dev", Server: "srv", Agent: "claude-rc",
				Home: "/home/shed",
				Cwd:  "/home/shed/a&b=c/d e",
			},
		},
		{
			"a cwd with the token's own separators and a percent sign",
			Token{
				Shed: "dev", Server: "srv", Agent: "opencode",
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
	got := (Token{Shed: "dev", Server: "srv"}).Encode()
	if got != "server=srv&shed=dev" {
		t.Fatalf("Encode() = %q, want %q", got, "server=srv&shed=dev")
	}
	if strings.Contains(got, "agent=") || strings.Contains(got, "machine=") {
		t.Fatalf("Encode() kept an empty or retired field: %q", got)
	}
}

// TestParseTokenRejectsStaleMachineToken: a `machine=` row id from before
// sheds were the only host fails in ParseToken, which is before any caller
// builds an ssh target.
func TestParseTokenRejectsStaleMachineToken(t *testing.T) {
	_, err := ParseToken("machine=foo")
	if err == nil {
		t.Fatal(`ParseToken("machine=foo") succeeded`)
	}
	if !strings.Contains(err.Error(), "names a machine") {
		t.Fatalf("error %q does not say the token names a machine", err)
	}
}

func TestParseTokenRejectsMalformedIds(t *testing.T) {
	tests := []struct {
		name string
		id   string
		want string
	}{
		{"the non-actionable sentinel", NoneID, "unknown key"},
		{"an unknown key", "shed=dev&server=srv&colour=red", "unknown key"},
		{"a repeated key", "shed=dev&shed=other&server=srv", "repeats"},
		{"an empty value", "shed=", "empty"},
		{"a shed with no server", "shed=dev", "needs both"},
		{"a server with no shed", "server=srv", "needs both"},
		{"a stale machine key beside a shed", "shed=dev&server=srv&machine=mini2", "names a machine"},
		{"no host at all", "agent=claude-rc", "needs both"},
		{"an invalid percent escape", "shed=%zz", "not a valid token"},
		{"the empty string", "", "needs both"},
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
}

func TestJoinNamesRendersEmptyAsNone(t *testing.T) {
	if got := joinNames(nil); got != "none" {
		t.Errorf("joinNames(nil) = %q, want %q", got, "none")
	}
	if got := joinNames([]string{"a", "b"}); got != "a, b" {
		t.Errorf("joinNames = %q", got)
	}
}
