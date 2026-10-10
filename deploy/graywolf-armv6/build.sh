#!/usr/bin/env bash
# Build an ARMv6 (Pi Zero W / Pi 1) graywolf .deb with the audio-buffer
# patch, the way graywolf's release workflow builds its
# arm-unknown-linux-gnueabihf target. See README.md for why.
#
# Usage: build.sh GRAYWOLF_REPO REF [OUT_DIR]
#   GRAYWOLF_REPO  a local graywolf git checkout (it is not modified)
#   REF            the graywolf tag or commit to build, e.g. v0.14.15
#   OUT_DIR        where the .deb goes (default: current directory)
#
# Needs on the build host: git, docker, go, node/npm, python3.
set -euo pipefail

usage() { sed -n '2,12p' "$0" | sed 's/^# \{0,1\}//'; exit 2; }
[[ $# -ge 2 && $# -le 3 ]] || usage
REPO=$(cd "$1" && pwd)
REF=$2
OUT=$(mkdir -p "${3:-.}" && cd "${3:-.}" && pwd)
HERE=$(cd "$(dirname "$0")" && pwd)
PATCH=$HERE/armv6-audio-buffer.patch
CROSS_IMAGE=ghcr.io/cross-rs/arm-unknown-linux-gnueabihf:0.2.5
TARGET=arm-unknown-linux-gnueabihf
CACHE=${XDG_CACHE_HOME:-$HOME/.cache}/checkin-board-graywolf-armv6

for tool in git docker go npm python3; do
  command -v "$tool" >/dev/null || { echo "build.sh: $tool not found" >&2; exit 1; }
done

COMMIT=$(git -C "$REPO" rev-parse --verify "$REF^{commit}" | cut -c1-8)
WORK=$(mktemp -d)
trap 'rm -rf "$WORK"' EXIT
SRC=$WORK/src
mkdir -p "$SRC" "$WORK/pkg" "$CACHE/cargo" "$CACHE/rustup"

echo "== graywolf $REF ($COMMIT): export and patch"
git -C "$REPO" archive "$COMMIT" | tar -x -C "$SRC"
(cd "$SRC" && git init -q && git apply "$PATCH")
VERSION=$(tr -d '[:space:]' < "$SRC/VERSION")
STAMP=$COMMIT-armv6buf          # graywolf and its modem must report the same
DEBVER=$VERSION+$COMMIT.armv6buf

echo "== modem ($TARGET) in $CROSS_IMAGE"
docker run --rm --platform linux/amd64 -v "$SRC":/src -v "$CACHE/cargo":/cargo -v "$CACHE/rustup":/rustup \
  -e GRAYWOLF_VERSION="$VERSION" -e GRAYWOLF_GIT_COMMIT="$STAMP" "$CROSS_IMAGE" bash -euo pipefail -c "
    export DEBIAN_FRONTEND=noninteractive CARGO_HOME=/cargo RUSTUP_HOME=/rustup PATH=/cargo/bin:\$PATH
    dpkg --add-architecture armhf
    apt-get update -qq
    apt-get install -y -qq libasound2-dev:armhf libudev-dev:armhf unzip curl ca-certificates >/dev/null
    curl -sLo /tmp/protoc.zip https://github.com/protocolbuffers/protobuf/releases/download/v28.3/protoc-28.3-linux-x86_64.zip
    unzip -o -q /tmp/protoc.zip -d /usr/local bin/protoc
    command -v cargo >/dev/null || curl -sSf https://sh.rustup.rs | sh -s -- -y --profile minimal --default-toolchain stable --no-modify-path
    rustup target add $TARGET >/dev/null
    export PKG_CONFIG_ALLOW_CROSS=1 PKG_CONFIG_PATH_arm_unknown_linux_gnueabihf=/usr/lib/arm-linux-gnueabihf/pkgconfig
    export CFLAGS_arm_unknown_linux_gnueabihf='-idirafter /usr/include'
    cd /src && cargo build -q --release --target $TARGET --bin graywolf-modem
    # Refuse anything a Pi Zero can't run.
    R=arm-unknown-linux-gnueabihf
    M=target/$TARGET/release/graywolf-modem
    \$R-readelf -A \$M | grep -q 'Tag_CPU_arch: v6' || { echo 'modem is not ARMv6' >&2; exit 1; }
    n=\$(\$R-objdump -d \$M | awk -F'\t' 'NF>=3{print \$3}' | awk '{print \$1}' |
         grep -cxE 'movw|movt|dmb|dsb|isb|sdiv|udiv|ubfx|sbfx|bfi|bfc|rbit|mls|cbz|cbnz|it' || true)
    [ \"\$n\" = 0 ] || { echo \"modem has \$n ARMv7-only instructions\" >&2; exit 1; }
  "
install -m 755 "$SRC/target/$TARGET/release/graywolf-modem" "$WORK/pkg/graywolf-modem"
grep -q 'was earlier than get_trigger_htstamp' "$WORK/pkg/graywolf-modem" &&
  { echo "build.sh: modem still uses cpal 0.17 (silent transmit on 32-bit ARM)" >&2; exit 1; }

echo "== web UI"
(cd "$SRC/web" && npm ci --no-audit --no-fund --include=optional >/dev/null && npx vite build >/dev/null)

echo "== graywolf (GOARM=6)"
(cd "$SRC" && CGO_ENABLED=0 GOOS=linux GOARCH=arm GOARM=6 go build -trimpath \
  -ldflags "-w -s -X main.Version=$VERSION -X main.GitCommit=$STAMP" -o "$WORK/pkg/graywolf" ./cmd/graywolf)

echo "== package $DEBVER"
P=$WORK/deb
mkdir -p "$P/DEBIAN" "$P/usr/bin" "$P/usr/lib/systemd/system" "$P/etc/udev/rules.d"
install -m 755 "$WORK/pkg/graywolf" "$WORK/pkg/graywolf-modem" "$P/usr/bin/"
install -m 644 "$SRC/packaging/systemd/graywolf.service" "$P/usr/lib/systemd/system/"
install -m 644 "$SRC/packaging/udev/99-graywolf-cm108.rules" "$P/etc/udev/rules.d/"
install -m 755 "$SRC/packaging/scripts/postinstall.sh" "$P/DEBIAN/postinst"
install -m 755 "$SRC/packaging/scripts/preremove.sh" "$P/DEBIAN/prerm"
echo /etc/udev/rules.d/99-graywolf-cm108.rules > "$P/DEBIAN/conffiles"
DEB=graywolf_${DEBVER}_armhf.deb
docker run --rm -v "$P":/pkg -v "$OUT":/out debian:trixie-slim sh -euc "
  cd /pkg
  cat > DEBIAN/control <<CTL
Package: graywolf
Version: $DEBVER
Section: hamradio
Priority: optional
Architecture: armhf
Maintainer: Chris Snell
Installed-Size: \$(du -sk --exclude=DEBIAN . | cut -f1)
Homepage: https://github.com/chrissnell/graywolf
Description: APRS radio transceiver, digipeater, and Internet gateway
 Local ARMv6 build of graywolf $REF ($COMMIT) with a fixed 2048-frame
 audio period for the Pi Zero (checkin-board deploy/graywolf-armv6).
CTL
  find . -type f ! -path './DEBIAN/*' -exec md5sum {} + | sed 's|  \./|  |' > DEBIAN/md5sums
  dpkg-deb --root-owner-group -Zgzip --build /pkg /out/$DEB >/dev/null
"
echo "== done: $OUT/$DEB"
(cd "$OUT" && shasum -a 256 "$DEB")
