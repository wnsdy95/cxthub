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

echo "── C. Workspace name rules (ASCII identifier enforced)"
wsmk() { ccurl -s -b "$JA" -o /dev/null -w '%{http_code}' -X POST "$B/workspaces" -H 'Content-Type: application/json' -d "{\"name\":\"$1\"}"; }
expect "non-ASCII name 422" "$(wsmk café)" 422
expect "number start 422" "$(wsmk 1abc)" 422
expect "special character end 422" "$(wsmk abc-)" 422
expect "valid name 200" "$(wsmk E2E_Main-1)" 200
WS=$(curl -sb "$JA" "$B/workspaces" | jget "[0]['id']")
SLUG=$(curl -sb "$JA" "$B/workspaces" | jget "[0]['slug']")
OWN=$(curl -sb "$JA" "$B/me" | jget "['username']")
expect "slug derived (lowercase and _ preserved)" "$SLUG" "e2e_main-1"

echo "── D. Invitations, membership, and five-role matrix"
inv() { ccurl -sb "$JA" -X POST "$B/workspaces/$WS/invites" -H 'Content-Type: application/json' -d "{\"role\":\"$1\"}" | jget "['token']"; }
login vw@t.io Vw "$JV"; ccurl -sb "$JV" -X POST "$B/invites/$(inv viewer)/accept" >/dev/null
login pl@t.io Pl "$JP"; ccurl -sb "$JP" -X POST "$B/invites/$(inv puller)/accept" >/dev/null
login mb@t.io Mb "$JM"; ccurl -sb "$JM" -X POST "$B/invites/$(inv member)/accept" >/dev/null
login mt@t.io Mt "$JT"; ccurl -sb "$JT" -X POST "$B/invites/$(inv maintainer)/accept" >/dev/null
expect "5 members" "$(curl -sb "$JA" "$B/workspaces/$WS/members" | python3 -c 'import json,sys;print(len(json.load(sys.stdin)))')" 5
expect "invite list: member 403" "$(code -b "$JM" "$B/workspaces/$WS/invites")" 403
expect "invite list: maintainer 200" "$(code -b "$JT" "$B/workspaces/$WS/invites")" 200

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
  if [ "$1" = "-" ]; then
    echo "$(code "$RB/snapshots?branch=main") $(code "$RB/secrets") $(code -X POST "$RB/push/negotiate" -H 'Content-Type: application/json' -d "$NEG") $(code -X PUT "$RB/secrets" -H 'Content-Type: application/json' -d "$ENVL")"
  else
    echo "$(ccode -b "$1" "$RB/snapshots?branch=main") $(ccode -b "$1" "$RB/secrets") $(ccode -b "$1" -X POST "$RB/push/negotiate" -H 'Content-Type: application/json' -d "$NEG") $(ccode -b "$1" -X PUT "$RB/secrets" -H 'Content-Type: application/json' -d "$ENVL")"
  fi
}
expect "anonymous 401/401/401/401" "$(mrow -)" "401 401 401 401"
expect "viewer 200/403/403/403" "$(mrow "$JV")" "200 403 403 403"
expect "puller 200/204/403/403" "$(mrow "$JP")" "200 204 403 403"
expect "member 200/204/200/403" "$(mrow "$JM")" "200 204 200 403"
expect "maint. 200/204/200/200" "$(mrow "$JT")" "200 204 200 200"
expect "policy owner narrowed → maintainer 403" "$(ccurl -sb "$JA" -o /dev/null -w '' -X PATCH "$B/workspaces/$WS" -H 'Content-Type: application/json' -d '{"secrets_policy":"owner"}')$(ccode -b "$JT" -X PUT "$RB/secrets" -H 'Content-Type: application/json' -d "$ENVL")" 403
ccurl -sb "$JA" -X PATCH "$B/workspaces/$WS" -H 'Content-Type: application/json' -d '{"secrets_policy":"members"}' >/dev/null

echo "── E. Public access, archival, and protected branches"
ccurl -sb "$JA" -X PATCH "$B/workspaces/$WS" -H 'Content-Type: application/json' -d '{"visibility":"public"}' >/dev/null
expect "public → anonymous read 200" "$(code "$RB/snapshots?branch=main")" 200
expect "public workspace resolution 200" "$(code "$B/public/workspaces/$OWN/$SLUG")" 200
# Public default role (non-member): Basic viewer is denied puller action (secrets pull), allowed when promoted to puller.
expect "public default viewer → anonymous secrets 401" "$(code "$RB/secrets")" 401
ccurl -sb "$JA" -X PATCH "$B/workspaces/$WS" -H 'Content-Type: application/json' -d '{"public_role":"puller"}' >/dev/null
expect "public puller → anonymous secrets pull allowed (200, envelope exists)" "$(code "$RB/secrets")" 200
expect "public puller → anonymous read still 200" "$(code "$RB/snapshots?branch=main")" 200
expect "public puller even anonymous push requires login 401" "$(code -X POST "$RB/push/negotiate" -H 'Content-Type: application/json' -d "$NEG")" 401
expect "public_role invalid value 422" "$(ccode -b "$JA" -X PATCH "$B/workspaces/$WS" -H 'Content-Type: application/json' -d '{"public_role":"owner"}')" 422
ccurl -sb "$JA" -X PATCH "$B/workspaces/$WS" -H 'Content-Type: application/json' -d '{"public_role":"viewer"}' >/dev/null
expect "restoring public viewer → anonymous secrets 401" "$(code "$RB/secrets")" 401
ccurl -sb "$JA" -X PATCH "$B/workspaces/$WS" -H 'Content-Type: application/json' -d '{"visibility":"private"}' >/dev/null
expect "private → anonymous public resolution 404" "$(code "$B/public/workspaces/$OWN/$SLUG")" 404
ccurl -sb "$JA" -X PATCH "$B/workspaces/$WS" -H 'Content-Type: application/json' -d '{"archived":true}' >/dev/null
expect "archived write 403" "$(ccode -b "$JA" -X POST "$RB/push/negotiate" -H 'Content-Type: application/json' -d "$NEG")" 403
expect "archived read 200" "$(ccode -b "$JA" "$RB/snapshots?branch=main")" 200
ccurl -sb "$JA" -X PATCH "$B/workspaces/$WS" -H 'Content-Type: application/json' -d '{"archived":false}' >/dev/null
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
expect "protected branch force 403" "$(ccode -b "$JA" -X PUT "$RB/refs/branch/main" -H 'Content-Type: application/json' -d "{\"target\":\"$H1\",\"force\":true}")" 403

echo "── F. CLI: init output, device-flow login, and secret round trip"
REPO="$TMP/repo"; mkdir -p "$REPO"; cd "$REPO"
git init -q
printf 'API_KEY=sk-e2e-secret-12345\nSHORT=ab\n' > .env
cxt init --no-hooks >/dev/null 2>&1
expect ".cxtsecrets extracts the .env value" "$(grep -c 'sk-e2e-secret-12345' .cxtsecrets)" 1
expect ".cxtsecrets exclude less than 4 characters" "$(grep -c '^ab$' .cxtsecrets)" 0
expect ".gitignore auto-registration" "$(grep -c -e '.cxt/' -e '.cxtsecrets' .gitignore)" 2
cxt remote add origin "http://127.0.0.1:$PORT/$OWN/$SLUG" >/dev/null 2>&1
CXT_NO_BROWSER=1 cxt login >"$TMP/login.out" 2>&1 &
LPID=$!
sleep 1.5
DCODE=$(grep -o '[B-Z2-9]\{3\}-[B-Z2-9]\{3\}' "$TMP/login.out" | head -1)
expect "device flow code output" "$([ -n "$DCODE" ] && echo yes)" yes
ccurl -sb "$JA" -X POST "$B/auth/device/approve" -H 'Content-Type: application/json' -d "{\"code\":\"$DCODE\"}" >/dev/null
wait "$LPID"
expect "device login complete(auth.json)" "$(python3 -c "import json;print('127.0.0.1:$PORT' in json.load(open('$HOME/.cxt/auth.json')))")" True
# Device name label: device flow passes CLI hostname as a label → shows in token list.
expect "device token with hostname label" "$(curl -sb "$JA" "$B/me/cli-tokens" | python3 -c "import json,socket,sys;ts=json.load(sys.stdin) or [];print(any(t.get('label')==socket.gethostname() for t in ts))")" True
# Enforce team passphrase format (four or more words, at least 12 characters) on push; pull accepts legacy formats.
cxt secrets push -p "$SECPASS" >/dev/null 2>&1
expect "server stores no plaintext after secret push" "$(grep -r 'sk-e2e-secret-12345' "$TMP/data" | wc -l | tr -d ' ')" 0
rm .cxtsecrets
expect "incorrect passphrase pull failure" "$(cxt secrets pull -p wrong >/dev/null 2>&1; echo $?)" 1
cxt secrets pull -p "$SECPASS" --remember >/dev/null 2>&1
expect "correct pull recovery" "$(grep -c 'sk-e2e-secret-12345' .cxtsecrets)" 1
rm .cxtsecrets
cxt secrets pull >/dev/null 2>&1
expect "--remember after -p omission" "$(grep -c 'sk-e2e-secret-12345' .cxtsecrets)" 1
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
        open(hit,'a').write('HIT\n')
        self.send_response(200); self.end_headers()
    def log_message(self,*a): pass
HTTPServer(('127.0.0.1',port),H).serve_forever()
PY
STUB_PID=$!
for i in $(seq 1 20); do code "http://127.0.0.1:$STUB_PORT/" >/dev/null 2>&1 && break; sleep 0.2; done
hits() { cat "$HITFILE" 2>/dev/null | wc -l | tr -d ' '; }
ccurl -sb "$JA" -X PATCH "$B/workspaces/$WS" -H 'Content-Type: application/json' -d "{\"webhook_url\":\"http://127.0.0.1:$STUB_PORT/h\"}" >/dev/null
ccurl -sb "$JA" -X PUT "$RB/refs/branch/wh-test" -H 'Content-Type: application/json' -d "{\"target\":\"$H1\"}" >/dev/null
# SSRF blocking is an absence assertion, so wait conservatively and confirm zero deliveries.
sleep 1
expect "SSRF: default setting loopback webhook not fired" "$(hits)" 0
CXT_AUTH=dev CXT_ALLOW_PRIVATE_WEBHOOK=1 "$TMP/bin/cxtd" serve --addr 127.0.0.1:$PORT2 --data "$TMP/data2" >"$TMP/srv2.log" 2>&1 &
SRV2_PID=$!
B2="http://127.0.0.1:$PORT2/api/v1"
ORIGIN2="http://127.0.0.1:$PORT2"
ccurl2() { command curl -H "Origin: $ORIGIN2" -H 'X-Cxt-CSRF: 1' "$@"; }
ccode2() { code -H "Origin: $ORIGIN2" -H 'X-Cxt-CSRF: 1' "$@"; }
for i in $(seq 1 20); do [ "$(code "$B2/repos")" = 200 ] && break; sleep 0.3; done
J2="$TMP/w2.jar"; ccurl2 -s -c "$J2" -X POST "$B2/auth/session" -H "Authorization: Bearer dev:wh@t.io:Wh" >/dev/null
WS2=$(ccurl2 -sb "$J2" -X POST "$B2/workspaces" -H 'Content-Type: application/json' -d '{"name":"WhTest"}' | jget "['id']")
OWN2=$(curl -sb "$J2" "$B2/me" | jget "['username']")
SLUG2=$(curl -sb "$J2" "$B2/workspaces" | jget "[0]['slug']")
REMOTE2="$ORIGIN2/$OWN2/$SLUG2"
RID2="$(repo_id "$REMOTE2")"
ccurl2 -sb "$J2" -X POST "$B2/repos" -H 'Content-Type: application/json' -d "{\"id\":\"$RID2\",\"remote_url\":\"$REMOTE2\",\"default_branch\":\"main\"}" >/dev/null
ccurl2 -sb "$J2" -X POST "$B2/repos/$RID2/push/objects" -H 'Content-Type: application/json' -d "$OBJECTS" >/dev/null
ccurl2 -sb "$J2" -X PATCH "$B2/workspaces/$WS2" -H 'Content-Type: application/json' -d "{\"webhook_url\":\"http://127.0.0.1:$STUB_PORT/h\"}" >/dev/null
ccurl2 -sb "$J2" -X PUT "$B2/repos/$RID2/refs/branch/main" -H 'Content-Type: application/json' -d "{\"target\":\"$H1\"}" >/dev/null
# Transmission is asynchronous — uses polling instead of fixed sleep (stabilizes in load-heavy CI).
for i in $(seq 1 25); do [ "$(hits)" -ge 1 ] && break; sleep 0.2; done
expect "webhook delivered when private targets are explicitly allowed" "$(hits)" 1
# Webhook event extension: triggers on secret update and member join in addition to ref update.
ccurl2 -sb "$J2" -X PUT "$B2/repos/$RID2/secrets" -H 'Content-Type: application/json' -d "$ENVL" >/dev/null
for i in $(seq 1 25); do [ "$(hits)" -ge 2 ] && break; sleep 0.2; done
expect "Secret update webhook delivery" "$(hits)" 2
INV2=$(ccurl2 -sb "$J2" -X POST "$B2/workspaces/$WS2/invites" -H 'Content-Type: application/json' -d '{"role":"member"}' | jget "['token']")
J3="$TMP/w3.jar"; ccurl2 -s -c "$J3" -X POST "$B2/auth/session" -H "Authorization: Bearer dev:joiner@t.io:Joiner" >/dev/null
ccurl2 -sb "$J3" -X POST "$B2/invites/$INV2/accept" >/dev/null
for i in $(seq 1 25); do [ "$(hits)" -ge 3 ] && break; sleep 0.2; done
expect "Member join webhook delivery" "$(hits)" 3
# Invite links are reusable (idempotent accept) — re-clicking by existing members does not create duplicate notifications.
ccurl2 -sb "$J3" -X POST "$B2/invites/$INV2/accept" >/dev/null
sleep 1
expect "idempotent reaccept emits no duplicate webhook" "$(hits)" 3

echo "── H. Security surface"
expect "CORS: arbitrary origin is not reflected" "$(curl -s -H 'Origin: https://evil.com' -o /dev/null -w '%{header_json}' "$B/repos" | python3 -c "import json,sys;print(json.load(sys.stdin).get('access-control-allow-origin',['none'])[0])")" none
expect "CORS: localhost origin is reflected" "$(curl -s -H 'Origin: http://localhost:5173' -o /dev/null -w '%{header_json}' "$B/repos" | python3 -c "import json,sys;print(json.load(sys.stdin).get('access-control-allow-origin',['none'])[0])")" "http://localhost:5173"
expect "Content-Type forced 415" "$(ccode -b "$JA" -X POST "$B/workspaces" -H 'Content-Type: text/plain' -d '{"name":"Csrf"}')" 415
expect "Reserved username 409" "$(ccode -b "$JA" -X PATCH "$B/me" -H 'Content-Type: application/json' -d '{"username":"api"}')" 409
expect "device: wrong poll_token 404" "$(S=$(curl -s -X POST "$B/auth/device/start" -H 'Content-Type: application/json' -d '{}'); C=$(echo "$S"|jget "['code']"); code -X POST "$B/auth/device/poll" -H 'Content-Type: application/json' -d "{\"code\":\"$C\",\"poll_token\":\"dpoll_x\"}")" 404
R429=$(for i in $(seq 1 25); do code -X POST "$B/auth/session" -H "Authorization: Bearer dev:rl@t.io:R"; echo; done | grep -c 429)
expect "login rate limit returns at least one 429 in 25 attempts" "$([ "$R429" -ge 1 ] && echo yes)" yes

echo
if [ "$FAIL" = 0 ]; then echo "E2E: all passed ✓"; else echo "E2E: failures exist ✗"; fi
exit "$FAIL"
