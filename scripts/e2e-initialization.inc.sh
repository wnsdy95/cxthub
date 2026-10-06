# Sourced only by the isolated sync E2E with its synthetic account and Git hooks.
# FS explicitly lacks atomic initialization; PG must exercise the automatic path.
if [ -n "${CXT_E2E_DSN:-}" ]; then
  echo "── S. New protected repository: pre-existing branch and genuine new birth"
  for INIT_KIND in legacy birth rewind; do
  INIT_BRANCH=main
  if [ "$INIT_KIND" = birth ]; then INIT_BRANCH=first-feature; fi
  INIT_SLUG=$(ccurl -fsSb "$J" -X POST "$B/repositories" -H 'Content-Type: application/json' \
    -d "{\"name\":\"FreshBranchE2E${INIT_KIND}\"}" | python3 -c 'import json,sys; print(json.load(sys.stdin)["slug"])') || exit 1
  INIT_REMOTE="http://127.0.0.1:$PORT/$OWN/$INIT_SLUG"
  INIT_RID=$(python3 - "$INIT_REMOTE" <<'PYINITID'
import hashlib,sys,urllib.parse
u=urllib.parse.urlsplit(sys.argv[1])
print('sha256:'+hashlib.sha256((u.netloc+u.path).lower().rstrip('/').encode()).hexdigest())
PYINITID
) || exit 1
  git init -q --bare "$TMP/initial-bare-$INIT_KIND.git" || exit 1
  mkdir -p "$TMP/initial-repo-$INIT_KIND"; cd "$TMP/initial-repo-$INIT_KIND" || exit 1
  git init -q && git remote add origin "$TMP/initial-bare-$INIT_KIND.git" && git commit -q --allow-empty -m initial-code || exit 1
  # This branch must predate installed hooks: it is a real legacy branch, not a birth.
  if [ "$INIT_KIND" = legacy ]; then git branch second-legacy || exit 1; fi
  cxt init >"$TMP/initial-init.out" 2>&1 || { cat "$TMP/initial-init.out"; exit 1; }
  cxt remote add origin "$INIT_REMOTE" >"$TMP/initial-connect.out" 2>&1 || { cat "$TMP/initial-connect.out"; exit 1; }
  ccurl -fsSb "$J" "$B/repos/$INIT_RID?initial_branch=$INIT_BRANCH" >"$TMP/initial-state.json" || exit 1
  python3 - "$TMP/initial-state.json" <<'PYINITSTATE'
import json,sys
r=json.load(open(sys.argv[1]))
assert r['context_protocol']==1 and r.get('initial_anchor_available') is True
PYINITSTATE
  [ "$?" = 0 ] || exit 1
  session "$TMP/initial-repo-$INIT_KIND" FIRSTMAIN
  printf '{"cwd":"%s","session_id":"sess-FIRSTMAIN","transcript_path":"%s","prompt":"begin"}\n' "$PWD" "$D/sess-FIRSTMAIN.jsonl" | cxt hook --provider claude --event UserPromptSubmit >"$TMP/initial-hook.out" 2>&1 || exit 1
  # The first real hook must normalize only the empty init cursor before
  # asynchronous pending capture or commit finalization can freeze its identity.
  python3 - "$INIT_RID" <<'PYINITCAPTURE'
import hashlib,json,pathlib,subprocess,sys
repo=sys.argv[1]
admin=subprocess.check_output(['git','rev-parse','--absolute-git-dir'],text=True).strip()
key=hashlib.sha256(admin.encode()).hexdigest()[:32]
p=json.loads((pathlib.Path('.cxt/worktrees')/key/'position.json').read_text())
assert p['repo_id']==repo and p['branch_id']=='legacy-'+hashlib.sha256((repo+'\x00main').encode()).hexdigest()[:32], 'first hook kept provisional repository identity'
assert p['branch']=='main' and not p.get('snapshot') and not p.get('selection'), 'first hook invented context selection'
PYINITCAPTURE
  [ "$?" = 0 ] || { cat "$TMP/initial-hook.out"; exit 1; }
  if [ "$INIT_KIND" = birth ]; then
    CXT_KEEP_SESSION=1 git switch -qc "$INIT_BRANCH" >"$TMP/initial-switch.out" 2>&1 || { cat "$TMP/initial-switch.out"; exit 1; }
  fi
  session "$TMP/initial-repo-$INIT_KIND" FIRSTFEATURE
  printf '{"cwd":"%s","session_id":"sess-FIRSTFEATURE","transcript_path":"%s","prompt":"work"}\n' "$PWD" "$D/sess-FIRSTFEATURE.jsonl" | cxt hook --provider claude --event UserPromptSubmit >>"$TMP/initial-hook.out" 2>&1 || exit 1
  echo feature > feature.txt
  git add feature.txt && git commit -qm first-feature >"$TMP/initial-commit.out" 2>&1 || { cat "$TMP/initial-commit.out"; exit 1; }
  # End the fixture-owned sessions through the official lifecycle before freezing targets.
  for INIT_SESSION in FIRSTMAIN FIRSTFEATURE; do
    printf '{"cwd":"%s","session_id":"sess-%s","transcript_path":"%s"}\n' "$PWD" "$INIT_SESSION" "$D/sess-$INIT_SESSION.jsonl" | cxt hook --provider claude --event SessionEnd >>"$TMP/initial-hook.out" 2>&1 || exit 1
  done
  if [ "$INIT_KIND" = rewind ]; then
    INIT_CODE_B=$(git rev-parse HEAD) || exit 1
    INIT_SNAP_B=$(ref_target .cxt/refs/heads/main) || exit 1
    session "$PWD" REWINDC
    printf '{"cwd":"%s","session_id":"sess-REWINDC","transcript_path":"%s","prompt":"continue to C"}\n' "$PWD" "$D/sess-REWINDC.jsonl" | cxt hook --provider claude --event UserPromptSubmit >>"$TMP/initial-hook.out" 2>&1 || exit 1
    echo C > feature.txt
    git add feature.txt && git commit -qm initial-C >"$TMP/initial-C.out" 2>&1 || { cat "$TMP/initial-C.out"; exit 1; }
    printf '{"cwd":"%s","session_id":"sess-REWINDC","transcript_path":"%s"}\n' "$PWD" "$D/sess-REWINDC.jsonl" | cxt hook --provider claude --event SessionEnd >>"$TMP/initial-hook.out" 2>&1 || exit 1
    INIT_SNAP_C=$(ref_target .cxt/refs/heads/main) || exit 1
    git reset --hard "$INIT_CODE_B" >"$TMP/initial-reset.out" 2>&1 || { cat "$TMP/initial-reset.out"; exit 1; }
    if ! python3 - "$INIT_CODE_B" "$INIT_SNAP_B" "$INIT_SNAP_C" <<'PYINITRESET'
import hashlib,json,pathlib,subprocess,sys
code,b,c=sys.argv[1:]
admin=subprocess.check_output(['git','rev-parse','--absolute-git-dir'],text=True).strip()
key=hashlib.sha256(admin.encode()).hexdigest()[:32]
position=json.loads((pathlib.Path('.cxt/worktrees')/key/'position.json').read_text())
ref=json.loads(pathlib.Path('.cxt/refs/heads/main').read_text())
assert subprocess.check_output(['git','rev-parse','HEAD'],text=True).strip()==code
assert b!=c and position.get('snapshot')==b and ref['target']==c, 'reset must select B while retaining C'
PYINITRESET
    then exit 1; fi
    session "$PWD" REWINDD
    printf '{"cwd":"%s","session_id":"sess-REWINDD","transcript_path":"%s","prompt":"continue from B"}\n' "$PWD" "$D/sess-REWINDD.jsonl" | cxt hook --provider claude --event UserPromptSubmit >>"$TMP/initial-hook.out" 2>&1 || exit 1
    echo D > feature.txt
    git add feature.txt && git commit -qm initial-D >"$TMP/initial-D.out" 2>&1 || { cat "$TMP/initial-D.out"; exit 1; }
    printf '{"cwd":"%s","session_id":"sess-REWINDD","transcript_path":"%s"}\n' "$PWD" "$D/sess-REWINDD.jsonl" | cxt hook --provider claude --event SessionEnd >>"$TMP/initial-hook.out" 2>&1 || exit 1
    INIT_SNAP_D=$(ref_target .cxt/refs/heads/main) || exit 1
    if ! python3 - "$TMP" "$INIT_SNAP_B" "$INIT_SNAP_C" "$INIT_SNAP_D" <<'PYINITCONTINUE'
import json,pathlib,sys
tmp=pathlib.Path(sys.argv[1]); b,c,d=sys.argv[2:]
snapshot=json.loads((pathlib.Path('.cxt/objects/snapshots')/d.removeprefix('sha256:')).read_text())
assert len({b,c,d})==3 and snapshot.get('parents')==[b] and c not in snapshot.get('graft_parents',[]), 'continuation did not preserve rewind semantics'
events=[json.loads(p.read_text()) for p in pathlib.Path('.cxt/history').glob('*.json')]
advance=[e for e in events if e['kind']=='advance' and e['branch']=='main' and e.get('source')==c and e.get('target')==d]
assert len(advance)==1, 'missing exact retained-C to D continuation'
(tmp/'initial-rewind-proof.json').write_text(json.dumps({'snapshots':[b,c,d],'advance':advance[0]})+'\n')
PYINITCONTINUE
    then exit 1; fi
  fi
  git push -qu origin "$INIT_BRANCH" >"$TMP/initial-push.out" 2>&1 || { cat "$TMP/initial-push.out"; exit 1; }
  ccurl -fsSb "$J" "$B/repos/$INIT_RID/refs" >"$TMP/initial-refs.json" || exit 1
  ccurl -fsSb "$J" "$B/repos/$INIT_RID/history" >"$TMP/initial-history.json" || exit 1
  ccurl -fsSb "$J" "$B/repos/$INIT_RID?initial_branch=$INIT_BRANCH" >"$TMP/initial-state.json" || exit 1
  python3 - "$TMP" "$INIT_KIND" "$INIT_BRANCH" <<'PYINITVERIFY'
import hashlib,json,pathlib,sys
p=pathlib.Path(sys.argv[1]); kind,branch=sys.argv[2:]; ref=json.loads((pathlib.Path('.cxt/refs/heads')/branch).read_text())
refs=json.loads((p/'initial-refs.json').read_text()); branches=[r for r in refs if r['kind']=='branch']
assert len(branches)==1 and branches[0]['name']==branch and branches[0]['branch_id']==ref['branch_id'] and branches[0]['target']==ref['target'], 'first selection not published exactly; inspect initial-push.out'
events=json.loads((p/'initial-history.json').read_text()); local=[json.loads(f.read_text()) for f in pathlib.Path('.cxt/history').glob('*.json')]
births=[e for e in events if e['kind']=='birth']
if kind=='birth':
    assert len(births)==1 and births[0]['branch']==branch and births[0] in local, 'birth is not the actual observed event'
else:
    assert not births and ref['branch_id']=='legacy-'+hashlib.sha256((ref['repo_id']+'\x00'+branch).encode()).hexdigest()[:32], 'pre-existing branch got a fabricated birth or wrong identity'
assert not json.loads((p/'initial-state.json').read_text()).get('initial_anchor_available',False), 'published branch still reports initial eligibility'
print('verified protected creation and exact first publication:',kind)
PYINITVERIFY
  INIT_STATUS=$?
  if [ "$INIT_STATUS" != 0 ]; then cat "$TMP/initial-push.out"; exit 1; fi
  expect "first $INIT_KIND publication preserves its actual branch evidence" "$INIT_STATUS" 0
  if [ "$INIT_KIND" = rewind ]; then
    ccurl -fsSb "$J" "$B/repos/$INIT_RID/snapshots" >"$TMP/initial-snapshots.json" || exit 1
    if ! python3 - "$TMP" <<'PYINITRETAINED'
import json,pathlib,sys
p=pathlib.Path(sys.argv[1]); proof=json.loads((p/'initial-rewind-proof.json').read_text())
events=json.loads((p/'initial-history.json').read_text())
snapshots=json.loads((p/'initial-snapshots.json').read_text())
assert proof['advance'] in events, 'first publication did not accept exact C to D continuation'
assert set(proof['snapshots']) <= {s['id'] for s in snapshots}, 'first publication discarded retained context'
PYINITRETAINED
    then exit 1; fi
    expect "first legacy publication accepts rewind/continuation and retains C" yes yes
    continue
  fi

  # Check the first selection above before creating any follow-up publication.
  INIT_FOLLOW_BRANCH=main
  if [ "$INIT_KIND" = legacy ]; then INIT_FOLLOW_BRANCH=second-legacy; fi
  ccurl -fsSb "$J" "$B/repos/$INIT_RID?initial_branch=$INIT_FOLLOW_BRANCH" >"$TMP/initial-follow-state.json" || exit 1
  if ! python3 - "$TMP/initial-follow-state.json" <<'PYINITELIGIBLE'
import json,sys
state=json.load(open(sys.argv[1]))
assert state['context_protocol']==1 and state.get('initial_anchor_available') is True, 'unpublished legacy branch lost eligibility'
PYINITELIGIBLE
  then exit 1; fi
  if [ "$INIT_KIND" = legacy ]; then
    # Existing branch switch only; no -c/-b and no hand-written context metadata.
    CXT_KEEP_SESSION=1 git switch -q "$INIT_FOLLOW_BRANCH" >"$TMP/initial-follow-switch.out" 2>&1 || { cat "$TMP/initial-follow-switch.out"; exit 1; }
    # The pre-init branch still points before any recorded context. Fast-forward
    # its real code to an observed commit before asking hooks to capture on it.
    git merge --ff-only main >"$TMP/initial-follow-align.out" 2>&1 || { cat "$TMP/initial-follow-align.out"; exit 1; }
    if ! python3 - "$INIT_FOLLOW_BRANCH" <<'PYINITFOLLOWPOSITION'
import hashlib,json,pathlib,subprocess,sys
branch=sys.argv[1]
admin=subprocess.check_output(['git','rev-parse','--absolute-git-dir'],text=True).strip()
key=hashlib.sha256(admin.encode()).hexdigest()[:32]
p=json.loads((pathlib.Path('.cxt/worktrees')/key/'position.json').read_text())
main=json.loads(pathlib.Path('.cxt/refs/heads/main').read_text())
code=subprocess.check_output(['git','rev-parse','main'],text=True).strip()
identity='legacy-'+hashlib.sha256((main['repo_id']+'\x00'+branch).encode()).hexdigest()[:32]
assert subprocess.check_output(['git','rev-parse','HEAD'],text=True).strip()==code, 'follow-up Git code is not aligned'
assert p['repo_id']==main['repo_id'] and p['branch']==branch and p['branch_id']==identity and p['git_commit']==code and p['snapshot']==main['target'], 'follow-up working context was not selected by real hooks'
PYINITFOLLOWPOSITION
    then exit 1; fi
    session "$PWD" SECONDLEGACY
    printf '{"cwd":"%s","session_id":"sess-SECONDLEGACY","transcript_path":"%s","prompt":"work on existing branch"}\n' "$PWD" "$D/sess-SECONDLEGACY.jsonl" | cxt hook --provider claude --event UserPromptSubmit >>"$TMP/initial-hook.out" 2>&1 || exit 1
    echo second > second.txt
    git add second.txt && git commit -qm second-legacy >"$TMP/initial-follow-commit.out" 2>&1 || { cat "$TMP/initial-follow-commit.out"; exit 1; }
    printf '{"cwd":"%s","session_id":"sess-SECONDLEGACY","transcript_path":"%s"}\n' "$PWD" "$D/sess-SECONDLEGACY.jsonl" | cxt hook --provider claude --event SessionEnd >>"$TMP/initial-hook.out" 2>&1 || exit 1
  fi
  # In the birth case HEAD stays on first-feature: explicitly publish existing main.
  if ! python3 - "$INIT_FOLLOW_BRANCH" "$TMP/initial-follow-ref.json" <<'PYINITLEGACY'
import hashlib,json,pathlib,sys
branch=sys.argv[1]
ref=json.loads((pathlib.Path('.cxt/refs/heads')/branch).read_text())
assert ref['branch_id']=='legacy-'+hashlib.sha256((ref['repo_id']+'\x00'+branch).encode()).hexdigest()[:32], 'follow-up branch is not the actual legacy identity'
events=[json.loads(p.read_text()) for p in pathlib.Path('.cxt/history').glob('*.json')]
assert not any(e['kind'] in ('birth','orphan') and (e['branch']==branch or e['branch_id']==ref['branch_id']) for e in events), 'follow-up fabricated a birth'
pathlib.Path(sys.argv[2]).write_text(json.dumps(ref)+'\n')
PYINITLEGACY
  then exit 1; fi
  cxt push origin "$INIT_FOLLOW_BRANCH" >"$TMP/initial-follow-push.out" 2>&1 || { cat "$TMP/initial-follow-push.out"; exit 1; }
  ccurl -fsSb "$J" "$B/repos/$INIT_RID/refs" >"$TMP/initial-follow-refs.json" || exit 1
  ccurl -fsSb "$J" "$B/repos/$INIT_RID/history" >"$TMP/initial-follow-history.json" || exit 1
  ccurl -fsSb "$J" "$B/repos/$INIT_RID?initial_branch=$INIT_FOLLOW_BRANCH" >"$TMP/initial-follow-state.json" || exit 1
  if ! python3 - "$TMP" "$INIT_BRANCH" "$INIT_FOLLOW_BRANCH" <<'PYINITFOLLOW'
import json,pathlib,sys
p=pathlib.Path(sys.argv[1]); first,follow=sys.argv[2:]
before=[r for r in json.loads((p/'initial-refs.json').read_text()) if r['kind']=='branch']
after=[r for r in json.loads((p/'initial-follow-refs.json').read_text()) if r['kind']=='branch']
expected=json.loads((p/'initial-follow-ref.json').read_text())
assert len(before)==1 and before[0]['name']==first, 'first-selection proof was not retained'
assert len(after)==2 and {r['name'] for r in after}=={first,follow}, 'follow-up widened publication'
assert before[0] in after, 'follow-up changed the first published branch'
found=next(r for r in after if r['name']==follow)
assert all(found[k]==expected[k] for k in ('repo_id','name','branch_id','target')), 'follow-up selected wrong legacy context'
events=json.loads((p/'initial-follow-history.json').read_text())
assert not any(e['kind'] in ('birth','orphan') and (e['branch']==follow or e['branch_id']==expected['branch_id']) for e in events), 'server received a fabricated follow-up birth'
state=json.loads((p/'initial-follow-state.json').read_text())
assert state['context_protocol']==1 and not state.get('initial_anchor_available',False), 'accepted branch still eligible'
print('verified independent legacy follow-up:',first,'->',follow)
PYINITFOLLOW
  then cat "$TMP/initial-follow-push.out"; exit 1; fi
  expect "$INIT_KIND first selection permits explicit $INIT_FOLLOW_BRANCH without changing its peer" yes yes
  done
fi
