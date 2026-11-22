#!/usr/bin/env bash
# cxthub E2E suite — runs the real cxtd and cxt binaries against scenarios first
# verified during dogfooding. Uses the FS store; PostgreSQL is covered by TestPGSmoke.
#
# Run from anywhere in the repository; isolated TMP, HOME, and ports leave no residue.
set -u

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
TMP="$(mktemp -d)"
# Randomize ports to avoid concurrent-run collisions. The webhook hit file also stays under $TMP.
PORT=$((18000 + RANDOM % 900))
PORT2=$((PORT + 1))
STUB_PORT=$((PORT + 2))
HITFILE="$TMP/webhook-hits.txt"
B="http://127.0.0.1:$PORT/api/v1"
ORIGIN="http://127.0.0.1:$PORT"
FAIL=0
SRV_PID=""; SRV2_PID=""; STUB_PID=""

GUARD_PID=""
cleanup() {
  # Reap each process immediately after killing it to avoid shell "Terminated" noise.
  for p in "$SRV_PID" "$SRV2_PID" "$STUB_PID" "$GUARD_PID"; do
    [ -n "$p" ] && kill "$p" 2>/dev/null && wait "$p" 2>/dev/null
  done
  rm -rf "$TMP"
}
trap cleanup EXIT

expect() { # expect <description> <actual> <expected>
  if [ "$#" -ne 3 ]; then echo "  ✗ ${1:-unnamed assertion}: expected 3 arguments, got $#"; FAIL=1; return; fi
  if [ "$2" = "$3" ]; then echo "  ✓ $1"; else echo "  ✗ $1  (got=$2 want=$3)"; FAIL=1; fi
}
jget() { python3 -c "import json,sys;print(json.load(sys.stdin)$1)"; }
code() { curl -s -o /dev/null -w '%{http_code}' "$@"; }
ccurl() { command curl -H "Origin: $ORIGIN" -H 'X-Cxt-CSRF: 1' "$@"; }
ccode() { code -H "Origin: $ORIGIN" -H 'X-Cxt-CSRF: 1' "$@"; }
repo_id() { # repo_id <cxthub remote URL> — normalize exactly as CLI/backend, then hash with SHA-256.
  python3 - "$1" <<'PY'
import hashlib, sys
u = sys.argv[1].strip()
if u.startswith("git@"):
    u = u[4:].replace(":", "/", 1)
for prefix in ("https://", "http://"):
    if u.startswith(prefix):
        u = u[len(prefix):]
if u.endswith(".git"):
    u = u[:-4]
u = u.rstrip("/").lower()
print("sha256:" + hashlib.sha256(u.encode()).hexdigest())
PY
}
fixture_objects() { # fixture_objects <repoID> — emit a batch whose DocHash matches canonical CIR.
  python3 - "$1" <<'PY'
import hashlib, json, sys
repo_id = sys.argv[1]
cir = {
    "envelope": {
        "cir_version": "1",
        "source_provider": "claude",
        "source_model": "",
        "captured_at": "",
        "cwd": "",
        "git_branch": "main",
        "session_origin_id": "",
        "fidelity": "full",
    },
    "events": [{
        "kind": "message",
        "seq": 0,
        "role": "user",
        "blocks": [{"type": "text", "text": "Check webhook SSRF handling."}],
    }],
}
canonical = json.dumps(cir, sort_keys=True, separators=(",", ":")).encode()
content_hash = "sha256:" + hashlib.sha256(canonical).hexdigest()
body = {
    "docs": [{"hash": content_hash, "cir": cir}],
    "snapshots": [{
        "id": content_hash,
        "repo_id": repo_id,
        "branch": "main",
        "parents": [],
        "doc_hash": content_hash,
        "provider": "claude",
        "fidelity": "full",
        "message": "test: verify webhook delivery",
        "author": {},
    }],
}
print(content_hash)
print(json.dumps(body, separators=(",", ":")))
PY
}

echo "── Build (isolated bin)"
( cd "$ROOT/backend" && go build -o "$TMP/bin/cxtd" ./cmd/cxtd ) || { echo "cxtd build failed"; exit 1; }
( cd "$ROOT/cli" && go build -o "$TMP/bin/cxt" ./cmd/cxt ) || { echo "cxt build failed"; exit 1; }
export PATH="$TMP/bin:$PATH"
export HOME="$TMP/home"; mkdir -p "$HOME"
git config --global user.email e2e@test.local
git config --global user.name E2E
git config --global init.defaultBranch main

echo "── A. dev authentication guard"
# Use background execution plus bounded polling because macOS does not ship a portable `timeout`.
# If the guard regresses and cxtd stays alive, terminate it after three seconds and fail cleanly.
"$TMP/bin/cxtd" serve --addr 0.0.0.0:$((PORT + 5)) --data "$TMP/x" >"$TMP/guard.out" 2>&1 &
GUARD_PID=$!
for i in $(seq 1 15); do kill -0 "$GUARD_PID" 2>/dev/null || break; sleep 0.2; done
kill "$GUARD_PID" 2>/dev/null # Clean up if guard is still running from a regression
wait "$GUARD_PID" 2>/dev/null
GUARD_PID=""
OUT=$(head -1 "$TMP/guard.out" 2>/dev/null)
case "$OUT" in *"refusing to bind dev authentication"*) expect "external binding without auth is denied" ok ok ;; *) expect "external binding without auth is denied" "$OUT" "refusal message" ;; esac
expect "external binding guard touches no storage" "$([ ! -e "$TMP/x" ] && echo yes)" yes

CXT_AUTH=dev "$TMP/bin/cxtd" serve --addr 127.0.0.1:$PORT --data "$TMP/data" >"$TMP/srv.log" 2>&1 &
SRV_PID=$!
for i in $(seq 1 20); do [ "$(code "$B/repos")" = 200 ] && break; sleep 0.3; done
expect "Server startup (loopback+dev)" "$(code "$B/repos")" 200

login() { # login <email> <name> <jar> — if existing cookies are present, send them for re-login replacement path search
  ccurl -s -b "$3" -c "$3" -X POST "$B/auth/session" -H "Authorization: Bearer dev:$1:$2" >/dev/null
}
JA="$TMP/a.jar"; JV="$TMP/v.jar"; JP="$TMP/p.jar"; JM="$TMP/m.jar"; JT="$TMP/t.jar"
login owner@t.io Owner "$JA"

echo "── B. Sessions: token hashing, login replacement, and device list"
RAW=$(grep cxt_session "$JA" | awk '{print $NF}')
expect "No plain text session token on disk" "$(grep -r "$RAW" "$TMP/data" | wc -l | tr -d ' ')" 0
login owner@t.io Owner "$JA"; login owner@t.io Owner "$JA"
expect "Single session maintained on browser re-login" "$(ls "$TMP/data/sessions" | wc -l | tr -d ' ')" 1
expect "Current marked in web session list" "$(curl -sb "$JA" "$B/me/sessions" | jget "[0]['current']")" True

echo "── C. Repository name rules (ASCII identifier enforced)"
create_repository() { ccurl -s -b "$JA" -o /dev/null -w '%{http_code}' -X POST "$B/repositories" -H 'Content-Type: application/json' -d "{\"name\":\"$1\"}"; }
expect "non-ASCII name 422" "$(create_repository café)" 422
expect "number start 422" "$(create_repository 1abc)" 422
expect "special character end 422" "$(create_repository abc-)" 422
expect "valid name 200" "$(create_repository E2E_Main-1)" 200
REPOSITORY_ID=$(curl -sb "$JA" "$B/repositories" | jget "[0]['id']")
SLUG=$(curl -sb "$JA" "$B/repositories" | jget "[0]['slug']")
OWN=$(curl -sb "$JA" "$B/me" | jget "['username']")
expect "slug derived (lowercase and _ preserved)" "$SLUG" "e2e_main-1"

echo "── D. Invitations, membership, and five-role matrix"
inv() { ccurl -sb "$JA" -X POST "$B/repositories/$REPOSITORY_ID/invites" -H 'Content-Type: application/json' -d "{\"role\":\"$1\"}" | jget "['token']"; }
login vw@t.io Vw "$JV"; ccurl -sb "$JV" -X POST "$B/invites/$(inv viewer)/accept" >/dev/null
login pl@t.io Pl "$JP"; ccurl -sb "$JP" -X POST "$B/invites/$(inv puller)/accept" >/dev/null
login mb@t.io Mb "$JM"; ccurl -sb "$JM" -X POST "$B/invites/$(inv member)/accept" >/dev/null
login mt@t.io Mt "$JT"; ccurl -sb "$JT" -X POST "$B/invites/$(inv maintainer)/accept" >/dev/null
expect "5 members" "$(curl -sb "$JA" "$B/repositories/$REPOSITORY_ID/members" | python3 -c 'import json,sys;print(len(json.load(sys.stdin)))')" 5
expect "invite list: member 403" "$(code -b "$JM" "$B/repositories/$REPOSITORY_ID/invites")" 403
expect "invite list: maintainer 200" "$(code -b "$JT" "$B/repositories/$REPOSITORY_ID/invites")" 200

REMOTE="$ORIGIN/$OWN/$SLUG"
RID="$(repo_id "$REMOTE")"
ccurl -sb "$JA" -X POST "$B/repos" -H 'Content-Type: application/json' \
  -d "{\"id\":\"$RID\",\"remote_url\":\"$REMOTE\",\"default_branch\":\"main\"}" >/dev/null
RB="$B/repos/$RID"
NEG='{"snapshot_haves":[],"doc_haves":[]}'
# Envelopes require a fingerprint for server-side consistency. Repeated role-specific PUTs use the same dummy fingerprint.
SECPASS='team secret pass phrase'
SECFP="$(python3 - "$SECPASS" "$RID" <<'PY'
import hashlib, sys
key = hashlib.pbkdf2_hmac("sha256", sys.argv[1].encode(), ("cxtsecrets-fp:v1:" + sys.argv[2]).encode(), 600000, 32)
print(hashlib.sha256(key).hexdigest()[:12])
PY
)"
ENVL="{\"version\":1,\"kdf\":\"PBKDF2-SHA256\",\"iterations\":600000,\"salt_b64\":\"AAAAAAAAAAAAAAAAAAAAAA==\",\"cipher\":\"AES-256-GCM\",\"nonce_b64\":\"AAAAAAAAAAAAAAAA\",\"ciphertext_b64\":\"AAAAAAAAAAAAAAAAAAAAAA==\",\"fingerprint\":\"$SECFP\"}"
mrow() { # mrow <jar|-> → "read pull push secretsPUT"
  local revision
  revision=$(curl -sb "$JA" "$RB/secrets" | python3 -c 'import json,sys;s=sys.stdin.read();print(json.loads(s)["revision"] if s else "absent")')
  if [ "$1" = "-" ]; then
    echo "$(code "$RB/snapshots?branch=main") $(code "$RB/secrets") $(code -X POST "$RB/push/negotiate" -H 'Content-Type: application/json' -d "$NEG") $(code -X PUT "$RB/secrets?expected_revision=$revision" -H 'Content-Type: application/json' -d "$ENVL")"
  else
    echo "$(ccode -b "$1" "$RB/snapshots?branch=main") $(ccode -b "$1" "$RB/secrets") $(ccode -b "$1" -X POST "$RB/push/negotiate" -H 'Content-Type: application/json' -d "$NEG") $(ccode -b "$1" -X PUT "$RB/secrets?expected_revision=$revision" -H 'Content-Type: application/json' -d "$ENVL")"
  fi
}
expect "anonymous 401/401/401/401" "$(mrow -)" "401 401 401 401"
expect "viewer 200/403/403/403" "$(mrow "$JV")" "200 403 403 403"
expect "puller 200/204/403/403" "$(mrow "$JP")" "200 204 403 403"
expect "member 200/204/200/403" "$(mrow "$JM")" "200 204 200 403"
expect "maint. 200/204/200/200" "$(mrow "$JT")" "200 204 200 200"
expect "policy owner narrowed → maintainer 403" "$(ccurl -sb "$JA" -o /dev/null -w '' -X PATCH "$B/repositories/$REPOSITORY_ID" -H 'Content-Type: application/json' -d '{"secrets_policy":"owner"}')$(ccode -b "$JT" -X PUT "$RB/secrets" -H 'Content-Type: application/json' -d "$ENVL")" 403
ccurl -sb "$JA" -X PATCH "$B/repositories/$REPOSITORY_ID" -H 'Content-Type: application/json' -d '{"secrets_policy":"members"}' >/dev/null

echo "── E. Public access, archival, and protected branches"
ccurl -sb "$JA" -X PATCH "$B/repositories/$REPOSITORY_ID" -H 'Content-Type: application/json' -d '{"visibility":"public"}' >/dev/null
expect "public → anonymous read 200" "$(code "$RB/snapshots?branch=main")" 200
expect "public repository resolution 200" "$(code "$B/public/repositories/$OWN/$SLUG")" 200
# Public default role (non-member): Basic viewer is denied puller action (secrets pull), allowed when promoted to puller.
expect "public default viewer → anonymous secrets 401" "$(code "$RB/secrets")" 401
ccurl -sb "$JA" -X PATCH "$B/repositories/$REPOSITORY_ID" -H 'Content-Type: application/json' -d '{"public_role":"puller"}' >/dev/null
expect "public puller → anonymous secrets pull allowed (200, envelope exists)" "$(code "$RB/secrets")" 200
expect "public puller → anonymous read still 200" "$(code "$RB/snapshots?branch=main")" 200
expect "public puller even anonymous push requires login 401" "$(code -X POST "$RB/push/negotiate" -H 'Content-Type: application/json' -d "$NEG")" 401
expect "public_role invalid value 422" "$(ccode -b "$JA" -X PATCH "$B/repositories/$REPOSITORY_ID" -H 'Content-Type: application/json' -d '{"public_role":"owner"}')" 422
ccurl -sb "$JA" -X PATCH "$B/repositories/$REPOSITORY_ID" -H 'Content-Type: application/json' -d '{"public_role":"viewer"}' >/dev/null
expect "restoring public viewer → anonymous secrets 401" "$(code "$RB/secrets")" 401
ccurl -sb "$JA" -X PATCH "$B/repositories/$REPOSITORY_ID" -H 'Content-Type: application/json' -d '{"visibility":"private"}' >/dev/null
expect "private → anonymous public resolution 404" "$(code "$B/public/repositories/$OWN/$SLUG")" 404
ccurl -sb "$JA" -X PATCH "$B/repositories/$REPOSITORY_ID" -H 'Content-Type: application/json' -d '{"archived":true}' >/dev/null
expect "archived write 403" "$(ccode -b "$JA" -X POST "$RB/push/negotiate" -H 'Content-Type: application/json' -d "$NEG")" 403
expect "archived read 200" "$(ccode -b "$JA" "$RB/snapshots?branch=main")" 200
ccurl -sb "$JA" -X PATCH "$B/repositories/$REPOSITORY_ID" -H 'Content-Type: application/json' -d '{"archived":false}' >/dev/null
FIXTURE="$(fixture_objects "$RID")"
H1="$(printf '%s\n' "$FIXTURE" | sed -n '1p')"
OBJECTS="$(printf '%s\n' "$FIXTURE" | sed -n '2p')"
expect "canonical object pre-save" "$(ccode -b "$JA" -X POST "$RB/push/objects" -H 'Content-Type: application/json' -d "$OBJECTS")" 200
expect "ref creation (ff)" "$(ccode -b "$JA" -X PUT "$RB/refs/branch/main" -H 'Content-Type: application/json' -d "{\"target\":\"$H1\"}")" 200
# Search surface: normal query 200 (content is covered by TestSearch), fewer than two characters 422, unauthenticated 401.
expect "search endpoint 200" "$(code -b "$JA" "$RB/search?q=ssrf")" 200
expect "search less than 2 characters 422" "$(code -b "$JA" "$RB/search?q=a")" 422
expect "search unauthenticated 401" "$(code "$RB/search?q=ssrf")" 401
ccurl -sb "$JA" -X PATCH "$RB/about" -H 'Content-Type: application/json' -d '{"description":"","website":"","topics":[],"protect_default":true}' >/dev/null
FORCE_REF_PAYLOAD="{\"target\":\"$H1\",\"force\":true}"
expect "protected branch force 403" "$(ccode -b "$JA" -X PUT "$RB/refs/branch/main" -H 'Content-Type: application/json' -d "$FORCE_REF_PAYLOAD")" 403

echo "── F. CLI: init output, device-flow login, and secret round trip"
REPO="$TMP/repo"; mkdir -p "$REPO"; cd "$REPO"
git init -q
printf 'API_KEY=sk-e2e-secret-12345\nSHORT=ab\n' > .env
cxt init --no-hooks >/dev/null 2>&1
expect ".cxtsecrets extracts the .env value" "$(grep -c 'sk-e2e-secret-12345' .cxtsecrets)" 1
expect ".cxtsecrets exclude less than 4 characters" "$(grep -c '^ab$' .cxtsecrets)" 0
expect ".gitignore auto-registration" "$(grep -c -e '.cxt/' -e '.cxtsecrets' .gitignore)" 2
# Use a fresh repository for a decryptable CLI round trip; the role matrix above
# intentionally stores a format-valid opaque fixture which is not decryptable.
CLISLUG=$(ccurl -sb "$JA" -X POST "$B/repositories" -H 'Content-Type: application/json' -d '{"name":"SecretsCLI"}' | jget "['slug']")
# This suite exercises existing FS repository compatibility. New atomic
# repository initialization requires PostgreSQL and is covered by e2e-sync.
CLIREMOTE="$ORIGIN/$OWN/$CLISLUG"
CLIRID="$(repo_id "$CLIREMOTE")"
CLIREPO_PAYLOAD="{\"id\":\"$CLIRID\",\"remote_url\":\"$CLIREMOTE\",\"default_branch\":\"main\"}"
expect "existing FS secrets repository registration" "$(ccode -b "$JA" -X POST "$B/repos" -H 'Content-Type: application/json' -d "$CLIREPO_PAYLOAD")" 200
CXT_NO_BROWSER=1 cxt login --server "$ORIGIN" >"$TMP/login.out" 2>&1 &
LPID=$!
sleep 1.5
DCODE=$(grep -o '[B-Z2-9]\{3\}-[B-Z2-9]\{3\}' "$TMP/login.out" | head -1)
expect "device flow code output" "$([ -n "$DCODE" ] && echo yes)" yes
ccurl -sb "$JA" -X POST "$B/auth/device/approve" -H 'Content-Type: application/json' -d "{\"code\":\"$DCODE\"}" >/dev/null
wait "$LPID"
cxt remote add origin "$CLIREMOTE" >/dev/null 2>&1
expect "device login complete(auth.json)" "$(python3 -c "import json;print('127.0.0.1:$PORT' in json.load(open('$HOME/.cxt/auth.json')))")" True
# Device name label: device flow passes CLI hostname as a label → shows in token list.
expect "device token with hostname label" "$(curl -sb "$JA" "$B/me/cli-tokens" | python3 -c "import json,socket,sys;ts=json.load(sys.stdin) or [];print(any(t.get('label')==socket.gethostname() for t in ts))")" True
# Enforce team passphrase format (four or more words, at least 12 characters) on push; pull accepts legacy formats.
expect "initial push records editing baseline" "$(cxt secrets push -p "$SECPASS" >/dev/null 2>&1; echo $?)" 0
expect "server stores no plaintext after secret push" "$(grep -r 'sk-e2e-secret-12345' "$TMP/data" | wc -l | tr -d ' ')" 0
rm .cxtsecrets
expect "incorrect passphrase pull failure" "$(cxt secrets pull -p wrong >/dev/null 2>&1; echo $?)" 1
cxt secrets pull -p "$SECPASS" --remember >/dev/null 2>&1
expect "correct pull recovery" "$(grep -c 'sk-e2e-secret-12345' .cxtsecrets)" 1
rm .cxtsecrets
cxt secrets pull >/dev/null 2>&1
expect "--remember after -p omission" "$(grep -c 'sk-e2e-secret-12345' .cxtsecrets)" 1
expect "secrets round trip does not upgrade an existing repository" "$(curl -fsSb "$JA" "$B/repos/$CLIRID" | python3 -c 'import json,sys; r=json.load(sys.stdin); print(r["id"], r.get("context_protocol", 0))')" "$CLIRID 0"

echo "── F2. Session reports: the server accepts what the CLI computes, and only the uploader reads it"
# A synthetic Claude Code session whose command printed the number of a real
# commit while that commit was created. No hook runs: only records are read.
SEGREV_BEFORE="$(curl -fsSb "$JA" "$B/repos/$CLIRID/view" | jget "['revision']")"
GIT_AUTHOR_DATE=2031-01-02T03:00:05Z GIT_COMMITTER_DATE=2031-01-02T03:00:05Z git commit -q --allow-empty -m "e2e segment commit"
SEGSHA="$(git rev-parse HEAD)"
SEGBRANCH="$(git symbolic-ref --short HEAD)"
mkdir -p "$HOME/.claude/projects/-e2e-segments"
python3 - "$PWD" "$SEGSHA" "$SEGBRANCH" >"$HOME/.claude/projects/-e2e-segments/e2e-segments.jsonl" <<'PYSEG'
import json, sys
cwd, sha, branch = sys.argv[1:4]
def line(kind, uuid, at, content, prompt=None):
    entry = {"type": kind, "uuid": uuid, "sessionId": "e2e-segments", "timestamp": "2031-01-02T03:00:%sZ" % at,
             "cwd": cwd, "gitBranch": branch, "message": {"content": content}}
    if prompt:
        entry["promptId"] = prompt
    print(json.dumps(entry))
line("user", "m1", "00.000", "commit it", "p1")
line("assistant", "m2", "01.000", [{"type": "tool_use", "id": "call-1", "name": "Bash"}])
line("user", "m3", "06.000", [{"type": "tool_result", "tool_use_id": "call-1", "content": "[%s %s] e2e segment commit" % (branch, sha[:7])}], "p1")
line("assistant", "m4", "07.000", [{"type": "text", "text": "committed"}])
line("user", "m5", "40.000", "one more thing", "p2")
PYSEG
cxt segments push --json >"$TMP/segments-push.json" 2>"$TMP/segments-push.err"
expect "segments push succeeds" "$?" 0
expect "one session report stored" "$(python3 -c "import json;r=json.load(open('$TMP/segments-push.json'));print(r['stored'],r['unchanged'],r['refused'],len(r['sessions']))")" "1 0 0 1"
expect "uploader lists its report" "$(curl -sb "$JA" "$B/repos/$CLIRID/anchored-sessions" | python3 -c 'import json,sys;r=json.load(sys.stdin);print(len(r),r[0]["provider"],r[0]["session_id"],r[0]["segments"],r[0]["records"],r[0]["revision"])')" "1 claude e2e-segments 1 5 1"
expect "commit leads to the session's segment" "$(curl -sb "$JA" "$B/repos/$CLIRID/anchored-commits/$SEGSHA" | python3 -c 'import json,sys;r=json.load(sys.stdin);s=r[0]["segment"];print(len(r),r[0]["session_id"],r[0]["segment_no"],r[0]["rule"],s["first"],s["last"],s["closes_turn"],s["commits"][0]["subject"])')" "1 e2e-segments 1 printed-number m1 m4 True e2e segment commit"
expect "stored report names the worktree and never its path" "$(curl -sb "$JA" "$B/repos/$CLIRID/anchored-sessions/claude/e2e-segments" | python3 -c "import json,sys;r=json.load(sys.stdin);u=r['body']['uncommitted'];print(u['first'],u['places'][0]['worktree'],u['places'][0]['rule'],'$TMP' in json.dumps(r))")" "m5 repo recorded-by-tool False"
# The conversation of the session's own records is one repository document, sent
# through the path that verifies documents; every range says where it sits in it.
SEGDOC="$(curl -sb "$JA" "$B/repos/$CLIRID/anchored-sessions/claude/e2e-segments" | jget "['body']['content']['doc']")"
expect "report places its ranges in the content document" "$(curl -sb "$JA" "$B/repos/$CLIRID/anchored-sessions/claude/e2e-segments" | python3 -c 'import json,sys;b=json.load(sys.stdin)["body"];s=b["segments"][0];u=b["uncommitted"];print(b["content"]["events"],s.get("first_event",0),s["events"],u["first_event"],u["events"])')" "5 0 4 4 1"
expect "content document holds the conversation in order" "$(curl -sb "$JA" "$B/repos/$CLIRID/docs/$SEGDOC" | python3 -c 'import json,sys;d=json.load(sys.stdin);e=d["cir"]["events"];print(len(e),e[0]["kind"],e[1]["kind"],e[2]["kind"],e[4]["blocks"][0]["text"],d["cir"]["envelope"].get("cwd",""),d["cir"]["envelope"]["session_origin_id"])')" "5 message tool_call tool_result one more thing  e2e-segments"
expect "an account outside the repository cannot read the content document" "$(code -b "$JM" "$B/repos/$CLIRID/docs/$SEGDOC")" 403
expect "session content creates no snapshot" "$(curl -sb "$JA" "$B/repos/$CLIRID/snapshots" | python3 -c 'import json,sys;print(len(json.load(sys.stdin) or []))')" 0
expect "the same report again changes nothing" "$(cxt segments push --json 2>/dev/null | python3 -c 'import json,sys;r=json.load(sys.stdin);print(r["stored"],r["unchanged"],r["refused"],r["content_not_uploaded"],r["sessions"][0]["content"])')" "0 1 0 0 stored"
# Appended records extend the same document: the earlier events keep their place.
printf '%s\n' '{"type":"assistant","uuid":"m6","sessionId":"e2e-segments","timestamp":"2031-01-02T03:00:41.000Z","cwd":"'"$PWD"'","gitBranch":"'"$SEGBRANCH"'","message":{"content":[{"type":"text","text":"done"}]}}' >>"$HOME/.claude/projects/-e2e-segments/e2e-segments.jsonl"
expect "an appended record replaces the report" "$(cxt segments push --json 2>/dev/null | python3 -c 'import json,sys;r=json.load(sys.stdin);print(r["stored"],r["unchanged"],r["sessions"][0]["revision"])')" "1 0 2"
SEGDOC2="$(curl -sb "$JA" "$B/repos/$CLIRID/anchored-sessions/claude/e2e-segments" | jget "['body']['content']['doc']")"
expect "the grown document keeps the earlier events and adds the new one" "$(curl -sb "$JA" "$B/repos/$CLIRID/docs/$SEGDOC2" | python3 -c 'import json,sys;e=json.load(sys.stdin)["cir"]["events"];print(len(e),e[4]["blocks"][0]["text"],e[5]["blocks"][0]["text"])')" "6 one more thing done"
expect "revision after the appended record" "$(curl -sb "$JA" "$B/repos/$CLIRID/anchored-sessions/claude/e2e-segments" | jget "['revision']")" 2
expect "the uncommitted part says where each of its turns begins" "$(curl -sb "$JA" "$B/repos/$CLIRID/anchored-sessions/claude/e2e-segments" | python3 -c 'import json,sys;u=json.load(sys.stdin)["body"]["uncommitted"];t=u["turns"];print(len(t),t[0]["first_ordinal"]==u["first_ordinal"],t[0].get("first_event",0)==u.get("first_event",0),[m["first_ordinal"] for m in t]==sorted(m["first_ordinal"] for m in t))')" "1 True True True"
# Branch IDs live in Git's own branch configuration. The one of a branch Git
# shows on origin is registered, and a teammate's ID for the same name joins it.
SEGORIGIN="$TMP/segments-origin.git"; git init -q --bare "$SEGORIGIN"
git remote add origin "$SEGORIGIN"; git push -q -u origin "$SEGBRANCH"; git branch e2e-never-pushed
# The earlier uploads already gave the first branch its ID; the new branch gets one now.
expect "every branch has an ID and the pushed one is registered" "$(cxt segments push --json 2>/dev/null | python3 -c 'import json,sys;b=json.load(sys.stdin)["branches"];print(b["with_id"],b["assigned"],b["on_origin"],b["registered"])')" "2 1 1 1"
SEGBRANCHID="$(git config --local --get "branch.$SEGBRANCH.cxtBranchId")"
expect "the ID is kept by Git with the branch" "$(git config --local --get "branch.$SEGBRANCH.cxtBranchIdIssuedFor") ${#SEGBRANCHID}" "$SEGBRANCH 32"
expect "the name on the remote has one lifetime, opened with that ID" "$(curl -sb "$JA" "$B/repos/$CLIRID/branch-lifetimes?name=$SEGBRANCH" | python3 -c 'import json,sys;r=json.load(sys.stdin);print(len(r),r[0]["ledger_id"],r[0]["opened_by"]["kind"],"closed_at" in r[0])')" "1 $SEGBRANCHID registered False"
expect "a branch that was never pushed is not reported" "$(curl -sb "$JA" "$B/repos/$CLIRID/branch-lifetimes" | python3 -c 'import json,sys;print(sorted(l["name"] for l in json.load(sys.stdin)))')" "['$SEGBRANCH']"
expect "the same branches again change nothing" "$(cxt segments push --json 2>/dev/null | python3 -c 'import json,sys;b=json.load(sys.stdin)["branches"];print(b["with_id"],b["assigned"],b["registered"])') $(git config --local --get "branch.$SEGBRANCH.cxtBranchId")" "2 0 1 $SEGBRANCHID"
SEGOTHERID="$(python3 -c 'print("e"*32)')"
expect "another ID for the same name joins the lifetime" "$(ccurl -sb "$JA" -X PUT "$B/repos/$CLIRID/branch-ids" -H 'Content-Type: application/json' -d "{\"branches\":[{\"name\":\"$SEGBRANCH\",\"local_id\":\"$SEGOTHERID\"}]}" | python3 -c 'import json,sys;r=json.load(sys.stdin)["branches"][0];print(r["outcome"],r["ledger_id"])')" "joined $SEGBRANCHID"
expect "an account outside the repository cannot read the ledger" "$(code -b "$JM" "$B/repos/$CLIRID/branch-lifetimes")" 403
# The worktree list: names and branches, never a path, and only for the member who sent it.
SEGWORKTREE="$TMP/segments-extra"; git worktree add -q -b e2e-extra "$SEGWORKTREE"
expect "the run lists both worktrees" "$(cxt segments push --json 2>/dev/null | python3 -c 'import json,sys;w=json.load(sys.stdin)["worktrees"];print(w["listed"],w["reported"])')" "2 True"
expect "the server holds their names and branches" "$(curl -sb "$JA" "$B/repos/$CLIRID/worktrees" | python3 -c 'import json,sys;print(sorted((w["name"],w.get("branch",""),w["removed"]) for w in json.load(sys.stdin)))')" "[('$(basename "$PWD")', '$SEGBRANCH', False), ('segments-extra', 'e2e-extra', False)]"
expect "no path was sent" "$(curl -sb "$JA" "$B/repos/$CLIRID/worktrees" | grep -c "$TMP")" 0
git worktree remove --force "$SEGWORKTREE"; git branch -q -D e2e-extra
expect "a removed worktree keeps its name, marked removed" "$(cxt segments push --json >/dev/null 2>&1; curl -sb "$JA" "$B/repos/$CLIRID/worktrees" | python3 -c 'import json,sys;print(sorted((w["name"],w["removed"]) for w in json.load(sys.stdin)))')" "[('$(basename "$PWD")', False), ('segments-extra', True)]"
expect "an account outside the repository cannot read the list" "$(code -b "$JM" "$B/repos/$CLIRID/worktrees")" 403
# A manual note is masked like a captured session and readable only by its author.
SEGNOTE="$(ccurl -sb "$JA" -X POST "$B/repos/$CLIRID/anchored-notes" -H 'Content-Type: application/json' -d "{\"target\":{\"kind\":\"commit\",\"commit\":\"$SEGSHA\"},\"body\":\"hand-applied; clone https://user:synthetic-pass@example.test/x.git\"}")"
expect "the note is stored masked, with its author" "$(echo "$SEGNOTE" | python3 -c 'import json,sys;n=json.load(sys.stdin);print(n["target"]["kind"],"synthetic-pass" in n["body"],"redacted:password" in n["body"],bool(n["author"]))')" "commit False True True"
expect "the author lists the note" "$(curl -sb "$JA" "$B/repos/$CLIRID/anchored-notes" | python3 -c 'import json,sys;print(len(json.load(sys.stdin)))')" 1
expect "an account outside the repository cannot read notes" "$(code -b "$JM" "$B/repos/$CLIRID/anchored-notes")" 403
# A push is what opens a session to the team. This server reads no hosting
# provider, so nothing is known to be pushed: a teammate reads nothing yet.
JS="$TMP/s.jar"; login teammate@t.io Teammate "$JS"
CLIWS="$(curl -sb "$JA" "$B/repositories" | python3 -c "import json,sys;print([r['id'] for r in json.load(sys.stdin) if r['slug']=='$CLISLUG'][0])")"
ccurl -sb "$JS" -X POST "$B/invites/$(ccurl -sb "$JA" -X POST "$B/repositories/$CLIWS/invites" -H 'Content-Type: application/json' -d '{"role":"member"}' | jget "['token']")/accept" >/dev/null
expect "a teammate lists no session before a push" "$(curl -sb "$JS" "$B/repos/$CLIRID/anchored-sessions" | python3 -c 'import json,sys;print(len(json.load(sys.stdin)))')" 0
expect "a teammate cannot read a session before a push" "$(code -b "$JS" "$B/repos/$CLIRID/anchored-sessions/claude/e2e-segments")" 404
expect "a teammate cannot read its conversation before a push" "$(code -b "$JS" "$B/repos/$CLIRID/anchored-sessions/claude/e2e-segments/events?offset=0&limit=5")" 404
expect "a teammate does not see a note about a commit not known to be pushed" "$(curl -sb "$JS" "$B/repos/$CLIRID/anchored-notes" | python3 -c 'import json,sys;print(len(json.load(sys.stdin)))')" 0
# Whoever may view the repository reads what is open of it; only a member stores or writes.
JW="$TMP/w.jar"; login onlooker@t.io Onlooker "$JW"
ccurl -sb "$JW" -X POST "$B/invites/$(ccurl -sb "$JA" -X POST "$B/repositories/$CLIWS/invites" -H 'Content-Type: application/json' -d '{"role":"viewer"}' | jget "['token']")/accept" >/dev/null
expect "a viewer lists no session before a push" "$(curl -sb "$JW" "$B/repos/$CLIRID/anchored-sessions" | python3 -c 'import json,sys;print(len(json.load(sys.stdin)))')" 0
expect "a viewer reads the commit list the server holds" "$(code -b "$JW" "$B/repos/$CLIRID/remote-git")" 200
expect "a viewer cannot read a session before a push" "$(code -b "$JW" "$B/repos/$CLIRID/anchored-sessions/claude/e2e-segments")" 404
expect "a viewer cannot store a report" "$(ccurl -sb "$JW" -o /dev/null -w '%{http_code}' -X PUT "$B/repos/$CLIRID/anchored-sessions/claude/e2e-viewer" -H 'Content-Type: application/json' -d '{"rule_version":1,"scope":"working-folder","records":0}')" 403
expect "a viewer cannot write a note" "$(ccurl -sb "$JW" -o /dev/null -w '%{http_code}' -X POST "$B/repos/$CLIRID/anchored-notes" -H 'Content-Type: application/json' -d '{"target":{"kind":"branch","branch":"main"},"body":"x"}')" 403
expect "a visitor who is not signed in reads nothing of a private repository" "$(code "$B/repos/$CLIRID/anchored-sessions")" 401
expect "the uploader reads the conversation through the session, without the document hash" "$(curl -sb "$JA" "$B/repos/$CLIRID/anchored-sessions/claude/e2e-segments/events?offset=0&limit=20" | python3 -c "import json,sys;raw=sys.stdin.read();p=json.loads(raw);print(len(p['events']),p['offset'],p['next'],'$SEGDOC2' in raw)")" "6 0 -1 False"
expect "a session's summary says nothing about the remote it never read" "$(curl -sb "$JA" "$B/repos/$CLIRID/anchored-sessions" | python3 -c 'import json,sys;c=json.load(sys.stdin)[0]["commits"][0];print("on_remote" in c, c.get("copies"))')" "False None"
expect "the ledger makes no ref" "$(curl -sb "$JA" "$B/repos/$CLIRID/refs" | python3 -c 'import json,sys;print(len(json.load(sys.stdin) or []))')" 0
# The collector a hook wakes reads the same records and sends only what changed.
# Runs are spaced; the test moves the last run back instead of waiting.
SEGSTATE="$(git rev-parse --absolute-git-dir)/cxt/collector-v1.json"
segcollect() {
  [ -f "$SEGSTATE" ] && python3 - "$SEGSTATE" <<'PYWIND'
import json, sys
state = json.load(open(sys.argv[1]))
state["last_run"]["at"] = "2026-01-01T00:00:00Z"
json.dump(state, open(sys.argv[1], "w"))
PYWIND
  cxt git-hook segment-collect >/dev/null 2>&1
}
segappend() { printf '%s\n' '{"type":"user","uuid":"'"$1"'","sessionId":"e2e-segments","timestamp":"2031-01-02T03:01:'"$2"'.000Z","cwd":"'"$PWD"'","gitBranch":"'"$SEGBRANCH"'","promptId":"'"$1"'","message":{"content":"more"}}' >>"$HOME/.claude/projects/-e2e-segments/e2e-segments.jsonl"; }
segrevision() { curl -sb "$JA" "$B/repos/$CLIRID/anchored-sessions/claude/e2e-segments" | jget "['revision']"; }
segcollect
expect "the collector remembers what the server holds and how its run ended" "$(python3 -c "import json;s=json.load(open('$SEGSTATE'));r=s['last_run'];print(s['version'],len(s['sessions']),r['stored'],r['unchanged'],r.get('error','-'),bool(s['enabled_at']))")" "1 1 0 1 - True"
expect "a run with nothing new leaves the report as it is" "$(segcollect; segrevision)" 2
segappend m7 10
expect "the collector sends a session that went on" "$(segcollect; segrevision)" 3
cxt config segments.auto off >/dev/null 2>&1
segappend m8 20
expect "turned off, the collector sends nothing" "$(segcollect; segrevision)" 3
cxt config segments.auto on >/dev/null 2>&1
expect "turned on again, it makes up for what it missed" "$(segcollect; segrevision)" 4
expect "an account outside the repository cannot list reports" "$(code -b "$JM" "$B/repos/$CLIRID/anchored-sessions")" 403
expect "an account outside the repository cannot read a report" "$(code -b "$JM" "$B/repos/$CLIRID/anchored-sessions/claude/e2e-segments")" 403
expect "an anonymous caller cannot read a report" "$(code "$B/repos/$CLIRID/anchored-sessions/claude/e2e-segments")" 401
expect "reports do not advance the repository revision" "$(curl -fsSb "$JA" "$B/repos/$CLIRID/view" | jget "['revision']")" "$SEGREV_BEFORE"
cd "$ROOT"

echo "── G. Webhooks: default SSRF blocking versus explicitly allowed private targets"
# Keep the randomized port and hit file under $TMP to prevent concurrent-run and crash-residue contamination.
CXT_HITFILE="$HITFILE" CXT_STUB_PORT="$STUB_PORT" python3 - <<'PY' &
import os
from http.server import BaseHTTPRequestHandler, HTTPServer
hit=os.environ['CXT_HITFILE']; port=int(os.environ['CXT_STUB_PORT'])
class H(BaseHTTPRequestHandler):
    def do_POST(self):
        n=int(self.headers.get('Content-Length',0)); self.rfile.read(n)
        open(hit,'a').write(self.headers.get('X-CXTHub-Event-ID','missing')+'\n')
        self.send_response(503 if os.path.exists(hit+'.fail') else 200); self.end_headers()
    def log_message(self,*a): pass
HTTPServer(('127.0.0.1',port),H).serve_forever()
PY
STUB_PID=$!
for i in $(seq 1 20); do code "http://127.0.0.1:$STUB_PORT/" >/dev/null 2>&1 && break; sleep 0.2; done
hits() { cat "$HITFILE" 2>/dev/null | wc -l | tr -d ' '; }
ccurl -sb "$JA" -X PATCH "$B/repositories/$REPOSITORY_ID" -H 'Content-Type: application/json' -d "{\"webhook_url\":\"http://127.0.0.1:$STUB_PORT/h\"}" >/dev/null
ccurl -sb "$JA" -X PUT "$RB/refs/branch/wh-test" -H 'Content-Type: application/json' -d "{\"target\":\"$H1\"}" >/dev/null
# Wait until the durable worker has actually attempted delivery before asserting absence.
for i in $(seq 1 30); do
  reason=$(curl -sb "$JA" "$B/repositories/$REPOSITORY_ID/notifications" | python3 -c 'import json,sys;j=json.load(sys.stdin);print(j[0].get("reason", "") if j else "")')
  [ "$reason" = "transport_failed" ] && break
  sleep 0.2
done
expect "SSRF failure is durably visible" "$reason" transport_failed
expect "SSRF: default setting loopback webhook not fired" "$(hits)" 0
CXT_AUTH=dev CXT_ALLOW_PRIVATE_WEBHOOK=1 "$TMP/bin/cxtd" serve --addr 127.0.0.1:$PORT2 --data "$TMP/data2" >"$TMP/srv2.log" 2>&1 &
SRV2_PID=$!
B2="http://127.0.0.1:$PORT2/api/v1"
ORIGIN2="http://127.0.0.1:$PORT2"
ccurl2() { command curl -H "Origin: $ORIGIN2" -H 'X-Cxt-CSRF: 1' "$@"; }
ccode2() { code -H "Origin: $ORIGIN2" -H 'X-Cxt-CSRF: 1' "$@"; }
for i in $(seq 1 20); do [ "$(code "$B2/repos")" = 200 ] && break; sleep 0.3; done
J2="$TMP/w2.jar"; ccurl2 -s -c "$J2" -X POST "$B2/auth/session" -H "Authorization: Bearer dev:wh@t.io:Wh" >/dev/null
REPOSITORY_ID2=$(ccurl2 -sb "$J2" -X POST "$B2/repositories" -H 'Content-Type: application/json' -d '{"name":"WhTest"}' | jget "['id']")
OWN2=$(curl -sb "$J2" "$B2/me" | jget "['username']")
SLUG2=$(curl -sb "$J2" "$B2/repositories" | jget "[0]['slug']")
REMOTE2="$ORIGIN2/$OWN2/$SLUG2"
RID2="$(repo_id "$REMOTE2")"
ccurl2 -sb "$J2" -X POST "$B2/repos" -H 'Content-Type: application/json' -d "{\"id\":\"$RID2\",\"remote_url\":\"$REMOTE2\",\"default_branch\":\"main\"}" >/dev/null
ccurl2 -sb "$J2" -X POST "$B2/repos/$RID2/push/objects" -H 'Content-Type: application/json' -d "$OBJECTS" >/dev/null
ccurl2 -sb "$J2" -X PATCH "$B2/repositories/$REPOSITORY_ID2" -H 'Content-Type: application/json' -d "{\"webhook_url\":\"http://127.0.0.1:$STUB_PORT/h\"}" >/dev/null
ccurl2 -sb "$J2" -X PUT "$B2/repos/$RID2/refs/branch/main" -H 'Content-Type: application/json' -d "{\"target\":\"$H1\"}" >/dev/null
# Transmission is asynchronous — uses polling instead of fixed sleep (stabilizes in load-heavy CI).
for i in $(seq 1 25); do [ "$(hits)" -ge 1 ] && break; sleep 0.2; done
expect "webhook delivered when private targets are explicitly allowed" "$(hits)" 1
# Webhook event extension: triggers on secret update and member join in addition to ref update.
ccurl2 -sb "$J2" -X PUT "$B2/repos/$RID2/secrets?expected_revision=absent" -H 'Content-Type: application/json' -d "$ENVL" >/dev/null
for i in $(seq 1 25); do [ "$(hits)" -ge 2 ] && break; sleep 0.2; done
expect "Secret update webhook delivery" "$(hits)" 2
INV2=$(ccurl2 -sb "$J2" -X POST "$B2/repositories/$REPOSITORY_ID2/invites" -H 'Content-Type: application/json' -d '{"role":"member"}' | jget "['token']")
J3="$TMP/w3.jar"; ccurl2 -s -c "$J3" -X POST "$B2/auth/session" -H "Authorization: Bearer dev:joiner@t.io:Joiner" >/dev/null
ccurl2 -sb "$J3" -X POST "$B2/invites/$INV2/accept" >/dev/null
for i in $(seq 1 25); do [ "$(hits)" -ge 3 ] && break; sleep 0.2; done
expect "Member join webhook delivery" "$(hits)" 3
# Invite links are reusable (idempotent accept) — re-clicking by existing members does not create duplicate notifications.
ccurl2 -sb "$J3" -X POST "$B2/invites/$INV2/accept" >/dev/null
sleep 1
expect "idempotent reaccept emits no duplicate webhook" "$(hits)" 3

echo "── G2. Durable notification retry survives a real server restart"
touch "$HITFILE.fail"
REV2=$(ccurl2 -sb "$J2" "$B2/repos/$RID2/secrets" | jget "['revision']")
ccurl2 -sb "$J2" -X PUT "$B2/repos/$RID2/secrets?expected_revision=$REV2" -H 'Content-Type: application/json' -d "$ENVL" >/dev/null
for i in $(seq 1 30); do
  state=$(ccurl2 -sb "$J2" "$B2/repositories/$REPOSITORY_ID2/notifications" | jget "[0]['state']")
  [ "$state" = "retrying" ] && break
  sleep 0.2
done
expect "failed HTTP delivery persists retry state" "$state" retrying
kill "$SRV2_PID"; wait "$SRV2_PID" 2>/dev/null; SRV2_PID=""
rm "$HITFILE.fail"
CXT_AUTH=dev CXT_ALLOW_PRIVATE_WEBHOOK=1 "$TMP/bin/cxtd" serve --addr 127.0.0.1:$PORT2 --data "$TMP/data2" >"$TMP/srv2-restart.log" 2>&1 &
SRV2_PID=$!
for i in $(seq 1 20); do [ "$(code "$B2/repos")" = 200 ] && break; sleep 0.3; done
# Preserve the real 30-second backoff; no test-only scheduler override.
for i in $(seq 1 150); do
  state=$(ccurl2 -sb "$J2" "$B2/repositories/$REPOSITORY_ID2/notifications" | jget "[0]['state']")
  [ "$state" = "delivered" ] && break
  sleep 0.3
done
expect "restart automatically resumes queued delivery" "$state" delivered
expect "receiver sees one failed and one successful attempt" "$(hits)" 5
expect "retry preserves stable event ID" "$(tail -2 "$HITFILE" | uniq | wc -l | tr -d ' ')" 1

echo "── H. Security surface"
expect "CORS: arbitrary origin is not reflected" "$(curl -s -H 'Origin: https://evil.com' -o /dev/null -w '%{header_json}' "$B/repos" | python3 -c "import json,sys;print(json.load(sys.stdin).get('access-control-allow-origin',['none'])[0])")" none
expect "CORS: localhost origin is reflected" "$(curl -s -H 'Origin: http://localhost:5173' -o /dev/null -w '%{header_json}' "$B/repos" | python3 -c "import json,sys;print(json.load(sys.stdin).get('access-control-allow-origin',['none'])[0])")" "http://localhost:5173"
expect "Content-Type forced 415" "$(ccode -b "$JA" -X POST "$B/repositories" -H 'Content-Type: text/plain' -d '{"name":"Csrf"}')" 415
expect "Reserved username 409" "$(ccode -b "$JA" -X PATCH "$B/me" -H 'Content-Type: application/json' -d '{"username":"api"}')" 409
DEVICE_POLL_START=$(curl -s -X POST "$B/auth/device/start" -H 'Content-Type: application/json' -d '{}')
DEVICE_POLL_CODE=$(printf '%s\n' "$DEVICE_POLL_START" | jget "['code']")
DEVICE_POLL_PAYLOAD="{\"code\":\"$DEVICE_POLL_CODE\",\"poll_token\":\"dpoll_x\"}"
expect "device: wrong poll_token 404" "$(code -X POST "$B/auth/device/poll" -H 'Content-Type: application/json' -d "$DEVICE_POLL_PAYLOAD")" 404
R429=$(for i in $(seq 1 25); do code -X POST "$B/auth/session" -H "Authorization: Bearer dev:rl@t.io:R"; echo; done | grep -c 429)
expect "login rate limit returns at least one 429 in 25 attempts" "$([ "$R429" -ge 1 ] && echo yes)" yes

echo
if [ "$FAIL" = 0 ]; then echo "E2E: all passed ✓"; else echo "E2E: failures exist ✗"; fi
exit "$FAIL"
