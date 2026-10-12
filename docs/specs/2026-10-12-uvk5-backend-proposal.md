# Proposal: UV-K5/UV-K1 radios as a second messaging backend

Status: **draft for review** (2026-10-12). Nothing is built yet.

## Summary

A node can send and receive race traffic either through **graywolf**
(today: a Pi, an AIOC and a radio) or through a **UV-K5 V3 / UV-K1
running the custom APRS firmware**, plugged into the Pi by USB-C. The
radio does the modem work itself, so its node needs no graywolf and no
AIOC, and a Pi Zero has plenty of CPU. Both kinds of node share the same
frequency and race: they all speak RC1 in ordinary APRS messages, so
they interoperate on air.

It's feasible, but this radio's messaging API is much thinner than
graywolf's. The main costs are:

- **30 characters per message**, against 67 today. That's about one bib
  per report, and the whole race has to use the shorter limit.
- **One message in flight at a time, with no "sent" signal.** The node
  sends one message, waits for its ack, then sends the next. That gives
  roughly **6-10 bibs a minute per radio node** at best, against the
  20-a-minute burst we test with. A backlog drains after the burst;
  nothing is lost.
- Some **firmware additions** (the spec's section 10 already offers them)
  make it exact and faster. They're strongly recommended before building
  the client.

Source: `uv-k1-k5v3-firmware-custom/claude/aprs-messaging-api.md`,
checked against the firmware (`aprs_rxinfo.c`, `aprs_station.c`,
`aprs_msg.c`).

## What the spec means for checkin-board

| Radio behaviour | Effect | What checkin-board needs |
|---|---|---|
| Text ≤ 30 bytes, cut silently | A report is "RC1 R CTL 12 @1234 4455/02" (26): one bib, sometimes two. A 6-character code with a 4-digit batch number (31) can't send even one | `max_text_len` allowed down to 30 (today's minimum is 67). Race-wide 30 when any node is a radio node. Codes of at most 5 characters in such races. Every RC1 sender (reports, heartbeats, gap requests, link-check replies) kept within it |
| One message slot; a second send **replaces** an unsent one; no SENT line | Sending the next message too early silently loses the first | A single sender per node: stop and wait for each message's ack (or time-out) before the next, at least 6 s apart. HQ's gap requests and link checks queue behind reports |
| Line number `{nn}` chosen by the radio, not reported | An ack can't be tied to a message for certain. A late ack of an earlier copy could confirm the wrong batch | With one message in flight, an ack from the peer during the wait is taken as ours. A wrongly confirmed batch is still recovered by RC1's gap requests, just later. Exact with firmware addition F1 |
| Busy channel 60 s: **everything pending dropped silently**; silent drops for no callsign, APRS off, out of band, low battery | "Sent" can't be known | Delivery only from acks (as today with graywolf's retries off). A pre-flight check before the race (below) |
| Auto-acks messages to it, ids up to 5 letters/digits (graywolf's `{050`: fine); **one ack slot** | Two messages to a radio within ~5 s: the first one's ack is replaced and lost, so the sender retries | Fine for a checkpoint, which hears little. Bad for a **radio HQ**, which hears every checkpoint: recommend HQ stays on graywolf, unless F3 |
| Min 5 s between its transmissions; acks go first | Caps the node's rate | Part of the sender pacing |
| Inbound: `APRSRAW:` hex lines (needs monitor on); no dedup | The node must decode AX.25 and drop duplicates itself | AX.25 address and APRS message parsing; a duplicate filter like graywolf's (source, msgid, text, window) |
| Session `ts` handshake; port held by one program | Restarts or unplugs need a fresh handshake | Reconnect loop: handshake, settings check, monitor on |
| Callsign read from the settings record; set only through "write all settings" | | Read it (station callsign, own-call checks). Setting it from checkin-board waits for a firmware command, so for now it's set on the radio |
| No audio level, no channel/PTT model | | Link check works without levels (they're already optional); a radio-specific Check radio |

## Firmware additions (spec section 10) to ask for first

- **F1** — the `0x0700` reply carries the line number used. Each ack
  then matches exactly one message.
- **F2** — `SENT:<to>{nn}` when a message goes on air, `DROP:<to>{nn}`
  when it's given up. The node then knows the slot is free, and can
  report "sent" and "dropped" like graywolf.
- **F3** — a queue of a few messages, and **at least two pending acks**.
  This allows pipelining, and a radio HQ that doesn't lose acks.

| Without them | With F1 and F2 | With F1, F2 and F3 |
|---|---|---|
| Strict stop-and-wait; acks matched by timing; ~6 bibs/min | Exact acks; next send as soon as SENT; ~10 bibs/min | Several in flight; close to the 5 s on-air limit; radio HQ practical |

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
2. **`internal/radiobackend`: graywolf's message model.**
   - Our own message rows in SQLite (migration 0009): ids, direction,
     status queued/sent/acked/rejected/failed, a change cursor and an
     event stream.
   - It implements `SendMessage`, `GetMessage`, `CatchUp`,
     `StreamEventsWithOpen`, `MarkRead`, `DeleteMessage`, `ListPackets`
     (RX frames, no level) and `MessagePreferences` (MaxText 30).
   - graywolf's per-peer retry preferences become a no-op.
   - The single sender enforces pacing and the one-slot rule, matches
     acks, and dedups inbound messages.
3. **Message length:**
   - Lower the `max_text_len` minimum to 30 (validation, race config,
     `stationsetup`), and set it race-wide in a mixed race.
   - Make every encoder honour it: link-check replies need trimming.
   - Refuse codes over 5 characters when it's 30.
4. **Configuration:**
   - Choose the backend per node: `CB_BACKEND=graywolf|uvk5` and
     `CB_UVK5_PORT` (default: the first `/dev/ttyACM*` that answers the
     handshake).
   - The Station page shows the backend.
   - graywolf-only parts are hidden on radio nodes: the graywolf card,
     the graywolf section of race configs, and Restore graywolf settings.
5. **Health:**
   - Check radio for radio nodes: port present, handshake, firmware
     version, `APR1`, APRS on, callsign set (not `N0CALL`) and as
     expected, monitor running.
   - The cable watcher holds reports while the radio is unplugged, like
     today's AIOC watch.
6. **Install:**
   - `install.sh --backend uvk5` skips graywolf.
   - The service gets the serial device (`DeviceAllow`, `dialout`) under
     its sandbox.
   - `stationsetup` gets a `backend` column, so it can set the race-wide
     30 characters and warn about long codes.
7. **Tests:**
   - Codec golden tests from the spec.
   - A fake radio speaking the serial protocol with the firmware's limits
     (one slot, one ack slot, 5 s gap, 60 s give-up, cut at 30).
   - The fake RF channel carries both kinds of node, so the simulations
     prove exactly-once delivery in mixed races, including lost acks and
     replaced messages.
   - Then `benchrace` on air with a real radio. It works unchanged,
     since it uses the app's web API.

Rough size: the serial link and codec are about a phase. The backend
with its message store, sender and tests is the largest piece. Health,
config, install and docs make one more. That's similar to phases 5-7
together.

## Decisions for you

1. **Which roles use the radio?** Recommendation: **checkpoints only**,
   with HQ on graywolf. A radio HQ loses acks whenever two checkpoints
   report within ~5 s, until F3.
2. **Firmware additions F1-F3 first?** Recommendation: **F1 and F2
   before building the client** (small changes per the spec), F3 later.
3. **Race-wide 30 characters, or per peer?** Race-wide is simple, but
   graywolf checkpoints then send about one bib per message too.
   Per-peer means HQ keeps each checkpoint's limit and uses 67 with
   graywolf nodes. Recommendation: **race-wide first**, per-peer later
   if airtime matters.
4. **Callsign on radio nodes:** set by hand on the radio, with
   checkin-board checking it (recommended for now), or a firmware command
   so `stationsetup` can set it.
5. **Throughput:** a radio checkpoint handles about 6-10 bibs a minute
   steadily. Bursts queue and drain. OK for your races' busiest
   checkpoints?
