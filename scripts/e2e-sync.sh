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
#   J2. global app hooks in unconnected repositories            → no store creation + residue quarantine
#   K. oversized single event                                  → v2 bounded push/pull + v1 fallback
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
# Preserve the isolated fixture on demand so fail-open Git hook diagnostics are
# still inspectable after the script exits.
cleanup() {
  [ -n "$SRV_PID" ] && kill "$SRV_PID" 2>/dev/null && wait "$SRV_PID" 2>/dev/null
  if [ "${CXT_E2E_KEEP_TMP:-0}" = 1 ]; then
    echo "SYNC E2E fixture preserved: $TMP"
  else
    rm -rf "$TMP"
  fi
}
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
# The fixture must prove wrapper ownership through a real process ancestry.
# Never inherit a developer shell's cxt wrapper markers: that made this test
# pass locally while taking the unmanaged desktop-app path in clean CI.
unset CXT_WRAPPED CXT_WRAPPER_PID CXT_WRAPPED_AGENT CXT_WRAPPED_SESSION_ID
unset CXT_KEEP_SESSION CXT_CARRY CXT_CLAUDE_MEMORY_PROFILE CXT_CLAUDE_MEMORY_CONFIG_FINGERPRINT CXT_CLAUDE_FLAG_AUTO_MEMORY_DIRECTORY
git config --global user.email e2e@test.local
git config --global user.name E2E
git config --global init.defaultBranch main

CXT_AUTH=dev "$TMP/bin/cxtd" serve --addr 127.0.0.1:$PORT --data "$TMP/data" >"$TMP/srv.log" 2>&1 &
SRV_PID=$!
for i in $(seq 1 30); do curl -sf -o /dev/null "$B/repos" && break; sleep 0.3; done

J="$TMP/a.jar"
ccurl -s -c "$J" -X POST "$B/auth/session" -H "Authorization: Bearer dev:o@t.io:O" >/dev/null
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

large_session() { # large_session <cwd> <label> — one event exceeds the v1 2MiB bound.
  session "$1" "$2"
  D="$HOME/.claude/projects/$(python3 -c "import re,sys;print(re.sub(r'[^A-Za-z0-9]','-',sys.argv[1]))" "$1")"
  python3 - "$D/s-$2.jsonl" <<'PY'
import json,sys
path=sys.argv[1]
rows=[json.loads(line) for line in open(path)]
rows[0]['message']['content']='x' * ((2 << 20) + (32 << 10))
with open(path,'w') as f:
    for row in rows:
        f.write(json.dumps(row,separators=(',',':'))+'\n')
PY
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
NOOP_PUSH=$(cxt push 2>&1)
expect "no-op push offers zero snapshots after preflight" "$(echo "$NOOP_PUSH" | grep -c 'pushed 0 snapshot(s)')" 1

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
PULL_TERM=cxt-e2e-pull-terminal
HOOKOUT=$(TERM_SESSION_ID="$PULL_TERM" cxt git-hook post-merge 0 2>&1)
expect "fetch-only: keep local main ref" "$(cat .cxt/refs/heads/main)" "$LOCAL_BEFORE"
expect "upstream hint output" "$(echo "$HOOKOUT" | grep -ci 'new context')" 1
# Pull briefing: store only validated identifiers for the incoming codeB/codeC
# range, then consume the notice once from the next prompt hook in the
# initiating terminal as additionalContext. Collaborator-authored labels stay
# in the DAG/web view and never enter the model prompt.
expect "briefing creation notice output" "$(echo "$HOOKOUT" | grep -c 'briefed to the agent')" 1
BRIEF_FILE=$(find .cxt/briefings -maxdepth 1 -type f -name '*.json' -print -quit 2>/dev/null)
expect "briefing uses one scoped queue" "$(find .cxt/briefings -maxdepth 1 -type f -name '*.json' | wc -l | tr -d ' ')" 1
expect "briefing contains identifiers but no collaborator labels" "$(python3 -c "import json;d=json.load(open('$BRIEF_FILE'));t='\n'.join(d.get('texts') or [d.get('text','')]);print('identifiers only' in t and 'sha256:' in t and 'codeC' not in t and 'codeB' not in t)" 2>/dev/null)" True
WRONG_BRIEF=$(echo "{\"cwd\":\"$TMP/repo1\",\"prompt\":\"go on elsewhere\"}" | TERM_SESSION_ID=cxt-e2e-other-terminal cxt hook --provider claude --event UserPromptSubmit)
expect "another terminal cannot consume pull briefing" "$([ -z "$WRONG_BRIEF" ] && echo yes)" yes
expect "wrong terminal leaves scoped briefing intact" "$(find .cxt/briefings -maxdepth 1 -type f -name '*.json' | wc -l | tr -d ' ')" 1
BRIEF=$(echo "{\"cwd\":\"$TMP/repo1\",\"prompt\":\"go on\"}" | TERM_SESSION_ID="$PULL_TERM" cxt hook --provider claude --event UserPromptSubmit)
expect "hook emits additionalContext JSON" "$(echo "$BRIEF" | python3 -c "
import json,sys
try:
    text=json.load(sys.stdin)['hookSpecificOutput']['additionalContext']
    print('identifiers only' in text and 'sha256:' in text and 'codeC' not in text and 'codeB' not in text)
except Exception: print('parse-fail')")" True
expect "briefing is consumed once and deleted" "$(find .cxt/briefings -maxdepth 1 -type f -name '*.json' | wc -l | tr -d ' ')" 0
BRIEF2=$(echo "{\"cwd\":\"$TMP/repo1\"}" | TERM_SESSION_ID="$PULL_TERM" cxt hook --provider claude --event UserPromptSubmit)
expect "subsequent prompt emits no briefing" "$([ -z "$BRIEF2" ] && echo yes)" yes
REPEAT_HOOKOUT=$(TERM_SESSION_ID="$PULL_TERM" cxt git-hook post-merge 0 2>&1)
expect "same remote tip is not briefed again" "$(echo "$REPEAT_HOOKOUT" | grep -c 'briefed to the agent')" 0
expect "repeat pull leaves no briefing queue" "$(find .cxt/briefings -maxdepth 1 -type f -name '*.json' | wc -l | tr -d ' ')" 0
expect "repeat fetch negotiates zero snapshot metadata" "$(echo "$REPEAT_HOOKOUT" | grep -c 'fetched .*snapshot')" 0
MANIFEST_STATE=$(curl -sb "$J" "$B/repos/$RID/manifest" | python3 -c "
import json,sys
m=json.load(sys.stdin); print('complete' if len(m.get('snapshot_states') or {})==len(m.get('snapshot_index') or []) else 'partial')
")
expect "manifest advertises complete snapshot state catalog" "$MANIFEST_STATE" complete

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

echo "── G. Context switch: desktop app retention + real wrapper ancestry/restart"
cd "$TMP/repo2"
PROJ="$HOME/.claude/projects/$(python3 -c "import re,sys;print(re.sub(r'[^A-Za-z0-9]','-',sys.argv[1]))" "$TMP/repo2")"
APP_SESSION_ID=11111111-1111-4111-8111-111111111111
APP_SESSION="$PROJ/$APP_SESSION_ID.jsonl"
mkdir -p "$PROJ"
cat > "$APP_SESSION" <<EOF
{"type":"user","cwd":"$TMP/repo2","sessionId":"$APP_SESSION_ID","gitBranch":"main","timestamp":"2026-07-05T00:00:00Z","message":{"role":"user","content":"task E"}}
{"type":"assistant","cwd":"$TMP/repo2","sessionId":"$APP_SESSION_ID","gitBranch":"main","timestamp":"2026-07-05T00:00:01Z","message":{"role":"assistant","model":"claude-fable-5","content":[{"type":"text","text":"done E"}],"usage":{"input_tokens":100,"output_tokens":10}}}
EOF

# A plain desktop-app shell has no cxt supervisor. It must checkpoint and seed
# the cxt DAG, but keep the vendor-owned native session file open and inject
# only one bounded memory handoff on the next official lifecycle hook.
APP_JSONL_BEFORE=$(find "$PROJ" -maxdepth 1 -type f -name '*.jsonl' | wc -l | tr -d ' ')
git checkout -qb app-feature-x >"$TMP/app-sw.out" 2>&1
expect "app switch checkpoints previous branch" "$([ "$(grep -c 'cxt: checkpoint' "$TMP/app-sw.out")" -ge 1 ] && echo yes)" yes
expect "app switch creates branch seed" "$(grep -c 'seed created' "$TMP/app-sw.out")" 1
expect "app switch retains native session" "$(grep -c 'app session retained' "$TMP/app-sw.out")" 1
expect "app switch does not create wrapper boundary" "$([ ! -e .cxt/boundary.json ] && echo yes)" yes
expect "app switch does not supersede provider files" "$(find "$PROJ" -maxdepth 1 -type f -name '*.superseded' | wc -l | tr -d ' ')" 0
expect "app switch does not materialize orphan native seed" "$(find "$PROJ" -maxdepth 1 -type f -name '*.jsonl' | wc -l | tr -d ' ')" "$APP_JSONL_BEFORE"
expect "app session file remains at original path" "$([ -f "$APP_SESSION" ] && echo yes)" yes
APP_HANDOFF=$(echo "{\"cwd\":\"$TMP/repo2\",\"session_id\":\"$APP_SESSION_ID\",\"transcript_path\":\"$APP_SESSION\",\"prompt\":\"continue\"}" | cxt hook --provider claude --event UserPromptSubmit)
expect "app handoff is one bounded project-memory injection" "$(echo "$APP_HANDOFF" | python3 -c "
import json,sys
try:
    text=json.load(sys.stdin)['hookSpecificOutput']['additionalContext']
    print('yes' if 'cxthub branch context handoff' in text and len(text.encode()) <= 16*1024 else 'no')
except Exception: print('no')")" yes
expect "app handoff is consumed once" "$(find .cxt/handoffs -maxdepth 1 -type f -name '*.json' 2>/dev/null | wc -l | tr -d ' ')" 0
git checkout -q main >/dev/null 2>&1
echo "{\"cwd\":\"$TMP/repo2\",\"session_id\":\"$APP_SESSION_ID\",\"transcript_path\":\"$APP_SESSION\",\"prompt\":\"back on main\"}" | cxt hook --provider claude --event UserPromptSubmit >/dev/null

# Resume the same file under an actual cxt wrapper and grow it once. The fake
# Claude child performs git checkout itself, so the hook's process ancestry is
# wrapper → provider child → git → cxt hook on every OS/CI runner.
echo '{"type":"user","sessionId":"11111111-1111-4111-8111-111111111111","gitBranch":"main","message":{"role":"user","content":"wrapper continued"}}' >> "$APP_SESSION"
export CLAUDELOG="$TMP/agent.log"
export CXT_E2E_REPO="$TMP/repo2"
export CXT_E2E_SWITCH_OUT="$TMP/sw.out"
export CXT_E2E_SWITCH_MARK="$TMP/agent-switched"
export CXT_E2E_AGENT_PID="$TMP/agent.pid"
cat > "$TMP/bin/claude" <<'SH'
#!/bin/bash
echo "MEMORY_PROFILE ${CXT_CLAUDE_MEMORY_PROFILE:-missing} ${#CXT_CLAUDE_MEMORY_CONFIG_FINGERPRINT} ${CXT_CLAUDE_FLAG_AUTO_MEMORY_DIRECTORY:-default}" >> "$CLAUDELOG"
echo "AGENT $*" >> "$CLAUDELOG"
echo $$ > "$CXT_E2E_AGENT_PID"
if [ ! -e "$CXT_E2E_SWITCH_MARK" ]; then
  : > "$CXT_E2E_SWITCH_MARK"
  cd "$CXT_E2E_REPO" || exit 1
  git checkout -qb feature-x >"$CXT_E2E_SWITCH_OUT" 2>&1
  exit $?
fi
trap 'kill $SP 2>/dev/null; exit 0' TERM
sleep 30 & SP=$!
wait $SP
SH
chmod +x "$TMP/bin/claude"
CLAUDE_MEMORY_OVERRIDE="$TMP/initial-claude-memory"
cxt claude --resume "$APP_SESSION_ID" --settings "{\"autoMemoryDirectory\":\"$CLAUDE_MEMORY_OVERRIDE\"}" >"$TMP/wrapper.out" 2>&1 &
WPID=$!
for i in $(seq 1 40); do
  [ -f .cxt/boundary.json ] && [ "$(grep -c '^AGENT ' "$CLAUDELOG" 2>/dev/null)" -ge 2 ] && break
  sleep 0.25
done
expect "Checkpoint execution" "$(grep -c 'cxt: checkpoint' "$TMP/sw.out")" 1
expect "seed creation output" "$(grep -c 'seed created' "$TMP/sw.out")" 1
expect "boundary signal output" "$(grep -c 'previous session is isolated' "$TMP/sw.out")" 1
expect "All previous sessions isolated (renamed)" "$([ "$(find "$PROJ" -maxdepth 1 -type f -name '*.jsonl.superseded' | wc -l | tr -d ' ')" -ge 1 ] && echo yes)" yes
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

# The first fake child already caused the transition. The wrapper must observe
# that boundary and restart a second child with the newly materialized seed.
expect "wrapper automatically restarts as seed (--resume)" "$(grep -c -- "--resume $SEEDID" "$CLAUDELOG")" 1
expect "restart target = new seed ID" "$(tail -1 "$CLAUDELOG" | grep -c -- "--resume $SEEDID")" 1
expect "wrapper carries proven Claude memory profile" "$(grep -c '^MEMORY_PROFILE v1 64 ' "$CLAUDELOG")" 2
expect "initial child carries its custom Claude memory directory" "$(grep -c "^MEMORY_PROFILE v1 64 $CLAUDE_MEMORY_OVERRIDE$" "$CLAUDELOG")" 1
expect "restarted child drops settings no longer present in its arguments" "$(grep -c '^MEMORY_PROFILE v1 64 default$' "$CLAUDELOG")" 1
if [ -f "$CXT_E2E_AGENT_PID" ]; then kill "$(cat "$CXT_E2E_AGENT_PID")" 2>/dev/null; fi
wait "$WPID" 2>/dev/null

echo "── H. Web personal settings(load_mode): PATCH /me → CLI consumption"
expect "load_mode saved(memory)" "$(ccurl -sb "$J" -X PATCH "$B/me" -H 'Content-Type: application/json' -d '{"load_mode":"memory"}' | jget "['load_mode']")" memory
expect "GET /me reflects" "$(curl -sb "$J" "$B/me" | jget "['load_mode']")" memory
expect "Invalid value 422" "$(ccurl -sb "$J" -o /dev/null -w '%{http_code}' -X PATCH "$B/me" -H 'Content-Type: application/json' -d '{"load_mode":"bogus"}')" 422
# CLI consumption: an explicit cxt load materializes using the account-wide
# setting. Plain desktop-app Git switches intentionally do not replace the
# vendor-owned native session and are covered in section G.
cd "$TMP/repo1"
cat > CLAUDE.md <<'EOF'
# User-owned instructions
Preserve this text and its file mode.
EOF
chmod 600 CLAUDE.md
cxt load main --provider claude >"$TMP/pref.out" 2>&1
expect "CLI load uses server personal settings(memory)" "$(grep -c 'fidelity: memory' "$TMP/pref.out")" 1
cxt load main --provider claude >/dev/null 2>&1
expect "memory load preserves user instructions" "$(python3 -c "from pathlib import Path; print('yes' if Path('CLAUDE.md').read_text().startswith('# User-owned instructions\nPreserve this text and its file mode.\n') else 'no')")" yes
expect "memory load refreshes one managed block" "$(grep -c '^<!-- cxt:begin managed memory' CLAUDE.md)" 1
expect "memory managed block stays within 64 KiB" "$(python3 -c "from pathlib import Path; b=Path('CLAUDE.md').read_bytes(); s=b.index(b'<!-- cxt:begin managed memory'); e=b.index(b'<!-- cxt:end managed memory -->', s)+len(b'<!-- cxt:end managed memory -->')+1; print('yes' if e-s <= 64*1024 else 'no')")" yes
expect "memory load preserves instruction file mode" "$(python3 -c "from pathlib import Path; print(oct(Path('CLAUDE.md').stat().st_mode & 0o777))")" 0o600
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
expect "fork context is selected without replacing app session" "$(grep -c 'app context selected' "$TMP/wfx.out")" 1
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

echo "── I. Branch lifecycle: rename transfers context; deletion archives without deleting history"
RENAME_OUT=$(git branch -m web-fork-x web-fork-renamed 2>&1)
expect "Git branch rename transfers context projection" "$(echo "$RENAME_OUT" | grep -c 'context moved')" 1
expect "renamed context keeps its exact target" "$(cat .cxt/refs/heads/web-fork-renamed 2>/dev/null)" "$FORK_FROM"
expect "renamed source context projection is gone" "$([ ! -e .cxt/refs/heads/web-fork-x ] && echo yes)" yes
expect "rename keeps all context objects readable" "$(cxt fsck | grep -c 'Missing 0')" 1

DELETE_OUT=$(git branch -D web-fork-renamed 2>&1)
expect "normal branch deletion invokes context archive" "$(echo "$DELETE_OUT" | grep -c 'context archived')" 1
expect "deleted Git branch has no active context projection" "$([ ! -e .cxt/refs/heads/web-fork-renamed ] && echo yes)" yes
expect "archive keeps the target snapshot readable" "$(cxt fsck | grep -c 'Missing 0')" 1
expect "archive records an immutable lifecycle event" "$(find .cxt/refs/tags/cxt/branch-state/v1 -type f -path '*/archived/*/web-fork-renamed' 2>/dev/null | wc -l | tr -d ' ')" 1

# Detached HEAD used to return before reading deletion transactions. Delete a
# second branch while detached to exercise the real hook and zeros→zeros Git
# transaction form, not only the parser unit test.
git checkout -q --detach HEAD >/dev/null 2>&1
DETACHED_DELETE_OUT=$(git branch -D web-fork-y 2>&1)
expect "detached branch deletion still invokes context archive" "$(echo "$DETACHED_DELETE_OUT" | grep -c 'context archived')" 1
expect "detached deletion removes only the active projection" "$([ ! -e .cxt/refs/heads/web-fork-y ] && echo yes)" yes
expect "detached archive preserves immutable history" "$(cxt fsck | grep -c 'Missing 0')" 1

# The archive helper pushes asynchronously. The server must converge to no
# branch projection while retaining the lifecycle tags as reachability roots.
for i in $(seq 1 40); do
  SERVER_ARCHIVE_STATE=$(curl -sb "$J" "$B/repos/$RID/refs" | python3 -c "
import json,sys
refs=json.load(sys.stdin)
branches={r['name'] for r in refs if r['kind']=='branch'}
archives=[r for r in refs if r['kind']=='tag' and '/archived/' in r['name'] and r['name'].startswith('cxt/branch-state/v1/')]
print('ready' if 'web-fork-x' not in branches and 'web-fork-renamed' not in branches and 'web-fork-y' not in branches and len(archives) >= 3 else 'waiting')
")
  [ "$SERVER_ARCHIVE_STATE" = ready ] && break
  sleep 0.25
done
expect "server applies both recoverable archive projections" "$SERVER_ARCHIVE_STATE" ready

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

echo "── J2. Global app hook: unconnected no-op + legacy residue quarantine"
mkdir -p "$TMP/unconnected"; cd "$TMP/unconnected"; git init -q
printf '%s' "{\"session_id\":\"unconnected\",\"cwd\":\"$TMP/unconnected\",\"prompt\":\"continue\"}" |
  cxt hook --provider codex --event UserPromptSubmit
expect "global app hook does not create .cxt before init" "$([ ! -e .cxt ] && echo yes)" yes

mkdir -p "$TMP/residue"; cd "$TMP/residue"; git init -q; mkdir -p .cxt/capture
printf '%s' "{\"session_id\":\"legacy-residue\",\"cwd\":\"$TMP/residue\",\"prompt\":\"continue\"}" |
  cxt hook --provider codex --event UserPromptSubmit
expect "directory-only residue is not promoted to an initialized store" "$([ ! -e .cxt/HEAD ] && echo yes)" yes
expect "directory-only residue receives tracked ignore protection" "$(grep -c '^\.cxt/$' .gitignore 2>/dev/null)" 1
expect "directory-only residue receives local ignore protection" "$(git check-ignore .cxt/probe 2>/dev/null | grep -c '^\.cxt/probe$')" 1
expect "directory-only residue does not grow capture state" "$(find .cxt -type f | wc -l | tr -d ' ')" 0

echo "── K. Chunk CAS v2: oversized event push/pull + old-client fallback"
git clone -q "$TMP/bare.git" "$TMP/repo4"
cd "$TMP/repo4"; cxt init >/dev/null 2>&1; cxt remote add origin "$REMOTE" >/dev/null 2>&1
large_session "$TMP/repo4" BIG
echo big > big.txt; git add big.txt; git commit -qm big-event >/dev/null 2>&1
git push -q origin main >"$TMP/big-push.out" 2>&1
BIG_HEAD=$(main_head)
BIG_MANIFEST=$(ccurl -sb "$J" -X POST "$B/repos/$RID/pull/objects" -H 'Content-Type: application/json' \
  -d "{\"doc_manifest_wants\":[\"$BIG_HEAD\"],\"chunk_formats_supported\":[\"cxt-doc-chunks-v1\",\"cxt-doc-chunks-v2\"]}" | python3 -c "
import json,sys
r=json.load(sys.stdin); m=(r.get('doc_manifests') or [{}])[0]
print(f\"{m.get('format','')}:{len(m.get('chunks') or [])}\")
")
expect "oversized event stored and served as v2" "$(echo "$BIG_MANIFEST" | cut -d: -f1)" cxt-doc-chunks-v2
expect "oversized event split into bounded chunks" "$([ "$(echo "$BIG_MANIFEST" | cut -d: -f2)" -gt 1 ] && echo yes)" yes
OLD_FALLBACK=$(ccurl -sb "$J" -X POST "$B/repos/$RID/pull/objects" -H 'Content-Type: application/json' \
  -d "{\"doc_manifest_wants\":[\"$BIG_HEAD\"]}" | python3 -c "import json,sys;r=json.load(sys.stdin);print(f\"{len(r.get('docs') or [])}:{len(r.get('doc_manifests') or [])}\")")
expect "client without v2 capability receives full-doc fallback" "$OLD_FALLBACK" 1:0
git clone -q "$TMP/bare.git" "$TMP/repo5"
cd "$TMP/repo5"; cxt init >/dev/null 2>&1; cxt remote add origin "$REMOTE" >/dev/null 2>&1
cxt pull >/dev/null 2>&1
expect "fresh client pulls and verifies v2 history" "$(cxt fsck | grep -c 'Missing 0')" 1

echo
if [ "$FAIL" = 0 ]; then echo "SYNC E2E: All passed ✓"; else echo "SYNC E2E: Failures exist ✗"; fi
exit "$FAIL"
