# Firmware requests: UV-K5 V3 / UV-K1 APRS messaging for checkin-board

To: the maintainer of `uv-k1-k5v3-firmware-custom` (branch `aprs`).
From: checkin-board, 2026-10-12.
Context: checkin-board is a race-checkpoint system. Its nodes exchange
short APRS messages (protocol "RC1") with an HQ node, and today they run
through graywolf. The plan is to let a node drive one of these radios
over USB-C instead, using the host messaging API
(`claude/aprs-messaging-api.md`, revision 2026-10-11), on the same
frequency and in the same race as graywolf nodes. See
[`2026-10-12-uvk5-backend-proposal.md`](2026-10-12-uvk5-backend-proposal.md).

The 67-character message change is already done, thank you: RC1 now fits
without changes. The requests below are **in order of importance**. The
first three are needed to deliver race data exactly and safely. The rest
make it faster, easier to set up, or easier to diagnose. Each should be
backward compatible: a new reply field, a new monitor line, or a new
command that older hosts ignore. A **capability flag or version** a host
can read (request 0) lets checkin-board use them as they appear.

## 0. Capability flags

A way for the host to learn which of the features below this firmware
has: for example, a bit field in the handshake reply or the settings
reply. Then checkin-board can use the exact mode where it exists and the
conservative mode where it doesn't, without parsing version strings.

## Needed for exact delivery

### 1. Line number in the send reply

`0x0701` reply carries the line number (`nn`, 01-99) the radio assigned
to this message.

- **Why:** today an ack can only be matched by timing. A late ack for an
  earlier copy of a message, arriving after the host moved on, would
  confirm the wrong message, and race data would be marked delivered when
  it wasn't. (RC1 recovers it later through gap requests, but slowly.)
  With the number, each ack matches exactly one message.
- **Spec section 10 already offers this.**

### 2. SENT and DROP lines

Monitor lines when a queued message actually goes on air, and when it's
given up:

```
SENT:<to>{nn}
DROP:<to>{nn} <reason>
```

`<reason>`: `busy` (60 s without a clear channel), `nocall` (placeholder
callsign), `aprsoff`, `band` (VFO out of range), `battery`, `replaced`
(a newer message took the slot), `off` (APRS switched off with it
pending).

- **Why:**
  - The host can send the next message as soon as the slot is free,
    instead of waiting a fixed time or for the ack.
  - It can show "sent" and "failed" the way graywolf does.
  - Today's silent drops become reasons an operator can fix: the most
    common field problems (battery, out of band) show at once instead of
    after a minute of time-outs.
- **Spec section 10 already offers SENT/DROP.** The reason field is the
  addition.

### 3. Several pending acks

Room for **at least 4 pending acks** (queued, in order, deduplicated by
sender + line number), instead of one `ack_pending` slot.

- **Why:** today, when two messages addressed to the radio arrive within
  its 5 s gap, the second one's ack replaces the first one's, and the
  first sender must retry. For a checkpoint that costs some airtime. For
  an **HQ node**, which hears from every checkpoint, acks would be lost
  routinely, so a radio can't serve as HQ without this.

## Throughput

### 4. A deeper outgoing queue

A queue of a few messages (4 to 8), each with its own line number
(request 1) and SENT/DROP line (request 2). A second `0x0700` adds to the
queue instead of replacing. When the queue is full, refuse with `Ok = 0`
and a "full" indication instead of replacing.

- **Why:** the host can keep several messages in flight (graywolf allows
  several per peer) instead of strict one-at-a-time with a wait for each
  ack. Replacing a queued message silently is the riskiest behaviour in
  the API today; refusing is safe.
- **Spec section 10 already offers a deeper queue.**

### 5. Settable digipeater path

A path setting for messages and acks: **none (direct)**, `WIDE1-1`, or
`WIDE1-1,WIDE2-1` (today's fixed path in `APRS_BuildHeader`). It could
be a field in the send command (per message), or a setting. A setting is
enough for us.

- **Why:**
  - checkin-board has a race-wide path setting; graywolf nodes follow it,
    radio nodes can't.
  - On a race where every station hears HQ directly, `WIDE1-1,WIDE2-1`
    makes every message and ack be repeated by every digipeater in range.
    That multiplies airtime on a shared frequency, and carries race
    traffic well beyond the course.

### 6. Configurable gap between own transmissions

A setting for the minimum gap between the radio's own transmissions:
default 5 s, allowed down to about 2 s.

- **Why:** the 5 s gap (with acks first) caps how fast a busy checkpoint
  can clear a backlog after a burst of runners. Lower is optional, at the
  operator's choice.

## Setup and diagnosis

### 7. Status command (read-only, no `ts`)

Reply with:

- battery voltage, and the low-battery flag;
- the main VFO frequency, and whether it's in an allowed band;
- whether the radio would refuse to transmit now, and why (the same
  reasons as request 2);
- whether the channel is busy (squelch open);
- whether monitoring is on for this port.

**Why:** checkin-board's **Check radio** runs before every race. It could
catch every silent-drop cause before the first runner arrives, instead
of discovering them as unacknowledged messages.

### 8. Set callsign and SSID

A command, with `ts`, that sets only the callsign and SSID, so the host
doesn't have to round-trip the whole 96-byte record with "write all
settings" (`0x070E`).

**Why:** checkin-board's `stationsetup` assigns each node its
callsign-SSID from one CSV. A duplicated callsign-SSID was the first
mistake in our field test. Today radio nodes would have to be set by
hand on the radio.

### 9. Optional: set the frequency

A command, with `ts`, to set the main VFO frequency (within the allowed
bands), so `stationsetup` can put every radio on the race frequency.

## Link quality

### 10. Signal strength per received frame

The receive signal strength (RSSI, or the S-meter reading) on each
`APRSRAW:` line, as an extra field older hosts ignore. For example,
`APRSRAW:<hex> RSSI:-87`.

**Why:** checkin-board's link check reports the signal level of each
probe heard, to judge a station's link margin before the race. It
already works without levels, but levels tell a marginal link from a
solid one.

### 11. Frame counters

Counts since start: frames decoded, frames with bad FCS (or a
demodulator's best guess), and frames transmitted. These could go in the
status command (request 7).

**Why:** checkin-board's Check radio shows "packets decoded, damaged" for
graywolf nodes. That's the most useful sign of a bad audio path or a weak
signal, and radio nodes would get it too.

## Not needed

- **Deduplicating inbound messages:** the host does it.
- **Retrying unacknowledged messages in the radio:** the host owns
  retries (RC1 sends each retry as a new message).
- **Reply-acks (APRS 1.1 `{nn}aa`):** not used by RC1.
