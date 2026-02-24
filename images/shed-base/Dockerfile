FROM ubuntu:24.04

SHELL ["/bin/bash", "-o", "pipefail", "-c"]

# System packages
RUN apt-get update && apt-get install -y \
    curl \
    git \
    tmux \
    vim \
    emacs-nox \
    wget \
    jq \
    openssh-client \
    build-essential \
    ca-certificates \
    ncurses-term \
    tree \
    ripgrep \
    unzip \
    sudo \
    && rm -rf /var/lib/apt/lists/*
# unzip: for extracting archives in provisioning scripts
# sudo: for provisioning scripts that need elevated privileges

# Create non-root shed user (UID 1000) with passwordless sudo (devcontainer pattern)
# Ubuntu 24.04 ships a default 'ubuntu' user at UID 1000; remove it first so
# bind-mounted host files (typically owned by UID 1000) are readable by shed.
RUN userdel -r ubuntu 2>/dev/null || true \
    && useradd -m -s /bin/bash -u 1000 shed \
    && echo 'shed ALL=(ALL) NOPASSWD:ALL' > /etc/sudoers.d/shed \
    && chmod 0440 /etc/sudoers.d/shed

# Node.js 22.x
RUN curl -fsSL https://deb.nodesource.com/setup_22.x | bash - \
    && apt-get install -y nodejs \
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
    && apt-get update && apt-get install -y gh \
    && rm -rf /var/lib/apt/lists/*

# Switch to shed user for user-level tool installs
USER shed

# mise
RUN curl https://mise.run | sh
ENV PATH="/home/shed/.local/share/mise/shims:/home/shed/.local/bin:${PATH}"

# UV + Python
RUN curl -LsSf https://astral.sh/uv/install.sh | sh
RUN /home/shed/.local/bin/uv python install 3.13

# Shell customization - colored prompt showing shed name and path
RUN echo 'export PS1="\\[\\033[1;36m\\][\\[\\033[1;32m\\]shed:\\${SHED_NAME:-dev}\\[\\033[1;36m\\]]\\[\\033[0m\\] \\[\\033[1;34m\\]\\w\\[\\033[0m\\]\\$ "' >> /home/shed/.bashrc

WORKDIR /workspace

CMD ["sleep", "infinity"]
