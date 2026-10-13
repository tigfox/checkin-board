# Race-day runbook

A short, printable checklist for the race coordinator and each station
operator. The details are in the [operator guide](operator-guide.md);
the node's own **Station guide** (Admin → Station → guide link) works
without internet.

## The week before (coordinator)

- [ ] Every node is installed, has done first-run setup, and uses the
      **same admin password** (README, "Install on a node").
- [ ] Fill in a stations CSV from [`example_stations.csv`](../example_stations.csv):
      one line per node, a **different callsign-SSID on every node**,
      exactly one `hq` line.
- [ ] `CB_ADMIN_PASSWORD=... bin/stationsetup -dry-run stations.csv`:
      every node answers and shows sensible changes.
- [ ] `CB_ADMIN_PASSWORD=... bin/stationsetup stations.csv`: type `yes`;
      every node says **set up**.
- [ ] HQ: load the roster (README, "Roster"), set the board branding.
- [ ] Bench test: all radios on low power in one room, then
      `bin/benchrace` (docs/bench-race.md). It should print PASS.
      **Reset the race on every node afterwards.**
- [ ] Print a station card (below) for every station.

## At each station, before the first runner (station operator)

- [ ] Radio on, on the race frequency, antenna up, AIOC cable seated.
      Radio node (UV-K5/UV-K1): USB-C cable in, APRS on, FM, Path
      Direct or WIDE1-1, battery charged.
- [ ] Node powered; the panel shows the station and its address.
- [ ] **Admin → Station → Radio → Check radio**: no ✗. "Nothing decoded
      yet" is fine until traffic is heard.
- [ ] **Admin → Link check** to HQ: PASS (MARGINAL: improve the antenna,
      then run it again).
- [ ] Volunteers' phones on the node's Wi-Fi, keypad open at the
      address on the panel, clock banner clear.
- [ ] HQ: **Start race**. Checkpoints: **Open checkpoint** when ready.

## During the race: quick fixes

| You see | What it means | Do |
|---|---|---|
| Keypad: **"Radio cable missing: entries are kept here…"** | The AIOC is unplugged or dropped off USB | Reseat the cable. If it doesn't come back in a minute, **reboot the node** (`sudo reboot`, or power cycle). Entries are safe and go out when it's back |
| A radio **stays transmitting** (TX light on, no packets) | A dropping AIOC can leave the radio keyed | **Unplug the AIOC from the radio** or turn the radio off now; then see the row above |
| Keypad: **"Radio not connected"** (UV-K5), panel "radio DOWN" | The UV-K5/UV-K1 is unplugged, off, or its port is busy | Reseat the USB-C cable (a data cable), switch the radio on; it reconnects by itself. Entries are safe |
| Check radio (UV-K5): **Can transmit: no** | No callsign, APRS off, out of band, not FM, or low battery | Fix it on the radio (the line says which); the checkpoint resends |
| A radio or its cable **fails and a spare of the other kind** is at hand (AIOC radio ↔ UV-K5) | — | Connect the spare; **Station → Radio → Radio type**, choose it, **Change radio type**. The app restarts in about 10 s (a bib logged meanwhile shows NOT confirmed: tap LOG again). Run **Check radio**; unplug or switch off the old radio. Nothing is lost |
| Keypad: **"graywolf unreachable"** | graywolf isn't running | Wait a minute; if it stays, reboot the node |
| Keypad: N **unconfirmed** climbing, "HQ heard" getting old | The link to HQ is down | Nothing is lost: entries keep retrying. Check antenna and frequency; run a link check |
| HQ: checkpoint **quiet** or **missing batches** | Its reports aren't arriving | HQ re-requests automatically; check that station by voice |
| Panel: **"modem BEHIND"** | The Pi can't keep up with the audio (Pi Zero) | Check the node runs the Pi Zero graywolf build at 24 kHz (Check radio) |
| Keypad: **"saved in the backup journal only"** | The database couldn't be written | Don't re-enter the bib; tell HQ; free disk space |

## After the race

- [ ] Checkpoints: **Close checkpoint**, **Secure for travel**, carry
      the node back.
- [ ] Back at HQ, on each checkpoint node: **Final check-in**; it
      moves to **Checked in** by itself once HQ has everything.
- [ ] HQ: **Complete race**, then **Download results CSV**.
- [ ] Save logs if needed: `journalctl -b > logs.txt` (logs are in RAM
      and lost at reboot).
- [ ] Optional: graywolf cleanup; **Restore graywolf settings**; Reset.

---

## Station card

Print one per station and tape it to the radio box.

| | |
|---|---|
| **Station** | __________________________ (code: ______) |
| **This node's callsign** | __________________ |
| **HQ callsign** | __________________ |
| **Race frequency** | ________ MHz |
| **Voice / net frequency** | ________ MHz |
| **Node Wi-Fi** (name / password) | __________________ / __________________ |
| **Keypad** | http://__________:8090/ (also on the panel) |
| **Volunteer password** | __________________ |
| **Station operator** (name, phone) | __________________ |
| **HQ contact** (name, phone) | __________________ |

**If the keypad says "Radio cable missing":** reseat the AIOC cable;
if it isn't back in a minute, reboot the node. Entries are kept.
**If the radio stays transmitting:** unplug the AIOC from the radio.
**If the link to HQ is down:** keep logging; nothing is lost.
**Never re-enter a bib** after a "saved" or "journal only" message.
