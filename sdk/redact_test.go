package sdk

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"
)

// secretCredential is a credential carrying every kind of secret at once, so a
// single check covers whichever channel is under test.
func secretCredential() Credential {
	return Credential{
		Bundle: Bundle{
			AuthMode:           AuthModeMTLS,
			HTTPSPort:          8443,
			TLSCertFingerprint: "sha256:abc",
			ClientCert:         "-----BEGIN CERTIFICATE-----\nLEAKED-CERT\n-----END CERTIFICATE-----\n",
			Token:              "shed_control_LEAKED-TOKEN",
			Scope:              "control",
			CertSerial:         "1a2b",
			ExpiresAt:          time.Date(2030, 1, 2, 3, 4, 5, 0, time.UTC),
		},
		KeyPEM: []byte("-----BEGIN EC PRIVATE KEY-----\nLEAKED-KEY\n-----END EC PRIVATE KEY-----\n"),
	}
}

// secrets are the substrings that must never appear in any rendering.
var secrets = []string{"LEAKED-TOKEN", "LEAKED-KEY", "LEAKED-CERT"}

func assertNoSecrets(t *testing.T, channel, rendered string) {
	t.Helper()
	for _, s := range secrets {
		if strings.Contains(rendered, s) {
			t.Errorf("%s leaked %q:\n%s", channel, s, rendered)
		}
	}
}

// TestCredentialNeverRendersItsSecrets walks every channel a value can escape
// through. String covers the common fmt verbs, but %#v bypasses Stringer
// entirely and encoding/json ignores both — and a structured logger reaches for
// exactly the one Stringer does not cover.
func TestCredentialNeverRendersItsSecrets(t *testing.T) {
	c := secretCredential()

	for _, tc := range []struct {
		channel  string
		rendered string
	}{
		{"%v", fmt.Sprintf("%v", c)},
		{"%+v", fmt.Sprintf("%+v", c)},
		{"%s", fmt.Sprintf("%s", c)},
		{"%#v", fmt.Sprintf("%#v", c)},
		{"%v on a pointer", fmt.Sprintf("%v", &c)},
		{"%#v on a pointer", fmt.Sprintf("%#v", &c)},
	} {
		assertNoSecrets(t, tc.channel, tc.rendered)
	}

	data, err := json.Marshal(c)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	assertNoSecrets(t, "json.Marshal", string(data))

	// The redacted form must still be USEFUL — a log line that says nothing is
	// not an improvement over one that says too much.
	for _, want := range []string{"mtls", "control", "1a2b"} {
		if !strings.Contains(string(data), want) {
			t.Errorf("the redacted JSON dropped useful metadata %q: %s", want, data)
		}
	}

	// slog's JSON handler goes through encoding/json, which is the channel
	// String() cannot reach.
	var buf bytes.Buffer
	slog.New(slog.NewJSONHandler(&buf, nil)).Info("minted", "credential", c)
	assertNoSecrets(t, "slog.JSONHandler", buf.String())

	var textBuf bytes.Buffer
	slog.New(slog.NewTextHandler(&textBuf, nil)).Info("minted", "credential", c)
	assertNoSecrets(t, "slog.TextHandler", textBuf.String())
}

// TestCredentialKeyPEMIsNotASerializableField pins the `json:"-"` tag
// specifically, so the protection survives even if the redacting MarshalJSON is
// ever removed or replaced.
func TestCredentialKeyPEMIsNotASerializableField(t *testing.T) {
	// Marshal the field through a struct that has no custom marshaler, which is
	// what a caller embedding Credential would get.
	type wrapper struct {
		KeyPEM []byte `json:"-"`
	}
	c := secretCredential()
	data, err := json.Marshal(wrapper{KeyPEM: c.KeyPEM})
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "{}" {
		t.Errorf("a `json:\"-\"` KeyPEM marshaled to %s, want {}", data)
	}

	// And the real type: the key is absent from the encoded form entirely, not
	// merely replaced somewhere a reader might miss.
	real, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(real, &decoded); err != nil {
		t.Fatal(err)
	}
	if v, ok := decoded["key"]; ok && v != "<redacted>" {
		t.Errorf("encoded key = %v, want it absent or <redacted>", v)
	}
}

// TestTokenCredentialRedactsToo: the mtls case above carries a certificate, but
// a token credential's bearer is just as sensitive and travels the same paths.
func TestTokenCredentialRedactsToo(t *testing.T) {
	c := Credential{Bundle: Bundle{
		AuthMode: AuthModeToken, HTTPPort: 8080, Token: "shed_control_LEAKED-TOKEN",
		Scope: "control", TokenID: "t1", ExpiresAt: time.Now(),
	}}
	data, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	assertNoSecrets(t, "json.Marshal (token mode)", string(data))
	assertNoSecrets(t, "%#v (token mode)", fmt.Sprintf("%#v", c))
	if !strings.Contains(string(data), "t1") {
		t.Errorf("the redacted JSON dropped the token id: %s", data)
	}
}

// TestBundleStillMarshalsForTheWire is the counterweight: Bundle is the wire
// type in BOTH directions, so it must keep encoding its credential verbatim. A
// redacting marshaler there would break the exchange it describes — which is
// why the redaction lives on Credential, a purely local type that is never
// decoded from anything.
func TestBundleStillMarshalsForTheWire(t *testing.T) {
	b := secretCredential().Bundle
	data, err := json.Marshal(b)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "LEAKED-TOKEN") {
		t.Errorf("Bundle stopped marshaling its token; the bootstrap wire form is broken: %s", data)
	}
	var round Bundle
	if err := json.Unmarshal(data, &round); err != nil {
		t.Fatal(err)
	}
	if round.Token != b.Token || round.ClientCert != b.ClientCert {
		t.Error("Bundle no longer round-trips through JSON")
	}
	// But it must not leak through the fmt verbs.
	assertNoSecrets(t, "Bundle %v", fmt.Sprintf("%v", b))
	assertNoSecrets(t, "Bundle %#v", fmt.Sprintf("%#v", b))
}
