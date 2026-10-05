#!/usr/bin/env python3
"""Exercise the actual shell gate and cleanup using disposable marker stubs."""
import os
from pathlib import Path
import subprocess
import tempfile
import unittest

ROOT = Path(__file__).resolve().parent.parent
SOURCE = (ROOT / "scripts/e2e-sync.sh").read_text()
GATE = SOURCE.split("# BEGIN wrapper prerequisite gate", 1)[1].split("\n", 1)[1].split("# END wrapper prerequisite gate.", 1)[0]
CLEANUP = SOURCE.split("# BEGIN fixture job cleanup", 1)[1].split("\n", 1)[1].split("# END fixture job cleanup.", 1)[0]
HOLDER = SOURCE.split("# BEGIN wrapper holder readiness.\n", 1)[1].split("# END wrapper holder readiness.", 1)[0]


class WrapperGateTest(unittest.TestCase):
    def test_bad_helper_results_never_execute_dependent_actions(self):
        with tempfile.TemporaryDirectory() as tmp:
            base = Path(tmp)
            seed = base / "seed"
            valid = f"{seed}\tseed-id\tresume-id\n"
            cases = [(1, valid), (0, ""), (0, f"{seed}\n"), (0, f"{seed}\tid\n"),
                     (0, f"{seed}\t\tid\tid\n"), (0, "\tid\tid\n"),
                     (0, f"{seed}\tid\tid\textra\n"), (0, valid + "extra line\n"),
                     (0, valid.rstrip("\n"))]
            # The final valid control proves the source gate is not vacuous.
            for status, output, entered in [(s, o, False) for s, o in cases] + [(0, valid, True)]:
                with self.subTest(status=status, output=output):
                    seed.write_text("original")
                    marker = base / "dependent-actions"
                    marker.unlink(missing_ok=True)
                    env = dict(os.environ, ROOT=str(ROOT), TMP=tmp, APP_SESSION_ID="original-id",
                               APP_SESSION=str(base / "app.jsonl"), CLAUDELOG=str(base / "agent.log"),
                               CLAUDE_MEMORY_OVERRIDE=str(base / "memory"), PROOF_STATUS=str(status),
                               PROOF_OUTPUT=output)
                    script = '''set -u
FAIL=0
mark() { echo called >> "$TMP/dependent-actions"; }
python3() {
  if [ "$1" = "$ROOT/scripts/e2e-wrapper-proof.py" ]; then
    printf '%s' "$PROOF_OUTPUT"; return "$PROOF_STATUS"
  fi
  mark; return 1
}
expect() { mark; }
git() { mark; }
cxt() { mark; }
ref_target() { mark; }
birth_field() { mark; }
fixture_job_active() { return 1; }
finish_fixture_job() { wait "$1" 2>/dev/null || true; }
''' + GATE + '\nprintf "RESULT:%s\\n" "$FAIL"\n'
                    result = subprocess.run(["bash", "-c", script], cwd=tmp, env=env, capture_output=True, text=True, timeout=5)
                    self.assertEqual(result.returncode, 0, result.stderr)
                    self.assertEqual(marker.exists(), entered, result.stdout + result.stderr)
                    if not entered:
                        self.assertEqual(seed.read_text(), "original")
                        self.assertIn("RESULT:1", result.stdout)
                        self.assertIn("dependent assertions not evaluated", result.stdout)

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

    def test_enforcement_requires_a_live_holder_that_opened_the_target(self):
        for scenario in ("missing", "term", "no-term", "expired", "delayed-ack"):
            with self.subTest(scenario=scenario), tempfile.TemporaryDirectory() as tmp:
                app = Path(tmp) / "app.jsonl"
                if scenario != "missing":
                    app.with_suffix('.jsonl.superseded').write_text("synthetic")
                holder = HOLDER
                if scenario == "expired":
                    holder = holder.replace('time.sleep(30)', 'time.sleep(0.5)')
                if scenario == "delayed-ack":
                    holder = holder.replace('        ready.write(str(os.getpid()))', '        time.sleep(0.08)\n        ready.write(str(os.getpid()))')
                action = 'kill -TERM "$TPID"' if scenario in ("missing", "term", "delayed-ack") else ('sleep 0.6' if scenario == "expired" else ':')
                script = 'set -u\nFAIL=0\n' + CLEANUP + '''
expect() { echo "holder=$2"; [ "$2" = "$3" ] || FAIL=1; }
cxt() { echo called > "$TMP/enforced"; ''' + action + '; }\n' + holder + '\nexit "$FAIL"\n'
                result = subprocess.run(["bash", "-c", script], env=dict(os.environ, TMP=tmp, APP_SESSION=str(app)), capture_output=True, text=True, timeout=5)
                self.assertEqual(result.returncode, 0 if scenario in ("term", "delayed-ack") else 1, result.stdout + result.stderr)
                self.assertEqual((Path(tmp) / "enforced").exists(), scenario != "missing")
                if scenario == "expired":
                    self.assertIn('holder=0', result.stdout)

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
            self.assertIn("cleanup incomplete", (Path(tmp) / "wrapper-observation.out").read_text())
            self.assertEqual((Path(tmp) / "signals").read_text().splitlines(), ["called"])

    def test_cleanup_failure_aborts_at_actual_call_sites_and_retains_handles(self):
        for variable in ("TPID", "WPID"):
            start = SOURCE.index(f'if ! finish_fixture_job "${variable}";')
            callsite = SOURCE[start:SOURCE.index(f'{variable}=""', start) + len(variable) + 3]
            for code in (0, 1):
                with self.subTest(variable=variable, code=code):
                    script = f'''TPID=77
WPID=88
finish_fixture_job() {{ return {code}; }}
trap 'printf "handles:%s:%s\\n" "$TPID" "$WPID"' EXIT
''' + callsite + '\necho continued\n'
                    result = subprocess.run(["bash", "-c", script], capture_output=True, text=True, timeout=2)
                    self.assertEqual(result.returncode, code, result.stderr)
                    self.assertEqual('continued' in result.stdout, code == 0)
                    if code:
                        self.assertIn('handles:77:88', result.stdout)

    def test_exit_cleanup_failure_cannot_report_success(self):
        cleanup = SOURCE[SOURCE.index('cleanup() {'):SOURCE.index('trap cleanup EXIT')]
        with tempfile.TemporaryDirectory() as tmp:
            script = '''set -u
FAIL=0; WPID=77; TPID=""; SRV_PID=""
finish_fixture_job() { FAIL=1; CXT_E2E_KEEP_TMP=1; return 1; }
''' + cleanup + '\ntrap cleanup EXIT\nexit 0\n'
            result = subprocess.run(["bash", "-c", script], env=dict(os.environ, TMP=tmp, CXT_E2E_DIAGNOSTICS_DIR=""), capture_output=True, text=True, timeout=2)
            self.assertEqual(result.returncode, 1, result.stderr)
            self.assertTrue(Path(tmp).is_dir())


if __name__ == "__main__":
    unittest.main()
