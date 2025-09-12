ARG TAG=latest

FROM rocker/r-ver:${TAG}

ENV DEBIAN_FRONTEND=noninteractive
RUN apt-get update && apt-get install -y --no-install-recommends \
    ca-certificates curl wget git openssh-client gnupg \
    build-essential pkg-config \
    python3 python3-venv python3-pip \
    ripgrep jq unzip zip rsync less vim nano \
    procps sudo fzf zsh man-db gh aggregate \
 && apt-get clean && rm -rf /var/lib/apt/lists/*

ARG NODE_MAJOR=20
RUN mkdir -p /etc/apt/keyrings \
 && curl -fsSL https://deb.nodesource.com/gpgkey/nodesource-repo.gpg.key \
      | gpg --dearmor -o /etc/apt/keyrings/nodesource.gpg \
 && chmod a+r /etc/apt/keyrings/nodesource.gpg \
 && echo "deb [arch=$(dpkg --print-architecture) signed-by=/etc/apt/keyrings/nodesource.gpg] https://deb.nodesource.com/node_${NODE_MAJOR}.x nodistro main" \
      > /etc/apt/sources.list.d/nodesource.list \
 && apt-get update \
 && apt-get install -y --no-install-recommends nodejs \
 && apt-get clean && rm -rf /var/lib/apt/lists/* \
 && mkdir -p /usr/local/share/npm-global

ENV NPM_CONFIG_PREFIX=/usr/local/share/npm-global
ENV PATH=$PATH:/usr/local/share/npm-global/bin

RUN corepack enable

# Sanity check (visible in build logs)
RUN node --version && npm --version

# from claude code dockerfile
ARG GIT_DELTA_VERSION=0.18.2
RUN ARCH=$(dpkg --print-architecture) \
 && wget "https://github.com/dandavison/delta/releases/download/${GIT_DELTA_VERSION}/git-delta_${GIT_DELTA_VERSION}_${ARCH}.deb" \
 && sudo dpkg -i "git-delta_${GIT_DELTA_VERSION}_${ARCH}.deb" \
 && rm "git-delta_${GIT_DELTA_VERSION}_${ARCH}.deb"

ARG CLAUDE_CODE_VERSION=latest
RUN npm install -g @anthropic-ai/claude-code@${CLAUDE_CODE_VERSION}

ARG CODEX_VERSION=latest
RUN npm install -g @openai/codex@${CODEX_VERSION}

# Non-root default user (id doesn't have to match host; we will map with keep-id)
RUN groupadd -g 1000 dev \
 && useradd -m -u 1000 -g 1000 -s /bin/bash dev

COPY .zshrc .gitconfig .gitignore_global /home/dev/

WORKDIR /workspace
ENV SHELL=/bin/zsh
ENV EDITOR=vim
ENV VISUAL=vim
ENV DEVCONTAINER=true
ENV DISABLE_AUTOUPDATER=1
ENV CLAUDE_CONFIG_DIR=/home/dev/.claude
USER dev
