# Dockerfile for building Firecracker rootfs image
# This creates a minimal Ubuntu 24.04 system with Docker, tmux, and shed-agent

FROM ubuntu:24.04

# Prevent interactive prompts during package installation
ENV DEBIAN_FRONTEND=noninteractive

# Install essential packages
RUN apt-get update && apt-get install -y \
    # System essentials
    systemd \
    systemd-sysv \
    dbus \
    # Docker
    docker.io \
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
    # SSH server for alternative access
    openssh-server \
    # Networking utilities
    iproute2 \
    iputils-ping \
    dnsutils \
    netcat-openbsd \
    # Build essentials (for compiling code)
    build-essential \
    # Clean up
    && rm -rf /var/lib/apt/lists/*

# Install mise (version manager) - matches Docker base image
RUN curl https://mise.run | sh
ENV PATH="/root/.local/share/mise/shims:/root/.local/bin:${PATH}"

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

# Create workspace directory
RUN mkdir -p /workspace && chmod 755 /workspace

# Create a non-root user for running workloads
RUN useradd -m -s /bin/bash -G docker shed && \
    echo "shed ALL=(ALL) NOPASSWD:ALL" >> /etc/sudoers

# Copy shed-agent binary (must be built before docker build)
COPY shed-agent /usr/local/bin/shed-agent
RUN chmod +x /usr/local/bin/shed-agent

# Copy shed-agent systemd service
COPY shed-agent.service /etc/systemd/system/shed-agent.service
RUN SYSTEMD_OFFLINE=1 systemctl enable shed-agent

# Copy network setup script
COPY network-setup.sh /usr/local/bin/network-setup.sh
RUN chmod +x /usr/local/bin/network-setup.sh

# Copy network setup systemd service
COPY network-setup.service /etc/systemd/system/network-setup.service
RUN SYSTEMD_OFFLINE=1 systemctl enable network-setup

# Set default working directory
WORKDIR /workspace

# Set the init system as entrypoint
ENTRYPOINT ["/sbin/init"]
