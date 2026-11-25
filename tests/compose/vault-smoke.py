#!/usr/bin/env python3
"""Control Vault two-OS-account system smoke test.

Runs as root inside the disposable container built from
tests/compose/vault-smoke.Dockerfile, and nowhere else. Root acts only as the
host administrator: it creates directories, starts processes under the
synthetic accounts with setpriv, and plays the human at each controlling
terminal. Every Vault command runs as an unprivileged account.

All credentials are generated per run, are synthetic, and are redacted from
the output. Results are printed as PASS/FAIL/WARN/INFO lines followed by one
"REPORT {json}" line. The exit status is non-zero if a required check fails.

Usage (inside the container): smoke.py [clean|migrate]. "clean" activates a
fresh desktop; "migrate" cuts over a plaintext desktop that used the same
account login, task owner and a retained tunnel with a TTL.
"""

import errno
import fcntl
import grp
import json
import os
import pty
import pwd
import re
import secrets
import select
import signal
import socket
import subprocess
import sys
import tempfile
import termios
import time
import urllib.request

BIN = "/usr/local/bin/control"
GW_ADDR = "127.0.0.1:7330"
GATEWAY = "http://" + GW_ADDR
WORKER_ADDR = "127.0.0.1:7331"
BROKER_ADDR = "127.0.0.1:7342"
# Administrator-created broker root (owned by the broker, 0755). The broker
# publishes the canonical public record here; enable reads only this path.
LAYOUT_ROOT = "/var/lib/control-vault"
MARKER = LAYOUT_ROOT + "/vault.json"
WORKER_NAME = "vault-worker"
RUN_DIR = "/run/vault-smoke"

GW, BROKER, DESK, WORKER, OTHER = "ctlgw", "ctlvault", "ctldesk", "ctlworker", "ctlother"

SECRETS = {}
RESULTS = []
PROCS = []


# ---------------------------------------------------------------- reporting


def redact(text):
    if isinstance(text, bytes):
        text = text.decode("utf-8", "replace")
    for name, value in SECRETS.items():
        if value:
            text = text.replace(value, "<" + name + ">")
    return text


def short(text, limit=600):
    text = redact(text).strip()
    return text if len(text) <= limit else text[:limit] + " ...[truncated]"


def check(name, ok, detail="", required=True):
    level = "PASS" if ok else ("FAIL" if required else "WARN")
    RESULTS.append({"check": name, "result": level, "required": required, "detail": short(detail, 2000)})
    print(f"{level} {name}" + (f": {short(detail)}" if detail else ""), flush=True)
    return ok


def info(name, detail):
    RESULTS.append({"check": name, "result": "INFO", "required": False, "detail": short(detail, 2000)})
    print(f"INFO {name}: {short(detail)}", flush=True)


class Fatal(Exception):
    pass


def must(name, ok, detail=""):
    if not check(name, ok, detail):
        raise Fatal(name)


# --------------------------------------------------------------- processes


def account(user):
    return pwd.getpwnam(user)


def as_user(user, argv, env=None):
    """argv run as user with a clean environment from setpriv --reset-env."""
    extra = [f"{k}={v}" for k, v in (env or {}).items()]
    return ["setpriv", f"--reuid={user}", f"--regid={user}", "--init-groups", "--reset-env", "--", "env", *extra, *argv]


class Result:
    def __init__(self, rc, out, tty="", timed_out=False, unanswered=None):
        self.rc, self.out, self.tty, self.timed_out = rc, out, tty, timed_out
        self.unanswered = unanswered or []

    @property
    def text(self):
        return self.out + ("\n" + self.tty if self.tty else "")

    def summary(self):
        return f"rc={self.rc}{' timeout' if self.timed_out else ''} output={short(self.text, 400)!r}"


def run(user, argv, env=None, stdin_data=None, timeout=60):
    """Run without a controlling terminal; output goes through files so a
    daemonized grandchild cannot hold a pipe open."""
    with tempfile.TemporaryFile(dir=RUN_DIR) as out, tempfile.TemporaryFile(dir=RUN_DIR) as inp:
        if stdin_data is not None:
            inp.write(stdin_data.encode())
            inp.seek(0)
        p = subprocess.Popen(as_user(user, argv, env), stdin=inp if stdin_data is not None else subprocess.DEVNULL,
                             stdout=out, stderr=subprocess.STDOUT, start_new_session=True, cwd=account(user).pw_dir)
        timed_out = False
        try:
            rc = p.wait(timeout)
        except subprocess.TimeoutExpired:
            timed_out = True
            os.killpg(p.pid, signal.SIGKILL)
            rc = p.wait()
        out.seek(0)
        return Result(rc, out.read().decode("utf-8", "replace"), timed_out=timed_out)


def echo_disabled(fd):
    try:
        return not (termios.tcgetattr(fd)[3] & termios.ECHO)
    except termios.error:
        return False


def pty_run(user, argv, answers, env=None, timeout=120):
    """Run user's command on a new controlling terminal owned by that user,
    as a login would. answers is a list of (prompt regex, reply, secret).
    Secret replies are typed only once the program has disabled echo."""
    pw = account(user)
    master, slave = pty.openpty()
    os.fchown(slave, pw.pw_uid, grp.getgrnam("tty").gr_gid)
    os.fchmod(slave, 0o620)

    def controlling_tty():
        os.setsid()
        fcntl.ioctl(0, termios.TIOCSCTTY, 0)

    p = subprocess.Popen(as_user(user, argv, env), stdin=slave, stdout=subprocess.PIPE, stderr=slave,
                         preexec_fn=controlling_tty, cwd=pw.pw_dir)
    os.close(slave)
    tty, out, pos = b"", b"", 0
    pending = list(answers)
    fds = [master, p.stdout.fileno()]
    deadline = time.monotonic() + timeout
    timed_out = False
    echoed_secret = False
    while fds:
        if time.monotonic() > deadline:
            timed_out = True
            os.killpg(p.pid, signal.SIGKILL)
            break
        ready, _, _ = select.select(fds, [], [], 0.2)
        for fd in ready:
            try:
                data = os.read(fd, 65536)
            except OSError as e:
                if e.errno != errno.EIO:
                    raise
                data = b""
            if not data:
                fds.remove(fd)
            elif fd == master:
                tty += data
            else:
                out += data
        while pending and master in fds:
            m = re.search(pending[0][0], tty[pos:].decode("utf-8", "replace"))
            if not m:
                break
            prompt, reply, secret = pending[0]
            if secret:
                wait_until = time.monotonic() + 5
                while not echo_disabled(master) and time.monotonic() < wait_until:
                    time.sleep(0.02)
                if not echo_disabled(master):
                    echoed_secret = True
                    break
            pos += len(tty[pos:].decode("utf-8", "replace")[: m.end()].encode())
            os.write(master, reply.encode() + b"\n")
            pending.pop(0)
        if echoed_secret:
            os.killpg(p.pid, signal.SIGKILL)
            break
    try:
        rc = p.wait(10)
    except subprocess.TimeoutExpired:
        os.killpg(p.pid, signal.SIGKILL)
        rc = p.wait()
    os.close(master)
    p.stdout.close()
    if echoed_secret:
        tty += b"\n[smoke: refused to type a secret while terminal echo was enabled]"
    return Result(rc, out.decode("utf-8", "replace"), tty.decode("utf-8", "replace"), timed_out,
                  [a[0] for a in pending])


def background(user, argv, env=None, log=None):
    logf = open(log, "ab") if log else subprocess.DEVNULL
    p = subprocess.Popen(as_user(user, argv, env), stdin=subprocess.DEVNULL, stdout=logf, stderr=subprocess.STDOUT,
                         start_new_session=True, cwd=account(user).pw_dir)
    PROCS.append(p)
    return p


def wait_tcp(addr, proc=None, timeout=30):
    host, port = addr.rsplit(":", 1)
    deadline = time.monotonic() + timeout
    while time.monotonic() < deadline:
        if proc is not None and proc.poll() is not None:
            return False
        try:
            socket.create_connection((host, int(port)), 0.5).close()
            return True
        except OSError:
            time.sleep(0.2)
    return False


def read_log(path):
    try:
        with open(path, "rb") as f:
            return f.read().decode("utf-8", "replace")
    except OSError:
        return ""


def parse_json(text):
    """The first JSON value in text, tolerating leading diagnostic lines."""
    dec = json.JSONDecoder()
    for i, ch in enumerate(text):
        if ch in "[{":
            try:
                return dec.raw_decode(text[i:])[0]
            except ValueError:
                continue
    return None


# Prompts are matched loosely so minor wording changes do not break the smoke.
P_ACCOUNT = r"(?i)account key[^\n]*:\s*$"
P_NEW = r"(?i)new[^\n]*passphrase[^\n]*:\s*$"
P_CONFIRM = r"(?i)confirm[^\n]*passphrase[^\n]*:\s*$"
P_PASS = r"(?i)passphrase[^\n]*:\s*$"
P_YES = r"(?i)type yes[^\n]*:\s*$"
P_ANY_SECRET = r"(?i)(account key|passphrase)[^\n]*:"


def approve(passphrase, confirm="yes"):
    return [(P_PASS, passphrase, True), (P_YES, confirm, False)]


# ------------------------------------------------------------------ probes

PROBE = r"""
import json, os, sys
out = {}
for spec in sys.argv[1:]:
    op, _, path = spec.partition(":")
    try:
        if op == "read":
            with open(path, "rb") as f: f.read(1)
        elif op == "list":
            os.listdir(path)
        elif op == "write":
            with open(path, "ab"): pass
        elif op == "create":
            fd = os.open(path, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600); os.close(fd); os.unlink(path)
        elif op == "readlink":
            os.readlink(path)
        elif op == "kill":
            os.kill(int(path), 0)
        out[spec] = "allowed"
    except PermissionError as e:
        out[spec] = "denied:" + (e.strerror or "")
    except OSError as e:
        out[spec] = "error:" + (e.strerror or str(e))
print(json.dumps(out))
"""


def probe(user, specs):
    r = run(user, ["python3", "-I", "-c", PROBE, *specs])
    return parse_json(r.out) or {"_error": r.summary()}


def files_containing(needles, roots, skip):
    """Root-side audit: regular files containing any needle value."""
    hits = []
    for root in roots:
        for dirpath, dirnames, filenames in os.walk(root):
            dirnames[:] = [d for d in dirnames if os.path.join(dirpath, d) not in skip]
            for name in filenames:
                path = os.path.join(dirpath, name)
                try:
                    st = os.lstat(path)
                    if not (st.st_mode & 0o170000 == 0o100000) or st.st_size > 16 << 20:
                        continue
                    with open(path, "rb") as f:
                        data = f.read()
                except OSError:
                    continue
                for label, value in needles.items():
                    if value and value.encode() in data:
                        hits.append({"path": path, "secret": label, "owner": pwd.getpwuid(st.st_uid).pw_name,
                                     "mode": oct(st.st_mode & 0o7777)})
    return hits


def tree(path):
    rows = []
    for dirpath, dirnames, filenames in os.walk(path):
        for name in sorted(dirnames + filenames):
            p = os.path.join(dirpath, name)
            try:
                st = os.lstat(p)
            except OSError:
                continue
            rows.append(f"{oct(st.st_mode & 0o7777)} {pwd.getpwuid(st.st_uid).pw_name} {p}")
    return rows


# -------------------------------------------------------------------- MCP


class MCP:
    """A minimal stdio MCP client for control mcp --vault-process. The
    adapter runs without a controlling terminal, like an editor-hosted agent."""

    def __init__(self, user, argv=(BIN, "mcp", "--vault-process"), env=None):
        self.log = os.path.join(RUN_DIR, f"mcp-{len(PROCS)}.log")
        self.p = subprocess.Popen(as_user(user, list(argv), env), stdin=subprocess.PIPE,
                                  stdout=subprocess.PIPE, stderr=open(self.log, "ab"), start_new_session=True,
                                  cwd=account(user).pw_dir)
        PROCS.append(self.p)
        self.next = 0

    def request(self, method, params, timeout=90):
        self.next += 1
        msg = {"jsonrpc": "2.0", "id": self.next, "method": method, "params": params}
        self.p.stdin.write((json.dumps(msg) + "\n").encode())
        self.p.stdin.flush()
        deadline = time.monotonic() + timeout
        buf = b""
        fd = self.p.stdout.fileno()
        while time.monotonic() < deadline:
            ready, _, _ = select.select([fd], [], [], 0.5)
            if not ready:
                if self.p.poll() is not None:
                    break
                continue
            chunk = os.read(fd, 1 << 20)
            if not chunk:
                break
            buf += chunk
            while b"\n" in buf:
                line, buf = buf.split(b"\n", 1)
                try:
                    reply = json.loads(line)
                except ValueError:
                    continue
                if reply.get("id") == self.next:
                    return reply
        return {"error": {"message": "no reply; adapter log: " + read_log(self.log)[-500:]}}

    def notify(self, method):
        self.p.stdin.write((json.dumps({"jsonrpc": "2.0", "method": method}) + "\n").encode())
        self.p.stdin.flush()

    def start(self):
        r = self.request("initialize", {"protocolVersion": "2025-06-18", "capabilities": {},
                                        "clientInfo": {"name": "vault-smoke", "version": "0"}})
        self.notify("notifications/initialized")
        return r

    def call(self, tool, args):
        r = self.request("tools/call", {"name": tool, "arguments": args})
        if "error" in r:
            return False, json.dumps(r["error"])
        result = r.get("result", {})
        text = "\n".join(c.get("text", "") for c in result.get("content", []) if isinstance(c, dict))
        if result.get("structuredContent") is not None:
            text += "\n" + json.dumps(result["structuredContent"])
        return not result.get("isError", False), text

    def close(self):
        try:
            self.p.stdin.close()
            self.p.wait(5)
        except Exception:
            os.killpg(self.p.pid, signal.SIGKILL)


def refused(r):
    """A deliberate refusal, not a missing command or a harness timeout."""
    return (r.rc != 0 and not r.timed_out
            and re.search(r'(?i)unknown (vault )?command|flag provided but not defined', r.text) is None)


def refused_for_approval(ok, text):
    return not ok and re.search(r"(?i)approval|not_sent|grant", text) is not None and WORKER_NAME not in text


# --------------------------------------------------------------- scenario


def setup_fleet():
    os.makedirs(RUN_DIR, mode=0o700, exist_ok=True)
    os.chmod(RUN_DIR, 0o700)
    st = os.stat(BIN)
    must("binary is root-owned and not writable by other accounts",
         st.st_uid == 0 and st.st_mode & 0o022 == 0, f"{BIN} {oct(st.st_mode & 0o7777)} uid={st.st_uid}")
    r = run(DESK, [BIN, "version"])
    info("control version", r.out)

    SECRETS["superuser-key"] = "smoke-only-superuser-" + secrets.token_hex(24)
    SECRETS["legacy-token"] = "smoke-only-token-" + secrets.token_hex(24)
    gw_state = account(GW).pw_dir + "/gateway"
    gw_log = os.path.join(RUN_DIR, "gateway.log")
    gw = background(GW, [BIN, "gateway", "--listen", GW_ADDR, "--data", gw_state],
                    env={"CONTROL_SUPERUSER_KEY": SECRETS["superuser-key"], "CONTROL_TOKEN": SECRETS["legacy-token"],
                         "CONTROL_RELEASE_REPO": ""}, log=gw_log)
    ok = False
    for _ in range(150):
        if gw.poll() is not None:
            break
        try:
            ok = urllib.request.urlopen(GATEWAY + "/healthz", timeout=1).status == 200
            if ok:
                break
        except OSError:
            time.sleep(0.2)
    must("throwaway gateway is healthy on loopback", ok, read_log(gw_log)[-800:])

    r = run(GW, [BIN, "users", "create", "vault-smoke"],
            env={"CONTROL_GATEWAY": GATEWAY, "CONTROL_SUPERUSER_KEY": SECRETS["superuser-key"]})
    record = parse_json(r.out) or {}
    key = next((v for k, v in record.items() if k.lower() == "token" and isinstance(v, str)), "")
    SECRETS["account-key"] = key
    must("operator provisions a synthetic fleet account", r.rc == 0 and bool(key), r.summary())

    worker_home = account(WORKER).pw_dir
    worker_env = {"CONTROL_HOME": worker_home + "/.control", "CONTROL_INSTALL_DIR": worker_home + "/bin",
                  "CONTROL_SERVICE_MODE": "process"}
    r = run(WORKER, [BIN, "setup", "--gateway", GATEWAY, "--name", WORKER_NAME, "--listen", WORKER_ADDR,
                     "--service", "process"], env={**worker_env, "CONTROL_USER_KEY": key}, timeout=180)
    must("worker account enrolls an execution node", r.rc == 0, r.summary())
    online = False
    last = None
    for _ in range(60):
        last = run(WORKER, [worker_home + "/bin/control", "machines", "--compact"], env=worker_env)
        data = parse_json(last.out)
        if isinstance(data, list) and any(n.get("name") == WORKER_NAME and n.get("online") for n in data):
            online = True
            break
        time.sleep(1)
    must("worker is online in the fleet", online, last.summary() if last else "")

    # Administrator step: the broker-owned root. Ordinary accounts can read,
    # not modify, it.
    b = account(BROKER)
    os.makedirs(LAYOUT_ROOT, exist_ok=True)
    os.chmod(LAYOUT_ROOT, 0o755)
    os.chown(LAYOUT_ROOT, b.pw_uid, b.pw_gid)
    SECRETS["passphrase"] = "smoke-pass-" + secrets.token_urlsafe(18)
    SECRETS["wrong-passphrase"] = "smoke-wrong-" + secrets.token_urlsafe(18)


def provision_refusals():
    before = tree(account(BROKER).pw_dir) + tree(LAYOUT_ROOT)
    r = run(BROKER, [BIN, "vault", "provision", "--frontend", DESK, "--gateway", GATEWAY, "--listen", BROKER_ADDR],
            stdin_data=SECRETS["account-key"] + "\n" + SECRETS["passphrase"] + "\n" + SECRETS["passphrase"] + "\n")
    check("provision refuses secrets without a controlling terminal",
          refused(r) and re.search(r"(?i)terminal", r.text) is not None, r.summary())
    # On a terminal with nobody answering: a prompt before the refusal times out.
    r = pty_run(BROKER, [BIN, "vault", "provision", "--frontend", DESK, "--gateway", "http://203.0.113.10:7330", "--listen", BROKER_ADDR],
                [], timeout=15)
    check("provision refuses a plain-HTTP non-loopback gateway before prompting", refused(r), r.summary())
    r = pty_run(BROKER, [BIN, "vault", "provision", "--frontend", DESK, "--gateway", GATEWAY, "--listen", BROKER_ADDR],
                [(P_ACCOUNT, SECRETS["superuser-key"], True)])
    check("provision refuses the operator (superuser) key", refused(r) and not parse_json(r.out), r.summary())
    r = pty_run(BROKER, [BIN, "vault", "provision", "--frontend", DESK, "--gateway", GATEWAY, "--listen", BROKER_ADDR],
                [(P_ACCOUNT, SECRETS["account-key"], True), (P_NEW, SECRETS["passphrase"], True),
                 (P_CONFIRM, SECRETS["wrong-passphrase"], True)])
    check("provision refuses a mismatched passphrase confirmation", refused(r) and not parse_json(r.out), r.summary())

    # The read-only service-account preflight runs before any secret prompt.
    # Nobody answers on these terminals: a prompt times out and fails.
    early = [("the broker account itself (same OS user) as the frontend", BIN, ["--frontend", BROKER], None),
             ("root as the frontend", BIN, ["--frontend", "root"], None),
             ("a nonexistent frontend account", BIN, ["--frontend", "ctl-no-such-user"], None),
             ("an unrelated account's binary copy", "/tmp/broker-untrusted/control", ["--frontend", DESK], None),
             ("LD_PRELOAD in the broker environment", BIN, ["--frontend", DESK], {"LD_PRELOAD": ""})]
    run(DESK, ["sh", "-c", f"mkdir -p /tmp/broker-untrusted && cp {BIN} /tmp/broker-untrusted/control && "
                           "chmod 0755 /tmp/broker-untrusted /tmp/broker-untrusted/control"])
    for why, binary, flags, env in early:
        r = pty_run(BROKER, [binary, "vault", "provision", *flags, "--gateway", GATEWAY, "--listen", BROKER_ADDR],
                    [], env=env, timeout=15)
        check(f"provision refuses {why} before any secret prompt",
              refused(r) and re.search(P_ANY_SECRET, r.tty) is None, r.summary())
    after = tree(account(BROKER).pw_dir) + tree(LAYOUT_ROOT)
    check("refused provisioning attempts and preflight leave no broker state", before == after,
          "new entries: " + ", ".join(sorted(set(after) - set(before))))


def provision():
    r = pty_run(BROKER, [BIN, "vault", "provision", "--frontend", DESK, "--gateway", GATEWAY, "--listen", BROKER_ADDR],
                [(P_ACCOUNT, SECRETS["account-key"], True), (P_NEW, SECRETS["passphrase"], True),
                 (P_CONFIRM, SECRETS["passphrase"], True)])
    marker = parse_json(r.out)
    must("broker account provisions Vault over its controlling PTY", r.rc == 0 and isinstance(marker, dict),
         r.summary())
    check("all secret prompts were answered with terminal echo disabled",
          not r.unanswered and "[smoke: refused" not in r.tty, f"unanswered={r.unanswered}")
    check("secrets never appear on the provisioning terminal or stdout",
          not any(v in r.text for v in SECRETS.values() if v), "")
    text = json.dumps(marker)
    check("public provisioning record contains no secret", not any(v in text for v in SECRETS.values() if v),
          "fields: " + ", ".join(sorted(marker)))
    info("public provisioning record", json.dumps({k: (v if k in ("address", "pin", "gateway", "broker", "frontend",
                                                                    "version") else "...") for k, v in marker.items()}))
    # The broker publishes the canonical record; ordinary accounts can only read it.
    try:
        with open(MARKER) as f:
            published = json.load(f)
        st = os.stat(MARKER)
    except (OSError, ValueError) as e:
        published, st = None, None
        info("canonical record", repr(e))
    must("broker publishes the canonical public record",
         published == marker and st is not None and st.st_uid == account(BROKER).pw_uid and not st.st_mode & 0o022,
         f"{MARKER} " + (oct(st.st_mode & 0o7777) if st else "missing"))
    check("public record names the broker and frontend accounts",
          marker.get("broker", {}).get("name") == BROKER and marker.get("frontend", {}).get("name") == DESK,
          json.dumps({"broker": marker.get("broker"), "frontend": marker.get("frontend")}))
    info("broker state after provisioning", "\n".join(tree(account(BROKER).pw_dir) + tree(LAYOUT_ROOT)))
    return marker


def legacy_helper_refused():
    """The legacy tunnel-service helper must not start from a Vault frontend,
    so it can never load old credentials. The path does not exist and must
    not be created."""
    path = account(DESK).pw_dir + "/smoke-nonexistent-tunnels"
    r = run(DESK, [BIN, "__tunnels", path], timeout=15)
    check("activated desktop cannot start the legacy tunnel helper",
          refused(r) and re.search(r"(?i)legacy Control helpers", r.text) is not None and not os.path.lexists(path),
          r.summary())


def file_separation(stage):
    skip = {"/proc", "/sys", "/dev", RUN_DIR, account(WORKER).pw_dir, account(GW).pw_dir}
    hits = files_containing({"account-key": SECRETS["account-key"], "passphrase": SECRETS["passphrase"],
                             "superuser-key": SECRETS["superuser-key"]},
                            ["/home", "/var", "/srv", "/tmp", "/etc", "/root", "/opt", "/usr/local", "/run"], skip)
    info(f"{stage}: files containing secrets (gateway and worker homes excluded)",
         json.dumps(hits) if hits else "none")
    check(f"{stage}: approval passphrase is not stored anywhere", not any(h["secret"] == "passphrase" for h in hits),
          json.dumps(hits))
    bad = [h for h in hits if h["owner"] != BROKER or int(h["mode"], 8) & 0o077]
    check(f"{stage}: every stored secret copy is a private broker-owned file", not bad, json.dumps(bad))
    held = any(h["secret"] == "account-key" for h in hits)
    check(f"{stage}: the account key is held by the broker account", held,
          "" if held else "no plaintext copy found; acceptable if encrypted at rest", required=False)
    specs = [f"read:{h['path']}" for h in hits] + [f"list:{account(BROKER).pw_dir}"]
    if os.path.isdir(LAYOUT_ROOT + "/private"):
        specs.append(f"list:{LAYOUT_ROOT}/private")
    result = probe(DESK, specs)
    check(f"{stage}: desktop account cannot read broker secrets or private state",
          all(v.startswith("denied") for v in result.values()), json.dumps(result))
    result = probe(OTHER, specs)
    check(f"{stage}: unrelated account cannot read broker secrets or private state",
          all(v.startswith("denied") for v in result.values()), json.dumps(result))


def broker_trust_refusals():
    # The desktop account can copy the binary, but the broker must not run
    # protected state from a location the desktop account controls.
    r = run(DESK, ["sh", "-c", f"mkdir -p /tmp/desk-bin && cp {BIN} /tmp/desk-bin/control && chmod 0755 /tmp/desk-bin "
                               "/tmp/desk-bin/control"])
    if r.rc == 0:
        r = run(BROKER, ["/tmp/desk-bin/control", "vault", "serve"], timeout=10)
        check("broker refuses to serve from a desktop-writable binary", refused(r), r.summary(),
              required=False)
    r = run(BROKER, [BIN, "vault", "serve"], env={"LD_PRELOAD": ""}, timeout=10)
    check("broker refuses to serve with LD_PRELOAD set", refused(r), r.summary(), required=False)


def start_broker():
    log = os.path.join(RUN_DIR, "broker.log")
    p = background(BROKER, [BIN, "vault", "serve"], log=log)
    must("broker serves on its loopback address", wait_tcp(BROKER_ADDR, p), read_log(log)[-800:])
    return p, log


def frontend(marker, broker_proc, broker_log):
    desk_home = account(DESK).pw_dir
    r = run(DESK, [BIN, "machines"])
    check("provisioned but unactivated desktop refuses plaintext fallback",
          refused(r) and re.search(r"(?i)vault|activation", r.text) is not None, r.summary())

    # A copy the desktop account controls is never trusted, even unmodified.
    w = run(DESK, ["sh", "-c", f"umask 077 && cp {MARKER} copy.json"])
    r = run(DESK, [BIN, "vault", "enable", desk_home + "/copy.json"])
    check("enable refuses a record copy outside the broker-owned location", w.rc == 0 and refused(r), r.summary())

    r = run(DESK, [BIN, "vault", "enable"])
    must("desktop account activates from the broker's public record", r.rc == 0, r.summary())
    r = run(DESK, [BIN, "vault", "enable", MARKER])
    info("repeat enable naming the canonical path", r.summary())
    r = run(DESK, [BIN, "vault", "status"])
    check("vault status reports activation", r.rc == 0 and re.search(r"(?i)enabled", r.out) is not None
          and BROKER_ADDR in r.out, r.summary())
    hits = files_containing({k: v for k, v in SECRETS.items() if k != "wrong-passphrase"}, [desk_home], set())
    check("desktop home holds no account key, passphrase, or operator key", not hits, json.dumps(hits))
    info("desktop home after activation", "\n".join(tree(desk_home)))

    r = run(DESK, [BIN, "machines"])
    check("ordinary CLI is locked for the desktop process",
          refused(r) and WORKER_NAME not in r.out and re.search(r"(?i)locked|vault", r.text) is not None, r.summary())
    r = run(DESK, [BIN, "--api", "http://" + WORKER_ADDR, "--token", "guess", "machines"])
    check("--api/--token cannot bypass Vault routing", refused(r) and WORKER_NAME not in r.out, r.summary())
    r = run(DESK, [BIN, "mcp"], timeout=10)
    check("plain control mcp is refused in Vault mode", refused(r), r.summary())
    # Never type an account key into a desktop process; nobody answers here.
    r = pty_run(DESK, [BIN, "vault", "provision", "--frontend", OTHER, "--gateway", GATEWAY, "--listen",
                       "127.0.0.1:7343"], [], timeout=15)
    with open(MARKER) as f:
        unchanged = json.load(f) == marker
    check("activated desktop provision is refused before any secret prompt", refused(r) and unchanged, r.summary())
    legacy_helper_refused()
    r = run(DESK, [BIN, "vault", "serve"], timeout=10)
    check("activated desktop cannot serve the broker", refused(r), r.summary())

    # CLI approval: vault run -- control COMMAND.
    r = run(DESK, [BIN, "vault", "run", "--", "control", "machines", "--compact"], timeout=30)
    check("vault run refuses approval without a controlling terminal", refused(r) and WORKER_NAME not in r.out,
          r.summary())
    r = pty_run(DESK, [BIN, "vault", "run", "--", "control", "machines", "--compact"],
                approve(SECRETS["wrong-passphrase"]))
    check("vault run with a wrong passphrase issues no command", refused(r) and WORKER_NAME not in r.out, r.summary())
    r = pty_run(DESK, [BIN, "vault", "run", "--", "control", "machines", "--compact"],
                approve(SECRETS["passphrase"], "no"))
    check("vault run declined at the confirmation issues no command", refused(r) and WORKER_NAME not in r.out,
          r.summary())
    r = pty_run(DESK, [BIN, "vault", "run", "--", "control", "machines", "--compact"], approve(SECRETS["passphrase"]))
    data = parse_json(r.out)
    check("vault run -- control machines lists the fleet through the broker",
          r.rc == 0 and isinstance(data, list) and any(n.get("name") == WORKER_NAME for n in data), r.summary())
    info("vault run terminal transcript", r.tty)
    r = pty_run(DESK, [BIN, "vault", "run", "--", "control", "exec", WORKER_NAME, "--timeout", "60s", "--", "id", "-un"],
                approve(SECRETS["passphrase"]), timeout=180)
    check("vault run -- control exec runs on the worker as its own account",
          r.rc == 0 and WORKER in r.out and DESK not in r.out, r.summary())

    # Agent approval: an MCP adapter with no terminal gets a request code that
    # the operator approves from a separate terminal.
    m1 = MCP(DESK)
    init = m1.start()
    check("MCP --vault-process adapter initializes without a broker grant", "result" in init, json.dumps(init)[:400])
    tools = m1.request("tools/list", {})
    names = [t.get("name") for t in tools.get("result", {}).get("tools", [])]
    check("MCP tool catalog is listed without approval", "control_nodes" in names, ", ".join(map(str, names))[:400])
    ok, text = m1.call("control_nodes", {"compact": True})
    check("first MCP fleet call is refused pending approval", refused_for_approval(ok, text), text)
    codes = set(re.findall(r"code ([A-Z0-9]{4}(?:-[A-Z0-9]{4})*)", text))

    r = pty_run(DESK, [BIN, "vault", "list"], [(P_PASS, SECRETS["passphrase"], True)])
    entries = parse_json(r.out)
    pending = [e for e in (entries or []) if isinstance(e, dict) and e.get("code")]
    check("vault list shows the pending MCP request", r.rc == 0 and bool(pending), r.summary())
    info("pending entries", json.dumps(entries))
    code = next((e["code"] for e in pending if e["code"] in codes), pending[0]["code"] if pending else "")
    check("MCP refusal carries the same request code as vault list", code in codes, f"code={code} seen={sorted(codes)}",
          required=False)

    r = pty_run(DESK, [BIN, "vault", "grant", code or "NONE"], approve(SECRETS["wrong-passphrase"]))
    check("vault grant with a wrong passphrase is refused", refused(r), r.summary())
    ok, text = m1.call("control_nodes", {"compact": True})
    check("MCP remains refused after a failed grant", refused_for_approval(ok, text), text)

    r = pty_run(DESK, [BIN, "vault", "grant", "--ttl", "10m", code or "NONE"], approve(SECRETS["passphrase"]))
    entry = parse_json(r.out) or {}
    check("vault grant CODE approves the waiting MCP connection", r.rc == 0 and bool(entry.get("id")), r.summary())
    ok, text = m1.call("control_nodes", {"compact": True})
    check("granted MCP connection lists the fleet", ok and WORKER_NAME in text, text)

    m2 = MCP(DESK)
    m2.start()
    ok, text = m2.call("control_nodes", {"compact": True})
    check("a second MCP connection does not inherit the grant", refused_for_approval(ok, text), text)

    r = pty_run(DESK, [BIN, "vault", "revoke", entry.get("id", "NONE")], [(P_PASS, SECRETS["passphrase"], True)])
    check("vault revoke GRANT_ID succeeds", r.rc == 0, r.summary())
    ok, text = m1.call("control_nodes", {"compact": True})
    check("revoked MCP connection can no longer issue fleet calls", not ok and WORKER_NAME not in text, text)
    m1.close()
    m2.close()

    # Process separation between the desktop and the broker.
    pid = broker_proc.pid
    with open(f"/proc/{pid}/status") as f:
        uid_line = next((l for l in f if l.startswith("Uid:")), "")
    check("broker process runs as the dedicated account",
          uid_line.split()[1:2] == [str(account(BROKER).pw_uid)], uid_line.strip())
    # Root here lacks CAP_SYS_PTRACE, so read the environment as the broker.
    environ = run(BROKER, ["cat", f"/proc/{pid}/environ"]).out
    with open(f"/proc/{pid}/cmdline", "rb") as f:
        cmdline = f.read().decode("utf-8", "replace")
    check("broker argv and environment carry no secret",
          not any(v in environ + cmdline for v in SECRETS.values() if v), cmdline.replace("\0", " "))
    result = probe(DESK, [f"read:/proc/{pid}/environ", f"read:/proc/{pid}/mem", f"readlink:/proc/{pid}/cwd",
                          f"kill:{pid}", f"write:{BIN}", f"write:{MARKER}",
                          f"create:{LAYOUT_ROOT}/planted"])
    check("desktop cannot inspect, signal, or replace the broker, its binary, or its record",
          all(v.startswith("denied") for v in result.values()), json.dumps(result))


def same_user_and_outsider():
    # Two Control homes under the same OS login are not isolated: enabling
    # the broker's own record from another home of that login must fail.
    alt = account(BROKER).pw_dir + "/same-login-frontend"
    run(BROKER, ["sh", "-c", f"umask 077 && mkdir -p {alt}"])
    r = run(BROKER, [BIN, "vault", "enable", MARKER], env={"CONTROL_HOME": alt})
    s = run(BROKER, [BIN, "vault", "status"], env={"CONTROL_HOME": alt})
    check("broker login cannot activate as its own frontend (same OS user)",
          refused(r) and re.search(r"(?i)vault enabled", s.out) is None, r.summary() + " | status: " + s.summary())

    r = run(OTHER, [BIN, "vault", "enable"])
    s = run(OTHER, [BIN, "vault", "status"])
    check("unrelated account cannot activate a record provisioned for the desktop",
          refused(r) and WORKER_NAME not in s.out and re.search(r"(?i)enabled;", s.out) is None,
          r.summary() + " | status: " + s.summary())
    if r.rc == 0:
        p = pty_run(OTHER, [BIN, "vault", "run", "--", "control", "machines", "--compact"],
                    approve(SECRETS["wrong-passphrase"]))
        check("unrelated account without the passphrase cannot run fleet commands",
              refused(p) and WORKER_NAME not in p.out, p.summary())
        p = pty_run(OTHER, [BIN, "vault", "list"], [(P_PASS, SECRETS["wrong-passphrase"], True)])
        check("unrelated account without the passphrase cannot list grants", refused(p), p.summary())


# ------------------------------------------------------- migration scenario

TUNNEL_ID = "smoke-retained"
TUNNEL_LISTEN = "127.0.0.1:7401"


def desk_control_dir():
    return account(DESK).pw_dir + "/.config/control"


def legacy_files():
    """Relative paths of the plaintext items a migration covers."""
    base = desk_control_dir()
    found = []
    for dirpath, _, filenames in os.walk(base):
        for name in filenames:
            rel = os.path.relpath(os.path.join(dirpath, name), base)
            if rel in ("admin.json", "update-admin.json") or name in ("identity.key", "service.json",
                                                                      "tunnels.json", "endpoint.json"):
                found.append(rel)
    return sorted(found)


def read_legacy_tunnels():
    for rel in legacy_files():
        if rel.endswith("/tunnels.json"):
            with open(os.path.join(desk_control_dir(), rel)) as f:
                return rel, json.load(f)
    return None, None


def find_entry(data, key, value):
    """Recursively find a dict whose key equals value."""
    if isinstance(data, dict):
        if data.get(key) == value:
            return data
        data = list(data.values())
    if isinstance(data, list):
        for item in data:
            found = find_entry(item, key, value)
            if found is not None:
                return found
    return None


def find_key(data, key):
    if isinstance(data, dict):
        if key in data:
            return data[key]
        data = list(data.values())
    if isinstance(data, list):
        for item in data:
            found = find_key(item, key)
            if found is not None:
                return found
    return None


def same_instant(a, b):
    def norm(t):
        m = re.match(r"(\d{4}-\d\d-\d\dT\d\d:\d\d:\d\d)(?:\.(\d+))?(Z|[+-]\d\d:\d\d)$", t or "")
        return (m.group(1), (m.group(2) or "").rstrip("0"), m.group(3)) if m else None
    return norm(a) is not None and norm(a) == norm(b)


def listener_uid(addr):
    host, port = addr.rsplit(":", 1)
    want = "%08X:%04X" % (int.from_bytes(socket.inet_aton(host), "little"), int(port))
    with open("/proc/net/tcp") as f:
        for line in f.readlines()[1:]:
            cols = line.split()
            if cols[1] == want and cols[3] == "0A":
                return int(cols[7])
    return None


def tunnel_helpers(user):
    uid = account(user).pw_uid
    pids = []
    for entry in os.listdir("/proc"):
        if not entry.isdigit():
            continue
        try:
            if os.stat(f"/proc/{entry}").st_uid != uid:
                continue
            with open(f"/proc/{entry}/cmdline", "rb") as f:
                argv = f.read().split(b"\0")
        except OSError:
            continue
        if b"__tunnels" in argv:
            pids.append(int(entry))
    return pids


def legacy_desktop():
    """The everyday account uses Control in plaintext mode before Vault."""
    r = pty_run(DESK, [BIN, "login", "--gateway", GATEWAY], [(P_ACCOUNT.replace("account", "api"),
                                                                SECRETS["account-key"], True)])
    must("legacy desktop logs in with the same synthetic account key", r.rc == 0, r.summary())
    r = run(DESK, [BIN, "exec", WORKER_NAME, "--timeout", "60s", "--", "id", "-un"], timeout=120)
    task = parse_json(r.out) or {}
    must("legacy desktop runs a task under its task owner", r.rc == 0 and bool(task.get("id")), r.summary())
    r = run(DESK, [BIN, "session"])
    owner = find_key(parse_json(r.out), "ownerId")
    must("legacy desktop reports a stable task owner", r.rc == 0 and bool(owner), r.summary())
    r = run(DESK, [BIN, "tunnel", "start", WORKER_NAME, WORKER_ADDR, "--listen", TUNNEL_LISTEN, "--id", TUNNEL_ID,
                   "--ttl", "2h"], env={"CONTROL_TUNNEL_SERVICE_MODE": "process"}, timeout=60)
    must("legacy desktop starts a retained tunnel with a TTL", r.rc == 0, r.summary())
    check("legacy retained tunnel listens", wait_tcp(TUNNEL_LISTEN, timeout=30), "")
    check("legacy tunnel listener belongs to the desktop account", listener_uid(TUNNEL_LISTEN) == account(DESK).pw_uid,
          f"uid={listener_uid(TUNNEL_LISTEN)}")
    rel, tunnels = read_legacy_tunnels()
    entry = find_entry(tunnels, "id", TUNNEL_ID) or {}
    must("legacy tunnel definition records an absolute expiry", bool(entry.get("expires")), json.dumps(tunnels))

    # Operator stops the legacy tunnel service before the cutover.
    pids = tunnel_helpers(DESK)
    for pid in pids:
        os.kill(pid, signal.SIGTERM)
    deadline = time.monotonic() + 20
    while tunnel_helpers(DESK) and time.monotonic() < deadline:
        time.sleep(0.2)
    stopped = not tunnel_helpers(DESK) and listener_uid(TUNNEL_LISTEN) is None
    must("legacy tunnel service is stopped", bool(pids) and stopped, f"pids={pids}")
    files = legacy_files()
    info("legacy plaintext state", "\n".join(tree(desk_control_dir())))
    must("legacy state has a login, task owner, retained definitions and tunnel-service login",
         "admin.json" in files and any(f.endswith("identity.key") for f in files)
         and any(f.endswith("tunnels.json") for f in files) and any(f.endswith("service.json") for f in files),
         ", ".join(files))
    info("legacy endpoint.json after the service stopped",
         "present" if any(f.endswith("endpoint.json") for f in files) else "removed by the stopped service")
    return {"task": task["id"], "owner": owner, "tunnel": entry, "tunnels_rel": rel, "tunnels": tunnels,
            "files": files}


def migrate_cutover(legacy):
    r = run(DESK, [BIN, "machines"])
    check("provisioned legacy desktop refuses plaintext fallback before cutover",
          refused(r) and WORKER_NAME not in r.out and re.search(r"(?i)vault|activation", r.text) is not None,
          r.summary())
    # Against a live broker, so the refusal is about the plaintext state.
    broker, _ = start_broker()
    r = run(DESK, [BIN, "vault", "enable"])
    check("enable without a receipt refuses plaintext credentials and removes nothing",
          refused(r) and re.search(r"(?i)plaintext|credential|receipt|migrat", r.text) is not None
          and legacy_files() == legacy["files"], r.summary())
    # The operator stops the broker before importing state.
    os.killpg(broker.pid, signal.SIGTERM)
    try:
        broker.wait(20)
    except subprocess.TimeoutExpired:
        os.killpg(broker.pid, signal.SIGKILL)
        broker.wait()
    check("broker stopped for the import", broker.poll() is not None and not wait_tcp(BROKER_ADDR, timeout=1), "")
    r = run(DESK, [BIN, "vault", "migrate", "export"])
    check("migration export refuses a non-pipe stdout", refused(r) and not parse_json(r.out), r.summary())

    # Operator pipes the export straight into the broker's import. The bundle
    # never touches disk and is never printed.
    exp_err = os.path.join(RUN_DIR, "export.err")
    imp_out = os.path.join(RUN_DIR, "import.out")
    with open(exp_err, "wb") as ee, open(imp_out, "wb") as io_:
        exp = subprocess.Popen(as_user(DESK, [BIN, "vault", "migrate", "export"]), stdin=subprocess.DEVNULL,
                               stdout=subprocess.PIPE, stderr=ee, start_new_session=True, cwd=account(DESK).pw_dir)
        imp = subprocess.Popen(as_user(BROKER, [BIN, "vault", "migrate", "import"]), stdin=exp.stdout, stdout=io_,
                               stderr=subprocess.STDOUT, start_new_session=True, cwd=account(BROKER).pw_dir)
        exp.stdout.close()
        exp_rc, imp_rc = exp.wait(60), imp.wait(60)
    receipt_text = read_log(imp_out)
    os.unlink(imp_out)
    receipt = parse_json(receipt_text) or {}
    must("export | import moves the legacy state into the broker", exp_rc == 0 and imp_rc == 0 and bool(receipt),
         f"export rc={exp_rc} {short(read_log(exp_err))} import rc={imp_rc} {short(receipt_text)}")
    check("import receipt contains no secret and is signed",
          not any(v in receipt_text for v in SECRETS.values() if v) and bool(receipt.get("signature")), "")
    dispositions = {i.get("path", "").split("/")[-1]: i.get("disposition") for i in receipt.get("items", [])}
    info("receipt dispositions", json.dumps(dispositions) + " revokeRequired=" +
         json.dumps(receipt.get("revokeRequired")))
    expected = {"admin.json": "imported", "identity.key": "imported", "tunnels.json": "imported",
                "service.json": "discarded"}
    if any(f.endswith("endpoint.json") for f in legacy["files"]):
        expected["endpoint.json"] = "discarded"
    check("same login imports the account, owner and definitions; discards tunnel-service files",
          dispositions == expected and not receipt.get("revokeRequired"), json.dumps(dispositions))
    protected = os.path.join(LAYOUT_ROOT, "private", legacy["tunnels_rel"])
    try:
        with open(protected) as f:
            stored = json.load(f)
        st = os.stat(protected)
    except (OSError, ValueError):
        stored, st = None, None
    check("retained definitions and TTL expiry are stored unchanged in protected broker state",
          stored == legacy["tunnels"] and st is not None and st.st_uid == account(BROKER).pw_uid
          and not st.st_mode & 0o077, protected + (" " + oct(st.st_mode & 0o7777) if st else " missing"))
    check("legacy desktop files are untouched until activation", legacy_files() == legacy["files"],
          ", ".join(legacy_files()))

    w = run(DESK, ["sh", "-c", "umask 077 && cat > receipt.json"], stdin_data=receipt_text)
    broker, log = start_broker()
    r = run(DESK, [BIN, "vault", "enable", "--receipt", account(DESK).pw_dir + "/receipt.json"])
    must("desktop activates with the signed receipt", w.rc == 0 and r.rc == 0, r.summary())
    left = legacy_files()
    check("activation removed the legacy login, owner key and tunnel files", not left, ", ".join(left))
    check("no service.json or endpoint.json remains for the desktop",
          not any(f.endswith(("service.json", "endpoint.json")) for f in left), "")
    hits = files_containing({k: v for k, v in SECRETS.items() if k != "wrong-passphrase"},
                            [account(DESK).pw_dir], set())
    check("desktop home holds no secret after cutover", not hits, json.dumps(hits))
    info("desktop home after cutover", "\n".join(tree(account(DESK).pw_dir)))
    legacy_helper_refused()
    check("no legacy tunnel helper runs for the desktop", not tunnel_helpers(DESK), "")
    return broker, log


def migrate_after(legacy, broker):
    restored = wait_tcp(TUNNEL_LISTEN, broker, timeout=90)
    check("broker runtime restores the retained tunnel without a session grant",
          restored and listener_uid(TUNNEL_LISTEN) == account(BROKER).pw_uid,
          f"listening={restored} uid={listener_uid(TUNNEL_LISTEN)}")
    r = pty_run(DESK, [BIN, "vault", "run", "--", "control", "tunnel", "list"], approve(SECRETS["passphrase"]))
    entry = find_entry(parse_json(r.out), "id", TUNNEL_ID) or {}
    check("retained tunnel keeps its original absolute expiry",
          r.rc == 0 and same_instant(entry.get("expires"), legacy["tunnel"].get("expires")),
          f"before={legacy['tunnel'].get('expires')} after={entry.get('expires')} {r.summary()}")
    r = pty_run(DESK, [BIN, "vault", "run", "--", "control", "session"], approve(SECRETS["passphrase"]))
    owner = find_key(parse_json(r.out), "ownerId")
    check("task owner is stable across the cutover", r.rc == 0 and owner == legacy["owner"],
          f"before={legacy['owner']} after={owner}")
    r = pty_run(DESK, [BIN, "vault", "run", "--", "control", "task", "get", WORKER_NAME, legacy["task"]],
                approve(SECRETS["passphrase"]), timeout=180)
    task = parse_json(r.out) or {}
    check("pre-cutover task remains visible to the same owner", r.rc == 0 and task.get("state") == "succeeded",
          r.summary())
    r = pty_run(DESK, [BIN, "vault", "run", "--", "control", "machines", "--compact"], approve(SECRETS["passphrase"]))
    check("migrated desktop lists the fleet through the broker", r.rc == 0 and WORKER_NAME in r.out, r.summary())
    r = run(DESK, [BIN, "machines"])
    check("migrated desktop ordinary CLI is locked", refused(r) and WORKER_NAME not in r.out, r.summary())


def scenario_clean():
    setup_fleet()
    provision_refusals()
    marker = provision()
    file_separation("after provision")
    broker_trust_refusals()
    broker, log = start_broker()
    frontend(marker, broker, log)
    same_user_and_outsider()
    file_separation("after approvals")
    return broker, log


def scenario_migrate():
    setup_fleet()
    legacy = legacy_desktop()
    provision()
    broker, log = migrate_cutover(legacy)
    migrate_after(legacy, broker)
    file_separation("after migration")
    return broker, log


SCENARIOS = {"clean": scenario_clean, "migrate": scenario_migrate}


def main():
    if os.geteuid() != 0 or os.environ.get("CONTROL_VAULT_SMOKE_CONTAINER") != "1" or not os.path.exists("/.dockerenv"):
        print("refusing to run outside the disposable vault-smoke container", file=sys.stderr)
        return 2
    scenario = sys.argv[1] if len(sys.argv) > 1 else "clean"
    if scenario not in SCENARIOS:
        print("usage: smoke.py [" + "|".join(SCENARIOS) + "]", file=sys.stderr)
        return 2
    print(f"SCENARIO {scenario}", flush=True)
    failed = False
    try:
        broker, log = SCENARIOS[scenario]()
        check("broker stayed up for the whole run", broker.poll() is None, read_log(log)[-800:])
        info("broker log tail", read_log(log)[-1500:])
    except Fatal as e:
        failed = True
        print(f"ABORT after required step failed: {e}", flush=True)
        for name in ("gateway.log", "broker.log"):
            info(name, read_log(os.path.join(RUN_DIR, name))[-1500:])
    except Exception as e:  # noqa: BLE001 - report the harness error, then fail
        failed = True
        print(f"ABORT harness error: {redact(repr(e))}", flush=True)
    finally:
        for p in PROCS:
            try:
                os.killpg(p.pid, signal.SIGTERM)
            except OSError:
                pass
    required_failures = [r["check"] for r in RESULTS if r["result"] == "FAIL"]
    summary = {"passed": sum(r["result"] == "PASS" for r in RESULTS), "failed": len(required_failures),
               "warnings": sum(r["result"] == "WARN" for r in RESULTS), "aborted": failed}
    print("SUMMARY " + json.dumps(summary), flush=True)
    print("REPORT " + json.dumps({"scenario": scenario, "summary": summary, "results": RESULTS}), flush=True)
    return 1 if failed or required_failures else 0


if __name__ == "__main__":
    sys.exit(main())
