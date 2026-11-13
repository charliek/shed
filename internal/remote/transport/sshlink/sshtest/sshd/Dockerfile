# A stand-in for a cloud shed's guest, for the SSH link's tests: OpenSSH's
# sshd configured the way guest layout 1 configures it, the link user, and a
# shed-agent on unix sockets. Built by internal/remote/transport/sshlink/sshtest.
FROM ubuntu:24.04

RUN apt-get update \
 && DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends openssh-server \
 && rm -rf /var/lib/apt/lists/* \
 && rm -f /etc/ssh/ssh_host_*_key /etc/ssh/ssh_host_*_key.pub

# The link user: a system user with a real shell and a password field of "*"
# (no password, not locked). sshd reads its key from a root-owned file.
RUN useradd --system --user-group --create-home --shell /bin/sh shed-link \
 && usermod -p '*' shed-link \
 && mkdir -p /etc/ssh/shed-link /etc/shed /run/sshd \
 && echo 1 > /etc/shed/layout

# 10-shed.conf is guest.SSHDConfig, written into the build context by the test.
COPY 10-shed.conf /etc/ssh/sshd_config.d/10-shed.conf
COPY entrypoint.sh /usr/local/sbin/shed-test-entrypoint
COPY shed-agent /usr/local/bin/shed-agent
RUN chmod 0755 /usr/local/sbin/shed-test-entrypoint /usr/local/bin/shed-agent \
 && chmod 0644 /etc/ssh/sshd_config.d/10-shed.conf

EXPOSE 22
ENTRYPOINT ["/usr/local/sbin/shed-test-entrypoint"]
