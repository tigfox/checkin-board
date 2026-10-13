# Plan: UV-K5/UV-K1 radio backend (phase 17)

Status: **17a-17e built** (2026-10-12); 17f (on-air bench) waits for the
radios. "As built" at the end. It follows
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

**One mode: the new firmware only** (user, 2026-10-12: no radio will run
today's firmware). At connect, checkin-board reads the capabilities
(`0x0712`) and **refuses a radio without requests 1-4** (line numbers,
SENT/DROP, several acks, the queue). Check radio says "firmware too old:
reflash". Features from later requests (status, set callsign, path,
RSSI, gap) are used when their bits are set.

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
  - `0x0712` capabilities; a radio without them (or without requests 1-4) is refused.
  - Settings `0x070C`: `APR1`, APRS on, callsign/SSID.
  - Monitor on.
  - Send `0x0700` and read the reply's line number, free slots and
    refusal reason.
  - Reconnect with backoff after an unplug or `ts` refusal.
- **Fake radio (`internal/uvk5/fake`):**
  - It speaks the serial protocol over an `io.ReadWriter` and behaves
    like the agreed firmware: a 4-deep queue that refuses when full,
    line numbers, SENT/DROP with reasons, 4 pending acks, auto-acks,
    5 s gap, 60 s busy give-up, status, RSSI, set callsign.
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
- **Sender:** up to the radio's free slots; "sent" on SENT; "failed"
  with a reason on DROP or a refusal.
  - Acks go to the engines as status changes, exactly as graywolf's do,
    so the checkpoint engine's retry ladder works as it is.
- **Ack matching:** by peer and line number. Line numbers wrap at 99,
  so a match also needs the message to be recent.
- **Receiver:** messages to our call become inbound rows. Duplicates
  (source, msg id, text, 30 s window) are dropped. The radio has
  already acked them.
- **Exit:** the engines' own tests run against the backend and the fake
  radio (same scenarios as against `gwfake`).

### 17c: mixed-race simulation (`internal/sim`)

- Nodes of either kind on one simulated channel, with the radio's
  constraints.
- Exactly-once delivery in these scenarios:
  - a graywolf HQ with radio checkpoints;
  - a mass start of 100 runners in 3 minutes;
  - 20% loss;
  - an ack replaced at the radio;
  - a 60 s busy-channel drop;
  - a radio unplugged mid-race (hold, then resume);
  - an HQ gap request answered by a radio checkpoint.
- A latency table for radio checkpoints (spec 9b style).
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
- Measure throughput and latency on the radio checkpoints.
- Results go in a test report.

## Order and dependencies

| Phase | Needs firmware | Can start |
|---|---|---|
| 17a | none (formats from the reply) | now |
| 17b | none (on the fake radio) | after 17a |
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

- **Formats aren't published yet.** The codec follows checkin-board's
  reply. Byte layouts the reply didn't fix (the capability and status
  replies, the set-callsign command) are checkin-board's assumptions,
  kept in one file (`internal/uvk5/proto.go`) and listed in the spec, so
  the firmware's revision changes only that file.
- **The firmware's RAM** (~0.9 KB spare): the maintainer budgets about
  0.3 KB at depth 4. The stack gauge after the queue lands is the check.
  If it's short, the scan progress bar and scan-range skip go first
  (unused); the RX/TX log and K5Viewer stay.
- **USB serial under the systemd sandbox** on Raspberry Pi OS: checked
  in 17e, on the bench.
- **Shared frequency:** a radio checkpoint's fixed `WIDE1-1,WIDE2-1` path
  (until firmware 5) costs airtime in a race where graywolf nodes go
  direct.

## As built (2026-10-12)

- The firmware's spec revision 2 replaced every assumed layout: capability
  reply (12 bytes, Ok first), feature bits in the spec's order, pinned
  reason codes (`session`, `empty`, `toolong`, `mode`, `invalid`, `store`
  added), the 20-byte status reply (10 Hz frequency units, 16-bit
  counters, free places, acks waiting), set-callsign data (13 bytes), and
  the settings record's path and gap bytes. Golden tests reproduce the
  spec's worked examples.
- The fake radio follows section 8's limits: a fifth pending ack is dropped
  and reported (`DROP:ack ... full`); on a busy channel only the item at
  the front times out, each with its own minute.
- The sim runs radio nodes in simulated time through
  `uvk5.DirectSession` (the fake radio in-process) and the backend's
  `Attach`/`Step`.
- Radio nodes ignore the race's digipeater path, the graywolf channel and
  TX timing (the radio has its own Path and Gap settings); race config
  sets only the callsign; Restore puts back only the callsign.
- The keypad and panel say **"Radio not connected"**, and the checkpoint
  holds its reports, while the radio's session is down.
- `CB_UVK5_PORT` accepts only serial device paths, and the port must open
  as a character device.
- Not done: the radio's RSSI isn't used by the link check yet (it's kept
  per sender in `State().LastRSSI`); answers to the firmware team's two
  questions are in the estimate document's reply.

## Radio type on the Station page (17g, 2026-10-13)

Radios get swapped in the field, so the radio type is no longer fixed at
install time (`install.sh --backend` is gone):

- One install sets up both: graywolf and the app's graywolf login, and
  the UV-K5's `/dev/ttyACM*` access (the `50-uvk5.conf` drop-in) on every
  node.
- **Station → Radio → Radio type** lists every kind from
  `internal/radiokind` (a registry: a future radio is an entry there, a
  backend, and a case in `startMessaging`). A kind that can't be used
  (graywolf without its login) is shown as not available, with why.
- The choice is saved in `node_radio` (migration 0010). `CB_BACKEND` is
  only the type until one is chosen. Choosing another type restarts the
  app in place (`syscall.Exec`, same PID, so systemd's restart limits
  don't apply); the page waits and reloads.
- Switching is safe mid-race:
  - Each kind keeps its own inbox cursor (`inbox_cursors`). The cursors
    are swapped at startup (`ActivateRadioKind`) before the inbox runs.
  - Radio message ids start at 2^40, so a batch bound to one backend's row
    never matches the other's. A row the new backend doesn't have answers
    404, and the batch goes out again as new; HQ dedups by
    (checkpoint, seq, text).
  - In the sims, a checkpoint goes graywolf → UV-K5 → graywolf with
    reports in flight, and HQ moves to a UV-K5. Every passage arrives
    exactly once.
- `stationsetup`'s `backend` column sets the type. The preview shows the
  switch; apply switches it, waits for the restart, previews again (what
  loading changes depends on the radio), then applies.
- Migration 0010 moves an existing UV-K5 node's message rows (and every
  reference to them) above 2^40.
- Post-race cleanup touches only the current radio's messages. Rows sent
  through the other type are counted ("left alone") and kept for a
  cleanup after switching back.
- A node whose chosen type needs graywolf but has no graywolf login (one
  first set up as a UV-K5 node) starts on the UV-K5 instead of failing,
  so its Station page stays reachable. The page says why.
- Not done: carrying the callsign over to a fresh radio automatically.
  **Check radio** flags N0CALL, and the Station page's Callsign sets it.

## First bench with real radios (17f, 2026-10-13)

Two UV-K5s (F4HWN v6.1.0, protocol revision 1) on the Pi Zero W nodes
10.0.1.183 and 10.0.0.65, over USB-C, in one room at low power.

Worked first time:
- the handshake, capabilities, settings, status and set-callsign
  commands;
- **Check radio**;
- switching both nodes from graywolf to the UV-K5 on the Station page
  (back in about 1 s);
- `SENT:` lines, `APRSRAW:` / `APRSRSSI:` lines, and the radio's own
  acks of short messages.

Found and fixed in checkin-board:
- **DTR.** Over USB-C the firmware writes monitor lines only while DTR
  is up (`cdc_acm_data_send_with_dtr`), but the spec says to open with
  DTR low. Command replies are sent regardless, so everything looked
  connected while no `SENT:` or `APRSRAW:` line ever arrived. The session
  now raises DTR after the handshake (never before: an AIOC keys PTT from
  DTR). Commit 16b9bee.
- **Local time.** The backend restamped `created_at` with the node's
  local clock, and the ack window compares SQLite text against UTC. On
  the EDT nodes no `SENT:` line or ack ever matched. Times are now stored
  as UTC, and `make test` runs in America/New_York. Commit 33b28c3.

Found in the firmware, reported in `aprs-firmware-request-estimate.md`:
- **Long messages aren't acked.** `APRS_RxInfo` looks for `{nn` only
  within the 43-character display text, so a message over about 34
  characters is never acked. A UV-K5 HQ therefore never confirms batches.
  The docs now say: UV-K5s for checkpoints only, HQ on graywolf.
- After a timed-out USB write the firmware stops sending monitor lines
  until DTR is toggled again (not seen on the bench).

Bench hardware:
- 10.0.1.183's USB drops ("disabled by hub (EMI?)") every time its radio
  transmits. It had the same fault with the AIOC.
- 10.0.0.65 decoded few of that radio's packets.
- Look at that radio's antenna and connection, and the node's USB
  cabling.

Bench race (UV-K5 checkpoint .65 → UV-K5 HQ .183, 20 runners):
- 16/20 at HQ, median 3 s. Every batch arrived, but none was acked
  (the firmware bug), so the checkpoint's window filled and the last
  batch never went out.

Still to do: the bench race with a **graywolf HQ** (the Pi 3, 10.0.1.88)
and UV-K5 checkpoints, once 183's hardware is sorted.
