# Sourced by e2e-sync.sh. All state belongs to its isolated HOME/server.
echo "── P. Real commit → squash → publication barrier → queued PR promotion"
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
if ! CXT_KEEP_SESSION=1 git switch -qc finalized-feature >"$TMP/pub-birth.out" 2>&1; then cat "$TMP/pub-birth.out"; FAIL=1; return; fi
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
PUB_MERGE=$(printf 'b%.0s' $(seq 1 40))
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
