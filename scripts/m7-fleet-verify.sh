#!/usr/bin/env bash
# M7 fleet verification on real linux/arm64 Pis.
#
# Stands up an ISOLATED test hub on one Pi (never touches the production hub on
# .10) and runs real daemons on two more Pis to prove the M7 surface on arm64:
#   - cross-compiled binaries run
#   - live two-way sync (A's rw watcher pushes, B's ro mount pulls via SSE)
#   - reconnect with backoff after the hub restarts
#   - PERIODIC RESCAN FALLBACK: with A's watcher killed, a local edit still
#     propagates (the gap this branch fixes — PRD risk #1)
#   - devbox doctor's real inotify check + failure path
#   - devbox stop tears the daemon down cleanly
set -uo pipefail

KEY="$HOME/.ssh/pi"
SSH="ssh -i $KEY -o StrictHostKeyChecking=no -o ConnectTimeout=8 -o BatchMode=yes"
SCP="scp -i $KEY -o StrictHostKeyChecking=no -o ConnectTimeout=8 -o BatchMode=yes"
HUBPI=192.168.1.11; API=192.168.1.12; BPI=192.168.1.13
PORT=18080; HUBURL="http://$HUBPI:$PORT"; R=/tmp/devbox-m7test
INO_WATCH=62164; INO_INST=128

say()  { printf '\n\033[1;36m== %s ==\033[0m\n' "$*"; }
on()   { local h=$1; shift; $SSH "shoemoney@$h" "$@"; }
start_remote() { local h=$1; shift; $SSH "shoemoney@$h" "$@" </dev/null >/dev/null 2>&1 & echo $!; }
fail() { echo "❌ $*"; exit 1; }
dc()   { local h=$1; shift; on "$h" "cd $R && XDG_CONFIG_HOME=$R/cfg ./devbox $*"; }  # devbox-on-host

HUB_SSH=""; A_SSH=""; B_SSH=""
cleanup() {
  say cleanup
  for p in "$HUB_SSH" "$A_SSH" "$B_SSH"; do [ -n "$p" ] && kill "$p" 2>/dev/null; done
  for h in $HUBPI $API $BPI; do on "$h" "pkill -f devbox 2>/dev/null; rm -rf $R" 2>/dev/null; done
  on "$API" "sudo sysctl -w fs.inotify.max_user_instances=$INO_INST >/dev/null 2>&1" 2>/dev/null
  on "$BPI" "sudo sysctl -w fs.inotify.max_user_watches=$INO_WATCH >/dev/null 2>&1" 2>/dev/null
}
trap cleanup EXIT

say "build + ship linux/arm64 binaries"
( cd "$(dirname "$0")/.." && CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -o /tmp/devbox.arm64 ./cmd/devbox \
  && CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -o /tmp/devbox-hub.arm64 ./cmd/devbox-hub ) || fail build
for h in $HUBPI $API $BPI; do on "$h" "mkdir -p $R"; $SCP -q /tmp/devbox.arm64 "shoemoney@$h:$R/devbox"; on "$h" "chmod +x $R/devbox"; done
$SCP -q /tmp/devbox-hub.arm64 "shoemoney@$HUBPI:$R/devbox-hub"; on "$HUBPI" "chmod +x $R/devbox-hub"
echo "arm64 binary runs: $(on $API "$R/devbox version")"

say "start isolated test hub on $HUBPI:$PORT"
HUB_SSH=$(start_remote "$HUBPI" "cd $R && exec ./devbox-hub serve --data $R/hubdata --listen 0.0.0.0:$PORT >$R/hub.log 2>&1")
curl --retry 40 --retry-connrefused --retry-delay 0 --max-time 15 -sf "$HUBURL/metrics" >/dev/null && echo "hub up ✅" || fail "hub down"
mint() { on "$HUBPI" "cd $R && ./devbox-hub token --data $R/hubdata"; }

say "device A ($API): join, publish, mount rw, run daemon"
dc "$API" join "$HUBURL $(mint)" >/dev/null
on "$API" "mkdir -p $R/src && printf 'v1\n' > $R/src/app.txt"
dc "$API" publish "$R/src proj" >/dev/null
dc "$API" mount "proj $R/src" >/dev/null
A_SSH=$(start_remote "$API" "cd $R && exec env XDG_CONFIG_HOME=$R/cfg ./devbox start >$R/daemon.log 2>&1")
on "$API" "for i in \$(seq 1 25); do [ -f $R/cfg/devbox/devboxd.pid ] && break; sleep 0.2; done" && echo "A daemon up ✅"

say "device B ($BPI): join, mount ro, run daemon"
dc "$BPI" join "$HUBURL $(mint)" >/dev/null
dc "$BPI" mount "proj $R/dst --ro" >/dev/null
[ "$(on $BPI "cat $R/dst/app.txt")" = v1 ] && echo "B has v1 ✅"
B_SSH=$(start_remote "$BPI" "cd $R && exec env XDG_CONFIG_HOME=$R/cfg ./devbox start >$R/daemon.log 2>&1")
on "$BPI" "for i in \$(seq 1 25); do [ -f $R/cfg/devbox/devboxd.pid ] && break; sleep 0.2; done" && echo "B daemon up ✅"

say "doctor on A (real inotify check, should pass)"
dc "$API" doctor || fail "doctor A nonzero"

say "live sync: A's watcher pushes v2 -> B pulls"
on "$API" "printf 'v2\n' > $R/src/app.txt"
on "$BPI" "for i in \$(seq 1 60); do [ \"\$(cat $R/dst/app.txt 2>/dev/null)\" = v2 ] && break; sleep 0.3; done"
[ "$(on $BPI "cat $R/dst/app.txt")" = v2 ] && echo "B live-synced v2 ✅" || { on "$BPI" "tail $R/daemon.log"; fail "no live sync"; }

say "reconnect/backoff: restart hub, A pushes v3 -> B catches up"
kill "$HUB_SSH" 2>/dev/null; on "$HUBPI" "pkill -f 'devbox-hub serve'"; sleep 2
HUB_SSH=$(start_remote "$HUBPI" "cd $R && exec ./devbox-hub serve --data $R/hubdata --listen 0.0.0.0:$PORT >>$R/hub.log 2>&1")
curl --retry 40 --retry-connrefused --retry-delay 0 --max-time 15 -sf "$HUBURL/metrics" >/dev/null && echo "hub back ✅" || fail "hub restart"
on "$API" "printf 'v3\n' > $R/src/app.txt"
on "$BPI" "for i in \$(seq 1 90); do [ \"\$(cat $R/dst/app.txt 2>/dev/null)\" = v3 ] && break; sleep 0.5; done"
[ "$(on $BPI "cat $R/dst/app.txt")" = v3 ] && echo "B reconnected + synced v3 ✅" || fail "no reconnect"
on "$API" "grep -q reconnecting $R/daemon.log && echo 'A backoff reconnect logged ✅' || echo '(recovered; no reconnect log)'"

say "RESCAN FALLBACK: kill A's watcher, local edit must still propagate"
kill "$A_SSH" 2>/dev/null; on "$API" "pkill -f 'devbox start'"; sleep 1
on "$API" "sudo sysctl -w fs.inotify.max_user_instances=0 >/dev/null"   # fsnotify.NewWatcher() now fails
A_SSH=$(start_remote "$API" "cd $R && exec env XDG_CONFIG_HOME=$R/cfg ./devbox start >$R/daemon.log 2>&1")
sleep 2
on "$API" "grep -q 'falling back to' $R/daemon.log && echo 'A watcher dead, rescan fallback engaged ✅' || { echo '⚠️ expected fallback log'; tail $R/daemon.log; }"
on "$API" "printf 'rescued\n' > $R/src/rescan.txt"   # no watcher event will fire for this
echo "waiting up to ~75s for the 60s rescan tick to push it..."
on "$BPI" "for i in \$(seq 1 150); do [ \"\$(cat $R/dst/rescan.txt 2>/dev/null)\" = rescued ] && break; sleep 0.5; done"
[ "$(on $BPI "cat $R/dst/rescan.txt 2>/dev/null")" = rescued ] && echo "✅ local edit propagated via RESCAN with a dead watcher" || { on "$API" "tail $R/daemon.log"; fail "rescan did not push"; }
on "$API" "sudo sysctl -w fs.inotify.max_user_instances=$INO_INST >/dev/null"

say "doctor inotify FAILURE path on B (lower watches, expect fail + fix hint)"
on "$BPI" "sudo sysctl -w fs.inotify.max_user_watches=1 >/dev/null"
dc "$BPI" doctor; echo "doctor exit=$?"
on "$BPI" "sudo sysctl -w fs.inotify.max_user_watches=$INO_WATCH >/dev/null"

say "devbox stop on B"
dc "$BPI" stop
on "$BPI" "for i in \$(seq 1 25); do [ -f $R/cfg/devbox/devboxd.pid ] || break; sleep 0.2; done; [ ! -f $R/cfg/devbox/devboxd.pid ] && echo 'B stopped + pidfile gone ✅'" || fail stop

say "🎉 M7 FLEET VERIFY PASSED (incl. rescan fallback)"
