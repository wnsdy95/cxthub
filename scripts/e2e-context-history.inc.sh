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
# L/M assert each completed operation; asynchronous sequence correctness is
# covered separately. Join capture/publication before recording its context.
if ! python3 "$ROOT/scripts/e2e-drain-publication.py" "$TMP/bin/cxt" "$PWD" >"$TMP/paired-A-drain.out" 2>&1; then
  cat "$TMP/paired-A-drain.out"; FAIL=1; CXT_E2E_KEEP_TMP=1; return
fi
CODE_A=$(git rev-parse HEAD)
SNAP_A=$(ref_target .cxt/refs/heads/history-pair)
session "$TMP/history-client" PB
echo b > paired.txt; git add paired.txt; git commit -qm paired-B >"$TMP/paired-B.out" 2>&1
if ! python3 "$ROOT/scripts/e2e-drain-publication.py" "$TMP/bin/cxt" "$PWD" >"$TMP/paired-B-drain.out" 2>&1; then
  cat "$TMP/paired-B-drain.out"; FAIL=1; CXT_E2E_KEEP_TMP=1; return
fi
CODE_B=$(git rev-parse HEAD)
SNAP_B=$(ref_target .cxt/refs/heads/history-pair)
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
expect "reset keeps shared context tip B" "$(ref_target .cxt/refs/heads/history-pair)" "$SNAP_B"
expect "reset leaves peer position B intact" "$(position_snapshot "$TMP/history-peer")" "$SNAP_B"
session "$TMP/history-client" PC
echo c > paired.txt; git add paired.txt; git commit -qm paired-C >"$TMP/paired-C.out" 2>&1
if ! python3 "$ROOT/scripts/e2e-drain-publication.py" "$TMP/bin/cxt" "$PWD" >"$TMP/paired-C-drain.out" 2>&1; then
  cat "$TMP/paired-C-drain.out"; FAIL=1; CXT_E2E_KEEP_TMP=1; return
fi
SNAP_C=$(ref_target .cxt/refs/heads/history-pair)
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
if ! cxt branch replay >"$TMP/alias-replay.out" 2>&1; then
  cat "$TMP/alias-replay.out"; FAIL=1; CXT_E2E_KEEP_TMP=1; return
fi
expect "tracking alias selects code A rather than the newer remote tip" "$(position_snapshot "$TMP/history-client")" "$SNAP_A"
expect "tracking alias retains shared tip C" "$(ref_target .cxt/refs/heads/history-pair)" "$SNAP_C"
expect "tracking alias creates no duplicate context ref" "$([ ! -f .cxt/refs/heads/my-history ] && echo yes)" yes
session "$TMP/history-client" PD
echo d > paired.txt; git add paired.txt; git commit -qm alias-D >"$TMP/alias-D.out" 2>&1
if ! python3 "$ROOT/scripts/e2e-drain-publication.py" "$TMP/bin/cxt" "$PWD" >"$TMP/alias-D-drain.out" 2>&1; then
  cat "$TMP/alias-D-drain.out"; FAIL=1; CXT_E2E_KEEP_TMP=1; return
fi
SNAP_D=$(ref_target .cxt/refs/heads/history-pair)
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
expect "local alias rename keeps canonical context" "$(ref_target .cxt/refs/heads/history-pair)" "$SNAP_D"
CXT_KEEP_SESSION=1 git switch -q history-pair >"$TMP/alias-leave.out" 2>&1
git branch -D renamed-history >"$TMP/alias-delete.out" 2>&1
cxt git-hook ref-sync >/dev/null 2>&1
expect "local alias removal keeps canonical context" "$(ref_target .cxt/refs/heads/history-pair)" "$SNAP_D"

echo "── N. Repository identity protection with real CLI branch operations"
mkdir -p "$TMP/protocol-client"
cd "$TMP/protocol-client"
git init -q
git remote add origin "$TMP/bare.git"
cxt init >/dev/null 2>&1
PROTOCOL_SLUG=$(ccurl -sb "$J" -X POST "$B/repositories" -H 'Content-Type: application/json' -d '{"name":"ProtectedContext"}' | jget "['slug']")
PROTOCOL_REMOTE="$ORIGIN/$OWN/$PROTOCOL_SLUG"
# This section specifically verifies migration of an existing legacy repository.
fixture_register_legacy_repo "$PROTOCOL_REMOTE" "$TMP/bare.git" || { FAIL=1; return; }
if ! cxt remote add origin "$PROTOCOL_REMOTE" >"$TMP/protocol-connect.out" 2>&1; then cat "$TMP/protocol-connect.out"; FAIL=1; return; fi
session "$TMP/protocol-client" PROTOCOL
echo protected > protocol.txt
git add protocol.txt
git commit -qm protocol-base >"$TMP/protocol-base.out" 2>&1
if ! cxt push >"$TMP/protocol-initial.out" 2>&1; then cat "$TMP/protocol-initial.out"; FAIL=1; return; fi
PROTOCOL_RID=$(curl -sb "$J" "$B/repos" | python3 -c 'import json,sys;print(next(r["id"] for r in json.load(sys.stdin) if r["remote_url"]==sys.argv[1]))' "$PROTOCOL_REMOTE")
PROTOCOL_MAIN=$(ref_target .cxt/refs/heads/main)
expect "explicit protocol upgrade" "$(ccurl -sb "$J" -X POST "$B/repos/$PROTOCOL_RID/context-protocol" -H 'Content-Type: application/json' -d '{}' -o "$TMP/protocol-response.json" -w '%{http_code}')" 200
expect "legacy name-only write is blocked even at the same hash" "$(ccurl -sb "$J" -X PUT "$B/repos/$PROTOCOL_RID/refs/branch/main" -H 'Content-Type: application/json' -d "{\"target\":\"$PROTOCOL_MAIN\"}" -o /dev/null -w '%{http_code}')" 409
if ! cxt push >"$TMP/protocol-current.out" 2>&1; then cat "$TMP/protocol-current.out"; FAIL=1; return; fi
expect "new CLI can still push after upgrade" yes yes
git branch protected-work >"$TMP/protocol-birth.out" 2>&1
cxt branch replay >>"$TMP/protocol-birth.out" 2>&1
if ! cxt push >>"$TMP/protocol-birth.out" 2>&1; then cat "$TMP/protocol-birth.out"; FAIL=1; return; fi
PROTOCOL_ID=$(birth_field protected-work branch_id)
expect "creation without checkout records an independent identity" "$(curl -sb "$J" "$B/repos/$PROTOCOL_RID/refs" | python3 -c 'import json,sys;r=json.load(sys.stdin);b=next(x for x in r if x["name"]=="protected-work" and x["kind"]=="branch");print("yes" if b.get("branch_id")==sys.argv[1] and b["target"]==sys.argv[2] else "no")' "$PROTOCOL_ID" "$PROTOCOL_MAIN")" yes
git branch -m protected-work protected-renamed >"$TMP/protocol-rename.out" 2>&1
cxt git-hook ref-sync >>"$TMP/protocol-rename.out" 2>&1
if ! cxt push >>"$TMP/protocol-rename.out" 2>&1; then cat "$TMP/protocol-rename.out"; FAIL=1; return; fi
expect "rename projects the same server identity" "$(curl -sb "$J" "$B/repos/$PROTOCOL_RID/refs" | python3 -c 'import json,sys;r=json.load(sys.stdin);print("yes" if any(x["kind"]=="branch" and x["name"]=="protected-renamed" and x.get("branch_id")==sys.argv[1] for x in r) and not any(x["kind"]=="branch" and x["name"]=="protected-work" for x in r) else "no")' "$PROTOCOL_ID")" yes
git branch protected-work >"$TMP/protocol-reuse.out" 2>&1
cxt branch replay >>"$TMP/protocol-reuse.out" 2>&1
if ! cxt push >>"$TMP/protocol-reuse.out" 2>&1; then cat "$TMP/protocol-reuse.out"; FAIL=1; return; fi
expect "reused name has a distinct identity at the same snapshot" "$([ "$(birth_field protected-work branch_id)" != "$PROTOCOL_ID" ] && echo yes)" yes
expect "stale identity cannot write the reused branch" "$(ccurl -sb "$J" -X PUT "$B/repos/$PROTOCOL_RID/refs/branch/protected-work" -H 'Content-Type: application/json' -d "{\"branch_id\":\"$PROTOCOL_ID\",\"target\":\"$PROTOCOL_MAIN\"}" -o /dev/null -w '%{http_code}')" 409

echo "── O. Damaged replica repair preserves evidence and unpushed work"
# Clone durable Git state and pull a verified context replica. Copying a live
# .cxt directory can copy a transient mkdir lock owned by a background ref-sync
# process; the copied owner can never remove that lock in the new directory.
git clone -q "$TMP/protocol-client" "$TMP/repair-client"
cd "$TMP/repair-client"
git remote set-url origin "$TMP/bare.git"
if ! cxt init >"$TMP/repair-init.out" 2>&1 || ! cxt remote add origin "$PROTOCOL_REMOTE" >>"$TMP/repair-init.out" 2>&1 || ! cxt pull >>"$TMP/repair-init.out" 2>&1; then
  cat "$TMP/repair-init.out"; FAIL=1; return
fi
# Record a real local branch operation so the repair fixture has the same
# independent Git-journal identity witness as the original protocol client.
if ! git branch repair-evidence >"$TMP/repair-witness.out" 2>&1 || ! cxt branch replay >>"$TMP/repair-witness.out" 2>&1; then
  cat "$TMP/repair-witness.out"; FAIL=1; return
fi
# No publisher may observe the local-only work created below. An empty queue
# alone is insufficient while an upstream branch replay can still enqueue it.
if ! python3 "$ROOT/scripts/e2e-drain-publication.py" "$TMP/bin/cxt" "$TMP/protocol-client" "$TMP/repair-client" >"$TMP/repair-drain.out" 2>&1; then
  cat "$TMP/repair-drain.out"
  echo "  ✗ publication is not quiescent; repair corruption not attempted"
  FAIL=1; CXT_E2E_KEEP_TMP=1; return
fi
# Linux CI emits Go goroutines if this small fixture ever stalls again. This
# deadline is test diagnostics only; production repair/capture timeouts stay unchanged.
repair_command() {
  if command -v timeout >/dev/null 2>&1; then
    GOTRACEBACK=all timeout --signal=QUIT --kill-after=5s 60s "$@"
  else
    "$@"
  fi
}
echo "  repair fixture: capture local-only work"
session "$TMP/repair-client" LOCAL_ONLY
if ! repair_command cxt save --provider claude -m local-only >"$TMP/repair-save.out" 2>&1; then cat "$TMP/repair-save.out"; FAIL=1; return; fi
if ! REPAIR_LOCAL=$(ref_target .cxt/refs/heads/main); then
  echo "repair proof failed: cannot read local snapshot" >"$TMP/repair-proof.out"
  cat "$TMP/repair-proof.out"; FAIL=1; return
fi
if ! curl --fail --silent --show-error -b "$J" "$B/repos/$PROTOCOL_RID/manifest" >"$TMP/repair-server-manifest.json"; then
  FAIL=1; return
fi
if ! python3 "$ROOT/scripts/e2e-repair-proof.py" local-only "$TMP/repair-server-manifest.json" "$PROTOCOL_RID" "$PROTOCOL_MAIN" "$REPAIR_LOCAL" >"$TMP/repair-proof.out" 2>&1; then
  cat "$TMP/repair-proof.out"; FAIL=1; return
fi
printf 'corrupted document fixture\n' > ".cxt/objects/docs/${PROTOCOL_MAIN#sha256:}"
printf 'broken config fixture\n' > .cxt/config
echo "  repair fixture: inspect damaged replica"
# A damaged replica is expected to fail doctor; retain its JSON and stderr
# separately so a process failure cannot be mistaken for an empty issue list.
repair_command cxt doctor --json >"$TMP/doctor-before.json" 2>"$TMP/doctor-before.err"
printf '%s\n' "$?" >"$TMP/doctor-before.status"
echo "  repair fixture: restore verified server objects"
if ! repair_command cxt repair --from-server --remote "$PROTOCOL_REMOTE" >"$TMP/repair.out" 2>&1; then cat "$TMP/repair.out"; FAIL=1; return; fi
expect "repair keeps unpushed local main" "$(ref_target .cxt/refs/heads/main)" "$REPAIR_LOCAL"
repair_command cxt doctor --json >"$TMP/doctor-after.json" 2>"$TMP/doctor-after.err"
DOCTOR_AFTER_STATUS=$?
printf '%s\n' "$DOCTOR_AFTER_STATUS" >"$TMP/doctor-after.status"
if [ "$DOCTOR_AFTER_STATUS" -ne 0 ]; then
  cat "$TMP/doctor-after.err" "$TMP/doctor-after.json"; FAIL=1; return
fi
if ! REPAIR_ISSUES=$(python3 "$ROOT/scripts/e2e-repair-proof.py" doctor "$TMP/doctor-after.json"); then
  FAIL=1; return
fi
expect "repair verified all referenced replica objects" "$REPAIR_ISSUES" 0
if [ "$REPAIR_ISSUES" != 0 ]; then
  # Synthetic fixture only: retain the exact category instead of hiding it
  # behind a count. This must not turn a failed integrity check into success.
  python3 -c 'import json,sys;print("repair diagnostic issues:", json.dumps(json.load(sys.stdin)["issues"]))' <"$TMP/doctor-after.json"
fi
expect "damaged bytes were quarantined exactly" "$(python3 - "$PROTOCOL_MAIN" <<'PYQUARANTINE'
import pathlib,sys
root=pathlib.Path('.git/cxt/repairs')
files=list(root.glob('*/.cxt/objects/docs/'+sys.argv[1].removeprefix('sha256:')+'.*'))
print('yes' if any(p.read_bytes()==b'corrupted document fixture\n' for p in files) else 'no')
PYQUARANTINE
)" yes
if ! repair_command cxt repair --from-server >"$TMP/repair-retry.out" 2>&1; then cat "$TMP/repair-retry.out"; FAIL=1; return; fi
expect "repair retry keeps the local-only snapshot" "$(ref_target .cxt/refs/heads/main)" "$REPAIR_LOCAL"
expect "repair rejects a conflicting repository URL" "$(cxt repair --from-server --remote "$REMOTE/another" >"$TMP/repair-wrong.out" 2>&1; echo $?)" 1
