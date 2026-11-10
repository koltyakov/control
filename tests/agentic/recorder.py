#!/usr/bin/env python3
"""Held-out benchmark CLI gate and independent local process recorder.

POSIX orchestrator only. Neither interface is an execution sandbox.
"""
import argparse
import datetime
import fcntl
import json
import os
from pathlib import Path
import signal
import subprocess
import sys
import time
import uuid


def now():
    return datetime.datetime.now(datetime.timezone.utc).isoformat()


def append(path, record):
    path.parent.mkdir(parents=True, exist_ok=True)
    with path.open("a", encoding="utf-8") as stream:
        fcntl.flock(stream, fcntl.LOCK_EX)
        stream.write(json.dumps(record, ensure_ascii=False) + "\n")
        stream.flush()


def scope_error(args, case):
    # Exact local usage requests only. Never treat a help-looking executable
    # argument, JSON value, or arbitrary option combination as a gate bypass.
    if args in (["exec", "--help"], ["exec", "-h"]):
        return None
    if not args:
        return "A Control command is required"
    command = args[0]
    targets = [case["node"], case["nodeId"]]
    if command == "version" or args == ["help"]:
        return None
    if command in ["machines", "runbooks"]:
        for i, arg in enumerate(args[1:], 1):
            if arg == "--node" and (i + 1 == len(args) or args[i + 1] not in targets):
                return "Discovery must target the assigned worker"
            if arg.startswith("--node=") and arg.split("=", 1)[1] not in targets:
                return "Discovery must target the assigned worker"
        if command == "machines" and any(a in ["add", "enable", "disable", "rename", "unregister", "revoke", "invites", "forget"] for a in args[1:]):
            return "Fleet mutation is outside this benchmark"
        return None
    if command not in ["call", "task", "exec", "system"]:
        return "Command outside benchmark allowlist"
    ni = 2 if command == "task" else 1
    if len(args) <= ni or args[ni] not in targets:
        return "Only the assigned User worker is authorized"
    if command == "system":
        return None
    if command == "call":
        if len(args) < 3:
            return "Missing method"
        method = args[2]
        if method in ["node.describe", "system.info", "leases.get", "capabilities.list"]:
            return None
        if method not in ["files.read", "files.list", "files.write"]:
            return "Method outside benchmark allowlist"
        query = json.loads(args[3]) if len(args) > 3 else {}
        path = query.get("path", "").replace("\\", "/")
        if ".." in path.split("/") or not (path == case["path"] or path.startswith(case["path"] + "/")):
            return "File access must stay under this episode fixture"
        if method == "files.write" and (case["family"] != "join" or path != case["path"] + "/report.json"):
            return "This episode does not authorize that file mutation"
        return None
    if command == "task":
        if args[1] in ["get", "wait", "logs"]:
            if len(args) < 4 or not (args[3] == case.get("originalTaskId") or args[3].startswith(case["taskPrefix"])):
                return "Only this episode task IDs may be read"
            return None
        if args[1] != "start":
            return "Task mutation outside benchmark allowlist"
        if case["family"] == "recover":
            return "Recovery is read-only; no new or replayed task is authorized"
        specs = [a for a in args[3:] if not a.startswith("--")]
        if len(specs) != 1 or specs[0].startswith("@"):
            return "Pass exactly one inline task specification"
        spec = json.loads(specs[0])
        if spec.get("capability") != "exec.run" or not spec.get("id", "").startswith(case["taskPrefix"]):
            return "Use exec.run with this episode task prefix"
        if spec.get("inputs") or spec.get("leaseId"):
            return "Inputs and leases are outside these isolated cases"
        if spec.get("args", {}).get("command") not in ["python3", "/usr/bin/python3", "powershell.exe", "pwsh.exe"]:
            return "Use an installed native Python/PowerShell executable"
        return None
    if command == "exec":
        if case["family"] == "recover":
            return "Recovery is read-only; do not execute or replay"
        if "--id" not in args or not args[args.index("--id") + 1].startswith(case["taskPrefix"]):
            return "Choose an explicit episode-prefixed task ID"
    return None


def run_process(argv, timeout, cwd=None):
    """Capture syntax/launch/timeout failures without replaying the command."""
    try:
        process = subprocess.Popen(argv, cwd=cwd, stdout=subprocess.PIPE,
                                   stderr=subprocess.PIPE, start_new_session=True)
    except OSError as error:
        return {"exitCode": 127, "stdout": "", "stderr": str(error), "launchFailed": True}
    timed_out = False
    try:
        stdout, stderr = process.communicate(timeout=timeout)
    except subprocess.TimeoutExpired:
        timed_out = True
        os.killpg(process.pid, signal.SIGKILL)
        stdout, stderr = process.communicate()
    return {"exitCode": 124 if timed_out else process.returncode,
            "stdout": stdout.decode("utf-8", errors="replace"),
            "stderr": stderr.decode("utf-8", errors="replace"),
            "timedOut": timed_out}


def control(root, episode, args):
    config = json.loads((root / "agent-config.json").read_text(encoding="utf-8"))
    case = config["episodes"][episode]
    trace = root / "traces" / (episode + ".jsonl")
    trace.parent.mkdir(parents=True, exist_ok=True)
    call_id, start = uuid.uuid4().hex, time.monotonic()
    # Reserve and count in one lock so concurrent calls cannot exceed the cap.
    with trace.open("a+", encoding="utf-8") as stream:
        fcntl.flock(stream, fcntl.LOCK_EX)
        stream.seek(0)
        count = sum(json.loads(line)["phase"] == "start" for line in stream if line.strip())
        stream.write(json.dumps({"phase": "start", "role": "agent", "kind": "control",
                                 "episode": episode, "callId": call_id,
                                 "startedAt": now(), "argv": args}) + "\n")
        stream.flush()
    try:
        error = "Episode call limit reached" if count >= case["callLimit"] else scope_error(args, case)
    except (ValueError, IndexError, KeyError, TypeError, AttributeError) as exception:
        error = "Invalid benchmark tool arguments: " + str(exception)
    result = ({"exitCode": 125, "stdout": "", "stderr": error, "blocked": True} if error
              else {**run_process([config["cli"], *args], 90), "blocked": False})
    append(trace, {"phase": "finish", "kind": "control", "episode": episode,
                   "callId": call_id, "endedAt": now(),
                   "elapsedSeconds": time.monotonic() - start, **result})
    return result


def shell(root, episode, request):
    """The parent starts this recorder before the shell parses the command."""
    config = json.loads((root / "agent-config.json").read_text(encoding="utf-8"))
    if episode not in config["episodes"]:
        raise ValueError("Unknown episode")
    command = request["command"]
    timeout = request.get("timeoutSeconds", 90)
    if not isinstance(command, str) or not isinstance(timeout, (int, float)) or not 0 < timeout <= 300:
        raise ValueError("Use a command string and timeoutSeconds in 0..300")
    scratch = root / "scratch" / episode
    scratch.mkdir(parents=True, exist_ok=True)
    argv = [request.get("shell", "/bin/zsh"), "-f", "-c", command]
    call_id, start = uuid.uuid4().hex, time.monotonic()
    trace = root / "shell-traces" / (episode + ".jsonl")
    append(trace, {"phase": "start", "kind": "local-shell", "episode": episode,
                   "callId": call_id, "startedAt": now(), "argv": argv, "cwd": str(scratch)})
    result = run_process(argv, timeout, cwd=scratch)
    append(trace, {"phase": "finish", "kind": "local-shell", "episode": episode,
                   "callId": call_id, "endedAt": now(),
                   "elapsedSeconds": time.monotonic() - start, **result})
    return result


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--root", required=True, type=Path)
    parser.add_argument("--episode", required=True)
    parser.add_argument("mode", choices=["control", "shell"])
    parser.add_argument("args", nargs=argparse.REMAINDER)
    options = parser.parse_args()
    if options.mode == "shell":
        result = shell(options.root, options.episode, json.load(sys.stdin))
    else:
        args = options.args[1:] if options.args[:1] == ["--"] else options.args
        result = control(options.root, options.episode, args)
    sys.stdout.write(result["stdout"])
    sys.stderr.write(result["stderr"])
    return result["exitCode"]


if __name__ == "__main__":
    raise SystemExit(main())
