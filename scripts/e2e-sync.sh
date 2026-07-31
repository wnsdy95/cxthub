#!/usr/bin/env bash
# cxthub sync E2E — verifies append grafts, memory inheritance, session boundaries,
# and fetch-only behavior with real cxtd/cxt binaries and Git hooks.
#
# Scenarios along the context-divergence policy path:
#   A. repo1 pushes session A, memorize             (base chain + memory)
#   B. repo2 (new clone, no pull) pushes independent session B → hook append (root graft)
#   C. repo2 pulls, adopts the graft, and memorizes             → B inherits memory from A
#   D. repo2 commits new session C                              → session boundary metadata
#   E. repo1 runs post-merge                                    → fetch-only, preserving local refs
#   F. repo1 pushes after both sides moved                      → automatic rebase-graft
#
# Run with isolated TMP, HOME, and a randomized port; no local state is retained.
set -u

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
TMP="$(mktemp -d)"
PORT=$((19100 + RANDOM % 800))
B="http://127.0.0.1:$PORT/api/v1"
ORIGIN="http://127.0.0.1:$PORT"
FAIL=0
SRV_PID=""
# Reap the server immediately after killing it to avoid shell "Terminated" noise.
cleanup() { [ -n "$SRV_PID" ] && kill "$SRV_PID" 2>/dev/null && wait "$SRV_PID" 2>/dev/null; rm -rf "$TMP"; }
trap cleanup EXIT

expect() { if [ "$2" = "$3" ]; then echo "  ✓ $1"; else echo "  ✗ $1  (got=$2 want=$3)"; FAIL=1; fi; }
jget() { python3 -c "import json,sys;print(json.load(sys.stdin)$1)"; }
ccurl() { command curl -H "Origin: $ORIGIN" -H 'X-Cxt-CSRF: 1' "$@"; }
main_head() { curl -sb "$J" "$B/repos/$RID/refs" | python3 -c "import json,sys;print(next((r['target'] for r in json.load(sys.stdin) if r['kind']=='branch' and r['name']=='main'),''))"; }

echo "── build(isolated bin) · server start :$PORT"
( cd "$ROOT/backend" && go build -o "$TMP/bin/cxtd" ./cmd/cxtd ) || { echo "cxtd build failed"; exit 1; }
( cd "$ROOT/cli" && go build -o "$TMP/bin/cxt" ./cmd/cxt ) || { echo "cxt build failed"; exit 1; }
export PATH="$TMP/bin:$PATH"
export HOME="$TMP/home"; mkdir -p "$HOME"
git config --global user.email e2e@test.local
git config --global user.name E2E
git config --global init.defaultBranch main

CXT_AUTH=dev "$TMP/bin/cxtd" serve --addr 127.0.0.1:$PORT --data "$TMP/data" >"$TMP/srv.log" 2>&1 &
SRV_PID=$!
for i in $(seq 1 30); do curl -sf -o /dev/null "$B/repos" && break; sleep 0.3; done

J="$TMP/a.jar"
ccurl -s -c "$J" -X POST "$B/auth/session" -H "Authorization: Bearer dev:o@t.io|O" >/dev/null
ccurl -sb "$J" -X POST "$B/workspaces" -H 'Content-Type: application/json' -d '{"name":"SyncE2E"}' >/dev/null
OWN=$(curl -sb "$J" "$B/me" | jget "['username']")
SLUG=$(curl -sb "$J" "$B/workspaces" | jget "[0]['slug']")
REMOTE="http://127.0.0.1:$PORT/$OWN/$SLUG"  # The two-segment workspace URL is the repository identity.
git init -q --bare "$TMP/bare.git"

session() { # session <cwd> <label> — write a synthetic Claude JSONL session with model and usage.
  D="$HOME/.claude/projects/$(python3 -c "import re,sys;print(re.sub(r'[^A-Za-z0-9]','-',sys.argv[1]))" "$1")"
  mkdir -p "$D"
  cat > "$D/s-$2.jsonl" <<EOF
{"type":"user","cwd":"$1","sessionId":"sess-$2","gitBranch":"main","timestamp":"2026-07-05T00:00:00Z","message":{"role":"user","content":"task $2"}}
{"type":"assistant","cwd":"$1","sessionId":"sess-$2","gitBranch":"main","timestamp":"2026-07-05T00:00:01Z","message":{"role":"assistant","model":"claude-fable-5","content":[{"type":"text","text":"done $2"}],"usage":{"input_tokens":100,"output_tokens":10}}}
EOF
}

echo "── A. repo1: Session A push + memorize"
mkdir -p "$TMP/repo1"; cd "$TMP/repo1"
git init -q; git remote add origin "$TMP/bare.git"
cxt init >/dev/null 2>&1
cxt remote add origin "$REMOTE" >/dev/null 2>&1
CXT_NO_BROWSER=1 cxt login >"$TMP/login.out" 2>&1 &
LPID=$!
DCODE=""
for i in $(seq 1 40); do
  DCODE=$(grep -o '[B-Z2-9]\{3\}-[B-Z2-9]\{3\}' "$TMP/login.out" 2>/dev/null | head -1)
  [ -n "$DCODE" ] && break; sleep 0.25
done
expect "device flow code output" "$([ -n "$DCODE" ] && echo yes)" yes
ccurl -sb "$J" -X POST "$B/auth/device/approve" -H 'Content-Type: application/json' -d "{\"code\":\"$DCODE\"}" >/dev/null
wait "$LPID"
session "$TMP/repo1" A
echo a > f.txt; git add f.txt; git commit -qm codeA >/dev/null 2>&1
git push -q -u origin main >"$TMP/p1.out" 2>&1
RID=$(curl -sb "$J" "$B/repos" | python3 -c "import json,sys; rs=json.load(sys.stdin); print(rs[0]['id'] if rs else '')")
HEAD_A=""
[ -n "$RID" ] && HEAD_A=$(main_head)
if [ -z "$HEAD_A" ]; then
  echo "  first push diagnostics:"
  sed 's/^/    /' "$TMP/p1.out"
  tail -20 "$TMP/srv.log" | sed 's/^/    server: /'
fi
expect "repo1 push after server main exists" "$([ -n "$HEAD_A" ] && echo yes)" yes
[ -z "$HEAD_A" ] && exit 1
cxt memorize >/dev/null 2>&1
cxt push >/dev/null 2>&1

echo "── B. repo2 (without pull): independent session B push → hook auto-append"
git clone -q "$TMP/bare.git" "$TMP/repo2"
cd "$TMP/repo2"; cxt init >/dev/null 2>&1; cxt remote add origin "$REMOTE" >/dev/null 2>&1
session "$TMP/repo2" B
echo b > g.txt; git add g.txt; git commit -qm codeB >/dev/null 2>&1
git push -q origin main >"$TMP/p2.out" 2>&1
expect "hook appended output" "$(grep -c 'appended' "$TMP/p2.out")" 1
HEAD_B=$(main_head)
expect "server main advances to B head" "$([ "$HEAD_B" != "$HEAD_A" ] && [ -n "$HEAD_B" ] && echo yes)" yes
GRAFT=$(curl -sb "$J" "$B/repos/$RID/snapshots" | python3 -c "
import json,sys
snaps={s['id']:s for s in json.load(sys.stdin)}
seen=set(); q=['$HEAD_B']; result='root-unlinked'
while q:
    cur=q.pop(0)
    if not cur or cur in seen or cur not in snaps: continue
    seen.add(cur); snap=snaps[cur]
    grafts=snap.get('graft_parents') or []
    if '$HEAD_A' in grafts:
        result='linked+grafted' if snap.get('grafted') else 'linked-nomark'; break
    q.extend((snap.get('parents') or []) + grafts)
print(result)
")
expect "B lineage root grafts to A head + marker" "$GRAFT" linked+grafted

echo "── C. repo2: pull (adopt lineage) → memorize (inherit memory)"
cxt pull >/dev/null 2>&1
LOCAL=$(python3 -c "
import json,glob
print('linked' if any('$HEAD_A' in (json.load(open(p)).get('graft_parents') or []) for p in glob.glob('$TMP/repo2/.cxt/objects/snapshots/*')) else 'unlinked')
")
expect "Local root adopts graft parents" "$LOCAL" linked
cxt memorize >/dev/null 2>&1
cxt push >/dev/null 2>&1
MEM=$(curl -sb "$J" "$B/repos/$RID/memories/$(main_head)" | python3 -c "
import json,sys; s=json.load(sys.stdin).get('summary','')
print('inherited' if 'task A' in s and 'task B' in s else 'missing')
")
expect "B head inherits A session summary" "$MEM" inherited

echo "── D. repo2: New session C commit → Session boundary meta"
session "$TMP/repo2" C
echo c > h.txt; git add h.txt; git commit -qm codeC >/dev/null 2>&1
git push -q origin main >/dev/null 2>&1
BOUND=$(curl -sb "$J" "$B/repos/$RID/snapshots" | python3 -c "
import json,sys
snaps={s['id']:s for s in json.load(sys.stdin)}
h=snaps['$(main_head)']; p=snaps.get((h.get('parents') or [''])[0],{})
hs,ps=h.get('session_id',''),p.get('session_id','')
print('boundary' if hs and ps and hs!=ps else 'no-boundary')
")
expect "session C↔B boundary detected from session_id" "$BOUND" boundary

echo "── E. repo1: post-merge hook = fetch-only(keep local refs + upstream hint) + pull briefing"
cd "$TMP/repo1"
LOCAL_BEFORE=$(cat .cxt/refs/heads/main)
HOOKOUT=$(cxt git-hook post-merge 0 2>&1)
expect "fetch-only: keep local main ref" "$(cat .cxt/refs/heads/main)" "$LOCAL_BEFORE"
expect "upstream hint output" "$(echo "$HOOKOUT" | grep -ci 'new context')" 1
# Pull briefing: summarize and store the incoming codeB/codeC range, then consume it once
# from the next prompt hook as additionalContext (the shared Claude/Codex live-injection channel).
expect "briefing creation notice output" "$(echo "$HOOKOUT" | grep -c 'briefed to the agent')" 1
expect "team member commit summary included in briefing" "$(python3 -c "import json;print('codeC' in json.load(open('.cxt/briefing.json'))['text'])" 2>/dev/null)" True
BRIEF=$(echo "{\"cwd\":\"$TMP/repo1\",\"prompt\":\"go on\"}" | cxt hook --provider claude --event UserPromptSubmit)
expect "hook emits additionalContext JSON" "$(echo "$BRIEF" | python3 -c "
import json,sys
try: print('codeC' in json.load(sys.stdin)['hookSpecificOutput']['additionalContext'])
except Exception: print('parse-fail')")" True
expect "briefing is consumed once and deleted" "$([ ! -f .cxt/briefing.json ] && echo yes)" yes
BRIEF2=$(echo "{\"cwd\":\"$TMP/repo1\"}" | cxt hook --provider claude --event UserPromptSubmit)
expect "subsequent prompt emits no briefing" "$([ -z "$BRIEF2" ] && echo yes)" yes

echo "── F. repo1: both-moved diverge push → automatic rebase-graft"
HEAD_PREV=$(main_head)
git pull -q origin main >/dev/null 2>&1 # code sync (context ref is fetch-only)
session "$TMP/repo1" D
echo d > i.txt; git add i.txt; git commit -qm codeD >/dev/null 2>&1
git push -q origin main >"$TMP/p4.out" 2>&1
expect "hook reports automatic append" "$(grep -c 'appended' "$TMP/p4.out")" 1
REBASE=$(curl -sb "$J" "$B/repos/$RID/snapshots" | python3 -c "
import json,sys
snaps={s['id']:s for s in json.load(sys.stdin)}
d=snaps.get('$(main_head)',{})
ps=(d.get('parents') or []) + (d.get('graft_parents') or [])
ok = '$HEAD_PREV' in ps and d.get('grafted')
seen=set(); q=[d.get('id','')]; reach=False
while q:
    c=q.pop()
    if c=='$HEAD_A': reach=True; break
    if c in seen or c not in snaps: continue
    seen.add(c); q.extend((snaps[c].get('parents') or []) + (snaps[c].get('graft_parents') or []))
print('rebased' if ok and reach else f'bad(parents={ps[:1]},grafted={d.get(\"grafted\")},reach={reach})')
")
expect "D rebased to previous head + reachability preservation" "$REBASE" rebased

echo "── G. Context switch: checkout -b → checkpoint, isolation, seed, boundary, enforcement, wrapper"
cd "$TMP/repo2"
PROJ="$HOME/.claude/projects/$(python3 -c "import re,sys;print(re.sub(r'[^A-Za-z0-9]','-',sys.argv[1]))" "$TMP/repo2")"
session "$TMP/repo2" E
git checkout -qb feature-x >"$TMP/sw.out" 2>&1
expect "Checkpoint execution" "$(grep -c 'cxt: checkpoint' "$TMP/sw.out")" 1
expect "seed creation output" "$(grep -c 'seed created' "$TMP/sw.out")" 1
expect "boundary signal output" "$(grep -c 'previous session is isolated' "$TMP/sw.out")" 1
expect "All previous sessions isolated (renamed)" "$(ls "$PROJ"/*.jsonl.superseded 2>/dev/null | wc -l | tr -d ' ')" "$(ls "$PROJ" | grep -c superseded)"
expect "Boundary record" "$([ -f .cxt/boundary.json ] && echo yes)" yes
SEED=$(python3 -c "import json;print(json.load(open('.cxt/boundary.json')).get('seed_path',''))")
SEEDID=$(python3 -c "import json;print(json.load(open('.cxt/boundary.json')).get('seed_id',''))")
RESUMEID=$(python3 -c "import json;cmd=json.load(open('.cxt/boundary.json')).get('resume_cmd','').split();print(cmd[-1] if cmd else '')")
expect "seed materialization" "$([ -n "$SEED" ] && [ -f "$SEED" ] && echo yes)" yes
expect "boundary seed ID matches materialized resume target" "$RESUMEID" "$SEEDID"
expect "seed inherits main compact memory" "$(grep -q 'Project understanding (main)' "$SEED" && echo yes)" yes
expect "seed inherits main session conversation" "$(grep -q 'task E' "$SEED" && echo yes)" yes
SEEDMEM=$(python3 -c "
import json,glob
tgt=open('.cxt/refs/heads/feature-x').read().strip()
for p in glob.glob('.cxt/objects/snapshots/*'):
    s=json.load(open(p))
    if s['id']==tgt: print(s.get('memory_hash','')); break
")
expect "seed snapshot retains full inherited memory object" "$([ -n "$SEEDMEM" ] && [ -f ".cxt/objects/memories/${SEEDMEM#sha256:}" ] && echo yes)" yes
SEEDMSG=$(python3 -c "
import json,glob
tgt=open('.cxt/refs/heads/feature-x').read().strip()
for p in glob.glob('.cxt/objects/snapshots/*'):
    s=json.load(open(p))
    if s['id']==tgt: print(s['message'][:5]); break
")
expect "seed is first snapshot of feature-x" "$SEEDMSG" "seed:"

# Capture exclusion: a commit before the seed session grows must not capture another session.
echo x > x.txt; git add x.txt; git commit -qm nocap >"$TMP/nc.out" 2>&1
expect "unresumed seed is not captured" "$(grep -c 'cxt: snapshot' "$TMP/nc.out")" 0
# After resuming (file growth), it captures as a formal active session.
case "$SEED" in
  "$HOME"/.codex/sessions/*)
    echo '{"timestamp":"2026-07-05T00:00:02Z","type":"event_msg","payload":{"type":"user_message","message":"resumed work"}}' >> "$SEED"
    ;;
  *)
    echo '{"type":"user","sessionId":"resumed","gitBranch":"feature-x","message":{"role":"user","content":"resumed work"}}' >> "$SEED"
    ;;
esac
echo y > y.txt; git add y.txt; git commit -qm cap >"$TMP/cap.out" 2>&1
expect "resumed seed captures" "$(grep -c 'cxt: snapshot' "$TMP/cap.out")" 1

# Enforcement: boundary-enforce terminates processes that still hold isolated session files.
SUP=$(ls "$PROJ"/*.jsonl.superseded | head -1)
tail -f "$SUP" >/dev/null 2>&1 &
TPID=$!
disown "$TPID" # boundary-enforce owns termination; remove this process from the shell job table.
cxt git-hook boundary-enforce >/dev/null 2>&1
sleep 0.3
expect "isolated session holder killed" "$(kill -0 "$TPID" 2>/dev/null && echo alive || echo dead)" dead

# Wrapper: cxt claude detects boundary and automatically restarts as a seed.
export CLAUDELOG="$TMP/agent.log"
cat > "$TMP/bin/claude" <<'SH'
#!/bin/bash
echo "AGENT $*" >> "$CLAUDELOG"
trap 'kill $SP 2>/dev/null; exit 0' TERM
sleep 30 & SP=$!
wait $SP
SH
chmod +x "$TMP/bin/claude"
cxt claude >/dev/null 2>&1 &
WPID=$!
sleep 1.5
session "$TMP/repo2" F
git checkout -qb feature-y >/dev/null 2>&1
NEWSEEDID=$(python3 -c "import json;print(json.load(open('.cxt/boundary.json')).get('seed_id',''))")
for i in $(seq 1 20); do grep -q -- "--resume $NEWSEEDID" "$CLAUDELOG" 2>/dev/null && break; sleep 0.5; done
expect "wrapper automatically restarts as seed (--resume)" "$(grep -c -- "--resume $NEWSEEDID" "$CLAUDELOG")" 1
expect "restart target = new seed ID" "$(tail -1 "$CLAUDELOG" | grep -c -- "--resume $NEWSEEDID")" 1
kill "$WPID" 2>/dev/null; wait "$WPID" 2>/dev/null; pkill -f "sleep 30" 2>/dev/null

echo "── H. Web personal settings(load_mode): PATCH /me → CLI consumption"
expect "load_mode saved(memory)" "$(ccurl -sb "$J" -X PATCH "$B/me" -H 'Content-Type: application/json' -d '{"load_mode":"memory"}' | jget "['load_mode']")" memory
expect "GET /me reflects" "$(curl -sb "$J" "$B/me" | jget "['load_mode']")" memory
expect "Invalid value 422" "$(ccurl -sb "$J" -o /dev/null -w '%{http_code}' -X PATCH "$B/me" -H 'Content-Type: application/json' -d '{"load_mode":"bogus"}')" 422
# CLI consumption: switching an authenticated repo1 to an existing branch uses the server's memory fidelity setting.
cd "$TMP/repo1"
git checkout -qb tmp-z >/dev/null 2>&1
git checkout -q main >"$TMP/pref.out" 2>&1
expect "Switch load uses server personal settings(memory)" "$(grep -c 'fidelity: memory' "$TMP/pref.out")" 1
ccurl -sb "$J" -X PATCH "$B/me" -H 'Content-Type: application/json' -d '{"load_mode":""}' >/dev/null

echo "── I. Web fork connection: git branch = align+connect / switch -c = seed priority"
cd "$TMP/repo1"
git checkout -q main >/dev/null 2>&1
# Choose the newest snapshot whose [git sha] exists in repo1. The head may be a checkpoint
# without a Git link, and commits originating in repo2 may not exist in repo1.
FORK_FROM=""; FORK_SHA=""
while read -r cid csha; do
  if git cat-file -t "$csha" >/dev/null 2>&1; then FORK_FROM="$cid"; FORK_SHA="$csha"; break; fi
done <<<"$(curl -sb "$J" "$B/repos/$RID/snapshots" | python3 -c "
import json,sys,re
for s in sorted(json.load(sys.stdin), key=lambda x: x['created_at'], reverse=True):
    m=re.search(r'\[git ([0-9a-f]+)\]', s['message'])
    if m: print(s['id'], m.group(1))
")"
expect "Fork point snapshot([git sha] link) confirmed" "$([ -n "$FORK_SHA" ] && echo yes)" yes
ccurl -sb "$J" -X POST "$B/repos/$RID/fork" -H 'Content-Type: application/json' \
  -d "{\"from\":\"$FORK_FROM\",\"new_branch\":\"web-fork-x\",\"author\":{\"name\":\"E2E\",\"email\":\"e2e@test.local\",\"team\":\"\"}}" >/dev/null
# Create an unpushed commit so HEAD(Y) advances beyond fork point(X).
echo z > z.txt; git add z.txt; git commit -qm ahead >/dev/null 2>&1
git branch web-fork-x
sleep 4 # Wait for detached fork-connect seed polling (≤1.5s) and the remote lookup.
expect "branch is aligned to fork point [git X]" "$(git rev-parse --short=7 web-fork-x)" "$(git rev-parse --short=7 "$FORK_SHA")"
expect "context ref is connected to fork snapshot" "$(cat .cxt/refs/heads/web-fork-x 2>/dev/null)" "$FORK_FROM"
# Switching should materialize the fork context; a fork-only ref represents an existing branch.
session "$TMP/repo1" WFX
git checkout -q web-fork-x >"$TMP/wfx.out" 2>&1
expect "switch does not create seed" "$(grep -c 'seed created' "$TMP/wfx.out")" 0
expect "fork context is prepared" "$(grep -c 'context prepared' "$TMP/wfx.out")" 1
expect "ref is still fork snapshot after switch" "$(cat .cxt/refs/heads/web-fork-x)" "$FORK_FROM"
git checkout -q main >/dev/null 2>&1
# `switch -c` creates a seed even when a same-named web fork exists: creating while switching
# means the user is branching from their current work.
ccurl -sb "$J" -X POST "$B/repos/$RID/fork" -H 'Content-Type: application/json' \
  -d "{\"from\":\"$FORK_FROM\",\"new_branch\":\"web-fork-y\",\"author\":{\"name\":\"E2E\",\"email\":\"e2e@test.local\",\"team\":\"\"}}" >/dev/null
session "$TMP/repo1" WFY
git checkout -qb web-fork-y >"$TMP/wfy.out" 2>&1
expect "switch -c prioritizes seed" "$(grep -c 'seed created' "$TMP/wfy.out")" 1
expect "ref points to the seed snapshot, not the web fork" "$([ "$(cat .cxt/refs/heads/web-fork-y)" != "$FORK_FROM" ] && echo yes)" yes
sleep 2 # Confirm fork-connect backed off after the seed won.
expect "helper does not overwrite seed" "$([ "$(cat .cxt/refs/heads/web-fork-y)" != "$FORK_FROM" ] && echo yes)" yes
git checkout -q main >/dev/null 2>&1

echo "── J. cxt setup: onboarding single command (idempotent, merge preservation)"
mkdir -p "$HOME/.codex"
cat > "$HOME/.codex/hooks.json" <<'EOF'
{"hooks":{"Stop":[{"hooks":[{"type":"command","command":"my-custom-hook","timeout":5}]}]}}
EOF
mkdir -p "$TMP/repo3"; cd "$TMP/repo3"
git init -q; git remote add origin "$TMP/bare.git"; git commit -q --allow-empty -m init
cxt setup "$REMOTE" --no-login >"$TMP/setup.out" 2>&1
expect "setup: .cxt created" "$([ -d .cxt ] && echo yes)" yes
expect "setup: git hook installation" "$(grep -c 'git-hook post-commit' .git/hooks/post-commit 2>/dev/null)" 1
expect "setup: remote registration" "$(grep -c 'origin' .cxt/config)" 1
expect "setup: claude hook creation(.claude/settings.json)" "$(grep -c 'cxt hook --provider claude --event UserPromptSubmit' .claude/settings.json 2>/dev/null)" 1
expect "setup: codex hook merge" "$(grep -c 'cxt hook --provider codex --event Stop' "$HOME/.codex/hooks.json")" 1
expect "setup: preserve existing codex user hooks" "$(grep -c 'my-custom-hook' "$HOME/.codex/hooks.json")" 1
expect "setup: codex /hooks approval notice" "$(grep -c 'requires one-time approval' "$TMP/setup.out")" 1
# Idempotence: a rerun leaves files unchanged and reports "already registered."
H1=$(shasum "$HOME/.codex/hooks.json" .claude/settings.json | shasum)
cxt setup --no-login >"$TMP/setup2.out" 2>&1
H2=$(shasum "$HOME/.codex/hooks.json" .claude/settings.json | shasum)
expect "setup idempotence: hook file unchanged" "$H2" "$H1"
expect "setup idempotence: already registered reported" "$(grep -c 'already registered' "$TMP/setup2.out")" 2

echo
if [ "$FAIL" = 0 ]; then echo "SYNC E2E: All passed ✓"; else echo "SYNC E2E: Failures exist ✗"; fi
exit "$FAIL"
