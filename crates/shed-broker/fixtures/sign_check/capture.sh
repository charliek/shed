#!/bin/sh
# Re-record vectors.json in a throwaway ubuntu:24.04 container (see README.md).
#
# The container installs OpenSSH, generates throwaway keys, runs a real sshd on
# its own 127.0.0.1 and records what a real OpenSSH client asks a capturing
# ssh-agent socket to sign. Nothing on the host is read except this directory,
# and only vectors.json is written back. The container (and with it sshd,
# ssh-agent and every key) is removed when the run ends.
set -eu
here=$(cd "$(dirname "$0")" && pwd)

docker run --rm -v "$here:/recipe" ubuntu:24.04 sh -c '
  set -e
  export DEBIAN_FRONTEND=noninteractive
  apt-get update -qq >/dev/null
  apt-get install -y -qq openssh-server openssh-client python3 >/dev/null
  mkdir -p /out
  python3 -I /recipe/capture.py /out
  cp /out/vectors.json /recipe/vectors.json
  chown --reference=/recipe/capture.py /recipe/vectors.json'
