#!/bin/sh
# Install or upgrade checkin-board on a graywolf node (Raspberry Pi OS /
# Debian with systemd). Run as root from the unpacked release directory:
#
#   sudo ./install.sh            # picks bin/checkin-board-<arch> for this machine
#   sudo ./install.sh /path/to/checkin-board
#   sudo ./install.sh --graywolf-version v0.14.14   # pin graywolf if it's installed now
#   sudo ./install.sh --no-graywolf                 # never install graywolf
#   sudo ./install.sh --graywolf-deb FILE.deb       # install this graywolf package
#                                                   # (e.g. the Pi Zero build; replaces one)
#
# If graywolf isn't installed, its latest release is installed first
# (spec 2.3), and your graywolf admin login is set up: interactively, or
# from GRAYWOLF_ADMIN_USER and GRAYWOLF_ADMIN_PASSWORD_FILE.
#
# Safe to re-run: settings and the graywolf password are never
# overwritten, an installed graywolf is never upgraded (unless
# --graywolf-deb gives a package), and on upgrade the database is copied
# aside first.
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
GW_VERSION=latest
GW_VERSION_SET=no
GW_INSTALL=yes
GW_DEB=
BIN_ARG=
while [ $# -gt 0 ]; do
	case "$1" in
	--graywolf-version)
		[ $# -ge 2 ] || { printf 'install: --graywolf-version needs a version\n' >&2; exit 2; }
		GW_VERSION=$2 GW_VERSION_SET=yes
		shift 2
		;;
	--graywolf-version=*)
		GW_VERSION=${1#*=} GW_VERSION_SET=yes
		shift
		;;
	--graywolf-deb)
		[ $# -ge 2 ] || { printf 'install: --graywolf-deb needs a .deb file\n' >&2; exit 2; }
		GW_DEB=$2
		shift 2
		;;
	--graywolf-deb=*)
		GW_DEB=${1#*=}
		shift
		;;
	--no-graywolf)
		GW_INSTALL=no
		shift
		;;
	-h | --help)
		sed -n '2,19p' "$0" | sed 's/^# \{0,1\}//'
		exit 0
		;;
	--)
		shift
		if [ $# -gt 0 ]; then
			BIN_ARG=$1
			shift
		fi
		;;
	-*)
		printf 'install: unknown option %s\n' "$1" >&2
		exit 2
		;;
	*)
		BIN_ARG=$1
		shift
		;;
	esac
done
# --graywolf-deb is checked before anything else happens.
if [ -n "$GW_DEB" ]; then
	if [ "$GW_INSTALL" = no ] || [ "$GW_VERSION_SET" = yes ]; then
		printf 'install: --graywolf-deb can'"'"'t be combined with --no-graywolf or --graywolf-version\n' >&2
		exit 2
	fi
	case "$GW_DEB" in
	*.deb) ;;
	*)
		printf 'install: --graywolf-deb %s isn'"'"'t a .deb file\n' "$GW_DEB" >&2
		exit 2
		;;
	esac
	[ -f "$GW_DEB" ] || { printf 'install: --graywolf-deb %s: not found\n' "$GW_DEB" >&2; exit 2; }
	if command -v dpkg-deb >/dev/null 2>&1; then
		pkg=$(dpkg-deb --field "$GW_DEB" Package 2>/dev/null || true)
		if [ "$pkg" != graywolf ]; then
			printf 'install: --graywolf-deb %s isn'"'"'t a graywolf package (Package: %s)\n' "$GW_DEB" "${pkg:-unreadable}" >&2
			exit 2
		fi
	fi
	GW_DEB=$(cd "$(dirname "$GW_DEB")" && pwd)/$(basename "$GW_DEB") # apt needs a path
fi
BIN_SRC=${BIN_ARG:-$(default_binary)}
[ -f "$BIN_SRC" ] || [ -n "$BIN_ARG" ] || BIN_SRC=$HERE/checkin-board
# graywolf's releases (overridable for tests).
GW_RELEASES_URL=${GW_RELEASES_URL:-https://api.github.com/repos/chrissnell/graywolf/releases}
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

# restart_on_failure is redefined below, once the app may be stopped.
restart_on_failure() { :; }

# --- graywolf (spec 2.3): installed from its release when missing ------

# fetch URL FILE: download with curl or wget, with timeouts; HTTPS-only
# (redirects included) for https URLs. Returns 127 with neither tool.
fetch() {
	if command -v curl >/dev/null 2>&1; then
		case "$1" in
		https:*) curl -fsSL --retry 2 --connect-timeout 10 --max-time 300 --proto '=https' --proto-redir '=https' -o "$2" "$1" ;;
		*) curl -fsSL --retry 2 --connect-timeout 10 --max-time 300 -o "$2" "$1" ;;
		esac
	elif command -v wget >/dev/null 2>&1; then
		wget -q --timeout=30 -O "$2" "$1"
	else
		return 127
	fi
}

TESTED_GW=$(printf '%s\n' "$VERSION" | sed -n 's/.*tested with graywolf \([0-9.]*\).*/\1/p')

# graywolf_installed: the graywolf package is fully installed (a removed
# but unpurged or half-configured one counts as missing), or a graywolf
# binary is on the PATH (installed some other way).
graywolf_installed() {
	[ "$(dpkg-query -W -f='${Status}' graywolf 2>/dev/null)" = "install ok installed" ] ||
		command -v graywolf >/dev/null 2>&1
}

# warn_version VERSION: a graywolf other than the tested one works as long
# as its API hasn't changed; say so. Debian revisions (-1, +b1) are ignored.
warn_version() {
	v=${1#v}
	v=${v%%[-+~]*}
	if [ -n "$TESTED_GW" ] && [ "$v" != "$TESTED_GW" ]; then
		say "Note: graywolf $v is installed; this release of checkin-board was tested with $TESTED_GW."
	fi
}

# safe_version: release versions reach URLs, grep and file names.
safe_version() {
	case "$1" in
	'' | *[!A-Za-z0-9._+~-]*) return 1 ;;
	esac
}

# install_graywolf: the latest (or pinned) graywolf release for this
# architecture. The package is checked against the release's
# checksums.txt, which proves the download intact, not who published it
# (both come from the same release). Every step is checked explicitly:
# this runs in an || context, where set -e is off. Returns non-zero,
# after saying why, when graywolf wasn't installed; the app then waits
# for it.
install_graywolf() {
	arch=$(dpkg --print-architecture 2>/dev/null || true)
	case "$arch" in
	# armhf is Raspbian's: it covers ARMv6 (Pi Zero, Pi 1), and graywolf's
	# armhf package runs there (bench); ARMv7 has its own armv7l package.
	amd64 | arm64 | armhf) ;;
	*)
		say "graywolf: no package for architecture '$arch'; install it by hand (see its handbook)."
		return 1
		;;
	esac
	if [ "$GW_VERSION" = latest ]; then
		url="$GW_RELEASES_URL/latest"
	elif safe_version "${GW_VERSION#v}"; then
		url="$GW_RELEASES_URL/tags/v${GW_VERSION#v}"
	else
		say "graywolf: --graywolf-version '$GW_VERSION' isn't a version."
		return 1
	fi
	tmp=$(mktemp -d /tmp/graywolf-install.XXXXXX) || {
		say "graywolf: can't make a temporary directory in /tmp."
		return 1
	}
	chmod 0755 "$tmp" # apt's _apt user reads the package from here
	# Interrupts go through EXIT, so the temp files go.
	trap 'rm -rf "$tmp"; restart_on_failure' EXIT
	trap 'exit 130' INT TERM
	rc=0
	fetch "$url" "$tmp/release.json" || rc=$?
	if [ $rc -ne 0 ]; then
		case $rc in
		127) say "graywolf: neither curl nor wget is installed; install graywolf by hand." ;;
		*) say "graywolf: couldn't fetch $url (offline, GitHub's rate limit, or no such version). Installing checkin-board without it." ;;
		esac
		rm -rf "$tmp"
		return 1
	fi
	tag=$(sed -n 's/.*"tag_name": *"\([^"]*\)".*/\1/p' "$tmp/release.json" | head -n 1)
	ver=${tag#v}
	if ! safe_version "$ver"; then
		say "graywolf: the release has no usable version ('$tag'); install it by hand."
		rm -rf "$tmp"
		return 1
	fi
	deb="graywolf_${ver}_${arch}.deb"
	deb_url=$(grep -F "/$deb\"" "$tmp/release.json" | sed -n 's/.*"browser_download_url": *"\([^"]*\)".*/\1/p' | head -n 1)
	sums_url=$(grep -F '/checksums.txt"' "$tmp/release.json" | sed -n 's/.*"browser_download_url": *"\([^"]*\)".*/\1/p' | head -n 1)
	if [ -z "$deb_url" ] || [ -z "$sums_url" ]; then
		say "graywolf: release $tag has no $deb or checksums.txt; install it by hand."
		rm -rf "$tmp"
		return 1
	fi
	say "Installing graywolf $ver ($arch) from its release..."
	if ! fetch "$deb_url" "$tmp/$deb" || ! fetch "$sums_url" "$tmp/checksums.txt"; then
		say "graywolf: download failed. Installing checkin-board without it."
		rm -rf "$tmp"
		return 1
	fi
	# Exactly one "<64 hex>  <deb>" line, checked explicitly (an empty
	# list must never pass).
	line=$(awk -v f="$deb" '$2 == f && $1 ~ /^[0-9a-f]+$/ && length($1) == 64 { print; exit }' "$tmp/checksums.txt")
	if [ -z "$line" ] || ! command -v sha256sum >/dev/null 2>&1 ||
		! (cd "$tmp" && printf '%s\n' "$line" | sha256sum -c --status); then
		rm -rf "$tmp"
		die "graywolf: $deb doesn't match the release's checksum (or has none); not installing it"
	fi
	if ! DEBIAN_FRONTEND=noninteractive apt-get install -y -q -o DPkg::Lock::Timeout=120 "$tmp/$deb"; then
		say "graywolf: apt couldn't install it (see above). Installing checkin-board without it."
		rm -rf "$tmp"
		return 1
	fi
	rm -rf "$tmp"
	trap restart_on_failure EXIT
	trap 'exit 130' INT TERM
	if [ "$(dpkg-query -W -f='${Status}' graywolf 2>/dev/null)" != "install ok installed" ]; then
		say "graywolf: the package didn't install cleanly; check 'dpkg -s graywolf'."
		return 1
	fi
	systemctl enable --quiet graywolf 2>/dev/null || true
	systemctl start graywolf 2>/dev/null || true
	warn_version "$ver"
	GW_FRESH=yes
}

# install_graywolf_deb: the package given with --graywolf-deb, over any
# installed graywolf (that's how a Pi Zero gets the fixed build,
# deploy/graywolf-armv6). Like install_graywolf, returns non-zero after
# saying why.
install_graywolf_deb() {
	pkg=$(dpkg-deb --field "$GW_DEB" Package 2>/dev/null || true)
	if [ "$pkg" != graywolf ]; then
		say "graywolf: $GW_DEB isn't a graywolf package (Package: '${pkg:-unreadable}'); not installing it."
		return 1
	fi
	was=no
	graywolf_installed && was=yes
	say "Installing graywolf from $(basename "$GW_DEB")..."
	if ! DEBIAN_FRONTEND=noninteractive apt-get install -y -q -o DPkg::Lock::Timeout=120 --allow-downgrades "$GW_DEB"; then
		say "graywolf: apt couldn't install it (see above)."
		return 1
	fi
	if [ "$(dpkg-query -W -f='${Status}' graywolf 2>/dev/null)" != "install ok installed" ]; then
		say "graywolf: the package didn't install cleanly; check 'dpkg -s graywolf'."
		return 1
	fi
	systemctl enable --quiet graywolf 2>/dev/null || true
	systemctl restart graywolf 2>/dev/null || true
	[ "$was" = no ] && GW_FRESH=yes
	return 0
}

# pi_zero_notes: a Pi Zero W / Pi 1 (ARMv6) needs graywolf's fixed build
# and 24 kHz audio, or it transmits a silent carrier and receives
# nothing (docs/feedback-2026-10-09.md, items 8 and 10).
pi_zero_notes() {
	[ "$(uname -m)" = armv6l ] || return 0
	gwv=$(dpkg-query -W -f='${Version}' graywolf 2>/dev/null || true)
	case "$gwv" in
	*armv6buf*)
		say "Pi Zero: graywolf $gwv has the Pi Zero fixes. Set its AIOC audio to 24 kHz if you haven't (station guide, Pi Zero nodes)."
		;;
	'') ;;
	*)
		say "WARNING: this is a Pi Zero (ARMv6), and graywolf $gwv lacks the Pi Zero fixes:"
		say "  it transmits a silent carrier and drops received audio. Build the fixed package"
		say "  (deploy/graywolf-armv6/build.sh), install it with --graywolf-deb, and set the"
		say "  AIOC audio to 24 kHz (station guide, Pi Zero nodes)."
		;;
	esac
}

# graywolf_admin: a fresh graywolf has no users, and its web UI offers
# "Create Admin Account" until one exists. The app's own login (created
# below) would be the first and hide that screen, so the operator's admin
# login is made first; if that fails, the app's login isn't made either.
GW_ADMIN_OK=no
graywolf_admin() {
	i=0
	while [ ! -f "$GW_DB" ] && [ $i -lt 30 ]; do # graywolf creates it on first start
		sleep 1
		i=$((i + 1))
	done
	gw_bin=$(command -v graywolf || true)
	if [ ! -f "$GW_DB" ] || [ -z "$gw_bin" ]; then
		say "graywolf: its database didn't appear; create your admin login in its web UI (http://$(hostname):8080)."
		return 0
	fi
	owner=$(stat -c %U "$GW_DB")
	user=${GRAYWOLF_ADMIN_USER:-}
	pw=
	if [ -n "${GRAYWOLF_ADMIN_PASSWORD_FILE:-}" ]; then
		if [ -r "$GRAYWOLF_ADMIN_PASSWORD_FILE" ]; then
			pw=$(head -n 1 "$GRAYWOLF_ADMIN_PASSWORD_FILE" || true)
		else
			say "graywolf: can't read GRAYWOLF_ADMIN_PASSWORD_FILE ($GRAYWOLF_ADMIN_PASSWORD_FILE)."
		fi
	elif [ -t 0 ]; then
		printf 'graywolf admin username [admin]: '
		IFS= read -r user || user=
		printf 'graywolf admin password (8+ characters, no spaces): '
		trap 'stty echo; restart_on_failure' EXIT
		stty -echo
		IFS= read -r pw || pw=
		stty echo
		trap restart_on_failure EXIT
		say ""
	fi
	user=${user:-admin}
	hint="sudo -u $owner graywolf auth set-password --user $user -config $GW_DB"
	if [ ${#pw} -lt 8 ] || printf '%s' "$pw" | grep -q '[[:space:]]'; then
		say "graywolf: no admin login made (no password given, or it was short or had spaces). Make one before using graywolf's web UI:"
		say "  $hint"
		unset pw
		return 0
	fi
	# graywolf may still be setting its database up: try a few times.
	i=0
	out=
	while [ $i -lt 5 ]; do
		if out=$(printf '%s\n' "$pw" | run_as "$owner" "$gw_bin" auth set-password --user "$user" -config "$GW_DB" 2>&1); then
			GW_ADMIN_OK=yes
			break
		fi
		sleep 2
		i=$((i + 1))
	done
	unset pw
	if [ "$GW_ADMIN_OK" = yes ]; then
		say "graywolf admin login '$user' created."
	else
		say "graywolf: couldn't create the admin login: $out"
		say "  Make it with: $hint"
	fi
}

GW_FRESH=no
if [ -n "$GW_DEB" ]; then
	install_graywolf_deb || die "graywolf: --graywolf-deb $GW_DEB wasn't installed (see above)"
elif graywolf_installed; then
	gwv=$(dpkg-query -W -f='${Version}' graywolf 2>/dev/null || true)
	if [ -n "$gwv" ]; then
		warn_version "$gwv"
	fi
elif [ "$GW_INSTALL" = yes ]; then
	# Before the app is stopped for an upgrade: downloads can take a while.
	install_graywolf || true
fi
pi_zero_notes

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
trap 'exit 130' INT TERM
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

if [ "$GW_FRESH" = yes ]; then
	graywolf_admin
fi

# create_gw_login: create (or reset) the app's own graywolf login with a
# random password, using graywolf's CLI as the owner of its database, and
# save the password for the app. Only for the app's dedicated login: an
# operator who pointed GW_USER at their own login is never touched.
create_gw_login() {
	if [ "$GW_FRESH" = yes ] && [ "$GW_ADMIN_OK" != yes ]; then
		say "Not creating the app's graywolf login yet: make your graywolf admin login first (above), then run this script again."
		return 1
	fi
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
		trap 'exit 130' INT TERM
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
