package remote

import (
	"regexp"
	"testing"

	"golang.org/x/crypto/ssh"
)

// The local link's stand-in key goes to the account service in every
// create's authorized_keys, so it must be one well-formed public key line:
// the compute contract's grammar (the reference service's authorizedKeyRx)
// and one key OpenSSH parses. A stand-in that is not one made every create
// on a local-link server against a contract-following service fail INVALID.
func TestLocalAuthorizedKeyIsOnePublicKeyLine(t *testing.T) {
	contract := regexp.MustCompile(`^[a-z0-9@.-]{1,64} [A-Za-z0-9+/]+={0,2}( [\x20-\x7e]*)?$`)
	if !contract.MatchString(localAuthorizedKey) {
		t.Fatalf("%q breaks the compute contract's authorized key grammar", localAuthorizedKey)
	}
	pub, _, _, rest, err := ssh.ParseAuthorizedKey([]byte(localAuthorizedKey))
	if err != nil {
		t.Fatalf("ParseAuthorizedKey(%q): %v", localAuthorizedKey, err)
	}
	if len(rest) != 0 {
		t.Fatalf("more than one line: %q", rest)
	}
	if pub.Type() != ssh.KeyAlgoED25519 {
		t.Fatalf("type %s, want %s", pub.Type(), ssh.KeyAlgoED25519)
	}
}
