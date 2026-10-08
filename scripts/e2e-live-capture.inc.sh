# Sourced by e2e-sync.sh: exercises the installed lifecycle hook -> detached
# observer -> fresh CLI capture -> backend path, without a Stop event.
echo "── Live app capture during an unfinished turn"
# Earlier scenarios deliberately retain rejected history in repo1. Use a fresh
# replica of the same code/server repository, never flush that unrelated outbox
# or suppress its errors while seeding the memory under test.
LIVE_REPO="$TMP/live-memory-repo"
if ! LIVE_EXPECTED_CODE=$(git -C "$TMP/repo1" rev-parse HEAD); then FAIL=1; return; fi
if ! git clone -q --branch main "$TMP/bare.git" "$LIVE_REPO" >"$TMP/live-clone.out" 2>&1; then
  cat "$TMP/live-clone.out"; FAIL=1; CXT_E2E_KEEP_TMP=1; return
fi
cd "$LIVE_REPO" || { FAIL=1; return; }
if ! cxt init >"$TMP/live-connect.out" 2>&1 ||
   ! cxt remote add origin "$REMOTE" >>"$TMP/live-connect.out" 2>&1 ||
   ! cxt pull >>"$TMP/live-connect.out" 2>&1; then
  cat "$TMP/live-connect.out"; FAIL=1; CXT_E2E_KEEP_TMP=1; return
fi
LIVE_WORKTREE=$(git rev-parse --absolute-git-dir | python3 -c 'import hashlib,sys;print(hashlib.sha256(sys.stdin.read().strip().encode()).hexdigest()[:32])')
LIVE_POSITION=".cxt/worktrees/$LIVE_WORKTREE/position.json"
if ! LIVE_PULL_SOURCE=$(python3 - "$LIVE_POSITION" "$RID" "$(main_head)" "$(ref_target .cxt/refs/heads/main)" "$(git rev-parse HEAD)" "$LIVE_EXPECTED_CODE" <<'PY'
import json,re,sys
p=json.load(open(sys.argv[1]))
assert p['repo_id']==sys.argv[2] and p['branch']=='main', 'pull did not select this repository main'
assert re.fullmatch(r'sha256:[0-9a-f]{64}',p['snapshot']), 'invalid pulled snapshot'
assert p['snapshot']==sys.argv[3]==sys.argv[4], 'pulled selection/local/server main mismatch'
assert p['git_commit']==sys.argv[5]==sys.argv[6], 'pulled selection does not match repo1 Git HEAD'
print(p['snapshot'])
PY
); then FAIL=1; CXT_E2E_KEEP_TMP=1; return; fi
# Seed an explicit, selected claim through the ordinary local distiller and
# publisher. No provider/model turn is involved. The default server admission
# remains unchanged: this fixture covers both providers with legacy documents.
cat > "$TMP/live-memory-claims.json" <<'JSON'
[{"kind":"rationale","text":"Preserve this selected memory through live capture and transcript growth."}]
JSON
if ! cxt memorize --claims "$TMP/live-memory-claims.json" >"$TMP/live-memorize.out" 2>&1 ||
   ! cxt push >"$TMP/live-seed-push.out" 2>&1; then
  cat "$TMP/live-memorize.out" "$TMP/live-seed-push.out" 2>/dev/null
  FAIL=1; CXT_E2E_KEEP_TMP=1; return
fi
# A concurrent ordinary push could repair the very omission under test. Join
# fixture publishers before starting these observers; never push during them.
# The helper joins all running processes for this fixture binary, but only
# drains/checks this new replica's queues, leaving intentional failures intact.
if ! python3 "$ROOT/scripts/e2e-drain-publication.py" "$TMP/bin/cxt" "$LIVE_REPO" >"$TMP/live-drain.out" 2>&1; then
  cat "$TMP/live-drain.out"
  FAIL=1; CXT_E2E_KEEP_TMP=1; return
fi
if ! cp "$LIVE_POSITION" "$TMP/live-selected-position.json"; then FAIL=1; return; fi
LIVE_SHARED_BEFORE=$(main_head)
LIVE_LOCAL_BEFORE=$(ref_target .cxt/refs/heads/main)
LIVE_CODE_BEFORE=$(git rev-parse HEAD)
if ! LIVE_SOURCE=$(python3 - "$TMP/live-selected-position.json" "$RID" "$LIVE_SHARED_BEFORE" "$LIVE_LOCAL_BEFORE" "$LIVE_CODE_BEFORE" "$LIVE_PULL_SOURCE" <<'PY'
import json,re,sys
p=json.load(open(sys.argv[1]))
assert p['repo_id']==sys.argv[2] and p['branch']=='main' and p['memory_pinned'], 'missing selected main memory'
assert re.fullmatch(r'sha256:[0-9a-f]{64}',p['snapshot']), 'invalid selected snapshot'
assert re.fullmatch(r'sha256:[0-9a-f]{64}',p['memory_hash']), 'invalid selected memory'
assert p['snapshot']==sys.argv[3]==sys.argv[4]==sys.argv[6], 'selected/local/server/pulled main mismatch'
assert p['git_commit']==sys.argv[5], 'selected code does not match the fresh clone'
print(p['snapshot'])
PY
); then FAIL=1; CXT_E2E_KEEP_TMP=1; return; fi
expect "live memory seed is the selected shared main" "$LIVE_SHARED_BEFORE" "$LIVE_SOURCE"
expect "live memory seed is the selected local main" "$LIVE_LOCAL_BEFORE" "$LIVE_SOURCE"

live_memory_proof() {
  local label="$1" target="$2" provider="$3" sid="$4"
  [ -n "$target" ] || return 1
  curl -fsSb "$J" "$B/repos/$RID/snapshots/$target" >"$TMP/live-$label-snapshot.json" || return 1
  curl -fsSb "$J" "$B/repos/$RID/memories/$target" >"$TMP/live-$label-memory.json" || return 1
  python3 - "$TMP" "$RID" "$LIVE_SOURCE" "$LIVE_POSITION" "$label" "$target" "$provider" "$sid" <<'PY'
import json,pathlib,re,sys
tmp,repo,source,position,label,target,provider,sid=sys.argv[1:]
tmp=pathlib.Path(tmp)
def read(path):
    return json.loads(pathlib.Path(path).read_text())
selected=read(tmp/'live-selected-position.json')
assert read(position)==selected, 'pending capture moved the selected worktree position'
local=read(pathlib.Path('.cxt/objects/snapshots')/target.removeprefix('sha256:'))
remote=read(tmp/f'live-{label}-snapshot.json')
memory=read(tmp/f'live-{label}-memory.json')
assert local['id']==target and local['doc_hash']==target and local['repo_id']==repo, 'wrong local target'
assert re.fullmatch(r'sha256:[0-9a-f]{64}',local.get('memory_hash','')), 'local attachment missing'
for key in ('id','repo_id','doc_hash','memory_hash','parents','provider','session_id'):
    assert remote.get(key)==local.get(key), f'server snapshot mismatch: {key}'
assert not local.get('doc_identity') and not remote.get('doc_identity'), 'expected legacy fixture'
assert memory['snapshot_id']==target, 'memory attached to another snapshot'
assert memory.get('claims_version')==1, 'typed claim version lost'
claims=read(tmp/'live-memory-claims.json')
assert sum(f.get('source_snapshot')==source and c==claims[0]
           for f in memory.get('fragments',[]) for c in f.get('claims',[]))==1, 'selected claim provenance lost or duplicated'
if label=='seed':
    assert target==source and local['memory_hash']==selected['memory_hash'], 'seed is not the selected attachment'
else:
    assert target!=source and local['parents']==[source], 'capture changed the selected ancestry'
    assert local['provider']==provider and local['session_id']==sid, 'wrong captured session'
    assert local['memory_hash']!=selected['memory_hash'], 'inherited digest was not rebound to the capture'
    # Inheritance changes the attachment owner/provider, retaining all source
    # fragments and rendered content. A new snapshot starts its own causal chain
    # and cannot inherit the source snapshot's graft-coverage certificate.
    expected=read(tmp/'live-seed-memory.json')
    expected.update(snapshot_id=target,provider=provider)
    expected.pop('previous_memory_hash',None)
    expected.pop('graft_coverage',None)
    assert memory==expected, 'inherited memory differs from the selected digest'
print('verified exact attachment and selected source')
PY
}
if ! live_memory_proof seed "$LIVE_SOURCE" "" "" >"$TMP/live-seed-proof.out" 2>&1; then
  cat "$TMP/live-seed-proof.out"
  FAIL=1; CXT_E2E_KEEP_TMP=1; return
fi
expect "selected memory seed is published before capture" yes yes
for LIVE_PROVIDER in claude codex; do
  LIVE_ID="11111111-1960-4196-8196-111111111111"
  if [ "$LIVE_PROVIDER" = codex ]; then LIVE_ID="22222222-1960-4196-8196-222222222222"; fi
  LIVE_PATH=$(python3 - "$HOME" "$LIVE_REPO" "$LIVE_PROVIDER" "$LIVE_ID" <<'PY'
import json,pathlib,re,sys
home,cwd,provider,sid=sys.argv[1:]
if provider=='codex':
 p=pathlib.Path(home,'.codex','sessions','2026','09','18',f'rollout-live-{sid}.jsonl')
 rows=[{'type':'session_meta','payload':{'id':sid,'cwd':cwd}}, {'type':'response_item','payload':{'type':'message','role':'user','content':[{'type':'input_text','text':'synthetic live first turn'}]}}]
else:
 p=pathlib.Path(home,'.claude','projects',re.sub(r'[^A-Za-z0-9]','-',cwd),sid+'.jsonl')
 rows=[{'type':'user','cwd':cwd,'sessionId':sid,'gitBranch':'main','message':{'role':'user','content':'synthetic live first turn'}}]
p.parent.mkdir(parents=True,exist_ok=True)
p.write_text(''.join(json.dumps(r)+'\n' for r in rows))
print(p)
PY
)
  printf '{"cwd":"%s","session_id":"%s","transcript_path":"%s","prompt":"synthetic live turn"}\n' "$LIVE_REPO" "$LIVE_ID" "$LIVE_PATH" | cxt hook --provider "$LIVE_PROVIDER" --event UserPromptSubmit >/dev/null
  LIVE_FIRST=""
  for i in $(seq 1 50); do
    LIVE_FIRST=$(curl -sb "$J" "$B/repos/$RID/pending" | python3 -c 'import json,sys; print(next((p["target"] for p in json.load(sys.stdin) if p["session_id"]==sys.argv[1] and p.get("activity_at")),""))' "$LIVE_ID")
    [ -n "$LIVE_FIRST" ] && break
    sleep 0.3
  done
  expect "$LIVE_PROVIDER prompt captured before Stop" "$([ -n "$LIVE_FIRST" ] && echo yes)" yes
  # The visible pending pointer must already have its attachment. Do not poll
  # memory or issue a normal push to heal a prematurely acknowledged capture.
  if live_memory_proof "$LIVE_PROVIDER-first" "$LIVE_FIRST" "$LIVE_PROVIDER" "$LIVE_ID" >"$TMP/live-$LIVE_PROVIDER-first-proof.out" 2>&1; then
    expect "$LIVE_PROVIDER first pending target carries selected memory" yes yes
  else
    cat "$TMP/live-$LIVE_PROVIDER-first-proof.out"
    expect "$LIVE_PROVIDER first pending target carries selected memory" no yes
    CXT_E2E_KEEP_TMP=1
  fi
  expect "$LIVE_PROVIDER first capture leaves server main unchanged" "$(main_head)" "$LIVE_SHARED_BEFORE"
  expect "$LIVE_PROVIDER first capture leaves local main unchanged" "$(ref_target .cxt/refs/heads/main)" "$LIVE_LOCAL_BEFORE"
  python3 - "$LIVE_PATH" "$LIVE_PROVIDER" "$LIVE_ID" "$LIVE_REPO" <<'PY'
import json,sys
path,provider,sid,cwd=sys.argv[1:]
row=({'type':'response_item','payload':{'type':'message','role':'assistant','content':[{'type':'output_text','text':'synthetic live progress'}]}} if provider=='codex' else {'type':'assistant','cwd':cwd,'sessionId':sid,'gitBranch':'main','message':{'role':'assistant','content':[{'type':'text','text':'synthetic live progress'}]}})
with open(path,'a') as f:f.write(json.dumps(row)+'\n')
PY
  LIVE_NEXT="$LIVE_FIRST"
  for i in $(seq 1 70); do
    LIVE_NEXT=$(curl -sb "$J" "$B/repos/$RID/pending" | python3 -c 'import json,sys;print(next((p["target"] for p in json.load(sys.stdin) if p["session_id"]==sys.argv[1]),""))' "$LIVE_ID")
    [ -n "$LIVE_NEXT" ] && [ "$LIVE_NEXT" != "$LIVE_FIRST" ] && break
    sleep 0.3
  done
  expect "$LIVE_PROVIDER growing turn updates automatically" "$([ -n "$LIVE_NEXT" ] && [ "$LIVE_NEXT" != "$LIVE_FIRST" ] && echo yes)" yes
  if live_memory_proof "$LIVE_PROVIDER-grown" "$LIVE_NEXT" "$LIVE_PROVIDER" "$LIVE_ID" >"$TMP/live-$LIVE_PROVIDER-grown-proof.out" 2>&1; then
    expect "$LIVE_PROVIDER grown pending target carries selected memory" yes yes
  else
    cat "$TMP/live-$LIVE_PROVIDER-grown-proof.out"
    expect "$LIVE_PROVIDER grown pending target carries selected memory" no yes
    CXT_E2E_KEEP_TMP=1
  fi
  expect "$LIVE_PROVIDER growth leaves server main unchanged" "$(main_head)" "$LIVE_SHARED_BEFORE"
  expect "$LIVE_PROVIDER growth leaves local main unchanged" "$(ref_target .cxt/refs/heads/main)" "$LIVE_LOCAL_BEFORE"
  printf '{"cwd":"%s","session_id":"%s","transcript_path":"%s"}\n' "$LIVE_REPO" "$LIVE_ID" "$LIVE_PATH" | cxt hook --provider "$LIVE_PROVIDER" --event SessionEnd >/dev/null
  expect "$LIVE_PROVIDER observation preserves transcript" "$([ -f "$LIVE_PATH" ] && echo yes)" yes
done
expect "live capture never advances main" "$(main_head)" "$LIVE_SHARED_BEFORE"
