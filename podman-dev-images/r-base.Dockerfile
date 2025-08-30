ARG TAG=latest

FROM rocker/r-ver:${TAG}

ENV DEBIAN_FRONTEND=noninteractive
RUN apt-get update && apt-get install -y --no-install-recommends \
    curl git openssh-client gnupg \
    build-essential pkg-config \
    ripgrep jq rsync less vim \
  && rm -rf /var/lib/apt/lists/*

# Non-root default user (id doesn't have to match host; we will map with keep-id)
RUN useradd -m -u 1000 -s /bin/bash dev
WORKDIR /workspace
USER dev

