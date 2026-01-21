FROM ubuntu:24.04

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
    && rm -rf /var/lib/apt/lists/*

# mise
RUN curl https://mise.run | sh

# RUN mise install \
#     java@temurin-21 \
#     node@lts \
#     uv \
#     gh 


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

# UV + Python
RUN curl -LsSf https://astral.sh/uv/install.sh | sh
RUN /root/.local/bin/uv python install 3.13

# Shell customization - colored prompt showing shed name and path
# Append to /root/.bashrc (runs last for login shells, after Ubuntu's default PS1)
RUN echo 'export PS1="\\[\\033[1;36m\\][\\[\\033[1;32m\\]shed:\\${SHED_NAME:-dev}\\[\\033[1;36m\\]]\\[\\033[0m\\] \\[\\033[1;34m\\]\\w\\[\\033[0m\\]\\$ "' >> /root/.bashrc

WORKDIR /workspace

CMD ["sleep", "infinity"]
