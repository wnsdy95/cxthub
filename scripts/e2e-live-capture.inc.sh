# Sourced by e2e-sync.sh: exercises the installed lifecycle hook -> detached
# observer -> fresh CLI capture -> backend path, without a Stop event.
echo "── Live app capture during an unfinished turn"
cd "$TMP/repo1"
LIVE_SHARED_BEFORE=$(main_head)
for LIVE_PROVIDER in claude codex; do
  LIVE_ID="11111111-1960-4196-8196-111111111111"
  if [ "$LIVE_PROVIDER" = codex ]; then LIVE_ID="22222222-1960-4196-8196-222222222222"; fi
  LIVE_PATH=$(python3 - "$HOME" "$TMP/repo1" "$LIVE_PROVIDER" "$LIVE_ID" <<'PY'
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
  printf '{"cwd":"%s","session_id":"%s","transcript_path":"%s","prompt":"synthetic live turn"}\n' "$TMP/repo1" "$LIVE_ID" "$LIVE_PATH" | cxt hook --provider "$LIVE_PROVIDER" --event UserPromptSubmit >/dev/null
  LIVE_FIRST=""
  for i in $(seq 1 50); do
    LIVE_FIRST=$(curl -sb "$J" "$B/repos/$RID/pending" | python3 -c 'import json,sys; print(next((p["target"] for p in json.load(sys.stdin) if p["session_id"]==sys.argv[1] and p.get("activity_at")),""))' "$LIVE_ID")
    [ -n "$LIVE_FIRST" ] && break
    sleep 0.3
  done
  expect "$LIVE_PROVIDER prompt captured before Stop" "$([ -n "$LIVE_FIRST" ] && echo yes)" yes
  python3 - "$LIVE_PATH" "$LIVE_PROVIDER" "$LIVE_ID" "$TMP/repo1" <<'PY'
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
  printf '{"cwd":"%s","session_id":"%s","transcript_path":"%s"}\n' "$TMP/repo1" "$LIVE_ID" "$LIVE_PATH" | cxt hook --provider "$LIVE_PROVIDER" --event SessionEnd >/dev/null
  expect "$LIVE_PROVIDER observation preserves transcript" "$([ -f "$LIVE_PATH" ] && echo yes)" yes
done
expect "live capture never advances main" "$(main_head)" "$LIVE_SHARED_BEFORE"
