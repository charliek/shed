# Multi-stage Dockerfile for Firecracker rootfs images
#
# Stages:
#   kernel-builder      - Temporary: compiles custom Firecracker kernel
#   shed-fc-base        - Minimal: shed infrastructure + basic dev tools + kernel
#   shed-fc-extensions  - Base + shed-extensions credential brokering (no coding agents).
#                         Recommended starting point for users who customize their own image.
#   shed-fc-full        - Extensions + mise/Node/Python/UV + all coding agents
#                         (Claude Code, OpenCode, Cursor CLI, Codex CLI)
#
# Build a specific variant:
#   docker buildx build --platform linux/amd64 --target shed-fc-base -t shed-fc-base:latest --load .
#   docker buildx build --platform linux/amd64 --target shed-fc-extensions -t shed-fc-extensions:latest --load .
#   docker buildx build --platform linux/amd64 --target shed-fc-full -t shed-fc-full:latest --load .
#
# Or use the build script:
#   ./scripts/build-firecracker-rootfs.sh --variant base

# Bump this when shed-extensions releases a new version.
# The extensions and full variants pull binaries from this tag.
ARG SHED_EXT_VERSION=v0.3.1
FROM ghcr.io/charliek/shed-extensions:${SHED_EXT_VERSION} AS shed-extensions

# Note: the shed initramfs (overlay-on-the-guest init) is built from
# initramfs/Dockerfile via scripts/build-initramfs.sh. It lives in its
# own Dockerfile so this file's COPY paths can keep resolving relative
# to the firecracker/ build context.

# =============================================================================
# Stage: kernel-builder (temporary)
# Compiles a custom Firecracker kernel with Docker, 9P, and BPF support.
# Only the vmlinux binary is copied into the final image.
# =============================================================================
FROM --platform=linux/amd64 ubuntu:24.04 AS kernel-builder

RUN apt-get update && apt-get install -y --no-install-recommends \
    build-essential flex bison bc libelf-dev libssl-dev \
    git curl ca-certificates file \
    && rm -rf /var/lib/apt/lists/*

# Pin kernel and Firecracker config versions for reproducibility.
# KERNEL_TAG: stable kernel release (6.1.x series).
# FC_VERSION: Firecracker release whose CI config is used as the base.
ARG KERNEL_TAG=v6.1.167
ARG FC_VERSION=v1.14.1

WORKDIR /build
RUN git clone --depth 1 --branch ${KERNEL_TAG} \
    https://git.kernel.org/pub/scm/linux/kernel/git/stable/linux.git linux

WORKDIR /build/linux

# Download Firecracker CI kernel config and merge custom fragment
RUN curl -fL -o .config \
    "https://raw.githubusercontent.com/firecracker-microvm/firecracker/${FC_VERSION}/resources/guest_configs/microvm-kernel-ci-x86_64-6.1.config"

COPY kernel-config-docker.fragment /tmp/fragment
RUN if [ -x scripts/kconfig/merge_config.sh ]; then \
        scripts/kconfig/merge_config.sh -m .config /tmp/fragment; \
    else \
        cat /tmp/fragment >> .config; \
    fi && make olddefconfig

RUN make -j$(nproc) vmlinux

# =============================================================================
# Stage: shed-fc-base
# Everything shed needs to function + basic development tools.
# =============================================================================
FROM ubuntu:24.04 AS shed-fc-base

LABEL io.shed.base-version="1"
LABEL org.opencontainers.image.source="https://github.com/charliek/shed"

SHELL ["/bin/bash", "-o", "pipefail", "-c"]

# Prevent interactive prompts during package installation
ENV DEBIAN_FRONTEND=noninteractive

# Install essential packages
RUN apt-get update && apt-get install -y --no-install-recommends \
    # System essentials
    systemd \
    systemd-sysv \
    dbus \
    # Utilities needed by provisioning hooks
    sudo \
    unzip \
    ca-certificates \
    # Required by shed-firstboot to apply the hostname read from kernel cmdline
    hostname \
    # Development tools
    tmux \
    git \
    curl \
    wget \
    vim \
    nano \
    htop \
    jq \
    ncurses-term \
    ripgrep \
    tree \
    # SSH server for alternative access
    openssh-server \
    # Networking utilities
    iproute2 \
    iputils-ping \
    dnsutils \
    netcat-openbsd \
    # Build essentials (for compiling code)
    build-essential \
    # Sandbox (used by Codex CLI)
    bubblewrap \
    # Clean up
    && rm -rf /var/lib/apt/lists/*

# Docker CE from official Docker repo (needed for docker-compose-plugin)
RUN install -m 0755 -d /etc/apt/keyrings \
    && curl -fsSL https://download.docker.com/linux/ubuntu/gpg -o /etc/apt/keyrings/docker.asc \
    && chmod a+r /etc/apt/keyrings/docker.asc \
    && printf "Types: deb\nURIs: https://download.docker.com/linux/ubuntu\nSuites: %s\nComponents: stable\nSigned-By: /etc/apt/keyrings/docker.asc\n" \
       "$(. /etc/os-release && echo "${UBUNTU_CODENAME:-$VERSION_CODENAME}")" \
       > /etc/apt/sources.list.d/docker.sources \
    && apt-get update \
    && apt-get install -y --no-install-recommends \
       docker-ce \
       docker-ce-cli \
       containerd.io \
       docker-buildx-plugin \
       docker-compose-plugin \
    && rm -rf /var/lib/apt/lists/*

# GitHub CLI
RUN curl -fsSL https://cli.github.com/packages/githubcli-archive-keyring.gpg \
      -o /usr/share/keyrings/githubcli-archive-keyring.gpg \
    && echo "deb [arch=$(dpkg --print-architecture) signed-by=/usr/share/keyrings/githubcli-archive-keyring.gpg] https://cli.github.com/packages stable main" \
      > /etc/apt/sources.list.d/github-cli.list \
    && apt-get update && apt-get install -y --no-install-recommends gh \
    && rm -rf /var/lib/apt/lists/*

# Configure systemd
# Remove unnecessary services
RUN rm -f /lib/systemd/system/multi-user.target.wants/* \
    /etc/systemd/system/*.wants/* \
    /lib/systemd/system/local-fs.target.wants/* \
    /lib/systemd/system/sockets.target.wants/*udev* \
    /lib/systemd/system/sockets.target.wants/*initctl* \
    /lib/systemd/system/sysinit.target.wants/systemd-tmpfiles-setup* \
    /lib/systemd/system/systemd-update-utmp*

# Configure Docker with vfs storage driver (most reliable in VM environments)
COPY daemon.json /etc/docker/daemon.json

# Enable Docker service
RUN SYSTEMD_OFFLINE=1 systemctl enable docker

# Enable SSH service
RUN SYSTEMD_OFFLINE=1 systemctl enable ssh

# Configure SSH
RUN mkdir -p /run/sshd /etc/ssh/sshd_config.d && \
    printf "%s\n" \
    "PermitRootLogin no" \
    "PasswordAuthentication no" \
    > /etc/ssh/sshd_config.d/10-shed.conf

# SSH client compatibility: ignore macOS-specific config directives
# (UseKeychain, AddKeysToAgent) that may be present in transferred host SSH configs
RUN mkdir -p /etc/ssh/ssh_config.d && \
    printf "%s\n" \
    "Host *" \
    "    IgnoreUnknown UseKeychain,AddKeysToAgent" \
    > /etc/ssh/ssh_config.d/10-macos-compat.conf

# Create non-root shed user (UID 1000) with passwordless sudo (devcontainer pattern)
# Ubuntu 24.04 ships a default 'ubuntu' user at UID 1000; remove it first so
# files created inside the VM are owned by shed (UID 1000).
RUN userdel -r ubuntu 2>/dev/null || true \
    && useradd -m -s /bin/bash -u 1000 -G docker shed \
    && echo 'shed ALL=(ALL) NOPASSWD:ALL' > /etc/sudoers.d/shed \
    && chmod 0440 /etc/sudoers.d/shed

# Set up .bash_profile to handle PATH (login shell) and source .bashrc (interactive).
# PATH modifications belong here, not in .bashrc, to prevent shell snapshot tools
# from capturing raw PATH assignments with unexpanded variable references.
RUN printf '%s\n' \
    'export PATH="/home/shed/.local/bin:$PATH"' \
    '[ -f ~/.bashrc ] && . ~/.bashrc' \
    > /home/shed/.bash_profile \
    && chown shed:shed /home/shed/.bash_profile

# Create workspace directory owned by shed
RUN mkdir -p /workspace && chown shed:shed /workspace

# Pre-create log directory so shed user can write logs without root
RUN mkdir -p /var/log/shed && chown shed:shed /var/log/shed

# Pre-create .ssh directory for credential transfers
RUN mkdir -p /home/shed/.ssh && chown shed:shed /home/shed/.ssh && chmod 700 /home/shed/.ssh

# Copy shed-agent binary (must be built before docker build)
COPY shed-agent /usr/local/bin/shed-agent
RUN chmod +x /usr/local/bin/shed-agent

# Copy shed-firstboot binary (handles identity regen on first/cloned boot)
COPY shed-firstboot /usr/local/bin/shed-firstboot
RUN chmod +x /usr/local/bin/shed-firstboot

# Copy shed-agent systemd service
COPY shed-agent.service /etc/systemd/system/shed-agent.service
RUN SYSTEMD_OFFLINE=1 systemctl enable shed-agent

# Copy shed-firstboot systemd service (runs at sysinit.target, before journald/sshd/shed-agent
# cache identity, so cloned rootfs gets fresh machine-id and SSH host keys)
COPY shed-firstboot.service /etc/systemd/system/shed-firstboot.service
RUN SYSTEMD_OFFLINE=1 systemctl enable shed-firstboot

# Copy network setup script
COPY network-setup.sh /usr/local/bin/network-setup.sh
RUN chmod +x /usr/local/bin/network-setup.sh

# Copy network setup systemd service
COPY network-setup.service /etc/systemd/system/network-setup.service
RUN SYSTEMD_OFFLINE=1 systemctl enable network-setup

# Copy custom Firecracker kernel (extracted by shed during image conversion)
COPY --from=kernel-builder /build/linux/vmlinux /boot/vmlinux

# Make machine-id transient (lives in /run, never persists to disk).
# Without this, dbus's postinst writes a value into /etc/machine-id at image
# build time, baking a single UUID into the image — every shed (fresh or
# snapshot-spawned) would inherit it. Symlinking /etc/machine-id to /run
# makes systemd treat it as transient: PID 1 generates a fresh value into
# /run/machine-id at every VM boot. /var/lib/dbus/machine-id is symlinked
# to /etc/machine-id (the modern systemd convention) so dbus sees the same
# value. systemd-machine-id-commit.service is masked so systemd doesn't try
# to convert the symlink back to a real file when root is writable. Net
# effect: every VM boot of every shed yields a unique machine-id, with
# nothing persisted to disk and no host-side ext4 manipulation required.
RUN rm -f /etc/machine-id /var/lib/dbus/machine-id && \
    ln -sf /run/machine-id /etc/machine-id && \
    ln -sf /etc/machine-id /var/lib/dbus/machine-id && \
    ln -sf /dev/null /etc/systemd/system/systemd-machine-id-commit.service

# Shell customization - colored prompt showing shed name, path, and git branch
USER shed
RUN cat >> /home/shed/.bashrc << 'PROMPT_EOF'
__git_prompt() {
    local branch
    branch=$(git symbolic-ref --short HEAD 2>/dev/null) || return
    local dirty=""
    [ -n "$(git status --porcelain 2>/dev/null)" ] && dirty=$'\001\033[31m\002✗'
    printf " \001\033[01;32m\002(%s)%s\001\033[00m\002" "$branch" "$dirty"
}
export PS1='\[\033[1;36m\][\[\033[1;32m\]shed:${SHED_NAME:-dev}\[\033[1;36m\]]\[\033[0m\] \[\033[01;34m\]\w\[\033[00m\]$(__git_prompt) \$ '
PROMPT_EOF
USER root

# Set default working directory
WORKDIR /workspace

# Set the init system as entrypoint
ENTRYPOINT ["/sbin/init"]


# =============================================================================
# Stage: shed-fc-extensions
# Base + shed-extensions credential brokering (SSH agent, AWS proxy, Docker
# credentials). No coding agents or language runtimes — recommended starting
# point for users who customize their own image.
# See https://github.com/charliek/shed-extensions for details.
# =============================================================================
FROM shed-fc-base AS shed-fc-extensions

COPY --from=shed-extensions /usr/local/bin/shed-ext-ssh-agent /usr/local/bin/shed-ext-ssh-agent
COPY --from=shed-extensions /usr/local/bin/shed-ext-aws-credentials /usr/local/bin/shed-ext-aws-credentials
COPY --from=shed-extensions /usr/local/bin/docker-credential-shed /usr/local/bin/docker-credential-shed
COPY --from=shed-extensions /etc/systemd/system/shed-ext-ssh-agent.service /etc/systemd/system/
COPY --from=shed-extensions /etc/systemd/system/shed-ext-aws-credentials.service /etc/systemd/system/
COPY --from=shed-extensions /etc/environment.d/shed-extensions.conf /etc/environment.d/
COPY --from=shed-extensions /etc/shed-extensions.d/ /etc/shed-extensions.d/
# Units ship disabled — shed-agent enables them from server config

# Configure Docker inside the guest to use shed credential helper
RUN mkdir -p /home/shed/.docker && \
    echo '{"credsStore": "shed"}' > /home/shed/.docker/config.json && \
    chown -R shed:shed /home/shed/.docker

USER root

WORKDIR /workspace

ENTRYPOINT ["/sbin/init"]


# =============================================================================
# Stage: shed-fc-full
# Extensions + mise, Node.js, Python (via UV), and all coding agents
# (Claude Code, OpenCode, Cursor CLI, Codex CLI). The full development
# experience built on top of the credential-brokering base.
# =============================================================================
FROM shed-fc-extensions AS shed-fc-full

USER shed

ENV PATH="/home/shed/.local/bin:${PATH}"

# mise (version manager)
RUN curl https://mise.run | sh
RUN echo 'eval "$(mise activate bash)"' >> /home/shed/.bashrc
ENV PATH="/home/shed/.local/share/mise/shims:${PATH}"

RUN mise use -g node@lts

# Ensure mise shims and local bin are in PATH for login shells via profile.d
USER root
RUN printf '%s\n' \
    '# Only add shed user paths for the shed user (not root)' \
    'if [ "$(id -u)" = "1000" ]; then' \
    '  export PATH="/home/shed/.local/share/mise/shims:/home/shed/.local/bin:$PATH"' \
    'fi' \
    > /etc/profile.d/shed-path.sh
USER shed

# Claude Code
# CLAUDE_CONFIG_DIR tells Claude Code to read/write .claude.json inside ~/.claude/
# instead of at ~/.claude.json in the home directory root. This lets credential
# mounts of ~/.claude/ cover all Claude state without a separate file transfer.
ENV CLAUDE_CONFIG_DIR="/home/shed/.claude"
RUN curl -fsSL https://claude.ai/install.sh | bash \
    && sudo sh -c 'echo "export CLAUDE_CONFIG_DIR=/home/shed/.claude" > /etc/profile.d/claude-config.sh'

# UV + Python
RUN curl -LsSf https://astral.sh/uv/install.sh | sh
RUN /home/shed/.local/bin/uv python install 3.13

# Additional mise tools
RUN mise use -g \
    github:charliek/prox \
    github:charliek/envsecrets

# OpenCode
RUN curl -fsSL https://opencode.ai/install | bash

# Cursor CLI (agent)
RUN curl https://cursor.com/install -fsS | bash

# Codex cli
RUN npm i -g @openai/codex

USER root

WORKDIR /workspace

ENTRYPOINT ["/sbin/init"]
