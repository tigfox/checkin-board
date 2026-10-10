# Phase 12a proposal: race config file, event page, graywolf config

Status: **approved 2026-10-10**, with one change from the user: **every
setting stays changeable on the device, and the operator has the final
say.** A loaded race config sets values; it never locks them. After
loading, any race, messaging, checkpoint-list or graywolf setting can
still be edited on the node as today (the role still changes only before
the race starts, as it does now). It answers the open
questions for feedback items 2, 4 and 5
([`docs/feedback-2026-10-09.md`](../feedback-2026-10-09.md)) with a
recommendation each, so phase 12a can start once they are confirmed or
changed.

## The idea in one paragraph

HQ (or whoever plans the race) writes **one file per race** listing every
station. Each node loads the same file, picks which station it is, and
gets its race and messaging settings, HQ's checkpoint list, an optional
event page, and optional graywolf settings, after a preview and a confirm.
Nothing in it is secret, so it can be emailed or put on a USB stick.

## Decisions to confirm

| # | Question (feedback doc) | Recommendation | Why |
|---|---|---|---|
| 2.1 | One file per race or per node? | **One per race**, listing every station | One file to hand out and keep in sync; HQ needs the whole list anyway |
| 2.2 | How does a node find its entry? | **Chosen from a list** when loading, **pre-selected** when graywolf's callsign matches a station's | Works on a fresh node (callsign not set yet) and avoids a wrong guess |
| 2.3 | HQ's checkpoint list from the same file? | **Yes**: codes, names, expected callsigns, course order | It's the same data; typing it twice is how mismatches happen |
| 2.4 | Roster and branding? | **No** (keep their own imports) | Roster can hold personal data; branding is HQ-only and has its own checks |
| 2.5 | Set the graywolf callsign? | **Yes, per station, in the graywolf section** (2.11) with its own confirm | Unique callsign-SSIDs per node were the first field-test mistake; the file is the natural place to assign them |
| 2.6 | Format | **JSON** (strict: unknown keys refused), with an **export** button that writes HQ's current setup as a file | No new dependency (Go reads JSON natively); export means nobody writes it from scratch |
| 2.7 | When can it apply? | **Only before the race starts** (setup), like the role | A mid-race change of codes or calls would strand data. Everything it sets stays editable on the device afterwards (user, 2026-10-10) |
| 2.8 | Secrets | **Never in the file**; loading refuses any field named like a password or token | Safe to share |
| 2.9 | Where pre-downloaded files come from | A folder on the node, **`/var/lib/checkin-board/race-configs/`**, filled by copying files in (scp, USB) or by `install.sh --race-config FILE` | No internet in the field; the installer is already the setup step |
| 2.10 | Are uploads kept? | **Yes**, saved into that folder; the Race page lists and can delete them | The next node or a reset can reuse it |
| 2.11 | Preview before applying? | **Yes**: a table of current value → new value, with graywolf changes in a separate section and confirm | Applying is irreversible enough to deserve a look |
| 4.1 | Event page format | **A small Markdown subset** (headings, paragraphs, lists, bold, simple tables) rendered as text; no HTML, no links off the node | Easy to write; can't run code; keeps the no-internet rule |
| 4.2 | Images | **Not in this phase** | Size and SD wear; a course map can come later as a separate upload |
| 4.3 | Where it shows | A link on the **Station page** next to the guide, and on the **keypad** for volunteers | Volunteers need frequencies and contacts too |
| 4.4 | Size limit | **64 KB** of event-page text; **256 KB** for the whole file | Plenty for notes; a bad file can't fill the card |
| 5.1 | Which graywolf settings | **TX timing** (TX delay, tail), **message preferences** (retention, max text length), **digipeater on/off**, **beacons off** (by name) | The settings that differ per event; all have API endpoints |
| 5.2 | API only | **Yes**: anything graywolf's API can't set stays out | Project rule |
| 5.3 | Node hardware (audio device, PTT, sample rate) | **Out of the file** (decided 2026-10-10: the guide covers the Pi Zero's 24 kHz) | It differs per node and per board |
| 5.4 | Undo | **Save graywolf's current values before applying** and offer **Restore graywolf settings** after the race, like peer retries today | Graywolf is shared with non-race use |
| 5.5 | Callsign | Per station, in this section (see 2.5) | |
| 5.6 | Confirm step | **Separate confirm** listing each graywolf change | It changes graywolf for everything on the node |

## File sketch

```json
{
  "format": "checkin-board-race/1",
  "race": { "name": "Ridge 50K" },
  "messaging": { "path": "", "heartbeat_sec": 300, "flush_after_sec": 20, "max_in_flight": 4,
                 "max_text_len": 67, "gap_grace_sec": 90 },
  "hq": { "callsign": "KD2DCM-3", "station_name": "Race HQ", "local_codes": ["START", "FIN"] },
  "checkpoints": [
    { "code": "AS1", "name": "Ridge", "station_name": "Ridge Aid #1", "callsign": "KD2DCM-4", "course_order": 1 },
    { "code": "AS2", "name": "Creek", "station_name": "Creek Aid", "callsign": "KD2DCM-5", "course_order": 2 }
  ],
  "event_page": "## Frequencies\n- Packet: 145.050 MHz\n- Voice net: 146.520 MHz\n\n## Contacts\n- Race director: ...",
  "graywolf": { "tx_delay_ms": 300, "tx_tail_ms": 100, "message_retention_days": 0, "digipeater": false }
}
```

Loading on a checkpoint node, as station AS1, sets: role checkpoint, race
name, station name "Ridge Aid #1", checkpoint code AS1, HQ callsign
KD2DCM-3, the messaging tunables, the event page, and (after its own
confirm) graywolf's callsign KD2DCM-4 and the graywolf settings. On the HQ
node it sets role HQ, local codes, HQ's checkpoint list, and graywolf's
callsign KD2DCM-3.

## Work, once approved

1. Store: `race_configs` folder handling; the event page (a settings row).
2. Parser and validator (strict JSON, size caps, every field validated
   with the existing rules, secrets refused), with fuzzing.
3. Apply: a preview (diff) and apply, setup-only, transactional for the
   app's settings; graywolf changes through its API with a saved backup and
   restore.
4. UI: Race page "Race config" card (list, upload, delete, preview, apply);
   Admin → HQ export; the event page view (Station page and keypad).
5. `install.sh --race-config FILE`.
6. Docs: operator guide, station guide, spec.
