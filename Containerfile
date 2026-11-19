# The images, from one build of the binary:
#
#   make image            the control plane:      podman build --target control-plane .
#   make agent-image      what agents run in:     podman build --target agent .
#   make workspace-image  what workspaces run in: podman build --target workspace .
#
# The agent image runs the same binary as `keera-agents agent`, and the
# workspace image as `keera-agents workspace`, so the runtime always speaks
# the protocol of the control plane built from the same source.
#
# The agent image is a base. A deployment that wants other tools, languages or
# certificates builds its own FROM it and sets KEERA_AGENTS_IMAGE to it. Keep
# the entrypoint, the user and /workspace: the drivers depend on them. The
# drivers mount the root filesystem read-only, so only /workspace and /tmp are
# writable at run time.

FROM docker.io/library/golang:1.27.4-alpine AS build

ARG VERSION=""

WORKDIR /src
COPY . .
# vendor/ is not in Git, so a build from a clone writes it here. With a vendor
# tree in the context, as in a release, the build stays offline.
RUN [ -f vendor/modules.txt ] || go mod vendor
RUN CGO_ENABLED=0 go build -mod=vendor -trimpath \
        -ldflags="-s -w -X github.com/bespinian/keera-agents/internal/version.release=${VERSION}" \
        -o /out/ ./cmd/keera-agents
RUN sh scripts/third-party-notices.sh > /out/THIRD_PARTY_NOTICES

FROM docker.io/library/alpine:3.24 AS agent

LABEL org.opencontainers.image.title="Keera Agent" \
      org.opencontainers.image.vendor="bespinian GmbH" \
      org.opencontainers.image.licenses="LicenseRef-Keera-Community-1.0" \
      org.opencontainers.image.source="https://github.com/bespinian/keera-agents"

# No locale package: musl is UTF-8 throughout. tzdata, so TZ sets the time
# the agent tells its model.
ENV LANG=C.UTF-8 \
    HOME=/workspace

# upgrade, so the image has the fixes released since the base was built.
RUN apk upgrade --no-cache \
 && apk add --no-cache \
      bash \
      ca-certificates \
      coreutils \
      curl \
      git \
      jq \
      python3 \
      ripgrep \
      tini \
      tzdata

# The agent user is uid and gid 1000, with its home on the workspace volume.
#
# Fixed, because both drivers run the container as 1000 and Kubernetes gives
# the volume that fsGroup. /workspace exists in the image, owned by the user,
# so a new podman volume is created with that owner, not root's.
RUN addgroup -g 1000 agent \
 && adduser -u 1000 -G agent -h /workspace -s /bin/bash -D -H agent \
 && mkdir -p /workspace \
 && chown 1000:1000 /workspace

# Execute-only. The kernel marks a process whose binary it may not read as not
# dumpable, so its /proc files belong to root. tini's environment holds the
# runtime token; readable, any command the agent runs could print it from
# /proc/1/environ.
COPY --from=build --chmod=0711 /out/keera-agents /usr/local/bin/keera-agents
RUN chmod 0711 /sbin/tini
COPY LICENSE /LICENSE
COPY --from=build /out/THIRD_PARTY_NOTICES /THIRD_PARTY_NOTICES

# Numeric, so Kubernetes can check runAsNonRoot against it.
USER 1000:1000
WORKDIR /workspace

# tini is PID 1: it reaps what the shell builtin's commands leave behind,
# which a Go process as PID 1 would not, and passes SIGTERM on, so a suspend
# lets the runtime report its task first.
ENTRYPOINT ["/sbin/tini", "--", "/usr/local/bin/keera-agents", "agent"]

# A workspace: a machine an engineer works in, with pi in a browser terminal
# or code-server with Cline. Debian, not Alpine: code-server is built for
# glibc. A deployment that wants other tools builds its own FROM it and sets
# KEERA_AGENTS_WORKSPACE_IMAGE; keep the entrypoint, the user and /workspace.
FROM docker.io/library/node:26.13.1-trixie-slim AS workspace

ARG TARGETARCH
ARG PI_VERSION=1.5.2
ARG CODE_SERVER_VERSION=4.145.2
ARG CLINE_VERSION=4.1.26
ARG TTYD_VERSION=1.7.7
# The downloads below are checked against these, as their releases publish
# them. Change them with the versions.
ARG TTYD_SHA256_AMD64=8a217c968aba172e0dbf3f34447218dc015bc4d5e59bf51db2f2cd12b7be4f55
ARG TTYD_SHA256_ARM64=b38acadd89d1d396a0f5649aa52c539edbad07f4bc7348b27b4f4b7219dd4165
ARG CODE_SERVER_SHA256_AMD64=7eddbdeaab34380bde3ac3bbadce58fe2f5722c4877f331ea593ae797c2517e1
ARG CODE_SERVER_SHA256_ARM64=0fa9af7f602536b1e320a17d32bb1a85e4a924fdc09283898f1bea73d2a5c1e7
ARG CLINE_SHA256=d8ea4c3505052c48e7c7a5558e7213a53827b6ba1da7e56513c87d896df51c4d

LABEL org.opencontainers.image.title="Keera Workspace" \
      org.opencontainers.image.vendor="bespinian GmbH" \
      org.opencontainers.image.licenses="LicenseRef-Keera-Community-1.0" \
      org.opencontainers.image.source="https://github.com/bespinian/keera-agents"

# HOME is the workspace volume, and so is where the owner installs things:
# the root filesystem is read-only at run time.
ENV LANG=C.UTF-8 \
    HOME=/workspace \
    NPM_CONFIG_PREFIX=/workspace/.local \
    PATH=/workspace/.local/bin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin

# fd-find and ripgrep, so pi finds them and does not download them itself at
# its first start, unpinned. Debian names fd fdfind, which pi also looks for.
RUN apt-get update \
 && apt-get upgrade -y \
 && apt-get install -y --no-install-recommends \
      bash build-essential ca-certificates curl fd-find git jq less openssh-client openssh-server \
      procps python3 python3-venv ripgrep tini tmux tzdata unzip vim-tiny \
 && case "$TARGETARCH" in \
      amd64) ttyd=x86_64 ttyd_sum=$TTYD_SHA256_AMD64 cs_sum=$CODE_SERVER_SHA256_AMD64 ;; \
      arm64) ttyd=aarch64 ttyd_sum=$TTYD_SHA256_ARM64 cs_sum=$CODE_SERVER_SHA256_ARM64 ;; \
      *) exit 1 ;; \
    esac \
 && curl -fsSL -o /usr/local/bin/ttyd \
      "https://github.com/tsl0922/ttyd/releases/download/${TTYD_VERSION}/ttyd.${ttyd}" \
 && echo "$ttyd_sum  /usr/local/bin/ttyd" | sha256sum -c - \
 && chmod 0755 /usr/local/bin/ttyd \
 && curl -fsSL -o /tmp/code-server.deb \
      "https://github.com/coder/code-server/releases/download/v${CODE_SERVER_VERSION}/code-server_${CODE_SERVER_VERSION}_${TARGETARCH}.deb" \
 && echo "$cs_sum  /tmp/code-server.deb" | sha256sum -c - \
 && apt-get install -y --no-install-recommends /tmp/code-server.deb \
 && rm -rf /tmp/code-server.deb /var/lib/apt/lists/*

# So pi tells Shift+Enter from Enter in tmux, as its docs ask for tmux 3.5.
# In /etc, not the home directory: the owner's ~/.tmux.conf still wins.
RUN printf 'set -g extended-keys on\nset -g extended-keys-format csi-u\n' > /etc/tmux.conf

# Into the image, not the home directory, which the volume hides. The
# runtime installs Cline into the workspace at the first start.
RUN NPM_CONFIG_PREFIX=/usr/local npm install -g --ignore-scripts "@earendil-works/pi-coding-agent@${PI_VERSION}" \
 && npm cache clean --force \
 && mkdir -p /opt/keera/vscode-extensions \
 && curl -fsSL -o "/opt/keera/vscode-extensions/cline-${CLINE_VERSION}.vsix" \
      "https://open-vsx.org/api/saoudrizwan/claude-dev/${CLINE_VERSION}/file/saoudrizwan.claude-dev-${CLINE_VERSION}.vsix" \
 && echo "$CLINE_SHA256  /opt/keera/vscode-extensions/cline-${CLINE_VERSION}.vsix" | sha256sum -c -

# The node image's user has uid 1000, which is the agent's. The password is
# '*', not locked: sshd refuses a locked account even for a key. sshd wants
# its privilege separation directory, though it runs as the user.
RUN userdel --remove node \
 && useradd --uid 1000 --user-group --home-dir /workspace --no-create-home --shell /bin/bash agent \
 && usermod --password '*' agent \
 && mkdir -p /workspace /run/sshd \
 && chown 1000:1000 /workspace

# Execute-only, as in the agent image: the runtime's /proc files are then
# root's, so nothing the engineer runs reads its token.
COPY --from=build --chmod=0711 /out/keera-agents /usr/local/bin/keera-agents
RUN chmod 0711 /usr/bin/tini
# THIRD_PARTY_NOTICES covers the binary. Debian's packages carry their
# licences in /usr/share/doc, and pi, code-server and Cline in their own
# directories.
COPY LICENSE /LICENSE
COPY --from=build /out/THIRD_PARTY_NOTICES /THIRD_PARTY_NOTICES

USER 1000:1000
WORKDIR /workspace

ENTRYPOINT ["/usr/bin/tini", "--", "/usr/local/bin/keera-agents", "workspace"]

# Last, so a build without --target gets the control plane.
FROM scratch AS control-plane

LABEL org.opencontainers.image.title="Keera Agents" \
      org.opencontainers.image.vendor="bespinian GmbH" \
      org.opencontainers.image.licenses="LicenseRef-Keera-Community-1.0" \
      org.opencontainers.image.source="https://github.com/bespinian/keera-agents"

COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/
COPY --from=build /out/keera-agents /keera-agents

COPY LICENSE /LICENSE
COPY --from=build /out/THIRD_PARTY_NOTICES /THIRD_PARTY_NOTICES

# nobody:nogroup. Numeric, since there is no /etc/passwd.
USER 65534:65534

EXPOSE 8070

ENTRYPOINT ["/keera-agents"]
