# checkin-board: race checkpoint reporting -- design

Status: DRAFT rev 3 (sign-off answers folded in, section 11; separate admin login). Not started
(see section 9 for per-phase status).
Date: 2026-10-05
Derived from: graywolf `docs/superpowers/specs/2026-10-05-race-checkpoint-design.md`
(rev 3, branch `feature/race-checkpoints` at `baea019a`). That version builds
the feature **inside** graywolf as `pkg/race`. This one rebuilds it as a
**standalone Go app** that runs next to an unmodified graywolf.

## 0. Ground rules and what changed

**Code provenance (user decision, 2026-10-05):**

- `pkg/race` (code, tests, latency simulation, docs) is **our own work**
  and may be ported, copied and relicensed freely.
- The rest of the graywolf repo is **not ours**. No graywolf code (outside
  `pkg/race`) is copied, vendored or imported. Where `pkg/race` calls
  graywolf packages (`aprs`, `messages`, `configstore`, `txgovernor`,
  `clocksync`, ...), those calls are replaced, not ported.
- `checkin-board` interacts with graywolf **only** through its published
  REST API, or through graywolf's operator-facing extension points:
  Actions/triggers, which run a command or webhook on a matching inbound
  message, and scripts. Statements in this spec about graywolf behaviour
  are observations to verify through the API (phase 1 contract tests).
  The design must not depend on graywolf internals.
- The fake graywolf used in tests is written from the published API
  description and observed behaviour, not from graywolf source.

| Area | In-tree (graywolf `pkg/race`) | Standalone (`checkin-board`) |
|---|---|---|
| Process | Part of the graywolf binary | Separate binary next to graywolf, same host by default |
| RF transport | Raw APRS frames through `txgovernor` | graywolf **Messages API** (`POST /api/messages`) |
| Inbound | `rxfanout` classifier ahead of the messages router | Read graywolf's inbox (`GET /api/messages/events` SSE + cursor catch-up) |
| ACKs | Own msgids (`R`+seq, `H`+n); HQ ACKs after the store write | graywolf assigns numeric msgids and ACKs automatically; we read `status` per row |
| Retries | Own outbox ladder, never gives up | Own outbox ladder driving `POST /api/messages/{id}/resend`; graywolf's ladder turned off for race peers (section 3.3) |
| Undecodable report | HQ sends REJ | Not possible (graywolf ACKs first); HQ logs it and shows it in health |
| Addressee | Configurable race addressee | HQ's station callsign (DM). Tactical labels can't be used: graywolf doesn't ACK or retry them |
| Storage | graywolf DB, migration 30 | Own SQLite file (`checkin-board.db`), own migrations |
| UI | Svelte routes in the graywolf UI | Own web app: **admin** interface + **volunteer** interface (section 8) |
| Auth | graywolf session | Two logins: a shared **volunteer** password (keypad) and a separate **admin** password (settings, tools). Every page and endpoint requires one of them (7.2). The app logs in to graywolf with a service account |
| Race lifecycle | n/a | Explicit **Complete race**, **graywolf cleanup** and **Reset** (4.7) |
| Upstream | Upstream PR stack, CONTRIBUTING rules | None. graywolf is unmodified, so section 9a of the original is dropped |

The wire grammar (`RC1`), batching, gap tracking, race clock, journal, roster
rules, recovery tools and board are **unchanged in behaviour**. Their code is
ported from `pkg/race`, with the graywolf dependencies replaced by
`internal/graywolf` (the API client) and app-owned storage.

## 1. Goal

Turn a set of graywolf stations into a checkpoint-reporting network for a
rural running race with no internet on course, **without changing graywolf**.

- Each **aid station** runs graywolf plus `checkin-board` (Pi or laptop +
  radio). A volunteer logs in to `checkin-board` on a phone/tablet and types
  bib numbers on a keypad. The app stamps the arrival time, batches
  entries, and sends them to HQ as APRS messages through graywolf.
- One **HQ / net-control** node (usually start/finish) runs the same app in
  HQ role. It collects every report and shows a live race board, a
  per-runner lookup, and a CSV export.
- Delivery is reliable: every batch is ACKed, the checkpoint shows each
  entry as queued / sent / confirmed, and HQ detects and requests any
  missing batches.

Decisions (carried over from the 2026-10-05 Q&A unless marked **new**):

| Topic | Decision |
|---|---|
| Topology | N aid stations + 1 HQ; existing digipeaters / relays fill gaps |
| Data | Bib + time-in only (plus a "void" to correct typos) |
| Bibs | Numeric, 1-4 digits |
| Clock | No GPS on nodes; time is set from the volunteer's browser (section 6) |
| HQ entry | HQ gets the same keypad page (start/finish) |
| Course | Out-and-back allowed: a runner may pass a checkpoint more than once |
| Frequency | Must coexist on 144.390; expected to run on a dedicated race frequency |
| Entry | Keypad page in the volunteer interface of `checkin-board` |
| Transport | **new:** `RC1` text carried in APRS DMs sent through graywolf's Messages API, 67-char default |
| Scale | 150-500 runners; mass-start surges ~20 bibs/min at early checkpoints |
| HQ output | Live board, runner lookup, CSV export |
| Downlink | ACK-based delivery confirmation + HQ gap/resend requests |
| Scope | **new:** standalone app; graywolf unmodified; API / Actions / scripts only; RF only (`prefer_is` never set) |
| Code reuse | **new:** `pkg/race` is ours and is ported freely; no other graywolf code |
| Access | **new:** every page requires login, including the board. One shared volunteer password for bib/time entry; a separate admin password for everything else |
| UI split | **new:** admin interface (settings, HQ tools, lifecycle; admin login) + volunteer interface (keypad, recent entries; volunteer login) |
| graywolf messages | **new:** race messages are left in graywolf until the race is marked complete; then an explicit cleanup |
| Reset | **new:** admin action that clears all race data on a node |
| graywolf version | **new:** built and tested against graywolf `0.14.14` (`main` at `559fc9ae`). Startup checks `GET /api/version` and warns on an untested version |

Non-goals (MVP; unchanged): cutoff times / overdue highlighting, time-out /
dwell, DNF reasons, free-text / medical traffic (graywolf's Messages page
already covers it), roster push over RF, webhook push, barcode/RFID,
peer-mesh sync. **Also new:** no changes to graywolf, and no raw-frame
access (KISS/AGW). Section 12 covers the fallback.

## 2. Architecture

```
            CHECKPOINT HOST                                      HQ HOST
 browser keypad / admin                                browser board / keypad / admin
     |  HTTP (:8090, login required)                         |  HTTP (:8090)
 checkin-board (role=checkpoint)                     checkin-board (role=hq)
   entries -> Batcher -> Outbox                        Inbox reader -> Ingest -> event log
     |  POST /api/messages      ^ SSE /api/messages/events     ^ SSE          | POST /api/messages
     |  POST /api/messages/{id}/resend                         |              |  (RC1 G gap requests)
     v                          |                              |              v
 graywolf (:8080) ---- RF (APRS DM, auto-ACK, digis) ---- graywolf (:8080)
```

A node can be **both**: HQ logs the start/finish line on its own keypad,
and those local entries go straight into the HQ event log with no RF hop.

### 2.1 Go layout

```
cmd/checkin-board/        main: config, wiring, signal handling
internal/config/          bootstrap config from env/file (exists in skeleton)
internal/graywolf/        typed REST client: auth, messages, SSE, prefs, station, version
                          (replaces the skeleton's internal/apiclient)
internal/wire/            RC1 codec: types, encode, decode, fuzz (port of pkg/race wire)
internal/raceclock/       browser-synced race clock (port of pkg/race clock)
internal/store/           SQLite (modernc.org/sqlite, no cgo), migrations, repositories
internal/checkpoint/      batcher, outbox, ack watcher, heartbeat, gap-request handler
internal/hq/              ingest, gap tracker, heartbeat/skew, board, roster, export
internal/inbox/           graywolf inbox reader: SSE + cursor catch-up, dispatch to CP/HQ
internal/journal/         power-safe CSV journal (port)
internal/recovery/        CP export, HQ import (port)
internal/lifecycle/       complete race, graywolf cleanup, reset (section 4.7)
internal/web/             HTTP server, auth, REST handlers, embedded static UI
web/                      static HTML/CSS/JS (embedded with go:embed)
```

The Pi Zero W (ARMv6) is a target, so the build is pure Go (`CGO_ENABLED=0`,
`GOARM=6`). That means `modernc.org/sqlite`, not `mattn/go-sqlite3`.

### 2.2 graywolf API surface used

All calls use the session cookie from `POST /api/auth/login` (section 7.2).

| Need | graywolf endpoint |
|---|---|
| Login / re-login on 401 | `POST /api/auth/login` |
| Liveness, server time | `GET /api/health` |
| Version check | `GET /api/version` |
| Station callsign (read / set from admin) | `GET` / `PUT /api/station/config` (`callsign`) |
| Send a batch / heartbeat / gap request | `POST /api/messages` `{to, text, path, channel, client_id}` |
| Delivery state of a sent row | `GET /api/messages/{id}` (`status`, `attempts`, `acked_at`, `msg_id`) and SSE `acked` / `rejected` changes |
| Retransmit with the **same** msgid | `POST /api/messages/{id}/resend` (409 = already in flight: retry next tick) |
| Inbound reports / heartbeats / gap requests | `GET /api/messages/events` (SSE) and `GET /api/messages?folder=inbox&since=&cursor=` for catch-up |
| Turn off graywolf's retry ladder for race peers | `PUT /api/messages/conversations/dm/{CALL}/prefs` `{wait_for_ack:false}` (section 3.3) |
| Max text length | `GET /api/messages/preferences` (`max_message_text_override`) |
| Keep race rows out of the operator's unread count | `POST /api/messages/{id}/read` after ingest |
| Post-race cleanup (4.7) | `DELETE /api/messages/{id}` (race rows only) |
| Link health hints (optional) | `GET /api/stations`, `GET /api/packets?type=message` |

**Actions / triggers / scripts** are an allowed integration path but aren't
used by the MVP: inbound race traffic doesn't use the Actions `@@` trigger
syntax, and the Messages API already covers it. They stay available for
post-MVP needs, such as a graywolf Action that restarts `checkin-board`
remotely.

Phase 1 must verify each row against the target graywolf version. One
example: the conversation-prefs route answers on 0.14.14, but it's missing
from the published `openapi.yaml`, so its use rests on the contract test,
not on the document.

### Roles (`settings.role`)

`checkpoint` | `hq`, chosen in the admin interface. There's no `off`: an app
that isn't racing just isn't running.

## 3. Wire protocol `RC1` (text unchanged, transport changed)

Every frame is an ordinary APRS **direct message** sent by graywolf. The
message **text** follows the original grammar, so it reads clearly in any
APRS client, in graywolf's Messages page and in the packet log:

| Type | Direction | Text | ACKed by |
|---|---|---|---|
| Report | CP -> HQ call | `RC1 R <cp> <seq> @HHMM bib/ss bib/ss @HHMM bib/ss ...` | HQ's graywolf, automatically |
| Heartbeat | CP -> HQ call | `RC1 H <cp> <lastseq> <HHMMSS>` | HQ's graywolf, automatically |
| Gap request | HQ -> CP call | `RC1 G <cp> 12,14-16` | CP's graywolf, automatically (ignored by HQ logic: HQ repeats until filled) |

Unchanged: `<cp>` 1-6 `[A-Z0-9]`; `<seq>` per-checkpoint monotonic,
persisted; bib `[0-9]{1,4}` normalised, `0` rejected; `@HHMM` **UTC**
minute groups; `bib/ss`; `-bib/ss` void; midnight rollover via
`TimeOfDay.Resolve` (nearest instant, valid while latency < 12 h);
`MaxGapSeqs` = 1000. The codec in `internal/wire` is pure, table-tested and
fuzz-tested, and it is the single source of the grammar.

### 3.1 Msgids

The original derived msgids from seq (`R`+base36). Through the Messages API
**graywolf assigns the msgid**. We can't choose it, but `resend` reuses it
(to be confirmed in phase 1). Consequences:

- One batch = one graywolf message row. The CP stores
  `graywolf_message_id` (row id) and `msg_id` with the batch. Every
  retransmit is a `resend` of that row, so HQ's graywolf sees the same
  (from, msgid, text) and can ACK the copy without storing it again.
- If the row is gone (`resend` returns 404), the outbox sends a **new**
  message with the same text. HQ's own `(cp, seq, text)` dedup (section 5)
  makes that idempotent, so a new msgid is harmless.
- HQ never relies on graywolf's dedup. Any duplicate inbox row is dropped
  by the app's batch-level dedup.

### 3.2 Text length

The usable maximum is `min(settings.max_text_len, graywolf limit)`, where
the graywolf limit is 67 unless `max_message_text_override` (68-200) is
set. The app reads it at startup and every 5 min, and the batcher packs to
it. Raising it is an operator decision in graywolf, not something the app
does.

### 3.3 Who owns retries

graywolf's own DM ladder is 30 s x `retry_max_attempts` (default 4,
~90 s), after which the row goes to `timeout`. The original design measured
that this gives up far too early (section 9b), and the race outbox has to
retry forever with its own backoff and in-flight cap.

**Decision:** on startup, and whenever the race peers change, the app sets
`wait_for_ack=false` on the DM conversation with each race peer (HQ's call
at a checkpoint; each checkpoint's call at HQ). In graywolf that means
"send once, don't enrol in the retry ladder". Phase 1 must confirm that an
ACK arriving later still flips the row to `acked`. The app's outbox then
drives every retransmit through `/resend` on its own schedule, which keeps
the measured behaviour: 30/60/120 s then every 300 s, `max_in_flight` = 4,
fast retransmit on a confirming ACK. **Complete race** (4.7) restores each
peer's previous prefs.

Side effect: during the race, the operator's manual DMs to those peers
from graywolf's Messages page aren't retried either. The admin settings
page lists the affected callsigns.

Fallback if phase 1 shows `wait_for_ack` can't be used: leave graywolf's
ladder on, and let the outbox `resend` a row only once it reaches
`timeout`. That costs about 4x the dead-link airtime (each round is 4
frames), so the app warns about it in status.

## 4. Behaviour

### 4.1 Checkpoint

1. `POST /api/entries {bib}` stamps `time_in = raceNow()` and stores the row
   `queued`. The keypad returns instantly, and RF never blocks a volunteer.
2. **Batcher** (unchanged): flushes when the next entry wouldn't fit, or
   `flush_after_sec` (default 20 s) after the oldest unbatched entry.
   Assigns `seq`, encodes, writes `batches` (state `pending`, no graywolf
   id yet) and marks entries `sent`.
3. **Outbox**: oldest-first, at most `max_in_flight` (4) unACKed batches.
   - First transmit: `POST /api/messages {to: hq_call, text, path,
     channel, client_id: "<cp>-<seq>"}` and store the returned `id` and
     `msg_id`. Crash safety is covered in step 7.
   - Retransmit: `POST /api/messages/{id}/resend`. Backoff is 30 s, 60 s,
     120 s, then every 300 s forever. A batch is never dropped.
   - **Fast retransmit** (unchanged): an ACK that confirms one of our
     batches proves the link works, so batches sent 20 s or more ago
     without an ACK are resent immediately, restarting at the 60 s rung.
   - graywolf errors: a 503, or a network error to graywolf, means retry
     next tick and show "graywolf unreachable". A 400 parks the batch as
     `rejected` and shows it in the outbox (a bug or a config problem such
     as a bad path). A 404 on resend sends it again as a new message.
4. **ACK**: the inbox reader sees an SSE `acked` change for a row we sent,
   or the per-tick `GET /api/messages/{id}` poll does. The batch and its
   entries become `confirmed`. A `rejected` status from a real peer REJ
   parks the batch as `rejected`.
5. Inbound `RC1 G` for our `<cp>` from the configured HQ call re-queues the
   named seqs for immediate resend, bounded by `requeueMinAge` (60 s).
   `RC1` from any other sender is ignored and logged.
6. **Heartbeat** every `heartbeat_sec` (default 300 s) and right after
   startup: `RC1 H` as a new DM. Heartbeats aren't resent: each one
   supersedes the last, and there's at most one outstanding heartbeat.
7. **Crash safety around POST**: on startup, any batch with
   `state=pending` and no `graywolf_message_id` is matched against
   `GET /api/messages?folder=sent&peer=<hq_call>` before it's sent again.
   It matches on `client_id` if phase 1 shows it's persisted, and
   otherwise on exact text, which is unique per (cp, seq).
8. Void: `DELETE /api/entries/{id}`. A `queued` entry is removed locally.
   Otherwise a `-bib/ss` void entry is queued.

### 4.2 HQ

1. The inbox reader gets an inbound DM (to our call, text starting `RC1 `)
   and dispatches it by type. graywolf has **already ACKed it** and holds
   it in its own store, so the inbox row is the durable copy, and the app
   ingests from it idempotently. The app moves its inbox cursor forward
   only after the ingest transaction commits.
2. `RC1 R`: decode, then `IngestReport` with `(cp, seq, text)` dedup
   (unchanged). An **undecodable** body can't be REJected, because graywolf
   already ACKed it. It's stored in `bad_reports` with sender and text and
   shown in health as a red counter per sender.
3. Gap tracker (unchanged): per missing seq, `gap_grace_sec` 90 s, then
   requests at +180 s and every 300 s, least-requested first, give up after
   12, at most one request per tick. Requests are `POST /api/messages
   {to: <cp's call>, text: "RC1 G ..."}`. The app needs the call to send
   to: `checkpoints.expected_call` if set, otherwise the most recent
   `from_call` heard for that `<cp>`.
4. Heartbeats: update last heard, last seq (tail-gap detection) and clock
   skew. Skew is withheld while HQ's race clock is unsynced.
5. **Sender check:** a report whose `from_call` doesn't match
   `expected_call` is still ingested, but flagged on the health panel.
   Callsigns are validated before use as an addressee.
6. Board, health panel, roster (no personal data, `category` only from a
   column headed `category`), HQ keypad with local codes, and results CSV
   export: **unchanged** from sections 4.2.3-4.2.7 of the original.

### 4.3 Courses that revisit a checkpoint

Unchanged: entries aren't unique per `(cp, bib)`. Every entry is stored.
The board shows the latest and the lookup shows all.

### 4.4 Missing data and manual recovery

Unchanged from the original 4.4: visible gaps, HQ re-request of given-up
batches, CP export and HQ import (batch-exact rebuild through the same
`(cp, seq, text)` dedup), and the power-safe journal
(`race-journal.csv` next to `checkin-board.db`, fsynced **before** the DB
write, torn-tail tolerant, `ErrJournalOnly`). One addition: the CP export
also includes each batch's graywolf `msg_id`, for cross-checking against
graywolf's packet log.

### 4.5 graywolf inbox during the race

Race traffic lands in graywolf's Messages inbox and sent folder, and the
app can't stop that.

- After an inbound `RC1` row is ingested, the app marks it read so the
  operator's unread count stays meaningful.
- **Nothing is deleted from graywolf while the race is active** (user
  decision). graywolf's rows are a second audit trail, and deleting an
  outbound row would cancel our resend target.
- The admin page warns when graywolf's `retention_days` is non-zero and
  shorter than the race window.

### 4.6 Inbox reader

- Holds one SSE connection to `/api/messages/events` and reconnects with
  backoff (1, 2, 5, 10, 30 s).
- On every (re)connect, and every 60 s as a backstop, it pages
  `GET /api/messages?folder=inbox&cursor=<saved>` until it's caught up, so
  nothing is missed while SSE is down. The cursor is persisted in the app
  DB.
- SSE events are only hints: on any event the reader fetches the row and
  handles it with the same code path as the catch-up. Processing is keyed
  on graywolf row id, so seeing a row twice is a no-op.
- On a 401 it logs in again once, then backs off. "graywolf unreachable"
  and "graywolf auth failed" both show on every app page.

### 4.7 Race lifecycle: complete, cleanup, reset (new)

The race has a state in `settings.race_state`: `setup` → `active` →
`complete`. Admin actions move it forward. **Reset** returns the node to
`setup`.

1. **Start race** (`setup` → `active`): checks that the role, codes, HQ
   call and graywolf connection are set, applies the `wait_for_ack` prefs
   (3.3), and stamps `race_started_at`. The keypad works only in `active`.
   Before that it shows "Race not started" so no entries are taken by
   accident.
2. **Complete race** (`active` → `complete`): the keypad stops taking
   entries. At a checkpoint, anything still queued is batched, and the
   outbox **keeps running** until every batch is confirmed or the admin
   presses "Stop sending". This is shown as "N batches still unconfirmed".
   HQ stops sending gap requests. The app restores each race peer's
   graywolf conversation prefs to their saved pre-race values. Results
   export and recovery import stay available.
3. **graywolf cleanup** (only in `complete`): deletes from graywolf the
   race message rows the app has recorded (its sent batches, heartbeats
   and gap requests, plus ingested inbound `RC1` rows), using
   `DELETE /api/messages/{id}` one row at a time, with progress. It only
   deletes rows whose ids the app stored and whose text starts with
   `RC1 `, and never deletes whole threads, so the operator's own
   conversations with those callsigns survive. It's optional and
   repeatable, and a 404 counts as already gone.
4. **Reset** (any state; admin only): clears **all race data on this
   node** so it can be reused:
   - CP: local entries, batches, seq counter, outbox state.
   - HQ: received batches, event log, checkpoint status, gaps, bad
     reports.
   - Both: the inbox cursor moves to "now", so old graywolf rows are never
     re-ingested; the race clock sync is cleared; `race_state` returns to
     `setup`.
   - Kept by default: settings (role, callsign, tactical name, HQ call,
     tuning), the HQ checkpoint list and the roster. Two checkboxes clear
     those too, for a fresh event.
   - Safety: the admin must type the race name to confirm. A reset during
     `active`, or while unconfirmed batches exist, shows a stronger
     warning. Before clearing, the app **always** writes a backup
     (`backups/<race>-<timestamp>/`: a DB snapshot via `VACUUM INTO`, the
     journal, and the CP export or results CSV) and rotates
     `race-journal.csv` into it. The backup isn't deleted by the app.
   - Reset doesn't touch graywolf. Cleanup (3) is the graywolf side.
   - At HQ, a checkpoint that resets mid-race and restarts at seq 1 is
     already handled: it's new data, and `seq_reuse_count` flags it.

## 5. Storage (app-owned SQLite, embedded SQL migrations)

| Table | Side | Key columns |
|---|---|---|
| `settings` | both (singleton) | role, race_name, race_state, race_started_at, station_tactical, checkpoint_code, hq_local_codes, hq_call, gw_channel, path, max_text_len, flush_after_sec, max_in_flight, heartbeat_sec, gap_grace_sec |
| `peer_prefs_backup` | both | callsign, original wait_for_ack/send_path (restored on Complete) |
| `checkpoints` | HQ | code (unique), name, course_order, expected_call |
| `runners` | HQ | bib (unique), category; deliberately no personal-data columns |
| `local_entries` | CP | cp_code, bib, time_in, clock_synced, state (queued/sent/confirmed), void_of, batch_id |
| `batches` | CP | (cp_code, seq) unique, text, state (pending/acked/rejected), **gw_message_id, gw_msg_id, client_id**, attempts, last_tx_at, next_tx_at, acked_at |
| `gw_rows` | both | gw_message_id, kind (batch/heartbeat/gap/inbound), deleted_at: what cleanup (4.7.3) may delete |
| `received_batches` | HQ | (cp_code, seq, text) unique; **gw_message_id**, source_call |
| `received_entries` | HQ | append-only event log: cp_code, bib, time_in, is_void, batch_seq (NULL = HQ keypad), source_call, received_at |
| `cp_status` | HQ | cp_code, last_heard_at, last_call, heartbeat_at, heartbeat_last_seq, clock_skew_sec, max_seq, batches_received, seq_reuse_count, bad_reports |
| `bad_reports` | HQ | gw_message_id, from_call, text, error, received_at |
| `inbox_state` | both | cursor, last processed gw row id |
| `auth` | both | bcrypt hashes of the admin and volunteer passwords, session secret |
| `sessions` | both | token hash, role (`admin`/`volunteer`), created_at, expires_at; deleted on logout, on expiry, and for every session of a role when that role's password changes |

The station **callsign** isn't stored in the app: graywolf's
`/api/station/config` owns it (section 8.1).

Carried over unchanged: event-log netting per (cp, bib, time_in), with
order-independent voids; seq reuse with different text treated as new data
and counted; all timestamps UTC, whole seconds; `MissingSeqs` bounded by
the result limit plus received batches.

## 6. Clock

Unchanged in substance. The race clock is synced from the volunteer's
browser (`POST /api/clock/sync {client_unix_ms}` plus RTT/2 compensation),
it's monotonic-based (`syncWall + time.Since(syncMono)`), held in memory
only, and shows "Clock not set" until a sync. Entries logged unsynced carry
`clock_synced=false`. HQ computes skew from the heartbeat's `HHMMSS`.

Difference: the original used graywolf's `pkg/clocksync` to skip the
browser sync when the OS clock was disciplined. The app can't see that
through the API. It runs its own check instead: it reads `timedatectl
show -p NTPSynchronized` on Linux where available and treats any other
answer as unsynced. The browser sync is always available either way.

## 7. App interfaces

### 7.1 REST API (app's own, `/api/...` on `:8090`; all require login)

Volunteer interface (volunteer **or** admin session):

| Method | Path | Role |
|---|---|---|
| POST | `/api/entries` | CP (and HQ keypad) |
| GET | `/api/entries?since=` | recent local entries + state |
| DELETE | `/api/entries/{id}` | void |
| GET | `/api/station` | race name, state, tactical name, cp code, unconfirmed count, last HQ contact, graywolf health |
| GET/POST | `/api/clock`, `/api/clock/sync` | read / set the race clock |

Admin interface (admin session only; a volunteer session gets 403):

| Method | Path | Role |
|---|---|---|
| GET/PUT | `/api/admin/settings` | both (callsign is proxied to graywolf, 8.1) |
| POST | `/api/admin/race/start`, `/complete`, `/stop-sending` | both (4.7) |
| POST | `/api/admin/race/cleanup-graywolf` | both, `complete` only |
| POST | `/api/admin/race/reset` | both; body must include the race name |
| GET | `/api/admin/outbox` | CP: batch status, graywolf link + auth health |
| GET/POST/PUT/DELETE | `/api/admin/checkpoints` | HQ |
| GET/POST/DELETE, POST `/import` | `/api/admin/runners` | HQ |
| GET | `/api/admin/board` | HQ |
| GET | `/api/admin/runners/{bib}/history` | HQ |
| GET | `/api/admin/export.csv` | HQ |
| GET | `/api/admin/status` | HQ: per-checkpoint health, bad reports |
| POST | `/api/admin/status/{cp}/rerequest` | HQ: re-arm given-up gaps |
| GET | `/api/admin/recovery/export.csv` | CP |
| POST | `/api/admin/recovery/import`, `/journal` | HQ |
| PUT | `/api/admin/password/admin` | both: change the admin password (needs the current one) |
| PUT | `/api/admin/password/volunteer` | both: set or change the volunteer password |
| GET | `/api/admin/gw` | graywolf version, callsign, reachable, authed, max text, retention |

The UI polls every 5 s. There are no WebSockets from the app to the
browser.

Carried-over security fixes: CSV formula neutralisation, capped and atomic
imports, bounded error lists, name/callsign validation, and a
`Content-Type: application/json` guard on mutating routes, against
same-site form forgery.

### 7.2 Auth

- **Two roles, two passwords** (user decision, 2026-10-05):
  - **Volunteer:** one shared password for the node, given to everyone
    working the keypad. A volunteer session can use only the volunteer
    interface (8.2) and its API: log and void bibs, see previous entries,
    read the station header, and sync the race clock.
  - **Admin:** a separate password for the station operator. An admin
    session can use the admin interface (8.1) and its API: settings
    (callsign, station tactical name, ...), race lifecycle, outbox, HQ
    board and tools, exports, recovery, and passwords. An admin session can
    also use the volunteer interface, so the operator can log bibs without a
    second login.
  - Each role's password is stored as a bcrypt hash. The two must differ,
    and setting one equal to the other is rejected.
- **First run:** a setup page, shown only while no admin password exists,
  sets the admin password. The admin then sets the volunteer password in
  the admin interface. Until it's set, the volunteer login says "Not set up
  yet: ask the station operator".
- **Login:** one login page with a role choice (Volunteer / Admin).
  `POST /api/login {role, password}` issues a session for that role.
  **Every** page, API route and export requires a valid session of the
  right role. That includes the HQ board: there's no public or read-only
  view. The only unauthenticated routes are the login/setup page, its
  static assets, `POST /api/login` and `POST /api/setup`.
- **Sessions:** `HttpOnly`, `SameSite=Strict` cookie holding a random
  token; the server stores only its hash with the role. Volunteer sessions
  expire after 24 h (a race day), and admin sessions after 8 h idle.
  Changing a role's password ends every session of that role. Logout ends
  the current one.
- **Enforcement:** role checks are server-side middleware on the route
  groups (`/api/admin/*` and `/admin` need admin), not just hidden links.
  A volunteer session gets 403 on admin routes, and the admin page
  redirects it to the admin login.
- **Brute force:** login attempts are rate-limited per client IP and per
  role (e.g. 5/min, then backoff), and admin failures are logged.
- **App → graywolf:** a dedicated graywolf user. Username and password come
  from env (`GW_USER`, `GW_PASSWORD`) or a `0600` file. They're never
  logged and never returned by any app API. The cookie is held in memory
  only.
- The app binds to `0.0.0.0:8090` by default (phones join the node's
  Wi-Fi/hotspot). graywolf stays on `:8080`, and `GW_BASE_URL` defaults to
  `http://localhost:8080`.

## 8. Web UI

The app has no Node toolchain, so the Pi Zero cross-build stays simple: it's
plain HTML/CSS/ES modules embedded with `go:embed`, no build step. There
are two interfaces, each behind its own login (7.2).

### 8.1 Admin interface (`/admin`, admin login)

- **Station settings:**
  - **Callsign:** read from graywolf `GET /api/station/config`. Changing it
    here calls `PUT /api/station/config` after a confirm dialog warning
    that it changes graywolf's station callsign for everything, not just
    the race.
  - **Station tactical name:** the station's on-air/voice-net name, for
    example `AID3` or `FINISH`. Up to 9 chars `[A-Z0-9-]`. It's shown in
    the volunteer header and in exports, and pre-fills the checkpoint code
    when it fits the 1-6 `[A-Z0-9]` code rule.
  - Role, race name, checkpoint code (CP), local codes (HQ), HQ callsign
    (CP), graywolf channel and path, and tuning (max text,
    flush/heartbeat/gap timings, in-flight cap).
  - The graywolf connection panel: version, reachable, authed, max text
    length, retention warning, and the peer callsigns with retries turned
    off (3.3).
- **Race lifecycle:** Start / Complete / Stop sending / graywolf cleanup /
  Reset (4.7), each with its state-dependent confirm.
- **CP tools:** outbox view (batches, attempts, next retry, msgid), recovery
  export.
- **HQ tools:** board (bibs only), bib search and history, checkpoint health
  (clock skew, missing / given-up batches with Re-request, wrong-sender and
  bad-report counters), results export, roster import, checkpoint editor,
  recovery and journal import.
- Passwords: change the admin password (needs the current one); set or
  change the volunteer password. Either change logs out every session of
  that role.
- Log out.

### 8.2 Volunteer interface (`/`, volunteer or admin login)

There are no settings, tools or admin links here: a volunteer phone shows
only the keypad and the entries list.


- Header: race name, station tactical name and checkpoint code, race
  state, race clock, and "N unconfirmed, last HQ contact Xm ago".
- **Keypad:** large numeric keypad (4-digit cap) and a big LOG button that
  saves only on LOG/Enter. HQ adds a local-checkpoint selector
  (START/FIN).
- **Previous entries:** the last 20 entries with queued / sent / confirmed
  badges, an unsynced-clock marker, and a void button (with confirm). "Show
  more" pages back through the station's whole list.
- Banners: "Clock not set" (with "Set time from this device"), drift above
  5 s, "graywolf unreachable", "Race not started" / "Race complete".
- Works one-handed at phone width.

UI behaviour carried over from phase 8: Enter on other controls never
logs, no lost or duplicated bibs on network failure, non-overlapping
pollers, unreachable banner.

## 9. Delivery phases (each one PR-sized, TDD, 80%+ coverage, `-race`)

| # | Phase | Output | Status |
|---|---|---|---|
| 0 | Sign-off | This spec approved | Answers received 2026-10-05; awaiting final approval |
| 1 | graywolf client + API spike | `internal/graywolf`: login/re-login, health, version, station get/put, send, get, resend, delete, list+cursor, SSE, conv prefs, preferences. `httptest` fakes for unit tests, plus an **opt-in contract test** (`GW_CONTRACT=1`) against a real graywolf 0.14.14 that proves: msgid is stable across resend; `wait_for_ack=false` stops the ladder and late ACKs still flip status; `client_id` round-trips (or not); the inbox cursor doesn't skip rows; single-row delete leaves the thread intact | **In progress** 2026-10-05. Client done, 96.3% coverage, reviewed (fixes: watchdog pauses during handler and covers connect; stalled-cursor detection; 30 s fail-fast after bad credentials; no credentials in URLs, errors or `%v`). Contract test written (`make contract`), **not yet run**: waiting on the graywolf test host |
| 2 | Wire codec + race clock | Port `types/encode/decode/clock` from `pkg/race` into `internal/wire`, `internal/raceclock`; table tests + `FuzzDecode` | Not started |
| 3 | Storage | SQLite (modernc), embedded migrations, repositories, roster CSV + `FuzzParseRosterCSV` | Not started |
| 4 | Inbox reader | SSE + catch-up, cursor persistence, idempotent dispatch, reconnect/backoff | Not started |
| 5 | Checkpoint engine | Batcher, outbox on `/messages` + `/resend`, ACK watcher, fast retransmit, gap-request handler, heartbeat, crash-safe POST (4.1.7) | Not started |
| 6 | HQ engine | Ingest, bad-report capture, gap tracker + sender, heartbeat/skew, sender check | Not started |
| 7 | Integration test | Two app instances against a **fake graywolf**: an in-memory Messages API written from the published API (auto-ACK, dedup, `wait_for_ack`, a lossy link with drop / reorder / dup). Asserts eventual exactly-once at HQ; re-runs the 9b latency table and the spoofed-traffic airtime bounds | Not started |
| 8 | Recovery, journal, lifecycle | Journal, CP export, HQ import, re-request; Start / Complete / Stop sending / graywolf cleanup / Reset with backup (4.7) | Not started |
| 9 | App REST + auth | Handlers, DTO validation, volunteer/admin login and role middleware (table test: every route × no session / volunteer / admin), first-run setup, password change + session revocation, `reset-admin-password` CLI, rate limit, session expiry, graywolf credential handling | Not started |
| 10 | Web UI | Admin + volunteer interfaces; JS unit tests for logic; scripted browser run of the real binary against the fake graywolf | Not started |
| 11 | Packaging + docs | `GOARM=6` build, systemd unit (`After=graywolf.service`), install script, operator README incl. recovery and reset procedures | Not started |
| 12 | Field rehearsal | 3 nodes on the bench (graywolf over KISS-TCP or real radios), then a walk-around on the course | Not started |

Phase 1 comes first on purpose: every later phase rests on graywolf
behaviours that this design assumes but hasn't tested through the API.

## 9b. Delivery latency

The original's measured table (`pkg/race`, phase 5) applies only if the app
reproduces the same on-air behaviour: window 4, fast retransmit,
30/60/120/300 s ladder, one frame per (re)transmit. Section 3.3 is what
keeps that true. Phase 7 re-runs the ported simulation (300-runner mass
start, 10% dup, up to 3 s delay, 5 seeds) through the fake graywolf, and
the table is filled in here. **Acceptance:** each loss row must be within
25% of the original "Shipped" column. If the 3.3 fallback is active,
dead-link airtime is reported separately.

Reference (original, shipped): 10% loss 22 s; 20% 37 s; 30% 3 m 2 s; 40%
16 m 4 s; 50% 42 m 41 s. Dead link: 12-42 frames/h per checkpoint.

Extra delay to account for: SSE notification plus a fetch on each ACK and
each inbound row. That's milliseconds on localhost, negligible against
RF.

## 10. Risks

| Risk | Mitigation |
|---|---|
| graywolf API changes between versions | Version check at startup; contract test (phase 1) run before each race against the deployed graywolf version |
| `wait_for_ack=false` not available or behaves differently | Fallback in 3.3 (resend only on `timeout`), with an airtime warning |
| Race peers' graywolf prefs left modified after a crash | Originals saved in `peer_prefs_backup`; restored on Complete; also listed on the admin page with a "Restore now" button |
| graywolf ACKs reports the app then fails to ingest | graywolf's inbox is durable; the cursor only advances after commit; the next catch-up retries |
| Undecodable report can't be REJected | Stored in `bad_reports`, shown per sender at HQ; CP export is the recovery path |
| Race traffic clutters graywolf Messages | Mark-read during the race; cleanup only after Complete (4.7) |
| Operator deletes race rows in graywolf mid-race | CP: resend 404 means send it new (HQ dedups). HQ: rows already ingested are unaffected |
| Accidental reset | Type-the-race-name confirm, stronger warning while active, automatic backup before clearing |
| Cleanup deletes operator messages | Deletes only app-recorded row ids whose text starts `RC1 `; never thread deletes |
| Changing the callsign from admin affects all of graywolf | Explicit confirm; shown as graywolf's setting, not the app's |
| Volunteer password spreads beyond the crew | Accepted (user decision): it only allows bib entry, voids and clock sync. The admin can rotate it, which logs out every volunteer session |
| Volunteer reaches settings or tools | Server-side role check on every admin route (7.2); tested per route in phase 9 |
| Admin password lost at a station | README recovery: `checkin-board reset-admin-password` CLI on the host (needs shell access); race data is untouched |
| graywolf down or restarting | Outbox and keypad keep working locally; banner; catch-up on reconnect |
| Shared 1200-baud congestion; no digis on dedicated freq; iGates gating `RC1` on 144.390; lost ACK means duplicate; bad clocks; typos | Unchanged from the original section 10 |
| Two processes on a Pi Zero W (512 MB) | Pure-Go binary, SQLite, no Node at runtime; measure RSS in phase 11 |

## 11. Resolved questions

Carried over: no GPS (browser race clock), numeric 1-4 digit bibs, HQ keypad
with a local-checkpoint selector, out-and-back supported, coexist on
144.390 or a dedicated frequency, cutoffs post-MVP, no personal data.

Answered 2026-10-05:

1. **Code reuse.** `pkg/race` is our work and may be used freely. The rest
   of graywolf is not: interact only via its API or Actions/triggers/
   scripts (section 0).
2. **Auth and UI.** One shared **volunteer** password for the bib/time
   entry interface (keypad, previous entries). The **admin** interface
   (callsign, station tactical name, other settings, HQ/CP tools) sits
   behind a **separate admin login** (clarified 2026-10-05; sections 7.2,
   8). The station tactical name is the station's voice-net name, as
   described in 8.1 (confirmed).
3. **graywolf messages.** Don't delete until the race is complete. Include
   a reset feature that clears all checkpoint data (section 4.7).
4. **Access.** All access requires login. There's no public board
   (section 7.2).

## 12. Fallback transport (not planned)

If the Messages API turns out unworkable in phase 1, graywolf also serves
**KISS-over-TCP** and **AGWPE** to external clients as published
interfaces. The app could then send and receive raw APRS frames itself. It
would get back the original design exactly: own msgids `R`+seq,
store-then-ACK, REJ on bad text. The cost is implementing APRS message
ACK/dedup ourselves (porting `pkg/race`'s transmit side, which is ours).
This is a decision for after phase 1, not part of the MVP.

## 13. Open questions

None blocking. To confirm during phase 10 UI review: the exact fields on
the admin settings page beyond those listed in 8.1.

## 14. Post-MVP backlog

Unchanged: cutoff times and overdue highlighting; `RC1 T` RF time
broadcast; DNF / drops; time-out / dwell; roster push over RF; webhook push
to timing software. New: the KISS/AGW transport (section 12); a graywolf
Action for remote restart/status of `checkin-board`; per-person admin
accounts if one admin password per station proves too coarse.
