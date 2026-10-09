#!/usr/bin/env python3
"""Publication fence tests: simulated inventories, owned children and real flocks."""
import contextlib
import copy
import fcntl
import importlib.util
import io
import json
import os
from pathlib import Path
import select
import subprocess
import sys
import tempfile
import threading
import time
import unittest
from unittest.mock import patch


spec = importlib.util.spec_from_file_location("drain", Path(__file__).with_name("e2e-drain-publication.py"))
drain = importlib.util.module_from_spec(spec)
spec.loader.exec_module(drain)
CLEAN = {"repository": "sha256:" + "a" * 64, "server_checked": False,
         "integrity_checked": False, "ready": 0, "retry_waiting": 0,
         "historical_uploads": [], "branch_operations": [], "issues": []}


class PublicationFenceTests(unittest.TestCase):
    def setUp(self):
        temp = tempfile.TemporaryDirectory()
        self.addCleanup(temp.cleanup)
        self.base = Path(temp.name).resolve()
        (self.base / "bin").mkdir()
        (self.base / "home").mkdir()
        self.binary = self.base / "bin/cxt"
        self.binary.write_text("#!/bin/sh\nexit 0\n")
        self.binary.chmod(0o700)
        self.repo = self.base / "repo"
        (self.repo / ".cxt").mkdir(parents=True)
        (self.repo / ".cxt/HEAD").write_text("ref: refs/heads/main\n")
        home = patch.dict(os.environ, {"HOME": str(self.base / "home")})
        home.start()
        self.addCleanup(home.stop)

    def fence(self, **kwargs):
        return drain.drain(str(self.binary), [str(self.repo)], **kwargs)

    def test_fixture_scope_rejects_unsafe_inputs_before_any_command(self):
        with patch.object(drain, "command") as command:
            bad = [([str(self.base.parent)], str(self.binary)),
                   ([str(self.repo), str(self.repo)], str(self.binary)),
                   ([], str(self.binary)), ([str(self.repo)], "cxt")]
            for roots, binary in bad:
                with self.subTest(roots=roots), self.assertRaises(drain.DrainError):
                    drain.drain(binary, roots)
            with patch.dict(os.environ, {"HOME": str(self.base.parent)}), self.assertRaises(drain.DrainError):
                self.fence()
            self.binary.rename(self.base / "real-cxt")
            self.binary.symlink_to(self.base / "real-cxt")
            with self.assertRaises(drain.DrainError):
                self.fence()
            command.assert_not_called()

    def test_private_fixture_and_no_alias_roots_or_metadata(self):
        self.base.chmod(0o755)
        with self.assertRaises(drain.DrainError):
            drain.fixture(str(self.binary), [str(self.repo)])
        self.base.chmod(0o700)
        alias = self.base / "alias"
        alias.symlink_to(self.repo)
        with self.assertRaises(drain.DrainError):
            drain.fixture(str(self.binary), [str(alias)])
        (self.repo / ".cxt/HEAD").rename(self.repo / "head")
        (self.repo / ".cxt/HEAD").symlink_to(self.repo / "head")
        with self.assertRaises(drain.DrainError):
            drain.fixture(str(self.binary), [str(self.repo)])

    def test_argv_boundaries_and_all_supported_helpers(self):
        exe = str(self.binary)
        valid = ["branch-replay 123", "branch-replay", "branch-state-sync", "capture-replay", "historical-sync",
                 "branch-deletion-finalize topic " + "a" * 40 + " 123",
                 "pending-sync --resolve opaque-session sha256:" + "b" * 64,
                 "live-watch claude private-session", "live-capture codex private-session",
                 "live-publish claude private-session"]
        output = "\n".join(f"{i + 10} {exe} git-hook {args}" for i, args in enumerate(valid))
        output += f"\n99 {exe}-other git-hook branch-state-sync"
        output += f"\n100 /bin/sh -c {exe} git-hook branch-state-sync"
        output += "\n101 /other/bin/cxt git-hook branch-state-sync"
        output += f"\n102 {exe} git-hook branch-state-sync-extra"
        for event in drain.LIVE_EVENTS:
            output += f"\n103 {exe}-other git-hook {event} claude private-session"
            output += f"\n104 /other/bin/cxt git-hook {event} claude private-session"
            output += f"\n105 /bin/sh -c {exe} git-hook {event} claude private-session"
            output += f"\n106 {exe} git-hook {event}-extra claude private-session"
        self.assertEqual([e for _, e in drain.matching_helpers(output, {exe})],
                         [v.split()[0] for v in valid])
        for args in ("branch-state-sync private-argument", "capture-replay private-argument", "branch-replay bad-pid",
                     "pending-sync --resolve private-session bad-hash",
                     "live-watch", "live-capture claude", "live-publish unknown private-session",
                     "live-publish claude private session", "live-watch codex " + "s" * 257):
            with self.assertRaises(drain.DrainError) as raised:
                drain.matching_helpers(f"12 {exe} git-hook {args}", {exe})
            self.assertNotIn("private", str(raised.exception))
            self.assertIn("pid=12", str(raised.exception))
        with self.assertRaises(drain.DrainError):
            drain.matching_helpers("unparseable", {exe})

    def test_empty_or_incomplete_inventory_fails_at_every_scan(self):
        for bad in ("", " \t\n", f"123 {self.binary}", f"123 {self.binary} git-hook"):
            for stage in range(3):
                with self.subTest(inventory=repr(bad), stage=stage):
                    observations = iter(["1 /test/idle"] * stage + [bad])
                    def command(args, *unused, **kwargs):
                        return next(observations) if args[0] == "ps" else json.dumps(CLEAN)
                    with patch.object(drain, "command", side_effect=command), self.assertRaises(drain.DrainError):
                        self.fence(timeout=2)
                    self.assert_locks_released()
        self.assertEqual(drain.matching_helpers("1 /other/bin/cxt git-hook\n2 /test/idle",
                                               {str(self.binary)}), [])

    def test_inventory_deadline_retains_last_fixture_observation(self):
        # Fake clock/processes reproduce both deadline boundaries without a
        # real ps, sleeping, or leaving an unjoined child after timeout.
        for cap, count, cause in ((.03, 2, "command still running: event=inventory"),
                                  (.01, 1, "deadline exceeded: inventory")):
            with self.subTest(cap=cap):
                now, spawned = [0.0], []
                class InventoryChild:
                    pid = 102
                    def wait(self, timeout):
                        if len(spawned) == 1:
                            now[0] += .001
                            return 0
                        now[0] += timeout
                        raise subprocess.TimeoutExpired("synthetic inventory", timeout)
                def spawn(args, **kwargs):
                    self.assertEqual(args, ["ps", "-ww", "-axo", "pid=,command="])
                    spawned.append(args)
                    kwargs["stdout"].write(
                        f"7 {self.binary} git-hook live-watch claude private-session\n".encode())
                    return InventoryChild()
                def sleep(seconds):
                    now[0] += seconds
                with patch.object(drain.time, "monotonic", side_effect=lambda: now[0]), \
                        patch.object(drain.time, "sleep", side_effect=sleep), \
                        patch.object(drain.subprocess, "Popen", side_effect=spawn):
                    with self.assertRaises(drain.DrainError) as raised:
                        self.fence(timeout=cap)
                self.assertEqual(str(raised.exception),
                                 cause + "; last fixture observation: pid=7 event=live-watch")
                self.assertEqual(len(spawned), count)
                self.assertNotIn("private-session", str(raised.exception))
                self.assertNotIn(str(self.binary), str(raised.exception))

    def test_inventory_errors_use_latest_successful_scan_only(self):
        cases = [([], "unavailable"), ([[(7, "live-watch")], []], "none"),
                 ([[], [(8, "live-publish")]], "pid=8 event=live-publish"),
                 ([[], [], [(9, "live-capture")]], "pid=9 event=live-capture")]
        for scans, expected in cases:
            with self.subTest(last_seen=expected):
                error = drain.DrainError("command still running: event=inventory")
                with patch.object(drain, "inventory", side_effect=scans + [error]), \
                        patch.object(drain, "command", return_value=json.dumps(CLEAN)):
                    with self.assertRaises(drain.DrainError) as raised:
                        self.fence(timeout=2)
                self.assertEqual(str(raised.exception),
                                 f"command still running: event=inventory; last fixture observation: {expected}")
                self.assert_locks_released()

    def test_observer_retry_and_final_publication_drain_before_historical(self):
        # SessionEnd has removed registration, but an in-flight observer cycle
        # can finish capture/publication before its next registration check.
        watcher = f"10 {self.binary} git-hook live-watch claude private-session"
        stages = [watcher, watcher + f"\n11 {self.binary} git-hook live-capture claude private-session",
                  watcher, watcher + f"\n12 {self.binary} git-hook live-publish claude private-session",
                  f"12 {self.binary} git-hook live-publish claude private-session",
                  f"13 {self.binary} git-hook pending-sync"] + ["1 /test/idle"] * 3
        calls = []
        scans = 0
        def command(args, *unused, **kwargs):
            nonlocal scans
            if args[0] == "ps":
                scans += 1
                return stages[scans - 1]
            self.assertGreaterEqual(scans, 7, "empty status must not bypass a live producer")
            calls.append(args[1:])
            return json.dumps(CLEAN)
        with patch.object(drain, "command", side_effect=command):
            self.fence(timeout=2)
        self.assertEqual(scans, len(stages))
        self.assertEqual(calls, [["git-hook", "historical-sync"], ["sync", "status", "--json"]])
        self.assert_locks_released()

    def test_delayed_live_helpers_restart_each_completion_scan(self):
        for event in sorted(drain.LIVE_EVENTS):
            for stage in (1, 2):
                with self.subTest(event=event, stage=stage):
                    observations = iter(["1 /test/idle"] * stage +
                                        [f"10 {self.binary} git-hook {event} codex private-session"] +
                                        ["1 /test/idle"] * 3)
                    calls = []
                    def command(args, *unused, **kwargs):
                        if args[0] == "ps":
                            return next(observations)
                        calls.append(args[1:])
                        return json.dumps(CLEAN)
                    with patch.object(drain, "command", side_effect=command):
                        self.fence(timeout=2)
                    self.assertEqual(calls.count(["git-hook", "historical-sync"]), 2)
                    self.assertEqual(calls.count(["sync", "status", "--json"]), stage)
                    self.assert_locks_released()

    def test_upstream_handoff_and_empty_queue_cannot_finish_early(self):
        stages = [[(10, "branch-replay")], [(11, "branch-state-sync")], [],
                  [(12, "historical-sync")], [], [], []]
        calls = []
        def command(args, *unused, **kwargs):
            calls.append(args[1:])
            return json.dumps(CLEAN) if args[1] == "sync" else ""
        with patch.object(drain, "inventory", side_effect=stages), patch.object(drain, "command", side_effect=command):
            self.fence(timeout=2)
        self.assertEqual(calls, [["git-hook", "historical-sync"], ["git-hook", "historical-sync"],
                                 ["sync", "status", "--json"]])

    def test_new_producer_after_status_forces_another_fence(self):
        with patch.object(drain, "inventory", side_effect=[[], [], [(9, "pending-sync")], [], [], []]), \
                patch.object(drain, "command", return_value=json.dumps(CLEAN)) as command:
            self.fence(timeout=2)
        self.assertEqual(command.call_count, 4)

    def test_producer_deadline_does_not_start_publication(self):
        start = time.monotonic()
        for event in ["branch-replay", "capture-replay"] + sorted(drain.LIVE_EVENTS):
            args = " claude private-session" if event in drain.LIVE_EVENTS else ""
            with self.subTest(event=event), patch.object(drain, "command", return_value=
                    f"7 {self.binary} git-hook {event}{args}") as command:
                with self.assertRaisesRegex(drain.DrainError, f"pid=7 event={event}") as raised:
                    self.fence(timeout=0.04)
                self.assertNotIn("private-session", str(raised.exception))
                self.assertTrue(all(call.args[0][0] == "ps" for call in command.call_args_list))
        self.assertLess(time.monotonic() - start, 1)
        with self.assertRaises(drain.DrainError):
            self.fence(timeout=46)

    def test_status_rejects_dirty_missing_malformed_and_duplicate_fields(self):
        drain.check_status(json.dumps(CLEAN))
        for key, item in (("historical_uploads", {"reason": "private-detail"}),
                          ("branch_operations", {"state": "private-detail"}), ("issues", "private-detail")):
            report = copy.deepcopy(CLEAN)
            report[key] = [item]
            if key == "historical_uploads":
                report["retry_waiting"] = 1
            with self.assertRaisesRegex(drain.DrainError, "unresolved") as raised:
                drain.check_status(json.dumps(report))
            self.assertNotIn("private-detail", str(raised.exception))
        for key in CLEAN:
            report = copy.deepcopy(CLEAN)
            del report[key]
            with self.assertRaises(drain.DrainError):
                drain.check_status(json.dumps(report))
        for key, bad in (("issues", None), ("historical_uploads", {}), ("ready", False),
                         ("retry_waiting", -1), ("repository", "private-detail"), ("server_checked", 0)):
            report = dict(CLEAN, **{key: bad})
            with self.assertRaises(drain.DrainError):
                drain.check_status(json.dumps(report))
        for raw in ("invalid private-detail", "[]", "[" * 2000, json.dumps(CLEAN)[:-1] + ', "issues": []}'):
            with self.assertRaises(drain.DrainError):
                drain.check_status(raw)

    def lock_holder(self, path):
        # Self-expiring on failure; the test never signals or kills this child.
        path.parent.mkdir(parents=True, exist_ok=True)
        child = subprocess.Popen([sys.executable, "-c", """
import fcntl, select, sys
with open(sys.argv[1], 'a+') as lock:
    fcntl.flock(lock, fcntl.LOCK_EX)
    print('ready', flush=True)
    select.select([sys.stdin], [], [], 2)
""", str(path)], stdin=subprocess.PIPE, stdout=subprocess.PIPE, text=True)
        def finish():
            try:
                child.communicate(None if child.returncode is not None else "release\n", timeout=3)
            except BrokenPipeError:
                child.wait(timeout=3)
        self.addCleanup(finish)
        self.assertTrue(select.select([child.stdout], [], [], 2)[0])
        self.assertEqual(child.stdout.readline().strip(), "ready")
        return child

    def assert_locks_released(self):
        directory = self.repo / ".cxt/locks/historical-backfill"
        for name in ("daemon", "worker", "queue"):
            path = directory / (name + ".flock")
            if path.exists():
                with path.open("r+") as lock:
                    fcntl.flock(lock, fcntl.LOCK_EX | fcntl.LOCK_NB)

    def test_real_lock_contention_times_out_and_releases_partial_locks(self):
        for name in ("daemon", "worker", "queue"):
            with self.subTest(lock=name):
                child = self.lock_holder(self.repo / f".cxt/locks/historical-backfill/{name}.flock")
                with self.assertRaisesRegex(drain.DrainError, name + " lock"):
                    with drain.publication_locks(self.repo, time.monotonic() + 0.04):
                        self.fail("entered while an owned process held the lock")
                child.communicate("release\n", timeout=3)
                self.assert_locks_released()

    def test_real_lock_release_allows_completion_without_inode_replacement(self):
        path = self.repo / ".cxt/locks/historical-backfill/queue.flock"
        child = self.lock_holder(path)
        inode = path.stat().st_ino
        release = threading.Timer(0.04, lambda: child.communicate("release\n", timeout=3))
        release.start()
        try:
            with drain.publication_locks(self.repo, time.monotonic() + 2):
                self.assertEqual(path.stat().st_ino, inode)
        finally:
            release.join()
        self.assert_locks_released()

    def test_actual_fixture_commands_and_dirty_status_release_locks(self):
        # Status verifies that all three OS locks are held by the fence.
        self.binary.write_text(f"#!{sys.executable}\n" + """
import fcntl, json, pathlib, sys
root = pathlib.Path.cwd()
with (root / 'calls').open('a') as calls:
    calls.write(' '.join(sys.argv[1:]) + '\\n')
if sys.argv[1:] == ['sync', 'status', '--json']:
    for name in ('daemon', 'worker', 'queue'):
        with (root / ('.cxt/locks/historical-backfill/' + name + '.flock')).open('r+') as lock:
            try: fcntl.flock(lock, fcntl.LOCK_EX | fcntl.LOCK_NB)
            except BlockingIOError: continue
            sys.exit(9)
    print((root / 'status.json').read_text())
elif sys.argv[1:] != ['git-hook', 'historical-sync']:
    sys.exit(8)
""")
        (self.repo / "status.json").write_text(json.dumps(CLEAN))
        with patch.object(drain, "inventory", return_value=[]):
            self.fence(timeout=2)
            (self.repo / "status.json").write_text(json.dumps(dict(CLEAN, issues=["private-detail"])))
            with self.assertRaises(drain.DrainError):
                self.fence(timeout=2)
        self.assertEqual((self.repo / "calls").read_text().splitlines(),
                         ["git-hook historical-sync", "sync status --json"] * 2)
        self.assert_locks_released()

    def test_command_timeout_leaves_owned_child_alive_then_joined(self):
        children = []
        real_popen = subprocess.Popen
        def spawn(*args, **kwargs):
            child = real_popen(*args, **kwargs)
            children.append(child)
            return child
        try:
            with patch.object(drain.subprocess, "Popen", side_effect=spawn):
                with self.assertRaisesRegex(drain.DrainError, "command still running"):
                    drain.command([sys.executable, "-c", "import time; time.sleep(.2)"],
                                  time.monotonic() + .04, "historical-sync")
            self.assertIsNone(children[0].poll())
        finally:
            for child in children:
                self.assertEqual(child.wait(timeout=3), 0)

    def test_failed_and_oversized_commands_do_not_echo_output(self):
        for script, limit in (("import sys; print('private-detail'); sys.exit(2)", 100),
                              ("print('private-detail')", 2)):
            with self.assertRaises(drain.DrainError) as raised:
                drain.command([sys.executable, "-c", script], time.monotonic() + 2,
                              "sync-status", limit=limit)
            self.assertNotIn("private-detail", str(raised.exception))
        with patch.object(drain.subprocess, "Popen") as spawn:
            with self.assertRaises(drain.DrainError):
                drain.command([str(self.binary)], time.monotonic() - 1, "historical-sync")
            spawn.assert_not_called()

    def test_main_sanitizes_filesystem_errors_and_refuses_symlink_lock(self):
        error = io.StringIO()
        with patch.object(drain, "drain", side_effect=OSError("private-detail")), contextlib.redirect_stderr(error):
            self.assertEqual(drain.main([str(self.binary), str(self.repo)]), 1)
        self.assertNotIn("private-detail", error.getvalue())
        directory = self.repo / ".cxt/locks/historical-backfill"
        directory.mkdir(parents=True)
        (directory / "worker.flock").symlink_to(self.repo / ".cxt/HEAD")
        with self.assertRaises(OSError):
            with drain.publication_locks(self.repo, time.monotonic() + 1):
                self.fail("symlink lock accepted")
        with (directory / "daemon.flock").open("r+") as lock:
            fcntl.flock(lock, fcntl.LOCK_EX | fcntl.LOCK_NB)


if __name__ == "__main__":
    unittest.main()
