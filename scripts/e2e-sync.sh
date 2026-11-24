#!/usr/bin/env bash
# cxthub hook-to-server E2E — real cxtd and cxt binaries, real Git hooks.
#
# What a hook does is wake a detached collector, which reads the session
# records of the agent tools and Git and uploads what changed. This suite runs
# that whole chain, and nothing in it calls the collector directly:
#
#   A. cxt setup            → the mark, five Git hooks, agent hooks, a registered repository
#   B. git commit           → post-commit hook → collector → the session is on the server
#   C. a session goes on    → an agent hook wakes the collector → the report grows
#   D. a linked worktree    → its commit is collected through the shared hooks
#   E. hooks of an earlier version → replaced the first time one runs; Git is never stopped
#   F. segments.auto off    → nothing is sent, and cxt doctor says why
#   G. a folder not set up  → hooks leave it alone
#   H. removed commands     → unknown commands, not silent successes
#
# Run with isolated TMP, HOME, and a randomized port; no local state is retained.
# CXT_E2E_DSN=<disposable PostgreSQL DSN> exercises production storage instead of FS.
set -u

# A fixture must not claim the host desktop app's real command session.
unset CODEX_THREAD_ID CODEX_SESSION_ID

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
TMP="$(mktemp -d)"
PORT=$((19100 + RANDOM % 800))
B="http://127.0.0.1:$PORT/api/v1"
ORIGIN="http://127.0.0.1:$PORT"
FAIL=0
SRV_PID=""
LPID=""

# BEGIN fixture job cleanup (also exercised without launching cxt).
fixture_job_active() {
  local wanted="$1" candidate
  for candidate in $(jobs -pr; jobs -ps); do
    [ "$candidate" = "$wanted" ] && return 0
  done
  return 1
}
# Only a running job started by this shell may be signalled. Never trust PID files.
finish_fixture_job() {
  local owned_pid="$1" running end=$((SECONDS + 30))
  [ -n "$owned_pid" ] || return 0
  case " ${CLEANUP_UNFINISHED:-} " in *" $owned_pid "*) return 1;; esac
  for running in $(jobs -pr); do
    if [ "$running" = "$owned_pid" ]; then kill -TERM "$owned_pid" 2>/dev/null || true; fi
  done
  while fixture_job_active "$owned_pid"; do
    if [ "$SECONDS" -ge "$end" ]; then
      echo "fixture cleanup incomplete: owned pid=$owned_pid did not exit" >>"$TMP/fixture-cleanup.out"
      CLEANUP_UNFINISHED="${CLEANUP_UNFINISHED:-} $owned_pid"
      FAIL=1; CXT_E2E_KEEP_TMP=1; return 1
    fi
    sleep 0.05
  done
  wait "$owned_pid" 2>/dev/null || true
}
# END fixture job cleanup.
# Reap the server immediately after killing it to avoid shell "Terminated" noise.
# Preserve the isolated fixture on demand so hook diagnostics are still
# inspectable after the script exits.
cleanup() {
  local test_status=$?
  if ! finish_fixture_job "$LPID"; then
    [ "$test_status" -ne 0 ] || test_status=1
  fi
  if [ "$FAIL" -ne 0 ] && [ "$test_status" -eq 0 ]; then test_status=1; fi
  if [ "$test_status" -ne 0 ] || [ "$FAIL" -ne 0 ]; then CXT_E2E_KEEP_TMP=1; fi
  if { [ "$test_status" -ne 0 ] || [ "$FAIL" -ne 0 ]; } && [ -n "${CXT_E2E_DIAGNOSTICS_DIR:-}" ]; then
    python3 "$ROOT/scripts/e2e-collect-diagnostics.py" "$TMP" "$CXT_E2E_DIAGNOSTICS_DIR" || echo "Could not collect E2E failure diagnostics" >&2
  fi
  [ -n "$SRV_PID" ] && kill "$SRV_PID" 2>/dev/null && wait "$SRV_PID" 2>/dev/null
  if [ "${CXT_E2E_KEEP_TMP:-0}" = 1 ]; then
    echo "SYNC E2E fixture preserved: $TMP"
  else
    rm -rf "$TMP"
  fi
  exit "$test_status"
}
trap cleanup EXIT

expect() { if [ "$2" = "$3" ]; then echo "  ✓ $1"; else echo "  ✗ $1  (got=$2 want=$3)"; FAIL=1; fi; }
jget() { python3 -c "import json,sys;print(json.load(sys.stdin)$1)"; }
ccurl() { command curl -H "Origin: $ORIGIN" -H 'X-Cxt-CSRF: 1' "$@"; }

# until_true <seconds> <command...> — poll instead of sleeping a fixed time: the
# collector is detached from the hook that woke it.
until_true() {
  local end=$((SECONDS + $1)); shift
  while [ "$SECONDS" -lt "$end" ]; do
    if "$@" >/dev/null 2>&1; then return 0; fi
    sleep 0.2
  done
  return 1
}

echo "── build(isolated bin) · server start :$PORT"
server_build=(go build -o "$TMP/bin/cxtd")
if [ -n "${CXT_E2E_DSN:-}" ]; then server_build+=(-tags postgres); fi
( cd "$ROOT/backend" && "${server_build[@]}" ./cmd/cxtd ) || { echo "cxtd build failed"; exit 1; }
if [ -n "${CXT_E2E_DSN:-}" ]; then
  ( cd "$ROOT/backend" && go build -tags postgres -o "$TMP/bin/cxt-admin" ./cmd/cxt-admin ) || { echo "cxt-admin build failed"; exit 1; }
fi
( cd "$ROOT/cli" && go build -o "$TMP/bin/cxt" ./cmd/cxt ) || { echo "cxt build failed"; exit 1; }
export PATH="$TMP/bin:$PATH"
export HOME="$TMP/home"; mkdir -p "$HOME"
export XDG_CACHE_HOME="$HOME/.cache"
unset CXT_REMOTE CXT_TOKEN
git config --global user.email e2e@test.local
git config --global user.name E2E
git config --global init.defaultBranch main

if [ -n "${CXT_E2E_DSN:-}" ]; then
  CXT_ENV_FILE=/dev/null CXT_POSTGRES_DSN="$CXT_E2E_DSN" \
    CXT_MIGRATIONS_DIR="$ROOT/schemas/db/migrations" \
    "$TMP/bin/cxt-admin" migrate --apply || { echo "fixture migration setup failed" >&2; exit 1; }
fi

fixture_server=(env CXT_ENV_FILE=/dev/null CXT_AUTH=dev CXT_POSTGRES_DSN="${CXT_E2E_DSN:-}" \
  CXT_MIGRATIONS_DIR="$ROOT/schemas/db/migrations" CXT_PUBLIC_URL="$ORIGIN" \
  CXT_COOKIE_SECURE=0 CXT_COOKIE_DOMAIN= CXT_CORS_ORIGINS="$ORIGIN" \
  RESEND_API_KEY= CXT_GITHUB_TOKEN= \
  "$TMP/bin/cxtd" serve --addr 127.0.0.1:$PORT --data "$TMP/data")
"${fixture_server[@]}" >"$TMP/srv.log" 2>&1 &
SRV_PID=$!
SERVER_READY=0
for i in $(seq 1 30); do
  kill -0 "$SRV_PID" 2>/dev/null || break
  if curl -sf -o /dev/null "$B/repos"; then SERVER_READY=1; break; fi
  sleep 0.3
done
if [ "$SERVER_READY" != 1 ]; then
  echo "fixture server did not become ready; authentication was not attempted" >&2
  tail -20 "$TMP/srv.log" >&2
  exit 1
fi

J="$TMP/a.jar"
SUFFIX="$(date +%s)$RANDOM"
ccurl -s -c "$J" -X POST "$B/auth/session" -H "Authorization: Bearer dev:sync$SUFFIX@t.io:Sync" >/dev/null
SLUG=$(ccurl -sb "$J" -X POST "$B/repositories" -H 'Content-Type: application/json' -d "{\"name\":\"Sync$SUFFIX\"}" | jget "['slug']")
OWN=$(curl -sb "$J" "$B/me" | jget "['username']")
REMOTE="$ORIGIN/$OWN/$SLUG"
git init -q --bare "$TMP/bare.git"

project_dir() { echo "$HOME/.claude/projects/$(python3 -c "import re,sys;print(re.sub(r'[^A-Za-z0-9]','-',sys.argv[1]))" "$1")"; }
# at <minutes> [seconds] — a moment that many minutes and seconds from now.
at() { python3 -c 'import datetime,sys;print((datetime.datetime.now(datetime.timezone.utc).replace(microsecond=0)+datetime.timedelta(minutes=int(sys.argv[1]),seconds=int(sys.argv[2]))).strftime("%Y-%m-%dT%H:%M:%SZ"))' "$1" "${2:-0}"; }
# commit_at <moment> <message> — a commit made at that moment, through the real hooks.
commit_at() { GIT_AUTHOR_DATE="$1" GIT_COMMITTER_DATE="$1" git commit -q --allow-empty -m "$2"; }
# claude_session <cwd> <session> <branch> <commit> <moment of the commit> — a
# synthetic Claude Code session whose command ran while that commit was made
# and printed its number.
claude_session() {
  mkdir -p "$(project_dir "$1")"
  python3 - "$1" "$2" "$3" "$4" "$5" >"$(project_dir "$1")/$2.jsonl" <<'PYSESSION'
import datetime, json, sys
cwd, session, branch, sha, committed = sys.argv[1:6]
start = datetime.datetime.strptime(committed, "%Y-%m-%dT%H:%M:%SZ").replace(tzinfo=datetime.timezone.utc) - datetime.timedelta(seconds=5)
def line(kind, uuid, second, content, prompt=None):
    at = (start + datetime.timedelta(seconds=second)).strftime("%Y-%m-%dT%H:%M:%S.000Z")
    entry = {"type": kind, "uuid": uuid, "sessionId": session, "timestamp": at, "cwd": cwd, "gitBranch": branch, "message": {"content": content}}
    if prompt:
        entry["promptId"] = prompt
    print(json.dumps(entry))
# Records are told apart by their identifiers: a record with an identifier
# another session already holds is a copy, and counts for that session only.
line("user", session + "-m1", 0, "commit it", session + "-p1")
line("assistant", session + "-m2", 1, [{"type": "tool_use", "id": session + "-call-1", "name": "Bash"}])
line("user", session + "-m3", 6, [{"type": "tool_result", "tool_use_id": session + "-call-1", "content": "[%s %s] synthetic commit" % (branch, sha[:7])}], session + "-p1")
line("assistant", session + "-m4", 7, [{"type": "text", "text": "committed"}])
PYSESSION
}
# session_goes_on <cwd> <session> <branch> <uuid> <moment> — one more prompt in a session.
session_goes_on() {
  printf '%s\n' '{"type":"user","uuid":"'"$4"'","sessionId":"'"$2"'","timestamp":"'"$5"'","cwd":"'"$1"'","gitBranch":"'"$3"'","promptId":"'"$4"'","message":{"content":"one more thing"}}' >>"$(project_dir "$1")/$2.jsonl"
}
on_server() { curl -fsSb "$J" "$B/repos/$RID/anchored-sessions/claude/$1"; }
revision() { on_server "$1" | jget "['revision']"; }
revision_is() { [ "$(revision "$1" 2>/dev/null)" = "$2" ]; }
# The collector spaces its runs at least ten seconds apart. The suite moves the last run
# back instead of waiting.
STATE=""
rewind() {
  [ -f "$STATE" ] && python3 - "$STATE" <<'PYREWIND'
import json, sys
state = json.load(open(sys.argv[1]))
if state.get("last_run"):
    state["last_run"]["at"] = "2026-01-01T00:00:00Z"
    json.dump(state, open(sys.argv[1], "w"))
PYREWIND
  return 0
}
collector_idle() { ! pgrep -f "git-hook segment-collect" >/dev/null 2>&1; }
# quiet — wait until no collector runs or waits, then let the next wake run at
# once. A wake that arrives while a collector runs is answered by one more run
# after the pause between runs, so the suite wakes one collector at a time.
quiet() { until_true 150 collector_idle; rewind; }

echo "── A. cxt setup: the mark, the hooks and a registered repository"
mkdir -p "$TMP/repo"; cd "$TMP/repo" || exit 1
git init -q; git remote add origin "$TMP/bare.git"
git commit -q --allow-empty -m "before cxt"
CXT_NO_BROWSER=1 cxt login --server "$ORIGIN" >"$TMP/login.out" 2>&1 &
LPID=$!
DCODE=""
for i in $(seq 1 40); do
  DCODE=$(grep -o '[B-Z2-9]\{3\}-[B-Z2-9]\{3\}' "$TMP/login.out" 2>/dev/null | head -1)
  [ -n "$DCODE" ] && break; sleep 0.25
done
expect "device flow code output" "$([ -n "$DCODE" ] && echo yes)" yes
ccurl -sb "$J" -X POST "$B/auth/device/approve" -H 'Content-Type: application/json' -d "{\"code\":\"$DCODE\"}" >/dev/null
wait "$LPID"; LPID=""
mkdir -p "$HOME/.codex"
printf '%s\n' '{"hooks":{"Stop":[{"hooks":[{"type":"command","command":"my-custom-hook","timeout":5}]}]}}' > "$HOME/.codex/hooks.json"
printf '#!/bin/sh\necho user-hook-ran >> "%s"\n' "$TMP/user-hook.log" > .git/hooks/post-commit; chmod +x .git/hooks/post-commit
cxt setup "$REMOTE" --no-login >"$TMP/setup.out" 2>&1
expect "setup succeeds" "$?" 0
expect "setup writes the mark" "$([ -f .cxt/HEAD ] && echo yes)" yes
expect "setup registers the remote" "$(grep -c 'origin' .cxt/config)" 1
expect "setup installs five Git hooks, each of which only wakes" "$(cat .git/hooks/post-commit .git/hooks/post-checkout .git/hooks/post-merge .git/hooks/post-rewrite .git/hooks/pre-push 2>/dev/null | grep -cF 'git-hook wake "${0##*/}" "$PPID" </dev/null')" 5
expect "setup installs no reference-transaction hook" "$([ ! -e .git/hooks/reference-transaction ] && echo yes)" yes
expect "setup keeps the user's own hook for chaining" "$(grep -c user-hook-ran .git/hooks/post-commit.pre-cxt)" 1
expect "setup merges the Claude hooks" "$(grep -c 'cxt hook --provider claude --event UserPromptSubmit' .claude/settings.json 2>/dev/null)" 1
expect "setup merges the Codex hooks and keeps the user's" "$(grep -c 'cxt hook --provider codex --event Stop' "$HOME/.codex/hooks.json") $(grep -c 'my-custom-hook' "$HOME/.codex/hooks.json")" "1 1"
RID="$(curl -fsSb "$J" "$B/repos?repository=$(curl -fsSb "$J" "$B/repositories" | jget "[0]['id']")" | jget "[0]['id']")"
expect "the repository is registered on the server" "$([ -n "$RID" ] && echo yes)" yes
expect "setup uploads nothing by itself" "$(curl -fsSb "$J" "$B/repos/$RID/anchored-sessions" | python3 -c 'import json,sys;print(len(json.load(sys.stdin) or []))')" 0
H1=$(cat "$HOME/.codex/hooks.json" .claude/settings.json .git/hooks/post-commit | shasum)
cxt setup --no-login >"$TMP/setup2.out" 2>&1
expect "setup again changes nothing and says so" "$(cat "$HOME/.codex/hooks.json" .claude/settings.json .git/hooks/post-commit | shasum) $(grep -c 'already registered' "$TMP/setup2.out")" "$H1 2"
STATE="$(git rev-parse --absolute-git-dir)/cxt/collector-v1.json"

echo "── B. git commit: the hook wakes the collector, and the session is on the server"
# The session below is recorded after automatic reading begins, which is now.
T1="$(at 2)"
commit_at "$T1" "first with cxt"
until_true 20 test -f "$STATE"
expect "the commit ran the user's hook and then woke the collector" "$(grep -c user-hook-ran "$TMP/user-hook.log") $([ -f "$STATE" ] && echo woke)" "1 woke"
SHA1="$(git rev-parse HEAD)"
claude_session "$PWD" sync-one main "$SHA1" "$T1"
quiet
git commit -q --allow-empty -m "second with cxt"
expect "a session recorded after setup is uploaded without being asked" "$(until_true 30 on_server sync-one && on_server sync-one | python3 -c 'import json,sys;r=json.load(sys.stdin);b=r["body"];print(r["revision"],len(b["segments"]),b["segments"][0]["commits"][0]["rule"],bool(b.get("content")))')" "1 1 printed-number True"
expect "the commit leads to the session" "$(curl -fsSb "$J" "$B/repos/$RID/anchored-commits/$SHA1" | python3 -c 'import json,sys;r=json.load(sys.stdin);print(len(r),r[0]["session_id"])')" "1 sync-one"
# The conversation that was uploaded is read back and searched through the
# server, with nothing but the worker that finishes an upload running.
expect "the uploaded conversation is read back through the session" "$(curl -fsSb "$J" "$B/repos/$RID/anchored-sessions/claude/sync-one/events?offset=0" | python3 -c 'import json,sys;r=json.load(sys.stdin);t=json.dumps(r["events"]);print(len(r["events"])>0, "commit it" in t, "committed" in t)')" "True True True"
expect "a word of the conversation is found by the search" "$(until_true 20 sh -c 'curl -fsSb "$0" "$1/repos/$2/anchored-search?q=committed" | grep -q "\"segment\"" ' "$J" "$B" "$RID"; curl -fsSb "$J" "$B/repos/$RID/anchored-search?q=committed" | python3 -c 'import json,sys;r=json.load(sys.stdin);print(len([h for h in r["hits"] if h["kind"]=="segment"])>0)')" "True"
quiet
expect "a copy of what was sent is kept under .cxt/sessions, marked automatic" "$(cxt log claude sync-one --json | python3 -c 'import json,sys;l=json.load(sys.stdin)["sessions"][0]["log"];print(len(l)>0, all(e.get("automatic") for e in l))')" "True True"
expect "the diagnosis finds nothing that does not fit" "$(cxt doctor --json | python3 -c 'import json,sys;print(len(json.load(sys.stdin).get("findings") or []))')" 0

echo "── C. an agent hook wakes the collector too"
session_goes_on "$PWD" sync-one main m5 "$(at 3)"
quiet
printf '%s' "{\"session_id\":\"sync-one\",\"cwd\":\"$PWD\"}" | cxt hook --provider claude --event Stop
expect "the agent hook succeeds" "$?" 0
expect "the session that went on is uploaded again" "$(until_true 30 revision_is sync-one 2 && revision sync-one)" 2

echo "── C2. a push wakes the collector when it has ended, and that run asks the server about the Git host"
# Git runs pre-push before the push and has no hook after it. The hook names the Git process that runs it, the one
# that pushes, and the collector is woken when that process has ended: its run then finds origin's branches changed
# in this clone, and asks the server to read the Git host. Without being asked the server reads it only every few
# hours. The origin here takes three seconds to receive a push, and no collector runs in that time. This server
# knows no Git host for the repository and says so; the collector remembers the branches it asked about either way.
# The commit pushed is no session's, so nothing else changes.
asked_remote_read() { python3 -c 'import json,sys;sys.exit(0 if json.load(open(sys.argv[1])).get("origin_branches") else 1)' "$STATE" 2>/dev/null; }
last_run_at() { python3 -c 'import json,sys;print(json.load(open(sys.argv[1]))["last_run"]["at"])' "$STATE" 2>/dev/null; }
printf '#!/bin/sh\nsleep 3\n' > "$TMP/bare.git/hooks/pre-receive"; chmod +x "$TMP/bare.git/hooks/pre-receive"
PROBE="$(git commit-tree "$(git rev-parse 'HEAD^{tree}')" -m "no session made this" </dev/null)"
quiet
REWOUND="$(last_run_at)"
expect "before any push the server was not asked" "$(asked_remote_read && echo asked || echo not asked)" "not asked"
git push -q origin "$PROBE:refs/heads/probe" 2>"$TMP/push.err" &
PUSHPID=$!
sleep 1.5
expect "while the push goes on no collector runs" "$([ -n "$REWOUND" ] && [ "$(last_run_at)" = "$REWOUND" ] && echo did not run) $(asked_remote_read && echo asked || echo not asked)" "did not run not asked"
wait "$PUSHPID"
expect "the push goes through the hook" "$?" 0
expect "when the push has ended the collector runs and asks the server about the Git host" "$(until_true 40 asked_remote_read && echo asked) $([ "$(last_run_at)" != "$REWOUND" ] && echo ran)" "asked ran"
rm -f "$TMP/bare.git/hooks/pre-receive"

echo "── C3. the collector tells the server which pushes this clone made, and the server says where a push came from"
# Git records in the clone each push the clone made. The run after the push reads that record and tells the server,
# with the collector's own state: the server can then say of a push that came without a session that it was made on
# a computer where cxt runs, and that the cxt there ended its run well. The commit pushed above is no session's.
origin_of() { curl -fsSb "$J" "$B/repos/$RID/push-origins" | python3 -c '
import json, sys
view = json.load(sys.stdin)
push = next((p for p in view["pushes"] if p["sha"] == sys.argv[1] and p["branch"] == sys.argv[2]), None)
if push is None:
    sys.exit(1)
collector = view["collectors"][push["collector"]] if "collector" in push else {}
print(push["state"], "claimed" if "claimed_at" in push else "unclaimed", "own" if collector.get("own") else "other", "failing" if collector.get("failing") else "well")' "$1" "$2" 2>/dev/null; }
expect "the push is said to come from a computer where cxt runs and ended its run well" "$(until_true 40 origin_of "$PROBE" probe >/dev/null && origin_of "$PROBE" probe)" "cxt claimed own well"
heard_at() { curl -fsSb "$J" "$B/repos/$RID/push-origins" | python3 -c 'import json,sys;print(next(c["heard_at"] for c in json.load(sys.stdin)["collectors"] if c["own"]))' 2>/dev/null; }
HEARD="$(heard_at)"
quiet
REWOUND="$(last_run_at)"
ran_again() { [ "$(last_run_at)" != "$REWOUND" ]; }
cxt git-hook wake post-commit </dev/null >/dev/null 2>&1
expect "a run with nothing new tells the server nothing more" "$(until_true 40 ran_again && echo ran) $([ -n "$HEARD" ] && [ "$(heard_at)" = "$HEARD" ] && echo not told)" "ran not told"

echo "── D. a linked worktree shares the hooks"
quiet
git worktree add -q "$TMP/linked" -b topic
T2="$(at 4)"
quiet
( cd "$TMP/linked" && commit_at "$T2" "in the worktree" )
SHA2="$(git -C "$TMP/linked" rev-parse HEAD)"
claude_session "$TMP/linked" sync-two topic "$SHA2" "$T2"
quiet
( cd "$TMP/linked" && git commit -q --allow-empty -m "again in the worktree" )
expect "a session of the linked worktree is uploaded" "$(until_true 30 on_server sync-two && on_server sync-two | python3 -c 'import json,sys;b=json.load(sys.stdin)["body"];print(len(b["segments"]),b["segments"][0]["commits"][0]["sha"]=="'"$SHA2"'")')" "1 True"
expect "nothing of cxt is written into the linked worktree" "$([ ! -e "$TMP/linked/.cxt" ] && echo yes)" yes

echo "── E. hooks of an earlier version are replaced, and Git is never stopped"
# What an earlier version installed: a reference-transaction hook that refused
# to create a branch when its cxt call failed, calling a command that is gone.
{
  printf '%s\n' '#!/bin/sh' "# cxt-hook v1 (managed by cxt — remove with 'cxt hooks uninstall')" 'input=$(cat)' 'case "$input" in' '*refs/heads/*)'
  printf '  printf '"'"'%%s\\n'"'"' "$input" | "%s" git-hook branch-transaction "$1" "$PPID"\n' "$TMP/bin/cxt"
  printf '%s\n' '  cxt_status=$?' '  if [ "$1" = "prepared" ] && [ "$cxt_status" -ne 0 ]; then exit "$cxt_status"; fi ;;' 'esac' 'exit 0'
} > .git/hooks/reference-transaction
chmod +x .git/hooks/reference-transaction
printf '#!/bin/sh\n# cxt-hook v1 (managed by cxt)\n"%s" git-hook post-checkout "$@" || true\n' "$TMP/bin/cxt" > .git/hooks/post-checkout
expect "the earlier hook is valid shell" "$(sh -n .git/hooks/reference-transaction 2>&1; echo $?)" 0
quiet
git branch from-earlier-hooks >"$TMP/earlier-branch.out" 2>&1
expect "a branch is created through the earlier hook" "$? $(git show-ref --verify --quiet refs/heads/from-earlier-hooks && echo exists)" "0 exists"
expect "the earlier reference-transaction hook is gone after its first run" "$([ ! -e .git/hooks/reference-transaction ] && echo yes)" yes
expect "the earlier post-checkout hook became this version's" "$(grep -cF 'git-hook wake "${0##*/}" "$PPID" </dev/null' .git/hooks/post-checkout)" 1
# The earlier hook woke cxt more than once for one branch, so one more run follows after the pause between runs.
quiet
git checkout -q from-earlier-hooks
quiet
git checkout -q main
expect "switching branches works with the replaced hooks" "$(git symbolic-ref --short HEAD)" main
quiet
# Without any cxt to run, Git still goes on and the user's hook still runs.
USER_HOOK_RUNS="$(grep -c user-hook-ran "$TMP/user-hook.log")"
mv "$TMP/bin/cxt" "$TMP/bin/cxt.away"
sed -i.bak "s#^CXT=.*#CXT='$TMP/bin/missing'#" .git/hooks/post-commit && rm -f .git/hooks/post-commit.bak
git commit -q --allow-empty -m "without cxt" 2>"$TMP/without-cxt.err"
expect "a commit succeeds when cxt cannot be run, and the user's hook still runs" "$? $(($(grep -c user-hook-ran "$TMP/user-hook.log") - USER_HOOK_RUNS))" "0 1"
mv "$TMP/bin/cxt.away" "$TMP/bin/cxt"
cxt hooks install >/dev/null 2>&1
expect "installing again puts this version's hook back" "$(grep -c "^CXT='$TMP/bin/cxt'" .git/hooks/post-commit)" 1

echo "── F. turned off: nothing is sent, and the diagnosis says why"
quiet
cxt config segments.auto off >/dev/null 2>&1
session_goes_on "$PWD" sync-one main m6 "$(at 5)"
git commit -q --allow-empty -m "while turned off"
sleep 2
expect "turned off, a commit starts no collector and uploads nothing" "$(collector_idle && revision sync-one)" 2
expect "the diagnosis names the collector that is off and the session the server is behind on" "$(cxt doctor --json | python3 -c 'import json,sys;c=sorted({f["code"] for f in json.load(sys.stdin)["findings"]});print(" ".join(c))')" "collector-off server-behind"
expect "asked for, the upload still works" "$(cxt push claude sync-one --json | python3 -c 'import json,sys;r=json.load(sys.stdin);print(r["stored"])') $(revision sync-one)" "1 3"
cxt config segments.auto on >/dev/null 2>&1

echo "── G. a folder that was not set up is left alone"
mkdir -p "$TMP/unconnected"; cd "$TMP/unconnected" || exit 1; git init -q
printf '%s' "{\"session_id\":\"unconnected\",\"cwd\":\"$TMP/unconnected\",\"prompt\":\"continue\"}" | cxt hook --provider codex --event UserPromptSubmit
expect "an agent hook does not create .cxt where cxt was not set up" "$([ ! -e .cxt ] && echo yes)" yes
cxt git-hook post-commit; cxt git-hook segment-collect
expect "a Git hook does not create .cxt either, and nothing is collected" "$([ ! -e .cxt ] && [ ! -e .git/cxt ] && echo yes)" yes
mkdir -p "$TMP/residue"; cd "$TMP/residue" || exit 1; git init -q; mkdir -p .cxt/capture
printf '%s' "{\"session_id\":\"residue\",\"cwd\":\"$TMP/residue\",\"prompt\":\"continue\"}" | cxt hook --provider codex --event UserPromptSubmit
expect "a leftover .cxt directory is not a mark" "$([ ! -e .cxt/HEAD ] && echo yes)" yes
expect "a leftover .cxt directory is kept out of Git" "$(grep -c '^\.cxt/$' .gitignore 2>/dev/null) $(git check-ignore .cxt/probe 2>/dev/null | grep -c '^\.cxt/probe$')" "1 1"
expect "nothing is written into a leftover .cxt directory" "$(find .cxt -type f | wc -l | tr -d ' ')" 0
expect "hooks cannot be installed where cxt was not set up" "$(cxt hooks install >/dev/null 2>&1; echo $?)" 1

echo "── H. commands of the capture approach are gone"
cd "$TMP/repo" || exit 1
for removed in save status checkout pull commit stash load claude codex mcp capture sync branch tag fsck repair; do
  cxt "$removed" >"$TMP/removed.out" 2>&1
  expect "cxt $removed is an unknown command" "$? $(grep -c 'unknown command' "$TMP/removed.out")" "2 1"
done
expect "this computer keeps the mark, the connection, its lock and the copies of what was sent" "$(ls .cxt | sort | tr '\n' ' ')" "HEAD config locks sessions "
if [ -z "${CXT_E2E_DSN:-}" ]; then
  expect "the server has no routes that take or list captures" "$(curl -sb "$J" -o /dev/null -w '%{http_code}' "$B/repos/$RID/snapshots") $(curl -sb "$J" -o /dev/null -w '%{http_code}' "$B/repos/$RID/refs")" "404 404"
fi

echo
if [ "$FAIL" = 0 ]; then echo "SYNC E2E: all passed ✓"; else echo "SYNC E2E: FAILED ✗"; fi
exit "$FAIL"
