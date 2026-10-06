#!/usr/bin/env python3
"""Version-1 Control desktop helper. Dependencies and permissions: docs/rpa.md."""

import importlib
import json
import os
from pathlib import Path
import signal
import sys
import time


FIELDS = {
    "inspect": ({"app", "limit"}, set()),
    "screenshot": (set(), set()),
    "click": ({"target", "x", "y", "button", "count"}, set()),
    "focus": ({"target"}, {"target"}),
    "setValue": ({"target", "text"}, {"target", "text"}),
    "move": ({"x", "y"}, {"x", "y"}),
    "drag": ({"x", "y", "durationMs"}, {"x", "y"}),
    "scroll": ({"clicks"}, {"clicks"}),
    "type": ({"text"}, {"text"}),
    "keys": ({"keys"}, {"keys"}),
    "wait": ({"milliseconds"}, {"milliseconds"}),
}


def integer(value, low, high):
    return type(value) is int and low <= value <= high


def validate(arguments):
    """Validate all actions before importing a backend or causing side effects."""
    if not isinstance(arguments, dict) or set(arguments) != {"actions"}:
        raise ValueError("expected only actions")
    actions = arguments["actions"]
    if not isinstance(actions, list) or not 1 <= len(actions) <= 100:
        raise ValueError("actions must contain 1..100 items")
    for index, action in enumerate(actions):
        if not isinstance(action, dict) or action.get("type") not in FIELDS:
            raise ValueError(f"action {index}: unknown action type")
        kind = action["type"]
        optional, required = FIELDS[kind]
        if set(action) - optional - {"type"} or required - set(action):
            raise ValueError(f"action {index}: invalid fields for {kind}")
        for key in ("app", "text"):
            if key in action and (not isinstance(action[key], str) or len(action[key]) > 4096):
                raise ValueError(f"action {index}: invalid {key}")
        for key, low, high in (
            ("x", -32768, 32767), ("y", -32768, 32767),
            ("limit", 1, 200), ("durationMs", 100, 5000),
            ("clicks", -100, 100), ("milliseconds", 1, 5000), ("count", 1, 2),
        ):
            if key in action and not integer(action[key], low, high):
                raise ValueError(f"action {index}: invalid {key}")
        if "target" in action:
            target = action["target"]
            if not isinstance(target, dict) or not target or set(target) - {"app", "name", "role", "automationId"}:
                raise ValueError(f"action {index}: invalid selector")
            if any(not isinstance(v, str) or not v or len(v) > 4096 for v in target.values()):
                raise ValueError(f"action {index}: selector values must be nonempty strings")
            if "automationId" in target and sys.platform != "win32":
                raise ValueError("automationId selectors are Windows-only")
        if kind == "click":
            if action.get("button", "left") not in ("left", "right", "middle"):
                raise ValueError(f"action {index}: invalid button")
            if "target" in action:
                if "x" in action or "y" in action or action.get("button", "left") != "left" or action.get("count", 1) != 1:
                    raise ValueError("selector click supports only a single native left activation")
            elif "x" not in action or "y" not in action:
                raise ValueError("click requires a selector or x and y")
        if kind == "type" and any(ord(c) > 126 or (ord(c) < 32 and c not in "\n\t") for c in action["text"]):
            raise ValueError("type supports printable ASCII, newline and tab; use setValue for Unicode")
        if kind == "keys":
            keys = action["keys"]
            if not isinstance(keys, list) or not 1 <= len(keys) <= 8 or any(not isinstance(k, str) or not 1 <= len(k) <= 32 for k in keys):
                raise ValueError("keys must contain 1..8 key names")
    return actions


class Desktop:
    def __init__(self, actions):
        self.gui = None
        self.accessibility = None
        if sys.platform == "win32":
            import ctypes
            session = ctypes.c_ulong()
            if not ctypes.windll.kernel32.ProcessIdToSessionId(os.getpid(), ctypes.byref(session)) or session.value == 0:
                raise RuntimeError("RPA requires a logged-in user session; use Windows user-login startup, not LocalService")
        needs_gui = any(a["type"] in {"screenshot", "move", "drag", "scroll", "type", "keys"} or (a["type"] == "click" and "target" not in a) for a in actions)
        needs_accessibility = any(a["type"] == "inspect" or "target" in a for a in actions)
        if needs_gui:
            if sys.platform == "darwin":
                import ApplicationServices
                if not ApplicationServices.AXIsProcessTrusted():
                    raise RuntimeError("grant Accessibility permission to the node/helper in System Settings")
                if any(a["type"] == "screenshot" for a in actions):
                    import Quartz
                    if not Quartz.CGPreflightScreenCaptureAccess():
                        raise RuntimeError("grant Screen Recording permission to the node/helper in System Settings")
            if sys.platform == "linux" and (os.environ.get("XDG_SESSION_TYPE") == "wayland" or os.environ.get("WAYLAND_DISPLAY")):
                raise RuntimeError("coordinate input and screenshots require an X11 session; Wayland is not supported")
            if sys.platform == "linux" and not os.environ.get("DISPLAY"):
                raise RuntimeError("DISPLAY is missing; run the node in the user's X11 desktop session")
            self.gui = importlib.import_module("pyautogui")
            self.gui.FAILSAFE = True
            self.gui.PAUSE = 0.1
            for action in actions:
                if action["type"] == "keys" and any(k not in self.gui.KEYBOARD_KEYS for k in action["keys"]):
                    raise ValueError("unknown key name; use PyAutoGUI key names")
                if "x" in action and not self.gui.onScreen(action["x"], action["y"]):
                    raise ValueError("coordinates must be on the primary display")
        if needs_accessibility:
            module = {"win32": "windows", "darwin": "macos", "linux": "linux"}.get(sys.platform)
            if module is None:
                raise RuntimeError("unsupported desktop platform")
            self.accessibility = importlib.import_module(module).Backend()

    def run(self, action, workspace, index):
        kind = action["type"]
        if kind == "inspect":
            return self.accessibility.inspect(action.get("app"), action.get("limit", 100))
        if "target" in action:
            self.accessibility.act(kind, action["target"], action.get("text"))
        elif kind == "screenshot":
            image = self.gui.screenshot()
            if image.width * image.height > 64_000_000:
                raise RuntimeError("screenshot exceeds 64 million pixels")
            name = f"screen-{index}.png"
            image.save(workspace / name, "PNG")
            if (workspace / name).stat().st_size > 32 << 20:
                raise RuntimeError("screenshot exceeds 32 MiB")
            width, height = self.gui.size()
            return {"image": name, "width": image.width, "height": image.height,
                    "screenWidth": width, "screenHeight": height,
                    "scaleX": image.width / width, "scaleY": image.height / height}
        elif kind == "click":
            self.gui.click(action["x"], action["y"], clicks=action.get("count", 1), interval=0.1, button=action.get("button", "left"))
        elif kind == "move":
            self.gui.moveTo(action["x"], action["y"])
        elif kind == "drag":
            try:
                self.gui.mouseDown(button="left")
                self.gui.moveTo(action["x"], action["y"], duration=action.get("durationMs", 500) / 1000)
            finally:
                self.gui.FAILSAFE = False
                try:
                    self.gui.mouseUp(button="left")
                finally:
                    self.gui.FAILSAFE = True
        elif kind == "scroll":
            self.gui.scroll(action["clicks"])
        elif kind == "type":
            for char in action["text"]:
                if char in "\n\t":
                    self.gui.press("enter" if char == "\n" else "tab")
                else:
                    self.gui.write(char)
        elif kind == "keys":
            held = []
            try:
                for key in action["keys"]:
                    self.gui.keyDown(key)
                    held.append(key)
            finally:
                # The corner failsafe must not prevent releasing held keys.
                self.gui.FAILSAFE = False
                try:
                    for key in reversed(held):
                        self.gui.keyUp(key)
                finally:
                    self.gui.FAILSAFE = True
        elif kind == "wait":
            time.sleep(action["milliseconds"] / 1000)
        return {"type": kind, "completed": True}


def interrupted(signum, frame):
    raise InterruptedError("desktop helper cancelled")


def main():
    response = {"results": []}
    try:
        request = json.load(sys.stdin)
        if request.get("version") != "1":
            raise ValueError("unsupported helper version")
        actions = validate(request.get("arguments"))
        workspace = Path(request["workspace"])
        if not workspace.is_absolute() or not workspace.is_dir():
            raise ValueError("workspace must be an existing absolute directory")
        desktop = Desktop(actions)
        for index, action in enumerate(actions):
            response["results"].append(desktop.run(action, workspace, index))
    except Exception as exc:
        response["error"] = f"{type(exc).__name__}: {exc}"
    json.dump(response, sys.stdout, ensure_ascii=True)
    sys.stdout.write("\n")


if __name__ == "__main__":
    signal.signal(signal.SIGTERM, interrupted)
    main()
