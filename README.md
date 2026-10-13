# checkin-board

Checkin-board is a race checkpoint reporting system that runs next to a [graywolf](https://github.com/chrissnell/graywolf)
APRS station and talks to it through graywolf's REST API.

The goal is to have a pre-configured APRS station that a ham can deploy and a race coordinator can access via a web app. The race coordinator enters bib numbers as they cross the checkpoint, and graywolf sends the messages to HQ. A board at HQ shows recent check-ins and allows configuration of race parameters.
Design: [`docs/specs/2026-10-05-race-checkpoint-design.md`](docs/specs/2026-10-05-race-checkpoint-design.md).

## Layout

```
cmd/checkin-board/     entrypoint
internal/config/       bootstrap env configuration
internal/graywolf/     graywolf REST client (auth, messages, SSE, prefs, station)
internal/wire/         RC1 message grammar (reports, heartbeats, gaps, link-check probes)
internal/raceclock/    browser-synced race clock
internal/store/        SQLite storage (pure Go), embedded migrations
internal/inbox/        follows graywolf's message feed, dispatches race traffic
internal/checkpoint/   checkpoint engine: batching, outbox, retries, heartbeats
internal/hq/           HQ engine: report ingest, gap requests, health view
internal/app/          assembles the service; role-aware inbox dispatch
internal/ops/          operator actions: keypad + journal, export/import, board, lifecycle
internal/journal/      power-safe bib journal (plain CSV, fsynced before the DB write)
internal/auth/         app logins: bcrypt, sessions, lockout
internal/branding/     status-board branding: contrast rules, logo processing
internal/web/          REST API (role-guarded route table) and embedded UI
internal/web/static/   the UI: plain HTML + ES modules, no build step, no inline script/style
internal/web/jstest/   `node --test` unit tests for the UI logic
internal/peers/        graywolf per-peer retry settings (backup / restore)
internal/linkcheck/    deployment link check: RC1 P probes, RC1 Q replies, verdicts
internal/uvk5/         UV-K5/UV-K1 radio link: serial protocol codec, AX.25, monitor lines, session, fake radio
internal/radiobackend/ a UV-K5 radio as graywolf's message API: message rows, paced sender, ack matching, lending the port
internal/radiokind/    the radio types a node can use (Station page, Radio type)
internal/firmware/     UV-K5 firmware files on the node, and the flash job (the node flashes the radio itself)
internal/web/uvstudio/ UV Studio (Apache-2.0, vendored, see VENDORED.md) with a Web Serial bridge to the node's radio
cmd/uvk5probe/         bench tool: talk to a UV-K5 directly, print its events
internal/provision/    stationsetup: stations CSV -> race config, pushed to and applied on every node
internal/nodeapi/      client for a node's web API (stationsetup, benchrace)
internal/bench/        benchrace: scripted race across nodes, exactly-once and latency report
internal/cablewatch/   radio cable watcher: holds checkpoint reports while the cable is missing
internal/testnode/     whole nodes (graywolf or radio) on fakes over HTTP (tests)
cmd/stationsetup/      set up every node from a stations CSV
cmd/benchrace/         radio bench race tool
internal/raceconfig/   race config files: parse/validate, preview, apply (incl. graywolf, with backup/restore), export, folder
internal/radiocheck/   radio status check: graywolf build, channel, audio, PTT, rate, level, decodes, modem keep-up
internal/hostmon/      node health from /proc: CPU 5-min average, graywolf modem keeping up, machine
internal/panel/        node panel: e-ink status screen and two-button menu (renderer, state machine, hook client)
internal/panel/menu/   the panel's action allowlist, default menu and validation
internal/panel/epd/    e-ink controllers (SSD1680Z/SSD1680/SSD1675 over periph.io)
internal/gwfake/       in-memory fake of graywolf's Messages API + simulated RF channel (tests)
internal/sim/          whole-node simulation: exactly-once and latency tests
deploy/                systemd unit, env template, install script (+ option tests)
deploy/graywolf-armv6/ ARMv6 (Pi Zero) graywolf build kit: audio-buffer patch, build.sh
docs/specs/            design spec
docs/operator-guide.md install, race day, recovery, reset (for station operators)
docs/race-day-runbook.md printable race-day checklist, quick fixes, station card
```

## Install on a node

`make dist` builds `dist/checkin-board-<version>-linux.tar.gz`: Linux
binaries for armv6 (any Raspberry Pi), arm64 and amd64, the systemd
unit, env template, `install.sh` and the operator guide. On the node,
unpack it and run `sudo ./install.sh`, which picks the binary for that
machine. See
[`docs/operator-guide.md`](docs/operator-guide.md) for setup, race day,
recovery and reset. The service runs as `checkin-board` with its data
in `/var/lib/checkin-board` and starts after `graywolf.service`.

**Pi Zero W nodes** need a patched graywolf and 24 kHz audio: build it
with [`deploy/graywolf-armv6/`](deploy/graywolf-armv6/README.md) and
install it with `sudo ./install.sh --graywolf-deb FILE.deb`. Then
**Admin → Station → Radio → Check radio** confirms the radio works.

## Configuration

| Variable           | Default                 | Description |
|--------------------|-------------------------|-------------|
| `CB_BACKEND`       | `graywolf`              | the radio type until one is chosen on the Station page (Radio → Radio type), which wins from then on: `graywolf` (a radio on an AIOC cable), or `uvk5` (a UV-K5 V3 / UV-K1 on USB-C); see "Radio types" |
| `CB_UVK5_PORT`     | (found)                 | the radio's serial port (`/dev/ttyACM*`, `/dev/ttyUSB*`, `/dev/serial/...`); unset: the first one that answers |
| `GW_BASE_URL`      | `http://localhost:8080` | graywolf base URL |
| `GW_USER`          | (needed for the graywolf radio type) | a graywolf login (not a Linux user) for the app; `install.sh` creates `checkin-board` with graywolf's CLI (see the operator guide) |
| `GW_PASSWORD`      |                         | graywolf password (or use `GW_PASSWORD_FILE`) |
| `GW_PASSWORD_FILE` |                         | file holding the password; must be `chmod 600`; wins over `GW_PASSWORD` |
| `GW_TIMEOUT`       | `10s`                   | per-request timeout |
| `CB_DB_PATH`       | `checkin-board.db`      | the app's SQLite database (keep it on persistent storage) |
| `CB_LISTEN`        | `:8090`                 | address the web UI and API listen on |
| `CB_HOOK_TOKEN_FILE` |                       | optional: token file (`chmod 600`, 24+ chars) that turns on the local hook a graywolf webhook Action uses to run a link check ([recipe](docs/linkcheck-action.md)) |

The bib journal (`race-journal.csv`) and reset backups (`backups/`) live
next to the database.

## Logins

On first visit, set the **admin** password (10+ characters). The setup
page asks for a one-time setup code, which the server prints in its log
at startup (`journalctl -u checkin-board | grep setup_code`). The admin
then sets the shared **volunteer** password (6+ characters) for the
keypad. Volunteers can only log bibs, void entries and set the clock;
everything else needs the admin login.

Lost the admin password? On the node itself:

```sh
echo 'a new admin password' | CB_DB_PATH=/path/to/checkin-board.db checkin-board reset-admin-password
```

This logs out every admin session and doesn't touch race data.

## Radio types (AIOC + graywolf, UV-K5 V3 / UV-K1)

Every node can send race traffic through either kind of radio, chosen on
**Admin → Station → Radio → Radio type**:

- **Radio with an AIOC cable (graywolf)**: any 2 m radio on an AIOC (or
  other sound-card) cable, with graywolf on the Pi as the modem. The
  default.
- **UV-K5 V3 / UV-K1 on USB-C (APRS firmware)**: a radio running the
  custom APRS firmware (protocol revision 1 or later), plugged into the Pi
  by USB-C (spec phase 17). The radio does the packet work, so even a Pi
  Zero has CPU to spare.

Nodes with either radio share one frequency and one race: they speak the
same RC1 messages in ordinary APRS messages. One install covers both
(`install.sh` sets up graywolf and lets the app open the radio's USB
port), so a radio can be **swapped mid-race**: connect the new radio,
choose its type and press **Change radio type**. The app restarts with it
in about 10 seconds (a bib logged meanwhile shows NOT confirmed: tap LOG
again); entries are kept, and reports not yet acknowledged go out through
the new radio (HQ ignores repeats). Then run **Check radio**. Unplug or
switch off the old radio: graywolf keeps running. `stationsetup`'s
`backend` column sets the type from the stations CSV. Use UV-K5s for
**checkpoints only; keep HQ on graywolf**. HQ has no 5-second gap between
packets on graywolf, and the current UV-K5 firmware (F4HWN v6.1.0) doesn't
ack messages longer than about 34 characters, so a UV-K5 HQ never confirms
a checkpoint's reports (bench, 2026-10-13; reported to the firmware team).

To add a new kind of radio later: an entry in `internal/radiokind`, a
backend that answers the app's graywolf-shaped message calls (as
`internal/radiobackend` does), and a case in `cmd/checkin-board`'s
`startMessaging`.

On a UV-K5: this station's callsign-SSID (or let the Station page or
`stationsetup` set it), APRS on, the race frequency in FM, and its Path
set to Direct or WIDE1-1. **Check radio** shows the connection, firmware,
callsign, whether it can transmit and why not, frequency, battery,
packets decoded and its queue. A radio sends one packet every 5 s at
most and holds 4 messages; about 30-50 bibs a minute on a clean link.

How it works: `internal/uvk5` speaks the radio's serial protocol
(`uv-k1-k5v3-firmware-custom/claude/aprs-messaging-api.md`, revision 2),
and `internal/radiobackend` presents the radio to the engines as
graywolf's message API, keeping message rows in the node's database. The
engines don't change. Plan and status:
[`docs/specs/2026-10-12-uvk5-backend-plan.md`](docs/specs/2026-10-12-uvk5-backend-plan.md).

### UV Studio and firmware (UV-K5 nodes)

**UV Studio** ([armel.github.io/uvstudio](https://armel.github.io/uvstudio/),
by Armel FAUVEAU, Apache-2.0) runs on the node at `/uvstudio/` (admin
login; Station → Radio → **Open UV Studio**). You get the radio's live
screen and keypad, its RF log with CSV export, and calibration and
boot-logo backup, from any browser on the node's network: phone, tablet
or laptop, over plain HTTP. UV Studio normally talks to a radio plugged
into the browser's own computer. Here a small stand-in for Web Serial
carries its bytes over a WebSocket to the radio on the Pi. While UV Studio
has the radio, the node's race reports wait (the keypad and panel say
"Radio in use: UV Studio"). Closing the page, or 10 minutes without a
click or key, gives the radio back. During a race it asks first. Its
firmware tools are left out, and nothing reaches the internet
(`internal/web/uvstudio/VENDORED.md` lists every change).

**Firmware** is flashed by the node itself, from files kept on it, so the
browser's connection doesn't matter once a flash starts. To flash:

1. Upload a `.bin` on Station → **Radio firmware**. You can also copy it
   into `/var/lib/checkin-board/firmware/`, or use
   `sudo ./install.sh --firmware FILE.bin`.
2. Press **Flash**. The node takes the radio.
3. Switch the radio off, hold PTT, and switch it on (bootloader mode). The
   node finds it, checks the bootloader (7.00.07 or later) and writes the
   image page by page.
4. The radio restarts and reconnects. Run **Check radio**.

The node never raises DTR on an AIOC cable (it keys PTT from DTR).

## Setting up every node from a CSV

`stationsetup` sets up all of a race's nodes from your computer, over the
local network, from one CSV with a line per node. Every node must already
be installed, have finished first-run setup with the **same admin
password**, and be in race setup (race not started).

1. Copy [`example_stations.csv`](example_stations.csv) and edit it, one
   line per node:

   | Column | Required | Meaning |
   |---|---|---|
   | `address` | yes | the node's IP or name (port 8090 unless given) |
   | `role` | yes | `hq` or `checkpoint`; exactly one `hq` |
   | `station_name` | yes | tactical name, up to 25 characters |
   | `code` | checkpoints | the checkpoint code; on the HQ line, HQ's own codes separated by `;` (or spaces), such as `START;FIN` |
   | `callsign` | yes | the node's callsign-SSID, different on every node. Checkpoints report to the HQ line's callsign |
   | `course_order` | no | the checkpoint's place along the course (1, 2, ...) |
   | `checkpoint_name` | no | the name on HQ's checkpoint list (default: `station_name`) |
   | `race_name`, `path`, `heartbeat_sec`, `flush_after_sec`, `max_in_flight`, `max_text_len`, `gap_grace_sec`, `gw_channel`, `tx_delay_ms`, `tx_tail_ms` | no | race-wide: set once (any line) or the same on every line. `path` is the digipeater path, `direct` for none. UV-K5 nodes ignore `path`, `gw_channel` and the TX timing (the radio has its own) |
   | `backend` | no | the node's radio type: `graywolf` (AIOC cable) or `uvk5` (UV-K5 V3 / UV-K1 on USB-C). A node on another type is switched to it (it restarts) before the rest is set; blank leaves it as it is |

   Blank cells leave a node's current value alone. Lines starting with `#`
   are comments. The CSV holds no passwords.
2. Build the tool and preview every node. Nothing changes yet, except that
   each node receives the generated race config file:

   ```sh
   make stationsetup
   CB_ADMIN_PASSWORD='the admin password' bin/stationsetup -dry-run my_stations.csv
   ```

   Each node lists what it would change, from its current value to the
   new one, or why it can't (a node that's unreachable, already racing,
   or has a wrong password).
3. Run it without `-dry-run`. It shows the same list, asks you to type
   `yes`, then sets up every node that's ready and reports each one:

   ```sh
   CB_ADMIN_PASSWORD='the admin password' bin/stationsetup my_stations.csv
   ```

What it sets on each node: the role, race name, station name and code,
the HQ callsign (checkpoints), the local codes and checkpoint list (HQ),
the messaging settings, and in graywolf the node's station callsign and
TX delay and tail (on a UV-K5 node: the radio's callsign), and, from the
optional `backend` column, the node's radio type (`graywolf` or `uvk5`; the
node restarts with it first, then the rest is set for that radio).
`-no-graywolf` leaves graywolf alone. graywolf's
original values are kept on the node, and **Admin → Race → Race config →
Restore graywolf settings** puts them back. Every setting stays editable
on the node afterwards. Under the hood the CSV becomes a race config file
(`stationsetup-<hash>.json`, named from its contents so it never replaces
one of yours; it's listed on each node's Race page), so it's checked by
the same rules as a file loaded in the browser. Running it again is safe:
a node that's already set up has nothing to change. If a node doesn't
answer while applying, it's reported as "outcome unknown": run the tool
again to see whether it was set up.

Exit status: 0 every node set up, 1 one or more weren't, 2 a usage or
CSV error (no node touched).

## Roster

A roster is optional. It lets HQ's board and results show each runner's
category, and flag bibs that aren't registered. It's a CSV with one line
per runner, like [`example_roster.csv`](example_roster.csv):

```csv
bib,category,name
0001,50K,Sample Runner 1
0101,25K,Sample Runner 11
```

- The **bib** must be the first column: 1-9999; leading zeros are fine.
- A column headed **category** (up to 32 characters, such as the
  distance or wave) is kept. Every other column (names, ages, emails)
  is **discarded on import**, so a registration export can be loaded as
  it is and no personal data reaches the node or the radio.
- With no header line, only the bibs are read.
- Excel's "CSV UTF-8" is fine. Blank lines are skipped.

Load it on the **HQ** node: **Admin → HQ → Roster → Import roster**,
then pick the file. The import is all or nothing: a bad line (a bib
out of range or listed twice) rejects the file and every bad line is
listed, so you can fix them in one pass. Importing again adds new bibs
and updates categories; bibs already loaded stay. A race reset keeps
the roster unless you tick "Also clear the checkpoint list and roster".

## Link check

Before the race, each node checks its radio link (spec 4.8): Admin → Link
check, or from the node's shell:

```sh
sudo -u checkin-board CB_DB_PATH=/var/lib/checkin-board/checkin-board.db checkin-board linkcheck
```

Exit code 0 PASS, 1 MARGINAL, 2 FAIL (or couldn't run); `--json`,
`--brief`, `--count N`, `--to CALL` (HQ), `--yes` (during the race). The
running service does the radio work; the command records the request in
the database and waits. Net control can also trigger it by radio through
a graywolf Action: [docs/linkcheck-action.md](docs/linkcheck-action.md).

## Pages

| Page | Login | What it's for |
|------|-------|---------------|
| `/` | any | sends you to the right page for your login |
| `/login.html` | none | first-run setup (setup code + admin password) and login |
| `/keypad.html` | volunteer or admin | log bibs, void entries, set the race clock from the phone |
| `/admin.html` | admin | race lifecycle and reset, race config files, station settings and graywolf callsign, radio check, event page, outbox and export (checkpoint), checkpoints, roster, health and recovery imports (HQ), board branding (HQ), passwords |
| `/event.html` | volunteer or admin | the event page: course notes, frequencies, contacts (set on Admin → Station or from a race config) |
| `/guide` | admin | station guide: settings, radio and graywolf setup, Pi Zero notes, link testing, 2 m band plan (served by the node; no internet needed) |
| `/board.html` | admin | HQ status board in the configured branding, printable |

## Node panel

Nodes with an Adafruit 2.13" e-ink bonnet show their status on it (spec
8.4; the bonnet's buttons and menu are shelved, since the display can't
update fast enough for a menu). The panel is
its own service, `checkin-board-panel`, talking to the app over the
local hook. `install.sh` sets it up when SPI is enabled
(`sudo raspi-config nonint do_spi 0`, then reboot).

Check the bonnet without the app, once per controller until the test
pattern is readable (a refresh that returns at once or times out points
at the wrong controller):

```sh
sudo systemctl stop checkin-board-panel
sudo -u checkin-board checkin-board panel -test ssd1680z
```

Try the panel on any machine, with frames written as PNGs and the
buttons on the keyboard (`t` top, `b` bottom):

```sh
CB_HOOK_TOKEN_FILE=... CB_LISTEN=:8090 checkin-board panel -display png:/tmp/frames -buttons stdin
```

## Development

```sh
make test     # unit tests with -race (no network, no RF)
make cover    # coverage report
make build    # bin/checkin-board
make stationsetup  # bin/stationsetup: set up every node from a CSV (see above)
make benchrace     # bin/benchrace: radio bench race (docs/bench-race.md)
make pi       # bin/checkin-board-armv6 for Pi Zero W
make jstest   # UI logic tests (needs Node 20+; test-only)
make e2e      # drives the real pages in headless Chrome (needs Chrome; test-only)
make installtest  # install.sh's graywolf package handling on real dpkg/apt in an armhf container (needs docker)
make soak     # 12-hour, 500-runner, 8-checkpoint race simulation (~1-2 min)
make fuzz     # long fuzz runs, FUZZTIME=30m per target
go test -short ./...                                   # skip the long simulations
CB_LATENCY_TABLE=1 go test -run TestLatencyTable ./internal/sim   # spec 9b table (~2 min)
```

## graywolf contract tests (transmit on RF)

These check the graywolf behaviours the design depends on: msgid stable
across resend, `wait_for_ack=false` stopping graywolf's retries while
late ACKs still register, `client_id` round trip, the inbox cursor not
skipping rows, and single-row delete. They **send real APRS messages**,
so run them only on a test setup with a licensed control operator.

```sh
GW_CONTRACT=1 GW_BASE_URL=http://gw-host:8080 GW_USER=race GW_PASSWORD=... \
GW_CONTRACT_PEER=N0CALL-2 make contract          # quick checks (~1 min)

GW_CONTRACT_SLOW=1 ... make contract              # + retry/ACK timing (~5 min)
```

`GW_CONTRACT_PEER` must be a station that ACKs DMs (for example a
second graywolf). Each test restores the peer's conversation prefs and
deletes the messages it sent. Lines starting `FINDING` in the `-v`
output record behaviour the design has a fallback for.
