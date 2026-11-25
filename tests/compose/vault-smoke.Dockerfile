# Disposable two-OS-account fixture for the Control Vault system smoke test.
# Run it through tests/compose/vault-smoke.sh. It never needs a privileged
# container, host accounts, host keys, or host Control configuration.
#
# Targets:
#   runtime-built     builds the CGO-free Linux binary from this checkout
#   runtime-prebuilt  installs a Linux binary from the "prebuilt" build context

FROM golang:1.27.4-bookworm AS build
WORKDIR /workspace
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN go run ./cmd/control-bundle --binary /out/control

FROM debian:bookworm-slim AS base
# python3 drives the PTY prompts; util-linux supplies setpriv; procps aids debugging.
RUN apt-get update \
    && apt-get install -y --no-install-recommends ca-certificates procps python3 util-linux \
    && rm -rf /var/lib/apt/lists/*
# Synthetic accounts that exist only in this image. None belongs to an
# administrative group or has a password, sudo rule, or login shell need.
#   ctlgw      runs the throwaway gateway and acts as its operator
#   ctlvault   dedicated non-admin Vault broker account
#   ctldesk    ordinary desktop (frontend) account
#   ctlworker  enrolled execution worker for the throwaway fleet
#   ctlother   unrelated third account on the same host
RUN set -eu; \
    for spec in ctlgw:2001 ctlvault:2002 ctldesk:2003 ctlworker:2004 ctlother:2005; do \
        name=${spec%%:*}; id=${spec##*:}; \
        groupadd --gid "$id" "$name"; \
        useradd --uid "$id" --gid "$id" --no-user-group --create-home --shell /bin/sh "$name"; \
        chmod 0700 "/home/$name"; \
    done
COPY --chmod=0644 tests/compose/vault-smoke.py /opt/control-vault-smoke/smoke.py
ENV CONTROL_VAULT_SMOKE_CONTAINER=1
ENTRYPOINT ["python3", "-I", "/opt/control-vault-smoke/smoke.py"]

FROM base AS runtime-built
# Root-owned, administrator-controlled install location.
COPY --from=build --chown=0:0 --chmod=0755 /out/control /usr/local/bin/control

FROM base AS runtime-prebuilt
COPY --from=prebuilt --chown=0:0 --chmod=0755 control /usr/local/bin/control
