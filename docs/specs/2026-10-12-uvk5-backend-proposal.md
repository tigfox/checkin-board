# Proposal: UV-K5/UV-K1 radios as a second messaging backend

Status: **draft for review**. Written 2026-10-12, then revised the same
day for the firmware's 67-character messages. Nothing is built yet. The
firmware changes it asks for are in
[`2026-10-12-uvk5-firmware-requests.md`](2026-10-12-uvk5-firmware-requests.md).

## Summary

A node can send and receive race traffic either through **graywolf**
(today: a Pi, an AIOC and a radio) or through a **UV-K5 V3 / UV-K1
running the custom APRS firmware**, plugged into the Pi by USB-C. The
radio does the modem work itself, so its node needs no graywolf and no
AIOC, and even a Pi Zero has CPU to spare. Both kinds of node share one
frequency and one race. They all speak RC1 in ordinary APRS messages, so
they interoperate on air with no change to the race protocol.

Since the firmware now takes **67-character messages** (revision
2026-10-11), RC1 traffic fits as it is: the same batch sizes, codes and
settings as graywolf nodes. What's left is the radio's thinner messaging
API:

- **One message in flight, with no "sent" signal.** The node sends a
  message, waits for its ack (or time-out), then sends the next.
- **Acks can't be matched to messages exactly.** The radio picks each
  message's line number and doesn't report it.
- **Drops are silent:** after 60 s of busy channel, or with no callsign,
  APRS off, out of band or low battery.
- **A fixed digipeater path** (`WIDE1-1,WIDE2-1`) on every message and
  ack.

It works with the firmware as it is today, but slowly, and with some
uncertainty. The firmware requests (items 1-3 there) make it exact, and
items 4-5 make it faster.

Source: `uv-k1-k5v3-firmware-custom/claude/aprs-messaging-api.md`
(revision 2026-10-11), checked against the firmware (`aprs_msg.c`,
`aprs_rxinfo.c`, `aprs_station.c`, `aprs_beacon.c`).

## What the API means for checkin-board

| Radio behaviour | Effect | What checkin-board does |
|---|---|---|
| 1-67 characters, over 67 refused | RC1 fits: same `max_text_len` (67) as graywolf nodes; no race-wide limit | Nothing special; the backend reports MaxText 67 |
| One message slot; a second send **replaces** an unsent one; no SENT line | Sending the next message too early silently loses the first | A single sender per node: stop and wait for each message's ack (or time-out), at least 6 s apart. HQ's gap requests and link checks queue behind reports |
| Line number `{nn}` chosen by the radio, not reported | An ack can't be tied to a message for certain. A late ack of an earlier copy could confirm the wrong batch | With one message in flight, an ack from the peer during the wait counts as ours. A wrongly confirmed batch is still recovered by RC1's gap requests (heartbeats show the hole), just later. Exact with firmware request 1 |
| Busy channel 60 s: **everything pending dropped silently**; silent drops for no callsign, APRS off, out of band, low battery | "Sent" and "failed" can't be known | Delivery only from acks (as today with graywolf's retries off). Check radio does a pre-flight check (callsign, APRS on). Reasons with firmware requests 2 and 6 |
| Auto-acks messages to it, ids of up to 5 letters/digits (graywolf's `{050`: fine); **one ack slot** | Two messages to a radio within ~5 s: the first one's ack is replaced and lost, so the sender retries | Fine for a checkpoint, which hears little. Costly for a **radio HQ**, which hears every checkpoint: HQ stays on graywolf until firmware request 3 |
| Min 5 s between its transmissions; acks go first | Caps the node's rate | Part of the sender pacing |
| Every message and ack goes via `WIDE1-1,WIDE2-1` (hard-coded) | The race's path setting can't apply. More airtime, and race traffic carried off the course | Nothing (the radio decides). Firmware request 5 |
| Inbound: `APRSRAW:` hex lines (with monitor on); no dedup | The node must decode AX.25 and drop duplicates itself | AX.25 address and APRS message parsing, plus a duplicate filter like graywolf's (source, msgid, text, window) |
| Session `ts` handshake; port held by one program | Restarts or unplugs need a fresh handshake | Reconnect loop: handshake, settings check, monitor on |
| Callsign readable from the settings record; set only through "write all settings" | | Read it (station callsign, own-call checks); set it on the radio by hand until firmware request 7 |
| No signal level, no frame counts | | Link check works without levels (already optional); Check radio's "packets decoded" from the node's own count of decoded frames |

**Throughput.** One message is in flight at a time, at least 6 s apart,
waiting for the ack. That's about 6-10 messages a minute when the link is
clean. Each 67-character report carries several bibs (about 5 in the same
minute), so a radio checkpoint keeps up with **roughly 30-50 bibs a
minute** steadily, and slows to a few messages a minute when acks are
lost. That's comfortably above our 20-a-minute test burst. HQ's gap
requests and link checks to that node share the same slot.

## Design in checkin-board

The engines (checkpoint, HQ, link check, inbox reader) talk to graywolf
through a few small interfaces (`app.Graywolf`). They stay unchanged.
Under them goes a **radio backend that behaves like graywolf's message
store**:

1. **`internal/uvk5`: the serial link.**
   - Open the USB-C port in pure Go (`x/sys/unix` termios, DTR/RTS low).
   - Framing: obfuscation, CRC-16/XMODEM, replies marked `FF FF`, with
     the spec's worked example as a golden test.
   - Handshake, settings read (`APR1`, APRS on, callsign, SSID) and
     monitor on.
   - Parse monitor lines mixed in with binary frames.
   - Decode AX.25 and APRS messages and acks from `APRSRAW:`.
   - Reconnect after an unplug or radio restart.
   - Use firmware request features when the radio has them (version or
     capability check), falling back to the conservative mode.
2. **`internal/radiobackend`: graywolf's message model.**
   - Our own message rows in SQLite (migration 0009): ids, direction,
     status queued/sent/acked/rejected/failed, a change cursor and an
     event stream.
   - It implements `SendMessage`, `GetMessage`, `CatchUp`,
     `StreamEventsWithOpen`, `MarkRead`, `DeleteMessage`, `ListPackets`
     (RX frames) and `MessagePreferences` (MaxText 67).
   - graywolf's per-peer retry preferences become a no-op.
   - The single sender enforces pacing and the one-slot rule, matches
     acks, and dedups inbound messages.
3. **Configuration:**
   - Choose the backend per node: `CB_BACKEND=graywolf|uvk5` and
     `CB_UVK5_PORT` (default: the first `/dev/ttyACM*` that answers the
     handshake).
   - The Station page shows the backend.
   - graywolf-only parts are hidden on radio nodes: the graywolf card,
     the graywolf section of race configs, and Restore graywolf settings.
   - The race path setting doesn't apply on radio nodes (the radio uses
     its own); the Station page says so.
4. **Health:**
   - Check radio for radio nodes: port present, handshake, firmware
     version, `APR1`, APRS on, callsign set (not `N0CALL`) and as
     expected, monitor running. Battery and transmit inhibit come with
     firmware request 6.
   - The cable watcher holds reports while the radio is unplugged, like
     today's AIOC watch.
5. **Install:**
   - `install.sh --backend uvk5` skips graywolf.
   - The service gets the serial device (`DeviceAllow`, `dialout`) under
     its sandbox.
   - `stationsetup` gets a `backend` column, to check each node is what
     the CSV says. It sets the callsign too once firmware request 7
     exists.
6. **Tests:**
   - Codec golden tests from the spec.
   - A fake radio speaking the serial protocol with the firmware's limits
     (one slot, one ack slot, 5 s gap, 60 s give-up, 67 characters, fixed
     path).
   - The fake RF channel carries both kinds of node, so the simulations
     prove exactly-once delivery in mixed races, including lost acks and
     replaced messages.
   - Then `benchrace` on air with a real radio. It works unchanged, since
     it uses the app's web API.

Rough size: the serial link and codec are about a phase. The backend
with its message store, sender and tests is the largest piece. Health,
config, install and docs make one more. That's similar to phases 5-7
together.

## Decisions (answered 2026-10-12)

Answered by the user and the firmware maintainer; see the
[plan](2026-10-12-uvk5-backend-plan.md). In short: checkpoints first,
firmware requests 0-8, 10 and 11 at queue depth 4 (9 dropped), callsign
read from the radio and set through request 8 when it exists, USB-C
only, frequency set on the radio. The questions as first asked:

1. **Which roles use the radio?** Recommendation: **checkpoints only**,
   with HQ on graywolf, until the radio can hold several pending acks
   (firmware request 3).
2. **Firmware first?** Recommendation: ask for **requests 1-3** before
   building the client. They decide how the sender and ack matching are
   written. The rest can follow.
3. **Callsign on radio nodes:** set by hand on the radio, with
   checkin-board checking it (recommended until firmware request 7), or
   wait for the command so `stationsetup` sets it.
4. **Throughput:** a radio checkpoint handles roughly 30-50 bibs a minute
   on a clean link, much less when acks are lost. OK for your busiest
   checkpoints?
