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

# Set up .bash_profile to handle PATH (login shell) and source .bashrc (interactive).
# PATH modifications belong here, not in .bashrc, to prevent shell snapshot tools
# from capturing raw PATH assignments with unexpanded variable references.
RUN printf '%s\n' \
    'export PATH="/home/shed/.local/bin:$PATH"' \
    '[ -f ~/.bashrc ] && . ~/.bashrc' \
    > /home/shed/.bash_profile \
    && chown shed:shed /home/shed/.bash_profile

# Pre-create log directory so shed user can write logs without root
RUN mkdir -p /var/log/shed && chown shed:shed /var/log/shed

# Node.js 22.x
RUN curl -fsSL https://deb.nodesource.com/setup_22.x | bash - \
    && apt-get install -y nodejs \
    && rm -rf /var/lib/apt/lists/*

# Claude Code
# CLAUDE_CONFIG_DIR tells Claude Code to read/write .claude.json inside ~/.claude/
# instead of at ~/.claude.json in the home directory root. This lets credential
# mounts of ~/.claude/ cover all Claude state without a separate file transfer.
RUN npm install -g @anthropic-ai/claude-code \
    && echo 'export CLAUDE_CONFIG_DIR="/home/shed/.claude"' > /etc/profile.d/claude-config.sh
ENV CLAUDE_CONFIG_DIR="/home/shed/.claude"

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
