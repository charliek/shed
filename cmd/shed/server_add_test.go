package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"testing"

	gossh "golang.org/x/crypto/ssh"
)

func testHostKey(t *testing.T) (hostKey, fingerprint string) {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := gossh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	return string(gossh.MarshalAuthorizedKey(signer.PublicKey())),
		gossh.FingerprintSHA256(signer.PublicKey())
}

func TestConfirmHostKey(t *testing.T) {
	hostKey, fp := testHostKey(t)

	// Args: (hostKey, expectedFingerprint, trustOnFirstUse, interactive, jsonMode).
	t.Run("matching fingerprint verifies", func(t *testing.T) {
		if err := confirmHostKey(hostKey, fp, false, false, false); err != nil {
			t.Errorf("matching fingerprint should succeed: %v", err)
		}
	})
	t.Run("fingerprint without SHA256: prefix is normalized", func(t *testing.T) {
		if err := confirmHostKey(hostKey, fp[len("SHA256:"):], false, false, false); err != nil {
			t.Errorf("bare fingerprint should succeed: %v", err)
		}
	})
	t.Run("mismatched fingerprint fails closed", func(t *testing.T) {
		if err := confirmHostKey(hostKey, "SHA256:deadbeefdeadbeefdeadbeef", false, false, false); err == nil {
			t.Error("mismatched fingerprint must fail")
		}
	})
	t.Run("no fingerprint non-interactive trusts on first use", func(t *testing.T) {
		if err := confirmHostKey(hostKey, "", false, false, false); err != nil {
			t.Errorf("non-interactive TOFU should succeed: %v", err)
		}
	})
	t.Run("json mode without trust flag fails closed", func(t *testing.T) {
		// --json can't prompt and must not silently trust a possibly-MITM key.
		if err := confirmHostKey(hostKey, "", false, false, true); err == nil {
			t.Error("json mode without --fingerprint/--trust-on-first-use must fail")
		}
	})
	t.Run("trust-on-first-use accepts even in json mode", func(t *testing.T) {
		if err := confirmHostKey(hostKey, "", true, false, true); err != nil {
			t.Errorf("--trust-on-first-use should accept: %v", err)
		}
	})
	t.Run("matching fingerprint accepts even in json mode", func(t *testing.T) {
		if err := confirmHostKey(hostKey, fp, false, false, true); err != nil {
			t.Errorf("--fingerprint should accept in json mode: %v", err)
		}
	})
	t.Run("unparseable host key fails", func(t *testing.T) {
		if err := confirmHostKey("not a valid key", fp, false, false, false); err == nil {
			t.Error("unparseable host key must fail")
		}
	})
}
