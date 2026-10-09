#!/usr/bin/env bash
# cxthub sync E2E — verifies append grafts, memory inheritance, session boundaries,
# and fetch-only behavior with real cxtd/cxt binaries and Git hooks.
#
# Scenarios along the context-divergence policy path:
#   A. legacy repo1 pushes session A, memorize      (base chain + memory)
#   B. repo2 (new clone, no pull) pushes independent session B → hook append (root graft)
#   C. repo2 pulls, adopts the graft, and memorizes             → B inherits memory from A
#   D. repo2 commits new session C                              → session boundary metadata
#   E. repo1 runs post-merge                                    → fetch-only, preserving local refs
#   F. repo1 pushes after both sides moved                      → automatic rebase-graft
#   J2. global app hooks in unconnected repositories            → no store creation + residue quarantine
#   K. oversized single event                                  → v2 bounded push/pull + v1 fallback
#   T. modern main birth → fresh clone + setup → same-identity ordinary push
#   U. first-owner setup → same-code capture → initial legacy observation (PG)
#
# Run with isolated TMP, HOME, and a randomized port; no local state is retained.
# CXT_E2E_PUBLICATION_ONLY=1 runs just the promotion/next-commit regression.
# CXT_E2E_CXT_BIN=/absolute/path tests a saved baseline CLI against that scenario.
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
WPID=""
TPID=""

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
      echo "fixture cleanup incomplete: owned pid=$owned_pid did not exit" >>"$TMP/wrapper-observation.out"
      CLEANUP_UNFINISHED="${CLEANUP_UNFINISHED:-} $owned_pid"
      FAIL=1; CXT_E2E_KEEP_TMP=1; return 1
    fi
    sleep 0.05
  done
  wait "$owned_pid" 2>/dev/null || true
}
# END fixture job cleanup.
# Reap the server immediately after killing it to avoid shell "Terminated" noise.
# Preserve the isolated fixture on demand so fail-open Git hook diagnostics are
# still inspectable after the script exits.
cleanup() {
  local test_status=$?
  local cleanup_pid
  for cleanup_pid in "$TPID" "$WPID"; do
    if ! finish_fixture_job "$cleanup_pid"; then
      [ "$test_status" -ne 0 ] || test_status=1
    fi
  done
  if [ "$FAIL" -ne 0 ] && [ "$test_status" -eq 0 ]; then test_status=1; fi
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
main_head() { curl -sb "$J" "$B/repos/$RID/refs" | python3 -c "import json,sys;print(next((r['target'] for r in json.load(sys.stdin) if r['kind']=='branch' and r['name']=='main'),''))"; }

# Explicit compatibility setup for legacy-store/migration fixtures only.
# PostgreSQL fresh-use cases never call this helper.
fixture_register_legacy_repo() {
  python3 - "$1" "$2" >"$TMP/legacy-registration.json" <<'PYLEGACYREPO'
import hashlib,json,sys,urllib.parse
remote,git_remote=sys.argv[1:]
u=urllib.parse.urlsplit(remote)
identity=(u.netloc+u.path).removesuffix('.git').rstrip('/').lower()
print(json.dumps({'id':'sha256:'+hashlib.sha256(identity.encode()).hexdigest(),'remote_url':remote,'git_remote_url':git_remote,'default_branch':'main'}))
PYLEGACYREPO
  [ "$?" = 0 ] || return 1
  ccurl -fsSb "$J" -X POST "$B/repos" -H 'Content-Type: application/json' \
    --data-binary @"$TMP/legacy-registration.json" >"$TMP/legacy-registration-response.json" || return 1
  python3 - "$TMP/legacy-registration-response.json" <<'PYLEGACYSTATE'
import json,sys
r=json.load(open(sys.argv[1])); assert r.get('context_protocol',0)==0
PYLEGACYSTATE
}

ref_target() { python3 - "$1" <<'PYREF'
import json,pathlib,sys
raw=pathlib.Path(sys.argv[1]).read_text().strip()
print(json.loads(raw)['target'] if raw.startswith('{') else raw)
PYREF
}

birth_field() { python3 - "$1" "$2" <<'PYBIRTH'
import glob,json,sys
rows=[json.load(open(path)) for path in glob.glob('.cxt/history/*.json')]
rows=[r for r in rows if r['branch']==sys.argv[1] and r['kind'] in ('birth','attach','orphan')]
rows.sort(key=lambda r:(r['created_at'],r['id']))
print(rows[-1].get(sys.argv[2],'') if rows else '')
PYBIRTH
}

echo "── build(isolated bin) · server start :$PORT"
server_build=(go build -o "$TMP/bin/cxtd")
if [ -n "${CXT_E2E_DSN:-}" ]; then server_build+=(-tags postgres); fi
( cd "$ROOT/backend" && "${server_build[@]}" ./cmd/cxtd ) || { echo "cxtd build failed"; exit 1; }
if [ -n "${CXT_E2E_CXT_BIN:-}" ]; then
  cp "$CXT_E2E_CXT_BIN" "$TMP/bin/cxt" || exit 1
else
  ( cd "$ROOT/cli" && go build -o "$TMP/bin/cxt" ./cmd/cxt ) || { echo "cxt build failed"; exit 1; }
fi
if [ "${CXT_E2E_PUBLICATION_ONLY:-0}" != 1 ]; then
  ( cd "$ROOT/cli" && go test -c -o "$TMP/bin/memory-reuse-test" ./internal/adapters/backendclient ) || exit 1
fi
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

CXT_ENV_FILE=/dev/null CXT_AUTH=dev CXT_POSTGRES_DSN="${CXT_E2E_DSN:-}" \
  CXT_MIGRATIONS_DIR="$ROOT/schemas/db/migrations" CXT_PUBLIC_URL="$ORIGIN" \
  CXT_COOKIE_SECURE=0 CXT_COOKIE_DOMAIN= CXT_CORS_ORIGINS="$ORIGIN" \
  RESEND_API_KEY= CXT_GITHUB_TOKEN= \
  "$TMP/bin/cxtd" serve --addr 127.0.0.1:$PORT --data "$TMP/data" >"$TMP/srv.log" 2>&1 &
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
ccurl -s -c "$J" -X POST "$B/auth/session" -H "Authorization: Bearer dev:o@t.io:O" >/dev/null
ccurl -sb "$J" -X POST "$B/repositories" -H 'Content-Type: application/json' -d '{"name":"SyncE2E"}' >/dev/null
OWN=$(curl -sb "$J" "$B/me" | jget "['username']")
SLUG=$(curl -sb "$J" "$B/repositories" | jget "[0]['slug']")
REMOTE="http://127.0.0.1:$PORT/$OWN/$SLUG"  # The two-segment repository URL is the repository identity.
git init -q --bare "$TMP/bare.git"

session() { # session <cwd> <label> — write a synthetic Claude JSONL session with model and usage.
  D="$HOME/.claude/projects/$(python3 -c "import re,sys;print(re.sub(r'[^A-Za-z0-9]','-',sys.argv[1]))" "$1")"
  mkdir -p "$D"
  cat > "$D/sess-$2.jsonl" <<EOF
{"type":"user","cwd":"$1","sessionId":"sess-$2","gitBranch":"main","timestamp":"2026-07-05T00:00:00Z","message":{"role":"user","content":"task $2"}}
{"type":"assistant","cwd":"$1","sessionId":"sess-$2","gitBranch":"main","timestamp":"2026-07-05T00:00:01Z","message":{"role":"assistant","model":"claude-fable-5","content":[{"type":"text","text":"done $2"}],"usage":{"input_tokens":100,"output_tokens":10}}}
EOF
}

large_session() { # large_session <cwd> <label> — one event exceeds the v1 2MiB bound.
  session "$1" "$2"
  D="$HOME/.claude/projects/$(python3 -c "import re,sys;print(re.sub(r'[^A-Za-z0-9]','-',sys.argv[1]))" "$1")"
  python3 - "$D/sess-$2.jsonl" <<'PY'
import json,sys
path=sys.argv[1]
rows=[json.loads(line) for line in open(path)]
rows[0]['message']['content']='x' * ((2 << 20) + (32 << 10))
with open(path,'w') as f:
    for row in rows:
        f.write(json.dumps(row,separators=(',',':'))+'\n')
PY
}

echo "── A. legacy repo1: Session A push + memorize (B root-append compatibility)"
mkdir -p "$TMP/repo1"; cd "$TMP/repo1"
git init -q; git remote add origin "$TMP/bare.git"
# A/B intentionally exercise same legacy identity with independent context roots.
# T separately requires a modern main birth and automatic fresh-clone adoption.
git commit -q --allow-empty -m legacy-code-baseline || exit 1
cxt init >/dev/null 2>&1
CXT_NO_BROWSER=1 cxt login --server "$ORIGIN" >"$TMP/login.out" 2>&1 &
LPID=$!
DCODE=""
for i in $(seq 1 40); do
  DCODE=$(grep -o '[B-Z2-9]\{3\}-[B-Z2-9]\{3\}' "$TMP/login.out" 2>/dev/null | head -1)
  [ -n "$DCODE" ] && break; sleep 0.25
done
expect "device flow code output" "$([ -n "$DCODE" ] && echo yes)" yes
ccurl -sb "$J" -X POST "$B/auth/device/approve" -H 'Content-Type: application/json' -d "{\"code\":\"$DCODE\"}" >/dev/null
wait "$LPID"
if [ -z "${CXT_E2E_DSN:-}" ]; then
  fixture_register_legacy_repo "$REMOTE" "$TMP/bare.git" || exit 1
fi
cxt remote add origin "$REMOTE" >/dev/null 2>&1
if [ "${CXT_E2E_PUBLICATION_ONLY:-0}" = 1 ]; then
  source "$ROOT/scripts/e2e-publication.inc.sh"
  if [ "$FAIL" = 0 ]; then echo "PUBLICATION E2E: All passed ✓"; else echo "PUBLICATION E2E: Failures exist ✗"; fi
  exit "$FAIL"
fi
session "$TMP/repo1" A
echo a > f.txt; git add f.txt; git commit -qm codeA >/dev/null 2>&1
if ! python3 - <<'PYALEGACY'
import hashlib,json,pathlib
ref=json.loads(pathlib.Path('.cxt/refs/heads/main').read_text())
assert ref['branch_id']=='legacy-'+hashlib.sha256((ref['repo_id']+'\x00main').encode()).hexdigest()[:32], 'A/B fixture requires real pre-existing main'
events=[json.loads(p.read_text()) for p in pathlib.Path('.cxt/history').glob('*.json')]
assert not any(e['branch']=='main' and e['kind'] in ('birth','orphan') for e in events), 'legacy fixture fabricated a main birth'
PYALEGACY
then exit 1; fi
if [ -z "${CXT_E2E_DSN:-}" ]; then
  # FS compatibility is explicit existing-repo migration, not new initialization.
  cxt push origin >"$TMP/legacy-bootstrap.out" 2>&1 || { cat "$TMP/legacy-bootstrap.out"; exit 1; }
  RID=$(ccurl -fsSb "$J" "$B/repos" | python3 -c 'import json,sys; print(json.load(sys.stdin)[0]["id"])') || exit 1
  ccurl -fsSb "$J" -X POST "$B/repos/$RID/context-protocol" -H 'Content-Type: application/json' -d '{}' >"$TMP/legacy-protection.json" || exit 1
fi
git push -q -u origin main >"$TMP/p1.out" 2>&1
RID=$(curl -sb "$J" "$B/repos" | python3 -c "import json,sys; rs=json.load(sys.stdin); print(rs[0]['id'] if rs else '')")
ccurl -fsSb "$J" "$B/repos/$RID" >"$TMP/first-publication-state.json" || exit 1
expect "first publication uses protected repository" "$(python3 -c 'import json,sys; print(json.load(open(sys.argv[1]))["context_protocol"])' "$TMP/first-publication-state.json")" 1
HEAD_A=""
[ -n "$RID" ] && HEAD_A=$(main_head)
if [ -z "$HEAD_A" ]; then
  echo "  first push diagnostics:"
  sed 's/^/    /' "$TMP/p1.out"
  tail -20 "$TMP/srv.log" | sed 's/^/    server: /'
fi
expect "repo1 push after server main exists" "$([ -n "$HEAD_A" ] && echo yes)" yes
[ -z "$HEAD_A" ] && exit 1
cat > "$TMP/authored-claims.json" <<'JSONCLAIMS'
[{"kind":"rationale","text":"Retain accepted history across branch joins."}]
JSONCLAIMS
cxt memorize --claims "$TMP/authored-claims.json" >/dev/null 2>&1
expect "explicit claims authoring succeeds" "$?" 0
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
TYPED_MEMORY=$(curl -sb "$J" "$B/repos/$RID/memories/$(main_head)" | python3 -c '
import json,sys
d=json.load(sys.stdin)
claims=[c for f in (d.get("fragments") or []) for c in (f.get("claims") or [])]
print("retained" if d.get("claims_version")==1 and sum(c.get("text")=="Retain accepted history across branch joins." for c in claims)==1 else "missing")
')
expect "typed claims survive remote pull, join and re-memorize" "$TYPED_MEMORY" retained


echo "── D. repo2: New session C commit → Session boundary meta"
session "$TMP/repo2" C
echo c > h.txt; git add h.txt; git commit -qm codeC >"$TMP/codeC.out" 2>&1
git push -q origin main >"$TMP/codeC-push.out" 2>&1
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
LOCAL_BEFORE=$(ref_target .cxt/refs/heads/main)
PULL_TERM=cxt-e2e-pull-terminal
HOOKOUT=$(TERM_SESSION_ID="$PULL_TERM" cxt git-hook post-merge 0 2>&1)
expect "fetch-only: keep local main ref" "$(ref_target .cxt/refs/heads/main)" "$LOCAL_BEFORE"
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

echo "── F. repo1: code pull selects remote context; next commit preserves both histories"
HEAD_PREV=$(main_head)
git pull -q origin main >/dev/null 2>&1 # code sync (context ref is fetch-only)
session "$TMP/repo1" D
echo d > i.txt; git add i.txt; git commit -qm codeD >/dev/null 2>&1
git push -q origin main >"$TMP/p4.out" 2>&1
expect "selected continuation push succeeds" "$([ "$(grep -c 'remains pending\|ref_conflict' "$TMP/p4.out")" = 0 ] && echo yes)" yes
REBASE=$(curl -sb "$J" "$B/repos/$RID/snapshots" | python3 -c "
import json,sys
snaps={s['id']:s for s in json.load(sys.stdin)}
d=snaps.get('$(main_head)',{})
ps=(d.get('parents') or []) + (d.get('graft_parents') or [])
ok = '$HEAD_PREV' in ps
seen=set(); q=[d.get('id','')]; reach=False
while q:
    c=q.pop()
    if c=='$HEAD_A': reach=True; break
    if c in seen or c not in snaps: continue
    seen.add(c); q.extend((snaps[c].get('parents') or []) + (snaps[c].get('graft_parents') or []))
print('rebased' if ok and reach else f'bad(parents={ps[:1]},grafted={d.get(\"grafted\")},reach={reach})')
")
expect "D continues selected context and retains earlier local work" "$REBASE" rebased

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
# Birth capture requires an observed live owner. An archived newer transcript
# must not become the source just because it was modified most recently.
printf '{"cwd":"%s","session_id":"%s","transcript_path":"%s","prompt":"task E"}\n' "$TMP/repo2" "$APP_SESSION_ID" "$APP_SESSION" | cxt hook --provider claude --event UserPromptSubmit >/dev/null
session "$TMP/repo2" UNOWNED

# A plain desktop-app shell has no cxt supervisor. It must checkpoint and record
# the branch birth independently of the conversation, but keep the vendor-owned native session file open and inject
# only one bounded memory handoff on the next official lifecycle hook.
APP_JSONL_BEFORE=$(find "$PROJ" -maxdepth 1 -type f -name '*.jsonl' | wc -l | tr -d ' ')
git checkout -qb app-feature-x >"$TMP/app-sw.out" 2>&1
expect "app switch checkpoints previous branch" "$([ "$(grep -c 'cxt: checkpoint' "$TMP/app-sw.out")" -ge 1 ] && echo yes)" yes
expect "app switch records a durable branch birth" "$(birth_field app-feature-x kind)" birth
APP_BIRTH_TARGET=$(birth_field app-feature-x target)
expect "app birth captures its registered session" "$(python3 -c 'import json,sys;print(json.load(open(sys.argv[1])).get("session_id",""))' ".cxt/objects/snapshots/${APP_BIRTH_TARGET#sha256:}")" "$APP_SESSION_ID"
expect "app switch retains native session" "$(grep -c 'app session retained' "$TMP/app-sw.out")" 1
expect "app switch does not create wrapper boundary" "$([ ! -e .cxt/boundary.json ] && echo yes)" yes
expect "app switch does not supersede provider files" "$(find "$PROJ" -maxdepth 1 -type f -name '*.superseded' | wc -l | tr -d ' ')" 0
expect "app switch does not materialize orphan native seed" "$(find "$PROJ" -maxdepth 1 -type f -name '*.jsonl' | wc -l | tr -d ' ')" "$APP_JSONL_BEFORE"
expect "app session file remains at original path" "$([ -f "$APP_SESSION" ] && echo yes)" yes
# End the synthetic observation through its supported lifecycle hook. Its
# watcher can otherwise retry publication even when the transcript is idle.
# The next prompt re-registers the same file after this completion boundary.
if ! printf '{"cwd":"%s","session_id":"%s","transcript_path":"%s"}\n' "$TMP/repo2" "$APP_SESSION_ID" "$APP_SESSION" | cxt hook --provider claude --event SessionEnd >"$TMP/app-end.out" 2>&1; then
  cat "$TMP/app-end.out"; FAIL=1; CXT_E2E_KEEP_TMP=1; exit 1
fi
# Earlier Git hooks can still be publishing retained memory attachments. This
# assertion tests one immediate handoff after that known work has completed;
# deterministic application tests separately exercise drift and retained retry.
# Join the real workers, then check their queue under the same publication locks.
# An empty queue alone does not prove an already-running worker has finished.
if ! python3 "$ROOT/scripts/e2e-drain-publication.py" "$TMP/bin/cxt" "$TMP/repo1" "$TMP/repo2" >"$TMP/app-drain.out" 2>&1; then
  cat "$TMP/app-drain.out"
  echo "  ✗ prior publication did not finish; handoff assertions not evaluated"
  FAIL=1
  CXT_E2E_KEEP_TMP=1
  exit 1
fi
expect "prior historical publication finished before immediate handoff" yes yes
APP_HANDOFF=$(echo "{\"cwd\":\"$TMP/repo2\",\"session_id\":\"$APP_SESSION_ID\",\"transcript_path\":\"$APP_SESSION\",\"prompt\":\"continue\"}" | cxt hook --provider claude --event UserPromptSubmit)
printf '%s\n' "$APP_HANDOFF" > "$TMP/app-handoff.json"
expect "app handoff is one bounded project-memory injection" "$(echo "$APP_HANDOFF" | python3 -c "
import json,sys
try:
    text=json.load(sys.stdin)['hookSpecificOutput']['additionalContext']
    prefix='[cxt context package v1]'
    package,end=json.JSONDecoder().raw_decode(text.split(prefix,1)[1].lstrip())
    selected=package['selection']
    proofs=[selected.get('context_delivery_hash',''), selected.get('memory_delivery_hash','')]
    proofs_valid=all(len(p)==71 and p.startswith('sha256:') and all(c in '0123456789abcdef' for c in p[7:]) for p in proofs)
    print('yes' if prefix in text and len(text.encode()) <= 16*1024 and selected['branch']=='main' and selected['source_policy']=='latest_server_main' and selected['working_position']['branch']=='app-feature-x' and selected['repository_id']=='$RID' and proofs_valid and not package.get('historical_evidence') else 'no')
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
cat > "$TMP/bin/claude" <<'SH'
#!/bin/bash
echo "MEMORY_PROFILE ${CXT_CLAUDE_MEMORY_PROFILE:-missing} ${#CXT_CLAUDE_MEMORY_CONFIG_FINGERPRINT} ${CXT_CLAUDE_FLAG_AUTO_MEMORY_DIRECTORY:-default}" >> "$CLAUDELOG"
python3 - "$@" <<'PYARGV' >> "$CLAUDELOG"
import json, sys
print('AGENT ' + json.dumps(sys.argv[1:]))
PYARGV
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
WRAPPER_STARTED=$SECONDS
cxt claude --resume "$APP_SESSION_ID" --settings "{\"autoMemoryDirectory\":\"$CLAUDE_MEMORY_OVERRIDE\"}" >"$TMP/wrapper.out" 2>&1 &
WPID=$!
for i in $(seq 1 40); do
  [ -f .cxt/boundary.json ] && [ "$(grep -c '^AGENT ' "$CLAUDELOG" 2>/dev/null)" -ge 2 ] && break
  sleep 0.25
done
printf 'elapsed_seconds=%s\nwrapper_pid_alive=%s\nboundary_present=%s\n' "$((SECONDS - WRAPPER_STARTED))" "$(kill -0 "$WPID" 2>/dev/null && echo yes || echo no)" "$([ -f .cxt/boundary.json ] && echo yes || echo no)" >"$TMP/wrapper-observation.out"
expect "Checkpoint execution" "$(grep -c 'cxt: checkpoint' "$TMP/sw.out")" 1
expect "wrapper switch records a durable branch birth" "$(birth_field feature-x kind)" birth
# BEGIN wrapper prerequisite gate (negative cases exercise this exact block).
if python3 "$ROOT/scripts/e2e-wrapper-proof.py" "$TMP" "$APP_SESSION_ID" >"$TMP/wrapper-proof.tsv" 2>"$TMP/wrapper-proof.err" &&
  IFS=$'\t' read -r SEED SEEDID RESUMEID PROOF_EXTRA <"$TMP/wrapper-proof.tsv" &&
  [ -n "$SEED" ] && [ -n "$SEEDID" ] && [ -n "$RESUMEID" ] && [ -z "$PROOF_EXTRA" ] &&
  [ "$(cat "$TMP/wrapper-proof.tsv")" = "$(printf '%s\t%s\t%s' "$SEED" "$SEEDID" "$RESUMEID")" ]; then
echo 'wrapper_proof=ready' >>"$TMP/wrapper-observation.out"
expect "boundary signal output" "$(grep -c 'previous session is isolated' "$TMP/sw.out")" 1
expect "seed inherits main compact memory" "$(grep -q 'task A' "$SEED" && grep -q 'task B' "$SEED" && grep -q 'task C' "$SEED" && echo yes)" yes
expect "seed inherits main session conversation" "$(grep -q 'task E' "$SEED" && echo yes)" yes
expect "seed excludes the unregistered sibling conversation" "$(grep -q 'task UNOWNED' "$SEED" && echo yes || echo no)" no
SEEDMEM=$(python3 -c "
import json,glob
raw=open('.cxt/refs/heads/feature-x').read().strip()
tgt=json.loads(raw)['target'] if raw.startswith('{') else raw
for p in glob.glob('.cxt/objects/snapshots/*'):
    s=json.load(open(p))
    if s['id']==tgt: print(s.get('memory_hash','')); break
")
expect "seed snapshot retains full inherited memory object" "$([ -n "$SEEDMEM" ] && [ -f ".cxt/objects/memories/${SEEDMEM#sha256:}" ] && echo yes)" yes
expect "new branch keeps the recorded source snapshot" "$(ref_target .cxt/refs/heads/feature-x)" "$(birth_field feature-x target)"
expect "branch birth pins the inherited memory object" "$(birth_field feature-x memory_hash)" "$SEEDMEM"

# Capture exclusion: a commit before the seed session grows must not capture another session.
# Check the exact commit's durable result even when a background worker wins
# the foreground lock. Finish nocap before growing its previously idle seed.
seed_capture_proof() {
  local mode="$1" sha="$2"
  python3 "$ROOT/scripts/e2e-drain-publication.py" "$TMP/bin/cxt" "$PWD" >"$TMP/g-$mode-drain.out" 2>&1 || {
    cat "$TMP/g-$mode-drain.out"; return 1
  }
  python3 - "$PWD" "$sha" "$SEED" "$SEEDID" "$mode" <<'PYGSEED'
import hashlib,json,pathlib,re,sys
root,sha,seed,session,mode=sys.argv[1:]
cxt=pathlib.Path(root)/'.cxt'
receipts=[json.loads(p.read_text()) for p in cxt.glob('worktrees/*/capture-passes/*.json')]
matches=[p for p in receipts if p['proof']['git_after']==sha and p['proof']['branch']=='feature-x']
assert len(matches)==1, 'expected one receipt for the exact feature-x commit'
p=matches[0]
assert p['version']==2 and all(p.get(k) is True for k in ('complete','inputs_ready','memory_finalized')), 'capture not complete'
proof,obs=p['proof'],p['observation']
assert all(obs.get(k)==proof.get(k) for k in ('repo_id','worktree_id','branch_id','branch','git_after','target')), 'observation position mismatch'
assert re.fullmatch('[0-9a-f]{32}',obs['id']), 'invalid observation ID'
assert json.loads((cxt/'history'/(obs['id']+'.json')).read_text())==obs, 'capture observation not durable'
outcomes=p['outcomes']
assert len(outcomes)==2 and {o['provider'] for o in outcomes}=={'claude','codex'}, 'missing provider outcome'
if mode=='nocap':
    assert all(o['state']=='absent' and not o.get('input') and not o.get('target') for o in outcomes), 'unresumed seed or another session was captured'
    assert proof['target']==p['initial'], 'uncaptured commit changed context'
else:
    saved=[o for o in outcomes if o['state']=='saved']
    assert len(saved)==1 and all(o['state'] in ('saved','absent') for o in outcomes), 'resumed seed was not the sole saved session'
    o=saved[0]; inp=o['input']
    assert pathlib.Path(inp['source_path']).resolve()==pathlib.Path(seed).resolve(), 'captured another session file'
    assert inp['session_id']==o['session_id']==session and inp['provider']==o['provider'], 'captured another session identity'
    assert re.fullmatch('sha256:[0-9a-f]{64}',o['target']) and o['target']==proof['target']!=p['initial'], 'missing new saved target'
    snap=json.loads((cxt/'objects/snapshots'/o['target'][7:]).read_text())
    assert snap['id']==snap['doc_hash']==o['target'] and snap['session_id']==session and snap['provider']==o['provider'], 'saved snapshot identity mismatch'
    assert snap['branch']=='feature-x' and snap['message']==p['message']=='cap [git '+sha[:7]+']', 'saved snapshot lost commit metadata'
    # Read only the fixture's retained native chunks, never a later provider file.
    assert 0<inp['size']<=16<<20 and len(inp['chunks'])==(inp['size']+(1<<20)-1)//(1<<20), 'unexpected synthetic input size'
    raw=bytearray()
    for chunk in inp['chunks']:
        assert re.fullmatch('sha256:[0-9a-f]{64}',chunk['hash']), 'invalid chunk hash'
        assert chunk['size']==min(1<<20,inp['size']-len(raw)), 'invalid chunk size'
        with (cxt/'capture/inputs'/chunk['hash'][7:]).open('rb') as f:
            data=f.read(chunk['size']+1)
        assert len(data)==chunk['size'] and 'sha256:'+hashlib.sha256(data).hexdigest()==chunk['hash'], 'frozen chunk mismatch'
        raw.extend(data)
    assert len(raw)==inp['size'] and 'sha256:'+hashlib.sha256(raw).hexdigest()==inp['hash'], 'frozen input mismatch'
    last=[json.loads(line) for line in raw.splitlines() if line.strip()][-1]
    if o['provider']=='claude':
        assert last['type']=='user' and last['message']=={'role':'user','content':'resumed work'}, 'resumed message missing from frozen input'
    else:
        assert last['type']=='event_msg' and last['payload']=={'type':'user_message','message':'resumed work'}, 'resumed message missing from frozen input'
print('exact commit capture proof verified: '+mode)
PYGSEED
}
echo x > x.txt; git add x.txt; git commit -qm nocap >"$TMP/nc.out" 2>&1
G_NOCAP_SHA=$(git rev-parse HEAD)
if ! seed_capture_proof nocap "$G_NOCAP_SHA" >"$TMP/g-nocap-proof.out" 2>&1; then
  cat "$TMP/g-nocap-proof.out"; FAIL=1; CXT_E2E_KEEP_TMP=1; exit 1
fi
expect "unresumed seed is not captured" yes yes
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
G_CAP_SHA=$(git rev-parse HEAD)
if ! seed_capture_proof cap "$G_CAP_SHA" >"$TMP/g-cap-proof.out" 2>&1; then
  cat "$TMP/g-cap-proof.out"; FAIL=1; CXT_E2E_KEEP_TMP=1; exit 1
fi
expect "resumed seed captures" yes yes

# Enforcement: boundary-enforce terminates processes that still hold isolated session files.
# BEGIN wrapper holder readiness.
SUP="$APP_SESSION.superseded"
python3 - "$SUP" "$TMP/holder-ready" <<'PYHOLDER' >"$TMP/holder.out" 2>&1 &
import os, sys, time
with open(sys.argv[1], 'rb') as held:
    with open(sys.argv[2], 'x') as ready:
        ready.write(str(os.getpid()))
    time.sleep(30)
PYHOLDER
TPID=$!
holder_ready() {
  [ -f "$TMP/holder-ready" ] && [ "$(cat "$TMP/holder-ready")" = "$TPID" ] && fixture_job_active "$TPID"
}
for i in $(seq 1 40); do
  holder_ready && break
  fixture_job_active "$TPID" || break
  sleep 0.025
done
if holder_ready; then
  cxt git-hook boundary-enforce >/dev/null 2>&1
  sleep 0.3
  HOLDER_EXIT=running
  if ! fixture_job_active "$TPID"; then
    wait "$TPID" 2>/dev/null
    HOLDER_EXIT=$?
  fi
  expect "isolated session holder terminated by SIGTERM" "$HOLDER_EXIT" 143
else
  echo "  ✗ superseded-file holder did not become ready; enforcement not evaluated"
  FAIL=1
fi
if ! finish_fixture_job "$TPID"; then exit 1; fi
TPID=""
# END wrapper holder readiness.

# The first fake child already caused the transition. The wrapper must observe
# that boundary and restart a second child with the newly materialized seed.
expect "wrapper restarts with the exact seed selector" "$RESUMEID" "$SEEDID"
expect "wrapper carries proven Claude memory profile" "$(grep -c '^MEMORY_PROFILE v1 64 ' "$CLAUDELOG")" 2
expect "both children retain the invocation's custom Claude memory directory" "$(grep -Fxc "MEMORY_PROFILE v1 64 $CLAUDE_MEMORY_OVERRIDE" "$CLAUDELOG")" 2
expect "restart does not silently revert the invocation to default settings" "$(grep -c '^MEMORY_PROFILE v1 64 default$' "$CLAUDELOG")" 0
else
  FAIL=1
  echo 'wrapper_proof=failed' >>"$TMP/wrapper-observation.out"
  echo "  ✗ wrapper prerequisites missing or invalid; dependent assertions not evaluated"
fi
# END wrapper prerequisite gate.
if [ "$FAIL" -ne 0 ]; then
  CXT_E2E_KEEP_TMP=1
  for diagnostic in "$TMP/wrapper-proof.err" "$TMP/sw.out" "$TMP/wrapper.out" "$CLAUDELOG"; do
    echo "  $(basename "$diagnostic"):"
    if [ -f "$diagnostic" ]; then tail -c 8192 "$diagnostic"; else echo "  unavailable"; fi
  done
fi
if ! finish_fixture_job "$WPID"; then exit 1; fi
WPID=""

echo "── H. Saved load mode cannot override managed latest-main input"
expect "load_mode saved(memory)" "$(ccurl -sb "$J" -X PATCH "$B/me" -H 'Content-Type: application/json' -d '{"load_mode":"memory"}' | jget "['load_mode']")" memory
expect "GET /me reflects" "$(curl -sb "$J" "$B/me" | jget "['load_mode']")" memory
expect "Invalid value 422" "$(ccurl -sb "$J" -o /dev/null -w '%{http_code}' -X PATCH "$B/me" -H 'Content-Type: application/json' -d '{"load_mode":"bogus"}')" 422
# Saved preferences remain readable for compatibility. Managed load ignores
# them, injects latest main, and leaves instruction files untouched. Explicit
# --mode memory separately exercises historical archive restoration.
cd "$TMP/repo1"
cat > CLAUDE.md <<'EOF'
# User-owned instructions
Preserve this text and its file mode.
EOF
chmod 600 CLAUDE.md
cxt load main --provider claude >"$TMP/pref.out" 2>&1
expect "managed load prepares bounded memory" "$(grep -c 'fidelity: memory' "$TMP/pref.out")" 1
expect "saved mode cannot replace managed main input" "$(python3 - "$TMP/pref.out" "$(main_head)" "$(git branch --show-current)" <<'PYPREF'
import json,pathlib,sys
output=pathlib.Path(sys.argv[1]).read_text()
path=next(line.split('written: ',1)[1] for line in output.splitlines() if 'written: ' in line)
found=False
for line in pathlib.Path(path).read_text().splitlines():
    row=json.loads(line)
    message=row.get('message',{})
    if message.get('role')!='user': continue
    content=message.get('content',[])
    texts=[content] if isinstance(content,str) else [part.get('text','') for part in content if isinstance(part,dict)]
    for text in texts:
        if not text.startswith('[cxt context package v1]\n'): continue
        selection=json.loads(text.split('\n',1)[1])['selection']
        found=(selection['source_policy']=='latest_server_main' and selection['branch']=='main'
            and selection['snapshot_id']==sys.argv[2] and selection['working_position']['branch']==sys.argv[3])
print('yes' if found else 'no')
PYPREF
)" yes
expect "managed load leaves instruction bytes unchanged" "$(python3 -c "from pathlib import Path; print('yes' if Path('CLAUDE.md').read_text() == '# User-owned instructions\nPreserve this text and its file mode.\n' else 'no')")" yes
cxt load main --provider claude --mode memory >"$TMP/legacy-memory.out" 2>&1
expect "explicit archive memory restoration stays available" "$(grep -c 'fidelity: memory' "$TMP/legacy-memory.out")" 1
cxt load main --provider claude --mode memory >/dev/null 2>&1
expect "memory load preserves user instructions" "$(python3 -c "from pathlib import Path; print('yes' if Path('CLAUDE.md').read_text().startswith('# User-owned instructions\nPreserve this text and its file mode.\n') else 'no')")" yes
expect "CLI memory uses server-assessed claims" "$(grep -c 'Assessment: server_assessed' CLAUDE.md)" 1
expect "CLI keeps retained rationale from shared query" "$(grep -c '\[retained; rationale;' CLAUDE.md)" 1
expect "CLI assessment names target worktree code" "$(grep -c "Selected committed code: $(git rev-parse HEAD)" CLAUDE.md)" 1
expect "CLI repeated load replaces assessment" "$(grep -c '^<!-- cxt:code-assessment:v1 -->' CLAUDE.md)" 1
expect "memory load refreshes one managed block" "$(grep -c '^<!-- cxt:begin managed memory' CLAUDE.md)" 1
expect "memory managed block stays within 64 KiB" "$(python3 -c "from pathlib import Path; b=Path('CLAUDE.md').read_bytes(); s=b.index(b'<!-- cxt:begin managed memory'); e=b.index(b'<!-- cxt:end managed memory -->', s)+len(b'<!-- cxt:end managed memory -->')+1; print('yes' if e-s <= 64*1024 else 'no')")" yes
expect "memory load preserves instruction file mode" "$(python3 -c "from pathlib import Path; print(oct(Path('CLAUDE.md').stat().st_mode & 0o777))")" 0o600
ccurl -sb "$J" -X PATCH "$B/me" -H 'Content-Type: application/json' -d '{"load_mode":""}' >/dev/null

echo "── I. Explicit Git start points and branch births preserve code authority"
cd "$TMP/repo1"
# Save each foreign owner before creating an independent same-name local birth.
capture_i_server_fork() {
  curl -fsSb "$J" "$B/repos/$RID/refs" -o "$TMP/i-fork-refs.json" || return 1
  python3 - "$TMP/i-fork-refs.json" "$RID" "$1" "$TMP/i-server-$1.json" <<'PYIFORK'
import json,pathlib,re,sys
rows=json.loads(pathlib.Path(sys.argv[1]).read_text())
matches=[r for r in rows if r['kind']=='branch' and r['name']==sys.argv[3]]
assert len(matches)==1, 'missing or duplicate server fork'
r=matches[0]
assert r['repo_id']==sys.argv[2] and re.fullmatch(r'[0-9a-f]{32}',r.get('branch_id','')), 'invalid server fork identity'
assert re.fullmatch(r'sha256:[0-9a-f]{64}',r['target']), 'invalid server fork target'
pathlib.Path(sys.argv[4]).write_text(json.dumps({k:r[k] for k in ('name','branch_id','target')})+'\n')
PYIFORK
}
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
if ! capture_i_server_fork web-fork-x; then
  FAIL=1; CXT_E2E_KEEP_TMP=1; exit 1
fi
# Create an unpushed commit so HEAD(Y) advances beyond fork point(X).
echo z > z.txt; git add z.txt; git commit -qm ahead >/dev/null 2>&1
git branch web-fork-x "$FORK_SHA"
cxt git-hook branch-replay
FORK_LOCAL=$(birth_field web-fork-x target)
WFX_ID=$(birth_field web-fork-x branch_id)
expect "helper preserves the explicit Git start point [git X]" "$(git rev-parse --short=7 web-fork-x)" "$(git rev-parse --short=7 "$FORK_SHA")"
expect "context ref is connected to fork snapshot" "$(ref_target .cxt/refs/heads/web-fork-x 2>/dev/null)" "$FORK_LOCAL"
# Switching should materialize the fork context; a fork-only ref represents an existing branch.
session "$TMP/repo1" WFX
git checkout -q web-fork-x >"$TMP/wfx.out" 2>&1
expect "switch does not create seed" "$(grep -c 'seed created' "$TMP/wfx.out")" 0
expect "fork context is selected without replacing app session" "$(grep -c 'app context selected' "$TMP/wfx.out")" 1
expect "ref is still fork snapshot after switch" "$(ref_target .cxt/refs/heads/web-fork-x)" "$FORK_LOCAL"
git checkout -q main >/dev/null 2>&1
# A same-named web fork is not an upstream binding. Local creation records
# its actual current source; the helper must never move Git to a guessed tip.
ccurl -sb "$J" -X POST "$B/repos/$RID/fork" -H 'Content-Type: application/json' \
  -d "{\"from\":\"$FORK_FROM\",\"new_branch\":\"web-fork-y\",\"author\":{\"name\":\"E2E\",\"email\":\"e2e@test.local\",\"team\":\"\"}}" >/dev/null
if ! capture_i_server_fork web-fork-y; then
  FAIL=1; CXT_E2E_KEEP_TMP=1; exit 1
fi
session "$TMP/repo1" WFY
# A real app reports the new conversation through its official prompt hook.
printf '{"cwd":"%s","session_id":"sess-WFY","transcript_path":"%s","prompt":"continue"}\n' "$TMP/repo1" "$D/sess-WFY.jsonl" | cxt hook --provider claude --event UserPromptSubmit >/dev/null
git checkout -qb web-fork-y >"$TMP/wfy.out" 2>&1
expect "switch -c records its own observed birth" "$(birth_field web-fork-y kind)" birth
WFY_BIRTH_TARGET=$(birth_field web-fork-y target)
WFY_ID=$(birth_field web-fork-y branch_id)
expect "branch birth captures the exact official hook session" "$(python3 -c 'import json,sys;print(json.load(open(sys.argv[1])).get("session_id",""))' ".cxt/objects/snapshots/${WFY_BIRTH_TARGET#sha256:}")" sess-WFY
expect "new branch uses its live source context" "$([ "$(ref_target .cxt/refs/heads/web-fork-y)" != "$FORK_FROM" ] && echo yes)" yes
cxt git-hook branch-replay
expect "replaying birth never overwrites the selected source" "$([ "$(ref_target .cxt/refs/heads/web-fork-y)" != "$FORK_FROM" ] && echo yes)" yes
git checkout -q main >/dev/null 2>&1
if ! printf '{"cwd":"%s","session_id":"sess-WFY","transcript_path":"%s"}\n' "$TMP/repo1" "$D/sess-WFY.jsonl" | cxt hook --provider claude --event SessionEnd >"$TMP/fork-end.out" 2>&1; then
  cat "$TMP/fork-end.out"; FAIL=1; CXT_E2E_KEEP_TMP=1; exit 1
fi

echo "── I. Branch lifecycle: rename transfers context; deletion archives without deleting history"
RENAME_OUT=$(git branch -m web-fork-x web-fork-renamed 2>&1)
expect "Git branch rename transfers context projection" "$(echo "$RENAME_OUT" | grep -c 'context moved')" 1
expect "renamed context keeps its exact target" "$(ref_target .cxt/refs/heads/web-fork-renamed 2>/dev/null)" "$FORK_LOCAL"
expect "renamed source context projection is gone" "$([ ! -e .cxt/refs/heads/web-fork-x ] && echo yes)" yes
expect "rename keeps all context objects readable" "$(cxt fsck | grep -c 'Missing 0')" 1

DELETE_OUT=$(git branch -D web-fork-renamed 2>&1)
expect "normal branch deletion invokes context archive" "$(echo "$DELETE_OUT" | grep -c 'context archived')" 1
expect "deleted Git branch has no active context projection" "$([ ! -e .cxt/refs/heads/web-fork-renamed ] && echo yes)" yes
expect "archive keeps the target snapshot readable" "$(cxt fsck | grep -c 'Missing 0')" 1
expect "archive records an immutable lifecycle event" "$(find .cxt/refs/tags/cxt/branch-state/v1 -type f -path '*/archived/*/web-fork-renamed' 2>/dev/null | wc -l | tr -d ' ')" 1

# A separate identity must be accepted before its lifecycle can remove server refs.
# Creation, rename and deletion still pass through the real installed Git hooks.
if ! git branch lifecycle-owned >"$TMP/i-owned.out" 2>&1 ||
   ! cxt branch replay >>"$TMP/i-owned.out" 2>&1 ||
   ! cxt push origin lifecycle-owned >>"$TMP/i-owned.out" 2>&1 ||
   ! curl -fsSb "$J" "$B/repos/$RID/refs" -o "$TMP/i-owned-refs.json"; then
  cat "$TMP/i-owned.out"; FAIL=1; CXT_E2E_KEEP_TMP=1; exit 1
fi
OWNED_ID=$(birth_field lifecycle-owned branch_id)
OWNED_TARGET=$(ref_target .cxt/refs/heads/lifecycle-owned)
if ! python3 - "$TMP/i-owned-refs.json" "$RID" "$OWNED_ID" "$OWNED_TARGET" <<'PYIOWNED'
import json,pathlib,re,sys
rows=json.loads(pathlib.Path(sys.argv[1]).read_text())
matches=[r for r in rows if r['kind']=='branch' and r['name']=='lifecycle-owned']
assert len(matches)==1 and re.fullmatch(r'[0-9a-f]{32}',sys.argv[3]), 'owned birth missing'
assert re.fullmatch(r'sha256:[0-9a-f]{64}',sys.argv[4]), 'invalid owned target'
r=matches[0]
assert (r['repo_id'],r.get('branch_id'),r['target'])==tuple(sys.argv[2:5]), 'owned birth not accepted exactly'
PYIOWNED
then
  FAIL=1; CXT_E2E_KEEP_TMP=1; exit 1
fi
# Like the existing I controls, capture the inherited output pipe so the
# asynchronous local finalizer completes before the next Git mutation/assertion.
if ! OWNED_RENAME_OUT=$(git branch -m lifecycle-owned lifecycle-owned-renamed 2>&1); then
  printf '%s\n' "$OWNED_RENAME_OUT" >>"$TMP/i-owned.out"
  cat "$TMP/i-owned.out"; FAIL=1; CXT_E2E_KEEP_TMP=1; exit 1
fi
printf '%s\n' "$OWNED_RENAME_OUT" >>"$TMP/i-owned.out"
if ! OWNED_DELETE_OUT=$(git branch -D lifecycle-owned-renamed 2>&1); then
  printf '%s\n' "$OWNED_DELETE_OUT" >>"$TMP/i-owned.out"
  cat "$TMP/i-owned.out"; FAIL=1; CXT_E2E_KEEP_TMP=1; exit 1
fi
printf '%s\n' "$OWNED_DELETE_OUT" >>"$TMP/i-owned.out"
expect "owned archive removes both local name projections" "$([ ! -e .cxt/refs/heads/lifecycle-owned ] && [ ! -e .cxt/refs/heads/lifecycle-owned-renamed ] && echo yes)" yes

# Detached HEAD used to return before reading deletion transactions. Delete a
# second branch while detached to exercise the real hook and zeros→zeros Git
# transaction form, not only the parser unit test.
git checkout -q --detach HEAD >/dev/null 2>&1
DETACHED_DELETE_OUT=$(git branch -D web-fork-y 2>&1)
expect "detached branch deletion still invokes context archive" "$(echo "$DETACHED_DELETE_OUT" | grep -c 'context archived')" 1
expect "detached deletion removes only the active projection" "$([ ! -e .cxt/refs/heads/web-fork-y ] && echo yes)" yes
expect "detached archive preserves immutable history" "$(cxt fsck | grep -c 'Missing 0')" 1

# Freeze exact local lifecycle evidence; never infer identity from a shared name.
if ! python3 - "$TMP" "$RID" "$WFX_ID" "$WFY_ID" "$OWNED_ID" <<'PYILOCAL'
import json,pathlib,re,sys
tmp=pathlib.Path(sys.argv[1]); repo=sys.argv[2]
rows=[json.loads(p.read_text()) for p in pathlib.Path('.cxt/history').glob('*.json')]
def event(identity,kind,name):
    found=[e for e in rows if e['branch_id']==identity and e['kind']==kind and e['branch']==name]
    assert len(found)==1, 'missing or duplicate local lifecycle event'
    e=found[0]
    assert e['repo_id']==repo and re.fullmatch(r'[0-9a-f]{32}',e['id']), 'invalid local event identity'
    return e
forks=[]; conflicted=[]; owned=[]
for identity,name,renamed in ((sys.argv[3],'web-fork-x','web-fork-renamed'),
                              (sys.argv[4],'web-fork-y',''),
                              (sys.argv[5],'lifecycle-owned','lifecycle-owned-renamed')):
    birth=event(identity,'birth',name)
    assert birth['id']==identity, 'birth must establish the exact modern identity'
    chain=[birth]
    if renamed:
        rename=event(identity,'rename',renamed)
        assert rename['previous_branch']==name and rename['binding_parent']==birth['id'], 'broken rename chain'
        chain.append(rename)
    archive=event(identity,'archive',renamed or name)
    assert archive['binding_parent']==chain[-1]['id'], 'broken archive chain'
    chain.append(archive)
    assert all(e.get('source')==birth['target'] and e.get('target')==birth['target'] for e in chain[1:]), 'lifecycle target changed'
    if name=='lifecycle-owned':
        owned=chain
    else:
        foreign=json.loads((tmp/('i-server-'+name+'.json')).read_text())
        assert foreign['branch_id']!=identity, 'fixture did not create independent identities'
        forks.append(foreign)
        conflicted.extend(e['id'] for e in chain)
assert len({e['id'] for e in owned}|set(conflicted))==8, 'lifecycle IDs overlap'
(tmp/'i-lifecycle-proof.json').write_text(json.dumps({'forks':forks,'conflicted':conflicted,'conflicted_branch_ids':sys.argv[3:5],'owned':owned})+'\n')
PYILOCAL
then
  FAIL=1; CXT_E2E_KEEP_TMP=1; exit 1
fi

# The archive helpers publish asynchronously. Same budget, exact identities;
# protocol 1 applies immutable history, not legacy lifecycle-tag counts.
for i in $(seq 1 40); do
  if ! curl -fsSb "$J" "$B/repos/$RID/refs" -o "$TMP/i-server-refs.json" ||
     ! curl -fsSb "$J" "$B/repos/$RID/history" -o "$TMP/i-server-history.json"; then
    SERVER_ARCHIVE_STATE=unreadable; break
  fi
  if ! SERVER_ARCHIVE_STATE=$(python3 - "$TMP" <<'PYIREMOTE'
import json,pathlib,sys
tmp=pathlib.Path(sys.argv[1])
proof=json.loads((tmp/'i-lifecycle-proof.json').read_text())
refs=json.loads((tmp/'i-server-refs.json').read_text())
events=json.loads((tmp/'i-server-history.json').read_text())
assert isinstance(refs,list) and isinstance(events,list), 'invalid server metadata'
branches=[r for r in refs if r['kind']=='branch']
history={e['id']:e for e in events}
assert len(history)==len(events), 'duplicate server history IDs'
for old in proof['forks']:
    matches=[r for r in branches if r['name']==old['name']]
    assert len(matches)==1 and all(matches[0].get(k)==v for k,v in old.items()), 'foreign server owner or target changed'
assert not set(proof['conflicted']).intersection(history), 'conflicting local lifecycle was accepted'
local_ids=set(proof['conflicted_branch_ids'])
assert not any(r['name']=='web-fork-renamed' or r.get('branch_id') in local_ids for r in branches), 'conflicting local identity was projected'
for expected in proof['owned']:
    if expected['id'] in history:
        assert history[expected['id']]==expected, 'accepted lifecycle payload changed'
owned_id=proof['owned'][0]['branch_id']
pending_ref=any(r['name'] in ('lifecycle-owned','lifecycle-owned-renamed') or r.get('branch_id')==owned_id for r in branches)
print('ready' if all(e['id'] in history for e in proof['owned']) and not pending_ref else 'waiting')
PYIREMOTE
); then
    SERVER_ARCHIVE_STATE=invalid-evidence; break
  fi
  [ "$SERVER_ARCHIVE_STATE" = ready ] && break
  sleep 0.25
done
expect "server preserves foreign owners and accepts only the owned lifecycle chain" "$SERVER_ARCHIVE_STATE" ready
if [ "$SERVER_ARCHIVE_STATE" != ready ]; then CXT_E2E_KEEP_TMP=1; fi

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
# Reuse the fixture's existing main identity and exact local code selection.
# A fresh clone can legitimately have a different code from the shared context tip.
cd "$TMP/repo1"
if ! CXT_KEEP_SESSION=1 git checkout -q main >"$TMP/big-select.out" 2>&1; then
  cat "$TMP/big-select.out"; exit 1
fi
python3 - "$(git rev-parse HEAD)" "$(git rev-parse --absolute-git-dir)" >"$TMP/big-binding.out" 2>&1 <<'PYBIGBINDING'
import hashlib,json,pathlib,sys
root=pathlib.Path('.')
code,admin=sys.argv[1:]
wt=hashlib.sha256(admin.encode()).hexdigest()[:32]
ref=json.loads((root/'.cxt/refs/heads/main').read_text())
p=json.loads((root/'.cxt/worktrees'/wt/'position.json').read_text())
assert ref.get('branch_id') and ref.get('kind')=='branch' and ref.get('name')=='main', 'missing canonical main identity'
assert p.get('repo_id')==ref.get('repo_id') and p.get('branch')=='main' and p.get('branch_id')==ref['branch_id'], 'main selection identity mismatch'
assert p.get('git_commit')==code and p.get('snapshot')==ref.get('target') and ref.get('target'), 'main code/context selection mismatch'
events=[json.loads(f.read_text()) for f in (root/'.cxt/history').glob('*.json')]
assert any(e.get('kind') in ('position','advance','birth','orphan','attach') and e.get('repo_id')==ref['repo_id']
           and e.get('branch_id')==ref['branch_id'] and e.get('target')==ref['target'] and e.get('git_after')==code
           for e in events), 'main has no exact stored code binding'
print('verified main code/context binding')
PYBIGBINDING
if [ "$?" != 0 ]; then cat "$TMP/big-binding.out"; exit 1; fi
large_session "$TMP/repo1" BIG
echo big > big.txt; git add big.txt; git commit -qm big-event >/dev/null 2>&1
# The user can push immediately, before background normalization finishes.
git push -q origin main >"$TMP/big-push.out" 2>&1 || { cat "$TMP/big-push.out"; exit 1; }
# Large sealed input is normalized outside the Git hook deadline. Wait for its
# actual worker/publication chain; never substitute a second live capture.
if ! python3 "$ROOT/scripts/e2e-drain-publication.py" "$TMP/bin/cxt" "$TMP/repo1" >"$TMP/big-drain.out" 2>&1; then
  cat "$TMP/big-drain.out"; exit 1
fi
python3 - "$(git rev-parse HEAD)" <<'PYBIGCAPTURE'
import json,pathlib,sys
passes=[json.loads(p.read_text()) for p in pathlib.Path('.cxt/worktrees').glob('*/capture-passes/*.json')]
p=[p for p in passes if p['proof']['git_after']==sys.argv[1]]
assert len(p)==1 and p[0].get('inputs_ready') and p[0].get('memory_finalized') and p[0].get('complete'), 'large frozen capture did not complete'
assert any(o.get('state')=='saved' and o.get('input',{}).get('size',0)>1024*1024 for o in p[0]['outcomes']), 'large source was not saved from sealed input'
ref=json.loads(pathlib.Path('.cxt/refs/heads/main').read_text())
assert p[0]['proof']['target']==ref['target'], 'background capture did not advance its unchanged local branch'
PYBIGCAPTURE
if [ "$?" != 0 ]; then exit 1; fi
BIG_EXPECTED=$(python3 - <<'PYBIGEXPECTED'
import json,re
with open('.cxt/refs/heads/main') as f:
    ref=json.load(f)
target=ref.get('target','')
assert ref.get('branch_id') and re.fullmatch(r'sha256:[0-9a-f]{64}',target), 'missing captured context identity/target'
print(target)
PYBIGEXPECTED
) || exit 1
BIG_HEAD=$(main_head)
expect "oversized captured context is the published main" "$BIG_HEAD" "$BIG_EXPECTED"
[ "$BIG_HEAD" = "$BIG_EXPECTED" ] || { cat "$TMP/big-push.out"; exit 1; }
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
if ! cxt pull >"$TMP/big-pull.out" 2>&1; then cat "$TMP/big-pull.out"; exit 1; fi
expect "fresh client pulls and verifies v2 history" "$(cxt fsck | grep -c 'Missing 0')" 1

echo
source "$ROOT/scripts/e2e-context-history.inc.sh"
source "$ROOT/scripts/e2e-publication.inc.sh"

source "$ROOT/scripts/e2e-live-capture.inc.sh"

echo "── R. Exact memory-body reuse through the real CLI client and API"
REUSE_TOKEN=$(python3 - "$J" <<'PYREUSE'
import pathlib,sys
for line in pathlib.Path(sys.argv[1]).read_text().splitlines():
    fields=line.split('\t')
    if len(fields)==7 and fields[5]=='cxt_session':
        print(fields[6]); break
PYREUSE
)
CXT_MEMORY_REUSE_TEST_URL="$B" CXT_MEMORY_REUSE_TEST_TOKEN="$REUSE_TOKEN" \
  CXT_BRANCH_PULL_TEST_REQUIRED="$([ -n "${CXT_E2E_DSN:-}" ] && echo 1 || echo 0)" \
  CXT_MEMORY_REUSE_TEST_REPO="$RID" "$TMP/bin/memory-reuse-test" \
  -test.run '^(TestMemoryReuseLiveProtocol|TestBranchPullLiveProtocol|TestCatalogMerkleLiveProtocol)$' -test.v >"$TMP/memory-reuse.out" 2>&1
REUSE_EXIT=$?
if [ "$REUSE_EXIT" != 0 ]; then cat "$TMP/memory-reuse.out"; fi
expect "memory reuse, branch and Merkle wire protocols verified by server" "$REUSE_EXIT" 0
unset REUSE_TOKEN

source "$ROOT/scripts/e2e-initialization.inc.sh"
source "$ROOT/scripts/e2e-setup.inc.sh"
source "$ROOT/scripts/e2e-first-owner.inc.sh"

if [ "$FAIL" = 0 ]; then echo "SYNC E2E: All passed ✓"; else echo "SYNC E2E: Failures exist ✗"; fi
exit "$FAIL"
