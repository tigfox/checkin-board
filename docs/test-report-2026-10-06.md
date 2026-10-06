# Test report: 2026-10-06 (phase 12, non-hardware part)

This covers everything in the phase 12 test campaign that can run without
radios. The bench and Pi items, (g) and (h), wait for hardware, so the
phase's exit criteria aren't met yet.

## (a) Coverage and static checks

- `go test -race ./...` passes. Every package is at 80% or more except
  `cmd/checkin-board` (39%: wiring; the CLI paths are tested).
  - `internal/web` 80.6%, `linkcheck` 87.1%, `store` 90.3%, `sim` 84.4%.
- `go vet`: clean.
- `staticcheck`: clean. It found one issue, a literal bidi character in a
  test, which is now escaped.
- `govulncheck`: one advisory, GO-2026-5932 in `golang.org/x/crypto`
  v0.57.0. There is no fixed version yet and the app doesn't call the
  affected code. Re-check when a fix ships.
- UI logic: 16 `node --test` tests pass.
- ARMv6 (`GOARM=6`) build: OK.

## (b) Long fuzz runs (30 min each, 2 workers each, concurrently)

| Target | Executions | Result |
|---|---|---|
| `wire.FuzzDecode` | 208.5 M | no failures |
| `store.FuzzParseRosterCSV` | 33.1 M | no failures |
| `branding.FuzzProcessLogo` | 26.4 M | no failures |
| `journal.FuzzParse` (new: round-trip property) | 128.4 M | no failures |
| `ops.FuzzParseExport` (new: accepted batches must encode) | 31.6 M | no failures |

Rerun with `make fuzz`.

## (c) Soak (`make soak`)

Simulated race:

- 12 hours, 500 runners, 8 checkpoints and HQ (9 nodes).
- 20% loss, 10% duplication, up to 4 s reordering.
- 3,917 passages; 7,983 frames on air during the race.

Results:

- **Exactly once at HQ:** pass. Every entry was confirmed by the time
  the last passage was logged.
- **Heap** (all 9 nodes, their in-memory databases and fake graywolf
  stations, in one process): 4.1 MB at hour 1, 9.0 MB at hour 12. It grew
  with the data held in memory, not with time. The real per-node figure
  needs the Pi run in (h).
- **Database size:** 83–93 bytes per race passage at each checkpoint,
  301 at HQ (1.2 MB for the whole race).

## (d) Fault injection

| Fault | Test |
|---|---|
| graywolf API down mid-race at both ends (staggered, 10–15 min) | `sim.TestGraywolfAPIOutageMidRace`: exactly once |
| Both apps killed and restarted repeatedly mid-race (memory lost, databases kept) | `sim.TestRepeatedRestartsMidRace`: exactly once |
| Kill between batch and send (unknown send outcome) | `checkpoint.TestUnknownSendOutcomeIsRecoveredNotDuplicated`, `TestRecover*` |
| Database write fails (disk full) | `ops.TestLogBibKeepsJournalWhenDatabaseFails`, `…JournalAlsoFails` |
| Torn journal tail after a power cut | `journal.TestJournalRecoversFromTornTail`, `…StartsFreshLineAfterWriteError` |
| graywolf session expiry or password change | `graywolf.TestReloginOnExpiredSession`, `TestPersistent401AfterReloginIsAuthError` |
| Event stream drop | `graywolf.TestStreamEventsReloginOn401`, inbox reconnect tests |
| OS clock step | `raceclock` monotonic tests |
| Checkpoint reset mid-race (seq reuse); HQ restored from an old backup; HQ wiped | `hq` seq-reuse tests; `sim.TestHeartbeatRevealsLostBatch`, `sim.TestWipedHQRecoversFromGraywolfInbox` |
| Bib saved but the reply to the phone lost | browser `TestE2EKeypadSurvivesLostReply`: logged once |

## (e) Security

- **Route access:** the matrix covers every route (59) as nobody,
  volunteer and admin. The link-check hook takes a token only, comes from
  loopback only, and is off by default.
- **CSRF guards:** cross-site and cross-origin requests and non-JSON
  content types are refused.
- **Login limits:** rate limit and lockout, including IPv6 /64 keys.
- **Upload and body limits.**
- **CSV formula injection:** neutralised in the exports.
- **Hostile logos:** SVG, GIF, truncated, oversized and decompression-bomb
  files are refused; a polyglot PNG's payload is stripped by re-encoding.
- **Branding text:** HTML and bidi-override text are rejected.
- **Hook replies:** on-air hook text never carries internal error
  details.

## (f) Browser E2E (`make e2e`, headless Chrome at 420 px)

All 8 pass:

- login form → keypad;
- keypad log, double tap (saved once), void;
- a lost reply, retried without duplication;
- the clock banner and "Set time from this device";
- admin tabs and saving settings;
- Start race;
- HQ checkpoints, branding and the status board;
- the link-check tab and cancel.

## (g), (h) Waiting for hardware

- (g): the graywolf contract tests on RF (`make contract`); 2–3 nodes on
  radios; a link check between every node and HQ.
- (h), on a Pi Zero W:
  - RSS (Linux arm64 idle is ≈ 20 MB);
  - CPU during a surge of 20 bibs/min;
  - startup time;
  - a smoke test of the systemd syscall filter.
