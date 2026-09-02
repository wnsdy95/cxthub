#!/usr/bin/env bash
# cxtd dogfood daemon — runs continuously as a macOS launchd LaunchAgent.
# Starts automatically at login (RunAtLoad) and restarts after a crash (KeepAlive).
#
#   scripts/dogfood-daemon.sh install    # create and load the plist
#   scripts/dogfood-daemon.sh uninstall  # unload and remove the plist
#   scripts/dogfood-daemon.sh restart    # rewrite config and restart with the installed binary
#   scripts/dogfood-daemon.sh status     # show status and recent logs
#   scripts/dogfood-daemon.sh logs       # tail logs
#   scripts/dogfood-daemon.sh config     # print resolved auth without changing anything
#
# When CXT_AUTH is not explicit, the daemon follows the effective
# VITE_FIREBASE_PROJECT_ID from frontend/web's development env files. A fresh
# checkout with no Firebase config still defaults to local dev authentication.
set -euo pipefail

LABEL="com.cxthub.cxtd"
PLIST="$HOME/Library/LaunchAgents/$LABEL.plist"
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
BIN="$(command -v cxtd || echo "$HOME/go/bin/cxtd")"
ADDR="${CXT_ADDR:-127.0.0.1:8907}"
DATA="$ROOT/cxt-data"
LOGDIR="$ROOT/.cxt-daemon"
GUI="gui/$(id -u)"

vite_env_value() {
  local key="$1"
  local file line value found=""
  if [[ -n "${!key+x}" ]]; then
    printf '%s' "${!key}"
    return
  fi
  for file in \
    "$ROOT/frontend/web/.env" \
    "$ROOT/frontend/web/.env.local" \
    "$ROOT/frontend/web/.env.development" \
    "$ROOT/frontend/web/.env.development.local"; do
    [[ -f "$file" ]] || continue
    while IFS= read -r line || [[ -n "$line" ]]; do
      line="${line%$'\r'}"
      if [[ "$line" =~ ^[[:space:]]*(export[[:space:]]+)?${key}[[:space:]]*=[[:space:]]*(.*)$ ]]; then
        value="${BASH_REMATCH[2]}"
        value="${value%%#*}"
        value="${value#"${value%%[![:space:]]*}"}"
        value="${value%"${value##*[![:space:]]}"}"
        if [[ ${#value} -ge 2 ]] && {
          [[ "$value" == \"*\" ]] || [[ "$value" == \'*\' ]];
        }; then
          value="${value:1:${#value}-2}"
        fi
        found="$value"
      fi
    done < "$file"
  done
  printf '%s' "$found"
}

WEB_FIREBASE_API_KEY="$(vite_env_value VITE_FIREBASE_API_KEY)"
WEB_FIREBASE_PROJECT="$(vite_env_value VITE_FIREBASE_PROJECT_ID)"
if [[ -n "${CXT_AUTH+x}" ]]; then
  AUTH="$CXT_AUTH"
  AUTH_SOURCE="CXT_AUTH"
elif [[ -n "${CXT_FIREBASE_PROJECT:-}" ]]; then
  AUTH="firebase"
  AUTH_SOURCE="CXT_FIREBASE_PROJECT"
elif [[ -n "$WEB_FIREBASE_API_KEY" ]]; then
  AUTH="firebase"
  AUTH_SOURCE="Vite development env"
else
  AUTH="dev"
  AUTH_SOURCE="default"
fi

if [[ "$AUTH" == "firebase" ]]; then
  FIREBASE="${CXT_FIREBASE_PROJECT:-$WEB_FIREBASE_PROJECT}"
else
  FIREBASE=""
fi

validate_auth() {
  case "$AUTH" in
    dev) ;;
    firebase)
      if [[ ! "$FIREBASE" =~ ^[a-z][a-z0-9-]{4,28}[a-z0-9]$ ]]; then
        echo "CXT_FIREBASE_PROJECT must be a valid Firebase project ID when CXT_AUTH=firebase." >&2
        return 2
      fi
      ;;
    *)
      echo "CXT_AUTH must be dev or firebase." >&2
      return 2
      ;;
  esac
}

write_plist() {
  mkdir -p "$LOGDIR" "$(dirname "$PLIST")"
  cat > "$PLIST" <<PLIST
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key><string>$LABEL</string>
  <key>ProgramArguments</key>
  <array>
    <string>$BIN</string>
    <string>serve</string>
    <string>--addr</string><string>$ADDR</string>
    <string>--data</string><string>$DATA</string>
  </array>
  <key>EnvironmentVariables</key>
  <dict>
    <key>CXT_AUTH</key><string>$AUTH</string>
    <key>CXT_FIREBASE_PROJECT</key><string>$FIREBASE</string>
  </dict>
  <key>WorkingDirectory</key><string>$ROOT</string>
  <key>RunAtLoad</key><true/>
  <key>KeepAlive</key><true/>
  <key>StandardOutPath</key><string>$LOGDIR/cxtd.out.log</string>
  <key>StandardErrorPath</key><string>$LOGDIR/cxtd.err.log</string>
  <key>ProcessType</key><string>Background</string>
</dict>
</plist>
PLIST
}

reload_plist() {
  launchctl bootout "$GUI" "$PLIST" 2>/dev/null || true
  launchctl bootstrap "$GUI" "$PLIST"
  launchctl enable "$GUI/$LABEL"
}

auth_label() {
  if [[ "$AUTH" == "firebase" ]]; then
    printf 'firebase:%s' "$FIREBASE"
  else
    printf 'dev'
  fi
}

case "${1:-}" in
  install)
    validate_auth
    # Stop an existing shell-background cxtd process if it owns the port.
    pkill -x cxtd 2>/dev/null || true
    sleep 1
    write_plist
    reload_plist
    echo "✓ installed and started: $LABEL"
    echo "  binary: $BIN"
    echo "  address: http://$ADDR  data: $DATA"
    echo "  auth: $(auth_label) ($AUTH_SOURCE)"
    echo "  logs: $LOGDIR/cxtd.{out,err}.log"
    ;;
  uninstall)
    launchctl bootout "$GUI" "$PLIST" 2>/dev/null || true
    rm -f "$PLIST"
    echo "✓ unloaded and removed: $LABEL"
    ;;
  restart)
    validate_auth
    write_plist
    reload_plist
    echo "✓ rewrote config and restarted: $LABEL"
    echo "  auth: $(auth_label) ($AUTH_SOURCE)"
    ;;
  status)
    if launchctl print "$GUI/$LABEL" >/dev/null 2>&1; then
      launchctl print "$GUI/$LABEL" | grep -E "state|pid|program =|last exit" | sed 's/^[[:space:]]*/  /'
    else
      echo "  not installed — scripts/dogfood-daemon.sh install"
    fi
    echo "── health check:"
    curl -s -o /dev/null -w "  http://$ADDR/api/v1/health → %{http_code}\n" "http://$ADDR/api/v1/health" || echo "  no response"
    ;;
  logs)
    tail -n 40 -F "$LOGDIR/cxtd.err.log" "$LOGDIR/cxtd.out.log"
    ;;
  config)
    validate_auth
    echo "auth=$AUTH"
    echo "firebase_project=$FIREBASE"
    echo "source=$AUTH_SOURCE"
    ;;
  *)
    echo "Usage: $0 {install|uninstall|restart|status|logs|config}"; exit 1 ;;
esac
