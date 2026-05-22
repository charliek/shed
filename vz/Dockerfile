# syntax=docker/dockerfile:1.7
#
# Multi-stage Dockerfile for VZ (Apple Virtualization.framework) rootfs images.
#
# Stages:
#   shed-vz-base        - Minimal: shed infrastructure + basic dev tools
#   shed-vz-extensions  - Base + shed-extensions credential brokering (no coding agents).
#                         Recommended starting point for users who customize their own image.
#   shed-vz-full        - Extensions + mise/Node/Python/UV + all coding agents
#                         (Claude Code, OpenCode, Cursor CLI, Codex CLI)
#
# Build a specific variant:
#   docker buildx build --platform linux/arm64 --target shed-vz-base --output type=oci,dest=base.tar .
#   docker buildx build --platform linux/arm64 --target shed-vz-extensions --output type=oci,dest=ext.tar .
#   docker buildx build --platform linux/arm64 --target shed-vz-full --output type=oci,dest=full.tar .
#
# Or use the build script:
#   ./scripts/build-vz-rootfs.sh --variant base
#
# Layer-count discipline: each stage uses as few RUNs as practical so the
# total layer count per variant stays well under MaxLayers (16). COPYs
# from the build context are folded into a single RUN using
# `--mount=type=bind,source=.,target=/ctx` (BuildKit 1.4+).

# Bump this when shed-extensions releases a new version.
# The extensions and full variants pull binaries from this tag.
ARG SHED_EXT_VERSION=v0.3.1
FROM ghcr.io/charliek/shed-extensions:${SHED_EXT_VERSION} AS shed-extensions

# Note: the shed initramfs (overlay-on-the-guest init) is built from
# initramfs/Dockerfile via scripts/build-initramfs.sh. It lives in its
# own Dockerfile so this file's COPY paths can keep resolving relative
# to the vz/ build context.

# =============================================================================
# Stage: shed-vz-base
# Everything shed needs to function + basic development tools.
# =============================================================================
FROM ubuntu:24.04 AS shed-vz-base

LABEL io.shed.base-version="1"
LABEL org.opencontainers.image.source="https://github.com/charliek/shed"

SHELL ["/bin/bash", "-o", "pipefail", "-c"]
ENV DEBIAN_FRONTEND=noninteractive

# Strip man pages, docs, and non-English locales from every subsequent
# dpkg/apt operation. Must come BEFORE the first apt-get install so the
# excludes apply to all packages. Keep en/en_US locales — many tools
# (less, perl, bash completion) misbehave without at least one locale.
RUN printf '%s\n' \
    'path-exclude=/usr/share/man/*' \
    'path-exclude=/usr/share/doc/*' \
    'path-exclude=/usr/share/locale/*' \
    'path-include=/usr/share/locale/en/*' \
    'path-include=/usr/share/locale/en_US/*' \
    > /etc/dpkg/dpkg.cfg.d/01-shed-strip

# Layer 1: APT-installed system + dev tools + Docker CE in one go.
# linux-image-virtual + initramfs-tools provide the in-VM kernel
# (extracted at image-build time by shed image build for VZ booting).
# linux-image-virtual is ~100 MB smaller than linux-image-generic because
# it ships a minimal recommended-modules set: virtio_blk, virtio_net,
# virtio_pci, 9p, 9pnet_virtio, loop, overlay. It does NOT ship the
# vsock guest-transport modules (vmw_vsock_virtio_transport[_common]);
# those live in linux-modules-extra-$KVERS-generic, which Noble does
# NOT expose as a -virtual metapackage (only kernel-flavor-specific
# aliases like -extra-aws/-azure/-gcp exist). We derive the exact
# kernel version from the linux-image-*-generic package the
# linux-image-virtual metapackage pulled in and install the matching
# -extra package by name. Without it, shed-agent.service can't open
# its vsock listener and host health checks time out.
# Editor lineup: neovim only. vim/nano/jed/htop dropped to save ~80 MB;
# users who need them can `apt install` inside a shed.
RUN apt-get update \
    && apt-get install -y --no-install-recommends \
       systemd systemd-sysv systemd-resolved dbus \
       sudo unzip ca-certificates hostname \
       tmux git gh curl wget neovim jq ncurses-term ripgrep tree \
       openssh-server \
       iproute2 iputils-ping dnsutils netcat-openbsd \
       build-essential bubblewrap \
       linux-image-virtual initramfs-tools \
    && KVERS=$(dpkg-query -W -f='${Package}\n' 'linux-image-*-generic' | head -1 | sed 's/linux-image-//') \
    && [ -n "$KVERS" ] || { echo "ERROR: could not derive kernel version from linux-image-*-generic"; exit 1; } \
    && apt-get install -y --no-install-recommends "linux-modules-extra-$KVERS" \
    && rm -rf /var/lib/apt/lists/*

# Layer 2: stage shed-built binaries + service files from the build
# context via a bind mount so we get a single layer for the placements
# + chmods + configs (instead of 1 layer per COPY). daemon.json is
# staged but placed by the `full` stage where docker is actually
# installed.
RUN --mount=type=bind,source=.,target=/ctx \
    install -m 0755 /ctx/shed-agent       /usr/local/bin/shed-agent && \
    install -m 0755 /ctx/shed-firstboot   /usr/local/bin/shed-firstboot && \
    install -m 0755 /ctx/network-setup.sh /usr/local/bin/network-setup.sh && \
    install -m 0644 /ctx/shed-agent.service     /etc/systemd/system/shed-agent.service && \
    install -m 0644 /ctx/shed-firstboot.service /etc/systemd/system/shed-firstboot.service && \
    install -m 0644 /ctx/network-setup.service  /etc/systemd/system/network-setup.service && \
    install -m 0644 /ctx/daemon.json            /usr/local/share/shed-docker-daemon.json

# Layer 3: systemd cleanup + service enablement + user + filesystem +
# machine-id transient setup + networking + bashrc — all in one RUN so
# we get one layer for the entire "configure the image" pass.
RUN set -euo pipefail && \
    # Strip unwanted systemd targets that don't make sense in a microVM.
    rm -f /lib/systemd/system/multi-user.target.wants/* \
          /etc/systemd/system/*.wants/* \
          /lib/systemd/system/local-fs.target.wants/* \
          /lib/systemd/system/sockets.target.wants/*udev* \
          /lib/systemd/system/sockets.target.wants/*initctl* \
          /lib/systemd/system/sysinit.target.wants/systemd-tmpfiles-setup* \
          /lib/systemd/system/systemd-update-utmp* && \
    # Enable services shipped by base. docker is enabled in the `full`
    # stage where it's actually installed.
    SYSTEMD_OFFLINE=1 systemctl enable ssh shed-agent shed-firstboot \
        network-setup systemd-networkd systemd-resolved && \
    # SSH server policy.
    mkdir -p /run/sshd /etc/ssh/sshd_config.d && \
    printf '%s\n' \
        'PermitRootLogin no' \
        'PasswordAuthentication no' \
        > /etc/ssh/sshd_config.d/10-shed.conf && \
    # SSH client: tolerate macOS-only directives that may travel in
    # transferred host configs.
    mkdir -p /etc/ssh/ssh_config.d && \
    printf '%s\n' \
        'Host *' \
        '    IgnoreUnknown UseKeychain,AddKeysToAgent' \
        > /etc/ssh/ssh_config.d/10-macos-compat.conf && \
    # Non-root shed user (UID 1000) with passwordless sudo (devcontainer
    # pattern). Ubuntu 24.04 ships its own 'ubuntu' at UID 1000 — drop
    # it first so files created in the VM are owned by shed.
    # docker group is pre-created so `-G docker` works regardless of
    # whether the `full` stage layers docker-ce on top. Use --system so
    # GID is allocated from the system range (docker-ce postinst
    # matches by group NAME, not GID, so any free GID is fine).
    userdel -r ubuntu 2>/dev/null || true && \
    (getent group docker >/dev/null 2>&1 || groupadd --system docker) && \
    useradd -m -s /bin/bash -u 1000 -G docker shed && \
    echo 'shed ALL=(ALL) NOPASSWD:ALL' > /etc/sudoers.d/shed && \
    chmod 0440 /etc/sudoers.d/shed && \
    # PATH setup: .bash_profile is the login shell entrypoint;
    # .bashrc is sourced by interactive shells.
    printf '%s\n' \
        'export PATH="/home/shed/.local/bin:$PATH"' \
        '[ -f ~/.bashrc ] && . ~/.bashrc' \
        > /home/shed/.bash_profile && \
    chown shed:shed /home/shed/.bash_profile && \
    # Workspace + log dir + .ssh.
    mkdir -p /workspace && chown shed:shed /workspace && \
    mkdir -p /var/log/shed && chown shed:shed /var/log/shed && \
    mkdir -p /home/shed/.ssh && chown shed:shed /home/shed/.ssh && chmod 700 /home/shed/.ssh && \
    # Vsock kernel modules at boot.
    mkdir -p /etc/modules-load.d && \
    printf '%s\n' vsock vmw_vsock_virtio_transport > /etc/modules-load.d/vsock.conf && \
    # DHCP on all en* interfaces.
    mkdir -p /etc/systemd/network && \
    printf '%s\n' \
        '[Match]' \
        'Name=en*' \
        '' \
        '[Network]' \
        'DHCP=yes' \
        > /etc/systemd/network/80-dhcp.network && \
    # IPv4 forward so docker compose networks route through enp0s1.
    printf 'net.ipv4.ip_forward=1\n' > /etc/sysctl.d/99-docker-forward.conf && \
    # Make machine-id transient (lives in /run, never persists to disk).
    # Without this, dbus's postinst bakes a single UUID into the image
    # and every shed (fresh or snapshot-spawned) inherits it. The
    # symlink → /run trick makes systemd treat machine-id as transient:
    # PID 1 generates a fresh value into /run/machine-id at every VM
    # boot. systemd-machine-id-commit is masked so systemd doesn't try
    # to convert the symlink back to a real file.
    rm -f /etc/machine-id /var/lib/dbus/machine-id && \
    ln -sf /run/machine-id /etc/machine-id && \
    ln -sf /etc/machine-id /var/lib/dbus/machine-id && \
    ln -sf /dev/null /etc/systemd/system/systemd-machine-id-commit.service && \
    # Colored shell prompt with shed name + cwd + git branch + dirty marker.
    cat >> /home/shed/.bashrc <<'PROMPT_EOF'
__git_prompt() {
    local branch
    branch=$(git symbolic-ref --short HEAD 2>/dev/null) || return
    local dirty=""
    [ -n "$(git status --porcelain 2>/dev/null)" ] && dirty=$'\001\033[31m\002✗'
    printf " \001\033[01;32m\002(%s)%s\001\033[00m\002" "$branch" "$dirty"
}
export PS1='\[\033[1;36m\][\[\033[1;32m\]shed:${SHED_NAME:-dev}\[\033[1;36m\]]\[\033[0m\] \[\033[01;34m\]\w\[\033[00m\]$(__git_prompt) \$ '
PROMPT_EOF

WORKDIR /workspace
ENTRYPOINT ["/sbin/init"]


# =============================================================================
# Stage: shed-vz-extensions
# Base + shed-extensions credential brokering (SSH agent, AWS proxy,
# Docker credentials). No coding agents or language runtimes —
# recommended starting point for users who customize their own image.
# See https://github.com/charliek/shed-extensions for details.
# =============================================================================
FROM shed-vz-base AS shed-vz-extensions

# Layer: bring in shed-extensions binaries + service units + config + the
# Docker credentials helper config — all in a single COPY-via-bind RUN
# so this stage adds exactly one layer on top of base.
RUN --mount=type=bind,from=shed-extensions,source=/,target=/ext \
    set -euo pipefail && \
    mkdir -p /etc/environment.d /etc/shed-extensions.d /home/shed/.docker && \
    install -m 0755 /ext/usr/local/bin/shed-ext-ssh-agent       /usr/local/bin/shed-ext-ssh-agent && \
    install -m 0755 /ext/usr/local/bin/shed-ext-aws-credentials /usr/local/bin/shed-ext-aws-credentials && \
    install -m 0755 /ext/usr/local/bin/docker-credential-shed   /usr/local/bin/docker-credential-shed && \
    install -m 0644 /ext/etc/systemd/system/shed-ext-ssh-agent.service       /etc/systemd/system/shed-ext-ssh-agent.service && \
    install -m 0644 /ext/etc/systemd/system/shed-ext-aws-credentials.service /etc/systemd/system/shed-ext-aws-credentials.service && \
    install -m 0644 /ext/etc/environment.d/shed-extensions.conf              /etc/environment.d/shed-extensions.conf && \
    cp -r /ext/etc/shed-extensions.d/. /etc/shed-extensions.d/ && \
    # Docker creds helper config for the shed user.
    echo '{"credsStore": "shed"}' > /home/shed/.docker/config.json && \
    chown -R shed:shed /home/shed/.docker
    # Units ship disabled — shed-agent enables them from server config.

WORKDIR /workspace
ENTRYPOINT ["/sbin/init"]


# =============================================================================
# Stage: shed-vz-full
# Extensions + Bun + coding agents (Claude Code, OpenCode, Codex CLI).
# mise stays for Go-binary tools (prox, envsecrets). Node/Python/uv
# dropped — Bun is the JS runtime + package manager, and users who
# need Python can `uv python install 3.13` inside a shed.
# =============================================================================
FROM shed-vz-extensions AS shed-vz-full

ENV PATH="/home/shed/.local/share/mise/shims:/home/shed/.bun/bin:/home/shed/.local/bin:${PATH}"
# CLAUDE_CONFIG_DIR tells Claude Code to read/write .claude.json inside
# ~/.claude/ instead of at ~/.claude.json. This lets a VirtioFS
# credential mount of ~/.claude/ cover all Claude state.
ENV CLAUDE_CONFIG_DIR="/home/shed/.claude"

# Docker (only in `full`). base + extensions ship without docker; users
# who want nested containers pull `full`. docker-buildx-plugin is NOT
# installed (rarely used by coding agents; users can `apt install` on
# demand). docker-compose-plugin is kept for `docker compose`.
RUN install -m 0755 -d /etc/apt/keyrings \
    && curl -fsSL https://download.docker.com/linux/ubuntu/gpg -o /etc/apt/keyrings/docker.asc \
    && chmod a+r /etc/apt/keyrings/docker.asc \
    && printf "Types: deb\nURIs: https://download.docker.com/linux/ubuntu\nSuites: %s\nComponents: stable\nSigned-By: /etc/apt/keyrings/docker.asc\n" \
       "$(. /etc/os-release && echo "${UBUNTU_CODENAME:-$VERSION_CODENAME}")" \
       > /etc/apt/sources.list.d/docker.sources \
    && apt-get update \
    && apt-get install -y --no-install-recommends \
       docker-ce docker-ce-cli containerd.io docker-compose-plugin \
    && rm -rf /var/lib/apt/lists/* \
    && install -m 0644 /usr/local/share/shed-docker-daemon.json /etc/docker/daemon.json \
    && SYSTEMD_OFFLINE=1 systemctl enable docker

# Layer: install all language runtimes + coding agents as the shed user,
# folded into a single sudo-driven RUN.
RUN set -euo pipefail && \
    # profile.d shim so login shells see the shed user's PATH.
    # Bun installs to ~/.bun/bin and is the runtime for OpenCode + Codex.
    printf '%s\n' \
        '# Only add shed user paths for the shed user (not root)' \
        'if [ "$(id -u)" = "1000" ]; then' \
        '  export PATH="/home/shed/.local/share/mise/shims:/home/shed/.bun/bin:/home/shed/.local/bin:$PATH"' \
        'fi' \
        > /etc/profile.d/shed-path.sh && \
    # CLAUDE_CONFIG_DIR for login shells (matches the ENV above).
    echo 'export CLAUDE_CONFIG_DIR=/home/shed/.claude' > /etc/profile.d/claude-config.sh && \
    # All shed-user installations happen via `runuser -l shed -c` so the
    # umasks, HOMEs, and downloaders behave as if `shed` ran them.
    runuser -l shed -c 'set -euo pipefail; \
        # mise (kept for prox + envsecrets Go binaries; no Node/Python). \
        curl https://mise.run | sh && \
        echo "eval \"\$(mise activate bash)\"" >> /home/shed/.bashrc && \
        # Bun (single-binary runtime + package manager for opencode + codex). \
        curl -fsSL https://bun.com/install | bash && \
        # Claude Code (ships its own bundled runtime; not Node-dependent). \
        curl -fsSL https://claude.ai/install.sh | bash && \
        # Additional mise-managed tools. \
        mise use -g github:charliek/prox github:charliek/envsecrets && \
        # OpenCode via Bun. \
        /home/shed/.bun/bin/bun add -g opencode-ai && \
        # Codex CLI via Bun (npm-compatible). \
        /home/shed/.bun/bin/bun install -g @openai/codex \
    '

WORKDIR /workspace
ENTRYPOINT ["/sbin/init"]
