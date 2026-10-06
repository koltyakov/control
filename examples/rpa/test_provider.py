"""Dependency-free tests. These never inspect or manipulate a real desktop."""

import tempfile
import unittest
from pathlib import Path
from types import SimpleNamespace
from unittest.mock import patch

import provider
from accessibility import BackendBase, walk


class SelectorBackend(BackendBase):
    def __init__(self, entries, truncated=False):
        self.items = entries
        self.truncated = truncated

    def entries(self, app):
        return self.items, self.truncated


class ProviderTests(unittest.TestCase):
    def test_validate_entire_batch(self):
        invalid = [
            {}, {"actions": []}, {"actions": [{"type": "shell"}]},
            {"actions": [{"type": "click", "x": 1}]},
            {"actions": [{"type": "click", "target": {"name": "Save"}, "count": 2}]},
            {"actions": [{"type": "inspect"}, {"type": "wait", "milliseconds": 5001}]},
            {"actions": [{"type": "screenshot", "path": "../file"}]},
            {"actions": [{"type": "type", "text": "世界"}]},
            {"actions": [{"type": "keys", "keys": [1]}]},
            {"actions": [{"type": "focus", "target": {"name": ""}}]},
            {"actions": [{"type": "move", "x": True, "y": 0}]},
        ]
        for arguments in invalid:
            with self.subTest(arguments=arguments), self.assertRaises(ValueError):
                provider.validate(arguments)
        provider.validate({"actions": [{"type": "setValue", "target": {"name": "Search"}, "text": "世界"}]})

    def test_unique_exact_selectors(self):
        backend = SelectorBackend([({"app": "Editor", "name": "Save", "role": "Button"}, "one")])
        self.assertEqual(backend.resolve({"app": "Editor", "name": "Save"}), "one")
        with self.assertRaises(RuntimeError):
            backend.resolve({"name": "save"})
        backend.items.append((backend.items[0][0], "two"))
        with self.assertRaises(RuntimeError):
            backend.resolve({"name": "Save"})
        backend.items.pop()
        backend.truncated = True
        with self.assertRaises(RuntimeError):
            backend.resolve({"name": "Save"})

    def test_bounded_traversal(self):
        describe = lambda handle: {"name": str(handle), "role": "Button"}
        entries, truncated = walk([(0, "Editor")], describe, lambda handle: list(range(201)), None)
        self.assertTrue(truncated)
        self.assertLessEqual(len(entries), 2000)
        entries, truncated = walk([(0, "Other"), (1, "Editor")], describe, lambda handle: [], "Editor")
        self.assertEqual([item["name"] for item, handle in entries], ["1"])
        self.assertFalse(truncated)

    def test_wayland_refuses_coordinate_input(self):
        with patch.object(provider.sys, "platform", "linux"), patch.dict(provider.os.environ, {"WAYLAND_DISPLAY": "wayland-0"}):
            with self.assertRaisesRegex(RuntimeError, "Wayland"):
                provider.Desktop([{"type": "move", "x": 1, "y": 1}])

    def test_unknown_keys_rejected_before_execution(self):
        gui = SimpleNamespace(KEYBOARD_KEYS=["ctrl", "c"])
        with patch.object(provider.sys, "platform", "linux"), patch.dict(provider.os.environ, {"DISPLAY": ":fake"}, clear=True), patch.object(provider.importlib, "import_module", return_value=gui):
            with self.assertRaisesRegex(ValueError, "unknown key"):
                provider.Desktop([{"type": "keys", "keys": ["invalid"]}])

    def test_modifier_and_mouse_cleanup_on_error(self):
        released = []

        def key_down(key):
            if key == "c":
                raise RuntimeError("failsafe")

        def move(*args, **kwargs):
            raise RuntimeError("failsafe")

        desktop = object.__new__(provider.Desktop)
        desktop.gui = SimpleNamespace(FAILSAFE=True, keyDown=key_down, keyUp=released.append,
                                      mouseDown=lambda **kw: None, mouseUp=lambda **kw: released.append("mouse"), moveTo=move)
        with tempfile.TemporaryDirectory() as directory:
            with self.assertRaises(RuntimeError):
                desktop.run({"type": "keys", "keys": ["ctrl", "c"]}, Path(directory), 0)
            with self.assertRaises(RuntimeError):
                desktop.run({"type": "drag", "x": 1, "y": 2}, Path(directory), 1)
        self.assertEqual(released, ["ctrl", "mouse"])
        self.assertTrue(desktop.gui.FAILSAFE)


if __name__ == "__main__":
    unittest.main()
