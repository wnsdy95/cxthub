# Sourced by e2e-sync.sh. All state belongs to its isolated HOME/server.
echo "── P. Real commit → squash → promotion → pull → next native capture"
git init -q --bare "$TMP/publication-code.git"
# Keep a stable forge identity while all Git transport stays inside this fixture.
git config --global url."$TMP/publication-code.git".insteadOf https://git.example.test/acme/publication.git
mkdir -p "$TMP/publication-client"; cd "$TMP/publication-client"
git init -q; git remote add origin https://git.example.test/acme/publication.git
cxt init >/dev/null 2>&1
PUB_SLUG=$(ccurl -sb "$J" -X POST "$B/workspaces" -H 'Content-Type: application/json' -d '{"name":"PublicationE2E"}' | jget "['slug']")
PUB_REMOTE="$ORIGIN/$OWN/$PUB_SLUG"
cxt remote add origin "$PUB_REMOTE" >/dev/null 2>&1
session "$PWD" PUBBASE
git add .gitignore; git commit -qm publication-base >"$TMP/pub-base.out" 2>&1
if ! cxt push >"$TMP/pub-initial.out" 2>&1; then cat "$TMP/pub-initial.out"; FAIL=1; return; fi
PUB_RID=$(curl -sb "$J" "$B/repos" | python3 -c 'import json,sys;print(next(r["id"] for r in json.load(sys.stdin) if r["remote_url"]==sys.argv[1]))' "$PUB_REMOTE")
if ! git push -qu origin main >"$TMP/pub-base-code.out" 2>&1; then cat "$TMP/pub-base-code.out"; FAIL=1; return; fi
if ! CXT_KEEP_SESSION=1 git switch -qc finalized-feature >"$TMP/pub-birth.out" 2>&1; then cat "$TMP/pub-birth.out"; FAIL=1; return; fi
# Keep a real main worktree with an already selected/captured app session while
# another worktree publishes the PR. No cursor or snapshot graph is patched.
PUB_MAIN="$TMP/publication-main"
if ! CXT_KEEP_SESSION=1 git worktree add -q "$PUB_MAIN" main >"$TMP/pub-main-worktree.out" 2>&1; then cat "$TMP/pub-main-worktree.out"; FAIL=1; return; fi
cd "$PUB_MAIN"
session "$PWD" PUBCONTINUE
PUB_NATIVE="$D/s-PUBCONTINUE.jsonl"
if ! cxt commit -m main-before-promotion >"$TMP/pub-main-before.out" 2>&1; then cat "$TMP/pub-main-before.out"; FAIL=1; return; fi
if ! cxt push >"$TMP/pub-main-before-push.out" 2>&1; then cat "$TMP/pub-main-before-push.out"; FAIL=1; return; fi
PUB_STORE="$TMP/publication-client/.cxt"
PUB_WORKTREE=$(git rev-parse --absolute-git-dir | python3 -c 'import hashlib,sys;print(hashlib.sha256(sys.stdin.read().strip().encode()).hexdigest()[:32])')
PUB_POSITION="$PUB_STORE/worktrees/$PUB_WORKTREE/position.json"
PUB_BEFORE=$(python3 -c 'import json,sys;print(json.load(open(sys.argv[1]))["snapshot"])' "$PUB_POSITION")
PUB_CODE_BEFORE=$(git rev-parse HEAD)
expect "main has a captured position before the Git move" "$(python3 - "$PUB_POSITION" "$PUB_BEFORE" "$PUB_CODE_BEFORE" <<'PYBEFORE'
import json,sys
p=json.load(open(sys.argv[1]))
print('ready' if p.get('snapshot')==sys.argv[2] and p.get('shared_target')==sys.argv[2]
      and p.get('git_commit')==sys.argv[3] and p.get('memory_pinned') else json.dumps(p,sort_keys=True))
PYBEFORE
)" ready
cd "$TMP/publication-client"
session "$PWD" PUBFIRST
git commit --allow-empty -qm first-source >"$TMP/pub-first.out" 2>&1
PUB_FIRST=$(ref_target .cxt/refs/heads/finalized-feature)
session "$PWD" PUBLAST
git commit --allow-empty -qm final-source >"$TMP/pub-last.out" 2>&1
PUB_SECOND=$(ref_target .cxt/refs/heads/finalized-feature)
# A second completed capture at the same Git SHA must not let the worker bind
# the older eligible source halfway through the eventual publication upload.
session "$PWD" PUBFOLLOWUP
if ! cxt commit -m final-source-followup >"$TMP/pub-followup.out" 2>&1; then cat "$TMP/pub-followup.out"; FAIL=1; return; fi
PUB_LAST=$(ref_target .cxt/refs/heads/finalized-feature)
cat > "$TMP/squash-editor" <<'PYEDITOR'
#!/usr/bin/env python3
import pathlib,sys
p=pathlib.Path(sys.argv[1])
p.write_text(p.read_text().replace('\npick ', '\nsquash ',1))
PYEDITOR
chmod +x "$TMP/squash-editor"
if ! CXT_KEEP_SESSION=1 GIT_EDITOR=true GIT_SEQUENCE_EDITOR="$TMP/squash-editor" git rebase -i HEAD~2 >"$TMP/pub-rebase.out" 2>&1; then cat "$TMP/pub-rebase.out"; FAIL=1; return; fi
PUB_HEAD=$(git rev-parse HEAD)
# Model the hosting service with a plain Git clone: merge the actual squashed
# source into main without running client capture hooks on the service side.
git clone -q https://git.example.test/acme/publication.git "$TMP/publication-host"
if ! git -C "$TMP/publication-host" fetch -q "$TMP/publication-client" finalized-feature ||
   ! git -C "$TMP/publication-host" merge --no-ff -qm publication-merge FETCH_HEAD ||
   ! git -C "$TMP/publication-host" push -q origin main; then FAIL=1; return; fi
PUB_MERGE=$(git -C "$TMP/publication-host" rev-parse HEAD)
PUB_REQUEST="{\"number\":175,\"base_branch\":\"main\",\"head_branch\":\"finalized-feature\",\"head_sha\":\"$PUB_HEAD\",\"merge_sha\":\"$PUB_MERGE\"}"
expect "PR is queued before its context arrives" "$(ccurl -sb "$J" -X POST "$B/repos/$PUB_RID/prs/promotions" -H 'Content-Type: application/json' -d "$PUB_REQUEST" | python3 -c 'import json,sys;print(json.load(sys.stdin).get("state"))')" waiting
if ! cxt push --append >"$TMP/pub-push.out" 2>&1; then cat "$TMP/pub-push.out" "$TMP/pub-rebase.out"; FAIL=1; return; fi
PUB_STATE=""
for i in $(seq 1 60); do
  PUB_STATE=$(curl -sb "$J" "$B/repos/$PUB_RID/prs/promotions" | python3 -c 'import json,sys;print(next((j["state"] for j in json.load(sys.stdin) if j["pr"]["number"]==175),""))')
  [ "$PUB_STATE" = completed ] && break
  sleep 0.25
done
expect "queued PR automatically completes after finalization" "$PUB_STATE" completed
PUB_HISTORY=$(curl -sb "$J" "$B/repos/$PUB_RID/history")
expect "squash finalization contains the last source context" "$(printf '%s' "$PUB_HISTORY" | python3 -c 'import json,sys;rows=json.load(sys.stdin);print("yes" if any(e["kind"]=="publish" and e.get("git_after")==sys.argv[1] and e.get("target")==sys.argv[2] for e in rows) else "no")' "$PUB_HEAD" "$PUB_LAST")" yes
expect "PR completion uses the finalized source" "$(printf '%s' "$PUB_HISTORY" | python3 -c 'import json,sys;rows=json.load(sys.stdin);print(sum(e.get("pr_completed",False) and e.get("source")==sys.argv[1] for e in rows))' "$PUB_LAST")" 1
expect "distinct original sessions were captured" "$([ "$PUB_FIRST" != "$PUB_LAST" ] && echo yes)" yes
expect "same-revision followup captured a new final source" "$([ "$PUB_SECOND" != "$PUB_LAST" ] && echo yes)" yes
expect "final source ancestry retains the earlier session" "$(curl -sb "$J" "$B/repos/$PUB_RID/snapshots" | python3 -c '
import json,sys
snaps={s["id"]:s for s in json.load(sys.stdin)};q=[sys.argv[1]];seen=set()
while q:
 h=q.pop()
 if h in seen: continue
 seen.add(h);s=snaps.get(h,{})
 q.extend((s.get("parents") or [])+(s.get("graft_parents") or []))
print("yes" if sys.argv[2] in seen else "no")' "$PUB_LAST" "$PUB_FIRST")" yes

PUB_PROMOTED=$(printf '%s' "$PUB_HISTORY" | python3 -c 'import json,sys;rows=json.load(sys.stdin);print(next(e["target"] for e in rows if e.get("pr_completed") and e.get("pr",{}).get("number")==175))')
expect "promotion advances beyond main's previous selection" "$([ -n "$PUB_PROMOTED" ] && [ "$PUB_PROMOTED" != "$PUB_BEFORE" ] && echo yes)" yes
cd "$PUB_MAIN"
if ! git pull --ff-only origin main >"$TMP/pub-main-git-pull.out" 2>&1; then cat "$TMP/pub-main-git-pull.out"; FAIL=1; return; fi
# Inspect the real Git-hook boundary before explicit context pull. The fake
# forge has no provider PR resolver, so this path can still need cxt pull.
PUB_POSTMERGE_POSITION=$(python3 - "$PUB_POSITION" "$PUB_PROMOTED" <<'PYPOSTMERGE'
import json,sys
p=json.load(open(sys.argv[1]))
print('promoted' if p.get('snapshot')==sys.argv[2] and p.get('shared_target')==sys.argv[2]
      else 'pending explicit cxt pull')
PYPOSTMERGE
)
echo "  post-merge selection: $PUB_POSTMERGE_POSITION"
expect "Git move records the forward fallback from the pre-merge position" "$(python3 - "$PUB_STORE/history" "$PUB_WORKTREE" "$PUB_BEFORE" "$PUB_CODE_BEFORE" "$PUB_MERGE" "$PUB_POSITION" <<'PYFALLBACK'
import json,pathlib,sys
events=[json.loads(p.read_text()) for p in pathlib.Path(sys.argv[1]).glob('*.json')]
# Selection is durable in position.json before its history event is replayed.
events.append(json.load(open(sys.argv[6])).get('selection') or {})
print('recorded' if any(e.get('kind')=='position' and e.get('worktree_id')==sys.argv[2]
      and e.get('source')==sys.argv[3] and e.get('target')==sys.argv[3]
      and e.get('git_before')==sys.argv[4] and e.get('git_after')==sys.argv[5]
      and e.get('memory_pinned') for e in events) else 'missing')
PYFALLBACK
)" recorded
if ! cxt pull >"$TMP/pub-main-context-pull.out" 2>&1; then cat "$TMP/pub-main-context-pull.out"; FAIL=1; return; fi
expect "main code receives the actual PR merge" "$(git rev-parse HEAD)" "$PUB_MERGE"
expect "main context ref receives the completed promotion" "$(ref_target "$PUB_STORE/refs/heads/main")" "$PUB_PROMOTED"
expect "pull refreshes the current worktree selection and shared target" "$(python3 - "$PUB_POSITION" "$PUB_PROMOTED" "$PUB_MERGE" <<'PYPOSITION'
import json,sys
p=json.load(open(sys.argv[1]))
ready=(p.get('branch')=='main' and p.get('snapshot')==sys.argv[2]
       and p.get('shared_target')==sys.argv[2] and p.get('git_commit')==sys.argv[3]
       and not p.get('rewound',False))
print('ready' if ready else json.dumps(p,sort_keys=True))
PYPOSITION
)" ready
# The existing app keeps writing its existing native file after the pull.
# Enlarge that transcript, then exercise the public command and normal push.
python3 - "$PUB_NATIVE" "$PWD" <<'PYTAIL'
import json,sys
with open(sys.argv[1],'a') as f:
    for role,text in [('user','continue after automatic PR promotion'),('assistant','post-promotion continuation captured')]:
        f.write(json.dumps({'type':role,'cwd':sys.argv[2],'sessionId':'sess-PUBCONTINUE',
                           'gitBranch':'main','timestamp':'2026-07-05T00:01:00Z',
                           'message':{'role':role,'content':text}})+'\n')
PYTAIL
if ! cxt commit -m post-promotion-continuation >"$TMP/pub-next-commit.out" 2>&1; then
  cat "$TMP/pub-next-commit.out"
  expect "next actual cxt commit captures enlarged native transcript" failed succeeded
  return
fi
expect "next actual cxt commit captures enlarged native transcript" succeeded succeeded
PUB_NEXT=$(ref_target "$PUB_STORE/refs/heads/main")
expect "next capture is a new snapshot" "$([ "$PUB_NEXT" != "$PUB_BEFORE" ] && [ "$PUB_NEXT" != "$PUB_PROMOTED" ] && echo yes)" yes
expect "next snapshot directly continues the promoted context" "$(python3 - "$PUB_STORE/objects/snapshots/${PUB_NEXT#sha256:}" "$PUB_PROMOTED" <<'PYNEXT'
import json,sys
s=json.load(open(sys.argv[1]))
print('continued' if sys.argv[2] in (s.get('parents') or []) and s.get('session_id')=='sess-PUBCONTINUE' else json.dumps(s,sort_keys=True))
PYNEXT
)" continued
if ! cxt push >"$TMP/pub-next-push.out" 2>&1; then cat "$TMP/pub-next-push.out"; FAIL=1; return; fi
expect "normal push publishes the next continuation on main" "$(curl -sb "$J" "$B/repos/$PUB_RID/refs" | python3 -c 'import json,sys;print(next(r["target"] for r in json.load(sys.stdin) if r["kind"]=="branch" and r["name"]=="main"))')" "$PUB_NEXT"
expect "pushed document contains the enlarged native transcript" "$(curl -sb "$J" "$B/repos/$PUB_RID/docs/$PUB_NEXT" | python3 -c 'import json,sys;body=json.dumps(json.load(sys.stdin));print("captured" if "task PUBCONTINUE" in body and "post-promotion continuation captured" in body else "missing")')" captured
expect "server continuation retains promoted and earlier main ancestry" "$(curl -sb "$J" "$B/repos/$PUB_RID/snapshots" | python3 -c '
import json,sys
snaps={s["id"]:s for s in json.load(sys.stdin)};q=[sys.argv[1]];seen=set()
while q:
 h=q.pop()
 if h in seen: continue
 seen.add(h);s=snaps.get(h,{})
 q.extend((s.get("parents") or [])+(s.get("graft_parents") or []))
print("retained" if all(h in seen for h in sys.argv[2:]) else "lost")' "$PUB_NEXT" "$PUB_PROMOTED" "$PUB_BEFORE" "$PUB_LAST")" retained
