import concurrent.futures
import json
from pathlib import Path
import shutil
import sys
import tempfile
import unittest

import recorder


class RecorderTests(unittest.TestCase):
    def setUp(self):
        temporary = tempfile.TemporaryDirectory()
        self.addCleanup(temporary.cleanup)
        self.root = Path(temporary.name)
        self.case = {"node": "worker", "nodeId": "stable-id", "family": "recover",
                     "path": "tasks/seed", "taskPrefix": "episode-a-",
                     "originalTaskId": "original", "callLimit": 12}
        self.config = {"cli": sys.executable, "episodes": {"episode": self.case}}
        self.save_config()

    def save_config(self):
        (self.root / "agent-config.json").write_text(json.dumps(self.config))

    def records(self, folder):
        return [json.loads(line) for line in (self.root / folder / "episode.jsonl").read_text().splitlines()]

    def test_exact_help_before_target_and_recovery_checks(self):
        for flag in ["--help", "-h"]:
            self.assertIsNone(recorder.scope_error(["exec", flag], self.case))

    def test_help_is_not_a_generic_authorization_bypass(self):
        for args in [["exec", "other", "--help"], ["exec", "--help", "worker"],
                     ["exec", "worker", "--", "echo", "--help"],
                     ["task", "start", "worker", "--help"], ["help", "exec"],
                     ["runbooks", "--node=other"], ["runbooks", "--node"]]:
            with self.subTest(args=args):
                self.assertIsNotNone(recorder.scope_error(args, self.case))

    def test_recovery_direct_read_allowed_tracked_read_denied(self):
        self.assertIsNone(recorder.scope_error(["call", "worker", "files.read", '{"path":"tasks/seed/receipt.json"}'], self.case))
        result = recorder.control(self.root, "episode", ["task", "start", "worker", '{"capability":"files.read"}'])
        self.assertTrue(result["blocked"])
        self.assertEqual(result["exitCode"], 125)
        self.assertEqual(len(self.records("traces")), 2)

    def test_execution_target_and_id_restrictions_remain(self):
        self.case["family"] = "join"
        self.assertIsNone(recorder.scope_error(["exec", "worker", "--id", "episode-a-1", "--", "echo", "--help"], self.case))
        for args in [["exec", "other", "--id", "episode-a-1", "--", "echo"],
                     ["exec", "worker", "--id", "foreign", "--", "echo"],
                     ["call", "worker", "files.read", '{"path":"tasks/seed/../other"}']]:
            self.assertIsNotNone(recorder.scope_error(args, self.case))

    def test_help_is_forwarded_and_nonzero_usage_is_not_a_denial(self):
        # Python has no exec subcommand; its real usage error must be retained.
        result = recorder.control(self.root, "episode", ["exec", "--help"])
        self.assertFalse(result["blocked"])
        self.assertNotEqual(result["exitCode"], 0)
        self.assertTrue(result["stderr"])
        rows = self.records("traces")
        self.assertEqual(rows[0]["argv"], ["exec", "--help"])
        self.assertEqual(rows[0]["callId"], rows[1]["callId"])

    def test_concurrent_calls_reserve_limit_atomically(self):
        self.case["callLimit"] = 1
        self.save_config()
        with concurrent.futures.ThreadPoolExecutor(max_workers=4) as pool:
            results = list(pool.map(lambda _: recorder.control(self.root, "episode", ["exec", "--help"]), range(4)))
        self.assertEqual(sum(not row["blocked"] for row in results), 1)
        self.assertEqual(len(self.records("traces")), 8)

    def test_independent_shell_failures_before_control_wrapper(self):
        for command in ["if then", "nonexistent_control_benchmark_command"]:
            result = recorder.shell(self.root, "episode", {"shell": "/bin/sh", "command": command})
            self.assertNotEqual(result["exitCode"], 0)
            self.assertTrue(result["stderr"])
        self.assertFalse((self.root / "traces").exists())
        self.assertEqual(len(self.records("shell-traces")), 4)

    @unittest.skipUnless(shutil.which("zsh"), "zsh not installed")
    def test_zsh_scalar_command_construction_failure(self):
        result = recorder.shell(self.root, "episode", {"shell": shutil.which("zsh"), "command": "runner='missing command'; $runner"})
        self.assertEqual(result["exitCode"], 127)
        self.assertIn("missing command", result["stderr"])

    def test_shell_success_cwd_and_nonzero_exit(self):
        result = recorder.shell(self.root, "episode", {"shell": "/bin/sh", "command": "pwd; echo diagnostic >&2; exit 9"})
        self.assertEqual(result["exitCode"], 9)
        self.assertEqual(Path(result["stdout"].strip()).resolve(), (self.root / "scratch" / "episode").resolve())
        self.assertIn("diagnostic", result["stderr"])

    def test_launch_failure_and_timeout_are_recorded_once(self):
        result = recorder.shell(self.root, "episode", {"shell": str(self.root / "missing"), "command": "unused"})
        self.assertTrue(result["launchFailed"])
        result = recorder.shell(self.root, "episode", {"shell": "/bin/sh", "command": "echo started; sleep 10", "timeoutSeconds": 0.05})
        self.assertTrue(result["timedOut"])
        self.assertIn("started", result["stdout"])
        self.assertEqual(result["exitCode"], 124)
        self.assertEqual(len(self.records("shell-traces")), 4)


if __name__ == "__main__":
    unittest.main()
