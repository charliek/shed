# Certificate user fixtures (two-language, written by Go)

Client certificates and the user each one names, read by BOTH:

- Go: `servertls.ClientCertUser` (`internal/servertls/clientuser.go`), and
- Rust: `shed_core::certuser::cert_user` (`crates/shed-core/src/certuser.rs`).

This folder is **written by a Go test**, never by hand:

```bash
UPDATE_CLIENTUSER=1 go test ./internal/servertls/ -run ClientUserFixture
```

Without `UPDATE_CLIENTUSER=1` the same test only checks the folder, so a fixture that no
longer matches its description fails `make test`. A built fixture is deterministic (fixed
Ed25519 keys, serial and times) and must equal what the test builds, byte for byte. The two
`issued-*` fixtures come from `SignClientCSR`, whose key, serial and validity differ on each
issue, and an update keeps them while they pass the check.

**What the check of an `issued-*` fixture tracks, and what it does not.** It tracks the
subject and issuer bytes only: the committed certificate's subject and issuer must equal,
byte for byte, those of a certificate issued now for the same user. It does **not** track
the rest of the certificate. A change to what `SignClientCSR` writes outside those two
names (validity, public key, signature algorithm, key usage, extended key usage, key
identifiers, any other extension) leaves the committed file passing as long as it still
parses, and neither reader verifies the signature, so a changed signature bit passes too.
These fixtures hold the two readers to the names the CA writes; they are not a copy of a
current certificate.

The folder lives under `crates/` because the Linux leg of the Rust core
(`make desktop-core-linux`) mounts only `crates/`.

`expected.json` records, for each fixture, Go's answer (computed by the test from the file:
`named:<user>`, `implicit`, `unbound`, `malformed`, `unparseable`) and Rust's answer (declared by
the test: `named:<user>`, `implicit`, `unbound`, `error`). The Rust test
(`crates/shed-core/tests/certuser_fixtures.rs`) requires its reader to give exactly Rust's answer.

**The rule** the Go test checks: where Go parses the certificate, Rust's answer equals Go's or
is `error`. Rust refuses some certificates Go reads (IA5String, T61String and BMPString values,
which the CA never writes). Where Go cannot parse the certificate, Rust's answer is recorded as
it is: Rust's strict reader refuses the damage it sees (a non-minimal or indefinite length,
trailing bytes, a broken structure on its path), but it does not validate what it skips
(validity, serial, signature, other subject attributes' contents), so it may still read a
user. No server accepts such a certificate, since every server parses it in Go. The fixtures
prove the rule on this set, in both languages; they prove nothing more.

## Go-unparseable fixtures

Go's `x509.ParseCertificate` refuses these; Rust's answer is recorded as it is.

| Fixture | What | Rust |
|---|---|---|
| `implicit-printable` | !implicit as a PrintableString (! is not printable) | `error` |
| `high-tag-value` | a value with a multi-byte tag | `error` |
| `attribute-no-value` | a CN attribute with no value, beside pat | `error` |
| `cn-not-printable` | a CN PrintableString holding @, beside pat | `named:pat` |
| `rdn-stray-element` | an RDN holding pat and then a NULL | `error` |
| `nonminimal-value-length` | pat's length in long form (81 03) | `error` |
| `nonminimal-length` | the certificate's length with a leading zero octet | `error` |
| `indefinite-length` | the certificate with an indefinite length | `error` |
| `trailing-bytes` | two bytes after the certificate | `error` |
| `damaged-validity` | a notBefore that is not a time | `named:pat` |
| `damaged-serial` | an empty serial INTEGER | `named:pat` |
| `damaged-signature` | a signature BIT STRING with 8 unused bits | `named:pat` |

## Every fixture

| Fixture | What | Go | Rust |
|---|---|---|---|
| `issued-pat` | issued by SignClientCSR for pat (PrintableString) | `named:pat` | `named:pat` |
| `issued-implicit` | issued by SignClientCSR for the implicit user (UTF8String !implicit) | `implicit` | `implicit` |
| `no-user` | a subject with a CN and no userId (a shed-server older than 0.10.0) | `unbound` | `unbound` |
| `empty-subject` | an empty subject | `unbound` | `unbound` |
| `utf8-pat` | pat as a UTF8String in its own RDN | `named:pat` | `named:pat` |
| `user-before-cn` | the userId RDN before the CN | `named:pat` | `named:pat` |
| `multi-valued-rdn` | pat in one multi-valued RDN beside the CN | `named:pat` | `named:pat` |
| `issuer-only` | a userId (admin) in the issuer only, none in the subject | `unbound` | `unbound` |
| `issuer-and-subject` | admin in the issuer and pat in the subject | `named:pat` | `named:pat` |
| `extension-only` | a userId (admin) only in a subjectAltName directoryName extension, none in the subject | `unbound` | `unbound` |
| `extension-oid-only` | an extension whose extnID is the userId OID (value admin), none in the subject | `unbound` | `unbound` |
| `subject-and-extension` | pat in the subject and admin in a subjectAltName directoryName | `named:pat` | `named:pat` |
| `v1-no-version` | a v1 certificate: no [0] version | `named:pat` | `named:pat` |
| `near-oid` | an attribute whose OID extends the userId OID by one arc | `unbound` | `unbound` |
| `longest-name` | a 63-character name, the longest | `named:az9-z9-z9-z9-z9-z9-z9-z9-z9-z9-z9-z9-z9-z9-z9-z9-z9-z9-z9-z9-zz` | `named:az9-z9-z9-z9-z9-z9-z9-z9-z9-z9-z9-z9-z9-z9-z9-z9-z9-z9-z9-z9-zz` |
| `third-rdn` | pat in the third RDN, after an OU | `named:pat` | `named:pat` |
| `too-long` | a 64-character name | `malformed` | `error` |
| `upper-case` | the value Pat | `malformed` | `error` |
| `trailing-newline` | pat followed by a newline, as a UTF8String | `malformed` | `error` |
| `empty-value` | an empty UTF8String | `malformed` | `error` |
| `other-marker` | the UTF8String !unresolved | `malformed` | `error` |
| `implicit-printable` | !implicit as a PrintableString (! is not printable) | `unparseable` | `error` |
| `ia5-pat` | pat as an IA5String | `named:pat` | `error` |
| `t61-pat` | pat as a T61String | `named:pat` | `error` |
| `bmp-pat` | pat as a BMPString | `named:pat` | `error` |
| `numeric-value` | 123 as a NumericString | `malformed` | `error` |
| `implicit-ia5` | !implicit as an IA5String | `implicit` | `error` |
| `integer-value` | an INTEGER value | `malformed` | `error` |
| `octet-string-value` | an OCTET STRING value | `malformed` | `error` |
| `high-tag-value` | a value with a multi-byte tag | `unparseable` | `error` |
| `two-rdns` | pat and admin, each in its own RDN | `malformed` | `error` |
| `two-in-one-rdn` | pat and admin in one multi-valued RDN with the CN | `malformed` | `error` |
| `same-user-twice` | pat twice, in two RDNs and two encodings | `malformed` | `error` |
| `named-and-implicit` | !implicit and pat | `malformed` | `error` |
| `attribute-two-values` | a userId attribute holding two values | `named:pat` | `error` |
| `attribute-no-value` | a CN attribute with no value, beside pat | `unparseable` | `error` |
| `cn-not-printable` | a CN PrintableString holding @, beside pat | `unparseable` | `named:pat` |
| `rdn-stray-element` | an RDN holding pat and then a NULL | `unparseable` | `error` |
| `empty-rdn` | an empty RDN before pat | `named:pat` | `error` |
| `nonminimal-value-length` | pat's length in long form (81 03) | `unparseable` | `error` |
| `nonminimal-length` | the certificate's length with a leading zero octet | `unparseable` | `error` |
| `indefinite-length` | the certificate with an indefinite length | `unparseable` | `error` |
| `trailing-bytes` | two bytes after the certificate | `unparseable` | `error` |
| `fourth-element` | a NULL after the signature, inside the certificate | `named:pat` | `error` |
| `damaged-validity` | a notBefore that is not a time | `unparseable` | `named:pat` |
| `damaged-serial` | an empty serial INTEGER | `unparseable` | `named:pat` |
| `damaged-signature` | a signature BIT STRING with 8 unused bits | `unparseable` | `named:pat` |
| `wrong-signature` | a well-formed signature that does not verify | `named:pat` | `named:pat` |
