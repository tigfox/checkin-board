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
docs/specs/            design spec
```

## Configuration

| Variable           | Default                 | Description |
|--------------------|-------------------------|-------------|
| `GW_BASE_URL`      | `http://localhost:8080` | graywolf base URL |
| `GW_USER`          | (required)              | graywolf username for the app's service account |
| `GW_PASSWORD`      |                         | graywolf password (or use `GW_PASSWORD_FILE`) |
| `GW_PASSWORD_FILE` |                         | file holding the password; must be `chmod 600`; wins over `GW_PASSWORD` |
| `GW_TIMEOUT`       | `10s`                   | per-request timeout |

## Development

```sh
make test     # unit tests with -race (no network, no RF)
make cover    # coverage report
make build    # bin/checkin-board
make pi       # bin/checkin-board-armv6 for Pi Zero W
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
