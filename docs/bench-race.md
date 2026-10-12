# Bench race (`benchrace`)

A test tool for the radio bench (spec section 9, phase 12 (g)). It logs a
scripted race through the checkpoint nodes' keypads, then reads HQ's
results to check that every passage arrived **exactly once**, and how
long each took. It uses only the nodes' own web API, the way an operator's
browser does. It runs on your computer, not on a node.

## Before a run

- Every node runs checkin-board and uses the **same admin password**.
- HQ: role HQ, race **started**.
- Each checkpoint: role checkpoint, HQ callsign set, checkpoint **open**.
- The bench's bibs (9001-9100 by default) aren't used yet on any node. The
  bench logs real entries, so **reset the race on every node after a
  bench** (Admin → Race → Reset), or pick other bibs with `-first-bib`.
- Run a link check between each checkpoint and HQ first; the bench
  doesn't. It measures delivery, not whether the radios work.

The bench checks all of this except the link check before it changes
anything: if a node isn't ready, it stops with exit status 2, and no
clock is synced and no bib logged.

## Run

```sh
make benchrace
CB_BENCH_PASSWORD='the admin password' bin/benchrace \
  -hq http://10.0.0.65:8090 \
  -cp http://10.0.1.183:8090
```

- `-cp` is repeated for each checkpoint node, in course order.
- `-runners 100`: runner count; bibs are consecutive from `-first-bib 9001`.
- `-duration 20m`: when the slowest runner reaches the last checkpoint.
  Everyone starts together, so the first checkpoint gets the biggest burst.
- `-settle 15m`: how long to wait for HQ after the last bib.
- `-seed 1`: the same seed gives the same race.
- `-json`: print the report as JSON.

The password comes from `CB_BENCH_PASSWORD`, so it doesn't show in the
process list.

## Report

```
PASS: 200/200 passages at HQ
latency (HQ received - checkpoint time in): p50 21s, p95 48s, max 1m31s
  AS1    100/100  p50 20s, p95 45s, max 1m31s
  AS2    100/100  p50 22s, p95 50s, max 58s
```

- Latency is HQ's `received_at` minus the checkpoint's `time_in`, to the
  second. Each node's own clock sets these values, so clock skew between
  nodes shows up here.
- Missing, duplicated, unexpected and unconfirmed passages are listed.
  Unexpected means a passage in the bench's bibs that the bench didn't
  log.
- A bib that got no clear answer from its keypad is retried with the same
  request id, so it's logged at most once. If it still fails, it's left
  out of the count; if HQ has it anyway, it's listed as *unconfirmed*,
  which doesn't fail the run.
- After everything has arrived, HQ is read twice more, so a late
  duplicate from a retransmitted batch still shows.
- A run stopped after logging began (Ctrl-C) still prints what HQ had,
  and exits 1: reset the race on every node.
- Exit status: 0 if every passage arrived exactly once, 1 if not, 2 for
  setup or usage errors. Nothing is logged after a setup error.

## Without radios

`go test ./internal/bench` runs the same bench against three whole
nodes: app engines and web API, on fake graywolfs sharing a simulated
channel with 10% loss. It takes from a few seconds to a few minutes,
depending on how many batches need the retry ladder.
