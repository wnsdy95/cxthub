# Sourced by e2e-sync.sh with its isolated binaries, server, HOME and fixtures.
echo "── L. Two real worktrees: rewind selects past context and preserves future on server"
git clone -q "$TMP/bare.git" "$TMP/history-client"
cd "$TMP/history-client"
cxt init >/dev/null 2>&1
cxt remote add origin "$REMOTE" >/dev/null 2>&1
cxt pull >/dev/null 2>&1
if ! CXT_KEEP_SESSION=1 git checkout -qb history-pair >"$TMP/paired-birth.out" 2>&1; then
  cat "$TMP/paired-birth.out"; FAIL=1; return
fi
session "$TMP/history-client" PA
echo a > paired.txt; git add paired.txt; git commit -qm paired-A >"$TMP/paired-A.out" 2>&1
CODE_A=$(git rev-parse HEAD)
SNAP_A=$(cat .cxt/refs/heads/history-pair)
session "$TMP/history-client" PB
echo b > paired.txt; git add paired.txt; git commit -qm paired-B >"$TMP/paired-B.out" 2>&1
CODE_B=$(git rev-parse HEAD)
SNAP_B=$(cat .cxt/refs/heads/history-pair)
if [ -z "$SNAP_A" ] || [ -z "$SNAP_B" ]; then cat "$TMP/paired-A.out" "$TMP/paired-B.out"; FAIL=1; return; fi
cxt push --append >"$TMP/paired-push-B.out" 2>&1
expect "paired contexts are distinct" "$([ "$SNAP_A" != "$SNAP_B" ] && echo yes)" yes
CXT_KEEP_SESSION=1 git worktree add -q --detach "$TMP/history-peer" "$CODE_B" >"$TMP/paired-peer.out" 2>&1
position_snapshot() { python3 - "$TMP/history-client" "$1" <<'PYPOS'
import hashlib,json,pathlib,subprocess,sys
admin=subprocess.check_output(['git','-C',sys.argv[2],'rev-parse','--absolute-git-dir'],text=True).strip()
key=hashlib.sha256(admin.encode()).hexdigest()[:32]
p=pathlib.Path(sys.argv[1])/'.cxt/worktrees'/key/'position.json'
print(json.loads(p.read_text()).get('snapshot','') if p.exists() else '')
PYPOS
}
expect "detached peer selects code B context" "$(position_snapshot "$TMP/history-peer")" "$SNAP_B"
git reset --hard "$CODE_A" >"$TMP/paired-reset.out" 2>&1
expect "reset selects context at code A" "$(position_snapshot "$TMP/history-client")" "$SNAP_A"
expect "reset keeps shared context tip B" "$(cat .cxt/refs/heads/history-pair)" "$SNAP_B"
expect "reset leaves peer position B intact" "$(position_snapshot "$TMP/history-peer")" "$SNAP_B"
session "$TMP/history-client" PC
echo c > paired.txt; git add paired.txt; git commit -qm paired-C >"$TMP/paired-C.out" 2>&1
SNAP_C=$(cat .cxt/refs/heads/history-pair)
expect "new continuation excludes later ancestry" "$(python3 - "$SNAP_A" "$SNAP_B" "$SNAP_C" <<'PYPARENTS'
import json,pathlib,sys
a,b,c=sys.argv[1:];s=json.loads((pathlib.Path('.cxt/objects/snapshots')/c.removeprefix('sha256:')).read_text())
print('yes' if s.get('parents')==[a] and b not in s.get('graft_parents',[]) else 'no')
PYPARENTS
)" yes
cxt push --append >"$TMP/paired-push-C.out" 2>&1
expect "server records retained B and current C together" "$(curl -sb "$J" "$B/repos/$RID/history" | python3 -c '
import json,sys
rows=json.load(sys.stdin)
print("yes" if any(e["kind"]=="advance" and e["branch"]=="history-pair" and e.get("source")==sys.argv[1] and e.get("target")==sys.argv[2] for e in rows) else "no")' "$SNAP_B" "$SNAP_C")" yes
expect "server branch advances to C" "$(curl -sb "$J" "$B/repos/$RID/refs" | python3 -c 'import json,sys;print(next((r["target"] for r in json.load(sys.stdin) if r["kind"]=="branch" and r["name"]=="history-pair"),""))')" "$SNAP_C"
expect "peer remains at B after server publication" "$(position_snapshot "$TMP/history-peer")" "$SNAP_B"

echo "── M. Tracking alias: one server branch, selected code, local rename and removal"
git update-ref refs/remotes/origin/history-pair "$CODE_A"
if ! CXT_KEEP_SESSION=1 git switch -qc my-history --track origin/history-pair >"$TMP/alias-create.out" 2>&1; then
  cat "$TMP/alias-create.out"; FAIL=1; return
fi
cxt git-hook branch-replay >"$TMP/alias-replay.out" 2>&1
expect "tracking alias selects code A rather than the newer remote tip" "$(position_snapshot "$TMP/history-client")" "$SNAP_A"
expect "tracking alias retains shared tip C" "$(cat .cxt/refs/heads/history-pair)" "$SNAP_C"
expect "tracking alias creates no duplicate context ref" "$([ ! -f .cxt/refs/heads/my-history ] && echo yes)" yes
session "$TMP/history-client" PD
echo d > paired.txt; git add paired.txt; git commit -qm alias-D >"$TMP/alias-D.out" 2>&1
SNAP_D=$(cat .cxt/refs/heads/history-pair)
expect "alias capture uses the canonical branch and historical parent A" "$(python3 - "$SNAP_A" "$SNAP_C" "$SNAP_D" <<'PYALIAS'
import json,pathlib,sys
a,c,d=sys.argv[1:];s=json.loads((pathlib.Path('.cxt/objects/snapshots')/d.removeprefix('sha256:')).read_text())
print('yes' if d!=c and s.get('branch')=='history-pair' and s.get('parents')==[a] and c not in s.get('graft_parents',[]) else 'no')
PYALIAS
)" yes
cxt push --append >"$TMP/alias-push.out" 2>&1
expect "alias push updates only the canonical server branch" "$(curl -sb "$J" "$B/repos/$RID/refs" | python3 -c 'import json,sys;r=json.load(sys.stdin);print("yes" if any(x["kind"]=="branch" and x["name"]=="history-pair" and x["target"]==sys.argv[1] for x in r) and not any(x["kind"]=="branch" and x["name"]=="my-history" for x in r) else "no")' "$SNAP_D")" yes
git branch -m renamed-history >"$TMP/alias-rename.out" 2>&1
cxt git-hook ref-sync >/dev/null 2>&1
expect "local alias rename keeps canonical context" "$(cat .cxt/refs/heads/history-pair)" "$SNAP_D"
CXT_KEEP_SESSION=1 git switch -q history-pair >"$TMP/alias-leave.out" 2>&1
git branch -D renamed-history >"$TMP/alias-delete.out" 2>&1
cxt git-hook ref-sync >/dev/null 2>&1
expect "local alias removal keeps canonical context" "$(cat .cxt/refs/heads/history-pair)" "$SNAP_D"
