# checkin-board

Race checkpoint reporting that runs next to a [graywolf](https://github.com/chrissnell/graywolf)
APRS station and talks to it only through graywolf's REST API.
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
internal/panel/        node panel: e-ink status screen and two-button menu (renderer, state machine, hook client)
internal/panel/menu/   the panel's action allowlist, default menu and validation
internal/panel/epd/    e-ink controllers (drivers arrive in phase 14)
internal/gwfake/       in-memory fake of graywolf's Messages API + simulated RF channel (tests)
internal/sim/          whole-node simulation: exactly-once and latency tests
deploy/                systemd unit, env template, install script
docs/specs/            design spec
docs/operator-guide.md install, race day, recovery, reset (for station operators)
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

## Configuration

| Variable           | Default                 | Description |
|--------------------|-------------------------|-------------|
| `GW_BASE_URL`      | `http://localhost:8080` | graywolf base URL |
| `GW_USER`          | (required)              | a graywolf login (not a Linux user) for the app; `install.sh` creates `checkin-board` with graywolf's CLI (see the operator guide) |
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
| `/admin.html` | admin | race lifecycle and reset, station settings and graywolf callsign, outbox and export (checkpoint), checkpoints, roster, health and recovery imports (HQ), board branding (HQ), passwords |
| `/board.html` | admin | HQ status board in the configured branding, printable |

## Node panel

Nodes with an Adafruit 2.13" e-ink bonnet show their status on it and
offer a two-button menu (Admin → Panel edits it; spec 8.4). The panel is
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
make pi       # bin/checkin-board-armv6 for Pi Zero W
make jstest   # UI logic tests (needs Node 20+; test-only)
make e2e      # drives the real pages in headless Chrome (needs Chrome; test-only)
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
