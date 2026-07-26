#!/usr/bin/env bash
# cxtd dogfood daemon — runs continuously as a macOS launchd LaunchAgent.
# Starts automatically at login (RunAtLoad) and restarts after a crash (KeepAlive).
#
#   scripts/dogfood-daemon.sh install    # create and load the plist
#   scripts/dogfood-daemon.sh uninstall  # unload and remove the plist
#   scripts/dogfood-daemon.sh restart    # restart with the newly installed binary
#   scripts/dogfood-daemon.sh status     # show status and recent logs
#   scripts/dogfood-daemon.sh logs       # tail logs
#
# Defaults to development authentication on 127.0.0.1:8907. To use Firebase,
# set both CXT_AUTH=firebase and CXT_FIREBASE_PROJECT=<project-id> before install.
set -euo pipefail

LABEL="com.cxthub.cxtd"
PLIST="$HOME/Library/LaunchAgents/$LABEL.plist"
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
BIN="$(command -v cxtd || echo "$HOME/go/bin/cxtd")"
ADDR="${CXT_ADDR:-127.0.0.1:8907}"
AUTH="${CXT_AUTH:-dev}"
FIREBASE="${CXT_FIREBASE_PROJECT:-}"
DATA="$ROOT/cxt-data"
LOGDIR="$ROOT/.cxt-daemon"
GUI="gui/$(id -u)"

case "$AUTH" in
  dev) ;;
  firebase)
    if [[ ! "$FIREBASE" =~ ^[a-z][a-z0-9-]{4,28}[a-z0-9]$ ]]; then
      echo "CXT_FIREBASE_PROJECT must be a valid Firebase project ID when CXT_AUTH=firebase." >&2
      exit 2
    fi
    ;;
  *)
    echo "CXT_AUTH must be dev or firebase." >&2
    exit 2
    ;;
esac

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

case "${1:-}" in
  install)
    # Stop an existing shell-background cxtd process if it owns the port.
    pkill -x cxtd 2>/dev/null || true
    sleep 1
    write_plist
    launchctl bootout "$GUI" "$PLIST" 2>/dev/null || true
    launchctl bootstrap "$GUI" "$PLIST"
    launchctl enable "$GUI/$LABEL"
    echo "✓ installed and started: $LABEL"
    echo "  binary: $BIN"
    echo "  address: http://$ADDR  data: $DATA"
    echo "  auth: $AUTH"
    echo "  logs: $LOGDIR/cxtd.{out,err}.log"
    ;;
  uninstall)
    launchctl bootout "$GUI" "$PLIST" 2>/dev/null || true
    rm -f "$PLIST"
    echo "✓ unloaded and removed: $LABEL"
    ;;
  restart)
    launchctl kickstart -k "$GUI/$LABEL"
    echo "✓ restarted with the newly installed binary: $LABEL"
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
  *)
    echo "Usage: $0 {install|uninstall|restart|status|logs}"; exit 1 ;;
esac
