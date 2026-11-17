# Sign-check vectors

`vectors.json` holds real `ssh-agent` sign requests (`SSH_AGENTC_SIGN_REQUEST`:
the key blob, the data to sign, the flags word) recorded from a real OpenSSH
client. `shed_broker::sign_check`'s tests (`crates/shed-broker/src/sign_check.rs`)
accept every vector recorded as `accept`, refuse the others with the recorded
class, and build every mutation, truncation and fuzz case from them. Each vector
is checked under its recorded `flags` (the check binds an RSA key's hash bits to
the payload), and a test asserts the recorded flags are what a genuine client
sends for that payload.

**Do not edit `vectors.json` by hand.** Re-record it with `capture.sh`.

## What was recorded

| Recorded with | Version |
|---|---|
| Client (`ssh`, `ssh-keygen`, `ssh-agent`) | `OpenSSH_9.6p1 Ubuntu-3ubuntu13.20, OpenSSL 3.0.13 30 Jan 2024` |
| Server (`sshd`) | Ubuntu package `openssh-server` `1:9.6p1-3ubuntu13.20` |
| Image | `ubuntu:24.04` (Ubuntu 24.04.5 LTS) |

The versions are also written into `vectors.json` by each recording.

| Vectors | What they cover |
|---|---|
| `login-*-hostbound-*` | the host-bound method, `publickey-hostbound-v00@openssh.com`, which an OpenSSH client uses against an OpenSSH 8.9+ server (`-o PubkeyAuthentication=host-bound`) |
| `login-*-publickey-*` | the plain `publickey` method (`-o PubkeyAuthentication=unbound`) |
| key exchanges | `sntrup761x25519-sha512` (the default) and `curve25519-sha256`, `ecdh-sha2-nistp384`, `diffie-hellman-group14-sha1`: session ids of 64, 32, 48 and 20 bytes |
| user keys | Ed25519; RSA 3072 signing as `rsa-sha2-512`, `rsa-sha2-256` and `ssh-rsa`; ECDSA P-256 |
| server host keys in the host-bound field | Ed25519, RSA, ECDSA P-256, and an Ed25519 host **certificate** (the client sends the certificate itself; `host_certificate_b64` is that certificate) |
| `*-user-deploy` | a login as a username the default policy does not allow (`refuse:username_not_allowed`) |
| `sshsig-*` | `ssh-keygen -Y sign -n git` with `sha512` (the default) and `-O hashalg=sha256`, for each key type, and one with `-n file` (`refuse:wrong_namespace`) |

Every login was **accepted by the real sshd**, and every SSHSIG signature
**verified** with `ssh-keygen -Y verify`, so each vector is a request OpenSSH
really sent and really got a valid signature for. `capture.py` fails if a case
produces anything other than exactly one sign request.

The host-bound layout (one more `string`, the server host key, after the key
blob) was checked against OpenSSH's `PROTOCOL` §3.1 and the agent-side parser
`parse_userauth_request` in `ssh-agent.c`, both at the `V_9_6_P1` tag of
`openssh/openssh-portable`, and it matches these recordings.

## Re-recording

```bash
crates/shed-broker/fixtures/sign_check/capture.sh
```

It needs Docker and nothing else. The host has no `sshd` requirement: like the
machine-transport suite's recipe for a box without one
(`tests/machine-transport/README.md`), the whole capture runs in a throwaway
`ubuntu:24.04` container:

1. installs `openssh-server`, `openssh-client` and `python3`;
2. `capture.py` creates two login users (`git`, `deploy`), generates
   **throwaway** user keys (Ed25519, RSA 3072, ECDSA P-256), host keys and a
   host CA for this run only — no key from the host is read, and none is kept;
3. starts a real `ssh-agent` holding the user keys, and a capturing agent socket
   in front of it that forwards every message unchanged and records each sign
   request;
4. starts two `sshd`s on the container's own `127.0.0.1` (one with plain host
   keys, one presenting the host certificate), runs each case through the
   capturing socket, and checks the outcome;
5. writes `vectors.json` back into this directory.

The container, its `sshd`s, its agent and every key are gone when it exits. New
keys and new session ids mean every vector changes on a re-record; the tests
must pass against the new file without edits, and a version change goes into the
table above.
