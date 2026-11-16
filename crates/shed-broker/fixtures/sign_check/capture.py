#!/usr/bin/env python3
"""Record real ssh-agent sign requests for `shed_broker::sign_check`'s tests.

Runs INSIDE a throwaway ubuntu:24.04 container (see `capture.sh` and the
README); it is not meant to run on a developer machine. Everything it touches
is created here and dies with the container:

- three throwaway user keys (ed25519, RSA 3072, ECDSA P-256) and three host
  keys, generated for this run; no key from the host is ever read;
- a real `ssh-agent` holding the user keys;
- a CAPTURING agent socket in front of it: it forwards every ssh-agent message
  to the real agent unchanged and records each SSH_AGENTC_SIGN_REQUEST (13);
- two real OpenSSH `sshd`s listening on 127.0.0.1 inside the container only:
  one with ed25519, RSA and ECDSA host keys, and one whose ed25519 host key
  carries a certificate signed by a throwaway CA.

Each case runs a real OpenSSH client (`ssh`, or `ssh-keygen -Y sign`) through
the capturing socket and must succeed: the login must be ACCEPTED by sshd, and
each SSHSIG must verify with `ssh-keygen -Y verify`. So every recorded request
is one OpenSSH really sent and really got a valid signature for. Each case
must produce exactly one sign request.

Output: `vectors.json` in the directory named by argv[1].
"""

from __future__ import annotations

import base64
import json
import os
import socket
import struct
import subprocess
import sys
import threading
import time
from pathlib import Path

WORK = Path("/tmp/signcap")
AGENT_SOCK = WORK / "agent.sock"
CAPTURE_SOCK = WORK / "capture.sock"
PORT = 2222
# A second sshd whose ed25519 host key carries a CA-signed host certificate, so
# the host-bound field gets a real vector from a certificate-presenting server.
CERT_PORT = 2223
SIGN_REQUEST = 13

records: list[dict] = []
records_lock = threading.Lock()


def run(argv: list[str], **kw) -> subprocess.CompletedProcess:
    proc = subprocess.run(argv, capture_output=True, text=True, **kw)
    if proc.returncode != 0:
        log = WORK / "sshd.log"
        raise RuntimeError(
            f"{argv[0]} exited {proc.returncode}\nstderr:\n{proc.stderr}\n"
            + (f"sshd log:\n{log.read_text()}" if log.exists() else "")
        )
    return proc


def read_exact(conn: socket.socket, n: int) -> bytes | None:
    buf = b""
    while len(buf) < n:
        chunk = conn.recv(n - len(buf))
        if not chunk:
            return None
        buf += chunk
    return buf


def read_msg(conn: socket.socket) -> bytes | None:
    head = read_exact(conn, 4)
    if head is None:
        return None
    (n,) = struct.unpack(">I", head)
    return read_exact(conn, n)


def write_msg(conn: socket.socket, payload: bytes) -> None:
    conn.sendall(struct.pack(">I", len(payload)) + payload)


def ssh_string(buf: bytes, pos: int) -> tuple[bytes, int]:
    (n,) = struct.unpack(">I", buf[pos : pos + 4])
    return buf[pos + 4 : pos + 4 + n], pos + 4 + n


def record_sign_request(payload: bytes) -> None:
    # SSH_AGENTC_SIGN_REQUEST: byte 13, string key blob, string data, uint32 flags.
    key, pos = ssh_string(payload, 1)
    data, pos = ssh_string(payload, pos)
    (flags,) = struct.unpack(">I", payload[pos : pos + 4])
    assert pos + 4 == len(payload), "sign request has trailing bytes"
    with records_lock:
        records.append({"public_key": key, "data": data, "flags": flags})


def proxy(client: socket.socket) -> None:
    with client, socket.socket(socket.AF_UNIX) as upstream:
        upstream.connect(str(AGENT_SOCK))
        while True:
            msg = read_msg(client)
            if msg is None:
                return
            if msg and msg[0] == SIGN_REQUEST:
                record_sign_request(msg)
            write_msg(upstream, msg)
            reply = read_msg(upstream)
            if reply is None:
                return
            write_msg(client, reply)


def serve_capture() -> None:
    srv = socket.socket(socket.AF_UNIX)
    srv.bind(str(CAPTURE_SOCK))
    srv.listen(16)
    while True:
        conn, _ = srv.accept()
        threading.Thread(target=proxy, args=(conn,), daemon=True).start()


def wait_for(cond, what: str, timeout: float = 10.0) -> None:
    deadline = time.monotonic() + timeout
    while time.monotonic() < deadline:
        if cond():
            return
        time.sleep(0.05)
    raise RuntimeError(f"timed out waiting for {what}")


def port_open(port: int) -> bool:
    try:
        with socket.create_connection(("127.0.0.1", port), timeout=0.25):
            return True
    except OSError:
        return False


def keygen(path: Path, kind: str, bits: str | None, comment: str) -> None:
    argv = ["ssh-keygen", "-q", "-t", kind, "-N", "", "-C", comment, "-f", str(path)]
    if bits:
        argv[4:4] = ["-b", bits]
    run(argv)


# (name, user key, ssh -o options, login user, expectation under the default policy)
LOGIN_CASES = [
    ("login-ed25519-hostbound-sntrup", "ed25519", {"PubkeyAuthentication": "host-bound"}, "git", "accept"),
    (
        "login-ed25519-publickey-curve25519",
        "ed25519",
        {"PubkeyAuthentication": "unbound", "KexAlgorithms": "curve25519-sha256"},
        "git",
        "accept",
    ),
    (
        "login-ed25519-hostbound-nistp384-ecdsahost",
        "ed25519",
        {
            "PubkeyAuthentication": "host-bound",
            "KexAlgorithms": "ecdh-sha2-nistp384",
            "HostKeyAlgorithms": "ecdsa-sha2-nistp256",
        },
        "git",
        "accept",
    ),
    (
        "login-ed25519-hostbound-group14sha1-rsahost",
        "ed25519",
        {
            "PubkeyAuthentication": "host-bound",
            "KexAlgorithms": "diffie-hellman-group14-sha1",
            "HostKeyAlgorithms": "rsa-sha2-512",
        },
        "git",
        "accept",
    ),
    ("login-rsa-sha2-512-hostbound", "rsa", {"PubkeyAuthentication": "host-bound"}, "git", "accept"),
    (
        "login-rsa-sha2-256-publickey",
        "rsa",
        {"PubkeyAuthentication": "unbound", "PubkeyAcceptedAlgorithms": "rsa-sha2-256"},
        "git",
        "accept",
    ),
    (
        "login-ssh-rsa-hostbound",
        "rsa",
        {"PubkeyAuthentication": "host-bound", "PubkeyAcceptedAlgorithms": "ssh-rsa"},
        "git",
        "accept",
    ),
    ("login-ecdsa-p256-hostbound", "ecdsa", {"PubkeyAuthentication": "host-bound"}, "git", "accept"),
    (
        "login-ed25519-hostbound-user-deploy",
        "ed25519",
        {"PubkeyAuthentication": "host-bound"},
        "deploy",
        "refuse:username_not_allowed",
    ),
    (
        "login-rsa-publickey-user-deploy",
        "rsa",
        {"PubkeyAuthentication": "unbound"},
        "deploy",
        "refuse:username_not_allowed",
    ),
]

# (name, user key, namespace, extra ssh-keygen args, expectation)
# Against the host-certificate sshd (CERT_PORT), trusting only its CA.
HOSTCERT_CASES = [
    (
        "login-ed25519-hostbound-hostcert",
        "ed25519",
        {"PubkeyAuthentication": "host-bound", "HostKeyAlgorithms": "ssh-ed25519-cert-v01@openssh.com"},
        "git",
        "accept",
    ),
]

SSHSIG_CASES = [
    ("sshsig-ed25519-git-sha512", "ed25519", "git", [], "accept"),
    ("sshsig-ed25519-git-sha256", "ed25519", "git", ["-O", "hashalg=sha256"], "accept"),
    ("sshsig-rsa-git-sha512", "rsa", "git", [], "accept"),
    ("sshsig-ecdsa-git-sha256", "ecdsa", "git", ["-O", "hashalg=sha256"], "accept"),
    ("sshsig-ed25519-file-sha512", "ed25519", "file", [], "refuse:wrong_namespace"),
]


def take_one(case: str) -> dict:
    with records_lock:
        got = list(records)
        records.clear()
    if len(got) != 1:
        raise RuntimeError(f"{case}: expected exactly one sign request, got {len(got)}")
    return got[0]


def main() -> None:
    out_dir = Path(sys.argv[1])
    WORK.mkdir(mode=0o700, parents=True, exist_ok=True)
    os.chmod(WORK, 0o700)

    # Throwaway login users. A '*' password is not "locked" to OpenSSH (a '!'
    # one is, and `useradd` leaves '!'), and password auth is off anyway.
    for user in ("git", "deploy"):
        run(["useradd", "-m", "-s", "/bin/sh", user])
        run(["usermod", "-p", "*", user])
    os.makedirs("/run/sshd", exist_ok=True)

    keys = {"ed25519": ("ed25519", None), "rsa": ("rsa", "3072"), "ecdsa": ("ecdsa", "256")}
    for name, (kind, bits) in keys.items():
        keygen(WORK / f"user_{name}", kind, bits, f"throwaway-{name}")
        keygen(WORK / f"host_{name}", kind, bits, f"throwaway-host-{name}")
    keygen(WORK / "host_ca", "ed25519", None, "throwaway-host-ca")
    run(
        [
            "ssh-keygen", "-q", "-s", str(WORK / "host_ca"), "-I", "throwaway-host", "-h",
            "-n", "127.0.0.1", str(WORK / "host_ed25519.pub"),
        ]
    )
    host_cert = WORK / "host_ed25519-cert.pub"
    # Read by sshd AS the login user, so it lives outside the 0700 work dir.
    authorized = Path("/etc/ssh/signcap_authorized_keys")
    authorized.write_text("".join((WORK / f"user_{n}.pub").read_text() for n in keys))
    authorized.chmod(0o644)
    known_hosts = WORK / "known_hosts"
    known_hosts.write_text(
        "".join(
            f"[127.0.0.1]:{PORT} " + " ".join((WORK / f"host_{n}.pub").read_text().split()[:2]) + "\n"
            for n in keys
        )
    )
    known_hosts_ca = WORK / "known_hosts_ca"
    known_hosts_ca.write_text(
        f"@cert-authority [127.0.0.1]:{CERT_PORT} "
        + " ".join((WORK / "host_ca.pub").read_text().split()[:2])
        + "\n"
    )
    signers = WORK / "allowed_signers"
    signers.write_text(
        "".join(
            "throwaway " + " ".join((WORK / f"user_{n}.pub").read_text().split()[:2]) + "\n"
            for n in keys
        )
    )

    def sshd_config(port: int, host_lines: list[str]) -> Path:
        path = WORK / f"sshd_config_{port}"
        lines = [
            f"Port {port}",
            "ListenAddress 127.0.0.1",
            *host_lines,
            "PidFile none",
            "UsePAM no",
            "StrictModes no",
            "PasswordAuthentication no",
            "KbdInteractiveAuthentication no",
            "PubkeyAuthentication yes",
            f"AuthorizedKeysFile {authorized}",
            "PermitRootLogin no",
            "AllowTcpForwarding no",
            "X11Forwarding no",
            # Off by default in this version; enabled only so the 20-byte
            # session id and the SHA-1 `ssh-rsa` signature get a real vector.
            "KexAlgorithms +diffie-hellman-group14-sha1",
            "PubkeyAcceptedAlgorithms +ssh-rsa",
            "LogLevel VERBOSE",
        ]
        path.write_text("\n".join(lines) + "\n")
        return path

    sshd_log = WORK / "sshd.log"
    plain = sshd_config(PORT, [f"HostKey {WORK / f'host_{n}'}" for n in keys])
    certified = sshd_config(CERT_PORT, [f"HostKey {WORK / 'host_ed25519'}", f"HostCertificate {host_cert}"])
    sshds = [
        subprocess.Popen(["/usr/sbin/sshd", "-D", "-f", str(cfg), "-E", str(sshd_log)])
        for cfg in (plain, certified)
    ]
    agent = subprocess.Popen(["ssh-agent", "-D", "-a", str(AGENT_SOCK)], stdout=subprocess.DEVNULL)
    try:
        wait_for(lambda: port_open(PORT) and port_open(CERT_PORT), "both sshds on 127.0.0.1")
        wait_for(AGENT_SOCK.exists, "ssh-agent socket")
        env = dict(os.environ, SSH_AUTH_SOCK=str(AGENT_SOCK))
        for n in keys:
            run(["ssh-add", "-q", str(WORK / f"user_{n}")], env=env)
        threading.Thread(target=serve_capture, daemon=True).start()
        wait_for(CAPTURE_SOCK.exists, "capture socket")

        vectors = []
        logins = [(c, PORT, known_hosts) for c in LOGIN_CASES]
        logins += [(c, CERT_PORT, known_hosts_ca) for c in HOSTCERT_CASES]
        for (name, key, opts, user, expect), port, kh in logins:
            argv = [
                "ssh",
                "-F",
                "/dev/null",
                "-o",
                f"IdentityAgent={CAPTURE_SOCK}",
                "-o",
                "IdentitiesOnly=yes",
                "-i",
                str(WORK / f"user_{key}.pub"),
                "-o",
                "BatchMode=yes",
                "-o",
                "StrictHostKeyChecking=yes",
                "-o",
                f"UserKnownHostsFile={kh}",
                "-o",
                "GlobalKnownHostsFile=/dev/null",
                "-p",
                str(port),
            ]
            for k, v in opts.items():
                argv += ["-o", f"{k}={v}"]
            argv += [f"{user}@127.0.0.1", "true"]
            # check=True: the login must have SUCCEEDED, i.e. sshd verified the
            # signature over exactly these bytes.
            run(argv)
            rec = take_one(name)
            vectors.append(
                {
                    "name": name,
                    "shape": "login",
                    "client": "ssh " + " ".join(f"-o {k}={v}" for k, v in opts.items()) + f" {user}@127.0.0.1",
                    "user_key": key,
                    "username": user,
                    "expect": expect,
                    **encode(rec),
                }
            )

        message = WORK / "message"
        message.write_text("shed sign_check fixture\n")
        for name, key, ns, extra, expect in SSHSIG_CASES:
            sig = WORK / "message.sig"
            sig.unlink(missing_ok=True)
            run(
                ["ssh-keygen", "-Y", "sign", "-n", ns, "-f", str(WORK / f"user_{key}.pub"), *extra, str(message)],
                env=dict(os.environ, SSH_AUTH_SOCK=str(CAPTURE_SOCK)),
            )
            rec = take_one(name)
            with message.open("rb") as stdin:
                subprocess.run(
                    ["ssh-keygen", "-Y", "verify", "-f", str(signers), "-I", "throwaway", "-n", ns, "-s", str(sig)],
                    stdin=stdin,
                    check=True,
                    capture_output=True,
                )
            vectors.append(
                {
                    "name": name,
                    "shape": "sshsig",
                    "client": f"ssh-keygen -Y sign -n {ns} " + " ".join(extra),
                    "user_key": key,
                    "namespace": ns,
                    "expect": expect,
                    **encode(rec),
                }
            )

        accepted = sum(1 for line in sshd_log.read_text().splitlines() if "Accepted publickey" in line)
        if accepted != len(logins):
            raise RuntimeError(f"sshd logged {accepted} accepted logins, expected {len(logins)}")

        versions = {
            "client": run(["ssh", "-V"]).stderr.strip(),
            "server": run(["dpkg-query", "-W", "-f", "${Version}", "openssh-server"]).stdout.strip(),
            "image": Path("/etc/os-release").read_text().split("PRETTY_NAME=")[1].splitlines()[0].strip('"'),
        }
        doc = {
            "comment": (
                "Real ssh-agent sign requests recorded by capture.py (see README.md). "
                "Keys are throwaway, generated for the capture. Do not edit by hand."
            ),
            "versions": versions,
            # The CA-signed host certificate the CERT_PORT sshd presented: a real
            # certificate blob, for the tests that refuse a certificate as the
            # key to sign with.
            "host_certificate_b64": host_cert.read_text().split()[1],
            "vectors": vectors,
        }
        (out_dir / "vectors.json").write_text(json.dumps(doc, indent=2) + "\n")
        print(f"recorded {len(vectors)} vectors; {versions}")
    finally:
        for proc in (*sshds, agent):
            proc.terminate()
        for proc in (*sshds, agent):
            proc.wait(timeout=10)


def encode(rec: dict) -> dict:
    return {
        "flags": rec["flags"],
        "public_key_b64": base64.b64encode(rec["public_key"]).decode(),
        "data_b64": base64.b64encode(rec["data"]).decode(),
    }


if __name__ == "__main__":
    main()
