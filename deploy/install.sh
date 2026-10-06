#!/bin/sh
# Install or upgrade checkin-board on a graywolf node (Raspberry Pi OS /
# Debian with systemd). Run as root from the unpacked release directory:
#
#   sudo ./install.sh            # picks bin/checkin-board-<arch> for this machine
#   sudo ./install.sh /path/to/checkin-board
#
# Safe to re-run: settings and the graywolf password are never
# overwritten, and on upgrade the database is copied aside first.
set -eu

HERE=$(cd "$(dirname "$0")" && pwd)

# The bundle carries one binary per architecture; pick this machine's.
default_binary() {
	case "$(uname -m)" in
	x86_64 | amd64) echo "$HERE/bin/checkin-board-linux-amd64" ;;
	aarch64 | arm64) echo "$HERE/bin/checkin-board-linux-arm64" ;;
	armv6l | armv7l | armv8l | arm*) echo "$HERE/bin/checkin-board-linux-armv6" ;;
	*) echo "$HERE/checkin-board" ;;
	esac
}
BIN_SRC=${1:-$(default_binary)}
[ -f "$BIN_SRC" ] || [ -n "${1:-}" ] || BIN_SRC=$HERE/checkin-board
USER_NAME=checkin-board
BIN=/usr/local/bin/checkin-board
ETC=/etc/checkin-board
ENV_FILE=$ETC/checkin-board.env
PW_FILE=$ETC/gw-password
STATE=/var/lib/checkin-board
UNIT=/etc/systemd/system/checkin-board.service
DOC=/usr/local/share/doc/checkin-board

say() { printf '%s\n' "$*"; }
die() { printf 'install: %s\n' "$*" >&2; exit 1; }

[ "$(id -u)" -eq 0 ] || die "run as root (sudo $0)"
command -v systemctl >/dev/null 2>&1 || die "systemd is required"
[ -f "$BIN_SRC" ] || die "binary not found: $BIN_SRC"
chmod 0755 "$BIN_SRC"
VERSION=$("$BIN_SRC" version 2>/dev/null) || die "$BIN_SRC doesn't run on this machine (wrong architecture?)"
say "Installing $VERSION"

# Service account: no login, no home directory of its own.
if ! id "$USER_NAME" >/dev/null 2>&1; then
	useradd --system --home-dir "$STATE" --no-create-home --shell /usr/sbin/nologin "$USER_NAME"
	say "Created system user $USER_NAME"
fi

# Upgrade: stop the service (also a crash-looping one) and copy the
# database aside before the new binary migrates it. If anything below
# fails, the old version is started again so the node doesn't go dark.
UPGRADE=no
[ -f "$BIN" ] && UPGRADE=yes
STOPPED=no
DONE=no
restart_on_failure() {
	if [ "$STOPPED" = yes ] && [ "$DONE" = no ]; then
		say "install: failed; starting the previous version again" >&2
		systemctl start checkin-board || true
	fi
}
trap restart_on_failure EXIT
if [ -f "$UNIT" ]; then
	systemctl stop checkin-board
	STOPPED=yes
fi
if [ -f "$STATE/checkin-board.db" ]; then
	# The state dir belongs to the service user: never follow links it
	# could have planted while root copies files.
	for d in "$STATE" "$STATE/backups"; do
		[ -L "$d" ] && die "$d is a symlink; refusing to write through it"
	done
	need=$(du -sk "$STATE"/checkin-board.db* | awk '{s += $1} END {print s + 1024}')
	free=$(df -Pk "$STATE" | awk 'NR == 2 {print $4}')
	[ "$free" -gt "$need" ] || die "not enough disk space for the pre-upgrade copy (${need} KB needed, ${free} KB free)"
	STAMP=$(date +%Y%m%d-%H%M%S)
	mkdir -p -m 0750 "$STATE/backups"
	BK="$STATE/backups/pre-upgrade-$STAMP"
	mkdir -m 0700 "$BK" # fails if it already exists
	for f in "$STATE"/checkin-board.db "$STATE"/checkin-board.db-wal "$STATE"/checkin-board.db-shm; do
		if [ -f "$f" ] && [ ! -L "$f" ]; then
			cp -p "$f" "$BK/"
		fi
	done
	chown -hR "$USER_NAME:$USER_NAME" "$BK"
	chown -h "$USER_NAME:$USER_NAME" "$STATE/backups"
	say "Database copied to $BK"
fi

install -m 0755 "$BIN_SRC" "$BIN"

install -d -m 0750 -o root -g "$USER_NAME" "$ETC"
if [ ! -f "$ENV_FILE" ]; then
	install -m 0640 -o root -g "$USER_NAME" "$HERE/checkin-board.env" "$ENV_FILE"
	say "Wrote $ENV_FILE (edit GW_USER / GW_BASE_URL if needed)"
fi

if [ ! -s "$PW_FILE" ]; then
	if [ -t 0 ]; then
		say "Password of the graywolf login the app uses (GW_USER in $ENV_FILE):"
		trap 'stty echo; restart_on_failure' EXIT
		trap 'stty echo; exit 130' INT TERM
		stty -echo
		IFS= read -r PW || PW=
		stty echo
		trap restart_on_failure EXIT
		trap - INT TERM
		say ""
		if [ -n "$PW" ]; then
			umask 077
			printf '%s\n' "$PW" >"$PW_FILE"
		fi
		unset PW
	fi
	[ -f "$PW_FILE" ] || : >"$PW_FILE"
fi
chown "$USER_NAME:$USER_NAME" "$PW_FILE"
chmod 0600 "$PW_FILE"

install -m 0644 "$HERE/checkin-board.service" "$UNIT"
if [ -f "$HERE/operator-guide.md" ]; then
	install -d -m 0755 "$DOC"
	install -m 0644 "$HERE/operator-guide.md" "$DOC/operator-guide.md"
fi

systemctl daemon-reload
systemctl enable --quiet checkin-board

if [ ! -s "$PW_FILE" ]; then
	DONE=yes
	say ""
	say "Not started: put the graywolf password in $PW_FILE, then:"
	say "  sudo systemctl start checkin-board"
	exit 0
fi

systemctl start checkin-board
DONE=yes
if [ "$UPGRADE" = yes ]; then
	say "Upgraded and restarted."
else
	say ""
	say "Started. Open http://$(hostname):8090/ and enter the setup code from:"
	say "  journalctl -u checkin-board | grep setup_code"
fi
