# graywolf for Pi Zero W (ARMv6)

A Raspberry Pi Zero W runs graywolf only with these, found in the
two-node field test of 2026-10-09
([`docs/feedback-2026-10-09.md`](../../docs/feedback-2026-10-09.md),
items 8 and 10):

1. **cpal 0.18 or later** (graywolf commit `4978244d` and later). cpal
   0.17's timestamp check overflows on 32-bit ARM and drops every audio
   period: the radio keys up with a silent carrier.
2. **A fixed 2048-frame audio period** (`armv6-audio-buffer.patch`). By
   default the AIOC gets a 241-frame period and a two-period ring, 10 ms of
   slack, and the single-core Zero overruns: received packets fail FCS.
3. **24 kHz audio**, set in graywolf after installing (below). At 48 kHz
   the Zero's demodulator can't keep up and silently drops audio.

graywolf's releases don't include the patch, and we don't send it
upstream: rebuild with `build.sh` for each graywolf update.

## Build

```sh
deploy/graywolf-armv6/build.sh ~/src/graywolf v0.14.15 dist/
```

- Arguments: a local graywolf checkout (not modified), the tag or commit
  to build, and the output directory.
- Needs git, docker, go, node/npm and python3 on the build host. The
  modem builds in the cross-rs image graywolf's release workflow uses
  (`ghcr.io/cross-rs/arm-unknown-linux-gnueabihf:0.2.5`, amd64, emulated on
  Apple silicon); the Rust toolchain and crates are cached under
  `~/.cache/checkin-board-graywolf-armv6`.
- It refuses to package a modem that isn't ARMv6, has ARMv7-only
  instructions, or still has cpal 0.17's failing check.
- Output: `graywolf_<version>+<commit>.armv6buf_armhf.deb`. graywolf and its
  modem both report `<commit>-armv6buf`, so the running version shows the
  patched build.

If the patch no longer applies to a new graywolf version, update it
(`graywolf-modem/build.rs`, `src/audio/mod.rs`, `src/audio/soundcard.rs`,
`src/modem/tx_worker.rs`) and keep its tests passing: on the host and on
`arm-unknown-linux-gnueabihf` under qemu (`period_matches_build_target`
proves the `gw_armv6` flag is set only there).

## Install on a node

```sh
scp graywolf_*_armhf.deb pi@NODE:/tmp/
ssh pi@NODE 'sudo mkdir -p /tmp/gw-rollback && sudo cp -p /usr/bin/graywolf /usr/bin/graywolf-modem /tmp/gw-rollback/ &&
  sudo dpkg -i /tmp/graywolf_*_armhf.deb && sudo systemctl restart graywolf && rm /tmp/graywolf_*_armhf.deb'
```

Install checkin-board with `install.sh --no-graywolf` on these nodes, so
the installer never replaces the patched graywolf with a release.

Then set the AIOC audio to 24 kHz. graywolf's web UI can't (its sample
rate list is 8000/16000/44100/48000); use the API script in
`docs/feedback-2026-10-09.md` ("Pi Zero: setting graywolf's audio to
24 kHz"). Check:

- `sudo journalctl -u graywolf -n 50 | grep "buffer 2048"` shows
  "input buffer 2048 frames/period";
- `grep -E 'rate|period_size' /proc/asound/card0/pcm0c/sub0/hw_params`
  shows `rate: 24000` and `period_size: 2048`;
- no `cpal ... stream` errors in graywolf's log.
