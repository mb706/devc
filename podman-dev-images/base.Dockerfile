FROM ubuntu:24.04

ENV DEBIAN_FRONTEND=noninteractive
RUN apt-get update && apt-get install -y --no-install-recommends \
    ca-certificates curl git openssh-client gnupg \
    build-essential pkg-config \
    python3 python3-venv python3-pip \
    nodejs npm \
    ripgrep jq unzip zip rsync less vim nano \
  && rm -rf /var/lib/apt/lists/*

# Non-root default user (id doesn't have to match host; we will map with keep-id)
RUN userdel -r ubuntu 2>/dev/null || true \
 && groupdel ubuntu 2>/dev/null || true \
 && groupadd -g 1000 dev \
 && useradd -m -u 1000 -g 1000 -s /bin/bash dev

WORKDIR /workspace
USER dev

