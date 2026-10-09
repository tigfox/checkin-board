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
PANEL_UNIT=/etc/systemd/system/checkin-board-panel.service
HOOK_FILE=$ETC/hook-token
# graywolf's own database, for creating the app's graywolf login
# (override with GRAYWOLF_DB=/path/to/graywolf.db).
GW_DB=${GRAYWOLF_DB:-/var/lib/graywolf/graywolf.db}
GW_LOGIN=checkin-board
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
[ -f "$STATE/checkin-board.db" ] && UPGRADE=yes # a node that has run before
STOPPED=no
DONE=no
PANEL_WAS=no
restart_on_failure() {
	if [ "$STOPPED" = yes ] && [ "$DONE" = no ]; then
		say "install: failed; starting the previous version again" >&2
		systemctl start checkin-board || true
		[ "$PANEL_WAS" = yes ] && systemctl start checkin-board-panel || true
	fi
}
trap restart_on_failure EXIT
if [ -f "$UNIT" ]; then
	if systemctl is-active --quiet checkin-board-panel 2>/dev/null; then
		PANEL_WAS=yes
	fi
	systemctl stop checkin-board-panel 2>/dev/null || true
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

# run_as USER CMD...: run a command as another (system) user.
run_as() {
	u=$1
	shift
	if [ "$u" = root ]; then
		"$@"
	elif command -v runuser >/dev/null 2>&1; then
		runuser -u "$u" -- "$@"
	elif command -v sudo >/dev/null 2>&1; then
		sudo -n -u "$u" -- "$@"
	else
		return 127
	fi
}

# create_gw_login: create (or reset) the app's own graywolf login with a
# random password, using graywolf's CLI as the owner of its database, and
# save the password for the app. Only for the app's dedicated login: an
# operator who pointed GW_USER at their own login is never touched.
create_gw_login() {
	gw_user=$(sed -n 's/^GW_USER=//p' "$ENV_FILE" | tail -n 1)
	[ "$gw_user" = "$GW_LOGIN" ] || return 1
	gw_bin=$(command -v graywolf || true)
	[ -n "$gw_bin" ] && [ -f "$GW_DB" ] || return 1
	owner=$(stat -c %U "$GW_DB")
	# 32 random bytes as hex: no spaces (graywolf reads one word).
	pw=$(od -An -N32 -tx1 /dev/urandom | tr -d ' \n')
	out=$(printf '%s\n' "$pw" | run_as "$owner" "$gw_bin" auth set-password --user "$GW_LOGIN" -config "$GW_DB" 2>&1) || {
		say "Couldn't create the graywolf login automatically: $out"
		return 1
	}
	case "$out" in
	*"Created user"* | *"Updated password"*) ;;
	*)
		say "Couldn't create the graywolf login automatically: $out"
		return 1
		;;
	esac
	(umask 077 && printf '%s\n' "$pw" >"$PW_FILE")
	unset pw
	case "$out" in
	*"Created user"*) say "Created graywolf login '$GW_LOGIN' for the app (password saved in $PW_FILE)" ;;
	*) say "Reset graywolf login '$GW_LOGIN' for the app (password saved in $PW_FILE)" ;;
	esac
}

if [ ! -s "$PW_FILE" ] && ! create_gw_login && [ ! -s "$PW_FILE" ]; then
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

# The local hook token (spec 2.2): the node panel uses it, and so can a
# graywolf webhook Action (docs/linkcheck-action.md).
if [ ! -s "$HOOK_FILE" ]; then
	(umask 077 && od -An -N32 -tx1 /dev/urandom | tr -d ' \n' >"$HOOK_FILE")
fi
chown "$USER_NAME:$USER_NAME" "$HOOK_FILE"
chmod 0600 "$HOOK_FILE"
if ! grep -q '^CB_HOOK_TOKEN_FILE=' "$ENV_FILE"; then
	printf '\nCB_HOOK_TOKEN_FILE=%s\n' "$HOOK_FILE" >>"$ENV_FILE"
fi

# Wi-Fi power saving makes a Pi drop off the network for minutes at a
# time (seen on the test node), so phones can't reach the keypad: turn it
# off for every Wi-Fi connection, now and after reboots.
NM_CONF=/etc/NetworkManager/conf.d/90-checkin-board-wifi-powersave.conf
if [ -d /etc/NetworkManager/conf.d ]; then
	if [ ! -f "$NM_CONF" ]; then
		printf '# checkin-board: keep Wi-Fi awake (2 = power saving off).\n[connection]\nwifi.powersave = 2\n' >"$NM_CONF"
		chmod 0644 "$NM_CONF"
		systemctl reload NetworkManager 2>/dev/null || true
		say "Wi-Fi power saving turned off ($NM_CONF)"
	fi
	if command -v iw >/dev/null 2>&1; then
		for dev in $(iw dev 2>/dev/null | awk '$1 == "Interface" {print $2}'); do
			iw dev "$dev" set power_save off 2>/dev/null || true
		done
	fi
elif command -v iw >/dev/null 2>&1 && iw dev 2>/dev/null | grep -q Interface; then
	say "Note: no NetworkManager here. Turn Wi-Fi power saving off in your network setup (iw dev wlan0 set power_save off at boot)."
fi

install -m 0644 "$HERE/checkin-board.service" "$UNIT"
install -m 0644 "$HERE/checkin-board-panel.service" "$PANEL_UNIT"
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

# The node panel (spec 8.4) needs SPI and the spi/gpio groups.
if [ -e /dev/spidev0.0 ] && getent group spi >/dev/null && getent group gpio >/dev/null; then
	systemctl enable --quiet checkin-board-panel
	systemctl restart checkin-board-panel
	say "Node panel started (e-ink bonnet)."
else
	systemctl disable --quiet checkin-board-panel 2>/dev/null || true
	say "Node panel not started: SPI is off (no /dev/spidev0.0). With an e-ink"
	say "bonnet attached: sudo raspi-config nonint do_spi 0, reboot, run this again."
fi
DONE=yes
if [ "$UPGRADE" = yes ]; then
	say "Upgraded and restarted."
else
	say ""
	say "Started. Open http://$(hostname):8090/ and enter the setup code from:"
	say "  journalctl -u checkin-board | grep setup_code"
fi
