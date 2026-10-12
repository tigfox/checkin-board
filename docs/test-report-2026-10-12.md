# Test report: node load, Pi Zero W vs Pi 3 (2026-10-12)

Phase 12 (h), feedback 2026-10-09 items 13 (Pi Zero CPU budget) and 14
(hardware choice). checkin-board `63f2006`.

## Method

For each node, a script on the operator's computer
(`scratchpad/loadtest.py`, not in the repo) did the following:

1. Restarted the app and timed how long until it answered HTTP.
2. Left the node idle for 2 minutes.
3. Opened the race and logged **200 bibs at 20 a minute** (one every
   3 s, the soak test's burst rate) through the keypad API, as a phone
   does.
4. Reset the race afterwards; each reset made a backup.

A sampler on the node logged total CPU (`/proc/stat`) and each process's
memory every 10 s. The app's peak memory is its `VmHWM` since the
restart.

| | 10.0.1.183 | 10.0.1.88 |
|---|---|---|
| Hardware | Pi Zero W (1 core, ARMv6, 427 MB) | Pi 3 Model B (4 cores, arm64, 905 MB) |
| Role | checkpoint CTL | HQ (FIN) |
| graywolf | 0.14.14 Pi Zero build | 0.14.14 release |
| Panel | e-ink panel running | none |
| Radio | **AIOC missing** (dropped off USB, feedback 2026-10-11 item 1): the modem had no audio to process, and reports were held | no radio channel |

**Caveat:** on neither node did graywolf's modem demodulate audio. On a
Pi Zero with a working AIOC at 24 kHz, the modem alone used about 46% of
the CPU (feedback 2026-10-09). Add that to the Pi Zero figures below for
race day. No messages went on air, so graywolf's own sending work isn't
included either.

## Results

| | Pi Zero W | Pi 3 |
|---|---|---|
| App answers HTTP after a restart | 2.0-2.2 s | 1.2 s |
| Keypad save, median / max (200 bibs) | 84 ms / 399 ms (a first run: 73 / 1693 ms) | 38 ms / 256 ms |
| Total CPU, idle (no modem audio) | 23% | 0.8% (of 4 cores) |
| Total CPU, during the burst | 20% avg, 24% max | 0.9% avg, 1.2% max |
| App memory, peak | 23.6 MB | 29.3 MB |
| Panel memory, peak | 18.2 MB | — |
| graywolf memory, peak | 36.9 MB | 37.1 MB |
| graywolf-modem memory | 5.0 MB | 3.0 MB |
| Memory available, lowest | 267 MB | 705 MB |

## Findings

- **The race load is negligible on both.** A 20-bibs-a-minute burst made
  no measurable difference to the Pi Zero's CPU: 20% average against 23%
  idle, which is within the noise. The app averaged 5.3% of the Pi
  Zero's CPU over the whole run. The CPU budget is graywolf's modem, not
  checkin-board.
- **Pi Zero race-day estimate:** about 23% (everything else) + about 46%
  (modem at 24 kHz) ≈ **70%**. That's under the panel's 80% warning but
  with little headroom. This matches feedback item 13.
- **Memory is fine on both.** The whole stack is under 90 MB, and the Pi
  Zero kept 267 MB free.
- **The Pi 3 has ample headroom** (about 1% CPU without modem audio). It
  also runs the stock graywolf release on 64-bit, with no patched build
  or 24 kHz workaround. For item 14: **recommend a Pi 3B+ or Pi Zero 2 W
  (4 × Cortex-A53) for field nodes**. Keep Pi Zero W nodes only with the
  patched graywolf and 24 kHz, and measure one with a working radio
  before relying on it.
- **Panel:** it logs nothing per refresh, so refreshes during the burst
  couldn't be counted from the log. Its memory stayed flat.
- **Still to measure** (needs a working AIOC on a Pi Zero): CPU with the
  modem demodulating during a burst with the panel refreshing; and the
  journal over a quiet hour (item 9: the flood is gone, and the journal
  is now 28 MB in RAM).
