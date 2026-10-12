#!/bin/sh
# Runs inside an armhf Debian container (make installtest): exercises
# install.sh's graywolf package handling on a real dpkg/apt, as on a Pi
# Zero. systemd is stubbed (no services start); uname -m reports armv6l.
# The graywolf packages are generated: install.sh only reads their
# metadata.
set -u
fail=0
check() { # check DESCRIPTION COMMAND...
	what=$1
	shift
	if "$@"; then echo "ok:   $what"; else echo "FAIL: $what"; fail=1; fi
}
# apt runs package scripts with DPkg::Path (no /usr/local), so systemctl
# is stubbed in /usr/bin, where a real node has it.
printf '#!/bin/sh\necho "systemctl $*" >> /tmp/systemctl.log\ncase "$1" in is-active|is-enabled) exit 3;; esac\nexit 0\n' > /usr/bin/systemctl
printf '#!/bin/sh\n[ "${1:-}" = -m ] && { echo armv6l; exit 0; }\nexec /usr/bin/uname "$@"\n' > /usr/local/bin/uname
chmod +x /usr/bin/systemctl /usr/local/bin/uname

# fakedeb NAME VERSION: a package with graywolf's postinst behaviour.
fakedeb() {
	d=/tmp/pkg-$1-$2
	mkdir -p "$d/DEBIAN" "$d/usr/bin"
	printf '#!/bin/sh\necho fake\n' > "$d/usr/bin/$1"
	chmod 755 "$d/usr/bin/$1"
	printf 'Package: %s\nVersion: %s\nArchitecture: armhf\nMaintainer: test\nDescription: test\n' "$1" "$2" > "$d/DEBIAN/control"
	printf '#!/bin/sh\nset -e\nsystemctl daemon-reload\nsystemctl enable %s.service\n' "$1" > "$d/DEBIAN/postinst"
	chmod 755 "$d/DEBIAN/postinst"
	dpkg-deb --root-owner-group --build "$d" "/tmp/$1_$2_armhf.deb" >/dev/null
}
fakedeb other 1
fakedeb graywolf 0.14.14
fakedeb graywolf 0.14.14+4978244d.armv6buf

cd /tmp && tar xzf /t/bundle.tar.gz && cd checkin-board-*/ || exit 1
export GRAYWOLF_ADMIN_USER=admin GRAYWOLF_ADMIN_PASSWORD_FILE=/tmp/adminpw
printf 'test-only\n' > /tmp/adminpw
gwver() { dpkg-query -W -f='${Version} ${Status}' graywolf 2>/dev/null; }

out=$(./install.sh --graywolf-deb /tmp/other_1_armhf.deb </dev/null 2>&1); rc=$?
check "a non-graywolf package is refused (exit 2)" [ $rc -eq 2 ]
check "...before any change (no service user)" sh -c '! id checkin-board >/dev/null 2>&1'

out=$(./install.sh --graywolf-deb /tmp/graywolf_0.14.14_armhf.deb </dev/null 2>&1)
check "a graywolf without the fixes installs" [ "$(gwver)" = "0.14.14 install ok installed" ]
check "...and the Pi Zero warning shows" sh -c "printf '%s' \"\$1\" | grep -q 'WARNING: this is a Pi Zero'" _ "$out"

out=$(./install.sh --graywolf-deb /tmp/graywolf_0.14.14+4978244d.armv6buf_armhf.deb </dev/null 2>&1)
check "the fixed build replaces it" [ "$(gwver)" = "0.14.14+4978244d.armv6buf install ok installed" ]
check "...with the has-the-fixes note" sh -c "printf '%s' \"\$1\" | grep -q 'has the Pi Zero fixes'" _ "$out"
check "...and graywolf restarted" grep -q 'restart graywolf' /tmp/systemctl.log

out=$(./install.sh </dev/null 2>&1)
check "a plain re-run keeps the fixed build" [ "$(gwver)" = "0.14.14+4978244d.armv6buf install ok installed" ]

printf '{"format":"checkin-board-race/1","race":{"name":"Test"}}\n' > /tmp/test-race.json
out=$(./install.sh --race-config /tmp/test-race.json </dev/null 2>&1)
f=/var/lib/checkin-board/race-configs/test-race.json
check "--race-config adds the file to the node" [ -f "$f" ]
check "...owned by the service user, mode 640" [ "$(stat -c '%U %a' "$f" 2>/dev/null)" = "checkin-board 640" ]

jc=/etc/systemd/journald.conf.d/60-checkin-board.conf
check "logs kept in RAM, 16-64 MB" sh -c "grep -qx 'Storage=volatile' $jc && grep -qxE 'RuntimeMaxUse=(1[6-9]|[2-5][0-9]|6[0-4])M' $jc"
check "graywolf's log rate capped" grep -qx 'LogRateLimitBurst=500' /etc/systemd/system/graywolf.service.d/60-checkin-board-logs.conf
check "...journald restarted once over all the re-runs" [ "$(grep -c 'restart systemd-journald' /tmp/systemctl.log)" = 1 ]

# A radio node (phase 17): UV-K5 on USB-C, no graywolf.
out=$(./install.sh --backend nope </dev/null 2>&1); rc=$?
check "an unknown backend is refused (exit 2)" [ "$rc" = 2 ]
out=$(./install.sh --uvk5-port /dev/ttyACM0 </dev/null 2>&1); rc=$?
check "--uvk5-port without --backend uvk5 is refused" [ "$rc" = 2 ]
out=$(./install.sh --backend uvk5 --uvk5-port '/dev/tty;rm' </dev/null 2>&1); rc=$?
check "...as is a port that isn't a plain path" [ "$rc" = 2 ]
: > /tmp/systemctl.log
out=$(./install.sh --backend uvk5 --uvk5-port /dev/ttyACM0 </dev/null 2>&1); rc=$?
env=/etc/checkin-board/checkin-board.env
dropin=/etc/systemd/system/checkin-board.service.d/50-uvk5.conf
check "--backend uvk5 installs (exit 0)" [ "$rc" = 0 ]
check "...sets CB_BACKEND and CB_UVK5_PORT" sh -c "grep -qx 'CB_BACKEND=uvk5' $env && grep -qx 'CB_UVK5_PORT=/dev/ttyACM0' $env"
check "...lets the service open the radio's port only" sh -c "grep -qx 'DeviceAllow=char-ttyACM rw' $dropin && grep -qx 'SupplementaryGroups=dialout' $dropin"
check "...and starts it with no graywolf password" grep -q 'start checkin-board' /tmp/systemctl.log
out=$(./install.sh --backend uvk5 </dev/null 2>&1)
check "a re-run keeps one CB_BACKEND line" [ "$(grep -c '^CB_BACKEND=' $env)" = 1 ]
out=$(./install.sh --backend graywolf </dev/null 2>&1)
check "--backend graywolf switches back and drops the device access" sh -c "grep -qx 'CB_BACKEND=graywolf' $env && [ ! -e $dropin ]"
exit $fail
