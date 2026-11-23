#!/usr/bin/env python3
"""The suite wakes one collector at a time.

The CI workflow runs this file by name. The helper it once tested held
capture publications back while a fixture was measured; it was removed with
capture. Its concern is still there in another form: a wake that arrives
while a collector runs is answered by one more run a minute later, which would
make a suite that does not wait both slow and dependent on timing. This checks
that every step of scripts/e2e-sync.sh that wakes the collector first waits
for the one before it.
"""
from pathlib import Path
import re
import unittest

ROOT = Path(__file__).resolve().parent.parent
SUITE = (ROOT / "scripts/e2e-sync.sh").read_text()
BODY = SUITE.split('echo "── B. git commit', 1)[1].split('echo "── G. a folder that was not set up', 1)[0]

# What runs a hook of cxt's in the suite's repository.
WAKES = re.compile(r'(?:^|\(\s*cd "\$TMP/linked" && |&& )(git commit|commit_at|git checkout|git worktree add|git branch [a-z]|printf .*\| cxt hook )')


def steps(body):
    """Every line that wakes the collector, with what stands between it and the wake before."""
    found, since = [], []
    for line in body.splitlines():
        text = line.strip()
        if not text or text.startswith("#"):
            continue
        if WAKES.search(text):
            found.append((text, list(since)))
            since = []
        else:
            since.append(text)
    return found


class OneCollectorAtATimeTest(unittest.TestCase):
    def test_the_matcher_finds_the_wakes(self):
        sample = 'quiet\ngit commit -q --allow-empty -m "x"\nexpect "y" "$(revision z)" 2\n( cd "$TMP/linked" && commit_at "$T2" "w" )\n'
        found = steps(sample)
        self.assertEqual([wake for wake, _ in found], ['git commit -q --allow-empty -m "x"', '( cd "$TMP/linked" && commit_at "$T2" "w" )'])
        self.assertEqual(found[1][1], ['expect "y" "$(revision z)" 2'])

    def test_every_wake_waits_for_the_collector_before_it(self):
        found = steps(BODY)
        self.assertGreaterEqual(len(found), 8, found)
        allowed_without_waiting = (
            # The first commit: no collector has run yet.
            'commit_at "$T1" "first with cxt"',
            # Turned off: no collector is started, which is what the step shows.
            'git commit -q --allow-empty -m "while turned off"',
            # No cxt can be run at all.
            'git commit -q --allow-empty -m "without cxt" 2>"$TMP/without-cxt.err"',
        )
        for wake, since in found:
            if wake in allowed_without_waiting:
                continue
            self.assertIn("quiet", since, wake)

    def test_quiet_waits_and_then_lets_the_next_wake_run_at_once(self):
        quiet = SUITE.split("quiet() {", 1)[1].split("}", 1)[0]
        self.assertLess(quiet.index("collector_idle"), quiet.index("rewind"))
        rewind = SUITE.split("rewind() {", 1)[1].split("\n}\n", 1)[0]
        self.assertIn('state["last_run"]["at"]', rewind)

    def test_the_wait_is_longer_than_the_collector_spacing(self):
        collector = (ROOT / "cli/internal/adapters/delivery/cli/segments.go").read_text()
        spacing = int(re.search(r"segmentCollectorInterval = (\d+) \* time\.Second", collector).group(1))
        waited = int(re.search(r"quiet\(\) \{ until_true (\d+) collector_idle", SUITE).group(1))
        # One run that waits out the spacing, and one more that was asked for meanwhile.
        self.assertGreater(waited, 2 * spacing)


if __name__ == "__main__":
    unittest.main()
