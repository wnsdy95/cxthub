# Sourced by the isolated sync E2E. A/B retain legacy root-append coverage;
# this separate scenario must adopt a proven modern main on first setup.
echo "── T. Modern main → fresh Git clone → setup AUTO → ordinary same-identity push"
if (
  # Scope executable denial to T; never invoke a native provider or inherit the
  # earlier wrapper's fake provider. Even a swallowed provider error fails T.
  mkdir -p "$TMP/t-deny-native" || exit 1
  export T_PROVIDER_CALLS="$TMP/t-provider-calls.out"
  : >"$T_PROVIDER_CALLS"
  for T_PROVIDER in claude codex gemini opencode gh; do
    cat >"$TMP/t-deny-native/$T_PROVIDER" <<'SHTDENY'
#!/bin/sh
printf '%s\n' "${0##*/}" >>"$T_PROVIDER_CALLS"
exit 97
SHTDENY
    chmod +x "$TMP/t-deny-native/$T_PROVIDER" || exit 1
  done
  export PATH="$TMP/t-deny-native:$PATH"
  T_SLUG=$(ccurl -fsSb "$J" -X POST "$B/repositories" -H 'Content-Type: application/json' \
    -d '{"name":"ModernSetupE2E"}' | python3 -c 'import json,sys; print(json.load(sys.stdin)["slug"])') || exit 1
  T_REMOTE="$ORIGIN/$OWN/$T_SLUG"
  T_RID=$(python3 - "$T_REMOTE" <<'PYTID'
import hashlib,sys,urllib.parse
u=urllib.parse.urlsplit(sys.argv[1])
print('sha256:'+hashlib.sha256((u.netloc+u.path).lower().rstrip('/').encode()).hexdigest())
PYTID
  ) || exit 1
  T_BARE="$TMP/t-setup-code.git"
  T_GIT_REMOTE=https://git.example.invalid/team/setup.git
  # Preserve a valid raw origin for server proof; all Git transport stays local.
  git config --global url."$T_BARE".insteadOf "$T_GIT_REMOTE" || exit 1
  git init -q --bare "$T_BARE" || exit 1
  mkdir -p "$TMP/t-author"; cd "$TMP/t-author" || exit 1
  git init -q -b setup-source && git remote add origin "$T_GIT_REMOTE" &&
    git commit -q --allow-empty -m setup-source-code || exit 1
  T_TRANSPORT=$(git remote get-url --push origin) || exit 1
  [ "$T_TRANSPORT" = "$T_BARE" ] || { echo 'T Git transport escaped owned bare'; exit 1; }
  cxt init >"$TMP/t-init.out" 2>&1 || { cat "$TMP/t-init.out"; exit 1; }
  if [ -z "${CXT_E2E_DSN:-}" ]; then
    # FS has no atomic new-repo initialization. Explicitly register and protect
    # an empty fixture; the modern main below still needs its real birth proof.
    fixture_register_legacy_repo "$T_REMOTE" "$T_GIT_REMOTE" || exit 1
    ccurl -fsSb "$J" -X POST "$B/repos/$T_RID/context-protocol" -H 'Content-Type: application/json' \
      -d '{}' >"$TMP/t-fs-protection.json" || exit 1
  fi
  cxt remote add origin "$T_REMOTE" >"$TMP/t-connect.out" 2>&1 || { cat "$TMP/t-connect.out"; exit 1; }
  # init predates the server binding. Move real Git code while no capture
  # exists, so reference-transaction selects the canonical repo/branch first.
  git commit -q --allow-empty -m setup-connected-code >"$TMP/t-connected-commit.out" 2>&1 || { cat "$TMP/t-connected-commit.out"; exit 1; }
  if ! python3 - "$T_RID" <<'PYTCONNECTED'
import hashlib,json,pathlib,subprocess,sys
rid=sys.argv[1]
code=subprocess.check_output(['git','rev-parse','HEAD'],text=True).strip()
admin=subprocess.check_output(['git','rev-parse','--absolute-git-dir'],text=True).strip()
key=hashlib.sha256(admin.encode()).hexdigest()[:32]
position=json.loads((pathlib.Path('.cxt/worktrees')/key/'position.json').read_text())
assert position['repo_id']==rid and position['branch']=='setup-source' and position['git_commit']==code, 'connected Git move did not establish exact seed selection'
assert position['branch_id']=='legacy-'+hashlib.sha256((rid+'\x00setup-source').encode()).hexdigest()[:32], 'seed selected wrong identity'
assert not position.get('snapshot') and not position.get('shared_target'), 'seed captured before canonical code selection'
assert not list(pathlib.Path('.cxt/objects/snapshots').glob('*')), 'unexpected pre-seed snapshot'
PYTCONNECTED
  then cat "$TMP/t-connected-commit.out"; exit 1; fi
  session "$PWD" TSETUPSEED
  printf '{"cwd":"%s","session_id":"sess-TSETUPSEED","transcript_path":"%s","prompt":"seed"}\n' "$PWD" "$D/sess-TSETUPSEED.jsonl" |
    cxt hook --provider claude --event UserPromptSubmit >"$TMP/t-seed-hook.out" 2>&1 || exit 1
  T_SEED_CODE=$(git rev-parse HEAD) || exit 1
  if ! cxt add claude >"$TMP/t-seed.out" 2>&1 ||
     ! cxt commit -m "setup source [git $T_SEED_CODE]" >>"$TMP/t-seed.out" 2>&1; then
    cat "$TMP/t-seed.out"; exit 1
  fi
  printf '{"cwd":"%s","session_id":"sess-TSETUPSEED","transcript_path":"%s"}\n' "$PWD" "$D/sess-TSETUPSEED.jsonl" |
    cxt hook --provider claude --event SessionEnd >>"$TMP/t-seed-hook.out" 2>&1 || exit 1
  if ! python3 "$ROOT/scripts/e2e-drain-publication.py" "$TMP/bin/cxt" "$PWD" >"$TMP/t-seed-drain.out" 2>&1; then
    cat "$TMP/t-seed-drain.out"; exit 1
  fi
  # main did not exist before this command. No rename, fabricated history or
  # initial-legacy observation may substitute for its actual creation event.
  CXT_KEEP_SESSION=1 git switch -q -c main >"$TMP/t-birth.out" 2>&1 || { cat "$TMP/t-birth.out"; exit 1; }
  session "$PWD" TSETUPA
  printf '{"cwd":"%s","session_id":"sess-TSETUPA","transcript_path":"%s","prompt":"author A"}\n' "$PWD" "$D/sess-TSETUPA.jsonl" |
    cxt hook --provider claude --event UserPromptSubmit >"$TMP/t-a-hook.out" 2>&1 || exit 1
  echo A > a.txt
  git add a.txt && git commit -qm setup-A >"$TMP/t-a-commit.out" 2>&1 || { cat "$TMP/t-a-commit.out"; exit 1; }
  printf '{"cwd":"%s","session_id":"sess-TSETUPA","transcript_path":"%s"}\n' "$PWD" "$D/sess-TSETUPA.jsonl" |
    cxt hook --provider claude --event SessionEnd >>"$TMP/t-a-hook.out" 2>&1 || exit 1
  if ! python3 "$ROOT/scripts/e2e-drain-publication.py" "$TMP/bin/cxt" "$PWD" >"$TMP/t-a-drain.out" 2>&1; then
    cat "$TMP/t-a-drain.out"; exit 1
  fi
  T_CODE_A=$(git rev-parse HEAD) || exit 1
  git push -qu origin main >"$TMP/t-a-push.out" 2>&1 || { cat "$TMP/t-a-push.out"; exit 1; }
  ccurl -fsSb "$J" "$B/repos/$T_RID" >"$TMP/t-server-before.json" || exit 1
  ccurl -fsSb "$J" "$B/repos/$T_RID/refs" >"$TMP/t-refs-before.json" || exit 1
  ccurl -fsSb "$J" "$B/repos/$T_RID/history" >"$TMP/t-history-before.json" || exit 1
  if ! python3 - "$TMP" "$T_RID" "$T_REMOTE" "$T_BARE" "$T_CODE_A" "$T_SEED_CODE" "$T_GIT_REMOTE" <<'PYTA'
import json,pathlib,re,subprocess,sys
p=pathlib.Path(sys.argv[1]); rid,remote,bare,code,seed_code,git_remote=sys.argv[2:]
repo=json.loads((p/'t-server-before.json').read_text())
assert repo['id']==rid and repo['context_protocol']==1
assert repo['remote_url']==remote and repo['git_remote_url']==git_remote, 'wrong server origin'
assert subprocess.check_output(['git','config','--get','remote.origin.url'],text=True).strip()==git_remote
assert subprocess.check_output(['git','remote','get-url','--push','origin'],text=True).strip()==bare
ref=json.loads(pathlib.Path('.cxt/refs/heads/main').read_text())
assert ref['repo_id']==rid and re.fullmatch('[0-9a-f]{32}',ref['branch_id']), 'main is not a modern identity'
local=[json.loads(f.read_text()) for f in pathlib.Path('.cxt/history').glob('*.json')]
births=[e for e in local if e['kind']=='birth' and e['branch']=='main']
assert len(births)==1 and births[0]['id']==ref['branch_id'], 'missing exact main birth'
birth=births[0]; creation=birth.get('creation',{})
assert creation.get('evidence')=='process-argv' and creation.get('command')==['git','switch','-q','-c','main'], 'main birth lacks actual Git command'
assert birth['git_after']==seed_code and creation['start_commit']==seed_code and creation['origin_branch']=='setup-source'
history=json.loads((p/'t-history-before.json').read_text())
assert birth in history, 'server did not accept exact birth payload'
branches=[r for r in json.loads((p/'t-refs-before.json').read_text()) if r['kind']=='branch']
assert len(branches)==1 and all(branches[0][k]==ref[k] for k in ('repo_id','name','branch_id','target')), 'first main push widened scope'
assert any(e['kind']=='publish' and e['branch_id']==ref['branch_id'] and e.get('git_after')==code and e.get('target')==ref['target'] for e in history), 'A lacks exact published code pin'
(p/'t-a-proof.json').write_text(json.dumps({'ref':ref,'birth':birth,'code':code})+'\n')
assert not (p/'t-provider-calls.out').read_text(), 'native provider was invoked'
PYTA
  then cat "$TMP/t-birth.out" "$TMP/t-a-push.out"; exit 1; fi

  # Fresh Git clone has neither .cxt nor provider sessions. setup is the only
  # adoption command: no explicit cxt pull/checkout or local metadata repair.
  git clone -q "$T_GIT_REMOTE" "$TMP/t-clone" || exit 1
  cd "$TMP/t-clone" || exit 1
  [ ! -e .cxt ] || { echo 'T clone unexpectedly contains context state'; exit 1; }
  cxt setup "$T_REMOTE" --no-login >"$TMP/t-setup.out" 2>&1 || { cat "$TMP/t-setup.out"; exit 1; }
  if ! python3 - "$TMP" "$T_REMOTE" "$T_BARE" "$T_GIT_REMOTE" <<'PYTSETUP'
import hashlib,json,pathlib,subprocess,sys
p=pathlib.Path(sys.argv[1]); remote,bare,git_remote=sys.argv[2:]
proof=json.loads((p/'t-a-proof.json').read_text()); ref=proof['ref']
assert subprocess.check_output(['git','rev-parse','HEAD'],text=True).strip()==proof['code']
assert subprocess.check_output(['git','config','--get','remote.origin.url'],text=True).strip()==git_remote
assert subprocess.check_output(['git','remote','get-url','origin'],text=True).strip()==bare
assert subprocess.check_output(['git','remote','get-url','--push','origin'],text=True).strip()==bare
config=json.loads(pathlib.Path('.cxt/config').read_text())
assert config['remotes']=={'origin':remote}, 'setup changed canonical server origin'
assert config.get('checkout_mode','auto')=='auto', 'setup did not retain AUTO mode'
got=json.loads(pathlib.Path('.cxt/refs/heads/main').read_text())
assert all(got[k]==ref[k] for k in ('repo_id','name','branch_id','target')), 'setup did not adopt modern main'
key=hashlib.sha256(subprocess.check_output(['git','rev-parse','--absolute-git-dir'],text=True).strip().encode()).hexdigest()[:32]
position=json.loads((pathlib.Path('.cxt/worktrees')/key/'position.json').read_text())
assert position['repo_id']==ref['repo_id'] and position['branch']=='main' and position['branch_id']==ref['branch_id']
assert position['git_commit']==proof['code'] and position['snapshot']==ref['target'] and position['shared_target']==ref['target'], 'setup lacks exact code/context selection'
assert not position.get('orphan') and not position.get('rewound')
history=[json.loads(f.read_text()) for f in pathlib.Path('.cxt/history').glob('*.json')]
assert proof['birth'] in history, 'setup lost raw accepted birth'
assert [e for e in history if e['kind'] in ('birth','orphan') and e['branch']=='main']==[proof['birth']], 'setup fabricated main identity'
assert not (p/'t-provider-calls.out').read_text(), 'setup invoked native provider'
PYTSETUP
  then cat "$TMP/t-setup.out"; exit 1; fi
  ccurl -fsSb "$J" "$B/repos/$T_RID/refs" >"$TMP/t-refs-after-setup.json" || exit 1
  if ! python3 - "$TMP" <<'PYTNOREF'
import json,pathlib,sys
p=pathlib.Path(sys.argv[1]); key=lambda r:(r['kind'],r['name'])
assert sorted(json.loads((p/'t-refs-before.json').read_text()),key=key)==sorted(json.loads((p/'t-refs-after-setup.json').read_text()),key=key), 'setup mutated a server ref'
PYTNOREF
  then exit 1; fi

  session "$PWD" TSETUPB
  printf '{"cwd":"%s","session_id":"sess-TSETUPB","transcript_path":"%s","prompt":"continue from A"}\n' "$PWD" "$D/sess-TSETUPB.jsonl" |
    cxt hook --provider claude --event UserPromptSubmit >"$TMP/t-b-hook.out" 2>&1 || exit 1
  echo B > b.txt
  git add b.txt && git commit -qm setup-B >"$TMP/t-b-commit.out" 2>&1 || { cat "$TMP/t-b-commit.out"; exit 1; }
  printf '{"cwd":"%s","session_id":"sess-TSETUPB","transcript_path":"%s"}\n' "$PWD" "$D/sess-TSETUPB.jsonl" |
    cxt hook --provider claude --event SessionEnd >>"$TMP/t-b-hook.out" 2>&1 || exit 1
  # SessionEnd can return before durable capture applies the current ref.
  if ! python3 "$ROOT/scripts/e2e-drain-publication.py" "$TMP/bin/cxt" "$PWD" >"$TMP/t-b-drain.out" 2>&1; then
    cat "$TMP/t-b-drain.out"; exit 1
  fi
  T_CODE_B=$(git rev-parse HEAD) || exit 1
  if ! python3 - "$TMP" "$T_CODE_B" <<'PYTCAPTURE'
import json,pathlib,sys
p=pathlib.Path(sys.argv[1]); code=sys.argv[2]; proof=json.loads((p/'t-a-proof.json').read_text()); before=proof['ref']
ref=json.loads(pathlib.Path('.cxt/refs/heads/main').read_text())
assert all(ref[k]==before[k] for k in ('repo_id','name','branch_id')) and ref['target']!=before['target'], 'capture replaced identity or failed to advance'
seen=set(); pending=[ref['target']]
while pending:
    h=pending.pop()
    if h in seen: continue
    seen.add(h); snap=json.loads((pathlib.Path('.cxt/objects/snapshots')/h.removeprefix('sha256:')).read_text())
    assert not snap.get('graft_parents') and not snap.get('grafted'), 'new clone needed a repair graft'
    pending.extend(snap.get('parents') or [])
assert before['target'] in seen, 'capture did not naturally inherit proven A'
history=[json.loads(f.read_text()) for f in pathlib.Path('.cxt/history').glob('*.json')]
assert any(e['kind']=='publish' and e['branch_id']==ref['branch_id'] and e.get('git_after')==code and e.get('target')==ref['target'] for e in history), 'B lacks exact code pin'
assert not (p/'t-provider-calls.out').read_text(), 'capture invoked native provider'
(p/'t-b-proof.json').write_text(json.dumps({'ref':ref,'code':code})+'\n')
PYTCAPTURE
  then cat "$TMP/t-b-hook.out" "$TMP/t-b-commit.out"; exit 1; fi
  git push -q origin main >"$TMP/t-b-push.out" 2>&1 || { cat "$TMP/t-b-push.out"; exit 1; }
  ccurl -fsSb "$J" "$B/repos/$T_RID" >"$TMP/t-server-after.json" || exit 1
  ccurl -fsSb "$J" "$B/repos/$T_RID/refs" >"$TMP/t-refs-after.json" || exit 1
  ccurl -fsSb "$J" "$B/repos/$T_RID/history" >"$TMP/t-history-after.json" || exit 1
  ccurl -fsSb "$J" "$B/repos/$T_RID/snapshots" >"$TMP/t-snapshots-after.json" || exit 1
  if ! python3 - "$TMP" "$T_REMOTE" "$T_BARE" "$T_GIT_REMOTE" <<'PYTFINAL'
import json,pathlib,subprocess,sys
p=pathlib.Path(sys.argv[1]); remote,bare,git_remote=sys.argv[2:]
a=json.loads((p/'t-a-proof.json').read_text()); b=json.loads((p/'t-b-proof.json').read_text()); ref=b['ref']
assert 'appended' not in (p/'t-b-push.out').read_text().lower(), 'ordinary clone push used auto-append'
refs=[r for r in json.loads((p/'t-refs-after.json').read_text()) if r['kind']=='branch']
assert len(refs)==1 and all(refs[0][k]==ref[k] for k in ('repo_id','name','branch_id','target')), 'server did not accept exact same-identity B'
history=json.loads((p/'t-history-after.json').read_text())
assert a['birth'] in history and [e for e in history if e['kind'] in ('birth','orphan') and e['branch']=='main']==[a['birth']]
assert any(e['kind']=='publish' and e['branch_id']==ref['branch_id'] and e.get('git_after')==b['code'] and e.get('target')==ref['target'] for e in history), 'server lacks exact B pin'
snaps={s['id']:s for s in json.loads((p/'t-snapshots-after.json').read_text())}; seen=set(); pending=[ref['target']]
while pending:
    h=pending.pop()
    if h in seen: continue
    seen.add(h); snap=snaps[h]
    assert not snap.get('graft_parents') and not snap.get('grafted'), 'ordinary push appended/grafted'
    pending.extend(snap.get('parents') or [])
assert a['ref']['target'] in seen, 'server lost natural A ancestry'
before=json.loads((p/'t-server-before.json').read_text()); after=json.loads((p/'t-server-after.json').read_text())
assert all(after[k]==before[k] for k in ('id','remote_url','git_remote_url','context_protocol','repository_id')), 'server origin changed'
assert after['remote_url']==remote and after['git_remote_url']==git_remote
assert json.loads(pathlib.Path('.cxt/config').read_text())['remotes']=={'origin':remote}
assert subprocess.check_output(['git','config','--get','remote.origin.url'],text=True).strip()==git_remote
assert subprocess.check_output(['git','remote','get-url','origin'],text=True).strip()==bare
assert subprocess.check_output(['git','remote','get-url','--push','origin'],text=True).strip()==bare
assert not (p/'t-provider-calls.out').read_text(), 'native provider was invoked'
PYTFINAL
  then cat "$TMP/t-b-push.out"; exit 1; fi
); then
  expect "setup adopts exact modern main; capture and ordinary push retain identity/ancestry/origin" yes yes
else
  echo '  T failed; inspect t-setup.out, t-*-proof.json and bounded t-*.out diagnostics'
  FAIL=1; CXT_E2E_KEEP_TMP=1
fi
