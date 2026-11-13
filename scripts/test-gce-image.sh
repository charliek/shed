#!/usr/bin/env bash
# Offline checks for the GCE image tooling (gce/ and scripts/build-gce-image.sh):
#   1. bash -n on every script, and shellcheck when it is installed;
#   2. build-gce-image.sh --print against scripts/testdata/build-gce-image.print.golden
#      (UPDATE_GOLDEN=1 rewrites it);
#   3. gce/shed-identity.sh against a fake metadata server: hostname, identity
#      file, host key made once and published, authorized keys, and refusal of
#      missing or malformed metadata.
# Needs bash, python3, curl and ssh-keygen. Touches nothing outside a temp dir.
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "${repo_root}"

failures=0
pass() { echo "ok   $1"; }
fail() { echo "FAIL $1"; failures=$((failures + 1)); }
ok_if() { local d="$1"; shift; if "$@"; then pass "${d}"; else fail "${d}"; fi; }

scripts=(gce/*.sh scripts/build-gce-image.sh scripts/read-guest-pins.sh scripts/test-gce-image.sh)

# --- 1. syntax and lint ---------------------------------------------------------
for s in "${scripts[@]}"; do
  if bash -n "${s}"; then pass "bash -n ${s}"; else fail "bash -n ${s}"; fi
done
if command -v shellcheck >/dev/null 2>&1 && shellcheck --version >/dev/null 2>&1; then
  if shellcheck "${scripts[@]}"; then pass "shellcheck"; else fail "shellcheck"; fi
else
  echo "skip shellcheck (not installed)"
fi

# --- 2. --print golden ------------------------------------------------------------
golden=scripts/testdata/build-gce-image.print.golden
got="$(env -i PATH="${PATH}" HOME="${HOME}" bash scripts/build-gce-image.sh --print \
  --project example-project --zone example-zone-a --network example-net --subnet example-subnet \
  --tag example-bake --version v0.0.0-test --timestamp 20260101-000000 \
  --label purpose=example 2>&1)"
if [ "${UPDATE_GOLDEN:-}" = 1 ]; then
  printf '%s\n' "${got}" > "${golden}"
  echo "updated ${golden}"
fi
if [ "${got}" = "$(cat "${golden}")" ]; then
  pass "build-gce-image.sh --print matches ${golden}"
else
  fail "build-gce-image.sh --print differs from ${golden} (UPDATE_GOLDEN=1 to accept):"
  diff <(printf '%s\n' "${got}") "${golden}" || true
fi
for flag in --project --zone --network --subnet --tag; do
  if env -i PATH="${PATH}" HOME="${HOME}" bash scripts/build-gce-image.sh --print \
      --project p --zone z --network n --subnet s --tag t "${flag}" "" >/dev/null 2>&1; then
    fail "build-gce-image.sh refuses an empty ${flag}"
  else
    pass "build-gce-image.sh refuses an empty ${flag}"
  fi
done

# --- 3. shed-identity.sh against a fake metadata server --------------------------------
work="$(mktemp -d)"
server_pid=""
cleanup() {
  if [ -n "${server_pid}" ]; then kill "${server_pid}" 2>/dev/null || true; fi
  rm -rf "${work}"
}
trap cleanup EXIT

mkdir -p "${work}/md/attrs" "${work}/md/guest" "${work}/py"
cat > "${work}/py/fake_metadata.py" <<'PY'
import os, sys
from http.server import BaseHTTPRequestHandler, HTTPServer

root = sys.argv[1]
prefix = "/computeMetadata/v1/instance/"

class H(BaseHTTPRequestHandler):
    def log_message(self, *a):
        pass

    def _key(self, kind):
        if self.headers.get("Metadata-Flavor") != "Google":
            self.send_error(403)
            return None
        p = self.path
        if not p.startswith(prefix + kind + "/"):
            self.send_error(404)
            return None
        rest = p[len(prefix + kind + "/"):]
        if not rest or ".." in rest:
            self.send_error(400)
            return None
        return rest

    def do_GET(self):
        if self.path.startswith(prefix + "guest-attributes/"):
            key = self._key("guest-attributes")
            base = os.path.join(root, "guest")
        else:
            key = self._key("attributes")
            base = os.path.join(root, "attrs")
        if key is None:
            return
        f = os.path.join(base, key)
        if not os.path.isfile(f):
            self.send_error(404)
            return
        body = open(f, "rb").read()
        self.send_response(200)
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def do_PUT(self):
        key = self._key("guest-attributes")
        if key is None:
            return
        n = int(self.headers.get("Content-Length", "0"))
        body = self.rfile.read(n)
        f = os.path.join(root, "guest", key)
        os.makedirs(os.path.dirname(f), exist_ok=True)
        open(f, "wb").write(body)
        self.send_response(200)
        self.send_header("Content-Length", "0")
        self.end_headers()

srv = HTTPServer(("127.0.0.1", 0), H)
open(os.path.join(root, "port"), "w").write(str(srv.server_address[1]))
srv.serve_forever()
PY
python3 -I "${work}/py/fake_metadata.py" "${work}/md" &
server_pid=$!
for _ in $(seq 1 50); do [ -s "${work}/md/port" ] && break; sleep 0.1; done
port="$(cat "${work}/md/port")"

ssh-keygen -q -t ed25519 -N '' -C 'server key' -f "${work}/server_key"
server_pub="$(cat "${work}/server_key.pub")"

identity() { # root-dir -> runs shed-identity.sh against the fake
  SHED_METADATA_URL="http://127.0.0.1:${port}/computeMetadata/v1" \
  SHED_IDENTITY_ROOT="$1" SHED_IDENTITY_TIMEOUT=2 \
    bash gce/shed-identity.sh 2>"${work}/identity.err"
}
set_attr() { printf '%s' "$2" > "${work}/md/attrs/$1"; }
clear_attrs() { rm -f "${work}/md/attrs/"* ; rm -rf "${work}/md/guest/"*; }

# Happy path.
r="${work}/root1"
mkdir -p "${r}/etc"
printf '127.0.0.1 localhost\n127.0.1.1 old-name\n' > "${r}/etc/hosts"
set_attr shed-name my-shed
set_attr shed-id abcdefghij0123456789abcdef
set_attr shed-link-authorized-keys "${server_pub}"$'\r\n'
if identity "${r}"; then pass "identity: succeeds with full metadata"; else fail "identity: succeeds with full metadata ($(cat "${work}/identity.err"))"; fi
ok_if "identity: hostname file" test "$(cat "${r}/etc/hostname" 2>/dev/null)" = my-shed
# shellcheck disable=SC2016 # $1 expands in the inner bash
ok_if "identity: /etc/hosts" bash -c 'grep -qx "127.0.1.1 my-shed # shed-identity" "$1" && ! grep -q old-name "$1" && grep -q "^127.0.0.1 localhost$" "$1"' _ "${r}/etc/hosts"
ok_if "identity: identity.env" test "$(cat "${r}/run/shed/identity.env" 2>/dev/null)" = "$(printf 'SHED_NAME=my-shed\nSHED_ID=abcdefghij0123456789abcdef')"
ok_if "identity: authorized_keys (CR stripped)" test "$(cat "${r}/etc/ssh/shed-link/authorized_keys" 2>/dev/null)" = "${server_pub}"
ok_if "identity: authorized_keys mode 0644" test "$(stat -c %a "${r}/etc/ssh/shed-link/authorized_keys")" = 644
ok_if "identity: host key mode 0600" test "$(stat -c %a "${r}/etc/ssh/ssh_host_ed25519_key")" = 600
pub1="$(ssh-keygen -y -f "${r}/etc/ssh/ssh_host_ed25519_key" | awk '{print $1" "$2}')"
ok_if "identity: host key published as shed/host-key" test "$(cat "${work}/md/guest/shed/host-key" 2>/dev/null)" = "${pub1}"
case "${pub1}" in "ssh-ed25519 "*) pass "identity: host key is ed25519" ;; *) fail "identity: host key is ed25519" ;; esac

# Second boot: same host key, new authorized keys (rotation: two lines).
ssh-keygen -q -t ed25519 -N '' -C 'next' -f "${work}/server_key2"
set_attr shed-link-authorized-keys "${server_pub}"$'\n'"$(cat "${work}/server_key2.pub")"
identity "${r}" || fail "identity: second boot"
pub2="$(ssh-keygen -y -f "${r}/etc/ssh/ssh_host_ed25519_key" | awk '{print $1" "$2}')"
ok_if "identity: host key kept across boots" test "${pub1}" = "${pub2}"
ok_if "identity: two keys during rotation" test "$(wc -l < "${r}/etc/ssh/shed-link/authorized_keys")" -eq 2
ok_if "identity: one hosts line after two boots" test "$(grep -c '# shed-identity$' "${r}/etc/hosts")" -eq 1

# Refusals.
refuse() { # description
  local rr="${work}/root-$RANDOM"
  mkdir -p "${rr}/etc"
  if identity "${rr}"; then
    fail "identity: refuses $1"
  elif [ -e "${rr}/run/shed/identity.env" ]; then
    fail "identity: refuses $1 (but wrote identity.env)"
  else
    pass "identity: refuses $1"
  fi
}
clear_attrs; set_attr shed-id abcdefghij0123456789abcdef; set_attr shed-link-authorized-keys "${server_pub}"
refuse "a missing shed-name"
clear_attrs; set_attr shed-name my-shed; set_attr shed-link-authorized-keys "${server_pub}"
refuse "a missing shed-id"
clear_attrs; set_attr shed-name my-shed; set_attr shed-id abcdefghij0123456789abcdef
refuse "missing authorized keys"
set_attr shed-link-authorized-keys ""
refuse "empty authorized keys"
set_attr shed-link-authorized-keys "command=\"/bin/sh\" ${server_pub}"
refuse "an authorized key with options"
set_attr shed-link-authorized-keys "${server_pub}"
set_attr shed-name "Bad_Name"
refuse "an invalid shed name"
set_attr shed-name "my-shed"; set_attr shed-id "x;rm"
refuse "an invalid shed ID"

if [ "${failures}" -gt 0 ]; then
  echo "test-gce-image: ${failures} failure(s)"
  exit 1
fi
echo "test-gce-image: all checks passed"
