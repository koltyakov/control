FROM golang:1.27.4-bookworm AS build
WORKDIR /workspace
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN go run ./cmd/control-bundle --binary /out/control

# Full Go toolchain and C compiler for race-enabled tests, with dependencies
# downloaded during the build so the test network can stay internal.
FROM build AS tests
RUN install -m 0755 /out/control /usr/local/bin/control
CMD ["/bin/sh", "/workspace/tests/compose/run-tests.sh"]

# FFmpeg and curl are test fixtures and health-check tools, respectively.
FROM debian:bookworm-slim AS node
RUN apt-get update \
    && apt-get install -y --no-install-recommends ca-certificates curl ffmpeg \
    && rm -rf /var/lib/apt/lists/*
COPY --from=build /out/control /usr/local/bin/control
COPY tests/compose/node.sh /usr/local/bin/control-test-node
ENTRYPOINT ["control"]

# Real systemd, sudo, and an unprivileged login for the Hybrid installation test.
# The generated installer supplies Control itself.
FROM debian:bookworm-slim AS hybrid-host
RUN apt-get update \
    && apt-get install -y --no-install-recommends ca-certificates curl dbus dbus-user-session jq libpam-systemd procps socat sudo systemd systemd-sysv \
    && rm -rf /var/lib/apt/lists/* \
    && useradd --create-home --uid 1000 --shell /bin/bash fixture \
    && echo 'fixture ALL=(ALL) NOPASSWD:ALL' > /etc/sudoers.d/fixture \
    && chmod 0440 /etc/sudoers.d/fixture
STOPSIGNAL SIGRTMIN+3
ENTRYPOINT ["/lib/systemd/systemd"]
