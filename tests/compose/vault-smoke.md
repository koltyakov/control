# Control Vault two-account smoke test

[Vault](../../docs/vault.md) · [Docker Compose testing](../../docs/testing.md)

This system-level check runs the real `control` binary in one disposable Linux container. It separates the broker and desktop roles with distinct OS accounts, as [Vault installation](../../docs/vault.md#install-the-broker-and-activate-the-frontend) requires. It complements the Go and integration suites; it is not part of `make test-compose`.

```sh
sh tests/compose/vault-smoke.sh                 # build the binary inside Docker (hermetic)
sh tests/compose/vault-smoke.sh --local-build   # cross-compile with the host Go toolchain (faster)
sh tests/compose/vault-smoke.sh --binary PATH   # use an existing linux/<docker arch> binary
```

`--scenario clean|migrate|all` selects the scenarios; the default is `all`. Each scenario runs in a fresh container. `--keep-image` keeps the `control-vault-smoke:local` image for reruns or debugging. `CONTROL_VAULT_SMOKE_OUT=DIR` selects the report directory, which defaults to a new temporary directory. The script prints `PASS`, `FAIL`, `WARN` and `INFO` lines, then writes `smoke-SCENARIO.log` and `report-SCENARIO.json`. It exits non-zero if a required check fails. `WARN` marks an advisory check.

## Isolation

- The container runs with `--network none`, `--cap-drop ALL`, `no-new-privileges` and pid and memory limits. It has no host mounts and is not privileged. Root keeps only the capabilities the administrator role needs: `CHOWN`, `DAC_OVERRIDE`, `FOWNER`, `SETUID`, `SETGID` and `KILL`.
- The OS accounts exist only in the image: `ctlgw` (gateway and operator), `ctlvault` (broker), `ctldesk` (desktop frontend), `ctlworker` (enrolled worker) and `ctlother` (an unrelated account). None of them belongs to an administrative group.
- The gateway, fleet account, worker and passphrases are generated for each run and are synthetic. Every known secret is redacted from the output. Host Control configuration, keys and accounts are never read.
- Host writes are limited to the temporary build directory, which is removed, and the report directory.

## Clean activation (`clean`)

The driver, `vault-smoke.py`, runs as container root and acts only as the administrator. It runs each command under its account with `setpriv --reset-env` and answers prompts on a new controlling PTY owned by that account. It types a secret only after the program has disabled terminal echo.

1. Start a loopback gateway. The operator creates a fleet account. `ctlworker` runs `control setup --service process` and comes online.
2. The administrator creates `/var/lib/control-vault`, owned by `ctlvault` with mode 0755. The binary is installed root-owned at `/usr/local/bin/control`.
3. Refusals before provisioning: no controlling terminal, a plain-HTTP non-loopback gateway, the operator key, and a mismatched passphrase. The read-only preflight must refuse these before any secret prompt, on a terminal where nobody answers: `--frontend` set to the broker itself, to root, or to an unknown account; a binary copy in a directory another account owns; and `LD_PRELOAD`. No refused attempt may leave broker state.
4. `ctlvault` runs `control vault provision --frontend ctldesk --gateway http://127.0.0.1:7330` on its PTY. The test checks the printed record against the canonical `vault.json` and checks that the record contains no secret.
5. File audit. Root scans for every plaintext copy of the account key, passphrase and operator key, excluding the gateway and worker homes. Each copy must be a private file owned by the broker. `ctldesk` and `ctlother` must be unable to read them or list the private state.
6. Broker self-checks: it refuses a binary copy in a directory the desktop owns, and refuses to start with `LD_PRELOAD` set. Then `ctlvault` starts `control vault serve`.
7. Desktop: plaintext fallback is refused before activation. A desktop-owned copy of the record is refused. `control vault enable` succeeds and `vault status` reports activation. After that, the desktop home holds no secret. Direct `control machines`, `--api`/`--token` overrides, plain `control mcp`, and `vault serve` are refused. `vault provision` is refused before any secret prompt. `control __tunnels` with a nonexistent path is refused ("vault frontend cannot start legacy Control helpers") and the path is not created.
8. `vault run`: it is refused without a terminal, with a wrong passphrase, and when the confirmation is declined. `vault run -- control machines --compact` lists the worker. `vault run -- control exec vault-worker -- id -un` runs as `ctlworker`.
9. MCP: `control mcp --vault-process` runs with no terminal. A fleet call returns `approval_required` with a code. `vault list` shows the same code, and a grant with a wrong passphrase fails. `vault grant CODE` approves that one connection. A second adapter connection is still refused. After `vault revoke ID`, the first connection is refused too.
10. Process separation: `ctldesk` cannot read the broker's `/proc/PID/environ`, `mem` or `cwd`. It cannot signal the broker, write the binary or `vault.json`, or create files in the broker root. The broker's argv and environment contain no secret.
11. Same-user and outsider checks: `ctlvault` cannot activate its own record from a second `CONTROL_HOME`. `ctlother` cannot activate a record provisioned for `ctldesk`. The file audit runs again.

## Unix migration (`migrate`)

1. Set up the same fleet. `ctldesk` uses Control in plaintext mode first. It logs in on its PTY with the same synthetic account key the broker will use. It runs `exec` on the worker, which creates its task owner, and records `ownerId` from `control session`. It starts `tunnel start vault-worker 127.0.0.1:7331 --listen 127.0.0.1:7401 --id smoke-retained --ttl 2h`; `CONTROL_TUNNEL_SERVICE_MODE=process` is set because the container has no systemd. The listener belongs to `ctldesk`.
2. The operator stops the legacy `__tunnels` service. The legacy state then has `admin.json`, `clients/*/identity.key`, `tunnels/*/service.json` and `tunnels/*/tunnels.json`. A gracefully stopped service removes its `endpoint.json`.
3. `ctlvault` provisions. Before cutover, the desktop refuses plaintext fallback, and `vault enable` without a receipt is refused against a live broker with nothing removed. `vault migrate export` refuses a non-pipe stdout.
4. With the broker stopped, the operator pipes `ctldesk`'s `vault migrate export` straight into `ctlvault`'s `vault migrate import`. The signed receipt contains no secret. It must import `admin.json`, `identity.key` and `tunnels.json`, discard `service.json` (and `endpoint.json` if present), and require no revocation. The protected `tunnels.json` must be byte-identical JSON, 0600 and owned by the broker. Desktop files stay untouched until activation.
5. The broker starts, and `vault enable --receipt` succeeds. The desktop then has no login, owner key, `tunnels.json`, `service.json` or `endpoint.json`, and holds no secret. The legacy helper is refused, and no desktop `__tunnels` process runs.
6. The broker runtime restores `127.0.0.1:7401` without any session grant, and the listener now belongs to `ctlvault`. Through `vault run`, `tunnel list` shows the original absolute expiry and `session` shows the same `ownerId`. `task get` still returns the pre-cutover task, and `machines` works. Direct CLI use stays locked. The file audit runs again.

## Dependencies and assumptions

- The test needs Docker with BuildKit, for `--build-context`. `--local-build` also needs Go matching `go.mod`.
- It relies only on the documented CLI contract. That covers the `vault` subcommands and flags (including `migrate export|import` and `enable --receipt`), the record at `/var/lib/control-vault/vault.json`, the `control_nodes` MCP tool, and plaintext `login`/`session`/`tunnel start` for the legacy setup. Prompts are matched loosely ("account key", "new/confirm passphrase", "passphrase", "type yes"). If one of these changes, update `vault-smoke.py`.
- The container has only loopback networking. Peer traffic between the broker and the worker uses whichever transport works over loopback; the test does not assert which one.
