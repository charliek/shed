# Dockerfile for building VZ (Apple Virtualization.framework) rootfs image
# This creates a minimal Ubuntu 24.04 system with Docker, tmux, and shed-agent
# Differences from Firecracker:
# - Uses hvc0 console (virtio console) instead of ttyS0
# - Uses DHCP networking (VZ provides NAT) instead of static IP
# - Targets linux/arm64 for Apple Silicon

FROM ubuntu:24.04

SHELL ["/bin/bash", "-o", "pipefail", "-c"]

# Prevent interactive prompts during package installation
ENV DEBIAN_FRONTEND=noninteractive

# Install essential packages
RUN apt-get update && apt-get install -y \
    # System essentials
    systemd \
    systemd-sysv \
    dbus \
    # Utilities needed by provisioning hooks
    sudo \
    unzip \
    ca-certificates \
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
    # Linux kernel (needed for VZ LinuxBootloader and in-VM modules)
    linux-image-generic \
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

# Node.js 22.x
RUN curl -fsSL https://deb.nodesource.com/setup_22.x | bash - \
    && apt-get install -y --no-install-recommends nodejs \
    && rm -rf /var/lib/apt/lists/*

# Claude Code
RUN npm install -g @anthropic-ai/claude-code

# OpenCode
RUN npm i -g opencode-ai

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

# Create non-root shed user (UID 1000) with passwordless sudo (devcontainer pattern)
# Ubuntu 24.04 ships a default 'ubuntu' user at UID 1000; remove it first so
# files created inside the VM are owned by shed (UID 1000).
RUN userdel -r ubuntu 2>/dev/null || true \
    && useradd -m -s /bin/bash -u 1000 -G docker shed \
    && echo 'shed ALL=(ALL) NOPASSWD:ALL' > /etc/sudoers.d/shed \
    && chmod 0440 /etc/sudoers.d/shed

# Create workspace directory owned by shed
RUN mkdir -p /workspace && chown shed:shed /workspace

# Pre-create log directory so shed user can write logs without root
RUN mkdir -p /var/log/shed && chown shed:shed /var/log/shed

# Pre-create .ssh directory for credential transfers
RUN mkdir -p /home/shed/.ssh && chown shed:shed /home/shed/.ssh && chmod 700 /home/shed/.ssh

# Copy shed-agent binary (must be built before docker build)
COPY shed-agent /usr/local/bin/shed-agent
RUN chmod +x /usr/local/bin/shed-agent

# Copy shed-agent systemd service
COPY shed-agent.service /etc/systemd/system/shed-agent.service
RUN SYSTEMD_OFFLINE=1 systemctl enable shed-agent

# Copy network setup script (DHCP-based for VZ NAT networking)
COPY network-setup.sh /usr/local/bin/network-setup.sh
RUN chmod +x /usr/local/bin/network-setup.sh

# Copy network setup systemd service
COPY network-setup.service /etc/systemd/system/network-setup.service
RUN SYSTEMD_OFFLINE=1 systemctl enable network-setup

# Load vsock modules at boot (required for shed-agent communication with host)
RUN mkdir -p /etc/modules-load.d && \
    printf "%s\n" vsock vmw_vsock_virtio_transport > /etc/modules-load.d/vsock.conf

# Enable systemd-networkd for DHCP
RUN SYSTEMD_OFFLINE=1 systemctl enable systemd-networkd systemd-resolved

# Configure systemd-networkd for DHCP on all ethernet interfaces
RUN mkdir -p /etc/systemd/network && \
    printf "%s\n" \
    "[Match]" \
    "Name=en*" \
    "" \
    "[Network]" \
    "DHCP=yes" \
    > /etc/systemd/network/80-dhcp.network

# Switch to shed user for user-level tool installs
USER shed

# mise (version manager)
RUN curl https://mise.run | sh
ENV PATH="/home/shed/.local/share/mise/shims:/home/shed/.local/bin:${PATH}"

# UV + Python
RUN curl -LsSf https://astral.sh/uv/install.sh | sh
RUN /home/shed/.local/bin/uv python install 3.13

# Shell customization - colored prompt showing shed name and path
RUN echo 'export PS1="\\[\\033[1;36m\\][\\[\\033[1;32m\\]shed:\\${SHED_NAME:-dev}\\[\\033[1;36m\\]]\\[\\033[0m\\] \\[\\033[1;34m\\]\\w\\[\\033[0m\\]\\$ "' >> /home/shed/.bashrc

# Switch back to root for systemd entrypoint
USER root

# Set default working directory
WORKDIR /workspace

# Set the init system as entrypoint
ENTRYPOINT ["/sbin/init"]
