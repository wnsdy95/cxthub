# PostgreSQL alone supports proven-new creation; no legacy fallback in this test.
if [ -n "${CXT_E2E_DSN:-}" ]; then
  echo "── U. First owner: setup a fresh clone before any context exists"
  if (
    mkdir -p "$TMP/u-deny-native" || exit 1
    export U_PROVIDER_CALLS="$TMP/u-provider-calls.out"
    : >"$U_PROVIDER_CALLS"
    for U_PROVIDER in claude codex gemini opencode gh; do
      cat >"$TMP/u-deny-native/$U_PROVIDER" <<'SHUDENY'
#!/bin/sh
printf '%s\n' "${0##*/}" >>"$U_PROVIDER_CALLS"
exit 97
SHUDENY
      chmod +x "$TMP/u-deny-native/$U_PROVIDER" || exit 1
    done
    export PATH="$TMP/u-deny-native:$PATH"
    U_SLUG=$(ccurl -fsSb "$J" -X POST "$B/repositories" -H 'Content-Type: application/json' \
      -d '{"name":"FirstOwnerSetupE2E"}' | python3 -c 'import json,sys; print(json.load(sys.stdin)["slug"])') || exit 1
    U_REMOTE="$ORIGIN/$OWN/$U_SLUG"
    U_GIT_REMOTE=https://git.example.invalid/team/first-owner.git
    U_BARE="$TMP/u-code.git"
    git config --global url."$U_BARE".insteadOf "$U_GIT_REMOTE" || exit 1
    git init -q --bare "$U_BARE" || exit 1
    mkdir -p "$TMP/u-author"; cd "$TMP/u-author" || exit 1
    git init -q -b owner-feature && git remote add origin "$U_GIT_REMOTE" &&
      git commit -q --allow-empty -m first-owner-code && git push -qu origin owner-feature || exit 1
    git clone -q -b owner-feature "$U_GIT_REMOTE" "$TMP/u-clone" || exit 1
    cd "$TMP/u-clone" || exit 1
    [ ! -e .cxt ] || exit 1
    U_CODE=$(git rev-parse HEAD) || exit 1
    cxt setup "$U_REMOTE" --no-login >"$TMP/u-setup.out" 2>&1 || { cat "$TMP/u-setup.out"; exit 1; }
    U_RID=$(python3 - "$U_REMOTE" <<'PYUID'
import hashlib,sys,urllib.parse
u=urllib.parse.urlsplit(sys.argv[1])
print('sha256:'+hashlib.sha256((u.netloc+u.path).lower().rstrip('/').encode()).hexdigest())
PYUID
    ) || exit 1
    ccurl -fsSb "$J" "$B/repos/$U_RID?initial_branch=owner-feature" >"$TMP/u-server-empty.json" || exit 1
    ccurl -fsSb "$J" "$B/repos/$U_RID/refs" >"$TMP/u-refs-empty.json" || exit 1
    ccurl -fsSb "$J" "$B/repos/$U_RID/history" >"$TMP/u-history-empty.json" || exit 1
    if ! python3 - "$TMP" "$U_RID" "$U_REMOTE" "$U_GIT_REMOTE" "$U_CODE" <<'PYUEMPTY'
import hashlib,json,pathlib,subprocess,sys
p=pathlib.Path(sys.argv[1]); rid,remote,git_remote,code=sys.argv[2:]
repo=json.loads((p/'u-server-empty.json').read_text())
assert repo['id']==rid and repo['context_protocol']==1 and repo['initial_anchor_available']
assert repo['git_remote_url']==git_remote and repo['remote_url']==remote
assert not json.loads((p/'u-refs-empty.json').read_text()), 'setup published a context ref'
assert not json.loads((p/'u-history-empty.json').read_text()), 'setup fabricated context history'
assert not list(pathlib.Path('.cxt/refs/heads').glob('**/*')), 'setup fabricated local branch ref'
assert not list(pathlib.Path('.cxt/history').glob('*.json')), 'setup fabricated local history'
key=hashlib.sha256(subprocess.check_output(['git','rev-parse','--absolute-git-dir'],text=True).strip().encode()).hexdigest()[:32]
position=json.loads((pathlib.Path('.cxt/worktrees')/key/'position.json').read_text())
assert position['repo_id']==rid and position['branch']=='owner-feature'
assert position['branch_id']=='legacy-'+hashlib.sha256((rid+'\x00owner-feature').encode()).hexdigest()[:32]
assert position['git_commit']==code==subprocess.check_output(['git','rev-parse','HEAD'],text=True).strip()
assert all(not position.get(k) for k in ('snapshot','shared_target','memory_hash','memory_source','selection','memory_pinned','orphan','rewound'))
assert not (p/'u-provider-calls.out').read_text()
(p/'u-empty-position.json').write_text(json.dumps(position))
PYUEMPTY
    then cat "$TMP/u-setup.out"; exit 1; fi

    # No extra Git move may repair a provisional position before first capture.
    session "$PWD" FIRSTOWNER
    printf '{"cwd":"%s","session_id":"sess-FIRSTOWNER","transcript_path":"%s","prompt":"first work"}\n' "$PWD" "$D/sess-FIRSTOWNER.jsonl" |
      cxt hook --provider claude --event UserPromptSubmit >"$TMP/u-capture.out" 2>&1 || exit 1
    cxt add claude >>"$TMP/u-capture.out" 2>&1 &&
      cxt commit -m 'first owner context' >>"$TMP/u-capture.out" 2>&1 || { cat "$TMP/u-capture.out"; exit 1; }
    printf '{"cwd":"%s","session_id":"sess-FIRSTOWNER","transcript_path":"%s"}\n' "$PWD" "$D/sess-FIRSTOWNER.jsonl" |
      cxt hook --provider claude --event SessionEnd >>"$TMP/u-capture.out" 2>&1 || exit 1
    cxt push origin owner-feature >"$TMP/u-push.out" 2>&1 || { cat "$TMP/u-push.out"; exit 1; }
    ccurl -fsSb "$J" "$B/repos/$U_RID/refs" >"$TMP/u-refs-published.json" || exit 1
    ccurl -fsSb "$J" "$B/repos/$U_RID/history" >"$TMP/u-history-published.json" || exit 1
    python3 - "$TMP/u-before-retry.json" <<'PYUSAVE'
import json,pathlib,sys
paths=[pathlib.Path('.cxt/HEAD'),pathlib.Path('.cxt/refs/heads/owner-feature'),*pathlib.Path('.cxt/worktrees').glob('*/position.json')]
pathlib.Path(sys.argv[1]).write_text(json.dumps({str(p):p.read_text() for p in paths}))
PYUSAVE
    [ "$?" = 0 ] || exit 1
    cxt setup "$U_REMOTE" --no-login >"$TMP/u-retry.out" 2>&1 || { cat "$TMP/u-retry.out"; exit 1; }
    ccurl -fsSb "$J" "$B/repos/$U_RID/refs" >"$TMP/u-refs-retry.json" || exit 1
    ccurl -fsSb "$J" "$B/repos/$U_RID/history" >"$TMP/u-history-retry.json" || exit 1
    if ! python3 - "$TMP" "$U_RID" "$U_CODE" <<'PYUFINAL'
import hashlib,json,pathlib,subprocess,sys
p=pathlib.Path(sys.argv[1]); rid,code=sys.argv[2:]; before=json.loads((p/'u-before-retry.json').read_text())
assert all(pathlib.Path(path).read_text()==raw for path,raw in before.items()), 'setup retry reset captured context'
assert subprocess.check_output(['git','rev-parse','HEAD'],text=True).strip()==code, 'setup or capture moved Git code'
ref=json.loads(pathlib.Path('.cxt/refs/heads/owner-feature').read_text())
assert ref['repo_id']==rid and ref['name']=='owner-feature'
assert ref['branch_id']=='legacy-'+hashlib.sha256((rid+'\x00owner-feature').encode()).hexdigest()[:32]
refs=json.loads((p/'u-refs-published.json').read_text())
branches=[r for r in refs if r['kind']=='branch']
assert len(branches)==1 and all(branches[0][k]==ref[k] for k in ('repo_id','name','branch_id','target')), 'first branch publication not exact'
events=json.loads((p/'u-history-published.json').read_text())
assert not any(e['kind'] in ('birth','orphan','attach') for e in events), 'existing Git branch got fabricated birth/attachment'
assert any(e['kind']=='publish' and e.get('target')==ref['target'] and e.get('repo_id')==rid and e.get('branch')=='owner-feature' and e.get('branch_id')==ref['branch_id'] and e.get('git_after')==code for e in events), 'missing exact same-code publication'
normalize=lambda rows: sorted(json.dumps(row,sort_keys=True) for row in rows)
assert normalize(refs)==normalize(json.loads((p/'u-refs-retry.json').read_text())), 'setup retry changed server refs'
assert normalize(events)==normalize(json.loads((p/'u-history-retry.json').read_text())), 'setup retry changed server history'
assert not (p/'u-provider-calls.out').read_text(), 'provider was invoked'
PYUFINAL
    then cat "$TMP/u-push.out" "$TMP/u-retry.out"; exit 1; fi
  ); then
    expect "first-owner setup permits same-code capture and exact initial push without fabricated birth" yes yes
  else
    echo '  U failed; inspect u-setup.out, u-capture.out, u-push.out and u-*-position.json'
    FAIL=1; CXT_E2E_KEEP_TMP=1
  fi
fi
