# syntax=docker/dockerfile:1
#
# Two-stage image for the Claude assistant bridge.
#
#   stage 1 (builder): compile the Go `assistant` binary.
#   stage 2 (runtime): debian-slim + the pinned native `claude` binary.
#
# The runtime mirrors the host layout (HOME=/home/user, claude at
# ~/.local/bin/claude, workdir /home/user/claudecgwd) so the bind-mounted
# config.yaml and session transcript keep resolving to the same paths they do
# on the host. See docs/DOCKER.md.

# ---- builder ----
FROM golang:1.26-bookworm AS builder
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
# Pure-Go (pty/vt10x use syscalls, no cgo) → build a static binary.
RUN CGO_ENABLED=0 go build -trimpath -o /out/assistant ./cmd/assistant

# ---- runtime ----
FROM debian:bookworm-slim

# Version of Claude Code to bake in. Override with --build-arg to bump.
ARG CLAUDE_VERSION=2.1.154
# Match the host user so bind-mounted files keep their ownership and claude can
# read its own credentials.
ARG UID=1000
ARG GID=1000

# ca-certificates: TLS to the API and Telegram. git: claude's git integration.
# ripgrep: claude's search (the bundled copy needs glibc, which we have, but the
# system one is a safe fallback). curl: only for the install step.
RUN apt-get update && apt-get install -y --no-install-recommends \
        ca-certificates curl git ripgrep \
    && rm -rf /var/lib/apt/lists/*

RUN groupadd -g ${GID} user \
    && useradd -m -u ${UID} -g ${GID} -s /bin/bash user

USER user
ENV HOME=/home/user
ENV PATH=/home/user/.local/bin:${PATH}
# The image is the unit of update; don't let claude self-update inside it.
ENV DISABLE_AUTOUPDATER=1

# Install the pinned native claude into ~/.local (matches the host's path).
RUN curl -fsSL https://claude.ai/install.sh | bash -s ${CLAUDE_VERSION} \
    && /home/user/.local/bin/claude --version

COPY --chown=${UID}:${GID} --from=builder /out/assistant /home/user/.local/bin/assistant

ENTRYPOINT ["/home/user/.local/bin/assistant", "-config", "/home/user/.config/assistant/config.yaml"]
