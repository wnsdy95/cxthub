#!/usr/bin/env python3
"""Exercise the actual shell gate and cleanup of scripts/e2e-sync.sh using disposable stubs.

The CI workflow runs this file by name. The agent wrappers it once also
covered were removed with capture; what is tested here is what the suite still
relies on: the server readiness gate, the cleanup of the suite's own shell
jobs, and the waiting for the detached collector.
"""
import os
from pathlib import Path
import subprocess
import tempfile
import unittest

ROOT = Path(__file__).resolve().parent.parent
SOURCE = (ROOT / "scripts/e2e-sync.sh").read_text()
CLEANUP = SOURCE.split("# BEGIN fixture job cleanup", 1)[1].split("\n", 1)[1].split("# END fixture job cleanup.", 1)[0]


class SuiteGateTest(unittest.TestCase):
    def test_server_readiness_gates_authentication(self):
        readiness = SOURCE.split('SRV_PID=$!\n', 1)[1].split('\nJ="$TMP/a.jar"', 1)[0]
        for scenario in ("ready", "delayed", "unavailable", "exited"):
            with self.subTest(scenario=scenario), tempfile.TemporaryDirectory() as tmp:
                (Path(tmp) / "srv.log").write_text("synthetic server diagnostic\n")
                script = '''set -u
SRV_PID=77; B=http://fixture.invalid; attempts=0
kill() { [ "$SCENARIO" != exited ]; }
curl() {
  attempts=$((attempts + 1))
  [ "$SCENARIO" = ready ] || { [ "$SCENARIO" = delayed ] && [ "$attempts" = 3 ]; }
}
sleep() { :; }
''' + readiness + '\nprintf "AUTH:%s\\n" "$attempts"\n'
                result = subprocess.run(["bash", "-c", script], env=dict(os.environ, TMP=tmp, SCENARIO=scenario),
                                        capture_output=True, text=True, timeout=2)
                success = scenario in ("ready", "delayed")
                self.assertEqual(result.returncode, 0 if success else 1, result.stdout + result.stderr)
                self.assertEqual("AUTH:" in result.stdout, success)
                if success:
                    self.assertIn("AUTH:" + ("1" if scenario == "ready" else "3"), result.stdout)
                else:
                    self.assertIn("server did not become ready", result.stderr)
                    self.assertIn("synthetic server diagnostic", result.stderr)

    def test_cleanup_joins_only_direct_shell_jobs(self):
        with tempfile.TemporaryDirectory() as tmp:
            env = dict(os.environ, TMP=tmp)
            outsider = subprocess.Popen(["sleep", "10"])
            try:
                script = 'set -u\nFAIL=0\n' + CLEANUP + '''
sleep 10 & owned=$!
finish_fixture_job "$owned" || exit 1
fixture_job_active "$owned" && exit 2
(exit 0) & completed=$!
sleep 0.05
finish_fixture_job "$completed" || exit 3
finish_fixture_job "$1" || exit 4
'''
                result = subprocess.run(["bash", "-c", script, "cleanup-test", str(outsider.pid)], env=env, capture_output=True, text=True, timeout=5)
                self.assertEqual(result.returncode, 0, result.stderr)
                self.assertIsNone(outsider.poll(), "non-child PID was signalled")
            finally:
                outsider.terminate()
                outsider.wait(timeout=2)

    def test_unfinished_cleanup_preserves_fixture_and_reports_failure(self):
        with tempfile.TemporaryDirectory() as tmp:
            # Synthetic job inventory/time; no signal is sent to an OS process.
            script = 'set -u\nFAIL=0\nSECONDS=0\n' + CLEANUP + '''
jobs() { echo 77; }
kill() { echo called >> "$TMP/signals"; }
sleep() { SECONDS=31; }
if finish_fixture_job 77; then exit 1; fi
if finish_fixture_job 77; then exit 2; fi
[ "$FAIL" = 1 ] && [ "$CXT_E2E_KEEP_TMP" = 1 ]
'''
            result = subprocess.run(["bash", "-c", script], env=dict(os.environ, TMP=tmp), capture_output=True, text=True, timeout=5)
            self.assertEqual(result.returncode, 0, result.stderr)
            self.assertIn("cleanup incomplete", (Path(tmp) / "fixture-cleanup.out").read_text())
            self.assertEqual((Path(tmp) / "signals").read_text().splitlines(), ["called"])

    def test_exit_cleanup_failure_cannot_report_success(self):
        cleanup = SOURCE[SOURCE.index('cleanup() {'):SOURCE.index('trap cleanup EXIT')]
        with tempfile.TemporaryDirectory() as tmp:
            script = '''set -u
FAIL=0; LPID=77; SRV_PID=""
finish_fixture_job() { FAIL=1; CXT_E2E_KEEP_TMP=1; return 1; }
''' + cleanup + '\ntrap cleanup EXIT\nexit 0\n'
            result = subprocess.run(["bash", "-c", script], env=dict(os.environ, TMP=tmp, CXT_E2E_DIAGNOSTICS_DIR=""), capture_output=True, text=True, timeout=2)
            self.assertEqual(result.returncode, 1, result.stderr)
            self.assertTrue(Path(tmp).is_dir())

    def test_a_failed_expectation_fails_the_suite_and_keeps_the_fixture(self):
        cleanup = SOURCE[SOURCE.index('cleanup() {'):SOURCE.index('trap cleanup EXIT')]
        expect = SOURCE[SOURCE.index('expect() {'):SOURCE.index('jget() {')]
        with tempfile.TemporaryDirectory() as tmp:
            script = 'set -u\nFAIL=0; LPID=""; SRV_PID=""\nfinish_fixture_job() { return 0; }\n' + cleanup + '\ntrap cleanup EXIT\n' + expect + '''
expect "one thing holds" yes yes
expect "another does not" no yes
exit "$FAIL"
'''
            result = subprocess.run(["bash", "-c", script], env=dict(os.environ, TMP=tmp, CXT_E2E_DIAGNOSTICS_DIR=""), capture_output=True, text=True, timeout=2)
            self.assertEqual(result.returncode, 1, result.stdout + result.stderr)
            self.assertIn("✓ one thing holds", result.stdout)
            self.assertIn("✗ another does not  (got=no want=yes)", result.stdout)
            self.assertIn("fixture preserved: " + tmp, result.stdout)
            self.assertTrue(Path(tmp).is_dir())

    def test_waiting_for_the_collector_polls_and_gives_up(self):
        waiting = SOURCE[SOURCE.index('until_true() {'):SOURCE.index('echo "── build(isolated bin)')]
        script = 'set -u\n' + waiting + '''
tries=0
third() { tries=$((tries + 1)); [ "$tries" -ge 3 ]; }
sleep() { :; }
until_true 5 third || exit 1
[ "$tries" = 3 ] || exit 2
# A condition that never holds ends at the deadline, without success.
sleep() { SECONDS=$((SECONDS + 1)); }
if until_true 2 false; then exit 3; fi
'''
        result = subprocess.run(["bash", "-c", script], capture_output=True, text=True, timeout=5)
        self.assertEqual(result.returncode, 0, result.stderr)

    def test_the_suite_never_runs_the_collector_itself_where_a_hook_should(self):
        # The point of the suite is the chain from a real hook. The collector
        # is called directly only to show that it does nothing where cxt was
        # not set up.
        direct = [line for line in SOURCE.splitlines() if "cxt git-hook segment-collect" in line and not line.lstrip().startswith("#")]
        self.assertEqual(direct, ["cxt git-hook post-commit; cxt git-hook segment-collect"])


if __name__ == "__main__":
    unittest.main()
