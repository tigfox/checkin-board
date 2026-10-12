# Plan: UV-K5/UV-K1 radio backend (phase 17)

Status: **planned** (2026-10-12). It follows
[`2026-10-12-uvk5-backend-proposal.md`](2026-10-12-uvk5-backend-proposal.md),
the firmware maintainer's estimate
(`uv-k1-k5v3-firmware-custom/claude/aprs-firmware-request-estimate.md`)
and checkin-board's reply at its end.

## Agreed

**Firmware.** All requests except 9 (set frequency) will be built, at
queue depth 4, in this order:

1. 0 (capabilities `0x0712`), 1 (line number, free slots and refusal
   reason in `0x0701` bytes 5-7), 2 (SENT/DROP lines with reasons);
2. 3 (4 pending acks), 4 (4-deep queue; refuse with "full", never
   replace);
3. 7 (status command) and 11 (frame counters);
4. 8 (set callsign/SSID);
5. 5 (path setting, messages and acks), 10 (`APRSRSSI:` line straight
   after its `APRSRAW:`), 6 (gap 2-10 s).

**Decisions:**

- Radios on **checkpoints only** at first; HQ stays on graywolf until
  firmware 3 and 4 are bench-tested.
- Radios connect by **USB-C only**.
- The frequency is set **on the radio** and checkin-board only reads it.
- Retries stay in checkin-board.
- Messages are **67 characters**, the same as graywolf nodes, so RC1
  doesn't change.

**Compatibility.** checkin-board supports **both** today's firmware
(revision 0, read from `0x0712`) and the new one:

- **Revision 0, conservative mode:** strict stop-and-wait, acks matched
  by timing, no "sent" status. It works with the radios as flashed today.
- **Exact mode:** acks matched by line number, sends paced by
  SENT/free slots, failures with reasons.

The mode is chosen from the capability bits at connect time, so nodes
pick up firmware features as radios are reflashed.

## Architecture

The engines (checkpoint, HQ, link check, inbox reader, ops) only see the
`app.Graywolf` interfaces. The radio backend implements them, so **no
engine changes**.

```
checkpoint / hq / linkcheck / inbox / ops
              |  app.Graywolf (SendMessage, GetMessage, CatchUp, StreamEvents, ...)
     +--------+---------+
 graywolf.Client     radiobackend.Backend --- SQLite rows (migration 0009)
 (REST, today)              |  sender (paced, one slot or 4) / receiver (dedup) / ack matcher
                       uvk5.Session --- uvk5.Port (USB-C serial, pure Go)
```

## Phases

Each phase follows the project loop: tests first, `-race`, ARMv6 build,
review, spec status, commit.

### 17a: the radio link (`internal/uvk5`)

- **Codec:**
  - Frame build and parse: `AB CD`, size, XOR key, CRC-16/XMODEM on
    commands, `FF FF` marker on replies, `DC BA`.
  - Golden tests from the spec's worked examples, plus a fuzz target for
    the reply parser.
- **Stream demux:** binary frames mixed with `\r\n` text lines
  (`APRSRAW:`, `APRS:`, `DIGI:`; later `SENT:`, `DROP:`, `APRSRSSI:`).
- **AX.25 / APRS:**
  - Decode addresses (callsign, SSID, last-address bit) and the path.
  - Decode message, ack and rej bodies, and message ids of up to 5
    characters.
- **Port:** open `/dev/ttyACM*` in raw mode with `x/sys/unix` termios
  (pure Go; DTR/RTS low for safety). Find the radio by handshake when
  `CB_UVK5_PORT` isn't set.
- **Session:**
  - Handshake with a random non-zero `ts`, and read the firmware
    version.
  - `0x0712` capabilities; revision 0 when it's absent.
  - Settings `0x070C`: `APR1`, APRS on, callsign/SSID.
  - Monitor on.
  - Send `0x0700`, with the reply bytes used only when the capability
    bits say so.
  - Reconnect with backoff after an unplug or `ts` refusal.
- **Fake radio (`internal/uvk5/fake`):**
  - It speaks the serial protocol over an `io.ReadWriter` and behaves
    like the firmware at revision 0: one slot that a second send
    replaces, one ack slot, 5 s gap, 60 s busy give-up, silent drops,
    the fixed path, auto-acks.
  - It also covers the agreed revision: the 4-deep queue, line numbers,
    SENT/DROP, 4 acks, status, RSSI.
  - It attaches to the simulated RF channel (`gwfake.Radio`).
- **Exit:** codec coverage ≥ 90%; fuzzing clean.

### 17b: the backend (`internal/radiobackend`)

- **Message store (migration 0009):**
  - Rows: id, direction, peer, text, msg id (line number when known),
    status (queued, sent, acked, rejected, failed), times, unread.
  - A change cursor and an event fan-out, with the same shapes as
    graywolf's `Message`, `MessageChange` and `Event`.
- **`app.Graywolf` implementation:**
  - `SendMessage` queues a row; `GetMessage`, `CatchUp`,
    `StreamEventsWithOpen` and `MarkRead` work as graywolf's do.
  - `DeleteMessage` (cleanup).
  - `ListPackets` returns RX frames, with RSSI from firmware 10.
  - `MessagePreferences` reports MaxText 67.
  - Peer preferences are no-ops; `StationConfig` reads the callsign from
    the radio.
- **Sender:**
  - Conservative mode: one message at a time, 6 s apart, the next sent
    only after an ack or a time-out (30-60 s, scaled by the path).
  - Exact mode: up to the free slots; "sent" on SENT; "failed" with a
    reason on DROP or a refusal.
  - Acks go to the engines as status changes, exactly as graywolf's do,
    so the checkpoint engine's retry ladder works as it is.
- **Ack matching:**
  - Exact mode: by peer and line number.
  - Conservative mode: an ack (or rej) from the peer while one message
    is outstanding.
  - A late ack after a time-out is ignored, and logged.
- **Receiver:** messages to our call become inbound rows. Duplicates
  (source, msg id, text, 30 s window) are dropped. The radio has
  already acked them.
- **Exit:** the engines' own tests run against the backend and the fake
  radio (same scenarios as against `gwfake`).

### 17c: mixed-race simulation (`internal/sim`)

- Nodes of either kind on one simulated channel, with the radio's
  constraints.
- Exactly-once delivery in these scenarios:
  - a graywolf HQ with radio checkpoints, in both modes;
  - a mass start of 100 runners in 3 minutes;
  - 20% loss;
  - an ack replaced at the radio;
  - a message replaced in conservative mode;
  - a 60 s busy-channel drop;
  - a radio unplugged mid-race (hold, then resume);
  - an HQ gap request answered by a radio checkpoint.
- A latency table for radio checkpoints in each mode (spec 9b style).
- **Exit:** all scenarios exactly once. Latency recorded in the spec
  against the proposal's estimate (30-50 bibs a minute steady).

### 17d: app integration

- **Configuration:**
  - `CB_BACKEND=graywolf|uvk5` (default graywolf) and `CB_UVK5_PORT`.
  - `main` builds the chosen backend; startup logs the backend,
    firmware version and capability bits.
- **Station page:**
  - A "Messaging backend" line.
  - graywolf-only parts are hidden on radio nodes: the graywolf card,
    the graywolf section of race configs (shown as "not used on this
    node"), Restore graywolf settings, and the 24 kHz/Pi Zero notes.
  - The path setting says the radio uses its own path (firmware 5 adds
    one).
- **Callsign:** read from the radio. Settable from the Station page and
  `stationsetup` once the capability bit for request 8 is set; until
  then the page says "set it on the radio".
- **Check radio** for radio nodes:
  - always: port, handshake, firmware revision, `APR1`, APRS on,
    callsign set and not `N0CALL`, monitor running;
  - with firmware 7: battery, frequency and band, transmit inhibit
    reason, busy channel;
  - with firmware 11: decoded, damaged and sent counts.
- **Cable watch:** hold reports while the radio is unplugged or the
  session is down (it reuses `cablewatch`'s hold).
- **Race config / `stationsetup`:**
  - The graywolf part is skipped on radio nodes, with a note.
  - A `backend` column checks each node is what the CSV says.
  - The callsign is set through firmware 8 when available.
- **Exit:**
  - web tests and browser e2e for a radio node page;
  - `testnode` gains a radio-backed node, so `benchrace` and
    `stationsetup` tests can run mixed.

### 17e: install and docs

- **Installer:** `install.sh --backend uvk5`:
  - skips graywolf entirely;
  - sets `CB_BACKEND` in the env file;
  - gives the service the serial device under its sandbox
    (`DeviceAllow=char-ttyACM rw`, the `dialout` group,
    `PrivateDevices` relaxed only for this);
  - adds a udev rule for a stable `/dev/uvk5` name if the radio's USB
    id allows it.
- `make installtest` cases for it.
- **Docs:**
  - operator guide (setting up a radio node);
  - station guide (radio checks, its fixed path, frequency on the
    radio);
  - README (backends);
  - race-day runbook rows for radio nodes;
  - `example_stations.csv` gets a `backend` column.

### 17f: on-air bench

- An opt-in **radio contract test** (like `make contract`) against a real
  radio: handshake, settings, send and ack against a graywolf node,
  inbound dedup, unplug and reconnect, and the agreed revision's
  features as they land.
- **Bench:** a graywolf HQ, one or two radio checkpoints and a graywolf
  checkpoint on low power. Run `benchrace` (100 runners) and a link check
  between each pair.
- Measure throughput and latency on the radio checkpoints in each mode.
- Results go in a test report.

## Order and dependencies

| Phase | Needs firmware | Can start |
|---|---|---|
| 17a | none (revision 0; new formats from the reply) | now |
| 17b | none (both modes on the fake radio) | after 17a |
| 17c | none | after 17b |
| 17d | 7, 8, 11 only for their extra checks | after 17b |
| 17e | none | with 17d |
| 17f | a radio with each revision as it lands | after 17d and 17e, plus a radio on the bench |

Until the firmware's spec revision is published, 17a-17c follow the
formats in checkin-board's reply (`0x0712`, `0x0701` bytes 5-7,
`SENT:`/`DROP:`/`APRSRSSI:` lines). When it lands, golden tests are
added from its worked examples and any difference is fixed in the codec
only.

## Risks

- **Conservative mode can mismatch acks** after a time-out. That's
  contained: a late ack is ignored rather than applied to the next
  message, and RC1's gap requests recover a batch HQ never got.
  Exactness comes with firmware 1-2.
- **The firmware's RAM** (~0.9 KB spare): the maintainer budgets about
  0.3 KB at depth 4. The stack gauge after the queue lands is the check.
  If it's short, the scan progress bar and scan-range skip go first
  (unused); the RX/TX log and K5Viewer stay.
- **USB serial under the systemd sandbox** on Raspberry Pi OS: checked
  in 17e, on the bench.
- **Shared frequency:** a radio checkpoint's fixed `WIDE1-1,WIDE2-1` path
  (until firmware 5) costs airtime in a race where graywolf nodes go
  direct.
