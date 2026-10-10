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
exit $fail
