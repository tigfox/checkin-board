# checkin-board: race checkpoint reporting -- design

Status: DRAFT rev 5 (adds configurable status-board branding, 8.3). Not started
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
| Branding | **new (2026-10-06):** the status board's logo, colour scheme, header and footer text are configurable in the admin panel (8.3) |
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
internal/wire/            RC1 codec: report/heartbeat/gap (port of pkg/race) + link-check P/Q
internal/raceclock/       browser-synced race clock (port of pkg/race clock)
internal/store/           SQLite via gorm + glebarez/sqlite (pure-Go modernc, no cgo), embedded SQL migrations, repositories
internal/checkpoint/      batcher, outbox, ack watcher, heartbeat, gap-request handler
internal/hq/              ingest, gap tracker, heartbeat/skew, board, roster, export
internal/inbox/           graywolf inbox reader: SSE + cursor catch-up, dispatch to CP/HQ
internal/journal/         power-safe CSV journal (port)
internal/recovery/        CP export, HQ import (port)
internal/lifecycle/       complete race, graywolf cleanup, reset (section 4.7)
internal/web/             HTTP server, auth, REST handlers, embedded static UI
internal/panel/           node panel (8.4): status/menu model, 1-bit renderer, panel loop
internal/panel/epd/       e-ink drivers (SSD1675, SSD1680, SSD1680Z) on periph.io SPI/GPIO
web/                      static HTML/CSS/JS (embedded with go:embed)
```

The Pi Zero W (ARMv6) is a target, so the build is pure Go (`CGO_ENABLED=0`,
`GOARM=6`). That means `modernc.org/sqlite` (through `glebarez/sqlite` for
gorm, which keeps the `pkg/race` store code portable), not
`mattn/go-sqlite3`.

### 2.2 graywolf API surface used

All calls use the session cookie from `POST /api/auth/login` (section 7.2).

| Need | graywolf endpoint |
|---|---|
| Login / re-login on 401 | `POST /api/auth/login` |
| Liveness, server time | `GET /api/health` (requires login on the test host, 0.14.14 `b589e686`) |
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

**Actions / triggers / scripts** are an allowed integration path. Race
traffic doesn't use them: it doesn't use the Actions `@@` trigger syntax,
and the Messages API already covers it. Where an Action does drive the
app, it is a graywolf **webhook Action** calling a local hook on the app
(decision 2026-10-06), never a command Action. The reasons:

- graywolf's service runs with `NoNewPrivileges`, so a command Action
  can't switch to the `checkin-board` user that owns the app's data.
- A webhook keeps every trigger inside the app's own validation, limits
  and audit.

**Local hooks** (`/api/hook/...`):

- They are off unless `CB_HOOK_TOKEN_FILE` is set.
- They answer only callers on the node itself that send the bearer token:
  over loopback, or from the very address the request arrived on (the
  panel reaching an app that listens on one LAN address only). Don't put
  a reverse proxy or tunnel on the node in front of the app: every
  client would look local, and only the token would protect the hooks.
- They reply 200 with one short line, because graywolf relays only
  "error: http NNN" for anything else.

The first hook is the remote link check (4.8.7). The node panel (8.4) is
the second user: its own service reads the e-ink bonnet's buttons and
calls `/api/hook/panel/...`. The same path stays open for more triggers,
such as a remote restart or status query through a graywolf webhook
Action.

Phase 1 must verify each row against the target graywolf version. One
example: the conversation-prefs route answers on 0.14.14, but it's missing
from the published `openapi.yaml`, so its use rests on the contract test,
not on the document.

### 2.3 Installing graywolf with the app (new, 2026-10-09)

`install.sh` also installs graywolf when the node doesn't have it, so a
bare Raspberry Pi becomes a node in one step (user, 2026-10-09). It uses
only graywolf's **published releases**, the way graywolf's handbook
says to (download the package for the node's architecture, then `apt
install ./graywolf_*.deb`). Nothing is copied from the graywolf repo
(section 0).

- **Already installed** (`dpkg -s graywolf`, or `graywolf` on the PATH):
  left alone, never upgraded. The script prints the installed version,
  and warns when it differs from the version this release was tested
  with (`checkin-board version`). Upgrading graywolf stays the
  operator's choice.
- **Not installed:**
  1. Find the latest release: GitHub's
     `api.github.com/repos/chrissnell/graywolf/releases/latest`.
     `--graywolf-version vX.Y.Z` pins one instead, e.g. the tested
     version.
  2. Pick the package for `dpkg --print-architecture`: `amd64`, `arm64`
     or `armhf` (Raspbian's armhf also covers ARMv6 Pi Zero and Pi 1).
     Any other architecture stops with a clear message.
  3. Download `graywolf_<version>_<arch>.deb` and the release's
     `checksums.txt` over HTTPS, and check the package's SHA-256 against
     it. A mismatch aborts. The checksums come from the same release, so
     this proves the download intact, not who published it.
  4. `apt install ./graywolf_<version>_<arch>.deb`. The package creates
     the `graywolf` user and its service.
  5. Start graywolf. It creates its database on first start; the script
     waits up to 30 s for it. The app copes with graywolf still
     starting.
  6. **graywolf's admin login.** A fresh graywolf has no users, and its
     web UI then offers "Create Admin Account". The app's own
     `checkin-board` login (created next, by graywolf's CLI) would be
     the first user and hide that screen. So, before creating it, the
     script creates the operator's admin login:
     - interactively, asking for a username (default `admin`) and
       password;
     - non-interactively, from `GRAYWOLF_ADMIN_USER` and
       `GRAYWOLF_ADMIN_PASSWORD_FILE`.
     Either way it uses graywolf's own `auth set-password`, run as the
     `graywolf` user.
  7. Warn when the installed version isn't the tested one.
- **Offline or GitHub unreachable:** the script says so and continues
  without graywolf, so the app installs and waits for graywolf like any
  node whose graywolf is down. Install at home, before going out to the
  course.
- **Not in scope:** configuring graywolf's radio side (callsign, AIOC
  sound card, PTT, channel). That is graywolf's own setup; the operator
  guide points to its handbook.
- **Testing:** in containers with a fake GitHub (a local HTTPS server
  serving a release JSON, a stub package and checksums). Cases: each
  architecture, already installed, checksum mismatch, offline, pinned
  version and the admin login. Then once for real on a spare SD card.

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
| Heartbeat | CP -> HQ call | `RC1 H <cp> <lastseq> <HHMMSS>[ C]` (` C`: this checkpoint is closed, 4.7) | HQ's graywolf, automatically |
| Gap request | HQ -> CP call | `RC1 G <cp> 12,14-16` | CP's graywolf, automatically (ignored by HQ logic: HQ repeats until filled) |
| Link probe (**new**, 4.8) | prober -> responder | `RC1 P <cp> <run> <i>/<n>` | responder's graywolf, automatically (the ACK is the round-trip measurement) |
| Probe reply (**new**, 4.8) | responder -> prober | `RC1 Q <cp> <run> <heard> <lvl> <via>` | prober's graywolf, automatically |

Unchanged: `<cp>` 1-6 `[A-Z0-9]`; `<seq>` per-checkpoint monotonic,
persisted; bib `[0-9]{1,4}` normalised, `0` rejected; `@HHMM` **UTC**
minute groups; `bib/ss`; `-bib/ss` void; midnight rollover via
`TimeOfDay.Resolve` (nearest instant, valid while latency < 12 h);
`MaxGapSeqs` = 1000. The codec in `internal/wire` is pure, table-tested and
fuzz-tested, and it is the single source of the grammar.

Link-check fields (new): `<cp>` is the **prober's** station code. `<run>`
is 1-9999, chosen by the prober per run. `<n>` is 1-20 and `<i>` is
1..`<n>`. `<heard>` lists the probe indices the responder decoded, in
the gap-list form (`1-3,5`, ascending, disjoint, canonical). `<lvl>` is
the responder's median receive audio level for those probes in whole dBFS
(`-23`, `0`), or `X` if unknown. `<via>` is `-` for direct, or the last
digipeater's callsign. Decoding is text-canonical like every other type.

### 3.1 Msgids

The original derived msgids from seq (`R`+base36). Through the Messages API
**graywolf assigns the msgid**. We can't choose it, but `resend` reuses it
(to be confirmed in phase 1). Consequences:

- One batch = one graywolf message row. The CP stores
  `graywolf_message_id` (row id) and `msg_id` with the batch. Every
  retransmit is a `resend` of that row, so HQ's graywolf sees the same
  (from, msgid, text) and can ACK the copy without storing it again.
- If the row is gone (`resend` returns 404), the outbox sends a **new**
  message with the same text.
- graywolf's `resend` resets attempts but **not** the row's acked or
  rejected state. A gap request for a batch that was already ACKed (or
  REJected) therefore releases its old row and sends it as a new message.
  Resending the old row would read as confirmed straight away, without HQ
  getting it. `RequeueSeqs` does this. The slow contract test records the
  actual behaviour as a FINDING. HQ's own `(cp, seq, text)` dedup (section 5)
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
   - graywolf errors. An error **status** means nothing went on air, so
     the attempt is rolled back and retried next tick. The exception is
     400 (refused), which usually means a settings problem affecting
     every batch (path, channel, length). The batch keeps its
     retry-ladder slot instead of being parked; the refusal is shown as
     an admin alert and clears on the next successful send. A
     **transport** error (timeout, reset) is ambiguous: graywolf may have
     created the row. The attempt is kept, and the engine looks the row
     up (as in step 7) before the batch would be sent again. A 404 on
     resend sends the batch as a new message.
   - Status is polled (`GET /api/messages/{id}`) every 10 s for in-flight
     batches *before* transmitting, covering ACKs the inbox feed skipped
     (4.6) without a wasted resend.
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
7. **Crash safety around POST**: on startup, and on any tick that finds one, any batch with
   `state=pending` and no `graywolf_message_id` is matched against
   `GET /api/messages?folder=sent&peer=<hq_call>` before it's sent again.
   It matches on `client_id` if phase 1 shows it's persisted, and
   otherwise on exact text, which is unique per (cp, seq). Only rows
   created after the batch, and not already recorded by the app, are
   considered, so a row from an earlier race with identical text (after
   a Reset restarts seq at 1) can't be taken.
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
   12, at most one request per tick. **As built (phase 6):**
   - Once HQ has a checkpoint list, only listed checkpoints are chased, so
     spoofed reports for phantom codes can't turn into gap-request airtime.
     With no list yet, every heard code is chased.
   - Before the first gap request to a checkpoint, graywolf's own retries
     are turned off for that call (3.3), so each request is one frame.
   - A send failure backs that checkpoint off for 30 s, and the others are
     still served.
   - HQ ticks every 5 s.
   - Known limitation: gap-tracker state (attempt counts, "given up") is
     in memory, so an HQ restart re-arms every open gap. That's harmless
     but repeats requests. Persisting it is a candidate for phase 8. Requests are `POST /api/messages
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

**As built (phase 4):** all processing goes through one path. A single
worker pages `GET /api/messages?folder=all` forward from the saved
cursor. SSE events, every (re)connect, and a 60 s backstop only *kick*
that worker; event payloads are never trusted. graywolf orders the feed
by `updated_at`, so it delivers new inbound rows and also re-delivers
our own outbound rows whenever their status changes. That gives the
checkpoint engine its ACK notifications from the same reader
(`Dispatcher.HandleOutbound`, which must be idempotent). Only RC1 text in
DM threads is dispatched; everything else is the operator's traffic.
Known limitation: graywolf's cursor compares `updated_at` to the whole
second, then id, so a status change on an older (lower-id) row in the
same second as the cursor can be skipped. New inbound rows are never
skipped, because their ids are always higher. The checkpoint engine's
per-tick `GET /api/messages/{id}` poll of in-flight batches (4.1.4)
covers the skipped case. A fresh node's first catch-up uses
`InitialSince` so it doesn't replay old races from graywolf's history.

Failure handling. A row whose dispatch keeps failing is skipped after
10 attempts, or at once if the dispatcher marks the error `Permanent`, so
one bad row can't stall the feed. The protocol recovers the content: HQ
gap requests re-fetch a batch, and an outbound row reappears on its next
status change. Skips are counted in the reader's status for the health
banner. Dispatchers should record bad input (e.g. an undecodable report)
and return nil rather than an error. "Connected" means graywolf accepted
the event stream (an on-open hook in the client), not merely that a
connection was attempted. Stream and catch-up errors are reported
separately. A failed mark-read is retried on the next catch-up. A
catch-up that stops at its page limit re-runs immediately.

- Holds one SSE connection to `/api/messages/events` and reconnects with
  backoff (1, 2, 5, 10, 30 s).
- On every (re)connect, and every 60 s as a backstop, it pages
  `GET /api/messages?folder=inbox&cursor=<saved>` until it's caught up, so
  nothing is missed while SSE is down. The cursor is persisted in the app
  DB.
- SSE events are only hints: on any event the reader fetches the row and
  handles it with the same code path as the catch-up. Processing is keyed
  on graywolf row id, so seeing a row twice is a no-op.
- **Starting point.** A node with no cursor reads from a starting point
  saved once, on its first run (`inbox_state.since`). A restart before
  any race traffic therefore doesn't skip messages graywolf received
  meanwhile. An admin "re-read graywolf messages since…" action
  (`SetInboxSince`, phases 8-9) covers a wiped database; rows already
  recorded are skipped.
- **Not configured yet.** A node with no role holds its position
  (`inbox.ErrNotReady`) instead of marking race traffic handled, so
  reports graywolf ACKed before HQ was configured are ingested once it
  is.
- On a 401 it logs in again once, then backs off. "graywolf unreachable"
  and "graywolf auth failed" both show on every app page.

### 4.7 Race lifecycle: complete, secure, check in, cleanup, reset

The race has a state in `settings.race_state`. Admin actions move it
forward, and **Reset** returns the node to `setup`. *As built in phase 8;
the secured phase and final check-in were added at the user's request,
2026-10-06.*

```
setup ─Start→ active ─Complete→ complete
checkpoint only:
  active | complete | checking_in ─Secure→ secured        (packed up; radio quiet)
  secured | complete ─Check in→ checking_in ─(all confirmed)→ checked_in
any state ─Reset→ setup
```

| State | Keypad | Checkpoint transmits | HQ gap requests |
|---|---|---|---|
| `setup` | off | no | no |
| `active` | on | yes | yes |
| `complete` | off (voids still allowed) | yes: backlog flushed at once | **yes** (so later check-ins can recover a lost batch) |
| `secured` | off | **no**, heartbeats included: packed up for the trip back | — |
| `checking_in` | off | yes: everything unconfirmed due at once, heartbeat at once | — |
| `checked_in` | off | no | — |

Each node's state is its own. On a checkpoint, the UI and the panel
(8.4) call Start race and Complete race **Open checkpoint** and **Close
checkpoint**. An aid station early on the course often closes, packs up
and heads back to HQ while the race goes on elsewhere (user,
2026-10-08). Closing a checkpoint never changes HQ's state or any other
checkpoint's. A checkpoint's usual sequence is Open checkpoint → Close
checkpoint → Secure for travel → HQ check-in.

1. **Start race** (`setup` → `active`): requires a role, and stamps
   `race_started_at` from the race clock. The keypad works only in
   `active`; before that it shows "Race not started", so no entries are
   taken by accident. graywolf's retries are turned off for each peer
   before the first send to it (3.3), not at Start.
2. **Complete race** (`active` → `complete`; "Close checkpoint" on a
   checkpoint): the keypad stops taking new entries, but voids (typo
   fixes) still work. A checkpoint flushes anything queued at once and
   keeps delivering. From then on its heartbeats carry the closed flag
   (` C`). HQ's health panel shows the checkpoint as **closed** (with
   the time HQ heard it), with its counts, instead of a "quiet" warning
   once it goes silent to travel. Closed sticks: a delayed pre-close
   heartbeat can't reopen it. Only a checkpoint reset for a new race
   reopens it, because its numbering restarts (heartbeat seq 0). HQ keeps asking a closed checkpoint for missing
   batches until they arrive or it checks in. The codec accepts
   heartbeats with or without the flag. A checkpoint on an older
   release just looks quiet at HQ, so every node of an event should
   run the same release. HQ keeps sending gap
   requests, so a checkpoint's final check-in can still recover a batch HQ
   lost after ACKing it. Results export and recovery imports stay
   available.
3. **Secure** (checkpoint: `active` | `complete` | `checking_in` →
   `secured`): the operator packs the node up for the trip back to HQ.
   Nothing is transmitted, not even heartbeats, until the final check-in.
   The UI shows "N entries to deliver at check-in".
4. **Final check-in** (checkpoint: `secured` | `complete` →
   `checking_in`): back at HQ, with the radio in range, every unconfirmed
   batch becomes due at once. Batches parked as rejected are revived as
   new messages, because graywolf keeps a rejected row rejected (3.1). A
   heartbeat goes out immediately, so HQ learns the last seq and requests
   any batch it lost. The node moves to **`checked_in`** by itself once
   nothing is unconfirmed **and** HQ has been heard since the check-in
   began, judged by this node's own clock, not graywolf's ACK time. It
   then restores graywolf's per-peer settings, retrying until that works.
   Parked batches are re-sent every 5 minutes during a long check-in. If
   RF can't
   finish, **Export** (4.4) is the offline path: carry the file to HQ's
   admin page. In simulation, a 20-entry backlog checks in within about a
   minute at close range.
5. **graywolf cleanup** (checkpoint: `complete` or later; HQ:
   `complete`): deletes from graywolf the race message rows the app has
   recorded (its sent batches, heartbeats and gap requests, plus ingested
   inbound `RC1` rows), one row at a time with progress.
   - It only deletes rows whose ids the app stored, re-checks that the text
     starts `RC1 `, and never deletes whole threads, so the operator's own
     conversations survive.
   - It **never deletes a row whose batch HQ hasn't confirmed**: that would
     cancel its resend.
   - It's optional and repeatable; a 404 counts as already gone.
   - Race rows survive a Reset in the app's records, so cleanup can still
     run afterwards.
6. **Reset** (any state; admin only): clears **all race data on this
   node** so it can be reused:
   - CP: local entries, batches, seq counter, outbox state.
   - HQ: received batches, event log, checkpoint status, gaps, bad
     reports.
   - Both: the inbox cursor moves to "now", so old graywolf rows are never
     re-ingested; the race clock sync is cleared; `race_state` returns to
     `setup`.
   - Kept by default: settings (role, callsign, tactical name, HQ call,
     tuning), board branding (8.3), the HQ checkpoint list and the
     roster. Checkboxes clear those too, for a fresh event: one for the
     checkpoint list and roster, one for branding.
   - Safety: the admin must type the race name (or `RESET` if it has
     none) to confirm. A checkpoint that hasn't checked in and still has
     unconfirmed entries refuses the reset unless the admin explicitly
     acknowledges it (the data stays in the backup). Before clearing, the
     app **always** writes a backup (`backups/<race>-<timestamp>/`: a DB
     snapshot via `VACUUM INTO`, the CP export or HQ results CSV) and
     moves `race-journal.csv` into it, so the next race starts a fresh
     journal. A failed snapshot aborts the reset; a failed export or
     journal move is reported as a warning. The backup isn't deleted by
     the app.
   - Reset also restores graywolf's per-peer settings, moves the inbox
     reader's starting point to now, and clears the race clock sync and
     both engines' in-memory state (contact times, gap tracker).
   - Reset doesn't touch graywolf. Cleanup (3) is the graywolf side.
   - At HQ, a checkpoint that resets mid-race and restarts at seq 1 is
     already handled: it's new data, and `seq_reuse_count` flags it.

### 4.8 Deployment link check (new)

A quick automated test, run when a node is set up at its location, that
confirms it has a **usable RF link** to HQ before the race depends on it.
It runs from the admin page ("Link check") or headless from the host
(`checkin-board linkcheck`). Either side can be the prober: a checkpoint
checks toward HQ, and HQ can check toward any checkpoint's call.

1. **Probes.** The prober sends `probe_count` (default 5) `RC1 P` DMs to
   the responder, `probe_spacing` (default 10 s) apart, with graywolf's
   retries off (3.3) so each probe is exactly one frame. graywolf's
   automatic ACK of each probe proves a full round trip. The prober records
   the RTT from the row's `sent_at` to `acked_at`.
2. **Responder.** The responder's app records which probe indices it
   decoded. For each probe it reads the frame's receive audio level and
   digipeater path from its own graywolf (`GET /api/packets?type=message`,
   matched on source call and text; the level is present only for frames
   from graywolf's own modem). After it has heard index `<n>`, or 30 s
   after the last probe it heard, it sends one `RC1 Q` reply. The reply is
   resent up to twice at 30 s if not ACKed. A checkpoint answers probes
   only from its configured HQ call. HQ answers any valid callsign, and
   flags ones that aren't in its checkpoint list.
3. **Measurements at the prober:**
   - **uplink:** probes the responder heard (from `<heard>`).
   - **round trip:** probes ACKed.
   - **downlink:** whether the reply arrived.
   - **RTT:** median.
   - **Remote receive level:** `<lvl>`.
   - **Local receive level:** of the responder's ACK and reply frames, from
     the prober's own `/api/packets`.
   - **Path:** `<via>`.
4. **Verdict:**
   - **PASS:** uplink ≥ n-1, round trip ≥ n-1, reply received, and median
     RTT ≤ 20 s.
   - **MARGINAL:** at least half the probes got through in each direction.
     The race will work, but with retries and gap requests. The result
     suggests antenna height, a relay or a digipeater path.
   - **FAIL:** anything else.
   Audio levels are advice, not part of the verdict. A level above -6 dBFS
   is flagged as "too hot" and one below -40 as "very low". They measure
   demodulated audio, not RF signal strength, and say so in the UI.
5. **Limits.** One run at a time per node, at least 2 min between runs,
   and roughly 12 frames of airtime per run. It's allowed in `setup` and,
   after a confirm, in `active`. It's not allowed in `complete`.
6. **Results.** Stored in `link_checks` on the prober. The responder keeps
   what it heard. The checkpoint admin page shows the history. HQ's health
   panel shows each checkpoint's latest result (time, verdict, u/n, level),
   whichever side ran it. **Start race** shows a warning, without blocking,
   if a checkpoint has no PASS in the last 2 h.
7. **Automation.** `checkin-board linkcheck [--to CALL] [--count N]
   [--json]` prints the result and exits 0 (PASS), 1 (MARGINAL) or 2
   (FAIL), so deployment scripts can use it. Optional: a graywolf
   **webhook Action** on each checkpoint's graywolf, posting to the app's
   local hook `POST /api/hook/linkcheck` (2.2, "Local hooks"). It lets
   net control trigger a check remotely with `@@<otp>#linkcheck`, and the
   one-line result comes back as graywolf's reply. The recipe is in
   `docs/linkcheck-action.md`. It's not required.

**As built (phase 12, 2026-10-06).** Differences from the text above,
and details it left open:

- **Downlink in the verdict.** "At least half in each direction" means
  uplink = probes heard out of probes sent, and downlink = ACKs received
  out of probes heard. An ACK can only come back for a probe that got
  there, and the earlier rule (ACKs out of probes sent) failed most runs
  at 40% loss even though each direction delivered 60%. An ACKed probe
  also counts as heard if the reply is lost. In simulation, over 20 seeds
  each with 5 probes:

  | Loss | Verdicts |
  |---|---|
  | 0% | all PASS |
  | 10% | 14 PASS, 6 MARGINAL |
  | 25% | mostly MARGINAL |
  | 40% | MARGINAL or FAIL |
  | 80% and 100% | all FAIL |

  Near the line, 5 probes give a noisy verdict, so the UI suggests 10.
- **Limits.** At most 10 probes per run. HQ's probes carry the station
  code `HQ`.
- **Responder.**
  - It answers only in `setup` and `active`, so a secured checkpoint
    stays quiet.
  - It makes at most 40 reply transmissions per hour, resends
    included, so forged probes can't fill the channel. graywolf's own
    ACK of each DM is outside the app's control.
  - It replies at once when it hears the last probe, otherwise 30 s
    after the last probe it heard. It tries at most three times
    (a first send and two resends), 30 s apart; a 409 in-flight
    conflict counts as a try.
  - It re-checks its role and HQ call before each send.
  - A probe from the same station and run number more than 30 min
    after the last one starts a fresh run. Responses are kept 7 days.
- **Prober.**
  - It finishes once the reply is in and every probe is ACKed (or 30 s
    after the last probe was actually sent). Otherwise it stops 2.5 min
    after the last probe.
  - Probes keep their spacing from the actual last send, so a stall never
    turns into a burst.
  - A request the service didn't start within a minute expires. A run is
    abandoned if it overruns its time by a minute, or if the race state
    or HQ call changes.
  - A run cancelled after it transmitted still counts for the 2-minute
    spacing.
  - The local level is read only from the peer's ACKs of this run's
    msgids and from its reply.
- **Concurrent writers.** The service tick, the inbox reader, the web UI
  and the CLI (another process) change runs and responses only through
  guarded updates (e.g. `WHERE state = 'running'`, `reply_acked_at IS
  NULL`). So a cancel, a reply or an ACK is never overwritten by a stale
  copy of the row.
- **Trust.** A reply is matched by callsign, run number and station code
  only. An on-air forger could fake a good result, as with any
  unauthenticated APRS traffic. The link check is a deployment aid, not
  a security control.
- **Delivery to the app.** Requests are rows in `link_checks`, so the
  CLI hands a run to the running service through the database and needs
  no network endpoint. `link_responses` keeps what the responder heard.
  Both are cleared by Reset.
- **Health panel and Start race.** HQ's health panel judges a run it only
  answered by what it heard and whether its reply was ACKed.
- **Remote trigger.** This uses a webhook Action, as described in 2.2,
  "Local hooks":
  - The hook is off unless `CB_HOOK_TOKEN_FILE` is set.
  - It answers only callers on the node itself (2.2) that send the bearer token.
  - It always replies 200 with one line: graywolf relays only "error:
    http NNN" for anything else.
  - It never starts a run during the race.

  See `docs/linkcheck-action.md`.

## 5. Storage (app-owned SQLite, embedded SQL migrations)

| Table | Side | Key columns |
|---|---|---|
| `settings` | both (singleton) | role, race_name, race_state, race_started_at, station_tactical, checkpoint_code, hq_local_codes, hq_call, gw_channel, path, max_text_len, flush_after_sec, max_in_flight, heartbeat_sec, gap_grace_sec |
| `peer_prefs_backup` | both | callsign, original wait_for_ack/send_path (restored on Complete) |
| `branding` | HQ (singleton) | header_text, footer_text, color_primary, color_accent, color_background, color_text (each `#RRGGBB`), updated_at (8.3; migration added in phase 9) |
| `branding_logo` | HQ (singleton) | image bytes (re-encoded, see 8.3), content_type, width, height, sha256, updated_at |
| `checkpoints` | HQ | code (unique), name, course_order, expected_call |
| `runners` | HQ | bib (unique), category; deliberately no personal-data columns |
| `local_entries` | CP | cp_code, bib, time_in, clock_synced, state (queued/sent/confirmed), void_of, batch_id |
| `batches` | CP | (cp_code, seq) unique, text, state (pending/acked/rejected), **gw_message_id, gw_msg_id, client_id**, attempts, last_tx_at, next_tx_at, acked_at |
| `gw_rows` | both | gw_message_id, kind (batch/heartbeat/gap/inbound), deleted_at: what cleanup (4.7.3) may delete |
| `received_batches` | HQ | (cp_code, seq, text) unique; **gw_message_id**, source_call |
| `received_entries` | HQ | append-only event log: cp_code, bib, time_in, is_void, batch_seq (NULL = HQ keypad), source_call, received_at |
| `cp_status` | HQ | cp_code, last_heard_at, last_call, heartbeat_at, heartbeat_last_seq, clock_skew_sec, max_seq, batches_received, seq_reuse_count, bad_reports |
| `bad_reports` | HQ | gw_message_id, from_call, text, error, received_at |
| `inbox_state` | both | cursor (singleton) |
| `link_checks` | both | run, role (prober/responder), peer_call, started_at, n, heard, acked, reply_received, rtt_median_ms, remote_lvl, local_lvl, via, verdict (phase 12) |
| `auth` | both | bcrypt hashes of the admin and volunteer passwords, session secret (migration added in phase 9) |
| `panel_menu` | both | position, label, action, confirm, roles, enabled (8.4); defaults seeded per role; kept by Reset |
| `panel_settings` | both | enabled, controller, refresh interval, rotation; last full refresh time (8.4) |
| `sessions` | both | token hash, role (`admin`/`volunteer`), created_at, expires_at; deleted on logout, on expiry, and for every session of a role when that role's password changes (phase 9) |

The station **callsign** isn't stored in the app: graywolf's
`/api/station/config` owns it (section 8.1).

Each phase adds the tables it needs as a new numbered migration
(`internal/store/migrations/NNNN_name.sql`, applied in order, each in one
transaction). Migration 0001 (phase 3) creates everything above except
`auth`, `sessions` and `link_checks`. Connections use `foreign_keys=ON`,
WAL, `synchronous=FULL` (so a confirmed write survives a power cut on
an SD card) and a single connection. gorm's automatic timestamps are
off, so every timestamp comes from the store's injectable clock.

ACK handling differs from `pkg/race`. A batch is matched to graywolf's
ACK by the graywolf **message row id** it is bound to (`BindMessage`;
partial unique index), not by a seq-derived msgid. Rebinding after a
resend 404 replaces the row. `UnboundAttempted` lists batches whose POST
may have succeeded before a crash (4.1.7). HQ records the inbox row in
`gw_rows` in the same transaction as the batch ingest.

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
| POST | `/api/admin/race/start`, `/complete` | both (4.7) |
| POST | `/api/admin/race/secure`, `/check-in` | CP (4.7) |
| POST | `/api/admin/peers/restore` | both: restore graywolf per-peer settings now |
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
| GET/PUT | `/api/admin/branding` | HQ: board branding (header, footer, colours); PUT validates (8.3) |
| PUT/DELETE | `/api/admin/branding/logo` | HQ: upload (multipart, ≤ 1 MB) or remove the logo |
| GET | `/api/branding`, `/api/branding/logo` | HQ, any session: what the board renders. The logo is served with its stored type, `X-Content-Type-Options: nosniff` and an ETag (its sha256) |
| POST | `/api/admin/linkcheck` | both: start a link check (`{to?, count?}`), 409 if one is running or rate-limited |
| GET | `/api/admin/linkchecks`, `/api/admin/linkchecks/{id}` | both: history / live progress |
| GET/PUT | `/api/admin/panel`, `/api/admin/panel/menu` | both: panel settings and menu (8.4); PUT validates labels and the action allowlist |
| GET | `/api/admin/panel/preview.png` | both: the panel's current screen, rendered as it would be on the display |
| POST | `/api/admin/panel/refresh`, `/api/admin/panel/test-pattern` | both: ask the panel for a full refresh (3-minute rule applies) or a controller test pattern |
| GET | `/api/hook/panel` | panel service only (local hook, 2.2): status snapshot, menu with availability, refresh due |
| POST | `/api/hook/panel/actions/{id}` | panel service only: run a menu item; the app checks role, state and allowlist |

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
  sets the admin password. It also requires a one-time **setup code**,
  which the server prints in its log at startup (`journalctl -u
  checkin-board`). That proves the person setting the admin password has
  access to the node, so a stranger on the hotspot can't claim a fresh
  node first. The admin then sets the volunteer password in
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
- **Brute force:** login attempts are rate-limited per client address and
  role (IPv6 grouped by /64). There are 5 attempts per minute, counted
  *before* checking so parallel guesses can't exceed it, then a lockout
  doubling from 1 to 15 minutes. Admin failures are logged. Password
  hashing is capped at two at a time, so a login flood can't starve the
  race engines of CPU. Setup refuses before any hashing once it's done.
- **Accepted risks (LAN deployment):** the app is served over plain HTTP
  on the node's own network. Passwords and session cookies can therefore
  be sniffed by anyone on that Wi-Fi, and the cookie can't be `Secure`.
  Mitigations are a WPA-protected hotspot and short sessions; TLS is
  post-MVP. Clients behind one NAT share a lockout key (phones on the
  node's own hotspot each have their own address). `reset-admin-password`
  shows the typed password on the terminal (it's a host-only command).
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
- **Race lifecycle:** Start / Complete / Secure / Final check-in (with live
  "N to deliver" progress) / graywolf cleanup / Restore graywolf peer
  settings / Reset (4.7), each with its state-dependent confirm.
- **CP tools:** outbox view (batches, attempts, next retry, msgid), recovery
  export.
- **HQ tools:** board (bibs only), bib search and history, checkpoint health
  (clock skew, missing / given-up batches with Re-request, wrong-sender and
  bad-report counters), results export, roster import, checkpoint editor,
  recovery and journal import.
- **Board branding (HQ):** the editor described in 8.3.
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

### 8.3 Status-board branding (new, 2026-10-06)

The HQ status board (the board, runner lookup and checkpoint health
views) can carry the event's own look. It is configured on the admin
page and applies to those views only. The keypad and admin pages keep
the app's neutral, high-contrast theme, so volunteers' screens look the
same at every event.

| Setting | Rules | Default |
|---|---|---|
| Logo | PNG, JPEG or WebP, ≤ 1 MB upload, ≤ 4096 px per side. **No SVG** (it can carry script). The server decodes the image and stores a re-encoded PNG scaled to at most 512 px tall, so a crafted file never reaches the browser as uploaded | none |
| Header text | 0-80 characters of printable text (same rules as names: no control or bidi-override characters); plain text, never HTML | the race name |
| Footer text | 0-200 characters, same rules | empty |
| Colours | primary (header bar), accent (highlights, latest passage), background, text. Each is `#RRGGBB`. | the app's neutral theme |

- **Readability is enforced, not left to taste.** Text on background, and
  header text on the primary colour, must reach WCAG AA contrast (4.5:1).
  The save is rejected otherwise, and the editor shows the ratio as you
  pick, because the board is read on a tablet outdoors.
- **Live preview.** The editor renders a sample board with the pending
  values before saving. **Reset to default** restores the neutral theme.
- **Rendering.** Colours become CSS custom properties on the board page,
  set from the validated values only (never raw input in a style
  attribute). Header and footer are inserted as text nodes. The logo is
  an `<img>` from `/api/branding/logo` with alt text set to the header
  text.
- **Scope.** Branding is HQ data, because the board lives at HQ.
  Checkpoints don't need it, and it never goes over RF. Reset keeps it
  unless its checkbox is ticked (4.7).
- **Exports.** The results CSV is unbranded (it's data for the timing
  crew). Printing the board uses the branding, with background colours
  dropped for paper.

### 8.4 Node panel: e-ink status display and buttons (new, 2026-10-08)

> **Decision 2026-10-09: status display only; buttons and menu shelved.**
> On the bench, none of five partial-refresh sequences drew anything on
> the bonnet (the presses arrived; the screen didn't change). Without
> partial refresh, each button press costs a ~2 s full refresh. The user
> judged that too slow, and too much wear, for a shallow menu.
>
> So the panel shows the status screen only:
> - The button reader is off by default (`checkin-board panel -buttons
>   none`).
> - The menu editor is gone from Admin → Panel.
> - The controller defaults to SSD1680Z (the detection wizard needs a
>   press). Others are chosen on Admin → Panel with the test patterns.
>
> The menu, wizard and partial-refresh code stays in the tree, tested,
> for a panel that can refresh fast enough. The text below describes the
> full design.

Each node's Raspberry Pi carries an [Adafruit 2.13" e-ink
bonnet](https://learn.adafruit.com/2-13-in-e-ink-bonnet). It has a
250×122 monochrome display and two buttons. The panel shows the node's
status at a glance, updated every few minutes. The two buttons drive a
short menu of commands (run a link check, open or close the checkpoint,
final check-in at HQ…). The menu is edited on the admin page. The panel
is optional: a node without a bonnet runs exactly as before.

**Hardware** (from Adafruit's guide):

| Item | Detail |
|---|---|
| Display | 2.13", 250×122, monochrome e-ink |
| Controller | Depends on the bonnet revision: SSD1675 (original), SSD1680 ("legacy"), SSD1680Z (current). Adafruit's advice is to try each. Nodes will carry a mix, so the panel finds the right one itself (below) |
| Interface | SPI0 with CE0 (GPIO 8), MOSI GPIO 10, SCLK GPIO 11; DC GPIO 22, RST GPIO 27, BUSY GPIO 17 |
| Buttons | GPIO 5 (top) and GPIO 6 (bottom), active low with pull-ups |
| Refresh | Adafruit: don't refresh more often than every 3 minutes long-term, or the panel can be damaged. SSD1680 parts also support a fast partial refresh |
| Other header users | None: the radio connects through an [AIOC](https://github.com/skuep/AIOC), which is all USB (sound card, virtual serial port, CM108-style PTT). Phase 14 checks graywolf's PTT is set to the AIOC, not a Pi GPIO |

**Finding the controller.** Bonnet revisions will be mixed across nodes
(user, 2026-10-08), so the controller is detected per node and saved in
`panel_settings`:

1. **Read-back probe** (if the bonnet wires it): read a controller
   register over SPI and match the chip. Phase 14 checks whether the
   bonnet allows this.
2. **Button wizard** (the fallback, on first start or after "Detect
   again"):
   - The panel draws a large "Press a button if you can read this" with
     each driver in turn, about 20 s each.
   - The first press picks that driver.
   - Unconfirmed drivers are tried again in a loop, so one person at the
     node finishes setup with no laptop.
3. **Manual override** on Admin → Panel, with a test pattern per driver.

#### Architecture

- **A separate process: `checkin-board panel`** (same binary), run as its
  own systemd service, `checkin-board-panel.service`. Only this process
  touches hardware.
  - It gets `/dev/spidev0.0` and `/dev/gpiochip0`
    (`SupplementaryGroups=spi gpio`, `DeviceAllow=`).
  - The main service keeps `PrivateDevices=yes` and its other
    sandboxing.
  - A display or GPIO fault can't stop race reporting. If the main app
    is down, the panel says so.
- **It talks to the app only through local hooks** (2.2): on the node, with a
  bearer token. `install.sh` generates the token and gives it to both
  services, so the panel needs no login.
  - `GET /api/hook/panel` returns the status snapshot and the menu with
    each item's availability.
  - `POST /api/hook/panel/actions/{id}` runs one menu item.
  - The app, not the panel, checks every action against the node's role
    and race state.
- **Pure Go**, like the rest of the app. It uses periph.io for SPI and
  GPIO (no cgo). The display driver is a small `internal/panel/epd`
  package with the three controllers' init and refresh sequences. Its
  starting points are periph.io's pure-Go SSD1680 driver (Waveshare 2.13"
  v3/v4) and Adafruit's MIT-licensed driver as reference, with
  attribution. Rendering is `image/draw` onto a 1-bit image with a
  bundled bitmap font.
- **Testable without hardware.** The renderer produces an `image.Image`.
  A fake display records frames, and golden-PNG tests pin every screen.
  The admin page shows a live preview of the panel
  (`/api/admin/panel/preview.png`), so the layout can be checked from a
  browser.

#### Status screen

The status screen uses five or six lines of a small fixed font, and its
content depends on the role:

- **Checkpoint:** tactical name and checkpoint code; race state;
  "N unconfirmed, HQ heard 3 min ago"; graywolf OK or DOWN; the last link
  check (verdict and when).
- **HQ:** race state; checkpoints heard out of those listed, and how many
  are closed or checked in; open gaps; graywolf OK or DOWN.
- **Both:**
  - the web address volunteers should open (`http://<node IP>:8090`);
  - warnings that need attention (race clock not set, journal-only
    saves, graywolf login failing);
  - the time of the update, so a stale screen is obvious.
- **Cadence:**
  - a full refresh every 5 minutes by default (adjustable from 3 to 30;
    never under 3);
  - an important change (race state, graywolf lost, a warning) moves the
    next refresh up, but never sooner than 3 minutes after the last;
  - nothing is refreshed when nothing changed.

#### Buttons and menu

The bonnet has only two buttons, so the scheme stays simple:

- **Top = next, bottom = select.** Any press on the status screen opens
  the menu.
- The highlighted item moves with **partial refreshes** (fast, about a
  second), rate-limited to one per second.
- A menu session ends with a full refresh back to the status screen, on
  **Back** or after 30 s idle.
- **Confirmation.**
  - An item marked "confirm" shows "Press SELECT again to confirm, NEXT
    to cancel". Lifecycle actions are always confirmed.
  - The result ("Link check started", "Can't: race not started") shows
    for 10 s.
  - On panels without partial refresh (SSD1675), navigation uses full
    refreshes. It's slower, and the 3-minute rule is relaxed only while
    someone is using the buttons.
- **Visibility.** Items not available in the current role and state are
  hidden. For example, "Open checkpoint" shows only before the start.

#### Menu items (editable on Admin → Panel)

Each item has:

- a label (up to 20 characters, plain text);
- an **action from a fixed allowlist**;
- confirm on or off (forced on for lifecycle actions);
- the roles it shows for;
- enabled on or off.

The admin can add, remove, rename, reorder and disable items, preview
the panel, and send a refresh (subject to the 3-minute rule). The menu
is stored in the app database and kept by Reset, like branding.

| Action | Does | Default label (role) |
|---|---|---|
| `status` | Back to the status screen | Status (both) |
| `link_check` | Start a link check (4.8). Checkpoint: to HQ. HQ: pick a checkpoint from its list on a second menu. During the race this needs the confirm step, which counts as the operator's confirmation | Run link check (both) |
| `start_race` | Start race (4.7): on a checkpoint, opens this checkpoint only | Open checkpoint (checkpoint), Open race (HQ) |
| `complete_race` | Complete race: on a checkpoint, closes this checkpoint only (keypad off, backlog sent, HQ told it's closed) while the race continues | Close checkpoint (checkpoint), Close race (HQ) |
| `secure` | Secure for travel | Secure for travel (checkpoint) |
| `check_in` | Final check-in | HQ check-in (checkpoint) |
| `show_network` | Show the node's addresses and the web URL | Network (both) |
| `show_link` | Show the last link check in detail | Last link check (both) |
| `refresh` | Full refresh now (subject to the 3-minute rule) | Refresh screen (both) |

- **Never on the panel:** Reset, graywolf cleanup, password and callsign
  changes, and anything else that loses data or changes graywolf. They
  stay on the admin page.
- **Logging.** Panel actions are logged like admin actions, with source
  `panel`.
- **Physical access.** Anyone standing at the node can press the buttons.
  The confirm step and the admin's choice of items are the safeguards.
  An optional button-sequence lock is a later option.

#### Settings (Admin → Panel)

- panel on or off;
- controller: detected (above), with "Detect again", a manual choice and
  a test pattern per driver;
- status refresh interval;
- rotation (0° or 180°);
- the menu editor.

The panel service polls the app (default 30 s) and the app decides when a
refresh is due, so changes made in the UI apply without a restart.

## 9. Delivery phases (each one PR-sized, TDD, 80%+ coverage, `-race`)

**Status at a glance (2026-10-09).**

- **Done:** phases 1–11 and 13–15.
- **Phase 12** is done except the parts that need radios: (g) the RF
  contract tests and the multi-node radio bench, and the rest of (h)
  (CPU with graywolf decoding real audio).
- **Phase 16** (field rehearsal) has not started.
- **Node panel:** a status display only. Its buttons and menu are
  shelved (8.4).
- **Test nodes** (Pi Zero W, Raspbian 13, both with an e-ink bonnet):

  | Node | Notes |
  |---|---|
  | 10.0.0.65 (Checkin-Board) | No radio connected yet. App set up. |
  | 10.0.1.183 (Checkin-Board2) | AIOC connected. Built from bare by `install.sh`, graywolf included. |

  Both run the current build, with SPI on and Wi-Fi power saving off.
  Neither graywolf has a radio channel configured yet; that is graywolf's
  own setup.

| # | Phase | Output | Status |
|---|---|---|---|
| 0 | Sign-off | This spec approved | Answers received 2026-10-05; awaiting final approval |
| 1 | graywolf client + API spike | `internal/graywolf`: login/re-login, health, version, station get/put, send, get, resend, delete, list+cursor, SSE, conv prefs, preferences. `httptest` fakes for unit tests, plus an **opt-in contract test** (`GW_CONTRACT=1`) against a real graywolf 0.14.14 that proves: msgid is stable across resend; `wait_for_ack=false` stops the ladder and late ACKs still flip status; `client_id` round-trips (or not); the inbox cursor doesn't skip rows; single-row delete leaves the thread intact | **In progress** 2026-10-05. Client done, 96.3% coverage, reviewed (fixes: watchdog pauses during handler and covers connect; stalled-cursor detection; 30 s fail-fast after bad credentials; no credentials in URLs, errors or `%v`). Contract test written (`make contract`), **not yet run**: live transmit deferred until the radio hardware is ready (user, 2026-10-06), so it runs in phase 12. Test host found on the LAN (0.14.14, commit `b589e686`) |
| 2 | Wire codec + race clock | Port `types/encode/decode/clock` from `pkg/race` into `internal/wire`, `internal/raceclock`; add `RC1 P/Q` link-check messages (4.8); table tests + `FuzzDecode` | **Done** 2026-10-05. wire 95.6%, raceclock 100% coverage; FuzzDecode 68M execs clean (now covering P/Q); gap-list parser shared with P/Q (behaviour unchanged, reviewed). Worst-case `RC1 Q` is exactly 67 chars, pinned by an exhaustive test. Clock types renamed `raceclock.Source`/`Status`; the OS-clock check (section 6) is injected later |
| 3 | Storage | SQLite (modernc), embedded migrations, repositories, roster CSV + `FuzzParseRosterCSV` | **Done** 2026-10-06. 90.8% coverage; FuzzParseRosterCSV 60 s clean; builds for ARMv6 with `CGO_ENABLED=0`; `govulncheck` clean (bumped modernc sqlite to v1.60.1 / SQLite 3.53.4 and x/text). Review fixes: gap requests release acked/rejected rows (above, 3.1); bad reports recorded in `gw_rows` and counted only against known checkpoints; UTF-8-safe truncation; `hq_call` SSID limited to 0-15. Also fixed a ported test that checked the old `race_runners` table name and so passed vacuously |
| 4 | Inbox reader | SSE + catch-up, cursor persistence, idempotent dispatch, reconnect/backoff | **Done** 2026-10-06. 95.1% coverage, stable over 30 `-race` runs. Review: no lost-row paths; fixed one bad row blocking the feed (skip after 10 failures or on `Permanent`), backlog drain, a truthful Connected flag (new `StreamEventsWithOpen` client hook), separate stream/catch-up errors, mark-read retry, cursor save on shutdown, and backoff reset only after a healthy stream |
| 5 | Checkpoint engine | Batcher, outbox on `/messages` + `/resend`, ACK watcher, fast retransmit, gap-request handler, heartbeat, crash-safe POST (4.1.7) | **Done** 2026-10-06. `internal/checkpoint` 89.7%, plus `internal/gwfake` (fake Messages API, 95.6%) and `internal/peers` (`wait_for_ack` backup and restore, 3.3). Review fixes: post-send writes on a detached context; ambiguous send outcomes kept and recovered instead of duplicated; Recover can't take an earlier race's row; poll before transmit; schedule writes conditional on the batch still being pending and unchanged; 400 alerts instead of parking; replayed ACKs aren't contact. Wiring into `main` waits for phase 6 (role-aware dispatcher) |
| 6 | HQ engine | Ingest, bad-report capture, gap tracker + sender, heartbeat/skew, sender check | **Done** 2026-10-06. `internal/hq` 90.8% (ingest of graywolf-ACKed rows, bad reports, gap tracker, health view with sender mismatch and never-heard checkpoints, operator re-request). Also wired the service: `internal/app` (role-aware dispatcher, assembly, graywolf-prefs refresh) and `main`; cached `peers.Ensurer` used by both engines; gwfake feed and event stream now behave like graywolf's. Review fixes: an unconfigured node holds race traffic (`ErrNotReady`) instead of dropping it; the inbox starting point is saved once (migration 0002); gap requests only for listed checkpoints once a list exists; per-checkpoint send-failure backoff; 5 s HQ tick; faster prefs retry |
| 7 | Integration test | Two app instances against a **fake graywolf**: an in-memory Messages API written from the published API (auto-ACK, dedup, `wait_for_ack`, a lossy link with drop / reorder / dup). Asserts eventual exactly-once at HQ; re-runs the 9b latency table and the spoofed-traffic airtime bounds | **Done** 2026-10-06. `internal/gwfake/radio.go` (shared lossy channel: graywolf-style dedup and auto-ACK; model verified) and `internal/sim` (whole nodes in simulated time). Eight scenarios pass exactly-once: mass start at 30% loss, voids, out-and-back, 40-min outage, three checkpoints, HQ restored from an old backup (heartbeat reveals the hole), HQ DB wiped (recovered from graywolf's inbox with no RF), and spoofing bounds. Latency results and the revised acceptance are in 9b |
| 8 | Recovery, journal, lifecycle | Journal, CP export, HQ import, re-request; Start / Complete / Secure / Final check-in / graywolf cleanup / Reset with backup (4.7) | **Done** 2026-10-06. `internal/journal` (port, 84.9%) and `internal/ops` (86.1%): keypad with power-safe journal, CP export (now with graywolf msgid) and HQ export/journal imports, board, runner history and results CSV, lifecycle. New **secured** phase and **final check-in** (user request); simulated end to end (`TestSecureTravelAndFinalCheckIn`). Review fixes: every state change is a compare-and-set (`SetRaceState`), and admin edits use `UpdateSettings`, which never touches lifecycle columns, so the engine, operators and edits can't overwrite each other; check-in starts only on success and is judged by local time; parked batches revived during check-in; reset closes the keypad first, then backs up, wipes, rotates the journal and restores peers; cleanup is blocked during check-in; the after-check-in hook is retried |
| 9 | App REST + auth | Handlers, DTO validation, volunteer/admin login and role middleware (table test: every route × no session / volunteer / admin), first-run setup, password change + session revocation, `reset-admin-password` CLI, rate limit, session expiry, graywolf credential handling. **Branding (8.3):** migration + store, validation (text rules, `#RRGGBB`, WCAG contrast), logo decode/re-encode (PNG/JPEG/WebP only, size and pixel caps), branding endpoints; fuzz the logo decoder with malformed images | **Done** 2026-10-06. Migrations 0003 (auth, sessions) and 0004 (branding). `internal/auth` (89.0%), `internal/branding` (96.9%; logo decoder fuzzed 60 s; pixel cap lowered to 12 MP after a test showed a 4096² image slipped through), `internal/web` (81.2%: 55 routes in a role-guarded table; the access test walks every route as nobody, volunteer and admin). `main` serves the API on `CB_LISTEN` (default `:8090`) with timeouts and graceful shutdown, purges sessions hourly, and has a `reset-admin-password` command. Smoke-tested as a real binary. Security review: no bypass, CSRF or upload hole. Hardening applied: setup code, setup refuses before hashing, bcrypt concurrency cap, attempt-first lockout with IPv6 /64 keys that fails closed when full, `Sec-Fetch-Site` check, stricter CSP, large uploads spill to disk, the volunteer banner never shows raw graywolf errors, and a wrong current admin password returns 400, not 401. `cmd` coverage is 10.5% (wiring; covered by the smoke run) |
| 10 | Web UI | Admin + volunteer interfaces; JS unit tests for logic; scripted browser run of the real binary against the fake graywolf. **Branded status board (8.3):** CSS custom properties from saved branding, header/footer/logo, branding editor with live preview and contrast readout, print stylesheet | **Done** 2026-10-06. Plain HTML + ES modules in `internal/web/static`, no build step, no inline script or style (CSP holds; branding colours go through CSSOM after a hex check). Pages: login/setup, keypad, admin (Race, Station, Outbox, HQ, Branding, Passwords; tabs by role), branded status board with print stylesheet. All text goes through `textContent`. UI logic in `logic.js`, 13 `node --test` tests (`make jstest`). Keypad retries are idempotent: a `request_id` per bib, reused while a save might have happened; the server answers a retry from a 10 min cache, refuses a reused id for a different bib (422) and holds its cap. Browser E2E in headless Chrome against the whole server on the fake graywolf (`make e2e`, opt-in, chromedp is test-only and not in the binary): login, keypad log / double tap / void, admin tabs and settings save, HQ checkpoints, branding save, board. `internal/web` 81.9%. Review fixes: keypad keeps the id on 5xx/409-in-progress/unreadable replies, blocks double saves, sequences list refreshes; admin renders tabs without interleaving, follows lifecycle changes by polling, and survives a failed first load |
| 11 | Packaging + docs | `GOARM=6` build, systemd unit (`After=graywolf.service`), install script, operator README incl. recovery and reset procedures | **Done** 2026-10-06. `make pi` / `make dist` (ARMv6 bundle: binary, unit, env template, `install.sh`, operator guide); `checkin-board version` (ldflags version + VCS revision). `deploy/checkin-board.service`: own system user, `StateDirectory`, `After=graywolf.service` (no network-online wait; field nodes often have no uplink), strict sandboxing with a soft-fail syscall filter, `GOMEMLIMIT=80MiB`, start-limit on config errors. `install.sh` is idempotent: never overwrites settings or the password, copies the DB aside before an upgrade, restarts the old version if the upgrade fails, refuses to write through symlinks in the service-owned state dir. Tested in Debian containers (arm64, and the ARMv6 bundle on armhf under emulation): install, re-run, upgrade, symlink refusal. `docs/operator-guide.md`: install, first-run setup, race day by role, recovery, lost admin password, reset. Idle RSS on Linux arm64 ≈ 20 MB; the Pi Zero figure and a syscall-filter smoke test wait for 12(h) |
| 12 | Test campaign + deployment link check | **Link check (4.8):** `RC1 P/Q` behaviour on both sides, admin UI, HQ health column, Start-race warning, `checkin-board linkcheck` CLI with exit codes, Action recipe. **Test campaign:** (a) ≥80% coverage in every package; `go vet`, `staticcheck`, `govulncheck`. (b) Long fuzz runs (30 min each): `FuzzDecode`, roster CSV, journal reader. (c) Soak: simulated 12 h race, 500 runners, 8 checkpoints, 20% loss through the fake graywolf; asserts exactly-once, flat memory, bounded DB growth. (d) Fault injection: `kill -9` between POST and store and between batch and send; graywolf restart, password change and SSE drop mid-race; disk full; torn journal tail after a power cut; OS clock step; checkpoint reset mid-race (seq reuse). (e) Security: auth matrix, Content-Type guard, rate limits, CSV injection, upload limits, hostile logo files (SVG, polyglots, decompression bombs) and branding text (HTML, bidi overrides). (f) Browser E2E at phone width: keypad log/void, network loss, clock banner, admin lifecycle. (g) Real-graywolf bench: contract tests (fast + slow) against the deployed version, then 2-3 graywolf nodes on real radios (low power / dummy loads) replaying a scripted 100-runner race, plus a link check between every node and HQ. (h) Pi Zero W: RSS < 100 MB, CPU during a 20 bibs/min surge, startup time. **Exit criteria:** all green, 9b latency within acceptance, link check PASS on every bench pair; results written to `docs/test-report-<date>.md` | **In progress** 2026-10-06. **Link check done** (4.8, as built): `internal/linkcheck` (87.1%) ticked by the app and simulated end to end; admin tab, HQ health column, Start-race warning; `checkin-board linkcheck` CLI (exit 0/1/2, `--json`, `--brief`); optional webhook Action recipe (2.2, local hooks); review fixes (guarded store transitions, no probe bursts after a stall, stale requests expire, resends budgeted). **Test campaign:** (a) every package ≥80% except `cmd`; `go vet`, `staticcheck` clean; `govulncheck` one unfixed, uncalled `x/crypto` advisory. (b) all five fuzz targets 30 min each, ≈428 M executions, no failures. (c) 12 h soak, 500 runners, 8 checkpoints, 20% loss: exactly once, ≈90 bytes per passage at each checkpoint. (d) graywolf API outage and repeated kill/restart keep exactly once; other faults mapped to existing tests. (e) polyglot logo, safe hook text. (f) 8 browser E2E tests. Report: `docs/test-report-2026-10-06.md`. **Waiting for hardware:** (g) and (h) |
| 13 | Node panel software + close checkpoint | **4.7:** "Open/Close checkpoint" wording on checkpoints, the heartbeat closed flag (` C`, codec + fuzz) and HQ's "closed" health state. **8.4:** status and menu model; 1-bit renderer with golden-PNG tests; panel loop with the refresh rules (3-minute floor, change-driven, partial only while navigating); fake display and fake buttons; `panel_menu` and `panel_settings` migrations with per-role defaults; action allowlist enforced in the app; local-hook panel endpoints; Admin → Panel tab (menu editor, settings, live preview); `checkin-board panel` subcommand; `checkin-board-panel.service` with device access; install script installs it only when SPI is enabled and a bonnet answers | **Done** 2026-10-08. **Close checkpoint:** a checkpoint's Start/Complete read Open/Close checkpoint; heartbeats carry ` C` once closed (codec + fuzz), HQ records `closed_at` and shows "closed HH:MM" instead of "quiet"; simulated (`TestCheckpointClosesWhileRaceContinues`). **Panel:**<br>- `internal/panel/menu`: the allowlist, per-role defaults and validation. Lifecycle items are always confirmed. Labels are ASCII only, because the display's font is.<br>- `internal/panel`: a 1-bit renderer with eight golden-PNG screens, and the state machine with the refresh rules. The full-refresh floor is also remembered by the app (`panel_settings.last_full_at`), so a restarting panel can't refresh faster than every 3 minutes.<br>- Menus, confirmation and the HQ link-check target picker. The controller wizard gives up after three passes over the controllers.<br>- Test patterns.<br>- Admin → Panel tab: settings, the menu editor and a live preview.<br>- Local-hook endpoints. The app re-checks role, state and allowlist, and refuses a stale menu ID.<br>- `checkin-board panel` with a PNG display and keyboard buttons; dry-run against the real app.<br>- Review fixes:<br>  - every full-refresh attempt counts against the floor, failed ones too;<br>  - the wizard's first frame waits for the floor, and "gave up" is remembered by the app, so only "Detect again" restarts it;<br>  - test patterns wait for the floor and use one hardware handle at a time;<br>  - interactive refreshes are budgeted (6 full or 60 partial per 3 min), so a stuck button can't wear the panel;<br>  - recovery from "app down" redraws at the floor;<br>  - a 600 ms guard stops a bounce confirming;<br>  - link checks are always confirmed;<br>  - actions carry the menu revision;<br>  - an app restart doesn't replay requests;<br>  - the floor never moves back;<br>  - closed sticks against late heartbeats.<br>- Coverage: panel 89.2%, menu 84.1%, epd 100%, web 80.9%. 10 browser tests.<br>- **Moved to phase 14:** the panel's systemd unit and installer, because they're useless without the hardware driver. |
| 14 | Node panel on hardware | `checkin-board-panel.service` (own unit with SPI/GPIO access; moved from 13) and the installer: generate the hook token for app and panel, install the panel only where SPI is enabled; the bonnet's GPIO buttons (debounced); `internal/panel/epd` drivers for SSD1680Z, SSD1680 and SSD1675 on periph.io; controller detection (read-back probe if wired, else the button wizard) and test patterns; check graywolf's PTT is on the AIOC; bench on the test node (checklist: each controller, both rotations, partial vs full refresh, button debounce, idle timeout, app down, graywolf down, reboot); measure refresh times and the panel service's RSS on the Pi | **Done** 2026-10-09 (status display; buttons and menu shelved, see 8.4).<br>- `internal/panel/epd`: SSD1680Z, SSD1680 and SSD1675 sequences after Adafruit's MIT driver, with partial refresh on the SSD1680 parts (compare old/new RAM, mode 0xFC) and deep sleep after each refresh. Frame packing and every sequence are tested over a recording bus (97%); the hardware itself lives in `epd/bonnet`, tested on the bench.<br>- The periph.io bus: SPI0.0 plus DC, RST and BUSY, with a BUSY timeout.<br>- The bonnet buttons (GPIO 5/6, falling edge, 150 ms debounce).<br>- `checkin-board panel -test CONTROLLER` (timed test pattern and partial refresh).<br>- `checkin-board-panel.service` (own user, spi/gpio groups, sandboxed).<br>- The installer: hook token, panel started only where SPI is on; container-tested.<br>- On the test node: Pi Zero W, Raspbian 13 (trixie), AIOC (USB). SPI was off and has been enabled.<br>**Bench 2026-10-09 (10.0.0.65):**<br>- The node came back after the reboot. It had only been unreachable for a while: Wi-Fi power saving is on.<br>- **Controller:** the wizard ran and the user confirmed **SSD1680Z** ("readable, text very small"). The font then moved to bold 8×16.<br>- **Timings:** full refresh 2.17 s, partial 0.39 s. SSD1680 and SSD1675 time the same on this panel (2.06 s and 2.35 s), so BUSY timing can't identify the controller, and the button wizard stays.<br>- **Memory (RSS):** app 22.5 MB, panel 15.8 MB, graywolf 34.5 MB.<br>- The panel survives the app stopping.<br>- No blocked system calls under the services' filters.<br>- graywolf has no channel or PTT yet (an AIOC is USB-only).<br>- **Font:** the larger font was confirmed readable, including at dawn and dusk.<br>- **Buttons:**<br>  - First bench: the buttons "did nothing". The presses were detected (the journal shows menu draws), but the first menu draw after a panel restart was a partial refresh with no previous frame, and it errored. Fixed: that case now does a full refresh.<br>  - Edge events stay the default; `-buttons poll` (50 ms) is kept as a fallback for kernels whose edges fail.<br>  - Every press is logged.<br>- **CPU, idle, over 60 s:** graywolf 3.8%, graywolf-modem 0.8%, app 1.9%, panel 0.6% (3.2% with 20 ms polling, so edges stay).<br>- **RAM:** 426 MB total. The Pi was low on memory because the bench's own package copies filled the RAM-backed `/tmp` (208 MB). Removed: 265 MB is now available. `/tmp` stays the place for install packages, since it spares the SD card; delete them after installing. The installer now turns Wi-Fi power saving off (user, 2026-10-09).<br>- **Partial refresh:** five sequences were benched (OTP mode 2 with 0xFC or 0xFF, with and without temperature-sensor and RAM options, and an explicit mode-2 LUT). The user watched each run: none drew. Partial refresh stays off. |
| 15 | Installer: graywolf bootstrap | **2.3:** `install.sh` installs the latest graywolf release for the node's architecture when it's missing (GitHub latest release, `.deb` for `dpkg --print-architecture`, SHA-256 against `checksums.txt`, `apt install`), never upgrades an existing one, sets up the operator's graywolf admin login before the app's, and warns on a version other than the tested one; `--graywolf-version` pins; offline continues without it. Container tests against a fake release server; one real install on a spare SD card | **Done** 2026-10-09:<br>- `install.sh` options: `--graywolf-version`, `--no-graywolf`, and `GW_RELEASES_URL` for tests.<br>- Release JSON is parsed with `sed`; downloads use curl or wget into a `mktemp` directory in `/tmp`, removed after; the SHA-256 is checked against the release's `checksums.txt`; then `apt-get install` of the local `.deb`.<br>- The admin login is created before the app's login.<br>- An installed graywolf only gets a version note.<br>- Container tests against a stub package and `file://` releases: fresh install, re-run, checksum mismatch (aborts), offline (continues), `--no-graywolf`.<br>- A real end-to-end run in an arm64 container: graywolf 0.14.14 from GitHub, and its CLI listed `admin`, then `checkin-board`.<br>- Review fixes:<br>  - every step is checked explicitly (`set -e` is off in `||` calls), with apt's errors shown and a dpkg lock timeout;<br>  - graywolf is installed before the app is stopped for an upgrade, and the downloads have timeouts and are HTTPS-only;<br>  - interrupts go through the cleanup trap;<br>  - an "installed" graywolf must be "install ok installed";<br>  - release versions are validated before use;<br>  - the checksum line is matched exactly (an empty list never passes);<br>  - the admin login is retried with graywolf's error shown, and if it fails on a fresh graywolf, the app's login is held back so the "Create Admin Account" screen survives;<br>  - `--help`, `--opt=value` and `--` are handled.<br>- New container cases: a hostile version, a broken package (apt fails), and no admin password (app login held back, graywolf left with no users). |
| 16 | Field rehearsal | Deploy to real locations; **run the link check at every node first**; then a walk-around on the course | Not started |

Phase 1 comes first on purpose: every later phase rests on graywolf
behaviours that this design assumes but hasn't tested through the API.

## 9b. Delivery latency

The original's measured table (`pkg/race`, phase 5) applies only if the app
reproduces the same on-air behaviour: window 4, fast retransmit,
30/60/120/300 s ladder, one frame per (re)transmit. Section 3.3 is what
keeps that true. Phase 7 re-ran the simulation (300-runner mass start,
10% dup, up to 3 s delay) through the fake graywolf
(`CB_LATENCY_TABLE=1 go test -run TestLatencyTable ./internal/sim`).

**Measured 2026-10-06, 100 seeds** (drain = last bib to all confirmed;
± is the 95% CI of the mean):

| Channel loss | checkin-board | Original "Shipped" (5 seeds) | Dead-link frames/h |
|---|---|---|---|
| 10% | 29 s ± 15 s | 22 s | 13 |
| 20% | 1 m 47 s ± 42 s | 37 s | 17 |
| 30% | 4 m 38 s ± 1 m 6 s | 3 m 2 s | 25 |
| 40% | 21 m 0 s ± 2 m 25 s | 16 m 4 s | 30 |
| 50% | 1 h 5 m ± 5 m | 42 m 41 s | 40 |

Findings:
- **Dead-link airtime** is within the original's 12-42 frames/h.
- **Drain** is 1.3-1.6× the original's figures, except at 10%. Tracing the
  slow runs showed the cause is **the last batch**. Nothing newer follows
  it, so no confirming ACK can fast-retransmit it, and it rides the
  30/60/120/300 s ladder. Each attempt needs both the frame and its ACK to
  survive ((1-p)², 25% at 50% loss).
- The drain distribution is therefore heavily skewed. A 5-seed mean is
  dominated by whether a rare tail stall happens, and is biased low. The
  original's 5-seed figures can't distinguish "slower" from "luckier": at
  30%, a 30-seed run (3 m 43 s ± 1 m 47 s) contains the original 3 m 2 s.
- The channel model was verified (delivered/sent = (1-loss)(1+dup)), and
  the engine code paths match the original's. A definitive comparison
  would need the original simulation re-run with 100 seeds. That run
  touches the graywolf repo, so it's left for the user to decide.
- **Acceptance (revised, pending user confirmation):** the test is a
  regression guard against the 100-seed baseline above. It fails if a
  loss rate's mean drain, minus its CI, exceeds 1.25× the baseline, or if
  dead-link airtime exceeds 42 frames/h. The original "within 25% of the
  5-seed figures" criterion was statistically unsound.
- **Possible tail improvement (not done; a design change):** let a
  heartbeat ACK, which proves the link works, expedite only the *oldest*
  pending batch, at most once per heartbeat interval. That bounds spoofed
  airtime the same way confirming ACKs do. Field data should decide this.
- **Spoof bounds** (`TestSpoofedTrafficBoundsAirtime`, a fresh msgid per
  forged frame): forged ACKs (unknown or replayed) cost 60 frames/h, the
  normal ladder. Forged gap requests in HQ's name cost 252 frames/h
  (bound 260, about 4/min from `requeueMinAge` × window, as designed).
  Strangers' gap requests are ignored.
- **To check on real graywolf (phase 12):** does graywolf's (from, msgid,
  text) dedup window slide with each repeat (as the fake assumes) or is it
  anchored to the first copy? It doesn't affect correctness: the app
  dedups batches itself.

Reference (original, shipped): 10% loss 22 s; 20% 37 s; 30% 3 m 2 s; 40%
16 m 4 s; 50% 42 m 41 s. Dead link: 12-42 frames/h per checkpoint.

Reference (original, shipped): 10% loss 22 s; 20% 37 s; 30% 3 m 2 s; 40%
16 m 4 s; 50% 42 m 41 s. Dead link: 12-42 frames/h per checkpoint.

Extra delay to account for: SSE notification plus a fetch on each ACK and
each inbound row. That's milliseconds on localhost, negligible against
RF. (The simulation charges a full 1 s step per hop, which adds a few
seconds at low loss.)

## 10. Risks

| Risk | Mitigation |
|---|---|
| graywolf API changes between versions | Version check at startup; contract test (phase 1) run before each race against the deployed graywolf version |
| A deployed node can't reach HQ, discovered only once runners arrive | Deployment link check (4.8) at setup; Start race warns about any checkpoint without a recent PASS |
| Link check probes are spoofed or replayed | Responder answers only expected callsigns (CP: HQ only); probes carry no data that changes race state; rate-limited per node |
| `wait_for_ack=false` not available or behaves differently | Fallback in 3.3 (resend only on `timeout`), with an airtime warning |
| Race peers' graywolf prefs left modified after a crash | Originals saved in `peer_prefs_backup`; restored on Complete; also listed on the admin page with a "Restore now" button |
| graywolf ACKs reports the app then fails to ingest | graywolf's inbox is durable; the cursor only advances after commit; the next catch-up retries |
| Undecodable report can't be REJected | Stored in `bad_reports`, shown per sender at HQ; CP export is the recovery path |
| Race traffic clutters graywolf Messages | Mark-read during the race; cleanup only after Complete (4.7) |
| E-ink panel worn out by refreshing too often | 3-minute floor on full refreshes, enforced in the app (not only the panel); partial refreshes only during button use, rate-limited; no refresh when nothing changed |
| Panel fault takes the node down | Separate process and service; only it gets SPI/GPIO; it talks to the app over the local hook; the app runs normally without it |
| Someone presses the buttons and changes the race state | Allowlisted actions only, nothing destructive; lifecycle actions always confirmed; items can be disabled per node; actions logged with source `panel` |
| Wrong controller for the bonnet revision (revisions are mixed across nodes) | Detected per node: read-back probe if wired, else the button wizard; manual override and test patterns; the bench (phase 14) checks all three |
| Bonnet pins clash with other hardware on the Pi header | The radio uses an AIOC (USB only), so nothing else is on the header; phase 14 confirms graywolf's PTT isn't set to a Pi GPIO |
| HQ raises a false "quiet" alarm for a checkpoint that closed early and is travelling | Closed flag on the checkpoint's heartbeats; HQ shows it as closed (4.7) |
| Operator deletes race rows in graywolf mid-race | CP: resend 404 means send it new (HQ dedups). HQ: rows already ingested are unaffected |
| Accidental reset | Type-the-race-name confirm, stronger warning while active, automatic backup before clearing |
| Cleanup deletes operator messages | Deletes only app-recorded row ids whose text starts `RC1 `; never thread deletes |
| Changing the callsign from admin affects all of graywolf | Explicit confirm; shown as graywolf's setting, not the app's |
| Volunteer password spreads beyond the crew | Accepted (user decision): it only allows bib entry, voids and clock sync. The admin can rotate it, which logs out every volunteer session |
| Volunteer reaches settings or tools | Server-side role check on every admin route (7.2); tested per route in phase 9 |
| Admin password lost at a station | README recovery: `checkin-board reset-admin-password` CLI on the host (needs shell access); race data is untouched |
| graywolf down or restarting | Outbox and keypad keep working locally; banner; catch-up on reconnect |
| Shared 1200-baud congestion; no digis on dedicated freq; iGates gating `RC1` on 144.390; lost ACK means duplicate; bad clocks; typos | Unchanged from the original section 10 |
| Uploaded logo used as an attack (script in SVG, malformed image, decompression bomb) | SVG refused; image decoded with size and pixel caps before full decode; re-encoded to PNG; served with stored type and `nosniff`; admin login required to upload |
| Branding makes the board unreadable outdoors | WCAG AA contrast enforced on save; Reset to default; branding never applies to the keypad |
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

Added 2026-10-06:

5. **Live RF testing** is deferred until the radio hardware is ready;
   everything else is built and tested against the fake graywolf (the
   contract test moves to phase 12).
6. **Branding.** The status board's logo, colour scheme, and header and
   footer text are configurable in the admin panel (8.3).

Answered 2026-10-08 (node panel, 8.4):

7. **Bonnet revision:** unknown on the first unit, and nodes will have
   a mix. So the controller is detected per node (probe or button
   wizard), with a manual override.
8. **Header pins:** the radio connects through an AIOC (USB sound card,
   serial and CM108 PTT), so nothing else uses the header. Phase 14
   confirms graywolf's PTT setting.
9. **Close checkpoint:** a checkpoint often closes before the race is
   complete (early on the course), then returns to HQ. "Close
   checkpoint" is that node's Complete, and HQ is told through the
   heartbeat's closed flag (4.7).
10. **Test node:** the test node has a bonnet attached, and there is
    shell access to it.

## 12. Fallback transport (not planned)

If the Messages API turns out unworkable in phase 1, graywolf also serves
**KISS-over-TCP** and **AGWPE** to external clients as published
interfaces. The app could then send and receive raw APRS frames itself. It
would get back the original design exactly: own msgids `R`+seq,
store-then-ACK, REJ on bad text. The cost is implementing APRS message
ACK/dedup ourselves (porting `pkg/race`'s transmit side, which is ours).
This is a decision for after phase 1, not part of the MVP.

## 13. Open questions

None blocking. The admin settings page shows the fields in 8.1 plus the
batching and timing knobs (batch after, batches in flight, heartbeat,
gap grace); confirm in the field whether those should be hidden behind
an "advanced" toggle.

## 14. Post-MVP backlog

Unchanged: cutoff times and overdue highlighting; `RC1 T` RF time
broadcast; DNF / drops; time-out / dwell; roster push over RF; webhook push
to timing software. New: the KISS/AGW transport (section 12);
the node panel's buttons and menu (8.4, shelved 2026-10-09: revisit
with a panel whose partial refresh works), with a button-sequence lock; a webhook Action for
remote restart and status of `checkin-board`; per-person admin
accounts if one admin password per station proves too coarse.
