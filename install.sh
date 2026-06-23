#!/bin/sh
# devbox installer — macOS + Linux. Detects your platform, installs the binary to
# a location you choose, and (optionally) sets up a keep-alive auto-restart service
# so the sync daemon survives crashes and reboots.
#
#   curl -fsSL https://git.shoemoney.ai/shoemoney/devbox/raw/branch/main/install.sh | sh
#   sh install.sh                       # interactive: pick dir, offer service
#   sh install.sh --hub --service       # also install the hub + enable the service
#   DEVBOX_BIN_DIR=~/.local/bin DEVBOX_SERVICE=1 sh install.sh   # non-interactive
#
# Knobs (flags or env): --bin-dir DIR (DEVBOX_BIN_DIR) · --hub (DEVBOX_INSTALL_HUB=1)
# · --service / --no-service (DEVBOX_SERVICE=1/0) · --release-url URL (DEVBOX_RELEASE_URL).
set -eu

INSTALL_HUB="${DEVBOX_INSTALL_HUB:-0}"
BIN_DIR="${DEVBOX_BIN_DIR:-}"
WANT_SERVICE="${DEVBOX_SERVICE:-ask}"
RELEASE_URL="${DEVBOX_RELEASE_URL:-https://git.shoemoney.ai/shoemoney/devbox/releases/download/latest}"

while [ $# -gt 0 ]; do
  case "$1" in
    --hub) INSTALL_HUB=1 ;;
    --bin-dir) BIN_DIR="$2"; shift ;;
    --service) WANT_SERVICE=1 ;;
    --no-service) WANT_SERVICE=0 ;;
    --release-url) RELEASE_URL="$2"; shift ;;
    -h|--help) sed -n '2,14p' "$0" | sed 's/^# \{0,1\}//'; exit 0 ;;
    *) echo "unknown option: $1" >&2; exit 2 ;;
  esac
  shift
done

say()  { printf '%s\n' "$*"; }
die()  { printf '🛑 %s\n' "$*" >&2; exit 1; }
is_tty(){ [ -t 0 ] && [ -t 1 ]; }

# --- detect platform -------------------------------------------------------
OS=$(uname -s | tr '[:upper:]' '[:lower:]')
case "$OS" in darwin|linux) ;; *) die "unsupported OS '$OS' (this installer is macOS/Linux; use install.ps1 on Windows)";; esac
ARCH=$(uname -m)
case "$ARCH" in
  x86_64|amd64) ARCH=amd64 ;;
  arm64|aarch64) ARCH=arm64 ;;
  *) die "unsupported arch '$ARCH'";;
esac
# windows-only target aside, we ship linux/amd64, linux/arm64*, darwin/arm64, darwin/amd64.
say "📦 devbox installer — detected ${OS}/${ARCH}"

SCRIPT_DIR=$(cd -- "$(dirname -- "$0")" 2>/dev/null && pwd || echo .)
TMP=$(mktemp -d)
trap 'rm -rf "$TMP"' EXIT
HAVE_GO=0; command -v go >/dev/null 2>&1 && HAVE_GO=1

# resolve_bin <name> -> prints an executable path for that binary, building or
# downloading as needed. Order: explicit env, local dist/, beside-script, download,
# go build (if a checkout + toolchain are present). Keeps working before any
# published release exists.
resolve_bin() {
  name=$1
  envvar=$(printf '%s_BIN' "$(echo "$name" | tr 'a-z-' 'A-Z_')") # devbox->DEVBOX_BIN, devbox-hub->DEVBOX_HUB_BIN
  eval explicit="\${$envvar:-}"
  if [ -n "${explicit:-}" ] && [ -x "$explicit" ]; then echo "$explicit"; return; fi

  for d in "$SCRIPT_DIR/dist" "$SCRIPT_DIR/../dist" "./dist"; do
    for cand in "$d/${name}"_*_"${OS}"_"${ARCH}"; do
      [ -x "$cand" ] && { echo "$cand"; return; }
    done
  done
  [ -x "$SCRIPT_DIR/$name" ] && { echo "$SCRIPT_DIR/$name"; return; }

  asset="${name}_${OS}_${ARCH}"
  if curl -fsSL "$RELEASE_URL/$asset" -o "$TMP/$name" 2>/dev/null && [ -s "$TMP/$name" ]; then
    chmod +x "$TMP/$name"; echo "$TMP/$name"; return
  fi

  if [ "$HAVE_GO" = 1 ] && [ -f "$SCRIPT_DIR/go.mod" ]; then
    say "   building $name from source (go)…" >&2
    ( cd "$SCRIPT_DIR" && CGO_ENABLED=0 go build -o "$TMP/$name" "./cmd/$name" ) || die "go build $name failed"
    echo "$TMP/$name"; return
  fi
  die "could not find or fetch '$name' — set ${envvar}=/path, run scripts/build-release.sh first, or set --release-url"
}

# --- choose install dir ----------------------------------------------------
default_bin_dir() {
  if [ -w /usr/local/bin ] 2>/dev/null; then echo /usr/local/bin; return; fi
  if command -v sudo >/dev/null 2>&1 && [ "$OS" = linux ]; then echo /usr/local/bin; return; fi
  echo "$HOME/.local/bin"
}
if [ -z "$BIN_DIR" ]; then
  def=$(default_bin_dir)
  if is_tty; then
    printf 'Where should the binaries live? [%s] ' "$def"
    read -r ans </dev/tty || ans=""
    BIN_DIR=${ans:-$def}
  else
    BIN_DIR=$def
  fi
fi
BIN_DIR=$(eval echo "$BIN_DIR")   # expand a leading ~

SUDO=""
if ! mkdir -p "$BIN_DIR" 2>/dev/null || ! [ -w "$BIN_DIR" ]; then
  if command -v sudo >/dev/null 2>&1; then
    SUDO="sudo"; $SUDO mkdir -p "$BIN_DIR" || die "cannot create $BIN_DIR"
  else
    die "$BIN_DIR is not writable and sudo is unavailable — pick another with --bin-dir"
  fi
fi

install_one() {
  src=$(resolve_bin "$1")
  $SUDO install -m 0755 "$src" "$BIN_DIR/$1"
  say "   ✅ $1 → $BIN_DIR/$1"
}

say "🚚 installing to $BIN_DIR"
install_one devbox
[ "$INSTALL_HUB" = 1 ] && install_one devbox-hub

case ":$PATH:" in *":$BIN_DIR:"*) ;; *)
  say "⚠️  $BIN_DIR is not on your PATH. Add to your shell rc:"
  say "      export PATH=\"$BIN_DIR:\$PATH\"" ;;
esac

# --- keep-alive service (optional) ----------------------------------------
DEVBOX="$BIN_DIR/devbox"

install_service_macos() {
  label="ai.shoemoney.devbox"
  plist="$HOME/Library/LaunchAgents/$label.plist"
  mkdir -p "$HOME/Library/LaunchAgents" "$HOME/Library/Logs"
  cat > "$plist" <<PLIST
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0"><dict>
  <key>Label</key><string>$label</string>
  <key>ProgramArguments</key><array><string>$DEVBOX</string><string>start</string></array>
  <key>RunAtLoad</key><true/>
  <key>KeepAlive</key><true/>
  <key>ThrottleInterval</key><integer>5</integer>
  <key>StandardOutPath</key><string>$HOME/Library/Logs/devbox.log</string>
  <key>StandardErrorPath</key><string>$HOME/Library/Logs/devbox.log</string>
</dict></plist>
PLIST
  launchctl unload "$plist" 2>/dev/null || true
  if launchctl load "$plist" 2>/dev/null; then
    say "   ✅ launchd agent loaded (KeepAlive). logs: ~/Library/Logs/devbox.log"
    say "      stop: launchctl unload $plist"
  else
    say "   ⚠️  wrote $plist but 'launchctl load' failed (run it from a desktop session, not SSH):"
    say "      launchctl load $plist"
  fi
}

install_service_linux() {
  unit="$HOME/.config/systemd/user/devbox.service"
  mkdir -p "$HOME/.config/systemd/user"
  cat > "$unit" <<UNIT
[Unit]
Description=devbox sync daemon
After=network-online.target
Wants=network-online.target

[Service]
ExecStart=$DEVBOX start
Restart=always
RestartSec=3

[Install]
WantedBy=default.target
UNIT
  if command -v systemctl >/dev/null 2>&1 &&
     systemctl --user daemon-reload 2>/dev/null &&
     systemctl --user enable --now devbox.service 2>/dev/null; then
    loginctl enable-linger "$USER" 2>/dev/null || say "   (run 'sudo loginctl enable-linger $USER' so it starts before login)"
    say "   ✅ systemd user service enabled (Restart=always). logs: journalctl --user -u devbox -f"
    say "      stop: systemctl --user disable --now devbox"
  else
    say "   ⚠️  wrote $unit, but couldn't enable it via 'systemctl --user' (no user session bus is"
    say "      common over headless SSH). Enable from a desktop login: systemctl --user enable --now devbox"
  fi
}

if [ "$WANT_SERVICE" = ask ]; then
  if is_tty; then
    printf 'Set up a keep-alive auto-restart service for the sync daemon? [Y/n] '
    read -r ans </dev/tty || ans=""
    case "$ans" in [Nn]*) WANT_SERVICE=0 ;; *) WANT_SERVICE=1 ;; esac
  else
    WANT_SERVICE=0
  fi
fi
if [ "$WANT_SERVICE" = 1 ]; then
  say "🔁 setting up keep-alive service"
  if [ "$OS" = darwin ]; then install_service_macos; else install_service_linux; fi
fi

# --- macOS Full Disk Access -----------------------------------------------
# You can't grant FDA from a script (it's a hard TCC boundary) — but a background
# daemon syncing ~/Desktop, ~/Documents, ~/Downloads or iCloud will silently fail
# without it (it can't even show the per-folder prompt). So point the user at it.
if [ "$OS" = darwin ]; then
  say ""
  say "🔐 macOS Full Disk Access — needed only if you'll sync folders in ~/Desktop,"
  say "   ~/Documents, ~/Downloads, or iCloud Drive. Grant it to:  $BIN_DIR/devbox"
  say "   System Settings → Privacy & Security → Full Disk Access → + (add the binary)"
  say "   ('devbox doctor' tells you if it's actually needed/missing.)"
  if is_tty; then
    printf '   open that settings pane now? [y/N] '
    read -r ans </dev/tty || ans=""
    case "$ans" in [Yy]*) open "x-apple.systempreferences:com.apple.preference.security?Privacy_AllFiles" 2>/dev/null || true ;; esac
  fi
fi

say ""
say "🎉 done. Next: run 'devbox setup' to join a hub and start syncing."
[ "$INSTALL_HUB" = 1 ] && say "    hub: 'devbox-hub serve --dashboard' — or use the Dockerfile for a NAS (see README)."
