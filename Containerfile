# Both images, from one build of the binary:
#
#   make image         the control plane:  podman build --target control-plane .
#   make agent-image   what agents run in: podman build --target agent .
#
# The agent image runs the same binary as `keera-agents agent`, so the runtime
# always speaks the protocol of the control plane built from the same source.
#
# The agent image is a base. A deployment that wants other tools, languages or
# certificates builds its own FROM it and sets KEERA_AGENTS_IMAGE to it. Keep
# the entrypoint, the user and /workspace: the drivers depend on them. The
# drivers mount the root filesystem read-only, so only /workspace and /tmp are
# writable at run time.

FROM docker.io/library/golang:1.27.3-alpine AS build

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
